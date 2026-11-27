// Package genomics implements a MedVFS adapter for genomics filesystem sources.
//
// Supported storage backends (selected via genomics.storage in vfs.yaml):
//   - nfs  : local filesystem or NFS mount — uses os.Walk on base_path
//   - gcs  : Google Cloud Storage bucket — uses GCS SDK (GOOGLE_APPLICATION_CREDENTIALS)
//   - s3   : AWS S3 or MinIO/S3-compatible — uses AWS SDK v2
//
// The adapter does NOT copy files. It scans the storage, validates indexes,
// extracts patient_id from filenames, and writes a genomics_manifest.csv
// with absolute paths or cloud URIs ready for Nextflow pipelines.
//
// Supported formats:  bam | cram | vcf | fastq | bed | hdf5
// Required indexes:   .bam→.bai, .cram→.crai, .vcf.gz→.tbi  (when validate_index=true)
package genomics

import (
	"context"
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/colabbio/medvfs/internal/adapters"
	"github.com/colabbio/medvfs/internal/config"
	"go.uber.org/zap"
)

// fileEntry represents one genomic file found during scanning.
type fileEntry struct {
	PatientID string
	Format    string // bam | cram | vcf | fastq | bed | hdf5
	Path      string // absolute local path or cloud URI (gs:// or s3://)
	IndexPath string // empty if not applicable or not found
	SizeBytes int64
}

// Adapter scans genomics storage and produces a manifest CSV.
type Adapter struct {
	cfg   config.SourceConfig
	log   *zap.Logger
	pidRe *regexp.Regexp // compiled patient_id_regex, or nil
}

// New creates a new Genomics adapter.
func New(cfg config.SourceConfig, log *zap.Logger) (*Adapter, error) {
	a := &Adapter{cfg: cfg, log: log}
	if g := cfg.Genomics; g != nil && g.PatientIDRegex != "" {
		re, err := regexp.Compile(g.PatientIDRegex)
		if err != nil {
			return nil, fmt.Errorf("genomics adapter %q: invalid patient_id_regex: %w", cfg.ID, err)
		}
		a.pidRe = re
	}
	return a, nil
}

// ID implements adapters.Adapter.
func (a *Adapter) ID() string { return a.cfg.ID }

// Validate checks that required config fields are present.
func (a *Adapter) Validate(_ context.Context) error {
	g := a.cfg.Genomics
	if g == nil {
		return fmt.Errorf("genomics adapter %q: genomics: block is required", a.cfg.ID)
	}
	switch g.Storage {
	case "nfs":
		if g.BasePath == "" {
			return fmt.Errorf("genomics adapter %q: base_path is required for storage=nfs", a.cfg.ID)
		}
	case "gcs":
		if g.Bucket == "" {
			return fmt.Errorf("genomics adapter %q: bucket is required for storage=gcs", a.cfg.ID)
		}
	case "s3":
		if g.Bucket == "" {
			return fmt.Errorf("genomics adapter %q: bucket is required for storage=s3", a.cfg.ID)
		}
	default:
		return fmt.Errorf("genomics adapter %q: unknown storage %q (nfs|gcs|s3)", a.cfg.ID, g.Storage)
	}
	if len(g.Formats) == 0 {
		return fmt.Errorf("genomics adapter %q: at least one format is required", a.cfg.ID)
	}
	return nil
}

// Fetch scans the storage and writes the genomics manifest CSV.
func (a *Adapter) Fetch(ctx context.Context, outputDir string, progress chan<- adapters.Event) error {
	progress <- adapters.Event{SourceID: a.cfg.ID, Status: "start", Message: "scanning genomics storage"}

	var (
		entries []fileEntry
		err     error
	)
	switch a.cfg.Genomics.Storage {
	case "nfs":
		entries, err = a.scanNFS(ctx)
	case "gcs":
		entries, err = a.scanGCS(ctx)
	case "s3":
		entries, err = a.scanS3(ctx)
	}
	if err != nil {
		return fmt.Errorf("genomics adapter %q: scan: %w", a.cfg.ID, err)
	}

	a.log.Info("Genomics files found", zap.String("source", a.cfg.ID), zap.Int("count", len(entries)))

	if err := a.writeManifest(entries, outputDir); err != nil {
		return fmt.Errorf("genomics adapter %q: write manifest: %w", a.cfg.ID, err)
	}
	progress <- adapters.Event{SourceID: a.cfg.ID, Status: "done", Message: "genomics manifest written", Rows: len(entries)}
	return nil
}

// ─── FileProvider ────────────────────────────────────────────────────────────

