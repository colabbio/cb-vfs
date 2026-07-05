package dataset

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colabbio/medvfs/internal/config"
	"go.uber.org/zap"
)

// ─── Helpers ──────────────────────────────────────────────────────────────────

func writeCSV(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readCSV(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// ─── Tests ────────────────────────────────────────────────────────────────────

func TestDerivedView_LeftJoin(t *testing.T) {
	dir := t.TempDir()
	log := zap.NewNop()

	// Source 1: OMOP cohort (3 patients)
	writeCSV(t, dir, "patients.csv", `person_id,year_of_birth,gender
P001,1965,Female
P002,1972,Male
P003,1988,Female
`)

	// Source 2: Genomics manifest (only 2 patients)
	writeCSV(t, dir, "genomics.csv", `patient_id,file_path,format
P001,/data/P001_tumor.bam,bam
P003,/data/P003_germline.vcf,vcf
`)

	sources := []config.SourceConfig{
		{ID: "omop-cohort", Materialize: config.MaterializeConfig{Output: "patients.csv"}},
		{ID: "genomics", Materialize: config.MaterializeConfig{Output: "genomics.csv"}},
	}

	views := []config.DerivedViewConfig{{
		ID: "cohort-with-genomics",
		Sources: []config.ViewSourceRef{
			{Ref: "omop-cohort", Key: "person_id"},
			{Ref: "genomics", Key: "patient_id"},
		},
		JoinType: "left",
		Columns: []string{
			"omop-cohort.person_id AS patient_id",
			"omop-cohort.year_of_birth",
			"omop-cohort.gender",
			"genomics.file_path AS bam_path",
			"genomics.format",
		},
		Output: "training_features.csv",
	}}

	if err := ExecuteDerivedViews(views, sources, dir, log); err != nil {
		t.Fatalf("ExecuteDerivedViews: %v", err)
	}

	result := readCSV(t, dir, "training_features.csv")

	// Verify header
	lines := strings.Split(strings.TrimSpace(result), "\n")
	if len(lines) != 4 { // header + 3 data rows
		t.Fatalf("expected 4 lines (header + 3 rows), got %d:\n%s", len(lines), result)
	}

	if lines[0] != "patient_id,year_of_birth,gender,bam_path,format" {
		t.Errorf("unexpected header: %s", lines[0])
	}

	// P001 has genomics data
	if !strings.Contains(lines[1], "P001") || !strings.Contains(lines[1], "/data/P001_tumor.bam") {
		t.Errorf("P001 row should have genomics data: %s", lines[1])
	}

	// P002 has NO genomics data (left join → empty)
	if !strings.Contains(lines[2], "P002") {
		t.Errorf("P002 should be present in left join: %s", lines[2])
	}
	parts := strings.Split(lines[2], ",")
	if len(parts) >= 5 && parts[3] != "" {
		t.Errorf("P002 bam_path should be empty, got %q", parts[3])
	}

	// P003 has genomics data
	if !strings.Contains(lines[3], "P003") || !strings.Contains(lines[3], "vcf") {
		t.Errorf("P003 row should have genomics data: %s", lines[3])
	}
}

func TestDerivedView_InnerJoin(t *testing.T) {
	dir := t.TempDir()
	log := zap.NewNop()

	writeCSV(t, dir, "ehr.csv", `id,diagnosis
P001,lung_cancer
P002,breast_cancer
P003,melanoma
`)

	writeCSV(t, dir, "slides.csv", `patient_id,slide_path
P001,/slides/P001_HE.svs
P003,/slides/P003_HE.svs
`)

	sources := []config.SourceConfig{
		{ID: "ehr", Materialize: config.MaterializeConfig{Output: "ehr.csv"}},
		{ID: "slides", Materialize: config.MaterializeConfig{Output: "slides.csv"}},
	}

	views := []config.DerivedViewConfig{{
		ID: "matched-cohort",
		Sources: []config.ViewSourceRef{
			{Ref: "ehr", Key: "id"},
			{Ref: "slides", Key: "patient_id"},
		},
		JoinType: "inner",
		Columns: []string{
			"ehr.id AS patient_id",
			"ehr.diagnosis",
			"slides.slide_path",
		},
		Output: "matched.csv",
	}}

	if err := ExecuteDerivedViews(views, sources, dir, log); err != nil {
		t.Fatalf("ExecuteDerivedViews: %v", err)
	}

	result := readCSV(t, dir, "matched.csv")
	lines := strings.Split(strings.TrimSpace(result), "\n")

	// Inner join: only P001 and P003 (both present in both sources)
	if len(lines) != 3 { // header + 2 rows
		t.Fatalf("expected 3 lines (inner join), got %d:\n%s", len(lines), result)
	}

	if !strings.Contains(result, "P001") || !strings.Contains(result, "P003") {
		t.Error("inner join should contain P001 and P003")
	}
	if strings.Contains(result, "P002") {
		t.Error("inner join should NOT contain P002 (missing from slides)")
	}
}

func TestDerivedView_FullJoin(t *testing.T) {
	dir := t.TempDir()
	log := zap.NewNop()

	writeCSV(t, dir, "omop.csv", `patient_id,age
P001,59
P002,52
`)

	writeCSV(t, dir, "fhir.csv", `id,gender
P002,Male
P003,Female
`)

	sources := []config.SourceConfig{
		{ID: "omop", Materialize: config.MaterializeConfig{Output: "omop.csv"}},
		{ID: "fhir", Materialize: config.MaterializeConfig{Output: "fhir.csv"}},
	}

	views := []config.DerivedViewConfig{{
		ID: "full-cohort",
		Sources: []config.ViewSourceRef{
			{Ref: "omop", Key: "patient_id"},
			{Ref: "fhir", Key: "id"},
		},
		JoinType: "full",
		Output:   "full.csv",
	}}

	if err := ExecuteDerivedViews(views, sources, dir, log); err != nil {
		t.Fatalf("ExecuteDerivedViews: %v", err)
	}

	result := readCSV(t, dir, "full.csv")
	lines := strings.Split(strings.TrimSpace(result), "\n")

	// Full join: P001 + P002 + P003 (union of keys)
	if len(lines) != 4 { // header + 3 rows
		t.Fatalf("expected 4 lines (full join), got %d:\n%s", len(lines), result)
	}

	if !strings.Contains(result, "P001") || !strings.Contains(result, "P002") || !strings.Contains(result, "P003") {
		t.Error("full join should contain all three patients")
	}
}

func TestDerivedView_ThreeSourceJoin(t *testing.T) {
	dir := t.TempDir()
	log := zap.NewNop()

	writeCSV(t, dir, "demographics.csv", `patient_id,age,gender
P001,59,Female
P002,52,Male
`)

	writeCSV(t, dir, "imaging.csv", `patient_id,slide_path,stain
P001,/slides/P001.svs,HE
P002,/slides/P002.svs,IHC
`)

	writeCSV(t, dir, "genomics.csv", `patient_id,bam_path,mutation_count
P001,/bam/P001.bam,142
P002,/bam/P002.bam,87
`)

	sources := []config.SourceConfig{
		{ID: "demo", Materialize: config.MaterializeConfig{Output: "demographics.csv"}},
		{ID: "imaging", Materialize: config.MaterializeConfig{Output: "imaging.csv"}},
		{ID: "genomics", Materialize: config.MaterializeConfig{Output: "genomics.csv"}},
	}

	views := []config.DerivedViewConfig{{
		ID: "multimodal",
		Sources: []config.ViewSourceRef{
			{Ref: "demo", Key: "patient_id"},
			{Ref: "imaging", Key: "patient_id"},
			{Ref: "genomics", Key: "patient_id"},
		},
		JoinType: "inner",
		Columns: []string{
			"demo.patient_id",
			"demo.age",
			"demo.gender",
			"imaging.slide_path",
			"imaging.stain",
			"genomics.bam_path",
			"genomics.mutation_count",
		},
		Output: "multimodal_features.csv",
	}}

	if err := ExecuteDerivedViews(views, sources, dir, log); err != nil {
		t.Fatalf("ExecuteDerivedViews: %v", err)
	}

	result := readCSV(t, dir, "multimodal_features.csv")
	lines := strings.Split(strings.TrimSpace(result), "\n")

	if len(lines) != 3 { // header + 2 rows
		t.Fatalf("expected 3 lines, got %d:\n%s", len(lines), result)
	}

	header := lines[0]
	expected := "patient_id,age,gender,slide_path,stain,bam_path,mutation_count"
	if header != expected {
		t.Errorf("header mismatch:\n  got:  %s\n  want: %s", header, expected)
	}

	// P001 row should have all data
	if !strings.Contains(lines[1], "P001") || !strings.Contains(lines[1], "/slides/P001.svs") || !strings.Contains(lines[1], "/bam/P001.bam") {
		t.Errorf("P001 row incomplete: %s", lines[1])
	}
}

func TestDerivedView_AllColumnsDefault(t *testing.T) {
	dir := t.TempDir()
	log := zap.NewNop()

	writeCSV(t, dir, "a.csv", `key,val_a
1,alpha
2,beta
`)

	writeCSV(t, dir, "b.csv", `key,val_b
1,one
2,two
`)

	sources := []config.SourceConfig{
		{ID: "a", Materialize: config.MaterializeConfig{Output: "a.csv"}},
		{ID: "b", Materialize: config.MaterializeConfig{Output: "b.csv"}},
	}

	views := []config.DerivedViewConfig{{
		ID: "auto-cols",
		Sources: []config.ViewSourceRef{
			{Ref: "a", Key: "key"},
			{Ref: "b", Key: "key"},
		},
		// No Columns specified → all columns prefixed
		Output: "auto.csv",
	}}

	if err := ExecuteDerivedViews(views, sources, dir, log); err != nil {
		t.Fatalf("ExecuteDerivedViews: %v", err)
	}

	result := readCSV(t, dir, "auto.csv")
	lines := strings.Split(strings.TrimSpace(result), "\n")

	// Header should be prefixed: a.key, a.val_a, b.key, b.val_b
	expected := "a.key,a.val_a,b.key,b.val_b"
	if lines[0] != expected {
		t.Errorf("auto-columns header mismatch:\n  got:  %s\n  want: %s", lines[0], expected)
	}
}

func TestDerivedView_ErrorTooFewSources(t *testing.T) {
	dir := t.TempDir()
	log := zap.NewNop()

	sources := []config.SourceConfig{
		{ID: "a", Materialize: config.MaterializeConfig{Output: "a.csv"}},
	}

	views := []config.DerivedViewConfig{{
		ID: "bad",
		Sources: []config.ViewSourceRef{
			{Ref: "a", Key: "key"},
		},
		Output: "bad.csv",
	}}

	err := ExecuteDerivedViews(views, sources, dir, log)
	if err == nil {
		t.Fatal("expected error for single source join")
	}
	if !strings.Contains(err.Error(), "at least 2 sources") {
		t.Errorf("unexpected error: %v", err)
	}
}
