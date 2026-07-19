package config

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"kiro-go/logger"
)

// ListApiKeys returns a snapshot of all configured API key entries.
func ListApiKeys() []ApiKeyEntry {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return nil
	}
	out := make([]ApiKeyEntry, len(cfg.ApiKeys))
	copy(out, cfg.ApiKeys)
	return out
}

// GetApiKeyEntry returns a copy of the entry with the given ID, or nil if not found.
func GetApiKeyEntry(id string) *ApiKeyEntry {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return nil
	}
	for i := range cfg.ApiKeys {
		if cfg.ApiKeys[i].ID == id {
			cp := cfg.ApiKeys[i]
			return &cp
		}
	}
	return nil
}

// AddApiKey appends a new API key entry. Generates ID and CreatedAt if missing,
// rejects empty Key values, and refuses duplicates of an existing Key.
func AddApiKey(entry ApiKeyEntry) (ApiKeyEntry, error) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return ApiKeyEntry{}, errors.New("config not initialized")
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
	// Sub-card pool invariant, enforced inside the same critical section that
	// appends the key so concurrent creations cannot over-commit the parent's
	// shared pool (the proxy layer's pre-check is advisory only).
	if pid := strings.TrimSpace(entry.ParentKeyID); pid != "" {
		if msg := childGrantErrorLocked(pid, "", entry.CreditsGranted); msg != "" {
			return ApiKeyEntry{}, errors.New(msg)
		}
	}
	if entry.ID == "" {
		entry.ID = newUUID()
	}
	if entry.CreatedAt == 0 {
		entry.CreatedAt = time.Now().Unix()
	}
	cfg.ApiKeys = append(cfg.ApiKeys, entry)
	if err := persistApiKeyLocked(entry); err != nil {
		// Roll back the in-memory append so we don't leave inconsistent state.
		cfg.ApiKeys = cfg.ApiKeys[:len(cfg.ApiKeys)-1]
		return ApiKeyEntry{}, err
	}
	return entry, nil
}

// UpdateApiKey applies a patch to an existing API key. Patch semantics:
//   - Name, Key are overwritten when non-empty in patch.
//   - Enabled, TokenLimit, CreditLimit are always overwritten (zero values are valid).
//   - Counters (TokensUsed/CreditsUsed/RequestsCount) are not touched here; use
//     RecordApiKeyUsage or ResetApiKeyUsage instead.
//   - Migrated stays as-is once true; only flips when explicitly set in patch.
func UpdateApiKey(id string, patch ApiKeyEntry) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	idx := -1
	for i := range cfg.ApiKeys {
		if cfg.ApiKeys[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return errors.New("api key not found")
	}
	if patch.Name != "" {
		newName := strings.TrimSpace(patch.Name)
		// Reject a rename that collides (case-insensitively) with any other entry.
		for j := range cfg.ApiKeys {
			if j != idx && newName != "" && strings.EqualFold(strings.TrimSpace(cfg.ApiKeys[j].Name), newName) {
				return errors.New("api key name already exists")
			}
		}
		cfg.ApiKeys[idx].Name = newName
	}
	if patch.Key != "" {
		newKey := strings.TrimSpace(patch.Key)
		// Reject duplicates against any other entry.
		for j := range cfg.ApiKeys {
			if j != idx && cfg.ApiKeys[j].Key == newKey {
				return errors.New("api key value collides with existing entry")
			}
		}
		cfg.ApiKeys[idx].Key = newKey
	}
	cfg.ApiKeys[idx].Enabled = patch.Enabled
	cfg.ApiKeys[idx].TokenLimit = patch.TokenLimit
	cfg.ApiKeys[idx].CreditLimit = patch.CreditLimit
	// Card-key fields (always overwritten; caller builds patch from existing).
	// CreditsGranted/CreditsUsed are ledger state — never set via UpdateApiKey
	// (use RechargeApiKey / RecordApiKeyUsage / ResetApiKeyUsage).
	cfg.ApiKeys[idx].ExpiresAt = patch.ExpiresAt
	cfg.ApiKeys[idx].MaxConcurrency = patch.MaxConcurrency
	cfg.ApiKeys[idx].BoundAccountIDs = patch.BoundAccountIDs
	cfg.ApiKeys[idx].ParentKeyID = patch.ParentKeyID
	if patch.Migrated {
		cfg.ApiKeys[idx].Migrated = true
	}
	return persistApiKeyLocked(cfg.ApiKeys[idx])
}