// BuildFileTree implements adapters.FileProvider.
// Scans genomics storage and returns FileMappings for FUSE virtualisation.
// Strategy:
//   - "download"      → returns mappings with LocalPath set (NFS only, files already local)
//   - "symlink_tree"  → creates symlinks from organised tree to original files (NFS only)
//   - "fuse_tree"     → returns mappings with RemoteRef for on-demand block fetching
//
// For GCS/S3 backends, only "fuse_tree" is meaningful (no local files to symlink).
// Index files (.bai, .crai, .tbi) are registered as separate virtual nodes so that
// tools like samtools, htslib, and bcftools can locate them via standard naming.
func (a *Adapter) BuildFileTree(ctx context.Context, outDir string, strategy string) ([]adapters.FileMapping, error) {
	g := a.cfg.Genomics

	// Scan the storage backend (reuses existing scan logic)
	var (
		entries []fileEntry
		err     error
	)
	switch g.Storage {
	case "nfs":
		entries, err = a.scanNFS(ctx)
	case "gcs":
		entries, err = a.scanGCS(ctx)
	case "s3":
		entries, err = a.scanS3(ctx)
	default:
		return nil, fmt.Errorf("genomics adapter %q: unknown storage %q", a.cfg.ID, g.Storage)
	}
	if err != nil {
		return nil, fmt.Errorf("genomics adapter %q: scan: %w", a.cfg.ID, err)
	}

	mappings := make([]adapters.FileMapping, 0, len(entries)*2) // room for data + index files
	for _, e := range entries {
		// Build organised logical path: {PatientID}/{filename}
		baseName := filepath.Base(e.Path)
		logicalPath := fmt.Sprintf("%s/%s", e.PatientID, baseName)

		fm := adapters.FileMapping{
			LogicalPath: logicalPath,
			SizeBytes:   e.SizeBytes,
			ContentType: formatToMIME(e.Format),
		}

		switch strategy {
		case "fuse_tree":
			fm.RemoteRef = pathToRemoteRef(e.Path, g.Storage)

		case "download", "symlink_tree":
			if g.Storage != "nfs" {
				// Cloud files have no local path — fall back to fuse_tree semantics
				fm.RemoteRef = pathToRemoteRef(e.Path, g.Storage)
			} else {
				fm.LocalPath = e.Path
				fm.RemoteRef = "file://" + e.Path

				if strategy == "symlink_tree" && outDir != "" {
					symlinkPath := filepath.Join(outDir, logicalPath)
					if mkErr := os.MkdirAll(filepath.Dir(symlinkPath), 0o755); mkErr != nil {
						return nil, mkErr
					}
					relPath, relErr := filepath.Rel(filepath.Dir(symlinkPath), e.Path)
					if relErr != nil {
						relPath = e.Path
					}
					_ = os.Remove(symlinkPath)
					if lnErr := os.Symlink(relPath, symlinkPath); lnErr != nil {
						a.log.Warn("symlink failed", zap.String("path", symlinkPath), zap.Error(lnErr))
					}
					fm.LocalPath = symlinkPath
				}
			}

		default:
			return nil, fmt.Errorf("genomics adapter %q: unknown strategy %q", a.cfg.ID, strategy)
		}

		mappings = append(mappings, fm)

		// Register index file as a separate virtual node.
		// samtools expects P001/tumor.bam.bai next to P001/tumor.bam.
		if e.IndexPath != "" {
			idxBaseName := filepath.Base(e.IndexPath)
			idxLogical := fmt.Sprintf("%s/%s", e.PatientID, idxBaseName)

			idxFm := adapters.FileMapping{
				LogicalPath: idxLogical,
				ContentType: "application/octet-stream",
			}

			switch strategy {
			case "fuse_tree":
				idxFm.RemoteRef = pathToRemoteRef(e.IndexPath, g.Storage)
			case "download", "symlink_tree":
				if g.Storage == "nfs" {
					idxFm.LocalPath = e.IndexPath
					idxFm.RemoteRef = "file://" + e.IndexPath
				} else {
					idxFm.RemoteRef = pathToRemoteRef(e.IndexPath, g.Storage)
				}
			}

			// Get index file size for NFS (we have stat access)
			if g.Storage == "nfs" {
				if info, statErr := os.Stat(e.IndexPath); statErr == nil {
					idxFm.SizeBytes = info.Size()
				}
			}
			// For GCS/S3, index size was not captured during scan;
			// FUSE will query FileSize on first Getattr.

			mappings = append(mappings, idxFm)
		}
	}

	a.log.Info("Genomics file tree built",
		zap.String("strategy", strategy),
		zap.String("storage", g.Storage),
		zap.Int("files", len(mappings)),
	)
	return mappings, nil
}

