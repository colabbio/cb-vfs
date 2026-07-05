package genomics

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/colabbio/medvfs/internal/config"
	"go.uber.org/zap"
)

func TestBuildFileTree_FuseTree_NFS(t *testing.T) {
	// Create temp dir with fake BAM + index files
	tmpDir := t.TempDir()
	bamPath := filepath.Join(tmpDir, "P001_tumor.bam")
	baiPath := filepath.Join(tmpDir, "P001_tumor.bam.bai")

	if err := os.WriteFile(bamPath, make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(baiPath, make([]byte, 512), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config.SourceConfig{
		ID:   "test-genomics",
		Type: "genomics",
		Genomics: &config.GenomicsConfig{
			Storage:  "nfs",
			BasePath: tmpDir,
			Formats:  []string{"bam"},
		},
	}

	log, _ := zap.NewDevelopment()
	adapter, err := New(cfg, log)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	mappings, err := adapter.BuildFileTree(context.Background(), "", "fuse_tree")
	if err != nil {
		t.Fatalf("BuildFileTree: %v", err)
	}

	if len(mappings) != 2 {
		t.Fatalf("expected 2 mappings (bam + bai), got %d", len(mappings))
	}

	// Check the BAM mapping
	bam := mappings[0]
	if bam.LogicalPath != "P001_tumor/P001_tumor.bam" {
		t.Errorf("bam LogicalPath = %q, want P001_tumor/P001_tumor.bam", bam.LogicalPath)
	}
	if bam.RemoteRef != "file://"+bamPath {
		t.Errorf("bam RemoteRef = %q, want file://%s", bam.RemoteRef, bamPath)
	}
	if bam.SizeBytes != 4096 {
		t.Errorf("bam SizeBytes = %d, want 4096", bam.SizeBytes)
	}
	if bam.ContentType != "application/vnd.bam" {
		t.Errorf("bam ContentType = %q, want application/vnd.bam", bam.ContentType)
	}
	if bam.LocalPath != "" {
		t.Errorf("bam LocalPath should be empty in fuse_tree, got %q", bam.LocalPath)
	}

	// Check the BAI mapping
	bai := mappings[1]
	if bai.LogicalPath != "P001_tumor/P001_tumor.bam.bai" {
		t.Errorf("bai LogicalPath = %q, want P001_tumor/P001_tumor.bam.bai", bai.LogicalPath)
	}
	if bai.RemoteRef != "file://"+baiPath {
		t.Errorf("bai RemoteRef = %q, want file://%s", bai.RemoteRef, baiPath)
	}
	if bai.SizeBytes != 512 {
		t.Errorf("bai SizeBytes = %d, want 512", bai.SizeBytes)
	}
}

func TestBuildFileTree_SymlinkTree_NFS(t *testing.T) {
	tmpDir := t.TempDir()
	outDir := t.TempDir()

	vcfPath := filepath.Join(tmpDir, "SAMPLE01_variants.vcf.gz")
	tbiPath := filepath.Join(tmpDir, "SAMPLE01_variants.vcf.gz.tbi")

	if err := os.WriteFile(vcfPath, make([]byte, 1024), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tbiPath, make([]byte, 256), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config.SourceConfig{
		ID:   "test-vcf",
		Type: "genomics",
		Genomics: &config.GenomicsConfig{
			Storage:  "nfs",
			BasePath: tmpDir,
			Formats:  []string{"vcf"},
		},
	}

	log, _ := zap.NewDevelopment()
	adapter, err := New(cfg, log)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	mappings, err := adapter.BuildFileTree(context.Background(), outDir, "symlink_tree")
	if err != nil {
		t.Fatalf("BuildFileTree: %v", err)
	}

	if len(mappings) != 2 {
		t.Fatalf("expected 2 mappings (vcf + tbi), got %d", len(mappings))
	}

	// Check symlink was created
	vcfMapping := mappings[0]
	symlinkTarget, err := os.Readlink(vcfMapping.LocalPath)
	if err != nil {
		t.Fatalf("symlink read failed: %v", err)
	}
	// The symlink should resolve to the original file
	resolvedPath := filepath.Join(filepath.Dir(vcfMapping.LocalPath), symlinkTarget)
	absResolved, _ := filepath.Abs(resolvedPath)
	absOriginal, _ := filepath.Abs(vcfPath)
	if absResolved != absOriginal {
		t.Errorf("symlink resolves to %q, want %q", absResolved, absOriginal)
	}
}

func TestBuildFileTree_PatientIDRegex(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a file with a parseable patient ID
	bamPath := filepath.Join(tmpDir, "HOSP-42_tumor_WGS.bam")
	if err := os.WriteFile(bamPath, make([]byte, 256), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config.SourceConfig{
		ID:   "test-regex",
		Type: "genomics",
		Genomics: &config.GenomicsConfig{
			Storage:        "nfs",
			BasePath:       tmpDir,
			Formats:        []string{"bam"},
			PatientIDRegex: `^(HOSP-\d+)_`,
		},
	}

	log, _ := zap.NewDevelopment()
	adapter, err := New(cfg, log)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	mappings, err := adapter.BuildFileTree(context.Background(), "", "fuse_tree")
	if err != nil {
		t.Fatalf("BuildFileTree: %v", err)
	}

	if len(mappings) != 1 {
		t.Fatalf("expected 1 mapping, got %d", len(mappings))
	}

	// Patient ID should be extracted via regex
	if mappings[0].LogicalPath != "HOSP-42/HOSP-42_tumor_WGS.bam" {
		t.Errorf("LogicalPath = %q, want HOSP-42/HOSP-42_tumor_WGS.bam", mappings[0].LogicalPath)
	}
}

func TestBuildFileTree_MultipleFormats(t *testing.T) {
	tmpDir := t.TempDir()

	// Create BAM + FASTQ files
	os.WriteFile(filepath.Join(tmpDir, "sample_reads.fastq.gz"), make([]byte, 2048), 0o644)
	os.WriteFile(filepath.Join(tmpDir, "sample_aligned.bam"), make([]byte, 4096), 0o644)
	os.WriteFile(filepath.Join(tmpDir, "sample_aligned.bam.bai"), make([]byte, 128), 0o644)
	os.WriteFile(filepath.Join(tmpDir, "README.txt"), make([]byte, 64), 0o644) // should be ignored

	cfg := config.SourceConfig{
		ID:   "test-multi",
		Type: "genomics",
		Genomics: &config.GenomicsConfig{
			Storage:  "nfs",
			BasePath: tmpDir,
			Formats:  []string{"bam", "fastq"},
		},
	}

	log, _ := zap.NewDevelopment()
	adapter, err := New(cfg, log)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	mappings, err := adapter.BuildFileTree(context.Background(), "", "fuse_tree")
	if err != nil {
		t.Fatalf("BuildFileTree: %v", err)
	}

	// BAM (1) + BAI (1) + FASTQ (1) = 3 mappings; README ignored
	if len(mappings) != 3 {
		t.Fatalf("expected 3 mappings, got %d", len(mappings))
	}

	// Verify no .txt files leaked through
	for _, m := range mappings {
		if filepath.Ext(m.LogicalPath) == ".txt" {
			t.Errorf("unexpected .txt file in mappings: %s", m.LogicalPath)
		}
	}
}

func TestPathToRemoteRef(t *testing.T) {
	tests := []struct {
		path, storage, want string
	}{
		{"/mnt/data/P001.bam", "nfs", "file:///mnt/data/P001.bam"},
		{"gs://bucket/P001.bam", "gcs", "gs://bucket/P001.bam"},
		{"s3://bucket/P001.bam", "s3", "s3://bucket/P001.bam"},
		{"/fallback.bam", "unknown", "file:///fallback.bam"},
	}

	for _, tt := range tests {
		got := pathToRemoteRef(tt.path, tt.storage)
		if got != tt.want {
			t.Errorf("pathToRemoteRef(%q, %q) = %q, want %q", tt.path, tt.storage, got, tt.want)
		}
	}
}

func TestFormatToMIME(t *testing.T) {
	tests := []struct {
		format, want string
	}{
		{"bam", "application/vnd.bam"},
		{"cram", "application/vnd.cram"},
		{"vcf", "text/x-vnd.vcf"},
		{"fastq", "application/x-fastq"},
		{"bed", "text/x-bed"},
		{"hdf5", "application/x-hdf5"},
		{"unknown", "application/octet-stream"},
	}

	for _, tt := range tests {
		got := formatToMIME(tt.format)
		if got != tt.want {
			t.Errorf("formatToMIME(%q) = %q, want %q", tt.format, got, tt.want)
		}
	}
}