// DeleteApiKey removes the API key entry with the given ID. Returns nil even if
// the ID is unknown (idempotent), matching the existing DeleteAccount style.
//
// Sub-card settlement: when the deleted key is a child with real consumption,
// its CreditsUsed is folded into the parent's CreditsUsed (atomically with the
// delete — one transaction in DB mode, one JSON save otherwise). The spend
// already happened upstream, so only the UNUSED remainder of the child's grant
// may flow back to the shared pool. Without this, deleting a fully-consumed
// child "refunded" its whole grant and the pool could be re-sold (double-spend).
func DeleteApiKey(id string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	for i, e := range cfg.ApiKeys {
		if e.ID != id {
			continue
		}
		settleParentID := ""
		settleCredits := 0.0
		if pid := strings.TrimSpace(e.ParentKeyID); pid != "" && e.CreditsUsed > 0 {
			settleParentID = pid
			settleCredits = e.CreditsUsed
		}
		removed := e
		cfg.ApiKeys = append(cfg.ApiKeys[:i], cfg.ApiKeys[i+1:]...)

		// Locate the parent AFTER the removal (indices shifted).
		var parent *ApiKeyEntry
		if settleParentID != "" {
			for j := range cfg.ApiKeys {
				if cfg.ApiKeys[j].ID == settleParentID {
					parent = &cfg.ApiKeys[j]
					break
				}
			}
		}
		if parent == nil {
			if err := persistApiKeyDeleteLocked(id); err != nil {
				cfg.ApiKeys = append(cfg.ApiKeys, removed)
				return err
			}
			return nil
		}

		parent.CreditsUsed += settleCredits
		var err error
		if dbEnabled {
			err = dbDeleteApiKeyWithSettlement(id, parent.ID, settleCredits)
		} else {
			err = saveLocked()
		}
		if err != nil {
			// Restore the in-memory state so it doesn't drift from the store.
			parent.CreditsUsed -= settleCredits
			cfg.ApiKeys = append(cfg.ApiKeys, removed)
			return err
		}
		return nil
	}
	return nil
}

// FindApiKeyByValue returns a copy of the entry whose Key matches the given value,
// or nil if no match. O(n) linear scan.
func FindApiKeyByValue(key string) *ApiKeyEntry {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || key == "" {
		return nil
	}
	for i := range cfg.ApiKeys {
		if subtle.ConstantTimeCompare([]byte(cfg.ApiKeys[i].Key), []byte(key)) == 1 {
			cp := cfg.ApiKeys[i]
			return &cp
		}
	}
	return nil
}

// HasApiKeys returns true when at least one API key entry is configured.
func HasApiKeys() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return len(cfg.ApiKeys) > 0
}

// RecordApiKeyUsage records one billable request against the key. It folds the
// deltas into the in-memory mirror (TokensUsed += inputTokens+outputTokens,
// CreditsUsed += credits, RequestsCount++, LastUsedAt = now) and persists.
//
// When the PostgreSQL backend is active the write goes through the authoritative
// ledger + detail log (db.RecordUsageWithDetail → usage_counters + usage_records)
// plus the per-key mirror (db.TouchAPIKeyUsage), carrying the model through for
// per-model attribution. Otherwise it rewrites the JSON config.
//
// Negative token/credit deltas are clamped to zero so every ledger stays
// monotonic.
func RecordApiKeyUsage(id, model string, inputTokens, outputTokens int64, credits float64) error {
	return recordApiKeyUsage(id, model, inputTokens, outputTokens, 0, 0, credits)
}

// RecordApiKeyUsageWithCache is RecordApiKeyUsage plus prompt-cache token
// attribution (cache read / cache creation input tokens) for the DISPLAY-ONLY
// detail log. Billing totals (usage_counters, credits, tokens) are unaffected —
// only usage_records carries the cache columns, which the usage panels read to
// compute cache hit rate. Negative deltas are clamped to zero.
func RecordApiKeyUsageWithCache(id, model string, inputTokens, outputTokens, cacheReadTokens, cacheCreationTokens int64, credits float64) error {
	return recordApiKeyUsage(id, model, inputTokens, outputTokens, cacheReadTokens, cacheCreationTokens, credits)
}

