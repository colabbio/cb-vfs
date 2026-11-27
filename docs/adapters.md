# MedVFS — Adapters Reference

> **Source:** `internal/adapters/`

---

## Adapter Architecture

Every data source in MedVFS is accessed through an **adapter**. Adapters implement a common interface for materialising heterogeneous medical data into local files.

### Core Interface

```go
type Adapter interface {
    ID() string
    Validate(ctx context.Context) error
    Fetch(ctx context.Context, outDir string, progress chan<- Event) error
}
```

| Method | Purpose |
|---|---|
| `ID()` | Returns the source ID from `vfs.yaml` |
| `Validate()` | Tests connectivity and credentials without fetching data |
| `Fetch()` | Materialises data into `outDir`, emitting progress events |

### Extended Interfaces

#### FileProvider — for FUSE / Serve mode

```go
type FileProvider interface {
    BuildFileTree(ctx context.Context, outDir string, strategy string) ([]FileMapping, error)
}
```

`BuildFileTree` produces a set of `FileMapping` entries that describe how remote files map to local virtual paths. The `strategy` parameter selects the materialisation approach:

| Strategy | Behaviour |
|---|---|
| `download` | Download full file to `outDir/{LogicalPath}` |
| `symlink_tree` | Create symlinks from logical paths to existing local files |
| `fuse_tree` | Return mappings only (no disk I/O) — used by FUSE mount |

#### StreamingAdapter — for live monitoring

```go
type StreamingAdapter interface {
    Adapter
    StartListening(ctx context.Context, events chan<- LiveEvent) error
}
```

Watches the data source for real-time changes (new files, modifications) and emits `LiveEvent` entries. Used by the DICOM local adapter for `fsnotify`-based file watching.

### FileMapping Structure

```go
type FileMapping struct {
    LogicalPath string   // "PT001/20240115_CT/series1/img001.dcm"
    LocalPath   string   // "/data/organised/PT001/..." (empty for FUSE)
    RemoteRef   string   // "wado-rs://..." or "https://..." or "/local/path"
    SizeBytes   int64    // file size from remote metadata
    ContentType string   // "application/dicom"
}
```

---

## Adapter Reference

### 1. OMOP — Clinical Records

> **Source:** [omop/omop_adapter.go](../internal/adapters/omop/omop_adapter.go)

Connects to an OMOP CDM PostgreSQL database and executes SQL queries. Results are materialised as CSV files.

#### YAML Configuration

```yaml
sources:
  - id: clinical_records
    type: omop
    dsn: "postgres://user:pass@host:5432/omop_cdm"   # direct DSN
    dsn_secret: k8s://namespace/secret-name            # or K8s secret
    query: |
      SELECT person_id, condition_concept_id, condition_start_date
      FROM condition_occurrence
      WHERE condition_concept_id = 201826
    materialize:
      output: clinical/conditions.csv
```

| Field | Required | Description |
|---|---|---|
| `dsn` | yes* | PostgreSQL connection string |
| `dsn_secret` | yes* | Alternative: secret reference (`k8s://`, `env://`, `file://`) |
| `query` | yes | SQL query executed against the OMOP CDM |

*One of `dsn` or `dsn_secret` is required.

#### Capabilities

| Feature | Supported |
|---|---|
| `Adapter` (setup mode) | ✅ |
| `FileProvider` (serve mode) | ❌ (CSV result, no file tree) |
| `StreamingAdapter` (live) | ❌ |

---

### 2. FHIR — Demographics & Observations

> **Source:** [fhir/fhir_adapter.go](../internal/adapters/fhir/fhir_adapter.go)

Queries a FHIR R4 REST API, paginates through Bundles, and flattens resources into CSV using dot-path field extraction.

#### YAML Configuration

```yaml
sources:
  - id: demographics
    type: fhir
    endpoint: https://fhir.hospital.org/r4
    auth:
      type: smart_on_fhir              # none | bearer | smart_on_fhir | basic
      client_id: medvfs-client
      client_secret: env://FHIR_SECRET
      token_url: https://auth.hospital.org/token
      scope: "system/Patient.read"
    fhir:
      resource: Patient                 # FHIR resource type
      params:                           # Search parameters
        gender: female
        birthdate: "ge1960"
      page_size: 200                    # _count parameter
      fields:                           # Dot-path extraction
        - id
        - name.0.family
        - name.0.given.0
        - birthDate
        - address.0.city
    materialize:
      output: demographics/patients.csv
```

