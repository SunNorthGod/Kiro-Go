package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"kiro-go/config"
	"kiro-go/db"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// billing_handlers.go serves the unified-ledger billing views for both the admin
// panel (per-key usage/recharge management) and the customer self-service portal
// (/user + /user/api/*). All money/usage data comes from the same authoritative
// source: config balance mirror + db usage_counters/usage_records/recharge_records.
// In JSON mode (no DATABASE_URL) the summary fields still work off the in-memory
// mirror; the paginated detail/recharge lists gracefully return empty.

// ---------- shared helpers ----------

func parsePageParams(r *http.Request) (page, pageSize int) {
	page = 1
	pageSize = 20
	if v, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && v > 0 {
		page = v
	}
	if v, err := strconv.Atoi(r.URL.Query().Get("pageSize")); err == nil && v > 0 && v <= 200 {
		pageSize = v
	}
	return
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if status != 200 {
		w.WriteHeader(status)
	}
	_ = json.NewEncoder(w).Encode(v)
}

// keyHasChildren reports whether any key names this one as parent.
func keyHasChildren(id string) bool {
	if id == "" {
		return false
	}
	for _, e := range config.ListApiKeys() {
		if e.ParentKeyID == id {
			return true
		}
	}
	return false
}

// keyUsageSummary builds the usage/balance summary for one key (by id), including
// a per-model breakdown when the DB backend is active.
func keyUsageSummary(e *config.ApiKeyEntry) map[string]interface{} {
	granted, used, balance, ok := config.GetApiKeyBalanceByID(e.ID)
	if !ok {
		granted, used, balance = e.CreditsGranted, e.CreditsUsed, e.CreditsGranted-e.CreditsUsed
	}
	byModel := []map[string]interface{}{}
	// Totals default to the in-memory mirror (works in JSON mode); when the DB is
	// active we recompute from the authoritative counters and also split
	// input/output tokens per model for the usage panel.
	var totalInputTokens, totalOutputTokens, totalRequests int64
	var totalCacheReadTokens, totalCacheCreationTokens int64
	totalRequests = e.RequestsCount
	if pool := config.DatabasePool(); pool != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if counters, err := db.GetUsageCounters(ctx, pool, e.ID); err == nil {
			type modelAgg struct {
				credits       float64
				requests      int64
				inputTokens   int64
				outputTokens  int64
				cacheRead     int64
				cacheCreation int64
			}
			agg := map[string]*modelAgg{}
			order := []string{}
			var reqSum int64
			for _, c := range counters {
				m := c.Model
				if m == "" {
					m = "(unknown)"
				}
				if agg[m] == nil {
					agg[m] = &modelAgg{}
					order = append(order, m)
				}
				agg[m].credits += c.TotalCredits
				agg[m].requests += c.TotalRequests
				agg[m].inputTokens += c.TotalInputTokens
				agg[m].outputTokens += c.TotalOutputTokens
				agg[m].cacheRead += c.TotalCacheReadTokens
				agg[m].cacheCreation += c.TotalCacheCreationTokens
				reqSum += c.TotalRequests
				totalInputTokens += c.TotalInputTokens
				totalOutputTokens += c.TotalOutputTokens
				totalCacheReadTokens += c.TotalCacheReadTokens
				totalCacheCreationTokens += c.TotalCacheCreationTokens
			}
			if reqSum > 0 {
				totalRequests = reqSum
			}
			for _, m := range order {
				byModel = append(byModel, map[string]interface{}{
					"model": m, "credits": agg[m].credits, "requests": agg[m].requests,
					"inputTokens": agg[m].inputTokens, "outputTokens": agg[m].outputTokens,
					"cacheReadInputTokens": agg[m].cacheRead, "cacheCreationInputTokens": agg[m].cacheCreation,
				})
			}
		}
	}
	// Cache hit rate = cache_read / total_input (total_input already includes the
	// cached prompt tokens). null when there's no input yet, so the UI can show a
	// dash instead of a misleading 0%.
	var cacheHitRate interface{}
	if totalInputTokens > 0 {
		hr := float64(totalCacheReadTokens) / float64(totalInputTokens)
		if hr > 1 {
			hr = 1
		}
		cacheHitRate = hr
	}
	// The card's spendable budget is what has been granted (recharged); expose it
	// as creditLimit so a "used / limit" gauge is meaningful. Fall back to the
	// entry's explicit CreditLimit cap when nothing has been granted.
	creditLimit := granted
	if creditLimit <= 0 {
		creditLimit = e.CreditLimit
	}
	return map[string]interface{}{
		"creditsGranted": granted,
		"creditsUsed":    used,
		"balance":        balance,
		"tokensUsed":     e.TokensUsed,
		"requestsCount":  e.RequestsCount,
		"byModel":        byModel,
		// Enriched fields for the plugin usage panel (and richer web display).
		"name":              e.Name,
		"creditLimit":       creditLimit,
		"expiresAt":         e.ExpiresAt, // unix seconds, 0 = never
		"totalCredits":      used,
		"totalRequests":     totalRequests,
		"totalInputTokens":  totalInputTokens,
		"totalOutputTokens": totalOutputTokens,
		// Cache accounting (display-only): lifetime cache read/creation tokens and
		// the derived hit rate (null when no input yet).
		"totalCacheReadTokens":     totalCacheReadTokens,
		"totalCacheCreationTokens": totalCacheCreationTokens,
		"cacheHitRate":             cacheHitRate,
	}
}

