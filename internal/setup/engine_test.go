package setup

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/colabbio/medvfs/internal/adapters"
	"github.com/colabbio/medvfs/internal/config"
	"go.uber.org/zap"
)

type stubAdapter struct {
	id          string
	validateErr error
	fetchErr    error
}

func (s *stubAdapter) ID() string { return s.id }
func (s *stubAdapter) Validate(ctx context.Context) error {
	return s.validateErr
}
func (s *stubAdapter) Fetch(ctx context.Context, outDir string, progress chan<- adapters.Event) error {
	progress <- adapters.Event{SourceID: s.id, Status: "start", Message: "starting"}
	if s.fetchErr != nil {
		progress <- adapters.Event{SourceID: s.id, Status: "error", Message: s.fetchErr.Error()}
		return s.fetchErr
	}
	progress <- adapters.Event{SourceID: s.id, Status: "done", Message: "ok", Rows: 3}
	return nil
}

func TestEngineRun_WritesManifest(t *testing.T) {
	outDir := t.TempDir()
	cfg := &config.VFSConfig{
		Version: "1",
		Setup:   config.SetupConfig{Parallelism: 2},
		Sources: []config.SourceConfig{
			{ID: "s1", Type: "files", Materialize: config.MaterializeConfig{Output: "s1_out"}},
			{ID: "s2", Type: "files", Materialize: config.MaterializeConfig{Output: "s2_out"}},
		},
	}

	eng := New(cfg, outDir, zap.NewNop(), func(src config.SourceConfig) (adapters.Adapter, error) {
		return &stubAdapter{id: src.ID}, nil
	})

	results, err := eng.Run(context.Background())
	if err != nil {
		t.Fatalf("Engine.Run failed: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	raw, err := os.ReadFile(filepath.Join(outDir, "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest.json: %v", err)
	}
	var parsed []Result
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("unmarshal manifest.json: %v", err)
	}
	if len(parsed) != 2 {
		t.Errorf("expected 2 manifest entries, got %d", len(parsed))
	}
}

func TestEngineRun_PropagatesValidationError(t *testing.T) {
	cfg := &config.VFSConfig{
		Version: "1",
		Sources: []config.SourceConfig{
			{ID: "bad", Type: "files", Materialize: config.MaterializeConfig{Output: "bad_out"}},
		},
	}

	eng := New(cfg, t.TempDir(), zap.NewNop(), func(src config.SourceConfig) (adapters.Adapter, error) {
		return &stubAdapter{id: src.ID, validateErr: errors.New("unreachable")}, nil
	})

	if _, err := eng.Run(context.Background()); err == nil {
		t.Fatal("expected error when adapter Validate fails, got nil")
	}
}
