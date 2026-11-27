package dataset

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/colabbio/medvfs/internal/adapters"
	"github.com/colabbio/medvfs/internal/config"
	"go.uber.org/zap/zaptest"
)

// ─── Splits Tests (G2) ──────────────────────────────────────────────────────

func TestLoadSplits(t *testing.T) {
	csv := "patient_id,split\nP001,train\nP002,val\nP003,test\nP004,train\n"
	csvPath := writeTempCSV(t, csv)

	splits, err := LoadSplits(context.Background(), csvPath, "", "")
	if err != nil {
		t.Fatalf("LoadSplits: %v", err)
	}

	if len(splits) != 4 {
		t.Errorf("expected 4 entries, got %d", len(splits))
	}
	if splits["P001"] != "train" {
		t.Errorf("P001: expected train, got %s", splits["P001"])
	}
	if splits["P002"] != "val" {
		t.Errorf("P002: expected val, got %s", splits["P002"])
	}
	if splits["P003"] != "test" {
		t.Errorf("P003: expected test, got %s", splits["P003"])
	}
}

func TestLoadSplits_CustomColumns(t *testing.T) {
	csv := "subject,partition\nSUB01,training\nSUB02,validation\n"
	csvPath := writeTempCSV(t, csv)

	splits, err := LoadSplits(context.Background(), csvPath, "subject", "partition")
	if err != nil {
		t.Fatalf("LoadSplits: %v", err)
	}

	if splits["SUB01"] != "training" {
		t.Errorf("SUB01: expected training, got %s", splits["SUB01"])
	}
}

func TestApplySplits(t *testing.T) {
	mappings := []adapters.FileMapping{
		{LogicalPath: "P001/slide.svs"},
		{LogicalPath: "P002/slide.svs"},
		{LogicalPath: "P003/slide.svs"},
		{LogicalPath: "UNKNOWN/slide.svs"},
	}

	splits := SplitMap{
		"P001": "train",
		"P002": "val",
		"P003": "test",
	}

	result := ApplySplits(mappings, splits)

	expected := map[string]string{
		"P001":    "train/P001/slide.svs",
		"P002":    "val/P002/slide.svs",
		"P003":    "test/P003/slide.svs",
		"UNKNOWN": "unassigned/UNKNOWN/slide.svs",
	}

	for _, m := range result {
		pid := extractPatientID(m.LogicalPath)
		// The split is the first component now
		parts := splitPath(m.LogicalPath)
		if parts[0] == "unassigned" {
			pid = parts[1]
		} else {
			pid = parts[1]
		}
		if exp, ok := expected[pid]; ok {
			if m.LogicalPath != exp {
				t.Errorf("patient %s: expected %s, got %s", pid, exp, m.LogicalPath)
			}
		}
	}
}

// ─── Patient Linking Tests (G4) ──────────────────────────────────────────────

func TestLoadPatientLinks(t *testing.T) {
	csv := "canonical_id,omero_id,genomics_id,fhir_id\nLUNG-001,Patient_001,P001,fhir-42\nLUNG-002,Patient_002,P002,fhir-43\n"
	csvPath := writeTempCSV(t, csv)

	links, err := LoadPatientLinks(context.Background(), csvPath)
	if err != nil {
		t.Fatalf("LoadPatientLinks: %v", err)
	}

	if len(links.Links) != 2 {
		t.Errorf("expected 2 links, got %d", len(links.Links))
	}

	if len(links.Columns) != 3 {
		t.Errorf("expected 3 source columns, got %d: %v", len(links.Columns), links.Columns)
	}

	link, ok := links.ByID["LUNG-001"]
	if !ok {
		t.Fatal("LUNG-001 not found")
	}

	if link.SourceIDs["omero_id"] != "Patient_001" {
		t.Errorf("expected Patient_001, got %s", link.SourceIDs["omero_id"])
	}
	if link.SourceIDs["genomics_id"] != "P001" {
		t.Errorf("expected P001, got %s", link.SourceIDs["genomics_id"])
	}
}

func TestApplyUnifiedLayout(t *testing.T) {
	// Setup links CSV
	csv := "canonical_id,omero_id,genomics_id\nLUNG-001,Patient_001,P001\n"
	csvPath := writeTempCSV(t, csv)
	links, err := LoadPatientLinks(context.Background(), csvPath)
	if err != nil {
		t.Fatalf("LoadPatientLinks: %v", err)
	}

	sourceMappings := map[string][]adapters.FileMapping{
		"omero_pathology": {
			{LogicalPath: "Patient_001/slide_HE.svs", RemoteRef: "omero://1234"},
		},
		"genomics_wgs": {
			{LogicalPath: "P001/tumor.bam", RemoteRef: "gs://bucket/P001/tumor.bam"},
		},
	}

	result := ApplyUnifiedLayout(sourceMappings, links)

	if len(result) != 2 {
		t.Fatalf("expected 2 mappings, got %d", len(result))
	}

	// Check that both are under LUNG-001
	for _, m := range result {
		pid := extractPatientID(m.LogicalPath)
		if pid != "LUNG-001" {
			t.Errorf("expected canonical ID LUNG-001, got %s in path %s", pid, m.LogicalPath)
		}
	}
}

