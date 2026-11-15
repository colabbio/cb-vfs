package vfs

import (
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// CohortGrantRule stores the DPO-approved paths/patient prefixes for a single grantId.
type CohortGrantRule struct {
	GrantID        string    `json:"grantId"`
	AllowedPaths   []string  `json:"allowedPaths"`
	AllowedPatients []string `json:"allowedPatients"`
	ExpiresAt      time.Time `json:"expiresAt"`
	Revoked        bool      `json:"revoked"`
	RevokeReason   string    `json:"revokeReason,omitempty"`
}

// CohortAclRegistry enforces Cohort Security Boundaries (CB-SPEC-COHORT-ACL-019)
// inside MedVFS FUSE and HTTP serve layers in real time.
type CohortAclRegistry struct {
	mu     sync.RWMutex
	grants map[string]*CohortGrantRule
}

// NewCohortAclRegistry creates an empty CohortAclRegistry.
func NewCohortAclRegistry() *CohortAclRegistry {
	return &CohortAclRegistry{
		grants: make(map[string]*CohortGrantRule),
	}
}

// SyncGrant registers or updates a DPO-approved Cohort Grant.
func (r *CohortAclRegistry) SyncGrant(rule CohortGrantRule) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := rule
	r.grants[rule.GrantID] = &cp
}

// RevokeGrant executes an immediate hot Kill-Switch for the given grantId.
func (r *CohortAclRegistry) RevokeGrant(grantID, reason string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	rule, ok := r.grants[grantID]
	if !ok {
		r.grants[grantID] = &CohortGrantRule{
			GrantID:      grantID,
			Revoked:      true,
			RevokeReason: reason,
		}
		return false
	}
	rule.Revoked = true
	rule.RevokeReason = reason
	return true
}

// IsAllowed checks whether resourcePath is authorized under grantID.
// Fails closed if the grant is missing, revoked, or expired.
func (r *CohortAclRegistry) IsAllowed(grantID, resourcePath string) bool {
	if grantID == "" || resourcePath == "" {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	rule, ok := r.grants[grantID]
	if !ok || rule.Revoked {
		return false
	}
	if !rule.ExpiresAt.IsZero() && time.Now().After(rule.ExpiresAt) {
		return false
	}

	clean := filepath.ToSlash(filepath.Clean(resourcePath))
	for _, prefix := range rule.AllowedPaths {
		p := filepath.ToSlash(filepath.Clean(prefix))
		if clean == p || strings.HasPrefix(clean, strings.TrimSuffix(p, "/")+"/") {
			return true
		}
	}
	for _, pat := range rule.AllowedPatients {
		if pat != "" && strings.Contains(clean, pat) {
			return true
		}
	}
	return false
}

// Snapshot returns a copy of all active/revoked grant rules.
func (r *CohortAclRegistry) Snapshot() []CohortGrantRule {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]CohortGrantRule, 0, len(r.grants))
	for _, g := range r.grants {
		out = append(out, *g)
	}
	return out
}
