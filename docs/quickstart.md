# MedVFS — Quick Start Guide

This guide walks you through building, configuring, and running MedVFS in both batch and streaming modes.

---

## Prerequisites

- **Go 1.22+** (build from source)
- **Docker** (optional — for containerised deployment)
- **FUSE** (for `serve` mode): `apt install fuse3` / `yum install fuse3`
- **libfuse-dev** (build dependency): `apt install libfuse3-dev`

---

## 1. Build

```bash
cd cb-vfs
make build
./medvfs version
# medvfs v0.1.0 (colabbio)
```

---

## 2. Write a `vfs.yaml`

MedVFS is entirely driven by a single YAML configuration file. Below are progressively complex examples.

### Example 1: OMOP Clinical Data (simplest)

```yaml
version: "1"
metadata:
  name: diabetes-cohort

storage:
  output_dir: ./output     # local directory (dev/test)

setup:
  parallelism: 2
  fail_fast: true

sources:
  - id: conditions
    type: omop
    dsn: "postgres://omop_user:omop_pass@localhost:5432/omop_cdm"
    query: |
      SELECT person_id, condition_concept_id, condition_start_date
      FROM condition_occurrence
      WHERE condition_concept_id = 201826
    materialize:
      output: conditions.csv

  - id: patients
    type: omop
    dsn: "postgres://omop_user:omop_pass@localhost:5432/omop_cdm"
    query: |
      SELECT person_id, gender_concept_id, year_of_birth
      FROM person
      WHERE person_id IN (
        SELECT person_id FROM condition_occurrence
        WHERE condition_concept_id = 201826
      )
    materialize:
      output: patients.csv

mount:
  path: /data/vfs
  readonly: true
```

Run it:
```bash
./medvfs setup --config vfs.yaml
# ✅ setup complete: 2 sources done
ls output/
# conditions.csv  patients.csv
```

### Example 2: OMERO Pathology Images

```yaml
version: "1"
metadata:
  name: pathology-slides

storage:
  output_dir: ./output

setup:
  parallelism: 4

sources:
  - id: wsi_slides
    type: omero
    endpoint: https://omero.hospital.org
    auth:
      type: bearer
      secret_ref: env://OMERO_TOKEN
    omero:
      project_id: 42
      extracts: [metadata_csv, thumbnail]
      thumbnail_size: 512
      max_images: 100
    materialize:
      output: slides/metadata.csv
      download_dir: slides/thumbnails

mount:
  path: /data/vfs
  readonly: true
```

### Example 3: Multi-Modal Pipeline (OMOP + FHIR + OMERO + Genomics)

```yaml
version: "1"
metadata:
  name: multimodal-rwe-study
  namespace: oncology
  run_id: run-2025-001

storage:
  type: pvc
  pvc_name: medvfs-data
  subpath: run-2025-001

setup:
  parallelism: 8
  fail_fast: false

sources:
  # Clinical records from OMOP CDM
  - id: clinical_records
    type: omop
    dsn_secret: k8s://colabbio/omop-dsn
    query: |
      SELECT p.person_id, p.year_of_birth, p.gender_concept_id,
             c.condition_concept_id, c.condition_start_date
      FROM person p
      JOIN condition_occurrence c ON p.person_id = c.person_id
      WHERE c.condition_concept_id IN (4112853, 436665)
    materialize:
      output: clinical/patients.csv

  # FHIR patient demographics
  - id: demographics
    type: fhir
    endpoint: https://fhir.hospital.org/r4
    auth:
      type: smart_on_fhir
      client_id: medvfs-client
      client_secret: env://FHIR_CLIENT_SECRET
      token_url: https://auth.hospital.org/token
      scope: "system/Patient.read"
    fhir:
      resource: Patient
      params:
        gender: female
        birthdate: "ge1960"
      page_size: 200
      fields:
        - id
        - name.0.family
        - name.0.given.0
        - birthDate
        - address.0.city
    materialize:
      output: demographics/patients.csv

  # Pathology WSI from OMERO
  - id: wsi_slides
    type: omero
    endpoint: https://omero.hospital.org
    auth:
      type: bearer
      secret_ref: k8s://colabbio/omero-token
    omero:
      project_id: 42
      image_filter:
        tag: "breast_cancer"
      extracts: [metadata_csv, thumbnail]
      thumbnail_size: 512
    materialize:
      output: pathology/metadata.csv
      download_dir: pathology/thumbnails

  # Radiology from hospital PACS (DICOMweb)
  - id: radiology
    type: orthanc
    endpoint: https://pacs.hospital.org
    auth:
      type: basic
      secret_ref: k8s://colabbio/pacs-credentials
    orthanc:
      study_filter:
        modality: CT
        study_date_from: "20240101"
        study_date_to: "20241231"
      extracts: [metadata_csv, series_manifest]
      max_studies: 500
    materialize:
      output: radiology/studies.csv
      download_dir: radiology/dicom

  # Genomics from GCS
  - id: genomics
    type: genomics
    genomics:
      storage: gcs
      bucket: hospital-genomics-prod
      prefix: oncology/breast/
      formats: [bam, vcf]
      validate_index: true
      patient_id_regex: "^([A-Z0-9]+)_.*"
    materialize:
      output: genomics/manifest.csv

mount:
  path: /data/vfs
  readonly: true
```

