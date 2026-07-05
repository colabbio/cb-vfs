package files

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/colabbio/medvfs/internal/config"
	"go.uber.org/zap/zaptest"
)

func TestBuildFileTree_NFS(t *testing.T) {
	// Create a temp directory with test files
	dir := t.TempDir()
	createFile(t, filepath.Join(dir, "labels.csv"), "id,label\n1,tumor\n2,normal\n")
	createFile(t, filepath.Join(dir, "config.yaml"), "epochs: 100\nlr: 0.001\n")
	createFile(t, filepath.Join(dir, "weights.pt"), "FAKE_PYTORCH_BINARY")
	createFile(t, filepath.Join(dir, "sub", "nested.json"), `{"key":"value"}`)
	createFile(t, filepath.Join(dir, ".hidden"), "should be excluded")

	cfg := config.SourceConfig{
		ID:   "test_files",
		Type: "files",
		Files: &config.FilesConfig{
			Storage:  "nfs",
			BasePath: dir,
		},
	}

	a := New(cfg, zaptest.NewLogger(t))
	mappings, err := a.BuildFileTree(context.Background(), "", "fuse_tree")
	if err != nil {
		t.Fatalf("BuildFileTree: %v", err)
	}

	if len(mappings) != 5 {
		t.Errorf("expected 5 files, got %d", len(mappings))
		for _, m := range mappings {
			t.Logf("  %s → %s", m.LogicalPath, m.RemoteRef)
		}
	}
}

func TestBuildFileTree_NFS_WithPatterns(t *testing.T) {
	dir := t.TempDir()
	createFile(t, filepath.Join(dir, "labels.csv"), "id,label\n")
	createFile(t, filepath.Join(dir, "data.parquet"), "FAKE_PARQUET")
	createFile(t, filepath.Join(dir, "readme.md"), "# Readme")
	createFile(t, filepath.Join(dir, "model.pt"), "FAKE_MODEL")

	cfg := config.SourceConfig{
		ID:   "filtered",
		Type: "files",
		Files: &config.FilesConfig{
			Storage:  "nfs",
			BasePath: dir,
			Patterns: []string{"*.csv", "*.parquet"},
		},
	}

	a := New(cfg, zaptest.NewLogger(t))
	mappings, err := a.BuildFileTree(context.Background(), "", "fuse_tree")
	if err != nil {
		t.Fatalf("BuildFileTree: %v", err)
	}

	if len(mappings) != 2 {
		t.Errorf("expected 2 files (csv + parquet), got %d", len(mappings))
		for _, m := range mappings {
			t.Logf("  %s", m.LogicalPath)
		}
	}
}

func TestBuildFileTree_NFS_WithExclude(t *testing.T) {
	dir := t.TempDir()
	createFile(t, filepath.Join(dir, "labels.csv"), "id,label\n")
	createFile(t, filepath.Join(dir, "temp.tmp"), "temp")
	createFile(t, filepath.Join(dir, ".hidden"), "hidden")

	cfg := config.SourceConfig{
		ID:   "excluded",
		Type: "files",
		Files: &config.FilesConfig{
			Storage:  "nfs",
			BasePath: dir,
			Exclude:  []string{"*.tmp", ".*"},
		},
	}

	a := New(cfg, zaptest.NewLogger(t))
	mappings, err := a.BuildFileTree(context.Background(), "", "fuse_tree")
	if err != nil {
		t.Fatalf("BuildFileTree: %v", err)
	}

	if len(mappings) != 1 {
		t.Errorf("expected 1 file (only csv), got %d", len(mappings))
	}
	if len(mappings) > 0 && mappings[0].LogicalPath != "labels.csv" {
		t.Errorf("expected labels.csv, got %s", mappings[0].LogicalPath)
	}
}

func TestBuildFileTree_NFS_MaxFiles(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 10; i++ {
		createFile(t, filepath.Join(dir, filepath.Base(t.Name())+string(rune('a'+i))+".csv"), "data")
	}

	cfg := config.SourceConfig{
		ID:   "limited",
		Type: "files",
		Files: &config.FilesConfig{
			Storage:  "nfs",
			BasePath: dir,
			MaxFiles: 3,
		},
	}

	a := New(cfg, zaptest.NewLogger(t))
	mappings, err := a.BuildFileTree(context.Background(), "", "fuse_tree")
	if err != nil {
		t.Fatalf("BuildFileTree: %v", err)
	}

	if len(mappings) != 3 {
		t.Errorf("expected 3 files (max_files), got %d", len(mappings))
	}
}

func TestBuildFileTree_SymlinkStrategy(t *testing.T) {
	dir := t.TempDir()
	outDir := t.TempDir()
	createFile(t, filepath.Join(dir, "labels.csv"), "id,label\n1,tumor\n")

	cfg := config.SourceConfig{
		ID:   "symlinked",
		Type: "files",
		Files: &config.FilesConfig{
			Storage:  "nfs",
			BasePath: dir,
		},
	}

	a := New(cfg, zaptest.NewLogger(t))
	mappings, err := a.BuildFileTree(context.Background(), outDir, "symlink_tree")
	if err != nil {
		t.Fatalf("BuildFileTree: %v", err)
	}

	if len(mappings) != 1 {
		t.Fatalf("expected 1 mapping, got %d", len(mappings))
	}

	// Check symlink was created
	symlinkPath := mappings[0].LocalPath
	if symlinkPath == "" {
		t.Fatal("LocalPath should be set for symlink strategy")
	}

	info, err := os.Lstat(symlinkPath)
	if err != nil {
		t.Fatalf("symlink stat: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Error("expected symlink, got regular file")
	}
}

func TestMimeFromExt(t *testing.T) {
	tests := []struct {
		ext  string
		want string
	}{
		{".csv", "text/csv"},
		{".parquet", "application/vnd.apache.parquet"},
		{".pt", "application/x-pytorch"},
		{".h5", "application/x-hdf5"},
		{".json", "application/json"},
		{".yaml", "application/x-yaml"},
		{".unknown", "application/octet-stream"},
	}

	for _, tt := range tests {
		got := mimeFromExt(tt.ext)
		if got != tt.want {
			t.Errorf("mimeFromExt(%q) = %q, want %q", tt.ext, got, tt.want)
		}
	}
}

func TestValidate(t *testing.T) {
	dir := t.TempDir()
	log := zaptest.NewLogger(t)

	tests := []struct {
		name    string
		cfg     config.SourceConfig
		wantErr bool
	}{
		{
			name: "valid nfs",
			cfg: config.SourceConfig{
				ID:    "valid",
				Files: &config.FilesConfig{Storage: "nfs", BasePath: dir},
			},
		},
		{
			name: "missing files block",
			cfg:  config.SourceConfig{ID: "bad"},
			wantErr: true,
		},
		{
			name: "invalid storage",
			cfg: config.SourceConfig{
				ID:    "bad",
				Files: &config.FilesConfig{Storage: "azure"},
			},
			wantErr: true,
		},
		{
			name: "nfs bad path",
			cfg: config.SourceConfig{
				ID:    "bad",
				Files: &config.FilesConfig{Storage: "nfs", BasePath: "/nonexistent"},
			},
			wantErr: true,
		},
		{
			name: "gcs missing bucket",
			cfg: config.SourceConfig{
				ID:    "bad",
				Files: &config.FilesConfig{Storage: "gcs"},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := New(tt.cfg, log)
			err := a.Validate(context.Background())
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// createFile is a test helper that creates a file with content.
func createFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