func recordApiKeyUsage(id, model string, inputTokens, outputTokens, cacheReadTokens, cacheCreationTokens int64, credits float64) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	// Clamp negative deltas up front so the in-memory mirror and the DB ledgers
	// agree and neither can roll backward.
	if inputTokens < 0 {
		inputTokens = 0
	}
	if outputTokens < 0 {
		outputTokens = 0
	}
	if cacheReadTokens < 0 {
		cacheReadTokens = 0
	}
	if cacheCreationTokens < 0 {
		cacheCreationTokens = 0
	}
	if credits < 0 {
		credits = 0
	}
	totalTokens := inputTokens + outputTokens
	for i := range cfg.ApiKeys {
		if cfg.ApiKeys[i].ID == id {
			if totalTokens > 0 {
				cfg.ApiKeys[i].TokensUsed += totalTokens
			}
			if credits > 0 {
				usedBefore := cfg.ApiKeys[i].CreditsUsed
				cfg.ApiKeys[i].CreditsUsed += credits
				// Overdraft alert: the balance check happens before the request
				// while the real cost lands here at stream end, so concurrent
				// requests can push a card past its grant. Log the crossing once
				// (no clawback — accepted conservative behavior; the low-balance
				// serialization gate in the proxy layer bounds the exposure).
				if g := cfg.ApiKeys[i].CreditsGranted; g > 0 && usedBefore <= g && cfg.ApiKeys[i].CreditsUsed > g {
					logger.Warnf("[Billing] api key %s (%s) overdrafted: used %.2f > granted %.2f (overdraft %.2f credits)",
						cfg.ApiKeys[i].ID, cfg.ApiKeys[i].Name, cfg.ApiKeys[i].CreditsUsed, g, cfg.ApiKeys[i].CreditsUsed-g)
				}
			}
			cfg.ApiKeys[i].RequestsCount++
			cfg.ApiKeys[i].LastUsedAt = time.Now().Unix()
			if dbEnabled {
				// Hot path: fold into the authoritative monotonic ledger + detail
				// log + per-key mirror directly (no full JSON rewrite).
				return dbRecordApiKeyUsage(id, model, inputTokens, outputTokens, cacheReadTokens, cacheCreationTokens, credits, cfg.ApiKeys[i].LastUsedAt)
			}
			return saveLocked()
		}
	}
	return errors.New("api key not found")
}

// RechargeApiKey credits `amount` onto the key's unified ledger (CreditsGranted),
// growing its spendable balance (CreditsGranted - CreditsUsed).
//
// When the PostgreSQL backend is active it delegates to db.RechargeAPIKey, which
// atomically increments credits_granted and appends a recharge_records row, then
// writes the returned running total back into the in-memory mirror. Otherwise it
// bumps the in-memory CreditsGranted and persists the JSON config.
//
// amount must be strictly positive and the id must exist; both are errors.
func RechargeApiKey(id string, amount float64, operator, note string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	if amount <= 0 {
		return errors.New("recharge amount must be positive")
	}
	for i := range cfg.ApiKeys {
		if cfg.ApiKeys[i].ID == id {
			// A sub-card's top-up draws from its parent's shared pool: the new
			// grant total must fit the allocatable remainder. Checked in the
			// same critical section as the write (no check-then-act window).
			if pid := strings.TrimSpace(cfg.ApiKeys[i].ParentKeyID); pid != "" {
				if msg := childGrantErrorLocked(pid, id, cfg.ApiKeys[i].CreditsGranted+amount); msg != "" {
					return errors.New(msg)
				}
			}
			if dbEnabled {
				balanceAfter, err := dbRechargeApiKey(id, amount, operator, note)
				if err != nil {
					return err
				}
				// balance_after is the authoritative post-recharge grant total.
				cfg.ApiKeys[i].CreditsGranted = balanceAfter
				return nil
			}
			cfg.ApiKeys[i].CreditsGranted += amount
			return saveLocked()
		}
	}
	return errors.New("api key not found")
}

// SetApiKeyGrant sets the key's total credit grant (CreditsGranted) to an
// absolute value. This is the admin "set quota / 额度" operation, distinct from
// RechargeApiKey which *adds* to the running grant. The card's displayed balance
// and its quota enforcement (ApiKeyOverLimit) are both driven by CreditsGranted,
// so this is how an operator dials a card's spendable ceiling up or down.
//
// It writes the credits_granted mirror through the normal full-row persist path
// (DB upsert when Postgres is active, JSON otherwise). The recharge_records audit
// trail is intentionally left untouched: a manual quota edit is not a top-up
// event. Negative totals are clamped to zero. The id must exist.
func SetApiKeyGrant(id string, total float64) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	if total < 0 {
		total = 0
	}
	for i := range cfg.ApiKeys {
		if cfg.ApiKeys[i].ID == id {
			cfg.ApiKeys[i].CreditsGranted = total
			return persistApiKeyLocked(cfg.ApiKeys[i])
		}
	}
	return errors.New("api key not found")
}

