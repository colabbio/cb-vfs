package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTempYAML(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "vfs.yaml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write temp yaml: %v", err)
	}
	return p
}

func TestLoad_ValidAndEnvExpansion(t *testing.T) {
	t.Setenv("TEST_VFS_NAME", "cohort-env-test")
	p := writeTempYAML(t, `
version: "1"
metadata:
  name: "${TEST_VFS_NAME}"
storage:
  type: emptydir
sources:
  - id: clinical
    type: omop
    query: "SELECT 1"
    materialize:
      output: clinical.csv
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load valid config: %v", err)
	}
	if cfg.Metadata.Name != "cohort-env-test" {
		t.Errorf("expected expanded name cohort-env-test, got %q", cfg.Metadata.Name)
	}
	if len(cfg.Sources) != 1 || cfg.Sources[0].ID != "clinical" {
		t.Errorf("unexpected sources: %+v", cfg.Sources)
	}
}

func TestLoad_ValidationErrors(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name:    "missing version",
			yaml:    "metadata:\n  name: x\nstorage:\n  type: emptydir\nsources:\n  - id: a\n    type: files\n    materialize:\n      output: out\n",
			wantErr: "version is required",
		},
		{
			name:    "invalid storage type",
			yaml:    "version: '1'\nmetadata:\n  name: x\nstorage:\n  type: s3\nsources:\n  - id: a\n    type: files\n    materialize:\n      output: out\n",
			wantErr: "storage.type",
		},
		{
			name:    "duplicate source id",
			yaml:    "version: '1'\nmetadata:\n  name: x\nstorage:\n  type: emptydir\nsources:\n  - id: a\n    type: files\n    materialize:\n      output: o1\n  - id: a\n    type: files\n    materialize:\n      output: o2\n",
			wantErr: "duplicate id",
		},
		{
			name:    "omop missing query",
			yaml:    "version: '1'\nmetadata:\n  name: x\nstorage:\n  type: emptydir\nsources:\n  - id: a\n    type: omop\n    materialize:\n      output: o1\n",
			wantErr: "query is required",
		},
		{
			name:    "path traversal in output",
			yaml:    "version: '1'\nmetadata:\n  name: x\nstorage:\n  type: emptydir\nsources:\n  - id: a\n    type: files\n    materialize:\n      output: ../escape\n",
			wantErr: "must not contain '..'",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := writeTempYAML(t, tc.yaml)
			_, err := Load(p)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("expected error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}
