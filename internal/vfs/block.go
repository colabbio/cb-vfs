package vfs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"go.uber.org/zap"
)

const (
	// DefaultBlockSize is the default block size in bytes (4 MB).
	DefaultBlockSize int64 = 4 * 1024 * 1024

	// DefaultCacheMaxBytes is the default maximum cache size (50 GB).
	DefaultCacheMaxBytes int64 = 50 * 1024 * 1024 * 1024
)

// blockStatus represents the state of a block in the cache.
type blockStatus int

const (
	blockEmpty    blockStatus = iota // not cached
	blockFetching                    // download in progress
	blockCached                      // available on disk
)

// blockEntry holds metadata for a single cache block.
type blockEntry struct {
	mu     sync.RWMutex
	status blockStatus

	// data holds the block content when cached in memory (hot blocks only).
	// Blocks may be evicted from memory but remain on disk.
	data []byte

	// diskPath is the path to the block file on disk.
	diskPath string

	// size is the actual number of bytes in this block (may be < BlockSize for the last block).
	size int64

	// accessCount tracks the number of reads (for LRU eviction).
	accessCount atomic.Int64
}

// BlockManager manages block-level access to virtual files.
// It splits files into fixed-size blocks, fetching them on-demand from remote
// sources and caching them on local disk.
type BlockManager struct {
	mu sync.RWMutex

	// blockSize is the size of each block in bytes.
	blockSize int64

	// cacheDir is the directory where block files are stored.
	cacheDir string

	// cacheMaxBytes is the maximum total size of the block cache.
	cacheMaxBytes int64

	// fetcher is the remote data source.
	fetcher RemoteFetcher

	// files maps fileID → file metadata.
	files map[string]*fileInfo

	// stats tracks cache metrics.
	stats CacheStats

	// log is the logger.
	log *zap.Logger

	// prefetcher is the background prefetch engine.
	prefetcher *Prefetcher
}

// fileInfo holds metadata for a single virtual file.
type fileInfo struct {
	remoteRef string
	size      int64
	blocks    map[int]*blockEntry
	mu        sync.RWMutex
}

// numBlocks returns the number of blocks needed for this file.
func (f *fileInfo) numBlocks(blockSize int64) int {
	n := f.size / blockSize
	if f.size%blockSize != 0 {
		n++
	}
	return int(n)
}

// NewBlockManager creates a block manager with the given parameters.
func NewBlockManager(blockSize int64, cacheDir string, cacheMaxBytes int64, fetcher RemoteFetcher, log *zap.Logger) (*BlockManager, error) {
	if blockSize <= 0 {
		blockSize = DefaultBlockSize
	}
	if cacheMaxBytes <= 0 {
		cacheMaxBytes = DefaultCacheMaxBytes
	}
	if cacheDir == "" {
		cacheDir = filepath.Join(os.TempDir(), "medvfs-cache")
	}

	blocksDir := filepath.Join(cacheDir, "blocks")
	if err := os.MkdirAll(blocksDir, 0o755); err != nil {
		return nil, fmt.Errorf("create cache dir: %w", err)
	}

	return &BlockManager{
		blockSize:     blockSize,
		cacheDir:      cacheDir,
		cacheMaxBytes: cacheMaxBytes,
		fetcher:       fetcher,
		files:         make(map[string]*fileInfo),
		log:           log,
	}, nil
}

// SetPrefetcher attaches a prefetch engine to this block manager.
func (bm *BlockManager) SetPrefetcher(p *Prefetcher) {
	bm.mu.Lock()
	defer bm.mu.Unlock()
	bm.prefetcher = p
}

// RegisterFile registers a new virtual file for block-based access.
func (bm *BlockManager) RegisterFile(fileID string, remoteRef string, size int64) {
	bm.mu.Lock()
	defer bm.mu.Unlock()
	bm.files[fileID] = &fileInfo{
		remoteRef: remoteRef,
		size:      size,
		blocks:    make(map[int]*blockEntry),
	}
	bm.stats.FilesTracked++
}

// FileSize returns the known size of a registered file.
func (bm *BlockManager) FileSize(fileID string) (int64, bool) {
	bm.mu.RLock()
	defer bm.mu.RUnlock()
	fi, ok := bm.files[fileID]
	if !ok {
		return 0, false
	}
	return fi.size, true
}

