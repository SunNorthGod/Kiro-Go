package config

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// apikeys_pool.go implements the shared-pool ("额度池") accounting for
// parent/child card keys, with every check-and-mutate running inside a single
// cfgLock critical section so concurrent operations can never over-commit the
// pool (fixes the check-then-act TOCTOU in the previous proxy-layer checks).
//
// Model: a parent card's budget (CreditsGranted) is shared between the parent's
// own consumption and everything committed to its sub-cards. A live sub-card
// commits max(grant, real spend) — an overdrafted child holds its full spend.
// Deleting a sub-card settles its consumed credits into the parent's
// CreditsUsed (irrecoverable spend, see DeleteApiKey); only the UNUSED part of
// its grant flows back to the pool. This closes the "delete a fully-consumed
// child to reclaim its full grant" double-spend hole.

// AllocEpsilon absorbs float rounding so a grant that exactly equals the
// allocatable pool is accepted rather than tripping a > comparison.
const AllocEpsilon = 1e-6

// childCommittedLocked is what a live child holds against the parent pool:
// its grant, or its real spend when it overdrafted past the grant.
func childCommittedLocked(e *ApiKeyEntry) float64 {
	if e.CreditsUsed > e.CreditsGranted {
		return e.CreditsUsed
	}
	return e.CreditsGranted
}

// allocatableChildCreditsLocked reports how much of a parent's budget can still
// be handed to sub-cards:
//
//	CreditsGranted − parent's own CreditsUsed − Σ max(child grant, child spend)
//
// Note the parent's CreditsUsed already includes credits settled from deleted
// sub-cards (see DeleteApiKey), so consumed-then-deleted children stay deducted
// forever. excludeChildID is skipped from the children sum (used when resizing
// an existing child). Caller holds cfgLock (read or write).
func allocatableChildCreditsLocked(parentID, excludeChildID string) float64 {
	var parent *ApiKeyEntry
	for i := range cfg.ApiKeys {
		if cfg.ApiKeys[i].ID == parentID {
			parent = &cfg.ApiKeys[i]
			break
		}
	}
	if parent == nil || parent.CreditsGranted <= 0 {
		return 0
	}
	committed := 0.0
	for i := range cfg.ApiKeys {
		e := &cfg.ApiKeys[i]
		if e.ID != excludeChildID && e.ParentKeyID == parentID {
			committed += childCommittedLocked(e)
		}
	}
	free := parent.CreditsGranted - parent.CreditsUsed - committed
	if free < 0 {
		free = 0
	}
	return free
}

// AllocatableChildCredits is the exported read-only view of the allocatable
// pool (display / advisory pre-checks). The authoritative check always re-runs
// inside the mutating call under the same lock.
func AllocatableChildCredits(parentID, excludeChildID string) float64 {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return 0
	}
	return allocatableChildCreditsLocked(parentID, excludeChildID)
}

// childGrantErrorLocked validates a sub-card's grant against its parent pool.
// selfID is "" on create. Returns "" when valid. Caller holds cfgLock.
func childGrantErrorLocked(parentID, selfID string, grant float64) string {
	var parent *ApiKeyEntry
	for i := range cfg.ApiKeys {
		if cfg.ApiKeys[i].ID == parentID {
			parent = &cfg.ApiKeys[i]
			break
		}
	}
	if parent == nil {
		return "parent key not found"
	}
	if strings.TrimSpace(parent.ParentKeyID) != "" {
		return "cannot nest sub-cards more than one level"
	}
	if grant > 0 && parent.CreditsGranted > 0 {
		free := allocatableChildCreditsLocked(parentID, selfID)
		if grant > free+AllocEpsilon {
			return fmt.Sprintf(
				"sub-card grant exceeds parent pool: %.2f allocatable, requested %.2f",
				free, grant)
		}
	}
	return ""
}

// ChildGrantError is the exported advisory wrapper for childGrantErrorLocked.
func ChildGrantError(parentID, selfID string, grant float64) string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return "config not initialized"
	}
	return childGrantErrorLocked(parentID, selfID, grant)
}