// keyUsageRecords returns paginated consumption detail rows (DB only; empty in JSON mode).
func keyUsageRecords(id string, page, pageSize int) map[string]interface{} {
	records := []map[string]interface{}{}
	var total int64
	if pool := config.DatabasePool(); pool != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if recs, t, err := db.ListUsageRecords(ctx, pool, id, page, pageSize); err == nil {
			total = t
			for _, rr := range recs {
				records = append(records, map[string]interface{}{
					"model": rr.Model, "inputTokens": rr.InputTokens, "outputTokens": rr.OutputTokens,
					"credits": rr.Credits, "createdAt": rr.CreatedAt,
					// Cache accounting for the usage panel's cache-hit-rate view.
					"cacheReadInputTokens":     rr.CacheReadInputTokens,
					"cacheCreationInputTokens": rr.CacheCreationInputTokens,
				})
			}
		}
	}
	return map[string]interface{}{"records": records, "total": total, "page": page, "pageSize": pageSize}
}

// keyRecharges returns paginated recharge history (DB only; empty in JSON mode).
func keyRecharges(id string, page, pageSize int) map[string]interface{} {
	records := []map[string]interface{}{}
	var total int64
	if pool := config.DatabasePool(); pool != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if recs, t, err := db.ListRechargeRecords(ctx, pool, id, page, pageSize); err == nil {
			total = t
			for _, rr := range recs {
				records = append(records, map[string]interface{}{
					"amount": rr.Amount, "operator": rr.Operator, "note": rr.Note,
					"balanceAfter": rr.BalanceAfter, "createdAt": rr.CreatedAt,
				})
			}
		}
	}
	return map[string]interface{}{"records": records, "total": total, "page": page, "pageSize": pageSize}
}

// keyChildren returns the child keys of a parent key with their balances.
func keyChildren(parentID string) map[string]interface{} {
	children := []map[string]interface{}{}
	for _, c := range config.ListApiKeys() {
		if c.ParentKeyID != parentID {
			continue
		}
		children = append(children, map[string]interface{}{
			"id": c.ID, "name": c.Name, "enabled": c.Enabled,
			"creditsGranted": c.CreditsGranted, "creditsUsed": c.CreditsUsed,
			"balance": c.CreditsGranted - c.CreditsUsed, "requestsCount": c.RequestsCount,
		})
	}
	return map[string]interface{}{"children": children}
}

// childStatus classifies a sub-card for the portal: disabled / expired / active.
func childStatus(c *config.ApiKeyEntry) string {
	if !c.Enabled {
		return "disabled"
	}
	if config.IsApiKeyExpired(*c) {
		return "expired"
	}
	return "active"
}

// childView renders one sub-card for the reseller portal. It returns the full key
// value (the reseller owns the card and needs to hand it to their customer) plus
// the live balance and a computed status.
func childView(c *config.ApiKeyEntry) map[string]interface{} {
	if c == nil {
		return nil
	}
	granted, used, balance, ok := config.GetApiKeyBalanceByID(c.ID)
	if !ok {
		granted, used, balance = c.CreditsGranted, c.CreditsUsed, c.CreditsGranted-c.CreditsUsed
	}
	return map[string]interface{}{
		"id": c.ID, "name": c.Name, "key": c.Key, "enabled": c.Enabled,
		"creditsGranted": granted, "creditsUsed": used, "balance": balance,
		"requestsCount": c.RequestsCount, "tokensUsed": c.TokensUsed,
		"createdAt": c.CreatedAt, "expiresAt": c.ExpiresAt, "status": childStatus(c),
	}
}

