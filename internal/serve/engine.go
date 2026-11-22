// Package serve implements the persistent daemon mode for MedVFS.
// The serve engine orchestrates adapters, VFS mounts, streaming, and
// an HTTP API for manifest queries, SSE events, and file proxying.
package serve

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/colabbio/medvfs/internal/adapters"
	"github.com/colabbio/medvfs/internal/config"
	"github.com/colabbio/medvfs/internal/dataset"
	"github.com/colabbio/medvfs/internal/output"
	"github.com/colabbio/medvfs/internal/setup"
	"github.com/colabbio/medvfs/internal/vfs"
	"go.uber.org/zap"
)


// Engine orchestrates the serve lifecycle:
//  1. Build adapters → collect FileMappings
//  2. Run non-FileProvider adapters (FHIR/OMOP produce CSVs)
//  3. Bridge tabular outputs into VFS (G6)
//  4. Apply patient linking for unified view (G4)
//  5. Apply dataset splits (G2)
//  6. Generate dataset manifest (G5)
//  7. Mount VFS (FUSE or download)
//  8. Start streaming adapters (if any)
//  9. Start output syncer (G3)
//  10. Launch HTTP server
//  11. Wait for shutdown signal
type Engine struct {
	cfg     *config.VFSConfig
	factory setup.AdapterFactory
	log     *zap.Logger
	vfsImpl vfs.VFS
	server  *Server

	mu         sync.RWMutex
	mappings   map[string][]adapters.FileMapping
	manifest   *dataset.DatasetManifest
	liveEvents chan adapters.LiveEvent
}

// NewEngine creates a serve engine from the config and adapter factory.
func NewEngine(cfg *config.VFSConfig, factory setup.AdapterFactory, log *zap.Logger) *Engine {
	return &Engine{
		cfg:        cfg,
		factory:    factory,
		log:        log,
		mappings:   make(map[string][]adapters.FileMapping),
		liveEvents: make(chan adapters.LiveEvent, 100),
	}
}