// ─── Versioning Tests (G5) ───────────────────────────────────────────────────

func TestGenerateManifest(t *testing.T) {
	sourceMappings := map[string][]adapters.FileMapping{
		"omero": {
			{LogicalPath: "train/P001/slide.svs", SizeBytes: 1000},
			{LogicalPath: "val/P002/slide.svs", SizeBytes: 2000},
		},
		"genomics": {
			{LogicalPath: "train/P001/tumor.bam", SizeBytes: 50000},
		},
	}

	manifest := GenerateManifest("lung_v3", "exp-042", sourceMappings)

	if manifest.DatasetID != "lung_v3" {
		t.Errorf("expected lung_v3, got %s", manifest.DatasetID)
	}
	if manifest.FileCount != 3 {
		t.Errorf("expected 3 files, got %d", manifest.FileCount)
	}
	if manifest.TotalBytes != 53000 {
		t.Errorf("expected 53000 bytes, got %d", manifest.TotalBytes)
	}
	if manifest.Sources["omero"] != 2 {
		t.Errorf("expected 2 omero files, got %d", manifest.Sources["omero"])
	}
	if manifest.ManifestSHA256 == "" {
		t.Error("manifest hash should not be empty")
	}
	if manifest.CreatedAt == "" {
		t.Error("created_at should not be empty")
	}

	// Test determinism
	manifest2 := GenerateManifest("lung_v3", "exp-042", sourceMappings)
	if manifest.ManifestSHA256 != manifest2.ManifestSHA256 {
		t.Error("manifest hash should be deterministic")
	}
}

func TestManifestToJSON(t *testing.T) {
	manifest := &DatasetManifest{
		DatasetID:      "test",
		FileCount:      42,
		ManifestSHA256: "abc123",
	}

	data, err := ManifestToJSON(manifest)
	if err != nil {
		t.Fatalf("ManifestToJSON: %v", err)
	}

	if len(data) == 0 {
		t.Error("JSON output should not be empty")
	}
}

// ─── Bridge Tests (G6) ──────────────────────────────────────────────────────

func TestBridgeTabularOutputs(t *testing.T) {
	outDir := t.TempDir()

	// Simulate FHIR output CSV
	fhirCSV := filepath.Join(outDir, "ehr", "patients.csv")
	os.MkdirAll(filepath.Dir(fhirCSV), 0o755)
	os.WriteFile(fhirCSV, []byte("id,name\n1,John\n"), 0o644)

	sources := []config.SourceConfig{
		{
			ID:   "fhir_ehr",
			Type: "fhir",
			Materialize: config.MaterializeConfig{
				Output: "ehr/patients.csv",
			},
		},
		{
			ID:   "omero_wsi",
			Type: "omero",
			Materialize: config.MaterializeConfig{
				Output: "images/",
			},
		},
	}

	log := zaptest.NewLogger(t)
	mappings := BridgeTabularOutputs(outDir, sources, log)

	if len(mappings) != 1 {
		t.Errorf("expected 1 bridged mapping (only FHIR), got %d", len(mappings))
	}

	if len(mappings) > 0 {
		if mappings[0].LogicalPath != "ehr/patients.csv" {
			t.Errorf("expected ehr/patients.csv, got %s", mappings[0].LogicalPath)
		}
		if mappings[0].ContentType != "text/csv" {
			t.Errorf("expected text/csv, got %s", mappings[0].ContentType)
		}
	}
}

func TestBridgeTabularOutputs_NoFHIR(t *testing.T) {
	outDir := t.TempDir()
	sources := []config.SourceConfig{
		{ID: "omero", Type: "omero"},
	}

	log := zaptest.NewLogger(t)
	mappings := BridgeTabularOutputs(outDir, sources, log)

	if len(mappings) != 0 {
		t.Errorf("expected 0 bridged mappings, got %d", len(mappings))
	}
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

func writeTempCSV(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.csv")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func splitPath(p string) []string {
	var parts []string
	for p != "" {
		dir, file := filepath.Split(p)
		if file != "" {
			parts = append([]string{file}, parts...)
		}
		p = filepath.Clean(dir)
		if p == "." || p == "/" {
			break
		}
	}
	return parts
}
