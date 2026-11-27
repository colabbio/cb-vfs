package vfs

import (
	"testing"
)

func TestParseS3URI_Valid(t *testing.T) {
	tests := []struct {
		uri    string
		bucket string
		key    string
	}{
		{"s3://my-bucket/path/to/file.bam", "my-bucket", "path/to/file.bam"},
		{"s3://genomics-data/sample001/tumor.vcf.gz", "genomics-data", "sample001/tumor.vcf.gz"},
		{"s3://ml-models/v2/weights.pt", "ml-models", "v2/weights.pt"},
		{"s3://bucket/key", "bucket", "key"},
	}

	for _, tt := range tests {
		bucket, key, err := parseS3URI(tt.uri)
		if err != nil {
			t.Errorf("parseS3URI(%q): unexpected error: %v", tt.uri, err)
			continue
		}
		if bucket != tt.bucket {
			t.Errorf("parseS3URI(%q): bucket = %q, want %q", tt.uri, bucket, tt.bucket)
		}
		if key != tt.key {
			t.Errorf("parseS3URI(%q): key = %q, want %q", tt.uri, key, tt.key)
		}
	}
}

func TestParseS3URI_Invalid(t *testing.T) {
	invalid := []string{
		"gs://bucket/key",    // wrong scheme
		"s3://",              // empty
		"s3://bucket",        // no key
		"s3:///key",          // empty bucket
		"s3://bucket/",       // empty key
		"http://example.com", // wrong scheme
		"",                   // empty string
	}

	for _, uri := range invalid {
		_, _, err := parseS3URI(uri)
		if err == nil {
			t.Errorf("parseS3URI(%q): expected error, got nil", uri)
		}
	}
}

func TestDispatchFetcher_S3Resolve(t *testing.T) {
	// Verify that the dispatch fetcher now resolves s3:// URIs
	// to the S3Fetcher instead of returning "not implemented".
	d := NewDispatchFetcher(nil, nil)

	fetcher, scheme, err := d.resolve("s3://bucket/key")
	if err != nil {
		t.Fatalf("resolve(s3://): unexpected error: %v", err)
	}
	if scheme != "s3" {
		t.Errorf("scheme = %q, want %q", scheme, "s3")
	}
	if fetcher == nil {
		t.Error("fetcher is nil, expected S3Fetcher")
	}
	if _, ok := fetcher.(*S3Fetcher); !ok {
		t.Errorf("fetcher type = %T, want *S3Fetcher", fetcher)
	}
}
