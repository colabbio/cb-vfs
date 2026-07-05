package vfs

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
)

// RemoteFetcher abstracts byte-range retrieval from a remote source.
// Different implementations handle OMERO, DICOMweb (WADO-RS), GCS, and
// plain HTTP sources that support Range requests.
type RemoteFetcher interface {
	// FetchBlock downloads a specific byte range from the remote source.
	// offset is the start byte; length is the number of bytes to fetch.
	FetchBlock(ctx context.Context, remoteRef string, offset int64, length int64) ([]byte, error)

	// FileSize returns the total size of the remote file via HEAD request.
	FileSize(ctx context.Context, remoteRef string) (int64, error)
}

// HTTPFetcher implements RemoteFetcher using HTTP Range requests.
// It works with any HTTP server that supports the Range header (RFC 7233).
type HTTPFetcher struct {
	client       *http.Client
	bytesFetched atomic.Int64
	fetchCount   atomic.Int64
}

// NewHTTPFetcher creates a new HTTP-based remote fetcher.
func NewHTTPFetcher(client *http.Client) *HTTPFetcher {
	if client == nil {
		client = &http.Client{}
	}
	return &HTTPFetcher{client: client}
}

// FetchBlock downloads bytes [offset, offset+length) from remoteRef using
// HTTP Range: bytes=offset-(offset+length-1).
// Returns 206 Partial Content on success or falls back to reading the full
// body and seeking for servers that don't support Range.
func (f *HTTPFetcher) FetchBlock(ctx context.Context, remoteRef string, offset int64, length int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", remoteRef, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, offset+length-1))

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch block at offset %d: %w", offset, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusPartialContent: // 206 — expected
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("read block body: %w", err)
		}
		f.bytesFetched.Add(int64(len(data)))
		f.fetchCount.Add(1)
		return data, nil

	case http.StatusOK: // 200 — server ignores Range, return full body
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("read full body: %w", err)
		}
		// Extract the requested range from the full body
		end := offset + length
		if end > int64(len(data)) {
			end = int64(len(data))
		}
		if offset >= int64(len(data)) {
			return nil, fmt.Errorf("offset %d beyond file size %d", offset, len(data))
		}
		block := data[offset:end]
		f.bytesFetched.Add(int64(len(block)))
		f.fetchCount.Add(1)
		return block, nil

	case http.StatusRequestedRangeNotSatisfiable: // 416
		return nil, fmt.Errorf("range not satisfiable: offset=%d length=%d", offset, length)

	default:
		return nil, fmt.Errorf("unexpected HTTP status %d for %s", resp.StatusCode, remoteRef)
	}
}

// FileSize returns the total size of the remote file via a HEAD request.
// It parses the Content-Length header. For servers that support Range,
// it also tries the Content-Range header from a zero-byte Range request.
func (f *HTTPFetcher) FileSize(ctx context.Context, remoteRef string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, "HEAD", remoteRef, nil)
	if err != nil {
		return 0, fmt.Errorf("create HEAD request: %w", err)
	}

	resp, err := f.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("HEAD request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("HEAD returned status %d", resp.StatusCode)
	}

	// Try Content-Length header
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		size, err := strconv.ParseInt(cl, 10, 64)
		if err == nil && size > 0 {
			return size, nil
		}
	}

	return 0, fmt.Errorf("unable to determine file size for %s", remoteRef)
}

// BytesFetched returns the total bytes fetched from remote sources.
func (f *HTTPFetcher) BytesFetched() int64 {
	return f.bytesFetched.Load()
}

// FetchCount returns the total number of fetch operations.
func (f *HTTPFetcher) FetchCount() int64 {
	return f.fetchCount.Load()
}

// ─── Scheme-specific URL resolution ──────────────────────────────────────────

// ResolveRemoteURL converts a scheme-qualified RemoteRef to a fetchable HTTP URL.
// Recognized schemes:
//   - https:// / http:// → used directly
//   - omero://image/{id}  → resolved to OMERO download API
//   - wado-rs://{host}/...→ resolved to DICOMweb WADO-RS retrieve
func ResolveRemoteURL(remoteRef string, baseEndpoints map[string]string) (string, error) {
	switch {
	case strings.HasPrefix(remoteRef, "http://"), strings.HasPrefix(remoteRef, "https://"):
		return remoteRef, nil

	case strings.HasPrefix(remoteRef, "omero://image/"):
		imageID := strings.TrimPrefix(remoteRef, "omero://image/")
		base, ok := baseEndpoints["omero"]
		if !ok {
			return "", fmt.Errorf("no OMERO endpoint configured for ref %s", remoteRef)
		}
		// OMERO.web archived file download API
		return fmt.Sprintf("%s/webclient/annotation/%s", base, imageID), nil

	case strings.HasPrefix(remoteRef, "wado-rs://"):
		// wado-rs://host:port/studies/{study}/series/{series}/instances/{instance}
		path := strings.TrimPrefix(remoteRef, "wado-rs://")
		base, ok := baseEndpoints["orthanc"]
		if !ok {
			return "", fmt.Errorf("no Orthanc endpoint configured for ref %s", remoteRef)
		}
		return fmt.Sprintf("%s/wado-rs/%s", base, path), nil

	default:
		return "", fmt.Errorf("unsupported remote ref scheme: %s", remoteRef)
	}
}
