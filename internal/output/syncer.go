// Package output implements the writable output layer for ML pipeline results.
// It provides periodic sync of local output directories to cloud storage (GCS/S3).
package output

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"github.com/colabbio/medvfs/internal/config"
	"go.uber.org/zap"
)

// Syncer watches a local output directory and periodically syncs new/modified
// files to a cloud storage target.
type Syncer struct {
	cfg    *config.OutputConfig
	runID  string
	log    *zap.Logger
	synced map[string]time.Time // path → last sync time
}

// NewSyncer creates a new output syncer.
func NewSyncer(cfg *config.OutputConfig, runID string, log *zap.Logger) *Syncer {
	return &Syncer{
		cfg:    cfg,
		runID:  runID,
		log:    log,
		synced: make(map[string]time.Time),
	}
}

// Start begins periodic sync. It blocks until ctx is cancelled.
// If cfg.Sync.OnExit is true, a final sync is performed on cancellation.
func (s *Syncer) Start(ctx context.Context) error {
	if s.cfg == nil || s.cfg.Sync == nil || !s.cfg.Sync.Enabled {
		return nil
	}

	// Ensure output directory exists
	if err := os.MkdirAll(s.cfg.LocalDir, 0o755); err != nil {
		return fmt.Errorf("create output dir %q: %w", s.cfg.LocalDir, err)
	}

	interval := time.Duration(s.cfg.Sync.IntervalSeconds) * time.Second
	if interval <= 0 {
		interval = 60 * time.Second
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	s.log.Info("output syncer started",
		zap.String("local_dir", s.cfg.LocalDir),
		zap.String("target", s.resolveTarget()),
		zap.Duration("interval", interval),
	)

	for {
		select {
		case <-ctx.Done():
			if s.cfg.Sync.OnExit {
				s.log.Info("performing final sync on exit")
				if err := s.syncOnce(context.Background()); err != nil {
					s.log.Error("final sync failed", zap.Error(err))
				}
			}
			return nil
		case <-ticker.C:
			if err := s.syncOnce(ctx); err != nil {
				s.log.Error("sync iteration failed", zap.Error(err))
			}
		}
	}
}

// SyncOnce performs a single sync pass, uploading new/modified files.
func (s *Syncer) syncOnce(ctx context.Context) error {
	target := s.resolveTarget()

	var uploaded int
	err := filepath.Walk(s.cfg.LocalDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}

		// Check if file was modified since last sync
		if lastSync, ok := s.synced[path]; ok {
			if !info.ModTime().After(lastSync) {
				return nil // already synced, not modified
			}
		}

		relPath, _ := filepath.Rel(s.cfg.LocalDir, path)
		remotePath := target + "/" + relPath

		if err := s.uploadFile(ctx, path, remotePath); err != nil {
			s.log.Warn("upload failed", zap.String("file", relPath), zap.Error(err))
			return nil // don't abort the walk
		}

		s.synced[path] = time.Now()
		uploaded++
		return nil
	})

	if uploaded > 0 {
		s.log.Info("sync completed", zap.Int("files_uploaded", uploaded))
	}
	return err
}

// uploadFile uploads a local file to a GCS URI.
func (s *Syncer) uploadFile(ctx context.Context, localPath, remotePath string) error {
	if strings.HasPrefix(remotePath, "gs://") {
		return s.uploadGCS(ctx, localPath, remotePath)
	}
	// S3 and other schemes can be added here
	return fmt.Errorf("unsupported sync target scheme: %s", remotePath)
}

// uploadGCS uploads a file to Google Cloud Storage.
func (s *Syncer) uploadGCS(ctx context.Context, localPath, gcsURI string) error {
	// Parse gs://bucket/path
	trimmed := strings.TrimPrefix(gcsURI, "gs://")
	idx := strings.Index(trimmed, "/")
	if idx < 0 {
		return fmt.Errorf("invalid GCS URI: %s", gcsURI)
	}
	bucket := trimmed[:idx]
	objPath := trimmed[idx+1:]

	client, err := storage.NewClient(ctx)
	if err != nil {
		return fmt.Errorf("create GCS client: %w", err)
	}
	defer client.Close()

	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()

	w := client.Bucket(bucket).Object(objPath).NewWriter(ctx)
	if _, err := io.Copy(w, f); err != nil {
		w.Close()
		return fmt.Errorf("upload %s: %w", objPath, err)
	}
	return w.Close()
}

// resolveTarget applies template variables to the sync target URI.
func (s *Syncer) resolveTarget() string {
	target := s.cfg.Sync.Target
	target = strings.ReplaceAll(target, "{{ .RunID }}", s.runID)
	target = strings.ReplaceAll(target, "{{.RunID}}", s.runID)
	target = strings.ReplaceAll(target, "{{ .Timestamp }}", time.Now().UTC().Format("20060102T150405"))
	target = strings.ReplaceAll(target, "{{.Timestamp}}", time.Now().UTC().Format("20060102T150405"))
	return strings.TrimRight(target, "/")
}