// Run executes the serve lifecycle. It blocks until ctx is cancelled.
func (e *Engine) Run(ctx context.Context) error {
	// 1. Determine VFS strategy
	strategy := vfs.SelectStrategy(e.cfg)
	e.log.Info("VFS strategy selected", zap.String("strategy", strategy))

	// 2. Create VFS
	v, err := vfs.New(e.cfg, strategy)
	if err != nil {
		return fmt.Errorf("create VFS: %w", err)
	}
	e.vfsImpl = v

	// 3. Build adapters and collect file mappings
	outDir := e.cfg.Storage.OutputDir
	fuseVFS, isFUSE := e.vfsImpl.(*vfs.FUSEVFS)
	for _, src := range e.cfg.Sources {
		if isFUSE {
			if authErr := fuseVFS.RegisterSourceAuth(src); authErr != nil {
				return fmt.Errorf("register source auth for %q: %w", src.ID, authErr)
			}
		}
		adapter, err := e.factory(src)
		if err != nil {
			return fmt.Errorf("create adapter %q: %w", src.ID, err)
		}

		// Check if adapter supports FileProvider
		fp, isFileProvider := adapter.(adapters.FileProvider)
		if !isFileProvider {
			// Non-FileProvider adapters (FHIR, OMOP): run Fetch to produce CSV outputs
			e.log.Info("running non-FileProvider adapter (tabular)",
				zap.String("source", src.ID),
				zap.String("type", src.Type),
			)
			progress := make(chan adapters.Event, 16)
			go func() { for range progress {} }() // drain
			if err := adapter.Fetch(ctx, outDir, progress); err != nil {
				e.log.Error("tabular adapter fetch failed",
					zap.String("source", src.ID),
					zap.Error(err),
				)
			}
			close(progress)
			continue
		}

		// Build file tree
		mappings, err := fp.BuildFileTree(ctx, outDir, strategy)
		if err != nil {
			return fmt.Errorf("build file tree for %q: %w", src.ID, err)
		}

		e.mu.Lock()
		e.mappings[src.ID] = mappings
		e.mu.Unlock()

		e.log.Info("source scanned",
			zap.String("source", src.ID),
			zap.Int("files", len(mappings)),
		)

		// Start streaming if supported
		sa, isStreaming := adapter.(adapters.StreamingAdapter)
		if isStreaming {
			go func(id string) {
				err := sa.StartListening(ctx, e.liveEvents)
				if err != nil {
					e.log.Error("streaming adapter stopped",
						zap.String("source", id),
						zap.Error(err),
					)
				}
			}(src.ID)
		}
	}

	// 4. Bridge tabular outputs (G6: FHIR/OMOP CSVs → VFS)
	if outDir != "" {
		bridged := dataset.BridgeTabularOutputs(outDir, e.cfg.Sources, e.log)
		if len(bridged) > 0 {
			e.mu.Lock()
			e.mappings["_tabular_bridge"] = bridged
			e.mu.Unlock()
			e.log.Info("tabular outputs bridged to VFS", zap.Int("files", len(bridged)))
		}
	}

	// 4b. Execute derived views (cross-source CSV joins)
	if e.cfg.Dataset != nil && len(e.cfg.Dataset.DerivedViews) > 0 && outDir != "" {
		e.log.Info("executing derived views", zap.Int("count", len(e.cfg.Dataset.DerivedViews)))
		if err := dataset.ExecuteDerivedViews(e.cfg.Dataset.DerivedViews, e.cfg.Sources, outDir, e.log); err != nil {
			e.log.Error("derived views failed, continuing without joins", zap.Error(err))
		} else {
			// Bridge derived view outputs into VFS
			for _, dv := range e.cfg.Dataset.DerivedViews {
				dvPath := filepath.Join(outDir, dv.Output)
				info, err := os.Stat(dvPath)
				if err != nil {
					continue
				}
				e.mu.Lock()
				e.mappings["_derived_"+dv.ID] = []adapters.FileMapping{{
					LogicalPath: dv.Output,
					RemoteRef:   "file://" + dvPath,
					SizeBytes:   info.Size(),
					ContentType: "text/csv",
				}}
				e.mu.Unlock()
				e.log.Info("derived view bridged to VFS",
					zap.String("view", dv.ID),
					zap.String("path", dv.Output),
				)
			}
		}
	}

	// 5. Apply patient linking (G4)
	if e.cfg.Dataset != nil && e.cfg.Dataset.PatientLinker != nil {
		pl := e.cfg.Dataset.PatientLinker
		e.log.Info("applying patient linking", zap.String("source", pl.Source))

		links, err := dataset.LoadPatientLinks(ctx, pl.Source)
		if err != nil {
			e.log.Error("failed to load patient links, continuing without linking", zap.Error(err))
		} else if pl.Layout == "unified" {
			e.mu.Lock()
			unified := dataset.ApplyUnifiedLayout(e.mappings, links)
			// Replace all per-source mappings with unified view
			e.mappings = map[string][]adapters.FileMapping{"_unified": unified}
			e.mu.Unlock()
			e.log.Info("unified patient layout applied",
				zap.Int("total_files", len(unified)),
				zap.Int("patients", len(links.Links)),
			)
		}
	}

	// 6. Apply dataset splits (G2)
	if e.cfg.Dataset != nil && e.cfg.Dataset.Splits != nil {
		sc := e.cfg.Dataset.Splits
		e.log.Info("applying dataset splits", zap.String("source", sc.Source))

		splits, err := dataset.LoadSplits(ctx, sc.Source, sc.PatientIDColumn, sc.SplitColumn)
		if err != nil {
			e.log.Error("failed to load splits, continuing without splits", zap.Error(err))
		} else {
			e.mu.Lock()
			for srcID, mappings := range e.mappings {
				e.mappings[srcID] = dataset.ApplySplits(mappings, splits)
			}
			e.mu.Unlock()

			// Log split distribution
			splitCounts := make(map[string]int)
			for _, ms := range e.mappings {
				for _, m := range ms {
					parts := splitPath(m.LogicalPath)
					if len(parts) > 0 {
						splitCounts[parts[0]]++
					}
				}
			}
			e.log.Info("dataset splits applied", zap.Any("distribution", splitCounts))
		}
	}

	// 7. Register all mappings with VFS
	for srcID, mappings := range e.mappings {
		if err := v.AddSource(srcID, mappings); err != nil {
			return fmt.Errorf("add source %q to VFS: %w", srcID, err)
		}
		e.log.Info("source registered in VFS",
			zap.String("source", srcID),
			zap.Int("files", len(mappings)),
		)
	}

	// 8. Generate dataset manifest (G5)
	if e.cfg.Dataset != nil && e.cfg.Dataset.Versioning != nil && e.cfg.Dataset.Versioning.Enabled {
		vc := e.cfg.Dataset.Versioning
		runID := fmt.Sprintf("run-%d", ctx.Value(contextKeyRunID{}))
		if runID == "run-<nil>" {
			runID = "run-default"
		}
		manifest := dataset.GenerateManifest(vc.DatasetID, runID, e.mappings)
		e.mu.Lock()
		e.manifest = manifest
		e.mu.Unlock()

		data, _ := dataset.ManifestToJSON(manifest)
		e.log.Info("dataset manifest generated",
			zap.String("dataset_id", manifest.DatasetID),
			zap.String("sha256", manifest.ManifestSHA256[:12]),
			zap.Int("file_count", manifest.FileCount),
			zap.Int64("total_bytes", manifest.TotalBytes),
		)

		// Register manifest as virtual file in VFS
		manifestMapping := []adapters.FileMapping{{
			LogicalPath: ".dataset.json",
			RemoteRef:   "inline://" + string(data),
			SizeBytes:   int64(len(data)),
			ContentType: "application/json",
		}}
		if err := v.AddSource("_manifest", manifestMapping); err != nil {
			e.log.Warn("could not register .dataset.json in VFS", zap.Error(err))
		}
	}

	// 9. Start output syncer (G3)
	if e.cfg.Output != nil && e.cfg.Output.Sync != nil && e.cfg.Output.Sync.Enabled {
		syncer := output.NewSyncer(e.cfg.Output, "serve", e.log.Named("syncer"))
		go func() {
			if err := syncer.Start(ctx); err != nil {
				e.log.Error("output syncer error", zap.Error(err))
			}
		}()
		e.log.Info("output syncer started",
			zap.String("local_dir", e.cfg.Output.LocalDir),
			zap.String("target", e.cfg.Output.Sync.Target),
		)
	}

	// 10. Start HTTP server
	port := 8090
	if e.cfg.Serve != nil && e.cfg.Serve.Port > 0 {
		port = e.cfg.Serve.Port
	}
	e.server = NewServer(port, e, e.log)
	go e.server.Start(ctx)

	// 11. Mount VFS (blocks until unmount or ctx cancel)
	e.log.Info("starting VFS mount")
	return v.Mount(ctx)
}

