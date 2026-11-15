package timelapse

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/colabbio/medvfs/internal/adapters"
)

func TestTimelapseAdapterBuildFileTreeAndFetch(t *testing.T) {
	tmpRoot := t.TempDir()
	// Create mock 24-well organoid timelapse frames
	for _, well := range []string{"A1", "B2"} {
		wellDir := filepath.Join(tmpRoot, well)
		if err := os.MkdirAll(wellDir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, frame := range []string{"t_0001.tif", "t_0002.tif"} {
			if err := os.WriteFile(filepath.Join(wellDir, frame), []byte("TIFF_MOCK"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	outDir := filepath.Join(t.TempDir(), "vfs_out")
	adp := New(Config{
		SourceID:        "morphovlm_organoids_2d_t",
		RootDir:         tmpRoot,
		PlateID:         "UCSC_PLATE_24W_01",
		WellFilter:      []string{"A1"},
		Channel:         "brightfield",
		IntervalMinutes: 60,
	})

	events := make(chan adapters.Event, 4)
	if err := adp.Fetch(context.Background(), outDir, events); err != nil {
		t.Fatalf("Fetch failed: %v", err)
	}
	close(events)

	expected := filepath.Join(outDir, "UCSC_PLATE_24W_01", "A1", "brightfield", "t_0001.tif")
	if _, err := os.Lstat(expected); err != nil {
		t.Fatalf("expected symlinked 2D+t frame at %s: %v", expected, err)
	}

	excluded := filepath.Join(outDir, "UCSC_PLATE_24W_01", "B2", "brightfield", "t_0001.tif")
	if _, err := os.Lstat(excluded); !os.IsNotExist(err) {
		t.Fatalf("expected well B2 to be filtered out, but %s exists", excluded)
	}
}
