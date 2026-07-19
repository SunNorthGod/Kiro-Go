package proxy

import (
	"encoding/json"
	"fmt"
	"kiro-go/config"
	"net/http"
	"strings"
)

// apiKeyView is the response payload for listing/inspecting API keys. This is an
// admin-only, password-protected view, so the full Key is returned (KeyMasked is
// kept for compact display): operators need to copy the card key + base URL to
// hand to customers. RPM is the key's real-time requests-in-the-last-60s.
type apiKeyView struct {
	ID            string  `json:"id"`
	Name          string  `json:"name,omitempty"`
	Key           string  `json:"key"`
	KeyMasked     string  `json:"keyMasked"`
	RPM           int     `json:"rpm"`
	Enabled       bool    `json:"enabled"`
	Migrated      bool    `json:"migrated,omitempty"`
	CreatedAt     int64   `json:"createdAt"`
	LastUsedAt    int64   `json:"lastUsedAt,omitempty"`
	TokenLimit    int64   `json:"tokenLimit,omitempty"`
	CreditLimit   float64 `json:"creditLimit,omitempty"`
	TokensUsed    int64   `json:"tokensUsed"`
	CreditsUsed   float64 `json:"creditsUsed"`
	RequestsCount int64   `json:"requestsCount"`

	// Unified-ledger + card-key fields.
	CreditsGranted  float64  `json:"creditsGranted"`
	Balance         float64  `json:"balance"`
	ExpiresAt       int64    `json:"expiresAt,omitempty"`
	Expired         bool     `json:"expired"`
	MaxConcurrency  int      `json:"maxConcurrency,omitempty"`
	BoundAccountIDs []string `json:"boundAccountIds,omitempty"`
	ParentKeyID     string   `json:"parentKeyId,omitempty"`
}

func toApiKeyView(e config.ApiKeyEntry) apiKeyView {
	return apiKeyView{
		ID:              e.ID,
		Name:            e.Name,
		Key:             e.Key,
		KeyMasked:       config.MaskApiKey(e.Key),
		Enabled:         e.Enabled,
		Migrated:        e.Migrated,
		CreatedAt:       e.CreatedAt,
		LastUsedAt:      e.LastUsedAt,
		TokenLimit:      e.TokenLimit,
		CreditLimit:     e.CreditLimit,
		TokensUsed:      e.TokensUsed,
		CreditsUsed:     e.CreditsUsed,
		RequestsCount:   e.RequestsCount,
		CreditsGranted:  e.CreditsGranted,
		Balance:         e.CreditsGranted - e.CreditsUsed,
		ExpiresAt:       e.ExpiresAt,
		Expired:         config.IsApiKeyExpired(e),
		MaxConcurrency:  e.MaxConcurrency,
		BoundAccountIDs: e.BoundAccountIDs,
		ParentKeyID:     e.ParentKeyID,
	}
}

func (h *Handler) apiListApiKeys(w http.ResponseWriter, r *http.Request) {
	entries := config.ListApiKeys()
	out := make([]apiKeyView, len(entries))
	for i, e := range entries {
		out[i] = toApiKeyView(e)
		if h.pool != nil {
			out[i].RPM = h.pool.KeyRPM(e.ID)
		}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"apiKeys": out})
}

func (h *Handler) apiGetApiKey(w http.ResponseWriter, r *http.Request, id string) {
	entry := config.GetApiKeyEntry(id)
	if entry == nil {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "API key not found"})
		return
	}
	view := toApiKeyView(*entry)
	if h.pool != nil {
		view.RPM = h.pool.KeyRPM(entry.ID)
	}
	json.NewEncoder(w).Encode(view)
}

type apiKeyCreateRequest struct {
	Name            string   `json:"name,omitempty"`
	Key             string   `json:"key,omitempty"`
	Enabled         *bool    `json:"enabled,omitempty"`
	TokenLimit      int64    `json:"tokenLimit,omitempty"`
	CreditLimit     float64  `json:"creditLimit,omitempty"`
	CreditsGranted  float64  `json:"creditsGranted,omitempty"` // optional opening balance
	ExpiresAt       int64    `json:"expiresAt,omitempty"`
	MaxConcurrency  int      `json:"maxConcurrency,omitempty"`
	BoundAccountIDs []string `json:"boundAccountIds,omitempty"`
	ParentKeyID     string   `json:"parentKeyId,omitempty"`
}

// allocEpsilon absorbs float rounding so a grant that exactly equals the
// allocatable pool is accepted rather than tripping a > comparison.
const allocEpsilon = 1e-6

