package vfs

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// ─── LocalFetcher Tests ──────────────────────────────────────────────────────

func TestLocalFetcher_FetchBlock(t *testing.T) {
	// Create a temp file with known content
	dir := t.TempDir()
	path := filepath.Join(dir, "test.dcm")
	content := []byte("ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}

	f := NewLocalFetcher()
	ref := "file://" + path

	tests := []struct {
		name     string
		offset   int64
		length   int64
		expected string
	}{
		{"first 4 bytes", 0, 4, "ABCD"},
		{"middle range", 10, 5, "KLMNO"},
		{"last bytes", 30, 6, "456789"},
		{"single byte", 0, 1, "A"},
		{"beyond file clamps", 34, 10, "89"}, // file is 36 bytes, reads 2
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := f.FetchBlock(context.Background(), ref, tt.offset, tt.length)
			if err != nil {
				t.Fatalf("FetchBlock: %v", err)
			}
			if string(data) != tt.expected {
				t.Errorf("got %q, want %q", string(data), tt.expected)
			}
		})
	}

	// Verify counters
	if f.FetchCount() != int64(len(tests)) {
		t.Errorf("FetchCount = %d, want %d", f.FetchCount(), len(tests))
	}
	if f.BytesRead() <= 0 {
		t.Error("BytesRead should be > 0")
	}
}

func TestLocalFetcher_FileSize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sized.bin")
	content := make([]byte, 4096)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}

	f := NewLocalFetcher()
	size, err := f.FileSize(context.Background(), "file://"+path)
	if err != nil {
		t.Fatalf("FileSize: %v", err)
	}
	if size != 4096 {
		t.Errorf("size = %d, want 4096", size)
	}
}

func TestLocalFetcher_InvalidURI(t *testing.T) {
	f := NewLocalFetcher()

	// Not a file:// URI
	_, err := f.FetchBlock(context.Background(), "http://example.com", 0, 10)
	if err == nil {
		t.Error("expected error for non-file:// URI")
	}

	// Non-existent file
	_, err = f.FetchBlock(context.Background(), "file:///nonexistent/path.dcm", 0, 10)
	if err == nil {
		t.Error("expected error for non-existent file")
	}
}

// ─── DispatchFetcher Tests ───────────────────────────────────────────────────

func TestDispatchFetcher_RoutesFileScheme(t *testing.T) {
	// Create a temp file
	dir := t.TempDir()
	path := filepath.Join(dir, "dispatch_test.bin")
	content := []byte("DISPATCH_TEST_CONTENT")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}

	d := NewDispatchFetcher(nil, nil)

	// file:// should route to LocalFetcher
	data, err := d.FetchBlock(context.Background(), "file://"+path, 0, 8)
	if err != nil {
		t.Fatalf("file:// dispatch: %v", err)
	}
	if string(data) != "DISPATCH" {
		t.Errorf("got %q, want %q", string(data), "DISPATCH")
	}

	if d.TotalFetchOps() != 1 {
		t.Errorf("TotalFetchOps = %d, want 1", d.TotalFetchOps())
	}
}

func TestDispatchFetcher_RejectsUnresolvedSchemes(t *testing.T) {
	d := NewDispatchFetcher(nil, nil)

	// omero:// should be resolved before reaching the fetcher
	_, err := d.FetchBlock(context.Background(), "omero://image/42", 0, 10)
	if err == nil {
		t.Error("expected error for unresolved omero:// scheme")
	}

	// wado-rs:// same
	_, err = d.FetchBlock(context.Background(), "wado-rs://host/study/1", 0, 10)
	if err == nil {
		t.Error("expected error for unresolved wado-rs:// scheme")
	}
}

func TestDispatchFetcher_UnsupportedSchemes(t *testing.T) {
	d := NewDispatchFetcher(nil, nil)

	_, err := d.FetchBlock(context.Background(), "gs://bucket/object", 0, 10)
	if err == nil {
		t.Error("expected error for gs:// (not yet implemented)")
	}

	_, err = d.FetchBlock(context.Background(), "s3://bucket/key", 0, 10)
	if err == nil {
		t.Error("expected error for s3:// (not yet implemented)")
	}
}

func TestDispatchFetcher_FileSize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "size_test.bin")
	content := make([]byte, 2048)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}

	d := NewDispatchFetcher(nil, nil)
	size, err := d.FileSize(context.Background(), "file://"+path)
	if err != nil {
		t.Fatalf("FileSize: %v", err)
	}
	if size != 2048 {
		t.Errorf("size = %d, want 2048", size)
	}
}

// ─── parseFileURI Tests ──────────────────────────────────────────────────────

func TestParseFileURI(t *testing.T) {
	tests := []struct {
		input   string
		want    string
		wantErr bool
	}{
		{"file:///data/dicom/scan.dcm", "/data/dicom/scan.dcm", false},
		{"file:///tmp/test", "/tmp/test", false},
		{"file://relative/path", "relative/path", false},
		{"http://example.com", "", true},
		{"file://", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := parseFileURI(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseFileURI(%q): err = %v, wantErr = %v", tt.input, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("parseFileURI(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// ─── BlockManager.BlockSize Tests ────────────────────────────────────────────

func TestBlockManager_BlockSize(t *testing.T) {
	dir := t.TempDir()
	f := NewHTTPFetcher(nil)

	bm, err := NewBlockManager(8*1024*1024, dir, DefaultCacheMaxBytes, f, nil)
	if err != nil {
		t.Fatal(err)
	}
	if bm.BlockSize() != 8*1024*1024 {
		t.Errorf("BlockSize = %d, want %d", bm.BlockSize(), 8*1024*1024)
	}

	// Default
	bm2, err := NewBlockManager(0, dir, 0, f, nil)
	if err != nil {
		t.Fatal(err)
	}
	if bm2.BlockSize() != DefaultBlockSize {
		t.Errorf("default BlockSize = %d, want %d", bm2.BlockSize(), DefaultBlockSize)
	}
}
