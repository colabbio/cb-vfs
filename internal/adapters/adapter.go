package adapters

import (
	"context"
	"time"
)

// Event is a progress/status notification emitted by an Adapter during Fetch.
type Event struct {
	SourceID string
	Status   string // "start" | "progress" | "done" | "error"
	Message  string
	Rows     int
	Bytes    int64
}

// Adapter is the interface every data source adapter must implement.
type Adapter interface {
	// ID returns the source identifier from vfs.yaml.
	ID() string

	// Validate checks connectivity and credentials without fetching data.
	Validate(ctx context.Context) error

	// Fetch materialises the source data into outDir, emitting progress events.
	Fetch(ctx context.Context, outDir string, progress chan<- Event) error
}

// ─── File Virtualization ──────────────────────────────────────────────────────

// FileMapping represents a single file that can be materialised locally
// via download, symlink, or FUSE virtual node. Each adapter that supports
// file-level access returns a slice of these from BuildFileTree.
type FileMapping struct {
	// LogicalPath is the organised path relative to the VFS root.
	// Example: "PT001/20240115_CHEST_CT/series_001/img_0001.dcm"
	LogicalPath string

	// LocalPath is the resolved local filesystem path (set after download/symlink).
	// Empty when strategy is FUSE (the FUSE handler resolves it on-demand).
	LocalPath string

	// RemoteRef is the canonical remote reference used by fetchers to retrieve
	// the file content. Format is scheme-qualified:
	//   "omero://image/42"
	//   "wado-rs://orthanc:8042/studies/1.2.3/series/4.5.6/instances/7.8.9"
	//   "https://storage.googleapis.com/bucket/path/to/file.bam"
	RemoteRef string

	// SizeBytes is the known file size (from remote metadata / HEAD request).
	// Used by the cache for LRU eviction and by FUSE for GetAttr.
	SizeBytes int64

	// ContentType is the MIME type of the file content.
	// Examples: "application/dicom", "image/tiff", "application/octet-stream"
	ContentType string
}

// FileProvider is implemented by adapters that can produce local file paths
// for remote resources. The strategy parameter selects the materialisation
// approach: "download", "symlink_tree", or "fuse_tree".
type FileProvider interface {
	BuildFileTree(ctx context.Context, outDir string, strategy string) ([]FileMapping, error)
}

// ─── Real-Time Streaming ──────────────────────────────────────────────────────

// LiveEvent represents a file-level event detected by a streaming adapter
// (e.g., a new DICOM file arriving in a watched directory).
type LiveEvent struct {
	SourceID  string
	Type      string // "new_file" | "modified" | "deleted"
	File      FileMapping
	Timestamp time.Time
}

// StreamingAdapter extends Adapter with the ability to watch a data source
// for real-time changes and emit LiveEvents as they occur.
type StreamingAdapter interface {
	Adapter

	// StartListening begins watching the data source for changes.
	// It blocks until ctx is cancelled, emitting events on the channel.
	// The caller is responsible for closing the events channel after
	// StartListening returns.
	StartListening(ctx context.Context, events chan<- LiveEvent) error
}