// GetApiKeyBalanceByID returns the key's unified-ledger balance: granted
// (CreditsGranted), used (CreditsUsed) and balance (granted - used). When the
// PostgreSQL backend is active it reads the authoritative api_keys mirror via
// db.GetKeyBalance; otherwise it computes from the in-memory entry. ok is false
// when the id is unknown (or, in DB mode, when the read fails).
func GetApiKeyBalanceByID(id string) (granted, used, balance float64, ok bool) {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return 0, 0, 0, false
	}
	if dbEnabled {
		g, u, b, err := dbGetApiKeyBalance(id)
		if err != nil {
			return 0, 0, 0, false
		}
		return g, u, b, true
	}
	for i := range cfg.ApiKeys {
		if cfg.ApiKeys[i].ID == id {
			g := cfg.ApiKeys[i].CreditsGranted
			u := cfg.ApiKeys[i].CreditsUsed
			return g, u, g - u, true
		}
	}
	return 0, 0, 0, false
}

// ResetApiKeyUsage clears TokensUsed/CreditsUsed/RequestsCount for the entry.
// LastUsedAt is preserved so operators can still see when the key was last used.
func ResetApiKeyUsage(id string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	for i := range cfg.ApiKeys {
		if cfg.ApiKeys[i].ID == id {
			cfg.ApiKeys[i].TokensUsed = 0
			cfg.ApiKeys[i].CreditsUsed = 0
			cfg.ApiKeys[i].RequestsCount = 0
			if dbEnabled {
				return dbResetApiKeyUsage(id)
			}
			return saveLocked()
		}
	}
	return errors.New("api key not found")
}

// GenerateApiKeyValue returns a new random 32-byte hex API key prefixed with "sk-".
func GenerateApiKeyValue() string {
	buf := make([]byte, 32)
	_, _ = rand.Read(buf)
	return "sk-" + hex.EncodeToString(buf)
}

// MaskApiKey produces a display-friendly masked version: keeps first 6 and last 4
// characters, replaces the middle with "****". Returns "" for empty input and
// the original string if it's too short to mask meaningfully.
func MaskApiKey(key string) string {
	if key == "" {
		return ""
	}
	if len(key) <= 10 {
		return key
	}
	return key[:6] + "****" + key[len(key)-4:]
}

// IsApiKeyExpired reports whether the key has a positive ExpiresAt that lies in
// the past. Keys with ExpiresAt == 0 never expire. Does not lock; pass a copy.
func IsApiKeyExpired(e ApiKeyEntry) bool {
	return e.ExpiresAt > 0 && time.Now().Unix() > e.ExpiresAt
}

// ApiKeyOverLimit returns (overToken, overCredit) for the entry under the unified
// balance model, staying backward compatible with the legacy fixed-limit fields.
// Limits/grants with value 0 are ignored. The function does not lock; callers
// should pass a copied entry.
//
// Token: over when TokenLimit > 0 && TokensUsed >= TokenLimit (unchanged).
//
// Credit (in priority order):
//   - An expired key (see IsApiKeyExpired) is always reported over on credit, so
//     it stops serving regardless of remaining balance.
//   - Unified balance model: if CreditsGranted > 0, over when the balance is
//     exhausted, i.e. CreditsUsed >= CreditsGranted.
//   - Legacy fallback: else if CreditLimit > 0, over when CreditsUsed >= CreditLimit.
//   - Otherwise (no grant, no limit, not expired): never over on credit.
func ApiKeyOverLimit(e ApiKeyEntry) (overToken bool, overCredit bool) {
	if e.TokenLimit > 0 && e.TokensUsed >= e.TokenLimit {
		overToken = true
	}

	switch {
	case IsApiKeyExpired(e):
		overCredit = true
	case e.CreditsGranted > 0:
		overCredit = e.CreditsUsed >= e.CreditsGranted
	case e.CreditLimit > 0:
		overCredit = e.CreditsUsed >= e.CreditLimit
	}
	return
}
