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
	mu        sync.RWMutex
	grants    map[string]*CohortGrantRule
	onRevoke  []func(rule CohortGrantRule)
}

// NewCohortAclRegistry creates an empty CohortAclRegistry.
func NewCohortAclRegistry() *CohortAclRegistry {
	return &CohortAclRegistry{
		grants: make(map[string]*CohortGrantRule),
	}
}

// RegisterRevokeListener registers a callback invoked asynchronously/unlocked
// whenever a grant is revoked via the hot Kill-Switch.
func (r *CohortAclRegistry) RegisterRevokeListener(fn func(rule CohortGrantRule)) {
	if fn == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onRevoke = append(r.onRevoke, fn)
}

// SyncGrant registers or updates a DPO-approved Cohort Grant.
func (r *CohortAclRegistry) SyncGrant(rule CohortGrantRule) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := rule
	r.grants[rule.GrantID] = &cp
}

// RevokeGrant executes an immediate hot Kill-Switch for the given grantId
// and notifies all registered eviction listeners outside the registry lock.
func (r *CohortAclRegistry) RevokeGrant(grantID, reason string) bool {
	r.mu.Lock()
	var snapshot CohortGrantRule
	var listeners []func(rule CohortGrantRule)
	rule, ok := r.grants[grantID]
	if !ok {
		snapshot = CohortGrantRule{
			GrantID:      grantID,
			Revoked:      true,
			RevokeReason: reason,
		}
		r.grants[grantID] = &snapshot
	} else {
		rule.Revoked = true
		rule.RevokeReason = reason
		snapshot = *rule
	}
	listeners = append(listeners, r.onRevoke...)
	r.mu.Unlock()

	for _, fn := range listeners {
		fn(snapshot)
	}
	return ok
}

// RuleMatchesPath checks if resourcePath matches the allowed paths or patient IDs of a rule.
func RuleMatchesPath(rule CohortGrantRule, resourcePath string) bool {
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

	return RuleMatchesPath(*rule, resourcePath)
}

// GetGrant returns the rule for grantID if present and active.
func (r *CohortAclRegistry) GetGrant(grantID string) (CohortGrantRule, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rule, ok := r.grants[grantID]
	if !ok {
		return CohortGrantRule{}, false
	}
	return *rule, true
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
