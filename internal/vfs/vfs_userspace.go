package vfs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/colabbio/medvfs/internal/adapters"
)

// UserspaceVFS implements VFS without requiring /dev/fuse or CAP_SYS_ADMIN.
// Designed for rootless HPC SLURM/Apptainer nodes (e.g., BSC MareNostrum 5)
// and zero-K8s laptop runs (colabbio/cb-vfs#7).
type UserspaceVFS struct {
	mu           sync.RWMutex
	stagingDir   string
	strategy     string
	mappings     map[string][]adapters.FileMapping
	stagedPaths  map[string]string // logicalPath -> physical staged path
	revokedPaths map[string]string
	stats        CacheStats
}

// NewUserspaceVFS creates a rootless userspace VFS rooted at stagingDir.
func NewUserspaceVFS(stagingDir string, strategy string) (*UserspaceVFS, error) {
	if stagingDir == "" {
		stagingDir = filepath.Join(os.TempDir(), "medvfs-userspace")
	}
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		return nil, err
	}
	return &UserspaceVFS{
		stagingDir:   stagingDir,
		strategy:     strategy,
		mappings:     make(map[string][]adapters.FileMapping),
		stagedPaths:  make(map[string]string),
		revokedPaths: make(map[string]string),
	}, nil
}

// Mount blocks until ctx is cancelled ( matching FUSEVFS lifecycle).
func (u *UserspaceVFS) Mount(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

// Unmount cleans up userspace resources.
func (u *UserspaceVFS) Unmount() error {
	return nil
}

// AddSource materializes symlinks or lightweight placeholder descriptors in the userspace directory.
func (u *UserspaceVFS) AddSource(sourceID string, mappings []adapters.FileMapping) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.mappings[sourceID] = mappings

	for _, m := range mappings {
		if _, revoked := u.revokedPaths[m.LogicalPath]; revoked {
			continue
		}
		target := filepath.Join(u.stagingDir, filepath.FromSlash(m.LogicalPath))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if strings.HasPrefix(m.RemoteRef, "file://") {
			localSrc := strings.TrimPrefix(m.RemoteRef, "file://")
			_ = os.Remove(target)
			if err := os.Symlink(localSrc, target); err != nil {
				// Fallback if symlinks are unsupported
				if data, readErr := os.ReadFile(localSrc); readErr == nil {
					_ = os.WriteFile(target, data, 0o600)
				}
			}
		} else if _, err := os.Stat(target); os.IsNotExist(err) {
			_ = os.WriteFile(target, []byte{}, 0o600)
		}
		u.stagedPaths[m.LogicalPath] = target
		u.stats.FilesTracked++
	}
	return nil
}

// RevokeAndEvictByGrant deletes staged files matching the revoked DPO grant from the userspace directory.
func (u *UserspaceVFS) RevokeAndEvictByGrant(rule CohortGrantRule) (int, int) {
	u.mu.Lock()
	defer u.mu.Unlock()

	matchAll := len(rule.AllowedPaths) == 0 && len(rule.AllowedPatients) == 0
	evicted := 0
	for logical, physical := range u.stagedPaths {
		if matchAll || RuleMatchesPath(rule, logical) {
			u.revokedPaths[logical] = rule.GrantID
			_ = os.Remove(physical)
			delete(u.stagedPaths, logical)
			evicted++
		}
	}
	return evicted, evicted
}

// Stats returns current userspace VFS statistics.
func (u *UserspaceVFS) Stats() CacheStats {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return u.stats
}
