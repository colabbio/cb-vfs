package orthanc

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

func TestOrthancAdapter_ValidateAndFetch(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/dicom+json")
		switch r.URL.Path {
		case "/dicom-web/studies":
			_, _ = w.Write([]byte(`[{
				"0020000D": {"vr": "UI", "Value": ["1.2.840.1"]},
				"00100020": {"vr": "LO", "Value": ["P001"]},
				"00100010": {"vr": "PN", "Value": ["Doe^Jane"]},
				"00080020": {"vr": "DA", "Value": ["20260115"]},
				"00080060": {"vr": "CS", "Value": ["CT"]},
				"00081030": {"vr": "LO", "Value": ["Chest CT"]},
				"00201206": {"vr": "IS", "Value": ["2"]}
			}]`))
		case "/dicom-web/studies/1.2.840.1/series":
			_, _ = w.Write([]byte(`[{
				"0020000E": {"vr": "UI", "Value": ["1.2.840.1.1"]},
				"00200011": {"vr": "IS", "Value": ["1"]},
				"0008103E": {"vr": "LO", "Value": ["Axial"]},
				"00201209": {"vr": "IS", "Value": ["120"]}
			}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	cfg := config.SourceConfig{
		ID:       "pacs",
		Type:     "orthanc",
		Endpoint: ts.URL,
		Orthanc: &config.OrthancQueryConfig{
			Extracts: []string{"metadata_csv", "series_manifest"},
		},
		Materialize: config.MaterializeConfig{Output: "imaging/studies.csv"},
	}

	a := New(cfg, zap.NewNop())
	if a.ID() != "pacs" {
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

	f, err := os.Open(filepath.Join(outDir, "imaging/studies.csv"))
	if err != nil {
		t.Fatalf("open studies.csv: %v", err)
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil || len(rows) != 2 || rows[1][0] != "1.2.840.1" || rows[1][1] != "P001" {
		t.Fatalf("unexpected studies.csv: err=%v rows=%+v", err, rows)
	}

	sf, err := os.Open(filepath.Join(outDir, "series_manifest.csv"))
	if err != nil {
		t.Fatalf("open series_manifest.csv: %v", err)
	}
	defer sf.Close()
	srows, err := csv.NewReader(sf).ReadAll()
	if err != nil || len(srows) != 2 || srows[1][3] != "1.2.840.1.1" {
		t.Fatalf("unexpected series_manifest.csv: err=%v rows=%+v", err, srows)
	}

	bad := New(config.SourceConfig{ID: "bad", Type: "orthanc"}, zap.NewNop())
	if err := bad.Validate(context.Background()); err == nil {
		t.Fatal("expected error for missing endpoint, got nil")
	}
}
