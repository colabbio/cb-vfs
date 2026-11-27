package output

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colabbio/medvfs/internal/config"
	"go.uber.org/zap"
)

func TestSyncer_DisabledAndResolveTarget(t *testing.T) {
	// Disabled syncer returns nil immediately
	sDisabled := NewSyncer(&config.OutputConfig{Sync: &config.SyncConfig{Enabled: false}}, "run-1", zap.NewNop())
	if err := sDisabled.Start(context.Background()); err != nil {
		t.Fatalf("disabled Start returned error: %v", err)
	}

	// resolveTarget expands RunID and trims trailing slash
	s := NewSyncer(&config.OutputConfig{
		LocalDir: t.TempDir(),
		Sync: &config.SyncConfig{
			Enabled: true,
			Target:  "gs://bucket/runs/{{ .RunID }}/{{.RunID}}/",
		},
	}, "run-42", zap.NewNop())

	got := s.resolveTarget()
	if got != "gs://bucket/runs/run-42/run-42" {
		t.Errorf("resolveTarget() = %q", got)
	}

	// uploadFile rejects unsupported schemes
	if err := s.uploadFile(context.Background(), "a", "ftp://bucket/a"); err == nil || !strings.Contains(err.Error(), "unsupported sync target scheme") {
		t.Errorf("expected unsupported scheme error, got %v", err)
	}

	// syncOnce walks local files without aborting on upload errors
	dir := s.cfg.LocalDir
	if err := os.WriteFile(filepath.Join(dir, "metrics.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	s.cfg.Sync.Target = "unsupported://bucket/out"
	if err := s.syncOnce(context.Background()); err != nil {
		t.Fatalf("syncOnce returned unexpected error: %v", err)
	}
}
