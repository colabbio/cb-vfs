// gcs.go — GCS backend helpers for the genomics adapter.
// Requires: cloud.google.com/go/storage (go get cloud.google.com/go/storage)
// Credentials: GOOGLE_APPLICATION_CREDENTIALS or Workload Identity
package genomics

import (
	"context"
	"fmt"
	"strings"

	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
)

// gcsObject is a lightweight representation of a GCS object.
type gcsObject struct {
	Name string
	Size int64
}

func newGCSClient(ctx context.Context) (*storage.Client, error) {
	return storage.NewClient(ctx)
}

func listGCSObjects(ctx context.Context, client *storage.Client, bucket, prefix string) ([]gcsObject, error) {
	bkt := client.Bucket(bucket)
	query := &storage.Query{Prefix: prefix}
	it := bkt.Objects(ctx, query)

	var objs []gcsObject
	for {
		attrs, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("GCS list objects (bucket=%s, prefix=%s): %w", bucket, prefix, err)
		}
		objs = append(objs, gcsObject{Name: attrs.Name, Size: attrs.Size})
	}
	return objs, nil
}

// findGCSIndex checks whether the index object exists in GCS and returns its URI.
func findGCSIndex(ctx context.Context, client *storage.Client, bucket, objName, format string) (string, error) {
	candidates, ok := indexExts[format]
	if !ok {
		return "", nil // no index required for this format
	}

	// Strip format extension from object name to build index candidate paths
	base := objName
	for _, ext := range extMap[format] {
		if strings.HasSuffix(strings.ToLower(base), ext) {
			base = base[:len(base)-len(ext)]
			break
		}
	}

	bkt := client.Bucket(bucket)
	for _, idxExt := range candidates {
		candidate := base + idxExt
		_, err := bkt.Object(candidate).Attrs(ctx)
		if err == nil {
			return fmt.Sprintf("gs://%s/%s", bucket, candidate), nil
		}
		// Also try objName + first candidate ext (e.g. file.bam.bai)
		alt := objName + candidates[0]
		if _, err2 := bkt.Object(alt).Attrs(ctx); err2 == nil {
			return fmt.Sprintf("gs://%s/%s", bucket, alt), nil
		}
	}
	return "", fmt.Errorf("index not found in GCS for gs://%s/%s (tried %v)", bucket, objName, candidates)
}
