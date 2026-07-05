package config

// VFSConfig is the top-level structure parsed from vfs.yaml
type VFSConfig struct {
	Version  string          `yaml:"version"`
	Metadata MetadataConfig  `yaml:"metadata"`
	Storage  StorageConfig   `yaml:"storage"`
	Setup    SetupConfig     `yaml:"setup"`
	Serve    *ServeConfig    `yaml:"serve,omitempty"`
	Dataset  *DatasetConfig  `yaml:"dataset,omitempty"`
	Output   *OutputConfig   `yaml:"output,omitempty"`
	Sources  []SourceConfig  `yaml:"sources"`
	Mount    MountConfig     `yaml:"mount"`
}

type MetadataConfig struct {
	Name      string `yaml:"name"`
	Namespace string `yaml:"namespace,omitempty"`
	RunID     string `yaml:"run_id,omitempty"`
}

// StorageConfig defines where materialised files are stored.
// Type: emptydir | hostpath | pvc
type StorageConfig struct {
	Type      string `yaml:"type"`                 // emptydir | hostpath | pvc
	HostPath  string `yaml:"host_path,omitempty"`  // used when type=hostpath
	PVCName   string `yaml:"pvc_name,omitempty"`   // used when type=pvc
	SubPath   string `yaml:"subpath,omitempty"`    // optional sub-directory
	OutputDir string `yaml:"output_dir,omitempty"` // override resolved path (dev/test)
}

type SetupConfig struct {
	Parallelism int  `yaml:"parallelism"` // max concurrent adapters
	FailFast    bool `yaml:"fail_fast"`   // abort all on first error
}

// SourceConfig describes one data source to materialise.
type SourceConfig struct {
	ID          string             `yaml:"id"`
	Type        string             `yaml:"type"` // omop | fhir | omero | orthanc | genomics | dicom | files
	DSNSecret   string             `yaml:"dsn_secret,omitempty"`
	DSN         string             `yaml:"dsn,omitempty"`
	Query       string             `yaml:"query,omitempty"`        // OMOP: SQL query
	Endpoint    string             `yaml:"endpoint,omitempty"`     // FHIR/OMERO/Orthanc: base URL
	Auth        *AuthConfig        `yaml:"auth,omitempty"`
	FHIR        *FHIRQueryConfig   `yaml:"fhir,omitempty"`
	OMERO       *OMEROQueryConfig  `yaml:"omero,omitempty"`
	Orthanc     *OrthancQueryConfig `yaml:"orthanc,omitempty"`
	Genomics    *GenomicsConfig    `yaml:"genomics,omitempty"`
	DICOM       *DICOMConfig       `yaml:"dicom,omitempty"`
	Files       *FilesConfig       `yaml:"files,omitempty"`
	Materialize MaterializeConfig  `yaml:"materialize"`
}

// OMEROQueryConfig defines what to extract from an OMERO server.
// MedVFS connects via OMERO.web JSON API (no Ice dependency required).
type OMEROQueryConfig struct {
	// ProjectID restricts extraction to images within a specific OMERO project.
	// Mutually exclusive with DatasetID (DatasetID takes precedence if both set).
	ProjectID int `yaml:"project_id,omitempty"`

	// DatasetID restricts extraction to images within a specific OMERO dataset.
	DatasetID int `yaml:"dataset_id,omitempty"`

	// ImageFilter applies optional server-side filters on image listing.
	ImageFilter *OMEROImageFilter `yaml:"image_filter,omitempty"`

	// Extracts lists what to materialise per image.
	// Supported values: metadata_csv | thumbnail | tile_manifest
	// Default: metadata_csv
	Extracts []string `yaml:"extracts,omitempty"`

	// ThumbnailSize is the pixel dimension of exported thumbnails (default: 256).
	ThumbnailSize int `yaml:"thumbnail_size,omitempty"`

	// MetadataFields selects which OMERO annotation/key-value pairs to include
	// as extra columns in the metadata CSV. Empty = all key-value annotations.
	MetadataFields []string `yaml:"metadata_fields,omitempty"`

	// MaxImages caps the number of images to fetch (0 = no limit, useful for dev).
	MaxImages int `yaml:"max_images,omitempty"`
}

