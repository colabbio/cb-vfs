package vfs

import (
	"testing"
)

// ─── parseGCSURI Tests ───────────────────────────────────────────────────────

func TestParseGCSURI(t *testing.T) {
	tests := []struct {
		input      string
		wantBucket string
		wantObject string
		wantErr    bool
	}{
		{"gs://my-bucket/path/to/object.dcm", "my-bucket", "path/to/object.dcm", false},
		{"gs://bucket/key", "bucket", "key", false},
		{"gs://b/o", "b", "o", false},
		{"gs://bucket-only", "", "", true},   // no object path
		{"gs://", "", "", true},              // empty
		{"s3://bucket/key", "", "", true},    // wrong scheme
		{"http://example.com", "", "", true}, // wrong scheme
		{"gs:///no-bucket", "", "", true},    // empty bucket
		{"gs://bucket/", "", "", true},       // empty object
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			bucket, object, err := parseGCSURI(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseGCSURI(%q): err = %v, wantErr = %v", tt.input, err, tt.wantErr)
			}
			if bucket != tt.wantBucket {
				t.Errorf("bucket = %q, want %q", bucket, tt.wantBucket)
			}
			if object != tt.wantObject {
				t.Errorf("object = %q, want %q", object, tt.wantObject)
			}
		})
	}
}

// ─── GCSFetcher construction test (no real GCS) ──────────────────────────────

func TestGCSFetcher_NoClientReturnsError(t *testing.T) {
	// NewGCSFetcher with nil client delays init to first use.
	// We can't test actual fetch without GCS, but verify construction is OK.
	f := NewGCSFetcher(nil, nil)
	if f == nil {
		t.Fatal("NewGCSFetcher returned nil")
	}
	if f.FetchCount() != 0 {
		t.Errorf("initial FetchCount = %d, want 0", f.FetchCount())
	}
	if f.BytesRead() != 0 {
		t.Errorf("initial BytesRead = %d, want 0", f.BytesRead())
	}
}

// ─── DispatchFetcher GCS routing test ────────────────────────────────────────

func TestDispatchFetcher_GCSSchemeResolvesOK(t *testing.T) {
	// Verify that gs:// scheme routes to the GCS fetcher (lazy init)
	d := NewDispatchFetcher(nil, nil)

	// resolve should return the GCS fetcher without error
	fetcher, scheme, err := d.resolve("gs://test-bucket/test-object.dcm")
	if err != nil {
		t.Fatalf("resolve(gs://...): unexpected error: %v", err)
	}
	if scheme != "gcs" {
		t.Errorf("scheme = %q, want \"gcs\"", scheme)
	}
	if fetcher == nil {
		t.Fatal("fetcher is nil after gs:// resolve")
	}

	// Verify it's a *GCSFetcher
	if _, ok := fetcher.(*GCSFetcher); !ok {
		t.Errorf("fetcher type = %T, want *GCSFetcher", fetcher)
	}
}
