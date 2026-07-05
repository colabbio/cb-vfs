// Package files implements a MedVFS adapter for static files.
//
// This adapter mounts arbitrary files (CSV, Parquet, JSON, YAML, model
// weights, configs) into the VFS from NFS, GCS, or S3 backends. Unlike
// the Genomics adapter, it applies no domain-specific parsing — files
// are mapped 1:1 with optional glob filtering.
//
// This is the workhorse adapter for ML pipelines where datasets consist
// of labels, annotations, pretrained weights, and configuration files.
package files

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"cloud.google.com/go/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/colabbio/medvfs/internal/adapters"
	"github.com/colabbio/medvfs/internal/config"
	"go.uber.org/zap"
	"google.golang.org/api/iterator"
)

// Adapter serves static files as a FUSE FileProvider.
type Adapter struct {
	cfg config.SourceConfig
	log *zap.Logger
}

// New creates a new static files adapter.
func New(cfg config.SourceConfig, log *zap.Logger) *Adapter {
	return &Adapter{cfg: cfg, log: log}
}

func (a *Adapter) ID() string { return a.cfg.ID }

// Validate checks that the storage backend is accessible.
func (a *Adapter) Validate(ctx context.Context) error {
	f := a.cfg.Files
	if f == nil {
		return fmt.Errorf("files[%s]: files config block is required", a.cfg.ID)
	}
	switch f.Storage {
	case "nfs":
		info, err := os.Stat(f.BasePath)
		if err != nil {
			return fmt.Errorf("files[%s]: base_path %q: %w", a.cfg.ID, f.BasePath, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("files[%s]: base_path %q is not a directory", a.cfg.ID, f.BasePath)
		}
	case "gcs":
		if f.Bucket == "" {
			return fmt.Errorf("files[%s]: bucket is required for gcs storage", a.cfg.ID)
		}
	case "s3":
		if f.Bucket == "" {
			return fmt.Errorf("files[%s]: bucket is required for s3 storage", a.cfg.ID)
		}
	default:
		return fmt.Errorf("files[%s]: unknown storage %q", a.cfg.ID, f.Storage)
	}
	return nil
}

// Fetch is a no-op for the files adapter — all access is via BuildFileTree.
func (a *Adapter) Fetch(_ context.Context, _ string, progress chan<- adapters.Event) error {
	progress <- adapters.Event{SourceID: a.cfg.ID, Status: "done", Message: "files adapter uses FUSE; no fetch required"}
	return nil
}

// ─── FileProvider ────────────────────────────────────────────────────────────

// BuildFileTree implements adapters.FileProvider.
// Scans the configured backend and returns FileMappings for all matching files.
func (a *Adapter) BuildFileTree(ctx context.Context, outDir string, strategy string) ([]adapters.FileMapping, error) {
	f := a.cfg.Files

	var mappings []adapters.FileMapping
	var err error

	switch f.Storage {
	case "nfs":
		mappings, err = a.scanNFS(ctx, f)
	case "gcs":
		mappings, err = a.scanGCS(ctx, f)
	case "s3":
		mappings, err = a.scanS3(ctx, f)
	default:
		return nil, fmt.Errorf("files[%s]: unknown storage %q", a.cfg.ID, f.Storage)
	}
	if err != nil {
		return nil, fmt.Errorf("files[%s]: scan: %w", a.cfg.ID, err)
	}

	// Apply symlink strategy for NFS
	if strategy == "symlink_tree" && f.Storage == "nfs" && outDir != "" {
		for i, m := range mappings {
			symlinkPath := filepath.Join(outDir, m.LogicalPath)
			if mkErr := os.MkdirAll(filepath.Dir(symlinkPath), 0o755); mkErr != nil {
				return nil, mkErr
			}
			srcPath := strings.TrimPrefix(m.RemoteRef, "file://")
			relPath, relErr := filepath.Rel(filepath.Dir(symlinkPath), srcPath)
			if relErr != nil {
				relPath = srcPath
			}
			_ = os.Remove(symlinkPath)
			if lnErr := os.Symlink(relPath, symlinkPath); lnErr != nil {
				a.log.Warn("symlink failed", zap.String("path", symlinkPath), zap.Error(lnErr))
			}
			mappings[i].LocalPath = symlinkPath
		}
	}

	a.log.Info("Static files tree built",
		zap.String("source", a.cfg.ID),
		zap.String("strategy", strategy),
		zap.String("storage", f.Storage),
		zap.Int("files", len(mappings)),
	)
	return mappings, nil
}

// ─── NFS scanner ─────────────────────────────────────────────────────────────

func (a *Adapter) scanNFS(_ context.Context, f *config.FilesConfig) ([]adapters.FileMapping, error) {
	var mappings []adapters.FileMapping

	err := filepath.Walk(f.BasePath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}

		// Apply glob filters
		name := info.Name()
		if !matchInclude(name, f.Patterns) {
			return nil
		}
		if matchExclude(name, f.Exclude) {
			return nil
		}

		// Build relative logical path
		relPath, _ := filepath.Rel(f.BasePath, path)

		mappings = append(mappings, adapters.FileMapping{
			LogicalPath: relPath,
			RemoteRef:   "file://" + path,
			SizeBytes:   info.Size(),
			ContentType: mimeFromExt(filepath.Ext(name)),
		})

		if f.MaxFiles > 0 && len(mappings) >= f.MaxFiles {
			return filepath.SkipAll
		}
		return nil
	})

	return mappings, err
}

