// Package dicom implements a MedVFS adapter that scans a local directory
// for DICOM files, parses key tags (PatientID, Modality, StudyDate,
// SeriesNumber), and produces:
//   - metadata_csv:  study-level CSV with parsed DICOM tags
//   - symlink_tree:  organised symlink hierarchy for ML pipelines
//   - fuse_tree:     FileMapping list for the FUSE virtual filesystem
//
// In streaming mode (via StartListening), it uses fsnotify to watch for
// new DICOM files arriving and emits LiveEvents.
package dicom

import (
	"context"
	"encoding/binary"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/colabbio/medvfs/internal/adapters"
	"github.com/colabbio/medvfs/internal/config"
	"github.com/fsnotify/fsnotify"
	"go.uber.org/zap"
)

// dicomFile holds parsed metadata from a single DICOM file.
type dicomFile struct {
	Path             string
	PatientID        string
	PatientName      string
	StudyInstanceUID string
	StudyDate        string
	StudyTime        string
	Modality         string
	SeriesInstanceUID string
	SeriesNumber     string
	SOPInstanceUID   string
	InstanceNumber   string
	SizeBytes        int64
}

// Adapter scans local DICOM directories and optionally watches for new files.
type Adapter struct {
	cfg   config.SourceConfig
	log   *zap.Logger
	mu    sync.Mutex
	files []dicomFile
}

// New creates a new DICOM local filesystem adapter.
func New(cfg config.SourceConfig, log *zap.Logger) *Adapter {
	return &Adapter{cfg: cfg, log: log}
}

// ID implements adapters.Adapter.
func (a *Adapter) ID() string { return a.cfg.ID }