| Field | Required | Default | Description |
|---|---|---|---|
| `fhir.resource` | yes | — | FHIR resource type (Patient, Observation, etc.) |
| `fhir.params` | no | — | Key-value search params |
| `fhir.page_size` | no | 100 | Results per page |
| `fhir.fields` | no | full JSON | Dot-path field selectors |

#### Auth Types

| Type | Fields Needed |
|---|---|
| `none` | — |
| `bearer` | `secret_ref` or `token` |
| `basic` | `secret_ref` (format: `user:password`) |
| `smart_on_fhir` | `client_id`, `client_secret`, `token_url`, `scope` |

#### Capabilities

| Feature | Supported |
|---|---|
| `Adapter` (setup mode) | ✅ |
| `FileProvider` (serve mode) | ❌ |
| `StreamingAdapter` (live) | ❌ |

---

### 3. OMERO — Pathology WSI

> **Source:** [omero/adapter.go](../internal/adapters/omero/adapter.go)

Connects to an OMERO.web JSON API to access Whole Slide Images. Supports metadata extraction, thumbnail download, and FUSE virtualisation.

#### YAML Configuration

```yaml
sources:
  - id: wsi_slides
    type: omero
    endpoint: https://omero.hospital.org
    auth:
      type: bearer
      secret_ref: env://OMERO_TOKEN
    omero:
      project_id: 42                    # filter by OMERO project
      dataset_id: 10                    # or filter by dataset (overrides project_id)
      image_filter:
        name_prefix: "breast_"          # name starts with
        tag: "cancer"                   # OMERO tag annotation
      extracts:
        - metadata_csv                  # image metadata CSV
        - thumbnail                     # rendered thumbnails
        - tile_manifest                 # tile coordinate manifest
      thumbnail_size: 512               # thumbnail pixel dimension
      metadata_fields:                  # which KV annotations to include
        - diagnosis
        - stain_type
      max_images: 200                   # limit for dev
    materialize:
      output: pathology/metadata.csv
      download_dir: pathology/thumbnails
```

#### BuildFileTree Strategies

| Strategy | Behaviour |
|---|---|
| `download` | Downloads each image via OMERO.web render endpoint |
| `fuse_tree` | Returns `FileMapping` with `RemoteRef = "omero://image/{id}"` for FUSE mount |
| `symlink_tree` | Creates symlinks from logical paths to OMERO managed repository |

#### Capabilities

| Feature | Supported |
|---|---|
| `Adapter` (setup mode) | ✅ |
| `FileProvider` (serve mode) | ✅ |
| `StreamingAdapter` (live) | ❌ |

---

### 4. Orthanc — Radiology DICOMweb

> **Source:** [orthanc/adapter.go](../internal/adapters/orthanc/adapter.go)

Queries a DICOMweb-compliant endpoint (Orthanc, hospital PACS, or GCP Healthcare DICOM Store) using QIDO-RS for study discovery and WADO-RS for data retrieval.

#### YAML Configuration

```yaml
sources:
  - id: radiology
    type: orthanc
    endpoint: https://pacs.hospital.org
    auth:
      type: basic
      secret_ref: k8s://colabbio/pacs-credentials
    orthanc:
      dicomweb_path: /dicom-web        # base DICOMweb path (default for Orthanc)
      study_filter:
        modality: CT                    # DICOM modality tag (0008,0060)
        patient_id: "P001*"             # supports wildcards
        study_date_from: "20240101"     # YYYYMMDD
        study_date_to: "20241231"
        study_description: "chest"      # free-text filter
      extracts:
        - metadata_csv                  # study/series metadata
        - series_manifest               # per-series file listing
        - thumbnail                     # rendered thumbnails
      max_studies: 500
    materialize:
      output: radiology/studies.csv
      download_dir: radiology/dicom
```

#### DICOMweb Endpoints Used

| Operation | Endpoint |
|---|---|
| Study discovery | `QIDO-RS: GET {dicomweb_path}/studies?{filters}` |
| Series listing | `QIDO-RS: GET {dicomweb_path}/studies/{uid}/series` |
| Instance retrieval | `WADO-RS: GET {dicomweb_path}/studies/{uid}/series/{uid}/instances/{uid}` |
| Metadata | `GET {dicomweb_path}/studies/{uid}/metadata` |

#### BuildFileTree Strategies

| Strategy | Behaviour |
|---|---|
| `download` | Downloads DICOM instances via WADO-RS |
| `fuse_tree` | Returns `FileMapping` with `RemoteRef = "wado-rs://..."` |
| `symlink_tree` | Creates symlinks (requires local Orthanc storage) |

#### Capabilities

