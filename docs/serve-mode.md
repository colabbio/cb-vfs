# MedVFS — Serve Mode (FUSE Daemon)

> **Source:** `internal/serve/` + `internal/vfs/`

---

## Overview

`medvfs serve` runs MedVFS as a persistent daemon that:

1. **Builds file trees** from all configured adapters
2. **Mounts a FUSE filesystem** that exposes remote files as local paths
3. **Caches blocks on disk** with intelligent prefetching
4. **Starts an HTTP API** for manifest queries, cache stats, and live events
5. **Watches for new files** via streaming adapters (e.g., fsnotify on DICOM dirs)

```
ML Container                    MedVFS Daemon
─────────────                   ─────────────
                                ┌─────────────────────────────┐
open("/data/vfs/PT001/img.dcm") │ FUSE FS (fuse.go)           │
    │                           │   └── ReadAt(offset, len)    │
    ▼                           │        │                     │
  kernel ──FUSE───────────────► │ BlockManager (block.go)      │
                                │   ├── block cached? → read   │
                                │   └── miss → HTTP Range GET  │
                                │        │                     │
                                │ Prefetch Engine (prefetch.go)│
                                │   ├── L1: sequential N+1..+3 │
                                │   ├── L2: format-aware       │
                                │   └── L3: manifest warmup    │
                                └─────────────────────────────┘
                                         │
                                    HTTP API (:8090)
                                  /manifest /stats /events
```

---

## Lifecycle

The serve engine ([engine.go](../internal/serve/engine.go)) orchestrates 6 stages:

| Stage | Action | Code |
|---|---|---|
| 1 | Select VFS strategy (FUSE / download / symlink) | `vfs.SelectStrategy(cfg)` |
| 2 | Create VFS instance | `vfs.New(cfg, strategy)` |
| 3 | Build adapters + file trees | `adapter.BuildFileTree(ctx, "", strategy)` |
| 4 | Start streaming adapters (if any) | `adapter.StartListening(ctx, events)` |
| 5 | Start HTTP server (non-blocking) | `server.Start(ctx)` |
| 6 | Mount VFS (blocks until shutdown) | `v.Mount(ctx)` |

### Strategy Selection

The VFS strategy is auto-detected:

```
serve.fuse.mount_point configured?
  └─ yes → FUSE strategy
  └─ no  → /dev/fuse exists?
              └─ yes → FUSE strategy
              └─ no  → download strategy (fallback)
```

---

## VFS Interface

```go
type VFS interface {
    Mount(ctx context.Context) error
    Unmount() error
    AddSource(sourceID string, mappings []FileMapping) error
    Stats() CacheStats
}
```

Files can be registered before or after `Mount()`. When using FUSE, newly added files appear immediately in the mounted filesystem.

---

## BlockManager

> **Source:** [block.go](../internal/vfs/block.go)

The BlockManager splits every virtual file into fixed-size blocks and fetches them on-demand from remote sources. It provides `ReadAt(ctx, fileID, buf, offset)` — the core read path for both FUSE and the HTTP proxy fallback.

### Configuration

| Parameter | Default | YAML Path |
|---|---|---|
| Block size | 4 MB | `serve.fuse.block_size_mb` |
| Cache directory | `/var/cache/medvfs` | `serve.fuse.cache_dir` |
| Max cache size | 50 GB | `serve.fuse.cache_max_gb` |

### Block States

```
blockEmpty ──fetch──► blockFetching ──done──► blockCached
     ▲                                          │
     └──────────── evict ◄──────────────────────┘
```

| State | Description |
|---|---|
| `blockEmpty` | Not cached, will trigger fetch on next read |
| `blockFetching` | Download in progress (concurrent readers wait) |
| `blockCached` | Available on disk (and optionally in memory) |

### Read Flow

1. **Calculate block index**: `blockIdx = offset / blockSize`
2. **Check cache**: If `blockCached`, read from disk (or memory if hot)
3. **Fetch on miss**: `RemoteFetcher.FetchBlock(remoteRef, offset, length)` via HTTP Range
4. **Write to disk cache**: `{cacheDir}/blocks/{prefix}/{fileID}_{block:06d}.blk`
5. **Trigger prefetch**: `prefetcher.OnBlockAccess(fileID, blockIdx)` queues adjacent blocks

### Disk Layout

```
/var/cache/medvfs/
└── blocks/
    ├── PT001_20/
    │   ├── PT001_2024_CT_img001_000000.blk
    │   ├── PT001_2024_CT_img001_000001.blk
    │   └── PT001_2024_CT_img001_000002.blk
    └── slide_b/
        ├── slide_breast_42_000000.blk
        └── slide_breast_42_000001.blk
```

### LRU Eviction

When `BytesCached > cacheMaxBytes`, the eviction algorithm:

1. Collects all cached blocks with their access counts
2. Sorts by access count ascending (least-used first)
3. Evicts blocks (deletes disk file, resets to `blockEmpty`) until under the limit

---

## Prefetch Engine

> **Source:** [prefetch.go](../internal/vfs/prefetch.go)

The prefetch engine runs 3 levels of anticipatory block fetching to minimise read latency.

### Architecture

```
┌──────────────────────────────────────────────────┐
│                Prefetcher                        │
│                                                  │
│  fetchQueue (chan, cap=256)                       │
│      │                                           │
│      ├── worker[0] ──► BlockManager.Prefetch()   │
│      ├── worker[1] ──► BlockManager.Prefetch()   │
│      ├── worker[2] ──► BlockManager.Prefetch()   │
│      └── worker[3] ──► BlockManager.Prefetch()   │
│                                                  │
│  Triggers:                                       │
│    OnBlockAccess(fileID, block) → L1 sequential  │
│    OnFileOpen(fileID, header)   → L2 format      │
│    WarmUp(mappings, percent)    → L3 manifest    │
└──────────────────────────────────────────────────┘
```