// resellerOverview builds the shared-pool summary + sub-card list for a parent
// card's self-service portal, mirroring the Rust ResellerOverview.
func resellerOverview(parent *config.ApiKeyEntry) map[string]interface{} {
	children := []map[string]interface{}{}
	allocated := 0.0
	for _, c := range config.ListApiKeys() {
		if c.ParentKeyID != parent.ID {
			continue
		}
		cc := c
		children = append(children, childView(&cc))
		allocated += c.CreditsGranted
	}
	_, ownUsed, _, ok := config.GetApiKeyBalanceByID(parent.ID)
	if !ok {
		ownUsed = parent.CreditsUsed
	}
	return map[string]interface{}{
		"canManageSubKeys": canManageSubKeys(parent),
		"budget":           parent.CreditsGranted,
		"ownUsed":          ownUsed,
		"allocated":        allocated,
		"allocatable":      allocatableCredits(parent.ID, ""),
		"subKeyCount":      len(children),
		"children":         children,
	}
}

// userCreateChild lets a reseller (parent card holder) open a sub-card from their
// own portal, carving its grant out of the shared allocatable pool. Mirrors the
// Rust create_sub_key: validate the pool, inherit the parent's account bindings,
// cap expiry by the parent, and record the opening grant as a recharge row.
func userCreateChild(w http.ResponseWriter, r *http.Request, parent *config.ApiKeyEntry) {
	if !canManageSubKeys(parent) {
		writeJSON(w, 403, map[string]string{"error": "此卡密不支持开子卡密（需为按额度的卡，且本身不是子卡密）"})
		return
	}
	var req struct {
		Name         string  `json:"name"`
		CreditLimit  float64 `json:"creditLimit"`
		DurationDays float64 `json:"durationDays"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid JSON"})
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeJSON(w, 400, map[string]string{"error": "请填写子卡密名称"})
		return
	}
	if req.CreditLimit <= 0 {
		writeJSON(w, 400, map[string]string{"error": "子卡密额度必须大于 0"})
		return
	}
	// Advisory pre-check for a friendly localized message; the authoritative pool
	// validation re-runs atomically inside config.CreateChildApiKey (same lock as
	// the create, so concurrent creations cannot over-commit the pool).
	free := allocatableCredits(parent.ID, "")
	if req.CreditLimit > free+allocEpsilon {
		writeJSON(w, 400, map[string]string{"error": fmt.Sprintf("超出可分配额度：可分配 %.2f credits，请求 %.2f credits", free, req.CreditLimit)})
		return
	}
	// Child expiry: derive from durationDays (capped by the parent's expiry) else
	// inherit the parent's expiry so a sub-card can never outlive its parent.
	childExpiry := parent.ExpiresAt
	if req.DurationDays > 0 {
		exp := time.Now().Unix() + int64(req.DurationDays*86400)
		if parent.ExpiresAt > 0 && exp > parent.ExpiresAt {
			exp = parent.ExpiresAt
		}
		childExpiry = exp
	}
	// Atomic create: pool check + key row + opening grant + recharge audit row in
	// one critical section (one DB transaction), replacing the old two-step
	// AddApiKey + RechargeApiKey whose half-created child was invisible to
	// concurrent pool checks.
	child, err := config.CreateChildApiKey(config.ApiKeyEntry{
		Name:            req.Name,
		Key:             config.GenerateApiKeyValue(),
		Enabled:         true,
		CreditLimit:     req.CreditLimit,
		ParentKeyID:     parent.ID,
		BoundAccountIDs: parent.BoundAccountIDs,
		ExpiresAt:       childExpiry,
	}, req.CreditLimit, "reseller:"+parent.Name, "开卡初始额度")
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 201, map[string]interface{}{"success": true, "child": childView(config.GetApiKeyEntry(child.ID))})
}

// userUpdateChild lets a reseller rename / enable-disable / resize one of their
// own sub-cards. Resizing is validated against the child's real spend and the
// parent's remaining allocatable pool.
func userUpdateChild(w http.ResponseWriter, r *http.Request, parent *config.ApiKeyEntry, childID string) {
	child := config.GetApiKeyEntry(childID)
	if child == nil || child.ParentKeyID != parent.ID {
		writeJSON(w, 404, map[string]string{"error": "子卡密不存在"})
		return
	}
	var req struct {
		Name        *string  `json:"name"`
		Enabled     *bool    `json:"enabled"`
		CreditLimit *float64 `json:"creditLimit"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid JSON"})
		return
	}
	patch := *child
	if req.Name != nil {
		patch.Name = strings.TrimSpace(*req.Name)
	}
	if req.Enabled != nil {
		patch.Enabled = *req.Enabled
	}
	var newGrant *float64
	if req.CreditLimit != nil {
		nl := *req.CreditLimit
		if nl <= 0 {
			writeJSON(w, 400, map[string]string{"error": "子卡密额度必须大于 0"})
			return
		}
		_, used, _, ok := config.GetApiKeyBalanceByID(child.ID)
		if !ok {
			used = child.CreditsUsed
		}
		if nl+allocEpsilon < used {
			writeJSON(w, 400, map[string]string{"error": fmt.Sprintf("新额度 %.2f 不能低于已消耗 %.2f credits", nl, used)})
			return
		}
		if free := allocatableCredits(parent.ID, child.ID); nl > free+allocEpsilon {
			writeJSON(w, 400, map[string]string{"error": fmt.Sprintf("超出可分配额度：可分配 %.2f credits，请求 %.2f credits", free, nl)})
			return
		}
		patch.CreditLimit = nl
		newGrant = &nl
	}
	if err := config.UpdateApiKey(child.ID, patch); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	// SetApiKeyGrantChecked re-validates spend coverage + parent pool atomically
	// with the write (the pre-checks above are advisory display messages only).
	if newGrant != nil {
		if err := config.SetApiKeyGrantChecked(child.ID, *newGrant); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
	}
	writeJSON(w, 200, map[string]interface{}{"success": true, "child": childView(config.GetApiKeyEntry(child.ID))})
}