// ─── GCS scanner ─────────────────────────────────────────────────────────────

func (a *Adapter) scanGCS(ctx context.Context, f *config.FilesConfig) ([]adapters.FileMapping, error) {
	client, err := storage.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("create GCS client: %w", err)
	}
	defer client.Close()

	bkt := client.Bucket(f.Bucket)
	query := &storage.Query{Prefix: f.Prefix}
	it := bkt.Objects(ctx, query)

	var mappings []adapters.FileMapping
	for {
		attrs, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("GCS list (bucket=%s, prefix=%s): %w", f.Bucket, f.Prefix, err)
		}

		// Skip directory markers
		if strings.HasSuffix(attrs.Name, "/") {
			continue
		}

		name := filepath.Base(attrs.Name)

		// Apply glob filters
		if !matchInclude(name, f.Patterns) {
			continue
		}
		if matchExclude(name, f.Exclude) {
			continue
		}

		// Build logical path relative to prefix
		logicalPath := attrs.Name
		if f.Prefix != "" {
			logicalPath = strings.TrimPrefix(attrs.Name, f.Prefix)
			logicalPath = strings.TrimPrefix(logicalPath, "/")
		}

		mappings = append(mappings, adapters.FileMapping{
			LogicalPath: logicalPath,
			RemoteRef:   fmt.Sprintf("gs://%s/%s", f.Bucket, attrs.Name),
			SizeBytes:   attrs.Size,
			ContentType: mimeFromExt(filepath.Ext(name)),
		})

		if f.MaxFiles > 0 && len(mappings) >= f.MaxFiles {
			break
		}
	}

	return mappings, nil
}

// ─── S3 scanner ──────────────────────────────────────────────────────────────

func (a *Adapter) scanS3(ctx context.Context, f *config.FilesConfig) ([]adapters.FileMapping, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}

	// Support custom endpoint (MinIO, LocalStack)
	var opts []func(*s3.Options)
	if f.Endpoint != "" {
		opts = append(opts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(f.Endpoint)
			o.UsePathStyle = true
		})
	}

	client := s3.NewFromConfig(cfg, opts...)

	var mappings []adapters.FileMapping
	paginator := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{
		Bucket: aws.String(f.Bucket),
		Prefix: aws.String(f.Prefix),
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("S3 list (bucket=%s, prefix=%s): %w", f.Bucket, f.Prefix, err)
		}

		for _, obj := range page.Contents {
			key := aws.ToString(obj.Key)

			// Skip directory markers
			if strings.HasSuffix(key, "/") {
				continue
			}

			name := filepath.Base(key)

			// Apply glob filters
			if !matchInclude(name, f.Patterns) {
				continue
			}
			if matchExclude(name, f.Exclude) {
				continue
			}

			// Build logical path relative to prefix
			logicalPath := key
			if f.Prefix != "" {
				logicalPath = strings.TrimPrefix(key, f.Prefix)
				logicalPath = strings.TrimPrefix(logicalPath, "/")
			}

			mappings = append(mappings, adapters.FileMapping{
				LogicalPath: logicalPath,
				RemoteRef:   fmt.Sprintf("s3://%s/%s", f.Bucket, key),
				SizeBytes:   aws.ToInt64(obj.Size),
				ContentType: mimeFromExt(filepath.Ext(name)),
			})

			if f.MaxFiles > 0 && len(mappings) >= f.MaxFiles {
				return mappings, nil
			}
		}
	}

	return mappings, nil
}

// matchInclude returns true if name matches any of the include patterns.
// Returns true if patterns is empty (include all files).
func matchInclude(name string, patterns []string) bool {
	if len(patterns) == 0 {
		return true // no filter → include everything
	}
	for _, p := range patterns {
		if matched, _ := filepath.Match(p, name); matched {
			return true
		}
	}
	return false
}

// matchExclude returns true if name matches any of the exclude patterns.
// Returns false if patterns is empty (exclude nothing).
func matchExclude(name string, patterns []string) bool {
	if len(patterns) == 0 {
		return false // no filter → exclude nothing
	}
	for _, p := range patterns {
		if matched, _ := filepath.Match(p, name); matched {
			return true
		}
	}
	return false
}

// mimeFromExt maps file extensions to MIME types.
func mimeFromExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".csv":
		return "text/csv"
	case ".tsv":
		return "text/tab-separated-values"
	case ".json":
		return "application/json"
	case ".yaml", ".yml":
		return "application/x-yaml"
	case ".parquet":
		return "application/vnd.apache.parquet"
	case ".npy", ".npz":
		return "application/x-numpy"
	case ".pt", ".pth":
		return "application/x-pytorch"
	case ".h5", ".hdf5":
		return "application/x-hdf5"
	case ".onnx":
		return "application/vnd.onnx"
	case ".pkl", ".pickle":
		return "application/x-pickle"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".tiff", ".tif":
		return "image/tiff"
	case ".svs":
		return "image/x-aperio-svs"
	case ".txt":
		return "text/plain"
	case ".md":
		return "text/markdown"
	default:
		return "application/octet-stream"
	}
}