### Configuration

| Parameter | Default | YAML Path |
|---|---|---|
| Enabled | `true` | `serve.fuse.prefetch.enabled` |
| Strategy | `format_aware` | `serve.fuse.prefetch.strategy` |
| Read-ahead blocks | 3 | `serve.fuse.prefetch.read_ahead_blocks` |
| Concurrent fetchers | 4 | `serve.fuse.prefetch.concurrent_fetchers` |
| Warmup percent | 20 | `serve.fuse.prefetch.warmup_percent` |

### L1: Sequential Read-Ahead

**Trigger:** Every `ReadAt()` call → `OnBlockAccess(fileID, blockIdx)`

When block N is read, blocks N+1, N+2, N+3 are queued at `PriorityLow`. This covers sequential scan patterns (e.g., reading a BAM file linearly).

### L2: Format-Aware Prefetch

**Trigger:** `OnFileOpen(fileID, header)` + `OnBlockLoaded(fileID, blockIdx, data)`

Format-specific profiles parse file headers to predict which blocks will be needed:

#### TIFF/SVS/NDPI Profile

| Event | Action |
|---|---|
| `OnFileOpen` | Parse header bytes 0–7 (byte order + IFD offset) → prefetch IFD block |
| `OnBlockLoaded` | Scan for TileOffsets (tag 324) / StripOffsets (tag 273) → prefetch up to 16 tile blocks |

**TIFF Header Format:**
```
[0-1]  Byte order: "II" (little-endian) or "MM" (big-endian)
[2-3]  Magic: 42 (0x2A)
[4-7]  First IFD offset (uint32)
```

**IFD Entry Format (12 bytes each):**
```
[0-1]  Tag ID (uint16) — e.g., 324 = TileOffsets
[2-3]  Field type (uint16)
[4-7]  Value count (uint32)
[8-11] Value or offset (uint32)
```

#### DICOM Profile

| Event | Action |
|---|---|
| `OnFileOpen` | No-op (metadata is in block 0, already loaded) |
| `OnBlockLoaded` | Scan for PixelData tag `(7FE0,0010)` → prefetch next 4 blocks |

The PixelData tag marks the boundary between DICOM metadata and the actual image data. Prefetching the blocks after this tag anticipates image rendering.

#### BAM/CRAM Profile

| Event | Action |
|---|---|
| `OnFileOpen` | Check magic bytes (`BAM\1` or `CRAM`) → prefetch block 1 (first alignments) |
| `OnBlockLoaded` | No-op (L1 sequential handles linear reads) |

### L3: Manifest Warmup

**Trigger:** Daemon startup → `WarmUp(mappings, warmupPercent)`

1. Sort all files by size (ascending — small files first)
2. Prefetch block 0 (header) of the smallest N% of files
3. Optimised for datasets with many small index/metadata files + few large data files

### Priority System

| Priority | Use Case | Queue Behaviour |
|---|---|---|
| `PriorityHigh` | Format-aware (IFD, PixelData, BAM header) | Processed first (planned) |
| `PriorityLow` | Sequential read-ahead, warmup | Background, can be dropped if queue full |

---

## HTTP API

> **Source:** [server.go](../internal/serve/server.go)

### Endpoints

| Endpoint | Method | Response | Description |
|---|---|---|---|
| `/health` | GET | `{"status": "ok"}` | Health check (K8s readiness probe) |
| `/manifest` | GET | `{"files": [...], "count": N}` | List all virtualised files |
| `/manifest?source=wsi` | GET | `{"files": [...], "count": N}` | Filter by source ID |
| `/stats` | GET | `{"cache": {...}, "hit_rate": "88.7%"}` | Cache statistics |
| `/events` | GET (SSE) | `event: new_file\ndata: {...}` | Live event stream |

### Cache Stats Response

```json
{
  "cache": {
    "total_blocks": 160,
    "cached_blocks": 142,
    "hit_count": 1580,
    "miss_count": 160,
    "bytes_cached": 595591168,
    "bytes_fetched": 671088640,
    "files_tracked": 47
  },
  "hit_rate": "90.8%"
}
```

### SSE Events

Live events are emitted by streaming adapters (e.g., DICOM local watcher):

```
event: new_file
data: {"source_id":"dicom_local","type":"new_file","file":{"logical_path":"PT001/CT/series1/img001.dcm","remote_ref":"/data/incoming/PT001/img001.dcm","size_bytes":524288},"timestamp":"2025-06-25T10:30:00Z"}
```

---

## RemoteFetcher Interface

```go
type RemoteFetcher interface {
    // FetchBlock retrieves bytes [offset, offset+length) from the remote source.
    FetchBlock(ctx context.Context, remoteRef string, offset, length int64) ([]byte, error)

    // FileSize returns the total size of the remote file.
    FileSize(ctx context.Context, remoteRef string) (int64, error)
}
```

The default implementation (`HTTPFetcher` in [fetcher.go](../internal/vfs/fetcher.go)) uses HTTP Range requests:

```
GET {remoteRef}
Range: bytes={offset}-{offset+length-1}
→ 206 Partial Content
```

---

## Security Considerations

| Requirement | Reason |
|---|---|
| `CAP_SYS_ADMIN` or `privileged: true` | FUSE mount requires this Linux capability |
| Network access to source endpoints | BlockManager fetches via HTTP Range |
| Disk write to cache directory | Block cache writes .blk files |
| `/dev/fuse` device available | Required for FUSE userspace filesystem |
