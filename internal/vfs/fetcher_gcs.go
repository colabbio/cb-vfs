package vfs

import (
	"context"
	"fmt"
	"io"
	"sync/atomic"

	gstorage "cloud.google.com/go/storage"
	"go.uber.org/zap"
)

// GCSFetcher implements RemoteFetcher for Google Cloud Storage objects.
// It reads byte ranges from GCS using the official Go client library,
// which transparently handles authentication via ADC (Application Default
// Credentials), GKE workload identity, and service account keys.
//
// Thread-safe for concurrent block reads.
type GCSFetcher struct {
	client     *gstorage.Client
	log        *zap.Logger
	fetchCount int64
	bytesRead  int64
}

// NewGCSFetcher creates a GCSFetcher using the provided GCS client.
// If client is nil, one is created using Application Default Credentials.
func NewGCSFetcher(client *gstorage.Client, log *zap.Logger) *GCSFetcher {
	return &GCSFetcher{
		client: client,
		log:    log,
	}
}

// Init lazily initialises the GCS client on first use.
func (f *GCSFetcher) Init(ctx context.Context) error {
	if f.client != nil {
		return nil
	}
	c, err := gstorage.NewClient(ctx)
	if err != nil {
		return fmt.Errorf("gcs fetcher: create client: %w", err)
	}
	f.client = c
	return nil
}

// FetchBlock reads a byte range from a GCS object.
// The remoteRef must be in the form "gs://bucket/object/path".
func (f *GCSFetcher) FetchBlock(ctx context.Context, remoteRef string, offset, length int64) ([]byte, error) {
	if err := f.Init(ctx); err != nil {
		return nil, err
	}

	bucket, object, err := parseGCSURI(remoteRef)
	if err != nil {
		return nil, err
	}

	obj := f.client.Bucket(bucket).Object(object)
	r, err := obj.NewRangeReader(ctx, offset, length)
	if err != nil {
		return nil, fmt.Errorf("gcs fetcher: range read gs://%s/%s offset=%d len=%d: %w",
			bucket, object, offset, length, err)
	}
	defer r.Close()

	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("gcs fetcher: read body: %w", err)
	}

	atomic.AddInt64(&f.fetchCount, 1)
	atomic.AddInt64(&f.bytesRead, int64(len(data)))

	return data, nil
}

// FileSize returns the size of a GCS object via its metadata.
func (f *GCSFetcher) FileSize(ctx context.Context, remoteRef string) (int64, error) {
	if err := f.Init(ctx); err != nil {
		return 0, err
	}

	bucket, object, err := parseGCSURI(remoteRef)
	if err != nil {
		return 0, err
	}

	attrs, err := f.client.Bucket(bucket).Object(object).Attrs(ctx)
	if err != nil {
		return 0, fmt.Errorf("gcs fetcher: attrs gs://%s/%s: %w", bucket, object, err)
	}
	return attrs.Size, nil
}

// FetchCount returns the total number of fetch operations.
func (f *GCSFetcher) FetchCount() int64 {
	return atomic.LoadInt64(&f.fetchCount)
}

// BytesRead returns the total bytes fetched from GCS.
func (f *GCSFetcher) BytesRead() int64 {
	return atomic.LoadInt64(&f.bytesRead)
}

// Close releases the underlying GCS client.
func (f *GCSFetcher) Close() error {
	if f.client != nil {
		return f.client.Close()
	}
	return nil
}

// ─── URI Parsing ─────────────────────────────────────────────────────────────

// parseGCSURI extracts bucket and object from a "gs://bucket/object" URI.
func parseGCSURI(uri string) (bucket, object string, err error) {
	const prefix = "gs://"
	if len(uri) <= len(prefix) || uri[:len(prefix)] != prefix {
		return "", "", fmt.Errorf("gcs fetcher: invalid URI (not gs://): %q", uri)
	}
	rest := uri[len(prefix):]
	for i := 0; i < len(rest); i++ {
		if rest[i] == '/' {
			bucket = rest[:i]
			object = rest[i+1:]
			if bucket == "" || object == "" {
				return "", "", fmt.Errorf("gcs fetcher: empty bucket or object in URI: %q", uri)
			}
			return bucket, object, nil
		}
	}
	return "", "", fmt.Errorf("gcs fetcher: no object path in URI: %q", uri)
}