// OMEROImageFilter filters images by name prefix or tag.
type OMEROImageFilter struct {
	// NamePrefix filters images whose name starts with this string.
	NamePrefix string `yaml:"name_prefix,omitempty"`

	// Tag filters images that have the given OMERO tag annotation.
	Tag string `yaml:"tag,omitempty"`
}

// AuthConfig supports several auth strategies for HTTP-based sources.
type AuthConfig struct {
	Type         string `yaml:"type"`                    // none | bearer | smart_on_fhir | basic
	SecretRef    string `yaml:"secret_ref,omitempty"`    // k8s:// | env:// | file://
	Token        string `yaml:"token,omitempty"`         // dev only — literal bearer token
	ClientID     string `yaml:"client_id,omitempty"`     // SMART-on-FHIR client credentials
	ClientSecret string `yaml:"client_secret,omitempty"` // dev only — literal secret
	TokenURL     string `yaml:"token_url,omitempty"`     // SMART-on-FHIR token endpoint
	Scope        string `yaml:"scope,omitempty"`         // SMART-on-FHIR scopes
}

// FHIRQueryConfig defines a FHIR search query and how to flatten the result into CSV.
type FHIRQueryConfig struct {
	// Resource is the FHIR resource type (Patient, Observation, Condition, etc.)
	Resource string `yaml:"resource"`

	// Params are FHIR search parameters appended to the query string.
	// Example: { gender: female, birthdate: "ge1950" }
	Params map[string]string `yaml:"params,omitempty"`

	// PageSize controls the _count parameter (default: 100).
	PageSize int `yaml:"page_size,omitempty"`

	// Fields are dot-path expressions to extract from each FHIR resource JSON.
	// If empty, the full resource is serialised as a single JSON column.
	// Examples:
	//   - "id"
	//   - "name.0.family"
	//   - "name.0.given.0"
	//   - "birthDate"
	//   - "extension.http://hl7.org/fhir/us/core/StructureDefinition/us-core-race.valueString"
	Fields []string `yaml:"fields,omitempty"`
}

// MaterializeConfig defines the output file for a source.
type MaterializeConfig struct {
	Output      string `yaml:"output"`                // relative path under mount (e.g. patients.csv)
	Format      string `yaml:"format,omitempty"`       // csv (default) | dicom_dir | vcf | fastq
	DownloadDir string `yaml:"download_dir,omitempty"` // directory for downloaded files (type: omero/orthanc)
}

type MountConfig struct {
	Path     string `yaml:"path"`
	Readonly bool   `yaml:"readonly"`
}

// ─── Orthanc / DICOMweb ───────────────────────────────────────────────────────

// OrthancQueryConfig configures a DICOMweb source (Orthanc local OR hospital PACS).
// MedVFS uses QIDO-RS for study discovery and WADO-RS for pixel/metadata retrieval.
// The same adapter works for any DICOMweb-compliant endpoint:
//   - Orthanc (local RWE server)
//   - Hospital PACS with DICOMweb support
//   - Google Cloud Healthcare DICOM Store
type OrthancQueryConfig struct {
	// StudyFilter narrows which DICOM studies to materialise.
	StudyFilter *DicomStudyFilter `yaml:"study_filter,omitempty"`

	// Extracts lists what to produce per study.
	// Supported: metadata_csv | series_manifest | thumbnail
	// Default: metadata_csv
	Extracts []string `yaml:"extracts,omitempty"`

	// MaxStudies caps the number of studies to process (0 = no limit).
	MaxStudies int `yaml:"max_studies,omitempty"`

	// DICOMwebPath is the base path for DICOMweb endpoints on the server.
	// Orthanc default: "/dicom-web"  |  GCP Healthcare: auto-inferred from endpoint
	DICOMwebPath string `yaml:"dicomweb_path,omitempty"`
}