// userDeleteChild lets a reseller delete one of their own sub-cards.
func userDeleteChild(w http.ResponseWriter, r *http.Request, parent *config.ApiKeyEntry, childID string) {
	child := config.GetApiKeyEntry(childID)
	if child == nil || child.ParentKeyID != parent.ID {
		writeJSON(w, 404, map[string]string{"error": "子卡密不存在"})
		return
	}
	if err := config.DeleteApiKey(childID); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]bool{"success": true})
}

// ---------- customer self-service portal (/user, /user/api/*) ----------

func (h *Handler) serveUserPage(w http.ResponseWriter, r *http.Request) {
	// Always revalidate the portal HTML so structure changes land immediately;
	// its ?v-busted script/style refs keep JS/CSS cacheable.
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	http.ServeFile(w, r, "web/user.html")
}

// userKeyFromRequest resolves the customer's API key from the Authorization
// bearer (or X-Api-Key) header to its config entry, or nil if missing/unknown.
func userKeyFromRequest(r *http.Request) *config.ApiKeyEntry {
	key := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if key == "" {
		key = strings.TrimSpace(r.Header.Get("X-Api-Key"))
	}
	if key == "" {
		return nil
	}
	return config.FindApiKeyByValue(key)
}

// handleUserAPI serves the customer portal API, scoped strictly to the caller's
// own key (authenticated by the key value itself).
func (h *Handler) handleUserAPI(w http.ResponseWriter, r *http.Request) {
	entry := userKeyFromRequest(r)
	if entry == nil || !entry.Enabled {
		writeJSON(w, 401, map[string]string{"error": "invalid or missing API key"})
		return
	}
	// An expired card must not access its portal (previously only !Enabled was
	// checked, so an expired-but-enabled card could still read/manage its data).
	if config.IsApiKeyExpired(*entry) {
		writeJSON(w, 403, map[string]string{"error": "API key has expired"})
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/user/api")
	switch {
	case path == "/me" && r.Method == "GET":
		granted, used, balance, ok := config.GetApiKeyBalanceByID(entry.ID)
		if !ok {
			granted, used, balance = entry.CreditsGranted, entry.CreditsUsed, entry.CreditsGranted-entry.CreditsUsed
		}
		writeJSON(w, 200, map[string]interface{}{
			"name": entry.Name, "enabled": entry.Enabled, "expired": config.IsApiKeyExpired(*entry),
			"creditsGranted": granted, "creditsUsed": used, "balance": balance,
			"tokensUsed": entry.TokensUsed, "requestsCount": entry.RequestsCount,
			"tokenLimit": entry.TokenLimit, "creditLimit": entry.CreditLimit,
			"expiresAt": entry.ExpiresAt, "maxConcurrency": entry.MaxConcurrency,
			"maxRPM": entry.MaxRPM,
			"createdAt": entry.CreatedAt, "lastUsedAt": entry.LastUsedAt,
			"isParent":         keyHasChildren(entry.ID),
			"canManageSubKeys": canManageSubKeys(entry),
			"allocatable":      allocatableCredits(entry.ID, ""),
		})
	case path == "/usage" && r.Method == "GET":
		writeJSON(w, 200, keyUsageSummary(entry))
	case path == "/usage/records" && r.Method == "GET":
		page, pageSize := parsePageParams(r)
		writeJSON(w, 200, keyUsageRecords(entry.ID, page, pageSize))
	case path == "/recharges" && r.Method == "GET":
		page, pageSize := parsePageParams(r)
		writeJSON(w, 200, keyRecharges(entry.ID, page, pageSize))
	case path == "/children" && r.Method == "GET":
		writeJSON(w, 200, resellerOverview(entry))
	case path == "/children" && r.Method == "POST":
		userCreateChild(w, r, entry)
	case strings.HasPrefix(path, "/children/") && r.Method == "PUT":
		userUpdateChild(w, r, entry, strings.TrimPrefix(path, "/children/"))
	case strings.HasPrefix(path, "/children/") && r.Method == "DELETE":
		userDeleteChild(w, r, entry, strings.TrimPrefix(path, "/children/"))
	default:
		writeJSON(w, 404, map[string]string{"error": "not found"})
	}
}

// ---------- admin billing endpoints (under /admin/api, already authed) ----------

// apiTopupApiKey credits the key's balance and records a recharge row.
func (h *Handler) apiTopupApiKey(w http.ResponseWriter, r *http.Request, id string) {
	var req struct {
		Amount float64 `json:"amount"`
		Note   string  `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid JSON"})
		return
	}
	if req.Amount <= 0 {
		writeJSON(w, 400, map[string]string{"error": "amount must be positive"})
		return
	}
	if err := config.RechargeApiKey(id, req.Amount, "admin", req.Note); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	granted, used, balance, _ := config.GetApiKeyBalanceByID(id)
	writeJSON(w, 200, map[string]interface{}{
		"success": true, "creditsGranted": granted, "creditsUsed": used, "balance": balance,
	})
}

func (h *Handler) apiApiKeyRecharges(w http.ResponseWriter, r *http.Request, id string) {
	page, pageSize := parsePageParams(r)
	writeJSON(w, 200, keyRecharges(id, page, pageSize))
}

func (h *Handler) apiApiKeyUsage(w http.ResponseWriter, r *http.Request, id string) {
	entry := config.GetApiKeyEntry(id)
	if entry == nil {
		writeJSON(w, 404, map[string]string{"error": "api key not found"})
		return
	}
	writeJSON(w, 200, keyUsageSummary(entry))
}

func (h *Handler) apiApiKeyUsageRecords(w http.ResponseWriter, r *http.Request, id string) {
	page, pageSize := parsePageParams(r)
	writeJSON(w, 200, keyUsageRecords(id, page, pageSize))
}

func (h *Handler) apiApiKeyChildren(w http.ResponseWriter, r *http.Request, id string) {
	writeJSON(w, 200, keyChildren(id))
}

// apiConcurrency exposes scheduler observability: sticky-cache hit rate and
// current in-flight counts per account / per key.
func (h *Handler) apiConcurrency(w http.ResponseWriter, r *http.Request) {
	hits, misses := h.pool.StickyMetrics()
	byAcct, byKey, total := h.pool.ConcurrencySnapshot()
	writeJSON(w, 200, map[string]interface{}{
		"stickyHits":        hits,
		"stickyMisses":      misses,
		"inflightByAccount": byAcct,
		"inflightByKey":     byKey,
		"inflightTotal":     total,
	})
}
