// Package dataset implements ML-specific post-processing for the VFS:
// dataset splits (train/val/test), cross-source patient linking,
// manifest versioning, and the FHIR/OMOP→VFS bridge.
package dataset

import (
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/colabbio/medvfs/internal/adapters"
	"github.com/colabbio/medvfs/internal/config"
	"go.uber.org/zap"
)

// ─── Dataset Splits (G2) ─────────────────────────────────────────────────────

// SplitMap maps patient IDs to their assigned split (train/val/test).
type SplitMap map[string]string

// LoadSplits reads a splits CSV from a local path and returns a SplitMap.
// The CSV must have at least two columns, identified by patientCol and splitCol.
func LoadSplits(_ context.Context, csvPath string, patientCol, splitCol string) (SplitMap, error) {
	if patientCol == "" {
		patientCol = "patient_id"
	}
	if splitCol == "" {
		splitCol = "split"
	}

	f, err := os.Open(csvPath)
	if err != nil {
		return nil, fmt.Errorf("open splits CSV %q: %w", csvPath, err)
	}
	defer f.Close()

	reader := csv.NewReader(f)

	// Read header
	header, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("read splits CSV header: %w", err)
	}

	patientIdx, splitIdx := -1, -1
	for i, col := range header {
		switch strings.TrimSpace(strings.ToLower(col)) {
		case strings.ToLower(patientCol):
			patientIdx = i
		case strings.ToLower(splitCol):
			splitIdx = i
		}
	}
	if patientIdx == -1 {
		return nil, fmt.Errorf("splits CSV: column %q not found in header %v", patientCol, header)
	}
	if splitIdx == -1 {
		return nil, fmt.Errorf("splits CSV: column %q not found in header %v", splitCol, header)
	}

	splits := make(SplitMap)
	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read splits CSV row: %w", err)
		}
		if patientIdx < len(record) && splitIdx < len(record) {
			pid := strings.TrimSpace(record[patientIdx])
			split := strings.TrimSpace(strings.ToLower(record[splitIdx]))
			if pid != "" && split != "" {
				splits[pid] = split
			}
		}
	}

	return splits, nil
}

// ApplySplits reorganises file mappings by prepending the split name to
// each file's LogicalPath based on the patient ID extracted from the path.
//
// Input:  "P001/slide_HE.svs"
// Output: "train/P001/slide_HE.svs"  (if P001 → train)
//
// Files whose patient ID is not found in the split map are placed under "unassigned/".
func ApplySplits(mappings []adapters.FileMapping, splits SplitMap) []adapters.FileMapping {
	result := make([]adapters.FileMapping, 0, len(mappings))
	for _, m := range mappings {
		// Extract patient ID from first path component
		patientID := extractPatientID(m.LogicalPath)
		split, ok := splits[patientID]
		if !ok {
			split = "unassigned"
		}

		newMapping := m
		newMapping.LogicalPath = split + "/" + m.LogicalPath
		result = append(result, newMapping)
	}
	return result
}

// extractPatientID returns the first component of a logical path.
// "P001/slide.svs" → "P001"
// "metadata/labels.csv" → "metadata"
func extractPatientID(logicalPath string) string {
	parts := strings.SplitN(logicalPath, "/", 2)
	if len(parts) > 0 {
		return parts[0]
	}
	return logicalPath
}

// ─── Patient Linking (G4) ────────────────────────────────────────────────────

// PatientLink maps a canonical ID to source-specific patient IDs.
type PatientLink struct {
	CanonicalID string
	SourceIDs   map[string]string // source column name → patient ID in that source
}

// LinkMap is the full cross-source patient mapping.
type LinkMap struct {
	Links   []PatientLink
	ByID    map[string]*PatientLink // canonical_id → link
	Columns []string               // all source columns from the CSV
}

