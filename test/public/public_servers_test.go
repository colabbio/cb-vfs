//go:build public

// Package public_test verifies MedVFS adapters and HTTP Range streaming against
// live, unauthenticated public servers. Intended for manual/network validation
// (e.g. from BSC or UC Berkeley networks) via:
//
//	make test-public
package public_test

import (
	"context"
	"encoding/csv"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/colabbio/medvfs/internal/adapters"
	"github.com/colabbio/medvfs/internal/adapters/fhir"
	"github.com/colabbio/medvfs/internal/adapters/orthanc"
	"github.com/colabbio/medvfs/internal/config"
	"github.com/colabbio/medvfs/internal/vfs"
	"go.uber.org/zap"
)

func TestPublicFHIR_HAPIR4(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cfg := config.SourceConfig{
		ID:       "public_hapi_fhir",
		Type:     "fhir",
		Endpoint: "https://hapi.fhir.org/baseR4",
		FHIR: &config.FHIRQueryConfig{
			Resource: "Patient",
			Params:   map[string]string{"_id": "nhanes-111640"},
			Fields:   []string{"id as patient_id", "gender"},
		},
		Materialize: config.MaterializeConfig{Output: "fhir_patients.csv"},
	}

	a := fhir.New(cfg)
	if err := a.Validate(ctx); err != nil {
		t.Fatalf("HAPI FHIR Validate failed: %v", err)
	}

	outDir := t.TempDir()
	events := make(chan adapters.Event, 16)
	if err := a.Fetch(ctx, outDir, events); err != nil {
		t.Fatalf("HAPI FHIR Fetch failed: %v", err)
	}
	close(events)

	f, err := os.Open(filepath.Join(outDir, "fhir_patients.csv"))
	if err != nil {
		t.Fatalf("open output csv: %v", err)
	}
	defer f.Close()

	rows, err := csv.NewReader(f).ReadAll()
	if err != nil || len(rows) < 2 {
		t.Fatalf("expected >= 2 CSV rows from HAPI FHIR, got %d (err=%v)", len(rows), err)
	}
	t.Logf("HAPI FHIR returned %d patient row(s)", len(rows)-1)
}

func TestPublicDICOMweb_OrthancDemo(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cfg := config.SourceConfig{
		ID:       "public_orthanc",
		Type:     "orthanc",
		Endpoint: "https://orthanc.uclouvain.be",
		Orthanc: &config.OrthancQueryConfig{
			DICOMwebPath: "/demo/dicom-web",
			Extracts:     []string{"metadata_csv"},
			MaxStudies:   2,
		},
		Materialize: config.MaterializeConfig{Output: "orthanc_studies.csv"},
	}

	a := orthanc.New(cfg, zap.NewNop())
	if err := a.Validate(ctx); err != nil {
		t.Fatalf("Orthanc demo Validate failed: %v", err)
	}

	outDir := t.TempDir()
	events := make(chan adapters.Event, 16)
	if err := a.Fetch(ctx, outDir, events); err != nil {
		t.Fatalf("Orthanc demo Fetch failed: %v", err)
	}
	close(events)

	f, err := os.Open(filepath.Join(outDir, "orthanc_studies.csv"))
	if err != nil {
		t.Fatalf("open output csv: %v", err)
	}
	defer f.Close()

	rows, err := csv.NewReader(f).ReadAll()
	if err != nil || len(rows) < 2 {
		t.Fatalf("expected >= 2 CSV rows from Orthanc demo, got %d (err=%v)", len(rows), err)
	}
	t.Logf("Orthanc demo returned %d study row(s)", len(rows)-1)
}

func TestPublicHTTPRange_FetchBlock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	fetcher := vfs.NewHTTPFetcher(nil)
	url := "https://raw.githubusercontent.com/colabbio/cb-vfs/main/LICENSE"

	block, err := fetcher.FetchBlock(ctx, url, 0, 64)
	if err != nil {
		t.Fatalf("HTTPFetcher.FetchBlock failed: %v", err)
	}
	if len(block) == 0 {
		t.Fatal("expected non-empty block from public HTTP Range fetch")
	}
	t.Logf("Fetched %d bytes via HTTP Range from %s", len(block), url)
}