// parentGrantErrorLocked guards the reverse invariant: a parent's grant may not
// drop below what is already committed against it (own spend + Σ per-child
// max(grant, spend)). Returns "" when valid (including no children).
// Caller holds cfgLock.
func parentGrantErrorLocked(selfID string, grant float64) string {
	committed := 0.0
	hasChildren := false
	for i := range cfg.ApiKeys {
		e := &cfg.ApiKeys[i]
		if e.ParentKeyID == selfID {
			committed += childCommittedLocked(e)
			hasChildren = true
		}
	}
	if !hasChildren {
		return ""
	}
	for i := range cfg.ApiKeys {
		if cfg.ApiKeys[i].ID == selfID {
			committed += cfg.ApiKeys[i].CreditsUsed
			break
		}
	}
	if grant+AllocEpsilon < committed {
		return fmt.Sprintf("quota %.2f is below the %.2f already committed (own usage + sub-card allocations)", grant, committed)
	}
	return ""
}

// ParentGrantError is the exported advisory wrapper for parentGrantErrorLocked.
func ParentGrantError(selfID string, grant float64) string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return "config not initialized"
	}
	return parentGrantErrorLocked(selfID, grant)
}

// SetApiKeyGrantChecked is SetApiKeyGrant with the shared-pool invariants
// enforced atomically in the same critical section as the write: a sub-card's
// new grant must cover its own spend and fit the parent's allocatable pool; a
// parent's new grant must cover everything already committed against it.
func SetApiKeyGrantChecked(id string, total float64) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	if total < 0 {
		total = 0
	}
	for i := range cfg.ApiKeys {
		if cfg.ApiKeys[i].ID != id {
			continue
		}
		if pid := strings.TrimSpace(cfg.ApiKeys[i].ParentKeyID); pid != "" {
			if total+AllocEpsilon < cfg.ApiKeys[i].CreditsUsed {
				return fmt.Errorf("new grant %.2f is below the sub-card's consumed %.2f credits", total, cfg.ApiKeys[i].CreditsUsed)
			}
			if msg := childGrantErrorLocked(pid, id, total); msg != "" {
				return errors.New(msg)
			}
		} else if msg := parentGrantErrorLocked(id, total); msg != "" {
			return errors.New(msg)
		}
		cfg.ApiKeys[i].CreditsGranted = total
		return persistApiKeyLocked(cfg.ApiKeys[i])
	}
	return errors.New("api key not found")
}

// CreateChildApiKey atomically validates the parent pool, creates the sub-card
// with its opening grant already applied, and (DB mode) writes the opening
// recharge audit row in the same transaction. Doing all of it under one lock —
// instead of the old AddApiKey + RechargeApiKey two-step — removes the window
// where a half-created child (grant still 0) was invisible to concurrent pool
// checks, letting resellers over-commit the pool with parallel creations.
func CreateChildApiKey(entry ApiKeyEntry, grant float64, operator, note string) (ApiKeyEntry, error) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return ApiKeyEntry{}, errors.New("config not initialized")
	}
	if grant <= 0 {
		return ApiKeyEntry{}, errors.New("sub-card grant must be positive")
	}
	pid := strings.TrimSpace(entry.ParentKeyID)
	if pid == "" {
		return ApiKeyEntry{}, errors.New("sub-card must reference a parent key")
	}
	entry.Key = strings.TrimSpace(entry.Key)
	if entry.Key == "" {
		return ApiKeyEntry{}, errors.New("api key value must not be empty")
	}
	entry.Name = strings.TrimSpace(entry.Name)
	for _, existing := range cfg.ApiKeys {
		if existing.Key == entry.Key {
			return ApiKeyEntry{}, errors.New("api key already exists")
		}
		if entry.Name != "" && strings.EqualFold(strings.TrimSpace(existing.Name), entry.Name) {
			return ApiKeyEntry{}, errors.New("api key name already exists")
		}
	}
	if msg := childGrantErrorLocked(pid, "", grant); msg != "" {
		return ApiKeyEntry{}, errors.New(msg)
	}
	if entry.ID == "" {
		entry.ID = newUUID()
	}
	if entry.CreatedAt == 0 {
		entry.CreatedAt = time.Now().Unix()
	}
	entry.CreditsGranted = grant
	cfg.ApiKeys = append(cfg.ApiKeys, entry)
	var err error
	if dbEnabled {
		err = dbInsertApiKeyWithOpeningGrant(entry, operator, note)
	} else {
		err = saveLocked()
	}
	if err != nil {
		cfg.ApiKeys = cfg.ApiKeys[:len(cfg.ApiKeys)-1]
		return ApiKeyEntry{}, err
	}
	return entry, nil
}