// LoadPatientLinks reads a patient linking CSV.
// Expected format: canonical_id, source_1_id, source_2_id, ...
func LoadPatientLinks(_ context.Context, csvPath string) (*LinkMap, error) {
	f, err := os.Open(csvPath)
	if err != nil {
		return nil, fmt.Errorf("open patient links CSV %q: %w", csvPath, err)
	}
	defer f.Close()

	reader := csv.NewReader(f)
	header, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("read patient links header: %w", err)
	}

	if len(header) < 2 {
		return nil, fmt.Errorf("patient links CSV needs at least 2 columns (canonical_id + source), got %d", len(header))
	}

	lm := &LinkMap{
		ByID:    make(map[string]*PatientLink),
		Columns: header[1:], // skip canonical_id column
	}

	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read patient links row: %w", err)
		}

		canonical := strings.TrimSpace(record[0])
		if canonical == "" {
			continue
		}

		link := PatientLink{
			CanonicalID: canonical,
			SourceIDs:   make(map[string]string),
		}
		for i := 1; i < len(record) && i < len(header); i++ {
			v := strings.TrimSpace(record[i])
			if v != "" {
				link.SourceIDs[header[i]] = v
			}
		}

		lm.Links = append(lm.Links, link)
		lm.ByID[canonical] = &lm.Links[len(lm.Links)-1]
	}

	return lm, nil
}

// ApplyUnifiedLayout reorganises mappings into a unified patient view.
// It replaces source-specific patient IDs with canonical IDs and groups
// files by source type.
//
// Input:  source="omero" → "Patient_001/slide.svs"
// Output: "LUNG-001/wsi/slide.svs"  (if Patient_001 maps to LUNG-001 via omero_id column)
func ApplyUnifiedLayout(sourceMappings map[string][]adapters.FileMapping, links *LinkMap) []adapters.FileMapping {
	// Build reverse index: sourceColumn:sourcePatientID → canonical
	reverseMap := make(map[string]string)
	for _, link := range links.Links {
		for col, pid := range link.SourceIDs {
			key := col + ":" + pid
			reverseMap[key] = link.CanonicalID
		}
	}

	var result []adapters.FileMapping

	for sourceID, mappings := range sourceMappings {
		// Infer source column from source ID (e.g., "omero_pathology" → try "omero_id", "omero_pathology_id")
		sourceCol := inferSourceColumn(sourceID, links.Columns)
		sourceType := inferSourceType(sourceID)

		for _, m := range mappings {
			patientID := extractPatientID(m.LogicalPath)

			// Try to find canonical ID
			canonical := patientID
			if sourceCol != "" {
				if cid, ok := reverseMap[sourceCol+":"+patientID]; ok {
					canonical = cid
				}
			}

			// Rebuild path: canonical_id/source_type/rest_of_path
			restOfPath := m.LogicalPath
			if idx := strings.Index(m.LogicalPath, "/"); idx >= 0 {
				restOfPath = m.LogicalPath[idx+1:]
			}

			newMapping := m
			newMapping.LogicalPath = fmt.Sprintf("%s/%s/%s", canonical, sourceType, restOfPath)
			result = append(result, newMapping)
		}
	}

	return result
}

// inferSourceColumn tries to match a source ID to a column in the links CSV.
func inferSourceColumn(sourceID string, columns []string) string {
	// Try exact match: source ID as column name
	for _, col := range columns {
		if strings.EqualFold(col, sourceID+"_id") {
			return col
		}
		if strings.EqualFold(col, sourceID) {
			return col
		}
	}
	// Try prefix match
	for _, col := range columns {
		base := strings.Split(sourceID, "_")[0] // "omero_pathology" → "omero"
		if strings.HasPrefix(strings.ToLower(col), strings.ToLower(base)) {
			return col
		}
	}
	return ""
}