---

## 3. Run `medvfs setup` (Batch Mode)

```bash
# Validate first
./medvfs validate --config vfs.yaml

# Materialise all data
./medvfs setup --config vfs.yaml

# Results are written to the storage output directory
tree output/
# output/
# ├── clinical/
# │   └── patients.csv
# ├── demographics/
# │   └── patients.csv
# ├── pathology/
# │   ├── metadata.csv
# │   └── thumbnails/
# │       ├── img_001.jpg
# │       └── img_002.jpg
# ├── radiology/
# │   ├── studies.csv
# │   └── dicom/
# └── genomics/
#     └── manifest.csv
```

---

## 4. Run `medvfs serve` (FUSE Daemon)

The `serve` command mounts a FUSE filesystem backed by remote sources. Files appear as local but are fetched on-demand with block caching and intelligent prefetching.

### Add `serve:` and `fuse:` sections to your config

```yaml
# ... sources as above ...

serve:
  port: 8090
  fuse:
    mount_point: /data/vfs
    cache_dir: /var/cache/medvfs
    cache_max_gb: 50
    block_size_mb: 4
    prefetch:
      enabled: true
      strategy: format_aware    # sequential | format_aware | manifest
      warmup_percent: 20
      concurrent_fetchers: 4
      read_ahead_blocks: 3
```

### Start the daemon

```bash
# Requires FUSE support (CAP_SYS_ADMIN or --privileged)
sudo ./medvfs serve --config vfs.yaml

# The mount point is now accessible:
ls /data/vfs/
# slides/  radiology/  genomics/

# Reading a file triggers on-demand fetch:
head -1 /data/vfs/slides/metadata.csv
# patient_id,image_name,size_x,size_y,...
```

### Query the HTTP API

```bash
# List all virtualised files
curl http://localhost:8090/manifest | jq .

# Cache statistics
curl http://localhost:8090/stats
# {"hits": 142, "misses": 18, "hit_rate": 0.887, "total_blocks": 160}

# Live events (SSE)
curl -N http://localhost:8090/events
# data: {"type":"file_added","source":"dicom_local","path":"P001/CT/series1/img001.dcm"}
```

---

## 5. Docker Usage

### Run setup in Docker

```bash
docker run --rm \
  -v $(pwd)/vfs.yaml:/etc/medvfs/vfs.yaml \
  -v $(pwd)/output:/output \
  -e OMOP_DSN="postgres://user:pass@host:5432/omop" \
  colabbio/medvfs:latest \
  setup --config /etc/medvfs/vfs.yaml
```

### Run serve in Docker (requires `--privileged` for FUSE)

```bash
docker run --rm \
  --privileged \
  -v $(pwd)/vfs.yaml:/etc/medvfs/vfs.yaml \
  -p 8090:8090 \
  colabbio/medvfs:latest \
  serve --config /etc/medvfs/vfs.yaml
```

---

## 6. Nextflow Integration

MedVFS pairs naturally with Nextflow for orchestrating multi-stage clinical ML pipelines:

```groovy
// nextflow.config
process {
    container = 'colabbio/medvfs:latest'
}

// main.nf
process SETUP_DATA {
    output:
    path "output/*", emit: data_dir

    script:
    """
    medvfs setup --config /pipeline/vfs.yaml
    """
}

process TRAIN_MODEL {
    input:
    path data_dir

    script:
    """
    python train.py --data-dir ${data_dir}
    """
}

workflow {
    SETUP_DATA()
    TRAIN_MODEL(SETUP_DATA.out.data_dir)
}
```

---

## Next Steps

- **[Kubernetes Deployment](kubernetes.md)** — DaemonSet, Sidecar, and CSI modes
- **[Adapters Reference](adapters.md)** — Per-adapter configuration
- **[Serve Mode](serve-mode.md)** — FUSE internals, BlockManager, prefetch engine