// BlockSize returns the configured block size in bytes.
// Used by the FUSE layer to set st_blksize in Getattr, which tools
// like OpenSlide may use as an I/O size hint.
func (bm *BlockManager) BlockSize() int64 {
	return bm.blockSize
}

// ReadAt reads len(p) bytes from the virtual file starting at byte offset off.
// It fetches blocks on-demand from the remote source if not cached.
func (bm *BlockManager) ReadAt(ctx context.Context, fileID string, p []byte, off int64) (int, error) {
	bm.mu.RLock()
	fi, ok := bm.files[fileID]
	bm.mu.RUnlock()
	if !ok {
		return 0, fmt.Errorf("file %q not registered", fileID)
	}

	if off >= fi.size {
		return 0, fmt.Errorf("offset %d beyond file size %d", off, fi.size)
	}

	// Clamp read to file boundary
	readLen := int64(len(p))
	if off+readLen > fi.size {
		readLen = fi.size - off
	}

	totalRead := 0
	remaining := readLen

	for remaining > 0 {
		blockIdx := int(off / bm.blockSize)
		blockOffset := off % bm.blockSize

		// How many bytes to read from this block
		bytesInBlock := bm.blockSize - blockOffset
		if bytesInBlock > remaining {
			bytesInBlock = remaining
		}

		blockData, err := bm.getBlock(ctx, fi, fileID, blockIdx)
		if err != nil {
			return totalRead, fmt.Errorf("get block %d: %w", blockIdx, err)
		}

		// Copy from block to output buffer
		if blockOffset >= int64(len(blockData)) {
			break
		}
		end := blockOffset + bytesInBlock
		if end > int64(len(blockData)) {
			end = int64(len(blockData))
		}
		n := copy(p[totalRead:], blockData[blockOffset:end])
		totalRead += n
		off += int64(n)
		remaining -= int64(n)

		// Trigger prefetch for adjacent blocks
		if bm.prefetcher != nil {
			bm.prefetcher.OnBlockAccess(fileID, blockIdx)
		}
	}

	return totalRead, nil
}

// getBlock returns the data for a specific block, fetching it from the
// remote source if not cached.
func (bm *BlockManager) getBlock(ctx context.Context, fi *fileInfo, fileID string, blockIdx int) ([]byte, error) {
	fi.mu.Lock()
	entry, exists := fi.blocks[blockIdx]
	if !exists {
		entry = &blockEntry{
			status:   blockEmpty,
			diskPath: bm.blockPath(fileID, blockIdx),
		}
		fi.blocks[blockIdx] = entry
	}
	fi.mu.Unlock()

	entry.mu.Lock()
	defer entry.mu.Unlock()

	switch entry.status {
	case blockCached:
		// Cache hit — read from disk or memory
		entry.accessCount.Add(1)
		bm.stats.HitCount++
		if entry.data != nil {
			return entry.data, nil
		}
		data, err := os.ReadFile(entry.diskPath)
		if err != nil {
			// Disk cache corrupted, re-fetch
			entry.status = blockEmpty
			return bm.fetchAndCache(ctx, fi, entry, fileID, blockIdx)
		}
		return data, nil

	case blockFetching:
		// Another goroutine is fetching — wait by re-acquiring lock
		// (simplified; in production use sync.Cond or channel)
		entry.mu.Unlock()
		entry.mu.Lock()
		if entry.status == blockCached {
			entry.accessCount.Add(1)
			bm.stats.HitCount++
			if entry.data != nil {
				return entry.data, nil
			}
			return os.ReadFile(entry.diskPath)
		}
		return nil, fmt.Errorf("block %d fetch failed", blockIdx)

	case blockEmpty:
		// Cache miss — fetch from remote
		bm.stats.MissCount++
		return bm.fetchAndCache(ctx, fi, entry, fileID, blockIdx)
	}

	return nil, fmt.Errorf("unknown block status for block %d", blockIdx)
}

