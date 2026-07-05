package dicom

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/colabbio/medvfs/internal/adapters"
	"github.com/colabbio/medvfs/internal/config"
	"go.uber.org/zap"
)

// createTestDICOMFile creates a minimal valid DICOM file with known tags.
func createTestDICOMFile(t *testing.T, dir, filename string, patientID, modality, studyDate string) string {
	t.Helper()
	path := filepath.Join(dir, filename)

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	// 128-byte preamble (zeros) + "DICM" magic
	preamble := make([]byte, 128)
	f.Write(preamble)
	f.WriteString("DICM")

	// Write explicit VR little-endian data elements
	writeTag := func(group, elem uint16, vr string, value string) {
		// Pad value to even length
		val := value
		if len(val)%2 != 0 {
			val += "\x00"
		}
		buf := make([]byte, 4)
		binary.LittleEndian.PutUint16(buf[0:2], group)
		binary.LittleEndian.PutUint16(buf[2:4], elem)
		f.Write(buf)
		f.WriteString(vr)
		lenBuf := make([]byte, 2)
		binary.LittleEndian.PutUint16(lenBuf, uint16(len(val)))
		f.Write(lenBuf)
		f.WriteString(val)
	}

	// StudyDate (0008,0020)
	writeTag(0x0008, 0x0020, "DA", studyDate)
	// Modality (0008,0060)
	writeTag(0x0008, 0x0060, "CS", modality)
	// PatientID (0010,0020)
	writeTag(0x0010, 0x0020, "LO", patientID)

	return path
}

func TestDICOMAdapterScan(t *testing.T) {
	dir := t.TempDir()

	// Create test DICOM files
	createTestDICOMFile(t, dir, "img1.dcm", "PT001", "CT", "20240115")
	createTestDICOMFile(t, dir, "img2.dcm", "PT001", "CT", "20240115")
	createTestDICOMFile(t, dir, "img3.dcm", "PT002", "MR", "20240220")

	log, _ := zap.NewNop(), error(nil)
	outDir := filepath.Join(dir, "output")

	cfg := config.SourceConfig{
		ID:   "test-dicom",
		Type: "dicom",
		DICOM: &config.DICOMConfig{
			LocalWatchDir: dir,
			Extracts:      []string{"metadata_csv"},
		},
		Materialize: config.MaterializeConfig{
			Output: "dicom_metadata.csv",
		},
	}

	adapter := New(cfg, log)

	t.Run("Validate", func(t *testing.T) {
		if err := adapter.Validate(context.Background()); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})

	t.Run("Fetch", func(t *testing.T) {
		progress := make(chan adapters.Event, 10)
		// Import needed
		err := adapter.Fetch(context.Background(), outDir, progress)
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		// Check CSV was created
		csvPath := filepath.Join(outDir, "dicom_metadata.csv")
		if _, err := os.Stat(csvPath); err != nil {
			t.Fatalf("CSV not created: %v", err)
		}
	})

	t.Run("BuildFileTree_fuse", func(t *testing.T) {
		mappings, err := adapter.BuildFileTree(context.Background(), outDir, "fuse_tree")
		if err != nil {
			t.Fatalf("BuildFileTree: %v", err)
		}
		if len(mappings) != 3 {
			t.Fatalf("expected 3 mappings, got %d", len(mappings))
		}
		// Verify logical paths are organised
		for _, m := range mappings {
			if m.LogicalPath == "" {
				t.Fatal("empty LogicalPath")
			}
			if m.RemoteRef == "" {
				t.Fatal("empty RemoteRef")
			}
			if m.ContentType != "application/dicom" {
				t.Fatalf("unexpected ContentType: %s", m.ContentType)
			}
		}
	})

	t.Run("BuildFileTree_symlink", func(t *testing.T) {
		symlinkDir := filepath.Join(dir, "symlinks")
		mappings, err := adapter.BuildFileTree(context.Background(), symlinkDir, "symlink_tree")
		if err != nil {
			t.Fatalf("BuildFileTree: %v", err)
		}
		// Verify symlinks were created
		for _, m := range mappings {
			symlinkPath := filepath.Join(symlinkDir, m.LogicalPath)
			target, err := os.Readlink(symlinkPath)
			if err != nil {
				t.Fatalf("symlink not created at %s: %v", symlinkPath, err)
			}
			if target == "" {
				t.Fatalf("symlink has empty target: %s", symlinkPath)
			}
		}
		if len(mappings) != 3 {
			t.Fatalf("expected 3 symlinks, got %d", len(mappings))
		}
	})
}

func TestDICOMIsDICOMFile(t *testing.T) {
	tests := []struct {
		path     string
		expected bool
	}{
		{"image.dcm", true},
		{"image.dicom", true},
		{"DICOMDIR", true},
		{"image", true},   // no extension — possibly DICOM
		{"image.txt", false},
		{"image.csv", false},
		{"image.jpg", false},
	}

	for _, tc := range tests {
		result := isDICOMFile(tc.path)
		if result != tc.expected {
			t.Errorf("isDICOMFile(%q): expected %v, got %v", tc.path, tc.expected, result)
		}
	}
}
