# DICOM & WSI Layer — Technical Reference

> **Document:** `cb-vfs/docs/dicom-wsi-layer.md`  
> **Audience:** Pathology engineers, imaging informatics teams, ML researchers working with DICOM/WSI  
> **Prerequisites:** [serve-mode.md](serve-mode.md), [adapters.md](adapters.md)

---

## 1. Overview

MedVFS provides a specialized layer for medical imaging that goes beyond simple file copying. This document covers:

| Component | Purpose |
|---|---|
| **DICOM Local Adapter** | Scan, parse, and organise raw DICOM files into structured paths |
| **Format-Aware Prefetch (L2)** | Anticipate block reads using TIFF IFD and DICOM PixelData metadata |
| **WSI Virtualisation** | Present remote Whole Slide Images as local files via FUSE |
| **Orthanc Adapter** | Query DICOMweb servers for study-level metadata and series manifests |

---

## 2. DICOM Local Adapter

### 2.1 What it does

The DICOM local adapter (`type: dicom`) scans a directory of raw `.dcm` files (e.g. from a DICOM receiver), parses essential DICOM tags, and produces:

1. **`metadata_csv`** — A CSV with `patient_id`, `study_date`, `modality`, `series_number`, `sop_instance_uid`, and the original file path.
2. **`symlink_tree`** — An organised directory tree of symlinks: `{root}/{PatientID}/{YYYYMMDD}_{Modality}/{SeriesNumber}/{filename}.dcm`.
3. **`fuse_tree`** — `FileMapping` entries for serve mode — each DICOM file becomes a virtual FUSE path.

### 2.2 Configuration

```yaml
sources:
  - id: incoming-dicom
    type: dicom
    dicom:
      local_watch_dir: /data/incoming/dicom
      symlink_root: /data/organised/dicom
      extracts:
        - metadata_csv
        - symlink_tree
        - fuse_tree
    materialize:
      output: dicom/metadata.csv
```

### 2.3 DICOM Tags Parsed

| Tag | Name | Tree Level | Example |
|---|---|---|---|
| `(0010,0020)` | PatientID | Root directory | `PT001` |
| `(0008,0020)` | StudyDate | Second level | `20240115` |
| `(0008,0060)` | Modality | Suffix of study dir | `CT`, `MR`, `DX` |
| `(0020,0011)` | SeriesNumber | Third level | `001`, `002` |
| `(0008,0018)` | SOPInstanceUID | Filename (CSV) | `1.2.840...` |

### 2.4 Generated Structure

```
{symlink_root}/
├── PT001/
│   ├── 20240115_CT/
│   │   ├── 001/
│   │   │   ├── img_0001.dcm → /data/incoming/.../file1.dcm
│   │   │   └── img_0002.dcm → /data/incoming/.../file2.dcm
│   │   └── 002/
│   │       └── img_0001.dcm
│   └── 20240220_MR/
│       └── 001/
│           └── img_0001.dcm
└── PT002/
    └── ...
```

### 2.5 Live Monitoring (StreamingAdapter)

The DICOM adapter is the **only** adapter that implements `StreamingAdapter`. When `serve.live_monitoring: true`, it uses `fsnotify` to watch `local_watch_dir` for new `.dcm` files and:

1. Parses the new file's DICOM tags.
2. Creates symlinks in the organised tree.
3. Registers new `FileMapping` entries in the FUSE mount.
4. Emits events via the HTTP SSE stream (`/events`).

This enables **real-time DICOM ingestion** — as a DICOM receiver stores new images, they appear instantly in the FUSE mount.

```
DICOM Receiver → /data/incoming/dicom/new_study/001.dcm
                        │
                        ▼ (fsnotify)
              DICOM Adapter → parse tags
                        │
                        ├─ Symlink: /data/organised/PT042/20240601_CT/001/img_0001.dcm
                        ├─ FUSE:    /data/vfs/dicom/PT042/20240601_CT/001/img_0001.dcm
                        └─ SSE:     {"event":"new_file", "path":"dicom/PT042/..."}
```

---

## 3. WSI (Whole Slide Image) Virtualisation

### 3.1 The Problem

Whole Slide Images present unique challenges for virtualisation:

| Challenge | Details |
|---|---|
| **File size** | 500 MB – 5 GB per slide (`.svs`, `.ndpi`, `.tiff`) |
| **Random access** | Tile viewers read small regions at specific zoom levels |
| **IFD structure** | TIFF/SVS files have an Image File Directory that maps tiles to byte offsets |
| **Latency sensitivity** | Interactive viewers expect < 200ms per tile read |

### 3.2 How MedVFS Handles WSI

MedVFS uses a **format-aware prefetch strategy (L2)** specifically designed for TIFF-based WSI formats:

```
WSI File: slide_001.svs (2.1 GB remote)
    │
    ▼
[First Read: offset=0, size=4MB → Block 0]
    │
    ▼
[L2 Parser detects TIFF magic bytes: 0x4949/0x4D4D]
    │
    ▼
[Parse IFD chain → extract tile offsets + byte counts]
    │
    ▼
[Prefetch tile blocks that are likely needed next]
    │
    ├── Zoom level 0 (thumbnail): blocks 0-2
    ├── Zoom level 1 (overview):  blocks 3-15
    └── Zoom level N (full res):  on-demand per region
```

