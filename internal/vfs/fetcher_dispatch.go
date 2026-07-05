package vfs

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"go.uber.org/zap"
)

// DispatchFetcher routes block fetch requests to the appropriate backend
// based on the URI scheme of the remote reference:
//
//   - file://   → LocalFetcher  (DICOM local, co-located NFS)
//   - http(s):// → HTTPFetcher  (OMERO, Orthanc DICOMweb, generic HTTP)
//   - gs://     → GCSFetcher   (Google Cloud Storage — future)
//   - s3://     → S3Fetcher    (AWS S3 — future)
//
// This enables the BlockManager and FUSE layer to be agnostic to the
// storage backend — all reads go through the same caching pipeline.
type DispatchFetcher struct {
	local   *LocalFetcher
	http    *HTTPFetcher
	gcs     *GCSFetcher
	gcsOnce sync.Once
	s3      *S3Fetcher
	s3Once  sync.Once
	log     *zap.Logger

	// Aggregate counters
	totalBytes atomic.Int64
	totalOps   atomic.Int64
}

// NewDispatchFetcher creates a multi-scheme fetcher.
// Pass nil for any fetcher to use defaults.
func NewDispatchFetcher(httpFetcher *HTTPFetcher, log *zap.Logger) *DispatchFetcher {
	if httpFetcher == nil {
		httpFetcher = NewHTTPFetcher(nil)
	}
	if log == nil {
		log, _ = zap.NewDevelopment()
	}

	return &DispatchFetcher{
		local: NewLocalFetcher(),
		http:  httpFetcher,
		log:   log,
	}
}

// FetchBlock dispatches the fetch to the correct backend based on URI scheme.
func (d *DispatchFetcher) FetchBlock(ctx context.Context, ref string, offset int64, length int64) ([]byte, error) {
	fetcher, scheme, err := d.resolve(ref)
	if err != nil {
		return nil, err
	}

	data, err := fetcher.FetchBlock(ctx, ref, offset, length)
	if err != nil {
		d.log.Debug("block fetch failed",
			zap.String("scheme", scheme),
			zap.String("ref", truncateRef(ref)),
			zap.Int64("offset", offset),
			zap.Error(err),
		)
		return nil, err
	}

	d.totalBytes.Add(int64(len(data)))
	d.totalOps.Add(1)
	return data, nil
}

// FileSize dispatches the size query to the correct backend.
func (d *DispatchFetcher) FileSize(ctx context.Context, ref string) (int64, error) {
	fetcher, _, err := d.resolve(ref)
	if err != nil {
		return 0, err
	}
	return fetcher.FileSize(ctx, ref)
}

// TotalBytesFetched returns aggregate bytes across all backends.
func (d *DispatchFetcher) TotalBytesFetched() int64 {
	return d.totalBytes.Load()
}

// TotalFetchOps returns aggregate fetch operations across all backends.
func (d *DispatchFetcher) TotalFetchOps() int64 {
	return d.totalOps.Load()
}

// resolve picks the right fetcher for a given ref URI.
func (d *DispatchFetcher) resolve(ref string) (RemoteFetcher, string, error) {
	switch {
	case strings.HasPrefix(ref, "file://"):
		return d.local, "file", nil

	case strings.HasPrefix(ref, "http://"), strings.HasPrefix(ref, "https://"):
		return d.http, "http", nil

	case strings.HasPrefix(ref, "omero://"), strings.HasPrefix(ref, "wado-rs://"):
		// These need URL resolution before HTTP fetch.
		// The caller (BlockManager) should have already resolved via
		// ResolveRemoteURL before storing the remoteRef.
		// If we get here with an unresolved scheme, it's a config error.
		return nil, "", fmt.Errorf("unresolved scheme in ref %q — use ResolveRemoteURL first", ref)

	case strings.HasPrefix(ref, "gs://"):
		gcs, err := d.initGCS()
		if err != nil {
			return nil, "gcs", fmt.Errorf("gcs fetcher init for %q: %w", truncateRef(ref), err)
		}
		return gcs, "gcs", nil

	case strings.HasPrefix(ref, "s3://"):
		s3f, err := d.initS3()
		if err != nil {
			return nil, "s3", fmt.Errorf("s3 fetcher init for %q: %w", truncateRef(ref), err)
		}
		return s3f, "s3", nil

	default:
		// Assume plain HTTP URL (no scheme = relative URL or bare hostname)
		return d.http, "http", nil
	}
}

// truncateRef shortens a ref for logging (avoids dumping full GCS paths).
func truncateRef(ref string) string {
	if len(ref) > 80 {
		return ref[:77] + "..."
	}
	return ref
}

// initGCS lazily creates the GCSFetcher on first gs:// access.
// This avoids requiring GCP credentials when only file:// or http:// are used.
func (d *DispatchFetcher) initGCS() (*GCSFetcher, error) {
	var initErr error
	d.gcsOnce.Do(func() {
		d.gcs = NewGCSFetcher(nil, d.log)
		d.log.Info("GCS fetcher initialized (lazy)")
	})
	if initErr != nil {
		return nil, initErr
	}
	return d.gcs, nil
}

// initS3 lazily creates the S3Fetcher on first s3:// access.
// This avoids requiring AWS credentials when only file:// or gs:// are used.
func (d *DispatchFetcher) initS3() (*S3Fetcher, error) {
	var initErr error
	d.s3Once.Do(func() {
		d.s3 = NewS3Fetcher(nil, d.log)
		d.log.Info("S3 fetcher initialized (lazy)")
	})
	if initErr != nil {
		return nil, initErr
	}
	return d.s3, nil
}
