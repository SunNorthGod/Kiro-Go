package proxy

import (
	"encoding/json"
	"io"
	"kiro-go/config"
	"net/http"
	"strings"
)

// normalizeLimitPtr defensively clamps a per-key limit override to the unified
// semantics before persisting: nil stays nil (inherit default); any negative
// value (a stale "-1 = unlimited" client) is folded to 0 (unlimited); 0 and
// positive values pass through unchanged (0 == unlimited, N == that value).
func normalizeLimitPtr(v *int) *int {
	if v == nil {
		return nil
	}
	n := *v
	if n < 0 {
		n = 0
	}
	return &n
}

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
	ExpiresAt int64 `json:"expiresAt,omitempty"`
	Expired   bool  `json:"expired"`
	// Per-key limit overrides, nullable with unified semantics: absent (null) ==
	// inherit system default; 0 == unlimited; N == that value. Pointers so the
	// "inherit" state is a distinct absent field rather than an ambiguous 0.
	MaxConcurrency  *int     `json:"maxConcurrency,omitempty"`
	MaxRPM          *int     `json:"maxRPM,omitempty"`
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
		MaxRPM:          e.MaxRPM,
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
	MaxConcurrency  *int     `json:"maxConcurrency,omitempty"` // null == inherit default, 0 == unlimited, N == value
	MaxRPM          *int     `json:"maxRPM,omitempty"`         // null == inherit default, 0 == unlimited, N == value
	BoundAccountIDs []string `json:"boundAccountIds,omitempty"`
	ParentKeyID     string   `json:"parentKeyId,omitempty"`
}

// allocEpsilon absorbs float rounding so a grant that exactly equals the
// allocatable pool is accepted rather than tripping a > comparison.
const allocEpsilon = config.AllocEpsilon

// canManageSubKeys reports whether a card may open/manage sub-cards. Mirrors the
// Rust reseller rule (credit_limit set && parent_key_id none): the card must be a
// budgeted card (positive grant) and must not itself be a sub-card (one level of
// nesting only).
func canManageSubKeys(e *config.ApiKeyEntry) bool {
	return e != nil && e.CreditsGranted > 0 && strings.TrimSpace(e.ParentKeyID) == ""
}

// allocatableCredits / childGrantError / parentGrantError delegate to the config
// package, which owns the shared-pool math (live children commit
// max(grant, spend); deleted children's spend is settled into the parent). These
// proxy-layer calls are advisory (display + friendly pre-check messages); the
// authoritative checks re-run atomically inside the config mutations
// (AddApiKey / CreateChildApiKey / SetApiKeyGrantChecked / RechargeApiKey).

func allocatableCredits(parentID, excludeChildID string) float64 {
	return config.AllocatableChildCredits(parentID, excludeChildID)
}

func childGrantError(parentID, selfID string, grant float64) string {
	return config.ChildGrantError(parentID, selfID, grant)
}

func parentGrantError(selfID string, grant float64) string {
	return config.ParentGrantError(selfID, grant)
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
		MaxConcurrency:  normalizeLimitPtr(req.MaxConcurrency),
		MaxRPM:          normalizeLimitPtr(req.MaxRPM),
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
	MaxRPM          *int      `json:"maxRPM,omitempty"`
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

	// Read the body once so we can both decode into the typed request AND detect
	// which keys were actually present. The per-key limit fields are tri-state
	// (absent = don't touch, null = set to inherit-default, number = set value),
	// which a plain *int cannot express: absent and explicit-null both decode to
	// nil. Presence detection lets the enable/disable toggle (which PUTs only
	// {"enabled":...}) leave the limits untouched while still allowing the edit
	// form to clear an override back to inherit by sending an explicit null.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid body"})
		return
	}
	var req apiKeyUpdateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	var rawFields map[string]json.RawMessage
	if err := json.Unmarshal(body, &rawFields); err != nil {
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
	// Tri-state per-key limits: only touch a field when the JSON key is present.
	// Present → set to the sent value (req.Max* is nil for an explicit null =
	// inherit, or a pointer to 0 = unlimited / N = value). Absent → leave as-is.
	if _, ok := rawFields["maxConcurrency"]; ok {
		patch.MaxConcurrency = normalizeLimitPtr(req.MaxConcurrency)
	}
	if _, ok := rawFields["maxRPM"]; ok {
		patch.MaxRPM = normalizeLimitPtr(req.MaxRPM)
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
	// SetApiKeyGrantChecked re-validates the shared-pool invariants atomically
	// (the pre-checks above are advisory only).
	if req.CreditsGranted != nil {
		if err := config.SetApiKeyGrantChecked(id, *req.CreditsGranted); err != nil {
			w.WriteHeader(http.StatusBadRequest)
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