// canManageSubKeys reports whether a card may open/manage sub-cards. Mirrors the
// Rust reseller rule (credit_limit set && parent_key_id none): the card must be a
// budgeted card (positive grant) and must not itself be a sub-card (one level of
// nesting only).
func canManageSubKeys(e *config.ApiKeyEntry) bool {
	return e != nil && e.CreditsGranted > 0 && strings.TrimSpace(e.ParentKeyID) == ""
}

// allocatableCredits reports how much of a parent card's budget can still be
// handed to sub-cards under the shared-pool model:
//
//	CreditsGranted (budget) − parent's own CreditsUsed − Σ(live children grants)
//
// The parent's own consumption and every child's allocation draw from the same
// budget, so no allocation may push total commitments past the budget.
// excludeChildID is skipped from the children sum (used when resizing an existing
// child). Returns 0 for an unknown parent or one without a positive budget.
func allocatableCredits(parentID, excludeChildID string) float64 {
	parent := config.GetApiKeyEntry(parentID)
	if parent == nil || parent.CreditsGranted <= 0 {
		return 0
	}
	allocated := 0.0
	for _, e := range config.ListApiKeys() {
		if e.ID != excludeChildID && e.ParentKeyID == parentID {
			allocated += e.CreditsGranted
		}
	}
	free := parent.CreditsGranted - parent.CreditsUsed - allocated
	if free < 0 {
		free = 0
	}
	return free
}

// childGrantError validates a sub-card's credit grant against its parent's pool.
// The parent must exist, nesting is limited to one level, and the proposed grant
// must fit within the parent's allocatable budget (see allocatableCredits) so a
// shared team pool can never be over-committed. selfID is "" on create (no key to
// exclude yet). Returns "" when valid, or a human-readable reason to reject.
func childGrantError(parentID, selfID string, grant float64) string {
	parent := config.GetApiKeyEntry(parentID)
	if parent == nil {
		return "parent key not found"
	}
	if strings.TrimSpace(parent.ParentKeyID) != "" {
		return "cannot nest sub-cards more than one level"
	}
	if grant > 0 && parent.CreditsGranted > 0 {
		free := allocatableCredits(parentID, selfID)
		if grant > free+allocEpsilon {
			return fmt.Sprintf(
				"sub-card grant exceeds parent pool: %.2f allocatable, requested %.2f",
				free, grant)
		}
	}
	return ""
}

// parentGrantError guards the reverse sub-card invariant: a parent key's grant
// may not be lowered below what is already committed against it — the parent's
// own usage plus everything handed to its children — which would over-commit the
// shared pool. selfID is the parent being edited. Returns "" when valid
// (including when the key has no children).
func parentGrantError(selfID string, grant float64) string {
	allocated := 0.0
	for _, e := range config.ListApiKeys() {
		if e.ParentKeyID == selfID {
			allocated += e.CreditsGranted
		}
	}
	if allocated <= 0 {
		return ""
	}
	committed := allocated
	if self := config.GetApiKeyEntry(selfID); self != nil {
		committed += self.CreditsUsed
	}
	if grant+allocEpsilon < committed {
		return fmt.Sprintf("quota %.2f is below the %.2f already committed (own usage + sub-card allocations)", grant, committed)
	}
	return ""
}

func (h *Handler) apiCreateApiKey(w http.ResponseWriter, r *http.Request) {
	var req apiKeyCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	keyValue := req.Key
	if keyValue == "" {
		keyValue = config.GenerateApiKeyValue()
	}

	// The create form's "额度" arrives as CreditLimit, but the unified ledger
	// (CreditsGranted) is what drives both the card's displayed balance and its
	// quota enforcement — ApiKeyOverLimit prefers a positive grant and treats
	// CreditLimit only as a legacy fallback. Seed the opening grant from the limit
	// when the client didn't send an explicit grant, so a freshly created card
	// shows (and enforces) its quota instead of silently reading as "unlimited".
	if req.CreditsGranted <= 0 && req.CreditLimit > 0 {
		req.CreditsGranted = req.CreditLimit
	}

	// Sub-card (child key) validation: parent exists, one-level nesting, and the
	// child's grant fits inside the parent's un-allocated pool.
	if pid := strings.TrimSpace(req.ParentKeyID); pid != "" {
		if msg := childGrantError(pid, "", req.CreditsGranted); msg != "" {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": msg})
			return
		}
	}

	entry, err := config.AddApiKey(config.ApiKeyEntry{
		Name:            req.Name,
		Key:             keyValue,
		Enabled:         enabled,
		TokenLimit:      req.TokenLimit,
		CreditLimit:     req.CreditLimit,
		CreditsGranted:  req.CreditsGranted,
		ExpiresAt:       req.ExpiresAt,
		MaxConcurrency:  req.MaxConcurrency,
		BoundAccountIDs: req.BoundAccountIDs,
		ParentKeyID:     req.ParentKeyID,
	})
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// Return the cleartext key exactly once on creation so the operator can copy it.
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"id":      entry.ID,
		"key":     entry.Key,
		"apiKey":  toApiKeyView(entry),
	})
}

