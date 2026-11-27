package fhir

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
)

func TestFHIRAdapter_ValidateAndFetch(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/fhir+json")
		switch r.URL.Path {
		case "/metadata":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"resourceType":"CapabilityStatement"}`))
		case "/Patient":
			_, _ = w.Write([]byte(`{
				"resourceType": "Bundle",
				"entry": [
					{"resource": {"id": "p1", "gender": "female", "birthDate": "1970-01-01"}},
					{"resource": {"id": "p2", "gender": "male", "birthDate": "1980-05-12"}}
				]
			}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	cfg := config.SourceConfig{
		ID:       "patients",
		Type:     "fhir",
		Endpoint: ts.URL,
		FHIR: &config.FHIRQueryConfig{
			Resource: "Patient",
			Fields:   []string{"id as patient_id", "gender", "birthDate"},
		},
		Materialize: config.MaterializeConfig{Output: "clinical/patients.csv"},
	}

	a := New(cfg)
	if a.ID() != "patients" {
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

	f, err := os.Open(filepath.Join(outDir, "clinical/patients.csv"))
	if err != nil {
		t.Fatalf("open output csv: %v", err)
	}
	defer f.Close()

	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatalf("read csv: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("expected 3 CSV rows (header + 2 patients), got %d", len(rows))
	}
	if rows[0][0] != "patient_id" || rows[1][0] != "p1" || rows[2][1] != "male" {
		t.Errorf("unexpected CSV content: %+v", rows)
	}
}
