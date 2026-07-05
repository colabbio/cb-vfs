# MedVFS — Medical Virtual File System

**MedVFS** is a data-plane orchestrator that virtualises heterogeneous clinical data sources — OMOP databases, FHIR servers, DICOM/PACS archives, genomics stores, static datasets — as local files consumable by ML training containers and Nextflow pipelines. It provides automated dataset splits, cross-source patient linking, deterministic versioning, and writable output sync for reproducible AI workflows.

> *"The data doesn't move; the model goes to the data."*

```
┌──────────────────────────────────────────────────────────────┐
│                     ML Training Container                    │
│                 reads /data/vfs/ as local files               │
└───────────────────────────┬──────────────────────────────────┘
                            │ FUSE mount
┌───────────────────────────┴──────────────────────────────────┐
│                      MedVFS Daemon                           │
│  BlockManager ← L1/L2/L3 Prefetch ← HTTP Range Fetcher      │
└─────┬──────────┬──────────┬──────────┬───────────┬───────────┘
      │          │          │          │           │
  OMOP CDM   FHIR R4    OMERO     Orthanc     Genomics
  (Postgres)  (REST)   (.web API)  (DICOMweb)  (GCS/S3/NFS)
```

---

## Features

| Feature | Description |
|---|---|
| **Dual-mode operation** | `setup` (batch materialisation) + `serve` (persistent FUSE daemon) |
| **8 source adapters** | OMOP, FHIR, OMERO, Orthanc, DICOM local, Genomics, **Static Files** (NFS/GCS/S3) |
| **Block-based FUSE** | 4 MB blocks with LRU eviction — remote files appear as local |
| **3-level prefetch** | L1 sequential read-ahead, L2 format-aware (TIFF/DICOM/BAM), L3 manifest warmup |
| **ML dataset engine** | Automated train/val/test splits, cross-source patient linking, deterministic SHA256 manifests |
| **Output sync** | Writable output directory with periodic sync to GCS/S3 for checkpoints, predictions, logs |
| **FHIR/OMOP bridge** | Tabular outputs from EHR adapters auto-injected into the VFS for unified data access |
| **K8s-native** | Helm chart with DaemonSet, Sidecar, Standalone modes + CSI skeleton |
| **Streaming** | Live filesystem updates via `fsnotify` + SSE events |
| **Auth** | Bearer, SMART-on-FHIR, Basic, K8s secrets, env vars |

---

## Quick Start

### Build

```bash
make build
# produces ./medvfs binary
```

### Validate a config

```bash
./medvfs validate --config vfs.yaml
# ✅ "vfs.yaml" is valid — 3 source(s) defined
```

### Materialise data (batch mode)

```bash
./medvfs setup --config vfs.yaml
# Downloads data to output directory
```

### Run as FUSE daemon (streaming mode)

```bash
./medvfs serve --config vfs.yaml
# Mounts VFS at /data/vfs, starts HTTP API on :8090
```

### Docker

```bash
make docker-build
docker run -v $(pwd)/vfs.yaml:/etc/medvfs/vfs.yaml \
           --privileged \
           colabbio/medvfs:latest serve --config /etc/medvfs/vfs.yaml
```

---

## Configuration

All behaviour is driven by a single `vfs.yaml` file. See [docs/quickstart.md](docs/quickstart.md) for annotated examples.

### Minimal example

```yaml
version: "1"
metadata:
  name: pathology-training
storage:
  type: emptydir
setup:
  parallelism: 4
sources:
  - id: slides
    type: omero
    endpoint: https://omero.hospital.org
    auth:
      type: bearer
      secret_ref: env://OMERO_TOKEN
    omero:
      project_id: 42
      extracts: [metadata_csv, thumbnail]
    materialize:
      output: slides/metadata.csv
      download_dir: slides/thumbnails
mount:
  path: /data/vfs
  readonly: true
```

---

## Modes of Operation

### `medvfs setup` — Batch Materialisation

Downloads and transforms all configured sources into flat files on disk. Ideal for:
- ML training with reproducible snapshots
- Regulatory-grade studies (ICH E6, 21 CFR Part 11)
- Nextflow pipeline inputs

### `medvfs serve` — Persistent FUSE Daemon

Mounts a virtual filesystem backed by remote sources. Files appear local but are fetched on-demand with intelligent caching and prefetching. Ideal for:
- Inference pipelines reading WSI tiles
- Real-time DICOM monitoring
- Containers that expect local file paths

#### HTTP API (default `:8090`)

