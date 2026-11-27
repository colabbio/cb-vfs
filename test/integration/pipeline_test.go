package integration

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/colabbio/medvfs/internal/adapters"
	"github.com/colabbio/medvfs/internal/config"
	"github.com/colabbio/medvfs/internal/dataset"
	"go.uber.org/zap/zaptest"
)

// TestEndToEnd_FilesSplitsLinkingVersioning simulates the full ML pipeline:
//
//  1. Static files adapter scans NFS directory
//  2. FHIR/OMOP bridge picks up tabular outputs
//  3. Patient linking unifies views
//  4. Splits partition into train/val/test
//  5. Manifest is generated for reproducibility
//
// This test does NOT mount FUSE — it validates the data transformation pipeline.
func TestEndToEnd_FilesSplitsLinkingVersioning(t *testing.T) {
	log := zaptest.NewLogger(t)

	// ── Setup: create test data ──────────────────────────────────────────

	// Dataset root
	root := t.TempDir()

	// Simulate WSI files from "omero" source (2 patients)
	mkFile(t, root, "wsi/Patient_001/slide_HE.svs", 2400000)
	mkFile(t, root, "wsi/Patient_002/slide_HE.svs", 1800000)
	mkFile(t, root, "wsi/Patient_003/slide_HE.svs", 3100000)

	// Simulate genomics files from "genomics" source (same patients, different IDs)
	mkFile(t, root, "genomics/P001/tumor_WGS.bam", 52000000)
	mkFile(t, root, "genomics/P001/tumor_WGS.bam.bai", 800000)
	mkFile(t, root, "genomics/P002/tumor_WGS.bam", 48000000)
	mkFile(t, root, "genomics/P003/tumor_WGS.bam", 55000000)

	// Simulate static metadata files
	mkFile(t, root, "metadata/labels.csv", 0)
	mkFile(t, root, "metadata/config.yaml", 0)
	mkFile(t, root, "metadata/pretrained.pt", 125000000)

	// Simulate FHIR output (tabular, already materialised)
	outDir := filepath.Join(root, "output")
	mkFileContent(t, outDir, "ehr/patients.csv", "patient_id,diagnosis\nLUNG-001,NSCLC\nLUNG-002,SCLC\nLUNG-003,NSCLC\n")

	// Splits CSV
	mkFileContent(t, root, "splits.csv", "patient_id,split\nLUNG-001,train\nLUNG-002,val\nLUNG-003,test\n")

	// Patient links CSV
	mkFileContent(t, root, "patient_map.csv", "canonical_id,omero_id,genomics_id\nLUNG-001,Patient_001,P001\nLUNG-002,Patient_002,P002\nLUNG-003,Patient_003,P003\n")

	// ── Step 1: Scan sources ─────────────────────────────────────────────

	// Simulate what BuildFileTree returns for each source
	wsiMappings := []adapters.FileMapping{
		{LogicalPath: "Patient_001/slide_HE.svs", RemoteRef: "file://" + filepath.Join(root, "wsi/Patient_001/slide_HE.svs"), SizeBytes: 2400000},
		{LogicalPath: "Patient_002/slide_HE.svs", RemoteRef: "file://" + filepath.Join(root, "wsi/Patient_002/slide_HE.svs"), SizeBytes: 1800000},
		{LogicalPath: "Patient_003/slide_HE.svs", RemoteRef: "file://" + filepath.Join(root, "wsi/Patient_003/slide_HE.svs"), SizeBytes: 3100000},
	}

	genomicsMappings := []adapters.FileMapping{
		{LogicalPath: "P001/tumor_WGS.bam", RemoteRef: "file://" + filepath.Join(root, "genomics/P001/tumor_WGS.bam"), SizeBytes: 52000000},
		{LogicalPath: "P001/tumor_WGS.bam.bai", RemoteRef: "file://" + filepath.Join(root, "genomics/P001/tumor_WGS.bam.bai"), SizeBytes: 800000},
		{LogicalPath: "P002/tumor_WGS.bam", RemoteRef: "file://" + filepath.Join(root, "genomics/P002/tumor_WGS.bam"), SizeBytes: 48000000},
		{LogicalPath: "P003/tumor_WGS.bam", RemoteRef: "file://" + filepath.Join(root, "genomics/P003/tumor_WGS.bam"), SizeBytes: 55000000},
	}

	sourceMappings := map[string][]adapters.FileMapping{
		"omero_pathology": wsiMappings,
		"genomics_wgs":    genomicsMappings,
	}

	// ── Step 2: Bridge FHIR output (G6) ──────────────────────────────────

	sources := []config.SourceConfig{
		{ID: "ehr_data", Type: "fhir", Materialize: config.MaterializeConfig{Output: "ehr/patients.csv"}},
	}
	bridged := dataset.BridgeTabularOutputs(outDir, sources, log)
	if len(bridged) != 1 {
		t.Fatalf("expected 1 bridged file, got %d", len(bridged))
	}
	sourceMappings["_tabular_bridge"] = bridged

	t.Logf("✓ Step 2: Bridged %d tabular outputs", len(bridged))

	// ── Step 3: Patient linking (G4) ─────────────────────────────────────

	links, err := dataset.LoadPatientLinks(context.Background(), filepath.Join(root, "patient_map.csv"))
	if err != nil {
		t.Fatalf("LoadPatientLinks: %v", err)
	}

	unified := dataset.ApplyUnifiedLayout(sourceMappings, links)
	if len(unified) == 0 {
		t.Fatal("unified layout produced no mappings")
	}

	// Verify all mappings use canonical IDs
	for _, m := range unified {
		parts := splitPath(m.LogicalPath)
		if len(parts) == 0 {
			continue
		}
		pid := parts[0]
		if pid != "LUNG-001" && pid != "LUNG-002" && pid != "LUNG-003" && pid != "ehr" {
			t.Errorf("expected canonical patient ID (LUNG-*), got %q in path %s", pid, m.LogicalPath)
		}
	}

	t.Logf("✓ Step 3: Patient linking produced %d unified mappings", len(unified))

	// Replace with unified view
	sourceMappings = map[string][]adapters.FileMapping{"_unified": unified}

	// ── Step 4: Apply splits (G2) ────────────────────────────────────────

	splits, err := dataset.LoadSplits(context.Background(), filepath.Join(root, "splits.csv"), "", "")
	if err != nil {
		t.Fatalf("LoadSplits: %v", err)
	}

	for srcID, mappings := range sourceMappings {
		sourceMappings[srcID] = dataset.ApplySplits(mappings, splits)
	}

	// Count split distribution
	splitCounts := map[string]int{}
	for _, ms := range sourceMappings {
		for _, m := range ms {
			parts := splitPath(m.LogicalPath)
			if len(parts) > 0 {
				splitCounts[parts[0]]++
			}
		}
	}

	if splitCounts["train"] == 0 {
		t.Error("expected train split to have files")
	}
	if splitCounts["val"] == 0 {
		t.Error("expected val split to have files")
	}
	if splitCounts["test"] == 0 {
		t.Error("expected test split to have files")
	}

	t.Logf("✓ Step 4: Splits applied — train:%d, val:%d, test:%d",
		splitCounts["train"], splitCounts["val"], splitCounts["test"])

	// ── Step 5: Generate manifest (G5) ───────────────────────────────────

	manifest := dataset.GenerateManifest("lung_cancer_v3", "exp-integration-001", sourceMappings)

	if manifest.DatasetID != "lung_cancer_v3" {
		t.Errorf("expected lung_cancer_v3, got %s", manifest.DatasetID)
	}
	if manifest.ManifestSHA256 == "" {
		t.Error("manifest hash should not be empty")
	}
	if manifest.FileCount == 0 {
		t.Error("manifest should have files")
	}

	data, err := dataset.ManifestToJSON(manifest)
	if err != nil {
		t.Fatalf("ManifestToJSON: %v", err)
	}

	t.Logf("✓ Step 5: Manifest generated — %d files, %d bytes, sha=%s",
		manifest.FileCount, manifest.TotalBytes, manifest.ManifestSHA256[:12])

	// ── Summary ──────────────────────────────────────────────────────────

	t.Logf("\n"+
		"═══════════════════════════════════════════════\n"+
		" Integration Test Summary\n"+
		"═══════════════════════════════════════════════\n"+
		" Dataset ID:   %s\n"+
		" Run ID:       %s\n"+
		" Total files:  %d\n"+
		" Total bytes:  %d\n"+
		" SHA256:       %s\n"+
		" Splits:       train=%d val=%d test=%d\n"+
		" Manifest:     %d bytes JSON\n"+
		"═══════════════════════════════════════════════",
		manifest.DatasetID,
		manifest.RunID,
		manifest.FileCount,
		manifest.TotalBytes,
		manifest.ManifestSHA256[:16],
		splitCounts["train"], splitCounts["val"], splitCounts["test"],
		len(data),
	)
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

func mkFile(t *testing.T, root, relPath string, size int) {
	t.Helper()
	path := filepath.Join(root, relPath)
	os.MkdirAll(filepath.Dir(path), 0o755)
	data := make([]byte, size)
	os.WriteFile(path, data, 0o644)
}

func mkFileContent(t *testing.T, root, relPath, content string) {
	t.Helper()
	path := filepath.Join(root, relPath)
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, []byte(content), 0o644)
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