// inferSourceType extracts a human-readable source type from the source ID.
func inferSourceType(sourceID string) string {
	// Common type mappings
	for _, prefix := range []string{"omero", "orthanc", "dicom"} {
		if strings.HasPrefix(strings.ToLower(sourceID), prefix) {
			return "wsi"
		}
	}
	if strings.HasPrefix(strings.ToLower(sourceID), "genomic") {
		return "genomics"
	}
	if strings.HasPrefix(strings.ToLower(sourceID), "fhir") || strings.HasPrefix(strings.ToLower(sourceID), "omop") {
		return "ehr"
	}
	if strings.HasPrefix(strings.ToLower(sourceID), "file") {
		return "files"
	}
	return sourceID
}

// ─── Dataset Versioning (G5) ─────────────────────────────────────────────────

// DatasetManifest holds versioning metadata for a materialised dataset.
type DatasetManifest struct {
	DatasetID      string            `json:"dataset_id"`
	RunID          string            `json:"run_id"`
	ManifestSHA256 string            `json:"manifest_sha256"`
	FileCount      int               `json:"file_count"`
	TotalBytes     int64             `json:"total_bytes"`
	Sources        map[string]int    `json:"sources"`
	Splits         map[string]int    `json:"splits,omitempty"`
	CreatedAt      string            `json:"created_at"`
}

// GenerateManifest creates a DatasetManifest from the current set of mappings.
func GenerateManifest(datasetID, runID string, sourceMappings map[string][]adapters.FileMapping) *DatasetManifest {
	manifest := &DatasetManifest{
		DatasetID: datasetID,
		RunID:     runID,
		Sources:   make(map[string]int),
		Splits:    make(map[string]int),
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}

	hasher := sha256.New()
	var allPaths []string

	for srcID, mappings := range sourceMappings {
		manifest.Sources[srcID] = len(mappings)
		for _, m := range mappings {
			manifest.FileCount++
			manifest.TotalBytes += m.SizeBytes
			allPaths = append(allPaths, m.LogicalPath)

			// Track split counts
			split := extractPatientID(m.LogicalPath)
			if split == "train" || split == "val" || split == "test" || split == "unassigned" {
				manifest.Splits[split]++
			}
		}
	}

	// Deterministic hash: sort paths, then hash path+size
	sort.Strings(allPaths)
	for _, p := range allPaths {
		fmt.Fprintf(hasher, "%s\n", p)
	}
	manifest.ManifestSHA256 = fmt.Sprintf("%x", hasher.Sum(nil))

	return manifest
}

// ManifestToJSON serialises the manifest as formatted JSON.
func ManifestToJSON(m *DatasetManifest) ([]byte, error) {
	return json.MarshalIndent(m, "", "  ")
}

// ─── FHIR/OMOP → VFS Bridge (G6) ────────────────────────────────────────────

// BridgeTabularOutputs scans the output directory for CSV files produced by
// non-FileProvider adapters (FHIR, OMOP) and returns FileMappings that can
// be registered in the VFS, making EHR data accessible alongside images.
func BridgeTabularOutputs(outDir string, sources []config.SourceConfig, log *zap.Logger) []adapters.FileMapping {
	var mappings []adapters.FileMapping

	for _, src := range sources {
		// Only bridge tabular sources that don't have FileProvider
		if src.Type != "fhir" && src.Type != "omop" {
			continue
		}

		csvPath := filepath.Join(outDir, src.Materialize.Output)
		info, err := os.Stat(csvPath)
		if err != nil {
			log.Debug("tabular output not found, skipping bridge",
				zap.String("source", src.ID),
				zap.String("path", csvPath),
			)
			continue
		}

		mappings = append(mappings, adapters.FileMapping{
			LogicalPath: src.Materialize.Output,
			RemoteRef:   "file://" + csvPath,
			SizeBytes:   info.Size(),
			ContentType: "text/csv",
		})

		log.Info("bridged tabular output to VFS",
			zap.String("source", src.ID),
			zap.String("path", src.Materialize.Output),
			zap.Int64("bytes", info.Size()),
		)
	}

	return mappings
}