| Feature | Supported |
|---|---|
| `Adapter` (setup mode) | ✅ |
| `FileProvider` (serve mode) | ✅ |
| `StreamingAdapter` (live) | ❌ |

---

### 5. DICOM Local — Filesystem Scanner

> **Source:** [dicom/adapter.go](../internal/adapters/dicom/adapter.go)

Scans a local directory for DICOM files, parses minimal DICOM tags (PatientID, StudyDate, Modality, SeriesNumber), and organises them into a structured tree.

#### YAML Configuration

```yaml
sources:
  - id: incoming_dicom
    type: dicom
    dicom:
      local_watch_dir: /data/incoming/dicom      # root scan directory
      symlink_root: /data/organised/dicom         # organised output
      extracts:
        - metadata_csv       # patient/study/series CSV
        - symlink_tree       # organised symlinks
        - fuse_tree          # FileMapping for FUSE
    materialize:
      output: dicom/metadata.csv
```

#### Organised Tree Structure

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

#### DICOM Tags Parsed

| Tag | Name | Usage |
|---|---|---|
| `(0010,0020)` | PatientID | Top-level directory |
| `(0008,0020)` | StudyDate | Second-level directory (YYYYMMDD) |
| `(0008,0060)` | Modality | Appended to study date dir |
| `(0020,0011)` | SeriesNumber | Third-level directory |

#### Capabilities

| Feature | Supported |
|---|---|
| `Adapter` (setup mode) | ✅ |
| `FileProvider` (serve mode) | ✅ |
| `StreamingAdapter` (live) | ✅ (fsnotify watcher) |

---

### 6. Genomics — BAM/VCF/FASTQ

> **Source:** [genomics/adapter.go](../internal/adapters/genomics/adapter.go)

Scans genomics repositories (NFS, GCS, or S3/MinIO) for alignment and variant files. Produces a validated manifest CSV — does **not** copy files.

#### YAML Configuration

```yaml
sources:
  - id: alignments
    type: genomics
    genomics:
      storage: gcs                      # nfs | gcs | s3
      # NFS:
      base_path: /mnt/nfs/genomics      # (storage=nfs)
      # GCS:
      bucket: hospital-genomics          # (storage=gcs)
      prefix: oncology/breast/
      # S3:
      endpoint: https://minio:9000       # (storage=s3, optional for AWS)
      region: eu-west-1
      # Common:
      formats: [bam, cram, vcf, fastq]   # file types to scan
      validate_index: true                # check .bai/.crai/.tbi
      patient_id_regex: "^([A-Z0-9]+)_.*" # capture group → patient_id
      max_files: 0                        # 0 = no limit
    materialize:
      output: genomics/manifest.csv
```

#### Storage Backends

| Backend | Scan Method | URI Format in Manifest |
|---|---|---|
| `nfs` | `os.Walk(base_path)` | `/mnt/nfs/genomics/P001_tumor.bam` |
| `gcs` | GCS SDK `bucket.Objects(prefix)` | `gs://bucket/prefix/P001_tumor.bam` |
| `s3` | AWS SDK v2 `ListObjectsV2` | `s3://bucket/prefix/P001_tumor.bam` |

#### Index Validation

When `validate_index: true`:

| File Type | Expected Index |
|---|---|
| `.bam` | `.bam.bai` or `.bai` |
| `.cram` | `.cram.crai` or `.crai` |
| `.vcf.gz` | `.vcf.gz.tbi` |

Missing indexes cause the source to fail (unless `fail_fast: false`).

#### Manifest CSV Output

```csv
patient_id,file_type,file_name,file_path,size_bytes,index_path
P001,bam,P001_tumor.bam,gs://bucket/P001_tumor.bam,4294967296,gs://bucket/P001_tumor.bam.bai
P001,vcf,P001_variants.vcf.gz,gs://bucket/P001_variants.vcf.gz,10485760,gs://bucket/P001_variants.vcf.gz.tbi
```

#### Capabilities

| Feature | Supported |
|---|---|
| `Adapter` (setup mode) | ✅ |
| `FileProvider` (serve mode) | ❌ (produces manifest only) |
| `StreamingAdapter` (live) | ❌ |

---

## Adding a New Adapter

1. Create `internal/adapters/{name}/adapter.go`
2. Implement the `Adapter` interface (minimum: `ID`, `Validate`, `Fetch`)
3. Optionally implement `FileProvider` for serve mode
4. Optionally implement `StreamingAdapter` for live monitoring
5. Register in the factory: `cmd/medvfs/main.go` → `buildAdapterFactory()`
6. Add config struct to `internal/config/schema.go`
