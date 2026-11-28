package vfs

import (
	"context"
	"errors"
	"os"
	"os/user"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/colabbio/medvfs/internal/adapters"
	"github.com/colabbio/medvfs/internal/config"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"go.uber.org/zap"
)

// FUSEVFS implements the VFS interface using a FUSE mount.
// It presents remote files as a local filesystem, with reads served
// through the BlockManager's block-based cache.
type FUSEVFS struct {
	mu sync.RWMutex

	cfg    *config.FUSEConfig
	bm     *BlockManager
	pf     *Prefetcher
	log    *zap.Logger
	server *fuse.Server

	// rootNode is the FUSE root inode.
	rootNode *fuseDirNode

	// mappings tracks all registered file mappings by source.
	mappings map[string][]adapters.FileMapping

	// mountTime is set at creation for stable mtime on virtual files.
	mountTime time.Time

	// uid/gid of the process for file ownership in Getattr.
	uid uint32
	gid uint32
}

// NewFUSEVFS creates a new FUSE-based VFS from the config.
func NewFUSEVFS(cfg *config.FUSEConfig) (*FUSEVFS, error) {
	log, _ := zap.NewDevelopment()

	blockSize := int64(cfg.BlockSizeMB) * 1024 * 1024
	if blockSize <= 0 {
		blockSize = DefaultBlockSize
	}
	cacheMax := int64(cfg.CacheMaxGB) * 1024 * 1024 * 1024
	if cacheMax <= 0 {
		cacheMax = DefaultCacheMaxBytes
	}

	fetcher := NewDispatchFetcher(nil, log)
	bm, err := NewBlockManager(blockSize, cfg.CacheDir, cacheMax, fetcher, log)
	if err != nil {
		return nil, err
	}

	// Configure prefetcher
	readAhead := 3
	concurrency := 4
	if cfg.Prefetch != nil {
		if cfg.Prefetch.ReadAheadBlocks > 0 {
			readAhead = cfg.Prefetch.ReadAheadBlocks
		}
		if cfg.Prefetch.ConcurrentFetchers > 0 {
			concurrency = cfg.Prefetch.ConcurrentFetchers
		}
	}
	pf := NewPrefetcher(bm, readAhead, concurrency, log)
	bm.SetPrefetcher(pf)

	// Resolve current uid/gid for file ownership
	uid, gid := resolveProcessOwner()

	return &FUSEVFS{
		cfg:       cfg,
		bm:        bm,
		pf:        pf,
		log:       log,
		rootNode:  &fuseDirNode{children: make(map[string]fs.InodeEmbedder)},
		mappings:  make(map[string][]adapters.FileMapping),
		mountTime: time.Now(),
		uid:       uid,
		gid:       gid,
	}, nil
}

// Mount activates the FUSE filesystem. It blocks until ctx is cancelled.
func (v *FUSEVFS) Mount(ctx context.Context) error {
	if err := os.MkdirAll(v.cfg.MountPoint, 0o755); err != nil {
		return err
	}

	allowOther := true
	if v.cfg.AllowOther != nil {
		allowOther = *v.cfg.AllowOther
	}

	opts := &fs.Options{
		MountOptions: fuse.MountOptions{
			AllowOther: allowOther,
			FsName:     "medvfs",
			Name:       "medvfs",
		},
	}

	server, err := fs.Mount(v.cfg.MountPoint, v.rootNode, opts)
	if err != nil {
		return err
	}
	v.server = server

	v.log.Info("FUSE mounted",
		zap.String("mount_point", v.cfg.MountPoint),
		zap.Int("files_tracked", v.bm.stats.FilesTracked),
	)

	// Wait for unmount or context cancellation
	go func() {
		<-ctx.Done()
		v.Unmount()
	}()

	server.Wait()
	return nil
}

// Unmount gracefully unmounts the FUSE filesystem.
func (v *FUSEVFS) Unmount() error {
	v.pf.Stop()
	if v.server != nil {
		return v.server.Unmount()
	}
	return nil
}

// AddSource registers file mappings from an adapter into the FUSE tree.
func (v *FUSEVFS) AddSource(sourceID string, mappings []adapters.FileMapping) error {
	v.mu.Lock()
	v.mappings[sourceID] = mappings
	v.mu.Unlock()

	for _, m := range mappings {
		srcID := m.SourceID
		if srcID == "" {
			srcID = sourceID
		}
		// Register file in block manager with its sourceID
		v.bm.RegisterFileWithSource(srcID, m.LogicalPath, m.RemoteRef, m.SizeBytes)

		// Register format for prefetch
		ext := fileExtension(m.LogicalPath)
		v.pf.RegisterFileExtension(m.LogicalPath, ext)

		// Add to FUSE directory tree
		v.rootNode.addFile(m.LogicalPath, m.SizeBytes, m.LogicalPath, v.bm, v.mountTime, v.uid, v.gid)
	}

	v.log.Info("source added to VFS",
		zap.String("source", sourceID),
		zap.Int("files", len(mappings)),
	)
	return nil
}

// RegisterSourceAuth registers a source's authentication configuration with the
// underlying DispatchFetcher so FUSE block reads use the source's credentials.
func (v *FUSEVFS) RegisterSourceAuth(src config.SourceConfig) error {
	if df, ok := v.bm.fetcher.(*DispatchFetcher); ok {
		return df.RegisterSourceAuth(src)
	}
	return nil
}

// Stats returns current cache statistics.
func (v *FUSEVFS) Stats() CacheStats {
	return v.bm.GetStats()
}

// ─── FUSE Node Implementations ───────────────────────────────────────────────