// DicomStudyFilter defines QIDO-RS search parameters.
type DicomStudyFilter struct {
	// Modality filters by DICOM modality tag (0008,0060).
	// Examples: CT | MR | PT | CR | DX | US | MG
	Modality string `yaml:"modality,omitempty"`

	// PatientID filters by DICOM PatientID tag (0010,0020). Supports wildcards (*).
	PatientID string `yaml:"patient_id,omitempty"`

	// StudyDateFrom and StudyDateTo define a date range in YYYYMMDD format.
	StudyDateFrom string `yaml:"study_date_from,omitempty"`
	StudyDateTo   string `yaml:"study_date_to,omitempty"`

	// StudyDescription is an optional free-text filter (DICOM tag 0008,1030).
	StudyDescription string `yaml:"study_description,omitempty"`
}

// ─── Genomics filesystem ──────────────────────────────────────────────────────

// GenomicsConfig configures the genomics filesystem adapter.
// Supports three storage backends selectable via the Storage field:
//   - nfs: local filesystem or NFS mount (uses os.Walk on BasePath)
//   - gcs: Google Cloud Storage bucket (uses GCS SDK)
//   - s3:  AWS S3 or S3-compatible endpoint like MinIO (uses AWS SDK v2)
//
// The adapter does NOT copy files — it scans the storage, validates indexes,
// and produces a manifest CSV with absolute paths or cloud URIs for downstream
// Nextflow pipelines and ML containers.
type GenomicsConfig struct {
	// Storage selects the backend: nfs | gcs | s3
	Storage string `yaml:"storage"`

	// --- NFS / local filesystem ---
	// BasePath is the root directory to scan (required when Storage=nfs).
	BasePath string `yaml:"base_path,omitempty"`

	// --- GCS ---
	// Bucket is the GCS bucket name (required when Storage=gcs).
	Bucket string `yaml:"bucket,omitempty"`
	// Prefix narrows the scan to objects under this prefix (optional).
	Prefix string `yaml:"prefix,omitempty"`

	// --- S3 / MinIO ---
	// Endpoint overrides the S3 endpoint URL for MinIO or other S3-compatible stores.
	// Leave empty for standard AWS S3.
	Endpoint string `yaml:"endpoint,omitempty"`
	// Region is required for AWS S3 (e.g. eu-west-1). Optional for MinIO.
	Region string `yaml:"region,omitempty"`

	// --- Common ---
	// Formats lists file types to include. Supported: bam | cram | vcf | fastq | bed | hdf5
	// Files are matched by extension (.bam, .vcf.gz, .fastq.gz, etc.)
	Formats []string `yaml:"formats"`

	// ValidateIndex checks that each BAM/CRAM has a .bai/.crai and each VCF.gz has a .tbi.
	// Fails the source if an index is missing.
	ValidateIndex bool `yaml:"validate_index,omitempty"`

	// PatientIDRegex is a Go regex with one capture group to extract patient_id
	// from the file name. Example: "^([A-Z0-9]+)_.*" extracts "P001" from "P001_tumor.bam"
	// If empty, the full filename stem is used as patient_id.
	PatientIDRegex string `yaml:"patient_id_regex,omitempty"`

	// MaxFiles caps the number of files per format (0 = no limit, useful for dev).
	MaxFiles int `yaml:"max_files,omitempty"`
}

// ─── Serve / FUSE ─────────────────────────────────────────────────────────────

// ServeConfig configures the persistent daemon mode (`medvfs serve`).
type ServeConfig struct {
	// Port is the HTTP server port for the manifest/events/stats/proxy API.
	Port int `yaml:"port,omitempty"` // default: 8090

	// FUSE configures the FUSE virtual filesystem layer.
	FUSE *FUSEConfig `yaml:"fuse,omitempty"`

	// HTTPProxyFallback, when true, exposes remote files via HTTP GET as a
	// fallback for environments where FUSE is not available.
	HTTPProxyFallback bool `yaml:"http_proxy_fallback,omitempty"`
}

// FUSEConfig controls the FUSE mount and block-based cache.
type FUSEConfig struct {
	// MountPoint is the local directory where the VFS will be mounted.
	// Example: "/data/vfs"
	MountPoint string `yaml:"mount_point"`

	// CacheDir is the local directory for the block cache.
	// Example: "/var/cache/medvfs"
	CacheDir string `yaml:"cache_dir"`

	// CacheMaxGB is the maximum disk usage for the block cache (default: 50).
	CacheMaxGB int `yaml:"cache_max_gb,omitempty"`

	// BlockSizeMB is the size of each cache block in megabytes (default: 4).
	BlockSizeMB int `yaml:"block_size_mb,omitempty"`

	// Prefetch configures the prefetch engine.
	Prefetch *PrefetchConfig `yaml:"prefetch,omitempty"`
}