type apiKeyUpdateRequest struct {
	Name            *string   `json:"name,omitempty"`
	Key             *string   `json:"key,omitempty"`
	Enabled         *bool     `json:"enabled,omitempty"`
	TokenLimit      *int64    `json:"tokenLimit,omitempty"`
	CreditLimit     *float64  `json:"creditLimit,omitempty"`
	CreditsGranted  *float64  `json:"creditsGranted,omitempty"` // absolute quota override ("额度")
	ExpiresAt       *int64    `json:"expiresAt,omitempty"`
	MaxConcurrency  *int      `json:"maxConcurrency,omitempty"`
	BoundAccountIDs *[]string `json:"boundAccountIds,omitempty"`
	ParentKeyID     *string   `json:"parentKeyId,omitempty"`
}

func (h *Handler) apiUpdateApiKey(w http.ResponseWriter, r *http.Request, id string) {
	existing := config.GetApiKeyEntry(id)
	if existing == nil {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "API key not found"})
		return
	}

	var req apiKeyUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	patch := *existing
	if req.Name != nil {
		patch.Name = *req.Name
	}
	if req.Key != nil {
		patch.Key = *req.Key
	}
	if req.Enabled != nil {
		patch.Enabled = *req.Enabled
	}
	if req.TokenLimit != nil {
		patch.TokenLimit = *req.TokenLimit
	}
	if req.CreditLimit != nil {
		patch.CreditLimit = *req.CreditLimit
	}
	if req.ExpiresAt != nil {
		patch.ExpiresAt = *req.ExpiresAt
	}
	if req.MaxConcurrency != nil {
		patch.MaxConcurrency = *req.MaxConcurrency
	}
	if req.BoundAccountIDs != nil {
		patch.BoundAccountIDs = *req.BoundAccountIDs
	}
	if req.ParentKeyID != nil {
		patch.ParentKeyID = *req.ParentKeyID
	}

	// If the operator is (re)setting the card's quota ("额度" -> CreditsGranted),
	// validate the sub-card pool invariant against the *post-patch* parent up
	// front, so a rejection leaves the key completely untouched. UpdateApiKey does
	// not touch the grant (it's ledger state); the absolute set happens after via
	// SetApiKeyGrant.
	if req.CreditsGranted != nil {
		if pid := strings.TrimSpace(patch.ParentKeyID); pid != "" {
			// Editing a sub-card: its grant must still fit the parent's pool.
			if msg := childGrantError(pid, id, *req.CreditsGranted); msg != "" {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]string{"error": msg})
				return
			}
		} else if msg := parentGrantError(id, *req.CreditsGranted); msg != "" {
			// Editing a parent (or standalone) card: don't drop below what its
			// children already hold.
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": msg})
			return
		}
	}

	if err := config.UpdateApiKey(id, patch); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// Apply the absolute quota after the metadata patch so the displayed balance
	// and enforcement track the "额度" field. Recharges (RechargeApiKey) still add
	// on top of this base; a subsequent edit re-sets the base to the shown value.
	if req.CreditsGranted != nil {
		if err := config.SetApiKeyGrant(id, *req.CreditsGranted); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	}

	updated := config.GetApiKeyEntry(id)
	if updated == nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "failed to reload entry"})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"apiKey":  toApiKeyView(*updated),
	})
}

func (h *Handler) apiDeleteApiKey(w http.ResponseWriter, r *http.Request, id string) {
	if err := config.DeleteApiKey(id); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func (h *Handler) apiResetApiKeyUsage(w http.ResponseWriter, r *http.Request, id string) {
	if err := config.ResetApiKeyUsage(id); err != nil {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	updated := config.GetApiKeyEntry(id)
	if updated == nil {
		json.NewEncoder(w).Encode(map[string]bool{"success": true})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"apiKey":  toApiKeyView(*updated),
	})
}
