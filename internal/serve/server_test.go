package serve

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
)

func TestServer_HealthAndACLFlow(t *testing.T) {
	srv := NewServer(0, nil, zap.NewNop())

	// 1. /health
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	srv.handleHealth(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /health status = %d, want 200", rec.Code)
	}
	var health map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &health); err != nil || health["status"] != "ok" {
		t.Fatalf("unexpected /health body: %s", rec.Body.String())
	}

	// 2. /internal/v1/acl/sync
	syncBody := `{"grantId":"grant-1","cohortId":"cohort-a","datasetId":"ds-1","allowedMrns":["MRN-1"],"expiresAt":"2099-01-01T00:00:00Z"}`
	req = httptest.NewRequest(http.MethodPost, "/internal/v1/acl/sync", bytes.NewBufferString(syncBody))
	rec = httptest.NewRecorder()
	srv.handleAclSync(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /internal/v1/acl/sync status = %d, body=%s", rec.Code, rec.Body.String())
	}

	// 3. /internal/v1/acl/status
	req = httptest.NewRequest(http.MethodGet, "/internal/v1/acl/status", nil)
	rec = httptest.NewRecorder()
	srv.handleAclStatus(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /internal/v1/acl/status status = %d", rec.Code)
	}

	// 4. /internal/v1/acl/revoke
	revokeBody := `{"grantId":"grant-1"}`
	req = httptest.NewRequest(http.MethodPost, "/internal/v1/acl/revoke", bytes.NewBufferString(revokeBody))
	rec = httptest.NewRecorder()
	srv.handleAclRevoke(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /internal/v1/acl/revoke status = %d, body=%s", rec.Code, rec.Body.String())
	}
}