// PrefetchConfig controls the multi-level prefetch engine.
type PrefetchConfig struct {
	// Enabled activates background prefetching (default: true).
	Enabled bool `yaml:"enabled"`

	// Strategy selects the prefetch approach: "sequential" | "format_aware" | "manifest"
	// Default: "format_aware"
	Strategy string `yaml:"strategy,omitempty"`

	// WarmupPercent is the percentage of manifest files to pre-warm at startup
	// (only when Strategy="manifest"). Default: 20.
	WarmupPercent int `yaml:"warmup_percent,omitempty"`

	// ConcurrentFetchers is the number of parallel download goroutines (default: 4).
	ConcurrentFetchers int `yaml:"concurrent_fetchers,omitempty"`

	// ReadAheadBlocks is the number of sequential blocks to prefetch after
	// each read (default: 3).
	ReadAheadBlocks int `yaml:"read_ahead_blocks,omitempty"`
}

// ─── DICOM local filesystem ──────────────────────────────────────────────────

// DICOMConfig configures the DICOM local filesystem adapter.
// This adapter scans a local directory tree for DICOM files, parses their
// tags, and produces an organised symlink tree and metadata CSV.
type DICOMConfig struct {
	// LocalWatchDir is the root directory to watch for DICOM files.
	// The adapter scans this recursively on startup and watches for new arrivals.
	LocalWatchDir string `yaml:"local_watch_dir"`

	// SymlinkRoot is the directory where the organised symlink tree is created.
	// Structure: {SymlinkRoot}/{PatientID}/{StudyDate}_{Modality}/{SeriesNumber}/{filename}.dcm
	SymlinkRoot string `yaml:"symlink_root,omitempty"`

	// Extracts lists what to produce per study.
	// Supported: metadata_csv | symlink_tree | fuse_tree
	// Default: metadata_csv
	Extracts []string `yaml:"extracts,omitempty"`
}

// ─── Static Files ─────────────────────────────────────────────────────────────

// FilesConfig configures the static file adapter for mounting arbitrary files
// (CSV, Parquet, JSON, YAML, model weights, etc.) into the VFS.
// Supports the same three backends as Genomics: nfs, gcs, s3.
type FilesConfig struct {
	// Storage selects the backend: nfs | gcs | s3
	Storage string `yaml:"storage"`

	// --- NFS / local filesystem ---
	BasePath string `yaml:"base_path,omitempty"`

	// --- GCS ---
	Bucket string `yaml:"bucket,omitempty"`
	Prefix string `yaml:"prefix,omitempty"`

	// --- S3 / MinIO ---
	Endpoint string `yaml:"endpoint,omitempty"`
	Region   string `yaml:"region,omitempty"`

	// --- Filtering ---
	// Patterns is a list of glob patterns to include.
	// Examples: ["*.csv", "*.parquet", "*.json", "*.pt", "*.h5"]
	// Empty means all files.
	Patterns []string `yaml:"patterns,omitempty"`

	// Exclude is a list of glob patterns to exclude.
	// Example: ["*.tmp", ".*"]
	Exclude []string `yaml:"exclude,omitempty"`

	// MaxFiles caps the number of files (0 = no limit).
	MaxFiles int `yaml:"max_files,omitempty"`
}

// ─── Dataset Configuration ────────────────────────────────────────────────────

// DatasetConfig provides ML-specific configuration for dataset organisation,
// splits, patient linking, and versioning metadata.
type DatasetConfig struct {
	// Splits defines how to partition data into train/val/test sets.
	Splits *SplitsConfig `yaml:"splits,omitempty"`

	// PatientLinker configures cross-source patient ID reconciliation.
	PatientLinker *PatientLinkerConfig `yaml:"patient_linker,omitempty"`

	// Versioning controls dataset manifest hashing for reproducibility.
	Versioning *VersioningConfig `yaml:"versioning,omitempty"`

	// DerivedViews defines cross-source CSV joins that combine data from
	// multiple materialised sources into new unified CSV files.
	DerivedViews []DerivedViewConfig `yaml:"derived_views,omitempty"`
}

