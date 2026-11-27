package omero

import (
	"context"
	"encoding/csv"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/colabbio/medvfs/internal/adapters"
	"github.com/colabbio/medvfs/internal/config"
	"go.uber.org/zap"
)

func TestOMEROAdapter_ValidateAndFetch(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v0/token/":
			_, _ = w.Write([]byte(`{"data":"csrf-token"}`))
		case "/api/v0/login/":
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			_, _ = w.Write([]byte(`{"eventContext":{"userName":"root"}}`))
		case "/api/v0/m/datasets/42/images/":
			_, _ = w.Write([]byte(`{
				"meta": {"offset": 0, "limit": 100, "totalCount": 1},
				"data": [{
					"@id": 7,
					"Name": "slide_001.svs",
					"AcquisitionDate": "2026-08-01",
					"Pixels": {"SizeX": 4096, "SizeY": 4096, "SizeZ": 1, "SizeC": 3, "SizeT": 1}
				}]
			}`))
		case "/api/v0/m/images/7/annotations/":
			_, _ = w.Write([]byte(`{
				"data": [{
					"id": 10,
					"values": [["patient_id", "P001"], ["diagnosis", "LUAD"]]
				}]
			}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	cfg := config.SourceConfig{
		ID:       "wsi_omero",
		Type:     "omero",
		Endpoint: ts.URL,
		OMERO: &config.OMEROQueryConfig{
			DatasetID: 42,
			Extracts:  []string{"metadata_csv"},
		},
		Auth:        &config.AuthConfig{Type: "none"},
		Materialize: config.MaterializeConfig{Output: "wsi/images.csv"},
	}

	a := New(cfg, zap.NewNop())
	if a.ID() != "wsi_omero" {
		t.Fatalf("unexpected ID: %s", a.ID())
	}
	if err := a.Validate(context.Background()); err != nil {
		t.Fatalf("Validate failed: %v", err)
	}

	outDir := t.TempDir()
	events := make(chan adapters.Event, 16)
	if err := a.Fetch(context.Background(), outDir, events); err != nil {
		t.Fatalf("Fetch failed: %v", err)
	}
	close(events)

	f, err := os.Open(filepath.Join(outDir, "wsi/images.csv"))
	if err != nil {
		t.Fatalf("open images.csv: %v", err)
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil || len(rows) != 2 || rows[1][1] != "slide_001.svs" {
		t.Fatalf("unexpected images.csv: err=%v rows=%+v", err, rows)
	}

	bad := New(config.SourceConfig{ID: "bad", Type: "omero"}, zap.NewNop())
	if err := bad.Validate(context.Background()); err == nil {
		t.Fatal("expected error for missing endpoint, got nil")
	}
}
