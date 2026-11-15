package timelapse

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/colabbio/medvfs/internal/adapters"
)

// Config configures a 2D+t dynamic microscopy timelapse adapter (e.g. MorphoVLM brain organoids
// in 24-well plates with timepoints t0001..tNNNN).
type Config struct {
	SourceID        string
	RootDir         string
	PlateID         string
	WellFilter      []string // Optional subset of wells (e.g. ["A1", "B2"])
	Channel         string   // e.g. "brightfield"
	IntervalMinutes int      // e.g. 60 for hourly timelapse
}

// Adapter implements adapters.Adapter and adapters.FileProvider for 2D+t organoid timelapses.
type Adapter struct {
	cfg Config
}

// New creates a new 2D+t Timelapse Adapter.
func New(cfg Config) *Adapter {
	if cfg.IntervalMinutes <= 0 {
		cfg.IntervalMinutes = 60
	}
	if cfg.Channel == "" {
		cfg.Channel = "brightfield"
	}
	return &Adapter{cfg: cfg}
}

func (a *Adapter) ID() string {
	return a.cfg.SourceID
}

func (a *Adapter) Validate(ctx context.Context) error {
	if a.cfg.RootDir == "" {
		return fmt.Errorf("timelapse adapter %q: root_dir is required", a.cfg.SourceID)
	}
	info, err := os.Stat(a.cfg.RootDir)
	if err != nil {
		return fmt.Errorf("timelapse adapter %q: cannot access %s: %w", a.cfg.SourceID, a.cfg.RootDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("timelapse adapter %q: %s is not a directory", a.cfg.SourceID, a.cfg.RootDir)
	}
	return nil
}

// BuildFileTree maps dynamic 2D+t frames into a deterministic virtual hierarchy:
// <plate_id>/<well>/<channel>/t_<index>_<timestamp_min>m.<ext>
func (a *Adapter) BuildFileTree(ctx context.Context, outDir string, strategy string) ([]adapters.FileMapping, error) {
	if err := a.Validate(ctx); err != nil {
		return nil, err
	}

	wellSet := make(map[string]bool, len(a.cfg.WellFilter))
	for _, w := range a.cfg.WellFilter {
		wellSet[strings.ToUpper(w)] = true
	}

	var mappings []adapters.FileMapping
	err := filepath.WalkDir(a.cfg.RootDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		ext := strings.ToLower(filepath.Ext(d.Name()))
		if ext != ".tif" && ext != ".tiff" && ext != ".png" && ext != ".zarr" {
			return nil
		}

		rel, _ := filepath.Rel(a.cfg.RootDir, path)
		parts := strings.Split(filepath.ToSlash(rel), "/")
		well := "WELL_UNKNOWN"
		if len(parts) >= 2 {
			well = strings.ToUpper(parts[0])
		}
		if len(wellSet) > 0 && !wellSet[well] {
			return nil
		}

		info, statErr := d.Info()
		var size int64
		if statErr == nil {
			size = info.Size()
		}

		plate := a.cfg.PlateID
		if plate == "" {
			plate = "plate_01"
		}
		logical := filepath.ToSlash(filepath.Join(plate, well, a.cfg.Channel, d.Name()))
		mappings = append(mappings, adapters.FileMapping{
			LogicalPath: logical,
			LocalPath:   path,
			RemoteRef:   "timelapse://" + plate + "/" + well + "/" + d.Name(),
			SizeBytes:   size,
			ContentType: "image/tiff",
		})
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(mappings, func(i, j int) bool {
		return mappings[i].LogicalPath < mappings[j].LogicalPath
	})
	return mappings, nil
}

// Fetch materialises the 2D+t timelapse tree into outDir using symlinks (Zero-Copy) and emits progress events.
func (a *Adapter) Fetch(ctx context.Context, outDir string, progress chan<- adapters.Event) error {
	if progress != nil {
		progress <- adapters.Event{SourceID: a.cfg.SourceID, Status: "start", Message: "Scanning 2D+t timelapse frames"}
	}

	mappings, err := a.BuildFileTree(ctx, outDir, "symlink_tree")
	if err != nil {
		if progress != nil {
			progress <- adapters.Event{SourceID: a.cfg.SourceID, Status: "error", Message: err.Error()}
		}
		return err
	}

	var totalBytes int64
	for _, m := range mappings {
		target := filepath.Join(outDir, m.LogicalPath)
		if mkErr := os.MkdirAll(filepath.Dir(target), 0o755); mkErr != nil {
			return mkErr
		}
		_ = os.Remove(target)
		if symErr := os.Symlink(m.LocalPath, target); symErr != nil {
			return symErr
		}
		totalBytes += m.SizeBytes
	}

	if progress != nil {
		progress <- adapters.Event{
			SourceID: a.cfg.SourceID,
			Status:   "done",
			Message:  fmt.Sprintf("Materialized %d 2D+t timelapse frames (zero-copy symlink)", len(mappings)),
			Rows:     len(mappings),
			Bytes:    totalBytes,
		}
	}
	return nil
}