// DerivedViewConfig defines a CSV that is produced by joining data from
// two or more materialised source CSVs on a common key column.
type DerivedViewConfig struct {
	// ID is a unique identifier for this derived view.
	ID string `yaml:"id"`

	// Sources lists the source CSVs to join and their key columns.
	// The first source is the "left" table; subsequent sources are joined onto it.
	Sources []ViewSourceRef `yaml:"sources"`

	// JoinType controls the join strategy: "left" (default), "inner", or "full".
	// - left:  keep all rows from the first source, NULL for missing right keys.
	// - inner: keep only rows where the key exists in ALL sources.
	// - full:  keep all rows from all sources (UNION of keys).
	JoinType string `yaml:"join_type,omitempty"`

	// Columns selects and optionally renames columns in the output CSV.
	// Format: "source_id.column_name" or "source_id.column_name AS alias"
	// If empty, all columns from all sources are included (prefixed with source_id).
	Columns []string `yaml:"columns,omitempty"`

	// Output is the relative path for the generated CSV file.
	Output string `yaml:"output"`
}

// ViewSourceRef references a materialised source CSV and specifies the join key.
type ViewSourceRef struct {
	// Ref is the source ID as defined in sources[].id.
	Ref string `yaml:"ref"`

	// Key is the column name in this source's CSV to use for joining.
	Key string `yaml:"key"`
}

// SplitsConfig defines the dataset split strategy.
type SplitsConfig struct {
	// Source is a URI pointing to a CSV with columns: patient_id,split
	// where split is one of: train, val, test
	// Supports file://, gs://, s3:// schemes.
	Source string `yaml:"source"`

	// PatientIDColumn is the CSV column name for patient ID (default: "patient_id").
	PatientIDColumn string `yaml:"patient_id_column,omitempty"`

	// SplitColumn is the CSV column name for split assignment (default: "split").
	SplitColumn string `yaml:"split_column,omitempty"`
}

// PatientLinkerConfig defines how to reconcile patient IDs across sources.
type PatientLinkerConfig struct {
	// Source is a URI pointing to a CSV mapping canonical IDs to source-specific IDs.
	// Columns: canonical_id, <source_id_1>, <source_id_2>, ...
	Source string `yaml:"source"`

	// Layout controls the VFS directory structure:
	//   unified   → /data/vfs/{split}/{canonical_id}/{source_type}/files
	//   by_source → /data/vfs/{split}/{source_id}/{patient_id}/files (default)
	Layout string `yaml:"layout,omitempty"`
}

// VersioningConfig controls dataset versioning and manifest generation.
type VersioningConfig struct {
	// Enabled activates manifest hash generation (default: true when block present).
	Enabled bool `yaml:"enabled"`

	// DatasetID is a human-readable identifier for the dataset.
	DatasetID string `yaml:"dataset_id,omitempty"`
}

// ─── Output / Write Layer ─────────────────────────────────────────────────────

// OutputConfig configures the writable output directory and optional sync.
type OutputConfig struct {
	// LocalDir is the writable directory for pipeline outputs.
	// Example: "/data/output"
	LocalDir string `yaml:"local_dir"`

	// Sync configures periodic synchronisation to cloud storage.
	Sync *SyncConfig `yaml:"sync,omitempty"`
}

// SyncConfig defines how outputs are synced to cloud storage.
type SyncConfig struct {
	// Enabled activates periodic sync (default: false).
	Enabled bool `yaml:"enabled"`

	// Target is the cloud URI to sync to.
	// Supports gs:// and s3:// schemes.
	// Template variables: {{ .RunID }}, {{ .Timestamp }}
	Target string `yaml:"target"`

	// IntervalSeconds is the sync interval (default: 60).
	IntervalSeconds int `yaml:"interval_seconds,omitempty"`

	// OnExit ensures a final sync when the process exits.
	OnExit bool `yaml:"on_exit,omitempty"`
}
