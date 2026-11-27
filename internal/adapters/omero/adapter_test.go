package omero

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/colabbio/medvfs/internal/config"
	"go.uber.org/zap"
)

func TestOMEROAdapter_Validate(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v0/token/" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":"csrf-token"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	cfg := config.SourceConfig{
		ID:       "wsi_omero",
		Type:     "omero",
		Endpoint: ts.URL,
		OMERO:    &config.OMEROQueryConfig{DatasetID: 42},
		Auth:     &config.AuthConfig{Type: "none"},
	}

	a := New(cfg, zap.NewNop())
	if a.ID() != "wsi_omero" {
		t.Fatalf("unexpected ID: %s", a.ID())
	}
	if err := a.Validate(context.Background()); err != nil {
		t.Fatalf("Validate failed: %v", err)
	}

	bad := New(config.SourceConfig{ID: "bad", Type: "omero"}, zap.NewNop())
	if err := bad.Validate(context.Background()); err == nil {
		t.Fatal("expected error for missing endpoint, got nil")
	}
}