### 3.3 Block Alignment

The `block_size_mb` setting (default: 4 MB) is chosen to align well with WSI tile structures:

| Format | Typical Tile Size | Tiles per Block |
|---|---|---|
| SVS (Aperio) | 240×240 px, JPEG | ~30-50 tiles/block |
| NDPI (Hamamatsu) | 1024×1024 px, JPEG | ~5-10 tiles/block |
| Generic TIFF | 256×256 px | ~20-40 tiles/block |

### 3.4 Prefetch Strategy for Tile Viewers

When a tile viewer (e.g. OpenSlide, QuPath, ASAP) reads a WSI via FUSE:

1. **Initial open** → L2 parser reads IFD, builds tile-to-block map.
2. **Thumbnail read** → Blocks containing the lowest-resolution IFD are prefetched.
3. **Region navigation** → L1 sequential prefetch covers adjacent blocks.
4. **Zoom level change** → L2 parser predicts which IFD entries map to the new zoom level's blocks.

```yaml
# Optimal serve config for WSI workloads
serve:
  fuse:
    mount_point: /data/vfs
    cache_dir: /var/cache/medvfs
    cache_max_gb: 100        # WSI datasets are large — generous cache
    block_size_mb: 4         # Aligned to tile structure
    prefetch:
      enabled: true
      strategy: format_aware  # L1 + L2 (IFD parsing)
      read_ahead_blocks: 5    # More read-ahead for sequential slide scanning
      concurrent_fetchers: 8  # More fetchers for parallel tile reads
      warmup_percent: 10      # Pre-warm 10% of slides (thumbnails)
```

### 3.5 Supported WSI Formats

| Extension | Format | L2 Parsing | Notes |
|---|---|---|---|
| `.svs` | Aperio ScanScope | ✅ TIFF IFD | Multi-resolution pyramid in single file |
| `.ndpi` | Hamamatsu NanoZoomer | ✅ TIFF IFD | Large offset TIFF variant |
| `.tiff` / `.tif` | Generic BigTIFF | ✅ TIFF IFD | Standard multi-page TIFF |
| `.mrxs` | 3DHISTECH MIRAX | ⚠️ Partial | Directory-based format — requires index file |
| `.vsi` | Olympus VS | ❌ No | Proprietary format — fallback to L1 sequential |
| `.dcm` (WSI) | DICOM WSI | ✅ DICOM tags | Uses PixelData tag `(7FE0,0010)` for block detection |

---

## 4. DICOM PixelData Prefetch

### 4.1 How it Works

For standard DICOM files (CT, MR, DX), the L2 parser detects the `(7FE0,0010)` PixelData tag and prefetches the image data blocks:

```
DICOM File: ct_scan_001.dcm (150 MB)
    │
    ▼
[Block 0 read → DICOM preamble + header]
    │
    ▼
[L2 Parser: scan tags sequentially until PixelData (7FE0,0010)]
    │
    ▼
[PixelData found at offset 2,847 bytes]
    │
    ▼
[Prefetch 4 blocks starting at PixelData offset]
    ├── Block at offset 0 MB (header — already cached)
    ├── Block at offset 4 MB (pixel data start)
    ├── Block at offset 8 MB (pixel data continuation)
    └── Block at offset 12 MB (pixel data continuation)
```

### 4.2 Multi-Frame DICOM

For multi-frame DICOM files (fluoroscopy, ultrasound cine loops), the parser detects the `(0028,0008)` NumberOfFrames tag and distributes prefetch across frame boundaries:

| DICOM Attribute | Usage |
|---|---|
| `(7FE0,0010)` PixelData | Start offset for image data |
| `(0028,0008)` NumberOfFrames | Total frames → compute frame size |
| `(0028,0010)` Rows | Frame dimensions |
| `(0028,0011)` Columns | Frame dimensions |
| `(0028,0100)` BitsAllocated | Bytes per pixel |

---

## 5. Orthanc Adapter for Remote DICOM

For DICOM images stored in a PACS/VNA (accessible via DICOMweb), the Orthanc adapter provides metadata extraction and optional image download:

```yaml
sources:
  - id: hospital-pacs
    type: orthanc
    endpoint: "http://pacs.hospital.es:8042"
    auth:
      secret_ref: "k8s://colabbio-prod/pacs/orthanc_token"
    orthanc:
      study_filter:
        modality: CT
        study_date_from: "20230101"
        study_date_to: "20241231"
        patient_id_prefix: "ONCO"
      extracts: [metadata_csv, series_manifest]
      max_studies: 500
    materialize:
      output: dicom-studies.csv
```

### 5.1 DICOMweb Queries

| Operation | DICOMweb Endpoint | Description |
|---|---|---|
| Study search | `QIDO-RS /studies` | List studies matching filters |
| Series list | `QIDO-RS /studies/{uid}/series` | Series within a study |
| Instance list | `QIDO-RS /series/{uid}/instances` | Instances within a series |
| Metadata | `WADO-RS /metadata` | Full DICOM metadata as JSON |
| Image retrieve | `WADO-RS /studies/{uid}` | Download DICOM files (setup mode) |