// Validate checks that the watch directory exists and is readable.
func (a *Adapter) Validate(ctx context.Context) error {
	if a.cfg.DICOM == nil {
		return fmt.Errorf("dicom adapter %q: dicom: block is required", a.cfg.ID)
	}
	dir := a.cfg.DICOM.LocalWatchDir
	if dir == "" {
		return fmt.Errorf("dicom adapter %q: dicom.local_watch_dir is required", a.cfg.ID)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("dicom adapter %q: cannot access %q: %w", a.cfg.ID, dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("dicom adapter %q: %q is not a directory", a.cfg.ID, dir)
	}
	return nil
}

// Fetch scans the local directory for DICOM files and produces the configured
// extracts (metadata_csv, symlink_tree, fuse_tree).
func (a *Adapter) Fetch(ctx context.Context, outDir string, progress chan<- adapters.Event) error {
	progress <- adapters.Event{SourceID: a.cfg.ID, Status: "start", Message: "scanning DICOM directory"}

	if err := a.scanDirectory(ctx); err != nil {
		return fmt.Errorf("dicom adapter %q: scan: %w", a.cfg.ID, err)
	}

	a.log.Info("DICOM files scanned",
		zap.String("source", a.cfg.ID),
		zap.Int("files", len(a.files)),
		zap.String("dir", a.cfg.DICOM.LocalWatchDir))

	extracts := a.cfg.DICOM.Extracts
	if len(extracts) == 0 {
		extracts = []string{"metadata_csv"}
	}

	for _, extract := range extracts {
		switch extract {
		case "metadata_csv":
			if err := a.writeMetadataCSV(outDir); err != nil {
				return fmt.Errorf("dicom adapter %q: metadata_csv: %w", a.cfg.ID, err)
			}
		case "symlink_tree":
			if err := a.buildSymlinkTree(outDir); err != nil {
				return fmt.Errorf("dicom adapter %q: symlink_tree: %w", a.cfg.ID, err)
			}
		case "fuse_tree":
			// fuse_tree is handled via BuildFileTree, not Fetch
			a.log.Info("fuse_tree requested — use BuildFileTree for VFS mode")
		default:
			a.log.Warn("Unknown DICOM extract type, skipping", zap.String("extract", extract))
		}
	}

	progress <- adapters.Event{SourceID: a.cfg.ID, Status: "done",
		Message: "DICOM scan complete", Rows: len(a.files)}
	return nil
}

// ─── FileProvider ────────────────────────────────────────────────────────────

// BuildFileTree implements adapters.FileProvider.
// Scans DICOM files and returns FileMappings with organised logical paths.
// Strategy:
//   - "download"      → returns mappings with LocalPath set (files already local)
//   - "symlink_tree"  → creates symlinks, returns mappings with LocalPath
//   - "fuse_tree"     → returns mappings with RemoteRef (file:// URI for local DICOM)
func (a *Adapter) BuildFileTree(ctx context.Context, outDir string, strategy string) ([]adapters.FileMapping, error) {
	if err := a.scanDirectory(ctx); err != nil {
		return nil, fmt.Errorf("dicom adapter %q: scan: %w", a.cfg.ID, err)
	}

	a.mu.Lock()
	files := make([]dicomFile, len(a.files))
	copy(files, a.files)
	a.mu.Unlock()

	mappings := make([]adapters.FileMapping, 0, len(files))
	for _, f := range files {
		logicalPath := buildLogicalPath(f)

		fm := adapters.FileMapping{
			LogicalPath: logicalPath,
			SizeBytes:   f.SizeBytes,
			ContentType: "application/dicom",
		}

		switch strategy {
		case "download", "symlink_tree":
			// For local DICOM, files are already on disk — LocalPath = original
			fm.LocalPath = f.Path
			fm.RemoteRef = "file://" + f.Path

			if strategy == "symlink_tree" {
				symlinkPath := filepath.Join(outDir, logicalPath)
				if err := os.MkdirAll(filepath.Dir(symlinkPath), 0o755); err != nil {
					return nil, err
				}
				// Create relative symlink from symlink location to original file
				relPath, err := filepath.Rel(filepath.Dir(symlinkPath), f.Path)
				if err != nil {
					relPath = f.Path // fallback to absolute
				}
				_ = os.Remove(symlinkPath) // idempotent
				if err := os.Symlink(relPath, symlinkPath); err != nil {
					a.log.Warn("Symlink failed", zap.String("path", symlinkPath), zap.Error(err))
				}
				fm.LocalPath = symlinkPath
			}

		case "fuse_tree":
			// FUSE mode: files are local, so RemoteRef is a file:// URI
			// The FUSE layer can serve these directly via os.Open
			fm.RemoteRef = "file://" + f.Path

		default:
			return nil, fmt.Errorf("unknown strategy %q", strategy)
		}

		mappings = append(mappings, fm)
	}

	a.log.Info("DICOM file tree built",
		zap.String("strategy", strategy),
		zap.Int("files", len(mappings)))

	return mappings, nil
}

// ─── StreamingAdapter ────────────────────────────────────────────────────────

// StartListening implements adapters.StreamingAdapter.
// Watches the DICOM directory for new files and emits LiveEvents.
func (a *Adapter) StartListening(ctx context.Context, events chan<- adapters.LiveEvent) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("dicom adapter %q: fsnotify: %w", a.cfg.ID, err)
	}
	defer watcher.Close()

	watchDir := a.cfg.DICOM.LocalWatchDir

	// Watch root and all subdirectories
	err = filepath.Walk(watchDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || !info.IsDir() {
			return err
		}
		return watcher.Add(path)
	})
	if err != nil {
		return fmt.Errorf("dicom adapter %q: walk watch dirs: %w", a.cfg.ID, err)
	}

	a.log.Info("DICOM watching for new files",
		zap.String("source", a.cfg.ID),
		zap.String("dir", watchDir))

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			if event.Op&(fsnotify.Create|fsnotify.Write) == 0 {
				continue
			}
			// Check if it's a DICOM file
			if !isDICOMFile(event.Name) {
				// If it's a new directory, watch it too
				if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
					watcher.Add(event.Name) //nolint:errcheck
				}
				continue
			}

			// Parse the new DICOM file
			df, err := parseDICOMFile(event.Name)
			if err != nil {
				a.log.Warn("Could not parse new DICOM file",
					zap.String("path", event.Name), zap.Error(err))
				continue
			}

			// Add to our internal list
			a.mu.Lock()
			a.files = append(a.files, df)
			a.mu.Unlock()

			// Emit live event
			events <- adapters.LiveEvent{
				SourceID:  a.cfg.ID,
				Type:      "new_file",
				Timestamp: time.Now(),
				File: adapters.FileMapping{
					LogicalPath: buildLogicalPath(df),
					LocalPath:   df.Path,
					RemoteRef:   "file://" + df.Path,
					SizeBytes:   df.SizeBytes,
					ContentType: "application/dicom",
				},
			}

		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			a.log.Warn("fsnotify error", zap.Error(err))
		}
	}
}