// GetMappings returns all file mappings, optionally filtered by source.
func (e *Engine) GetMappings(sourceID string) []adapters.FileMapping {
	e.mu.RLock()
	defer e.mu.RUnlock()

	if sourceID != "" {
		return e.mappings[sourceID]
	}

	var all []adapters.FileMapping
	for _, m := range e.mappings {
		all = append(all, m...)
	}
	return all
}

// GetManifest returns the dataset manifest (nil if versioning is disabled).
func (e *Engine) GetManifest() *dataset.DatasetManifest {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.manifest
}

// GetStats returns VFS cache statistics.
func (e *Engine) GetStats() vfs.CacheStats {
	if e.vfsImpl != nil {
		return e.vfsImpl.Stats()
	}
	return vfs.CacheStats{}
}

// contextKeyRunID is a context key for the run ID.
type contextKeyRunID struct{}

// splitPath splits a path into its components.
func splitPath(p string) []string {
	var parts []string
	for {
		dir, file := split(p)
		if file != "" {
			parts = append([]string{file}, parts...)
		}
		if dir == "" || dir == "/" || dir == "." {
			break
		}
		p = dir
	}
	return parts
}

// split is a simple path split that doesn't import path/filepath.
func split(path string) (dir, file string) {
	i := len(path) - 1
	for i >= 0 && path[i] != '/' {
		i--
	}
	if i < 0 {
		return "", path
	}
	return path[:i], path[i+1:]
}
