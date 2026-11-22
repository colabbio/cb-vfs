// Package vfs implements the virtual file system layer for MedVFS.
//
// The VFS presents remote medical data (DICOM, WSI, Genomics) as local files
// using three strategies:
//   - download:      pre-download full files to local disk (batch setup)
//   - symlink_tree:  create organised symlinks to co-located storage (Orthanc/NFS)
//   - fuse:          lazy on-demand access with block-based caching (FUSE mount)
package vfs

import (
	"context"
	"fmt"
	"os"

	"github.com/colabbio/medvfs/internal/adapters"
	"github.com/colabbio/medvfs/internal/config"
)

// Strategy constants for file virtualisation.
const (
	StrategyDownload    = "download"
	StrategySymlinkTree = "symlink_tree"
	StrategyFUSE        = "fuse"
)

// CacheStats holds runtime metrics for the VFS block cache.
type CacheStats struct {
	// TotalBlocks is the total number of blocks in the cache.
	TotalBlocks int64 `json:"total_blocks"`

	// CachedBlocks is the number of blocks currently cached on disk.
	CachedBlocks int64 `json:"cached_blocks"`

	// HitCount is the cumulative number of cache hits.
	HitCount int64 `json:"hit_count"`

	// MissCount is the cumulative number of cache misses (fetched from remote).
	MissCount int64 `json:"miss_count"`

	// BytesCached is the current cache size in bytes.
	BytesCached int64 `json:"bytes_cached"`

	// BytesFetched is the cumulative bytes fetched from remote sources.
	BytesFetched int64 `json:"bytes_fetched"`

	// FilesTracked is the number of virtual files currently registered.
	FilesTracked int `json:"files_tracked"`
}

// HitRate returns the cache hit ratio as a percentage (0.0–100.0).
func (s CacheStats) HitRate() float64 {
	total := s.HitCount + s.MissCount
	if total == 0 {
		return 0
	}
	return float64(s.HitCount) / float64(total) * 100
}

// VFS is the interface for the virtual file system layer.
// Implementations provide either a FUSE mount or a download/symlink strategy.
type VFS interface {
	// Mount activates the VFS (e.g., mounts the FUSE filesystem).
	// It blocks until ctx is cancelled or Unmount is called.
	Mount(ctx context.Context) error

	// Unmount gracefully shuts down the VFS (e.g., unmounts FUSE).
	Unmount() error

	// AddSource registers file mappings from an adapter.
	// Can be called before or after Mount (FUSE will show new files immediately).
	AddSource(sourceID string, mappings []adapters.FileMapping) error

	// Stats returns current cache and performance metrics.
	Stats() CacheStats
}

// GrantEvictor is implemented by VFS backends that support hot DPO Kill-Switch eviction.
type GrantEvictor interface {
	RevokeAndEvictByGrant(rule CohortGrantRule) (evictedFiles int, evictedBlocks int)
}

// SelectStrategy inspects the serve configuration and runtime environment
// to choose the optimal virtualisation strategy.
func SelectStrategy(cfg *config.VFSConfig) string {
	// Rootless HPC / userspace override (SLURM / Apptainer nodes without /dev/fuse access)
	if os.Getenv("MEDVFS_USERSPACE_STAGING") == "true" || os.Getenv("MEDVFS_USERSPACE_STAGING") == "1" {
		return StrategyDownload
	}

	// Explicit FUSE config → FUSE strategy
	if cfg.Serve != nil && cfg.Serve.FUSE != nil {
		if cfg.Serve.FUSE.MountPoint != "" {
			return StrategyFUSE
		}
	}

	// Check if /dev/fuse is available (Linux FUSE support)
	if _, err := os.Stat("/dev/fuse"); err == nil {
		return StrategyFUSE
	}

	// Fallback: userspace download/staging strategy
	return StrategyDownload
}

// New creates a new VFS instance based on the selected strategy.
func New(cfg *config.VFSConfig, strategy string) (VFS, error) {
	switch strategy {
	case StrategyFUSE:
		if cfg.Serve == nil || cfg.Serve.FUSE == nil {
			return nil, fmt.Errorf("FUSE strategy requires serve.fuse configuration")
		}
		return NewFUSEVFS(cfg.Serve.FUSE)
	case StrategyDownload, StrategySymlinkTree:
		stagingDir := ""
		if cfg != nil {
			stagingDir = cfg.Storage.OutputDir
			if stagingDir == "" && cfg.Serve != nil && cfg.Serve.FUSE != nil {
				stagingDir = cfg.Serve.FUSE.MountPoint
			}
		}
		return NewUserspaceVFS(stagingDir, strategy)
	default:
		return nil, fmt.Errorf("VFS strategy %q not yet implemented", strategy)
	}
}