// ─── Internal helpers ────────────────────────────────────────────────────────

// scanDirectory walks the watch directory and parses DICOM files.
func (a *Adapter) scanDirectory(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.files = nil

	return filepath.Walk(a.cfg.DICOM.LocalWatchDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		// Check context cancellation periodically
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if info.IsDir() || !isDICOMFile(path) {
			return nil
		}

		df, err := parseDICOMFile(path)
		if err != nil {
			a.log.Debug("Skipping non-DICOM file", zap.String("path", path), zap.Error(err))
			return nil // skip non-parseable files
		}
		a.files = append(a.files, df)
		return nil
	})
}

// isDICOMFile checks if a file path looks like a DICOM file.
func isDICOMFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	base := strings.ToLower(filepath.Base(path))
	// Standard .dcm extension
	if ext == ".dcm" || ext == ".dicom" {
		return true
	}
	// DICOMDIR
	if base == "dicomdir" {
		return true
	}
	// No extension but could be DICOM (common in PACS export)
	if ext == "" {
		return true
	}
	return false
}

// parseDICOMFile does a minimal parse of a DICOM file to extract key tags.
// We do NOT import a full DICOM library — we read just the preamble, magic,
// and scan for key tags using a lightweight tag scanner.
func parseDICOMFile(path string) (dicomFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return dicomFile{}, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return dicomFile{}, err
	}

	df := dicomFile{
		Path:      path,
		SizeBytes: info.Size(),
	}

	// Read preamble (128 bytes) + magic ("DICM")
	preamble := make([]byte, 132)
	if _, err := io.ReadFull(f, preamble); err != nil {
		return dicomFile{}, fmt.Errorf("not a DICOM file (too short)")
	}
	if string(preamble[128:132]) != "DICM" {
		return dicomFile{}, fmt.Errorf("not a DICOM file (no DICM magic)")
	}

	// Scan data elements to extract key tags.
	// We read explicit VR little-endian (most common transfer syntax).
	tags := scanTags(f, 8192) // scan first 8KB of data elements

	df.PatientID = tags["00100020"]
	df.PatientName = tags["00100010"]
	df.StudyInstanceUID = tags["0020000D"]
	df.StudyDate = tags["00080020"]
	df.StudyTime = tags["00080030"]
	df.Modality = tags["00080060"]
	df.SeriesInstanceUID = tags["0020000E"]
	df.SeriesNumber = tags["00200011"]
	df.SOPInstanceUID = tags["00080018"]
	df.InstanceNumber = tags["00200013"]

	// Use defaults if critical fields are empty
	if df.PatientID == "" {
		df.PatientID = "UNKNOWN"
	}
	if df.Modality == "" {
		df.Modality = "OT" // Other
	}
	if df.StudyDate == "" {
		df.StudyDate = "00000000"
	}

	return df, nil
}

// scanTags does a minimal scan of DICOM data elements to extract string values
// for known tags. Reads up to maxBytes of tag data after the preamble.
func scanTags(r io.Reader, maxBytes int) map[string]string {
	tags := make(map[string]string)
	buf := make([]byte, maxBytes)
	n, _ := io.ReadFull(r, buf)
	if n < 8 {
		return tags
	}
	buf = buf[:n]

	// Scan through explicit VR data elements
	pos := 0
	for pos+8 <= len(buf) {
		group := binary.LittleEndian.Uint16(buf[pos:])
		elem := binary.LittleEndian.Uint16(buf[pos+2:])
		tagHex := fmt.Sprintf("%04X%04X", group, elem)

		// Stop at pixel data or beyond group 0x0020
		if group > 0x0020 && group != 0x0032 {
			break
		}

		vr := string(buf[pos+4 : pos+6])

		var valueLen int
		var dataOffset int

		// Check for VRs with 4-byte length (OB, OW, OF, SQ, UC, UN, UR, UT)
		switch vr {
		case "OB", "OW", "OF", "SQ", "UC", "UN", "UR", "UT":
			if pos+12 > len(buf) {
				break
			}
			valueLen = int(binary.LittleEndian.Uint32(buf[pos+8:]))
			dataOffset = pos + 12
		default:
			if pos+8 > len(buf) {
				break
			}
			valueLen = int(binary.LittleEndian.Uint16(buf[pos+6:]))
			dataOffset = pos + 8
		}

		if valueLen < 0 || valueLen > 65535 || dataOffset+valueLen > len(buf) {
			// Skip this element — likely corrupted or we've gone off track
			pos += 8
			continue
		}

		// Extract string values for known patient/study/series tags
		switch tagHex {
		case "00080018", "00080020", "00080030", "00080060",
			"00100010", "00100020",
			"0020000D", "0020000E", "00200011", "00200013":
			val := strings.TrimRight(string(buf[dataOffset:dataOffset+valueLen]), "\x00 ")
			tags[tagHex] = val
		}

		if vr == "SQ" {
			// Skip sequences
			if valueLen == 0xFFFFFFFF {
				break // undefined length — bail
			}
		}
		pos = dataOffset + valueLen
		// Align to even boundary
		if pos%2 != 0 {
			pos++
		}
	}
	return tags
}

