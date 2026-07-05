package vfs

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
)

// LocalFetcher implements RemoteFetcher for file:// URIs.
// It reads blocks directly from the local filesystem using ReadAt,
// enabling the FUSE VFS to serve local files (e.g., DICOM images
// received via the local adapter) through the same block-based
// caching pipeline used for remote files.
type LocalFetcher struct {
	bytesRead  atomic.Int64
	fetchCount atomic.Int64
}

// NewLocalFetcher creates a new local filesystem fetcher.
func NewLocalFetcher() *LocalFetcher {
	return &LocalFetcher{}
}

// FetchBlock reads bytes [offset, offset+length) from a local file.
// The ref must be a file:// URI (e.g., "file:///data/dicom/scan.dcm").
func (f *LocalFetcher) FetchBlock(ctx context.Context, ref string, offset int64, length int64) ([]byte, error) {
	path, err := parseFileURI(ref)
	if err != nil {
		return nil, err
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("local fetcher: open %q: %w", path, err)
	}
	defer file.Close()

	buf := make([]byte, length)
	n, err := file.ReadAt(buf, offset)
	if err != nil && n == 0 {
		return nil, fmt.Errorf("local fetcher: read at offset %d: %w", offset, err)
	}

	f.bytesRead.Add(int64(n))
	f.fetchCount.Add(1)

	return buf[:n], nil
}

// FileSize returns the size of a local file.
func (f *LocalFetcher) FileSize(ctx context.Context, ref string) (int64, error) {
	path, err := parseFileURI(ref)
	if err != nil {
		return 0, err
	}

	info, err := os.Stat(path)
	if err != nil {
		return 0, fmt.Errorf("local fetcher: stat %q: %w", path, err)
	}
	return info.Size(), nil
}

// BytesRead returns the cumulative bytes read from local files.
func (f *LocalFetcher) BytesRead() int64 {
	return f.bytesRead.Load()
}

// FetchCount returns the total number of local fetch operations.
func (f *LocalFetcher) FetchCount() int64 {
	return f.fetchCount.Load()
}

// parseFileURI strips the file:// prefix from a URI and returns the local path.
// Accepts both "file:///absolute/path" and "file://relative/path".
func parseFileURI(ref string) (string, error) {
	if !strings.HasPrefix(ref, "file://") {
		return "", fmt.Errorf("not a file:// URI: %q", ref)
	}
	path := strings.TrimPrefix(ref, "file://")
	if path == "" {
		return "", fmt.Errorf("empty path in file:// URI: %q", ref)
	}
	return path, nil
}