// fuseDirNode represents a directory in the FUSE filesystem.
type fuseDirNode struct {
	fs.Inode
	mu       sync.RWMutex
	children map[string]fs.InodeEmbedder
}

// addFile creates the directory tree and adds a file node for a FileMapping.
func (d *fuseDirNode) addFile(logicalPath string, size int64, fileID string, bm *BlockManager, mountTime time.Time, uid, gid uint32) {
	parts := splitPath(logicalPath)
	if len(parts) == 0 {
		return
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	current := d
	for i, part := range parts {
		if i == len(parts)-1 {
			// Leaf — file node
			current.children[part] = &fuseFileNode{
				fileID:    fileID,
				size:      size,
				bm:        bm,
				mountTime: mountTime,
				uid:       uid,
				gid:       gid,
			}
		} else {
			// Intermediate — directory node
			child, ok := current.children[part]
			if !ok {
				dir := &fuseDirNode{children: make(map[string]fs.InodeEmbedder)}
				current.children[part] = dir
				current = dir
			} else if dir, ok := child.(*fuseDirNode); ok {
				current = dir
			}
		}
	}
}

// Readdir returns the directory listing.
func (d *fuseDirNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	entries := make([]fuse.DirEntry, 0, len(d.children))
	for name, child := range d.children {
		mode := uint32(syscall.S_IFREG | 0o444) // regular file
		if _, isDir := child.(*fuseDirNode); isDir {
			mode = uint32(syscall.S_IFDIR | 0o555) // directory
		}
		entries = append(entries, fuse.DirEntry{Name: name, Mode: mode})
	}
	return fs.NewListDirStream(entries), 0
}

// Lookup resolves a child name within this directory.
func (d *fuseDirNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	d.mu.RLock()
	child, ok := d.children[name]
	d.mu.RUnlock()
	if !ok {
		return nil, syscall.ENOENT
	}

	var mode uint32
	if fileNode, isFile := child.(*fuseFileNode); isFile {
		mode = syscall.S_IFREG | 0o444
		out.Size = uint64(fileNode.size)
	} else {
		mode = syscall.S_IFDIR | 0o555
	}
	out.Mode = mode

	stable := fs.StableAttr{Mode: mode}
	inode := d.NewInode(ctx, child, stable)
	return inode, 0
}

// fuseFileNode represents a virtual file backed by the BlockManager.
type fuseFileNode struct {
	fs.Inode
	fileID    string
	size      int64
	bm        *BlockManager
	mountTime time.Time
	uid       uint32
	gid       uint32
}

// Getattr returns complete POSIX file attributes.
// Tools like OpenSlide, libvips, samtools, and rsync check timestamps,
// nlink, ownership, and block counts — incomplete stat results cause
// subtle failures or warnings.
func (f *fuseFileNode) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Size = uint64(f.size)
	out.Mode = syscall.S_IFREG | 0o444
	out.Nlink = 1
	out.Uid = f.uid
	out.Gid = f.gid

	// Stable timestamps: use mount time so tools see consistent mtime.
	// This prevents spurious re-processing by pipeline managers.
	mtime := f.mountTime
	out.SetTimes(
		&mtime, // atime
		&mtime, // mtime
		&mtime, // ctime
	)

	// Block-aligned stat fields — some tools use st_blksize for I/O hints.
	out.Blksize = uint32(f.bm.BlockSize())
	out.Blocks = uint64((f.size + 511) / 512) // st_blocks in 512-byte units

	return 0
}

// Open opens the file for reading.
// Fails immediately with EACCES if the file's Cohort Grant has been revoked via the DPO Kill-Switch.
func (f *fuseFileNode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if f.bm != nil && f.bm.IsFileRevoked(f.fileID) {
		return nil, 0, syscall.EACCES
	}
	return nil, fuse.FOPEN_KEEP_CACHE, 0
}

// Read reads data from the virtual file via the BlockManager.
// If a grant is revoked while a file descriptor is already open, returns EACCES immediately.
func (f *fuseFileNode) Read(ctx context.Context, fh fs.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	n, err := f.bm.ReadAt(ctx, f.fileID, dest, off)
	if err != nil {
		if errors.Is(err, ErrAccessRevoked) {
			return nil, syscall.EACCES
		}
		return nil, syscall.EIO
	}
	return fuse.ReadResultData(dest[:n]), 0
}

// RevokeAndEvictByGrant purges all cached blocks and blocks future FUSE reads for files matching rule.
func (v *FUSEVFS) RevokeAndEvictByGrant(rule CohortGrantRule) (int, int) {
	if v.bm == nil {
		return 0, 0
	}
	return v.bm.RevokeAndEvictByGrant(rule)
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

// splitPath splits a logical path like "PT001/study/series/file.dcm" into parts.
func splitPath(path string) []string {
	var parts []string
	for _, p := range splitPathRaw(path) {
		if p != "" && p != "." {
			parts = append(parts, p)
		}
	}
	return parts
}

func splitPathRaw(path string) []string {
	result := make([]string, 0)
	for path != "" {
		i := 0
		for i < len(path) && path[i] != '/' {
			i++
		}
		result = append(result, path[:i])
		if i < len(path) {
			path = path[i+1:]
		} else {
			break
		}
	}
	return result
}

// resolveProcessOwner returns the uid/gid of the current process.
// Falls back to 0 (root) if resolution fails — this is fine in containers.
func resolveProcessOwner() (uint32, uint32) {
	u, err := user.Current()
	if err != nil {
		return 0, 0
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return 0, 0
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return uint32(uid), 0
	}
	return uint32(uid), uint32(gid)
}