// buildLogicalPath creates an organised path from DICOM metadata.
// Format: {PatientID}/{StudyDate}_{Modality}/{SeriesNumber}/{filename}.dcm
func buildLogicalPath(f dicomFile) string {
	seriesDir := f.SeriesNumber
	if seriesDir == "" {
		seriesDir = "series_unknown"
	} else {
		seriesDir = "series_" + seriesDir
	}

	studyDir := f.StudyDate + "_" + f.Modality
	filename := filepath.Base(f.Path)

	return filepath.Join(f.PatientID, studyDir, seriesDir, filename)
}

// writeMetadataCSV writes a CSV of all scanned DICOM files with their tags.
func (a *Adapter) writeMetadataCSV(outDir string) error {
	outPath := filepath.Join(outDir, a.cfg.Materialize.Output)
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

	headers := []string{
		"file_path", "patient_id", "patient_name",
		"study_instance_uid", "study_date", "study_time",
		"modality", "series_instance_uid", "series_number",
		"sop_instance_uid", "instance_number", "size_bytes",
	}
	if err := w.Write(headers); err != nil {
		return err
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	// Sort by patient → study → series → instance for deterministic output
	sort.Slice(a.files, func(i, j int) bool {
		if a.files[i].PatientID != a.files[j].PatientID {
			return a.files[i].PatientID < a.files[j].PatientID
		}
		if a.files[i].StudyDate != a.files[j].StudyDate {
			return a.files[i].StudyDate < a.files[j].StudyDate
		}
		if a.files[i].SeriesNumber != a.files[j].SeriesNumber {
			return a.files[i].SeriesNumber < a.files[j].SeriesNumber
		}
		return a.files[i].InstanceNumber < a.files[j].InstanceNumber
	})

	for _, df := range a.files {
		row := []string{
			df.Path, df.PatientID, df.PatientName,
			df.StudyInstanceUID, df.StudyDate, df.StudyTime,
			df.Modality, df.SeriesInstanceUID, df.SeriesNumber,
			df.SOPInstanceUID, df.InstanceNumber,
			fmt.Sprintf("%d", df.SizeBytes),
		}
		if err := w.Write(row); err != nil {
			return err
		}
	}
	a.log.Info("DICOM metadata CSV written",
		zap.String("path", outPath), zap.Int("files", len(a.files)))
	return nil
}

// buildSymlinkTree creates an organised symlink tree from scanned DICOM files.
func (a *Adapter) buildSymlinkTree(outDir string) error {
	root := a.cfg.DICOM.SymlinkRoot
	if root == "" {
		root = filepath.Join(outDir, "dicom_tree")
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	created := 0
	for _, df := range a.files {
		logicalPath := buildLogicalPath(df)
		symlinkPath := filepath.Join(root, logicalPath)

		if err := os.MkdirAll(filepath.Dir(symlinkPath), 0o755); err != nil {
			return err
		}
		relPath, err := filepath.Rel(filepath.Dir(symlinkPath), df.Path)
		if err != nil {
			relPath = df.Path
		}
		_ = os.Remove(symlinkPath)
		if err := os.Symlink(relPath, symlinkPath); err != nil {
			a.log.Warn("Symlink failed", zap.String("path", symlinkPath), zap.Error(err))
			continue
		}
		created++
	}
	a.log.Info("DICOM symlink tree built",
		zap.String("root", root), zap.Int("symlinks", created))
	return nil
}