// pathToRemoteRef converts a file path or cloud URI to the scheme-qualified
// RemoteRef expected by the DispatchFetcher:
//
//	nfs path "/mnt/data/P001.bam" → "file:///mnt/data/P001.bam"
//	gcs URI  "gs://bucket/P001.bam" → "gs://bucket/P001.bam" (already has scheme)
//	s3  URI  "s3://bucket/P001.bam" → "s3://bucket/P001.bam" (already has scheme)
func pathToRemoteRef(path, storage string) string {
	switch storage {
	case "nfs":
		return "file://" + path
	case "gcs", "s3":
		// GCS and S3 scan results already contain scheme-qualified URIs
		return path
	default:
		return "file://" + path
	}
}

// formatToMIME maps genomics format names to MIME types.
func formatToMIME(format string) string {
	switch format {
	case "bam":
		return "application/vnd.bam"
	case "cram":
		return "application/vnd.cram"
	case "vcf":
		return "text/x-vnd.vcf"
	case "fastq":
		return "application/x-fastq"
	case "bed":
		return "text/x-bed"
	case "hdf5":
		return "application/x-hdf5"
	default:
		return "application/octet-stream"
	}
}

// ─── NFS backend ─────────────────────────────────────────────────────────────

func (a *Adapter) scanNFS(_ context.Context) ([]fileEntry, error) {
	g := a.cfg.Genomics
	extSet := buildExtSet(g.Formats)
	var entries []fileEntry
	count := 0

	err := filepath.Walk(g.BasePath, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		format := matchFormat(info.Name(), extSet)
		if format == "" {
			return nil
		}
		if g.MaxFiles > 0 && count >= g.MaxFiles {
			return filepath.SkipAll
		}

		entry := fileEntry{
			PatientID: a.extractPatientID(info.Name()),
			Format:    format,
			Path:      path,
			SizeBytes: info.Size(),
		}

		// Validate / locate index
		if g.ValidateIndex {
			idxPath, err := findIndex(path, format)
			if err != nil {
				return fmt.Errorf("missing index for %s: %w", path, err)
			}
			entry.IndexPath = idxPath
		} else {
			entry.IndexPath, _ = findIndex(path, format)
		}

		entries = append(entries, entry)
		count++
		return nil
	})
	return entries, err
}

// ─── GCS backend ─────────────────────────────────────────────────────────────

func (a *Adapter) scanGCS(ctx context.Context) ([]fileEntry, error) {
	// Dynamic import: only instantiate GCS client if storage=gcs is requested.
	// Build tag or conditional import pattern avoids pulling GCS SDK into
	// deployments that never use it.
	//
	// Implementation uses the GCS JSON API via the storage SDK.
	// Credentials: GOOGLE_APPLICATION_CREDENTIALS env var or Workload Identity.
	g := a.cfg.Genomics
	extSet := buildExtSet(g.Formats)

	client, err := newGCSClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("GCS client: %w", err)
	}
	defer client.Close()

	var entries []fileEntry
	count := 0
	prefix := g.Prefix

	objs, err := listGCSObjects(ctx, client, g.Bucket, prefix)
	if err != nil {
		return nil, err
	}

	for _, obj := range objs {
		name := filepath.Base(obj.Name)
		format := matchFormat(name, extSet)
		if format == "" {
			continue
		}
		if g.MaxFiles > 0 && count >= g.MaxFiles {
			break
		}

		uri := fmt.Sprintf("gs://%s/%s", g.Bucket, obj.Name)
		entry := fileEntry{
			PatientID: a.extractPatientID(name),
			Format:    format,
			Path:      uri,
			SizeBytes: obj.Size,
		}
		if g.ValidateIndex {
			idxURI, err := findGCSIndex(ctx, client, g.Bucket, obj.Name, format)
			if err != nil {
				return nil, fmt.Errorf("missing GCS index for %s: %w", uri, err)
			}
			entry.IndexPath = idxURI
		}
		entries = append(entries, entry)
		count++
	}
	return entries, nil
}

// ─── S3/MinIO backend ────────────────────────────────────────────────────────

