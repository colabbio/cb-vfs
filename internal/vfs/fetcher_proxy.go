package vfs

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ProxyFetcher implements RemoteFetcher for the hierarchical `cb-proxy://` scheme.
//
// URI format:
//   cb-proxy://<host[:port]>/<grantId>/<logicalPath>
//   (or cb-proxy+https://<host>/<grantId>/<logicalPath> for TLS)
//
// It streams byte-range blocks on demand from a child Hospital MedVFS / Edge Proxy
// node into a parent Regional Aggregator (Level 2) while propagating X-ColabBio-Grant-Id.
type ProxyFetcher struct {
	client *http.Client
}

// NewProxyFetcher creates a ProxyFetcher with sensible streaming timeouts.
func NewProxyFetcher(client *http.Client) *ProxyFetcher {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &ProxyFetcher{client: client}
}

func parseProxyRef(ref string) (targetURL string, grantID string, err error) {
	httpScheme := "http"
	trimmed := ref
	if strings.HasPrefix(ref, "cb-proxy+https://") {
		httpScheme = "https"
		trimmed = "cb-proxy://" + strings.TrimPrefix(ref, "cb-proxy+https://")
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return "", "", fmt.Errorf("invalid cb-proxy URI %q: %w", ref, err)
	}
	if u.Host == "" {
		return "", "", fmt.Errorf("cb-proxy URI %q missing host", ref)
	}
	parts := strings.SplitN(strings.TrimPrefix(u.Path, "/"), "/", 2)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("cb-proxy URI %q must be cb-proxy://<host>/<grantId>/<logicalPath>", ref)
	}
	grantID = parts[0]
	logicalPath := parts[1]
	targetURL = fmt.Sprintf("%s://%s/files/%s", httpScheme, u.Host, logicalPath)
	return targetURL, grantID, nil
}

// FetchBlock performs an HTTP Range GET against the child MedVFS node with X-ColabBio-Grant-Id.
func (p *ProxyFetcher) FetchBlock(ctx context.Context, ref string, offset int64, length int64) ([]byte, error) {
	targetURL, grantID, err := parseProxyRef(ref)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-ColabBio-Grant-Id", grantID)
	if length > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, offset+length-1))
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cb-proxy fetch %s: %w", targetURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("cb-proxy ACL denied (403) for grant=%s url=%s", grantID, targetURL)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return nil, fmt.Errorf("cb-proxy unexpected status %d for %s", resp.StatusCode, targetURL)
	}

	return io.ReadAll(resp.Body)
}

// FileSize queries the remote file size via HEAD with X-ColabBio-Grant-Id.
func (p *ProxyFetcher) FileSize(ctx context.Context, ref string) (int64, error) {
	targetURL, grantID, err := parseProxyRef(ref)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, targetURL, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("X-ColabBio-Grant-Id", grantID)

	resp, err := p.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("cb-proxy HEAD status %d for %s", resp.StatusCode, targetURL)
	}
	cl := resp.Header.Get("Content-Length")
	if cl == "" {
		return 0, nil
	}
	return strconv.ParseInt(cl, 10, 64)
}
