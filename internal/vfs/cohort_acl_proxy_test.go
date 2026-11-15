package vfs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestCohortAclRegistrySyncAndKillSwitch(t *testing.T) {
	acl := NewCohortAclRegistry()
	acl.SyncGrant(CohortGrantRule{
		GrantID:         "GRANT-LAFE-001",
		AllowedPaths:    []string{"/slides/prostate"},
		AllowedPatients: []string{"TCGA-EJ-7781"},
		ExpiresAt:       time.Now().Add(1 * time.Hour),
	})

	if !acl.IsAllowed("GRANT-LAFE-001", "/slides/prostate/TCGA-EJ-7781.svs") {
		t.Fatal("expected in-cohort path to be allowed")
	}
	if acl.IsAllowed("GRANT-LAFE-001", "/slides/secret/PAT-OUTSIDE.svs") {
		t.Fatal("expected out-of-cohort path to be blocked")
	}

	// Execute hot Kill-Switch
	acl.RevokeGrant("GRANT-LAFE-001", "DPO audit revocation")
	if acl.IsAllowed("GRANT-LAFE-001", "/slides/prostate/TCGA-EJ-7781.svs") {
		t.Fatal("expected revoked grant to fail closed immediately")
	}
}

func TestDispatchFetcherHierarchicalCbProxyScheme(t *testing.T) {
	mockPayload := []byte("0123456789ABCDEF")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-ColabBio-Grant-Id") != "GRANT-LAFE-001" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", "16")
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(mockPayload[4:10])
	}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	df := NewDispatchFetcher(nil, zap.NewNop())

	// Authorized hierarchical fetch
	ref := "cb-proxy://" + host + "/GRANT-LAFE-001/prostate/TCGA-EJ-7781.svs"
	sz, err := df.FileSize(context.Background(), ref)
	if err != nil || sz != 16 {
		t.Fatalf("expected size 16, got %d err=%v", sz, err)
	}
	blk, err := df.FetchBlock(context.Background(), ref, 4, 6)
	if err != nil {
		t.Fatalf("expected block fetch to succeed: %v", err)
	}
	if string(blk) != "456789" {
		t.Fatalf("unexpected block data %q", string(blk))
	}

	// Unauthorized grant must fail with 403
	badRef := "cb-proxy://" + host + "/GRANT-BAD-999/prostate/TCGA-EJ-7781.svs"
	if _, err := df.FetchBlock(context.Background(), badRef, 0, 4); err == nil {
		t.Fatal("expected 403 error for unauthorized grant")
	}
}