| Endpoint | Method | Description |
|---|---|---|
| `/manifest` | GET | List all virtualised files |
| `/manifest?source=slides` | GET | Filter by source ID |
| `/dataset` | GET | Dataset versioning manifest (SHA256, splits, file count) |
| `/stats` | GET | Cache hit rate, block counts |
| `/events` | GET (SSE) | Live stream of file arrivals |
| `/proxy/{path}` | GET | HTTP fallback for FUSE-less envs |

---

## Prefetch Engine

MedVFS includes a 3-level prefetch engine that anticipates data access patterns:

| Level | Strategy | When |
|---|---|---|
| **L1** | Sequential read-ahead (N+1..N+3 blocks) | Always active |
| **L2** | Format-aware: parses TIFF IFDs, DICOM PixelData tags, BAM headers | On file open / block load |
| **L3** | Manifest warmup: pre-caches headers of smallest files first | On daemon startup |

---

## Kubernetes Deployment

MedVFS ships with a production Helm chart supporting three deployment modes:

```bash
# DaemonSet — persistent mount on every node
helm install medvfs deployments/helm/medvfs/ --set mode=daemonset

# Sidecar — injected into ML pipeline pods
helm install medvfs deployments/helm/medvfs/ --set mode=sidecar

# Standalone — single pod for dev/test
helm install medvfs deployments/helm/medvfs/ --set mode=standalone
```

See [docs/kubernetes.md](docs/kubernetes.md) for detailed configuration.

---

## Source Adapters

| Adapter | Type | Inputs | Output |
|---|---|---|---|
| **OMOP** | `omop` | PostgreSQL (OMOP CDM) | CSV per SQL query |
| **FHIR** | `fhir` | FHIR R4 REST API | CSV with flattened fields |
| **OMERO** | `omero` | OMERO.web JSON API | Metadata CSV + thumbnails |
| **Orthanc** | `orthanc` | DICOMweb (QIDO/WADO-RS) | Metadata CSV + DICOM files |
| **DICOM local** | `dicom` | Local directory | Symlink tree + CSV |
| **Genomics** | `genomics` | NFS / GCS / S3 | Manifest CSV with validated indexes |
| **Static Files** | `files` | NFS / GCS / S3 | 1:1 file mapping with glob filters |

See [docs/adapters.md](docs/adapters.md) for per-adapter configuration reference.

---

## Project Structure

```
cb-vfs/
├── cmd/medvfs/              # CLI entry point (cobra)
├── internal/
│   ├── adapters/            # Source adapters
│   │   ├── dicom/           # Local DICOM directory scanner
│   │   ├── fhir/            # FHIR R4 REST → CSV
│   │   ├── files/           # Static files (NFS/GCS/S3) ← NEW
│   │   ├── genomics/        # BAM/CRAM/VCF from NFS/GCS/S3
│   │   ├── omero/           # OMERO.web JSON API
│   │   ├── omop/            # OMOP CDM (PostgreSQL)
│   │   └── orthanc/         # DICOMweb (QIDO/WADO-RS)
│   ├── config/              # vfs.yaml schema and loader
│   ├── dataset/             # ML dataset engine (splits, linking, versioning) ← NEW
│   ├── output/              # Writable output sync (GCS/S3) ← NEW
│   ├── secrets/             # Secret resolution (k8s://, env://, file://)
│   ├── serve/               # Daemon engine + HTTP/SSE server
│   ├── setup/               # Batch materialisation engine
│   └── vfs/                 # FUSE filesystem, block cache, prefetch
├── test/
│   └── integration/         # End-to-end pipeline tests ← NEW
├── deployments/
│   ├── helm/medvfs/         # Production Helm chart (with ML config)
│   └── examples/            # Example vfs.yaml configs
├── docs/                    # Technical documentation
├── Makefile
└── go.mod
```

---

## Development

```bash
# Run all tests
make test

# Run integration tests (requires OMOP_DSN env var)
make test-integration

# Lint
make lint

# Start local OMOP test database
make dev-db
```

---

## Documentation

| Document | Location | Description |
|---|---|---|
| [Quick Start](docs/quickstart.md) | `cb-vfs/docs/` | Getting started with annotated examples |
| [YAML Spec](../cb-docs/architecture/medvfs-yaml-spec.md) | `cb-docs/` | Complete `vfs.yaml` reference |
| [Runtime Architecture](../cb-docs/architecture/medvfs-runtime-architecture.md) | `cb-docs/` | Architecture, Nextflow integration, K8s |
| [Market Analysis](../cb-docs/architecture/medvfs-market-disruption.md) | `cb-docs/` | Competitive landscape |

---

## License

Proprietary — ColabBio / Healthcentrix LLC. All rights reserved.