### 5.2 Setup vs. Serve Mode

| Mode | Orthanc Behaviour |
|---|---|
| `setup` | Download study metadata → CSV + series manifests. Optionally download DICOM files. |
| `serve` | Register remote DICOM instances as virtual FUSE files. BlockManager fetches on-demand via WADO-RS. |

---

## 6. End-to-End: Pathology AI Pipeline

A complete digital pathology pipeline using MedVFS:

```
Hospital PACS (DICOM)                    Pathology OMERO Server
         │                                         │
         │ Orthanc adapter                         │ OMERO adapter
         │ (QIDO-RS metadata)                      │ (REST API)
         ▼                                         ▼
┌─────────────────────────────────────────────────────────┐
│                    MedVFS serve                          │
│                                                         │
│  FUSE Mount: /data/vfs/                                 │
│  ├── dicom/                                             │
│  │   ├── PT001/20240115_CT/001/img_0001.dcm  (virtual)  │
│  │   └── PT002/20240220_MR/001/img_0001.dcm  (virtual)  │
│  ├── slides/                                            │
│  │   ├── biopsy_001.svs    ← 2.1 GB, L2 prefetch       │
│  │   └── biopsy_002.ndpi   ← 1.8 GB, L2 prefetch       │
│  └── manifests/                                         │
│      ├── dicom-metadata.csv                             │
│      └── slides.csv                                     │
│                                                         │
│  BlockManager Cache: 100 GB LRU                         │
│  Prefetch: format_aware (TIFF IFD + DICOM PixelData)    │
│  HTTP API: :8090/stats → cache hit rate 92%             │
└─────────────────────────────────────────────────────────┘
         │
         ▼
┌─────────────────────────────────────────────────────────┐
│              ML Training Container (readOnly)            │
│                                                         │
│  # The container sees local files — no VFS awareness    │
│  import openslide                                       │
│  slide = openslide.OpenSlide("/data/vfs/slides/001.svs")│
│  tile = slide.read_region((x, y), level, (256, 256))    │
│                                                         │
│  # DICOM files also appear as local                     │
│  import pydicom                                         │
│  ds = pydicom.dcmread("/data/vfs/dicom/PT001/.../001.dcm")│
└─────────────────────────────────────────────────────────┘
```

---

## 7. Performance Tuning

### 7.1 WSI Workloads

| Parameter | Recommended | Rationale |
|---|---|---|
| `cache_max_gb` | 100-200 | WSI datasets are large; generous cache avoids re-fetches |
| `block_size_mb` | 4 | Aligned to typical tile page sizes |
| `strategy` | `format_aware` | L2 IFD parsing critical for random-access tile reading |
| `read_ahead_blocks` | 5-8 | Covers typical tile viewer read-ahead pattern |
| `concurrent_fetchers` | 8-16 | Parallelise fetches for multi-tile reads |
| `warmup_percent` | 10-20 | Pre-cache thumbnails for slide overview rendering |

### 7.2 Radiology (CT/MR) Workloads

| Parameter | Recommended | Rationale |
|---|---|---|
| `cache_max_gb` | 50 | CT series are 200-500 MB; moderate cache sufficient |
| `block_size_mb` | 4 | Good for sequential slice reading |
| `strategy` | `sequential` | CT/MR are read sequentially (slice by slice) |
| `read_ahead_blocks` | 3 | Standard sequential prefetch |
| `concurrent_fetchers` | 4 | Lower parallelism sufficient for sequential reads |
| `warmup_percent` | 0 | No warmup needed — sequential reading is predictable |

### 7.3 Mixed Workloads

```yaml
# Config for mixed pathology + radiology pipeline
serve:
  fuse:
    mount_point: /data/vfs
    cache_dir: /var/cache/medvfs
    cache_max_gb: 150
    block_size_mb: 4
    prefetch:
      enabled: true
      strategy: format_aware   # Best of both worlds
      read_ahead_blocks: 5
      concurrent_fetchers: 8
      warmup_percent: 15
```

---

## 8. Troubleshooting

| Symptom | Likely Cause | Fix |
|---|---|---|
| Slow tile rendering in viewer | Cache cold, L2 not engaged | Check `strategy: format_aware` and increase `warmup_percent` |
| High cache eviction rate | `cache_max_gb` too small for dataset | Increase cache or reduce `warmup_percent` |
| "permission denied" on FUSE mount | FUSE not available in container | Ensure `privileged: true` or `CAP_SYS_ADMIN` + `/dev/fuse` |
| OpenSlide fails to open `.svs` | Block 0 (IFD header) not cached | Verify FUSE mount works: `ls /data/vfs/slides/` |
| fsnotify not detecting new files | `live_monitoring: false` or missing permissions | Enable `serve.live_monitoring: true` and check dir permissions |
| DICOM adapter skips files | File not parseable (truncated or non-DICOM) | Check logs for "failed to parse DICOM tags" warnings |
