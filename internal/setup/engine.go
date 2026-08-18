package setup

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/colabbio/medvfs/internal/adapters"
	"github.com/colabbio/medvfs/internal/config"
	"github.com/colabbio/medvfs/internal/dataset"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)


// Result holds metadata about a successfully materialised source.
type Result struct {
	SourceID  string    `json:"source_id"`
	Output    string    `json:"output"`
	Rows      int       `json:"rows,omitempty"`
	StartedAt time.Time `json:"started_at"`
	DoneAt    time.Time `json:"done_at"`
}

// Engine orchestrates the parallel execution of all adapters.
type Engine struct {
	cfg     *config.VFSConfig
	outDir  string
	log     *zap.Logger
	factory AdapterFactory
}

// AdapterFactory creates an Adapter for a given SourceConfig (allows test injection).
type AdapterFactory func(src config.SourceConfig) (adapters.Adapter, error)

// New creates a setup Engine.
func New(cfg *config.VFSConfig, outDir string, log *zap.Logger, factory AdapterFactory) *Engine {
	return &Engine{cfg: cfg, outDir: outDir, log: log, factory: factory}
}

// Run executes all configured sources in parallel and writes a manifest.
func (e *Engine) Run(ctx context.Context) ([]Result, error) {
	parallelism := e.cfg.Setup.Parallelism
	if parallelism <= 0 {
		parallelism = 4
	}

	sem := make(chan struct{}, parallelism)
	progress := make(chan adapters.Event, 128)
	var mu sync.Mutex
	results := make([]Result, 0, len(e.cfg.Sources))

	// Progress logger goroutine
	var wgLog sync.WaitGroup
	wgLog.Add(1)
	go func() {
		defer wgLog.Done()
		for ev := range progress {
			switch ev.Status {
			case "start":
				e.log.Info("source started", zap.String("source", ev.SourceID), zap.String("msg", ev.Message))
			case "progress":
				e.log.Info("source progress", zap.String("source", ev.SourceID), zap.Int("rows", ev.Rows))
			case "done":
				e.log.Info("source done", zap.String("source", ev.SourceID), zap.String("msg", ev.Message))
			case "error":
				e.log.Error("source error", zap.String("source", ev.SourceID), zap.String("msg", ev.Message))
			}
		}
	}()

	g, gctx := errgroup.WithContext(ctx)

	for _, src := range e.cfg.Sources {
		src := src // capture loop variable
		g.Go(func() error {
			sem <- struct{}{}
			defer func() { <-sem }()

			adapter, err := e.factory(src)
			if err != nil {
				return fmt.Errorf("source %q: create adapter: %w", src.ID, err)
			}

			if err := adapter.Validate(gctx); err != nil {
				return fmt.Errorf("source %q: validate: %w", src.ID, err)
			}

			start := time.Now()
			if err := adapter.Fetch(gctx, e.outDir, progress); err != nil {
				return fmt.Errorf("source %q: fetch: %w", src.ID, err)
			}

			mu.Lock()
			results = append(results, Result{
				SourceID:  src.ID,
				Output:    src.Materialize.Output,
				StartedAt: start,
				DoneAt:    time.Now(),
			})
			mu.Unlock()
			return nil
		})
	}

	err := g.Wait()
	close(progress)
	wgLog.Wait()

	if err != nil {
		return results, err
	}

	// Execute derived views (cross-source joins) if configured
	if e.cfg.Dataset != nil && len(e.cfg.Dataset.DerivedViews) > 0 {
		e.log.Info("executing derived views", zap.Int("count", len(e.cfg.Dataset.DerivedViews)))
		if err := dataset.ExecuteDerivedViews(e.cfg.Dataset.DerivedViews, e.cfg.Sources, e.outDir, e.log); err != nil {
			return results, fmt.Errorf("execute derived views: %w", err)
		}
	}

	// Write manifest.json
	if merr := writeManifest(e.outDir, results); merr != nil {
		e.log.Warn("could not write manifest", zap.Error(merr))
	}

	return results, nil
}


func writeManifest(outDir string, results []Result) error {
	data, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(outDir, "manifest.json"), data, 0o644)
}
