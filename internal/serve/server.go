package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/colabbio/medvfs/internal/vfs"
	"go.uber.org/zap"
)

// Server provides the HTTP API for MedVFS serve mode.
// Endpoints:
//
//	GET  /health                  → 200 OK
//	GET  /manifest                → JSON list of all file mappings
//	GET  /stats                   → JSON cache statistics
//	GET  /events                  → SSE stream of live events
//	POST /internal/v1/acl/sync    → Push CohortGrantRule from cb-svc-ai
//	POST /internal/v1/acl/revoke  → Execute hot Kill-Switch on grantId
//	GET  /internal/v1/acl/status  → Snapshot of active/revoked cohort ACLs
type Server struct {
	port   int
	engine *Engine
	acl    *vfs.CohortAclRegistry
	log    *zap.Logger
}

// NewServer creates a new HTTP server for the serve engine.
func NewServer(port int, engine *Engine, log *zap.Logger) *Server {
	return &Server{port: port, engine: engine, acl: vfs.NewCohortAclRegistry(), log: log}
}

// Start begins serving HTTP requests. It blocks until ctx is cancelled.
func (s *Server) Start(ctx context.Context) {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/manifest", s.handleManifest)
	mux.HandleFunc("/dataset", s.handleDataset)
	mux.HandleFunc("/stats", s.handleStats)
	mux.HandleFunc("/events", s.handleEvents)
	mux.HandleFunc("/internal/v1/acl/sync", s.handleAclSync)
	mux.HandleFunc("/internal/v1/acl/revoke", s.handleAclRevoke)
	mux.HandleFunc("/internal/v1/acl/status", s.handleAclStatus)

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", s.port),
		Handler: mux,
	}

	go func() {
		<-ctx.Done()
		srv.Close()
	}()

	s.log.Info("HTTP server starting", zap.Int("port", s.port))
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		s.log.Error("HTTP server error", zap.Error(err))
	}
}

// handleHealth returns a simple health check response.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleManifest returns the complete file mapping as JSON.
func (s *Server) handleManifest(w http.ResponseWriter, r *http.Request) {
	sourceID := r.URL.Query().Get("source")
	mappings := s.engine.GetMappings(sourceID)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"files": mappings,
		"count": len(mappings),
	})
}

// handleStats returns cache statistics as JSON.
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	stats := s.engine.GetStats()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"cache":    stats,
		"hit_rate": fmt.Sprintf("%.1f%%", stats.HitRate()),
	})
}

// handleEvents streams live events via Server-Sent Events (SSE).
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "SSE not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case event := <-s.engine.liveEvents:
			data, err := json.Marshal(event)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Type, string(data))
			flusher.Flush()
		}
	}
}

// handleDataset returns the dataset versioning manifest as JSON.
func (s *Server) handleDataset(w http.ResponseWriter, r *http.Request) {
	manifest := s.engine.GetManifest()
	if manifest == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{
			"status": "versioning_disabled",
			"hint":   "add dataset.versioning.enabled: true to vfs.yaml",
		})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(manifest)
}

func (s *Server) handleAclSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var rule vfs.CohortGrantRule
	if err := json.NewDecoder(r.Body).Decode(&rule); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.acl.SyncGrant(rule)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "synced", "grantId": rule.GrantID})
}

func (s *Server) handleAclRevoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		GrantID string `json:"grantId"`
		Reason  string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.acl.RevokeGrant(req.GrantID, req.Reason)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "revoked", "grantId": req.GrantID})
}

func (s *Server) handleAclStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"grants": s.acl.Snapshot(),
	})
}