func (a *Adapter) scanS3(ctx context.Context) ([]fileEntry, error) {
	g := a.cfg.Genomics
	extSet := buildExtSet(g.Formats)

	client, err := newS3Client(ctx, g.Endpoint, g.Region)
	if err != nil {
		return nil, fmt.Errorf("S3 client: %w", err)
	}

	var entries []fileEntry
	count := 0

	objs, err := listS3Objects(ctx, client, g.Bucket, g.Prefix)
	if err != nil {
		return nil, err
	}

	for _, obj := range objs {
		name := filepath.Base(obj.Key)
		format := matchFormat(name, extSet)
		if format == "" {
			continue
		}
		if g.MaxFiles > 0 && count >= g.MaxFiles {
			break
		}

		var uri string
		if g.Endpoint != "" {
			// MinIO / custom S3: use endpoint-based URI
			uri = fmt.Sprintf("%s/%s/%s", strings.TrimRight(g.Endpoint, "/"), g.Bucket, obj.Key)
		} else {
			uri = fmt.Sprintf("s3://%s/%s", g.Bucket, obj.Key)
		}

		entry := fileEntry{
			PatientID: a.extractPatientID(name),
			Format:    format,
			Path:      uri,
			SizeBytes: obj.Size,
		}
		if g.ValidateIndex {
			idxURI, err := findS3Index(ctx, client, g.Bucket, obj.Key, g.Endpoint, format)
			if err != nil {
				return nil, fmt.Errorf("missing S3 index for %s: %w", uri, err)
			}
			entry.IndexPath = idxURI
		}
		entries = append(entries, entry)
		count++
	}
	return entries, nil
}

// ─── Manifest writer ─────────────────────────────────────────────────────────

func (a *Adapter) writeManifest(entries []fileEntry, outputDir string) error {
	outPath := filepath.Join(outputDir, a.cfg.Materialize.Output)
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return err
	}
	f, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer f.Close()

	w := csv.NewWriter(f)
	defer w.Flush()

	_ = w.Write([]string{"patient_id", "format", "path", "index_path", "size_gb", "storage"})
	for _, e := range entries {
		sizeGB := fmt.Sprintf("%.3f", float64(e.SizeBytes)/1e9)
		_ = w.Write([]string{
			e.PatientID, e.Format, e.Path, e.IndexPath, sizeGB, a.cfg.Genomics.Storage,
		})
	}
	a.log.Info("Genomics manifest written", zap.String("path", outPath), zap.Int("files", len(entries)))
	return nil
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

// extMap maps format name → list of recognized extensions
var extMap = map[string][]string{
	"bam":   {".bam"},
	"cram":  {".cram"},
	"vcf":   {".vcf", ".vcf.gz"},
	"fastq": {".fastq", ".fastq.gz", ".fq", ".fq.gz"},
	"bed":   {".bed", ".bed.gz"},
	"hdf5":  {".h5", ".hdf5"},
}

// indexExts maps format → expected index extensions (ordered by preference)
var indexExts = map[string][]string{
	"bam":  {".bam.bai", ".bai"},
	"cram": {".cram.crai", ".crai"},
	"vcf":  {".vcf.gz.tbi", ".tbi"},
}

func buildExtSet(formats []string) map[string]string {
	set := make(map[string]string)
	for _, fmt := range formats {
		for _, ext := range extMap[fmt] {
			set[ext] = fmt
		}
	}
	return set
}

func matchFormat(name string, extSet map[string]string) string {
	lower := strings.ToLower(name)
	// Check compound extensions first (.vcf.gz before .gz)
	for ext, format := range extSet {
		if strings.HasSuffix(lower, ext) {
			return format
		}
	}
	return ""
}

func findIndex(filePath, format string) (string, error) {
	candidates, ok := indexExts[format]
	if !ok {
		return "", nil // format has no index requirement
	}
	base := filePath
	// Strip format extension to get base
	for _, ext := range extMap[format] {
		if strings.HasSuffix(strings.ToLower(base), ext) {
			base = base[:len(base)-len(ext)]
			break
		}
	}
	for _, idxExt := range candidates {
		candidate := base + idxExt
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
		// Also try filePath + idxExt (e.g. file.bam + .bai)
		if _, err := os.Stat(filePath + ".bai"); err == nil {
			return filePath + ".bai", nil
		}
	}
	return "", fmt.Errorf("index not found for %s (expected %v)", filePath, candidates)
}

func (a *Adapter) extractPatientID(filename string) string {
	// Strip all extensions to get stem
	stem := filename
	for strings.Contains(stem, ".") {
		ext := filepath.Ext(stem)
		if ext == "" {
			break
		}
		stem = strings.TrimSuffix(stem, ext)
	}
	if a.pidRe == nil {
		return stem
	}
	m := a.pidRe.FindStringSubmatch(filename)
	if len(m) >= 2 {
		return m[1]
	}
	return stem
}
