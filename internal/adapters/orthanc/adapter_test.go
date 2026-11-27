package orthanc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/colabbio/medvfs/internal/config"
	"go.uber.org/zap"
)

func TestOrthancAdapter_Validate(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/dicom-web/studies" {
			w.Header().Set("Content-Type", "application/dicom+json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[]`))
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	cfg := config.SourceConfig{
		ID:       "pacs",
		Type:     "orthanc",
		Endpoint: ts.URL,
		Orthanc:  &config.OrthancQueryConfig{Extracts: []string{"metadata_csv"}},
	}

	a := New(cfg, zap.NewNop())
	if a.ID() != "pacs" {
		t.Fatalf("unexpected ID: %s", a.ID())
	}
	if err := a.Validate(context.Background()); err != nil {
		t.Fatalf("Validate failed: %v", err)
	}

	bad := New(config.SourceConfig{ID: "bad", Type: "orthanc"}, zap.NewNop())
	if err := bad.Validate(context.Background()); err == nil {
		t.Fatal("expected error for missing endpoint, got nil")
	}
}
