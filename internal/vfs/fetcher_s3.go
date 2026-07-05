package vfs

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"go.uber.org/zap"
)

// S3Fetcher implements RemoteFetcher for AWS S3 (and S3-compatible) objects.
// It reads byte ranges using the S3 GetObject API with the Range header,
// enabling the FUSE VFS to lazily stream large files (BAM, VCF, model weights)
// from S3 without pre-downloading.
//
// Supports custom endpoints for S3-compatible stores (MinIO, Ceph, LocalStack).
// Thread-safe for concurrent block reads.
type S3Fetcher struct {
	client     *s3.Client
	log        *zap.Logger
	mu         sync.Mutex
	fetchCount int64
	bytesRead  int64
}

// NewS3Fetcher creates an S3Fetcher. If client is nil, one is created lazily
// using the default AWS credential chain (env vars, ~/.aws, IRSA, etc.).
func NewS3Fetcher(client *s3.Client, log *zap.Logger) *S3Fetcher {
	return &S3Fetcher{
		client: client,
		log:    log,
	}
}

// Init lazily initialises the S3 client on first use.
func (f *S3Fetcher) Init(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.client != nil {
		return nil
	}

	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("s3 fetcher: load AWS config: %w", err)
	}

	f.client = s3.NewFromConfig(cfg)
	f.log.Info("S3 fetcher initialized (lazy)")
	return nil
}

// FetchBlock reads a byte range from an S3 object.
// The remoteRef must be in the form "s3://bucket/key/path".
func (f *S3Fetcher) FetchBlock(ctx context.Context, remoteRef string, offset, length int64) ([]byte, error) {
	if err := f.Init(ctx); err != nil {
		return nil, err
	}

	bucket, key, err := parseS3URI(remoteRef)
	if err != nil {
		return nil, err
	}

	// Build Range header: "bytes=offset-end" (inclusive)
	rangeHeader := fmt.Sprintf("bytes=%d-%d", offset, offset+length-1)

	out, err := f.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Range:  aws.String(rangeHeader),
	})
	if err != nil {
		return nil, fmt.Errorf("s3 fetcher: get s3://%s/%s range=%s: %w",
			bucket, key, rangeHeader, err)
	}
	defer out.Body.Close()

	var buf bytes.Buffer
	buf.Grow(int(length))
	n, err := io.Copy(&buf, out.Body)
	if err != nil {
		return nil, fmt.Errorf("s3 fetcher: read body: %w", err)
	}

	atomic.AddInt64(&f.fetchCount, 1)
	atomic.AddInt64(&f.bytesRead, n)

	return buf.Bytes(), nil
}

// FileSize returns the size of an S3 object via HeadObject.
func (f *S3Fetcher) FileSize(ctx context.Context, remoteRef string) (int64, error) {
	if err := f.Init(ctx); err != nil {
		return 0, err
	}

	bucket, key, err := parseS3URI(remoteRef)
	if err != nil {
		return 0, err
	}

	head, err := f.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return 0, fmt.Errorf("s3 fetcher: head s3://%s/%s: %w", bucket, key, err)
	}

	if head.ContentLength != nil {
		return *head.ContentLength, nil
	}
	return 0, nil
}

// FetchCount returns the total number of fetch operations.
func (f *S3Fetcher) FetchCount() int64 {
	return atomic.LoadInt64(&f.fetchCount)
}

// BytesRead returns the total bytes fetched from S3.
func (f *S3Fetcher) BytesRead() int64 {
	return atomic.LoadInt64(&f.bytesRead)
}

// ─── URI Parsing ─────────────────────────────────────────────────────────────

// parseS3URI extracts bucket and key from an "s3://bucket/key" URI.
func parseS3URI(uri string) (bucket, key string, err error) {
	const prefix = "s3://"
	if len(uri) <= len(prefix) || uri[:len(prefix)] != prefix {
		return "", "", fmt.Errorf("s3 fetcher: invalid URI (not s3://): %q", uri)
	}
	rest := uri[len(prefix):]
	for i := 0; i < len(rest); i++ {
		if rest[i] == '/' {
			bucket = rest[:i]
			key = rest[i+1:]
			if bucket == "" || key == "" {
				return "", "", fmt.Errorf("s3 fetcher: empty bucket or key in URI: %q", uri)
			}
			return bucket, key, nil
		}
	}
	return "", "", fmt.Errorf("s3 fetcher: no key path in URI: %q", uri)
}