// fetchAndCache downloads a block from the remote source and writes it to disk.
func (bm *BlockManager) fetchAndCache(ctx context.Context, fi *fileInfo, entry *blockEntry, fileID string, blockIdx int) ([]byte, error) {
	entry.status = blockFetching

	offset := int64(blockIdx) * bm.blockSize
	length := bm.blockSize
	if offset+length > fi.size {
		length = fi.size - offset
	}

	data, err := bm.fetcher.FetchBlock(ctx, fi.remoteRef, offset, length)
	if err != nil {
		entry.status = blockEmpty
		return nil, fmt.Errorf("fetch block %d from %s: %w", blockIdx, fi.remoteRef, err)
	}

	// Write to disk cache
	if err := os.MkdirAll(filepath.Dir(entry.diskPath), 0o755); err != nil {
		bm.log.Warn("failed to create block cache dir", zap.Error(err))
	} else if err := os.WriteFile(entry.diskPath, data, 0o644); err != nil {
		bm.log.Warn("failed to write block to cache", zap.String("path", entry.diskPath), zap.Error(err))
	}

	entry.data = data
	entry.size = int64(len(data))
	entry.status = blockCached
	entry.accessCount.Add(1)

	bm.stats.BytesCached += entry.size
	bm.stats.BytesFetched += entry.size

	return data, nil
}

// Prefetch queues blocks for background download.
func (bm *BlockManager) Prefetch(ctx context.Context, fileID string, blockIndices []int) {
	bm.mu.RLock()
	fi, ok := bm.files[fileID]
	bm.mu.RUnlock()
	if !ok {
		return
	}

	for _, blockIdx := range blockIndices {
		if blockIdx < 0 || blockIdx >= fi.numBlocks(bm.blockSize) {
			continue
		}

		fi.mu.RLock()
		entry, exists := fi.blocks[blockIdx]
		fi.mu.RUnlock()

		if exists && entry.status != blockEmpty {
			continue // already cached or being fetched
		}

		// Fetch in background
		go func(idx int) {
			_, err := bm.getBlock(ctx, fi, fileID, idx)
			if err != nil {
				bm.log.Debug("prefetch failed", zap.String("file", fileID), zap.Int("block", idx), zap.Error(err))
			}
		}(blockIdx)
	}
}

// Evict removes least-recently-used blocks to bring cache size under the limit.
func (bm *BlockManager) Evict() int {
	if bm.stats.BytesCached <= bm.cacheMaxBytes {
		return 0
	}

	type candidate struct {
		fileID   string
		blockIdx int
		access   int64
		size     int64
	}

	var candidates []candidate

	bm.mu.RLock()
	for fileID, fi := range bm.files {
		fi.mu.RLock()
		for idx, entry := range fi.blocks {
			if entry.status == blockCached {
				candidates = append(candidates, candidate{
					fileID:   fileID,
					blockIdx: idx,
					access:   entry.accessCount.Load(),
					size:     entry.size,
				})
			}
		}
		fi.mu.RUnlock()
	}
	bm.mu.RUnlock()

	// Sort by access count (ascending — evict least-used first)
	for i := 0; i < len(candidates); i++ {
		for j := i + 1; j < len(candidates); j++ {
			if candidates[j].access < candidates[i].access {
				candidates[i], candidates[j] = candidates[j], candidates[i]
			}
		}
	}

	evicted := 0
	for _, c := range candidates {
		if bm.stats.BytesCached <= bm.cacheMaxBytes {
			break
		}

		bm.mu.RLock()
		fi, ok := bm.files[c.fileID]
		bm.mu.RUnlock()
		if !ok {
			continue
		}

		fi.mu.Lock()
		entry, ok := fi.blocks[c.blockIdx]
		if ok && entry.status == blockCached {
			os.Remove(entry.diskPath)
			entry.data = nil
			entry.status = blockEmpty
			bm.stats.BytesCached -= c.size
			evicted++
		}
		fi.mu.Unlock()
	}

	bm.log.Info("cache eviction complete",
		zap.Int("evicted", evicted),
		zap.Int64("cache_bytes", bm.stats.BytesCached),
	)
	return evicted
}

// GetStats returns a snapshot of the current cache statistics.
func (bm *BlockManager) GetStats() CacheStats {
	return bm.stats
}

// blockPath returns the filesystem path for a cached block file.
func (bm *BlockManager) blockPath(fileID string, blockIdx int) string {
	// Use first 8 chars of fileID as subdirectory for filesystem fan-out
	prefix := fileID
	if len(prefix) > 8 {
		prefix = prefix[:8]
	}
	return filepath.Join(bm.cacheDir, "blocks", prefix, fmt.Sprintf("%s_%06d.blk", fileID, blockIdx))
}
