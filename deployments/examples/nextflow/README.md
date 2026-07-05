# MedVFS + Nextflow — Multimodal Oncology ML Pipeline

## Overview

This Nextflow pipeline demonstrates a production-ready integration between **MedVFS** (data virtualisation) and **Nextflow** (workflow orchestration) for multimodal clinical AI training.

### Architecture

```
┌──────────────────────────────────────────────────────────────────┐
│                      Nextflow Orchestrator                        │
│                                                                    │
│  ┌─────────────┐    ┌──────────────┐    ┌────────────────────┐   │
│  │  MEDVFS     │    │  TILE        │    │  FEATURE           │   │
│  │  SETUP      │───▶│  EXTRACT     │───▶│  EXTRACT           │   │
│  │  (sidecar)  │    │  (OpenSlide) │    │  (ResNet50 GPU)    │   │
│  └─────────────┘    └──────────────┘    └────────┬───────────┘   │
│        │                                          │               │
│        │ .dataset.json                           ▼               │
│        │                              ┌────────────────────┐     │
│        └─────────────────────────────▶│  TRAIN_MODEL       │     │
│                                       │  (PyTorch GPU)     │     │
│                                       └────────┬───────────┘     │
│                                                │                 │
│                                                ▼                 │
│                                       ┌────────────────────┐     │
│                                       │  EVALUATE          │     │
│                                       │  (metrics + report)│     │
│                                       └────────────────────┘     │
└──────────────────────────────────────────────────────────────────┘
```

### Data Flow

| Stage | Input | Output |
|---|---|---|
| `MEDVFS_SETUP` | `vfs.yaml` | FUSE-mounted `/data/vfs/{train,val,test}/...` + `.dataset.json` |
| `TILE_EXTRACT` | WSI slides (`.svs`) | `tiles/{split}/{patient}/{tile}.png` |
| `FEATURE_EXTRACT` | PNG tiles + pretrained model | `features/{patient}.npy` (2048-d vectors) |
| `TRAIN_MODEL` | Train/val features + EHR CSVs | `model_checkpoint.pt` + `training_log.csv` |
| `EVALUATE` | Trained model + test features | `evaluation_report.json` |

## Quick Start

### Local (Docker)

```bash
# Run with the included example config
nextflow run main.nf \
  -profile local \
  --vfs_config ../../deployments/examples/ml-pipeline.yaml \
  --epochs 10 \
  --output_dir ./results
```

### Kubernetes

```bash
# Assumes:
#   - K8s cluster with GPU nodes labeled node-role.kubernetes.io/ml=true
#   - medvfs-sa ServiceAccount with cloud storage access
#   - MedVFS container image published to ghcr.io/colabbio/medvfs

nextflow run main.nf \
  -profile k8s \
  --vfs_config gs://colabbio-configs/lung_cancer/vfs.yaml \
  --epochs 50 \
  --batch_size 64 \
  --output_dir gs://colabbio-results/run-042/
```

## Parameters

| Parameter | Default | Description |
|---|---|---|
| `--vfs_config` | `./vfs.yaml` | MedVFS configuration file |
| `--tile_size` | `256` | WSI tile size in pixels |
| `--tile_level` | `0` | OpenSlide pyramid level (0 = highest resolution) |
| `--batch_size` | `32` | Training batch size |
| `--epochs` | `50` | Number of training epochs |
| `--learning_rate` | `0.0001` | Adam learning rate |
| `--model` | `resnet50` | Feature extractor backbone |
| `--output_dir` | `./results` | Directory for final outputs |

## MedVFS Integration Patterns

### Pattern A: MedVFS as Init Container (Batch)

```groovy
process MEDVFS_BATCH {
    container 'ghcr.io/colabbio/medvfs:latest'
    script:
    """
    medvfs setup --config vfs.yaml --output-dir ./data
    """
}
```

Best for: Reproducible training runs where data must be snapshotted.

### Pattern B: MedVFS as FUSE Sidecar (Streaming)

Used in this pipeline. MedVFS runs in the background and presents remote data as local files via FUSE. The dataset engine applies splits and patient linking on-the-fly.

Best for: Large datasets (>100 GB) where full materialisation is impractical.

### Pattern C: MedVFS HTTP Proxy (No FUSE)

```groovy
process READ_VIA_HTTP {
    script:
    """
    curl http://medvfs-svc:8090/proxy/train/patient001/wsi/slide.svs -o slide.svs
    """
}
```

Best for: Environments where FUSE is not available (e.g., serverless).

## VFS Configuration

This pipeline expects a `vfs.yaml` with:
- At least one image source (OMERO, Orthanc, or DICOM)
- Optional genomics source
- Optional EHR source (FHIR or OMOP)
- `dataset.splits` configured for train/val/test partitioning

See [`ml-pipeline.yaml`](../ml-pipeline.yaml) for a complete example.
