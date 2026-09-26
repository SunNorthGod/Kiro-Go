package proxy

// dashboard.go powers the admin 概览 (overview) page: a single /admin/api/overview
// call returns global counters, real-time RPM, concurrency/stickiness/cache runtime
// state, account + card-key summaries, and a daily time-series for the trend chart.
//
// Daily history is REAL and persistent (it survives restarts), matching the Rust
// panel which derives daily stats from persisted usage records:
//   - DB mode (production): aggregated live from usage_records by CST calendar day
//     (db.GetDailyUsage) — the authoritative, always-accurate history.
//   - JSON mode: accumulated in-memory and persisted to <configdir>/daily_stats.json
//     (throttled), reloaded on startup.
// Credits are the billing unit; tokens are display-only.

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"kiro-go/config"
	"kiro-go/db"
)

// cstZone is the business-day timezone (UTC+8) used for daily bucketing so day
// boundaries line up with the Rust panel and the zh user base.
var cstZone = time.FixedZone("CST", 8*3600)

func cstDay(t time.Time) string { return t.In(cstZone).Format("2006-01-02") }

// dayBucket accumulates one calendar day's successful-request totals.
type dayBucket struct {
	Requests int64   `json:"requests"`
	Tokens   int64   `json:"tokens"`
	Credits  float64 `json:"credits"`
}

const dailyRetentionDays = 30
const dailySaveThrottleSec = 15

var dailyLoadOnce sync.Once

func (h *Handler) dailyFilePath() string {
	return filepath.Join(config.ConfigDir(), "daily_stats.json")
}

// loadDailyOnce lazily loads persisted daily buckets from disk (JSON mode). It is
// a no-op on missing file / parse error (the map just starts empty), and harmless
// in DB mode where the series is derived from usage_records instead.
func (h *Handler) loadDailyOnce() {
	dailyLoadOnce.Do(func() {
		data, err := os.ReadFile(h.dailyFilePath())
		if err != nil {
			return
		}
		m := map[string]*dayBucket{}
		if json.Unmarshal(data, &m) != nil {
			return
		}
		h.dailyMu.Lock()
		if h.dailyStats == nil {
			h.dailyStats = m
		} else {
			for k, v := range m {
				if h.dailyStats[k] == nil {
					h.dailyStats[k] = v
				}
			}
		}
		h.dailyMu.Unlock()
	})
}

// saveDailyToDisk writes the current buckets (best-effort). Caller must NOT hold dailyMu.
func (h *Handler) saveDailyToDisk() {
	h.dailyMu.Lock()
	data, err := json.Marshal(h.dailyStats)
	h.dailyMu.Unlock()
	if err != nil {
		return
	}
	_ = os.WriteFile(h.dailyFilePath(), data, 0600)
}

// recordDaily folds one successful request into today's (CST) bucket, then persists
// to disk at most once every dailySaveThrottleSec seconds. Nil-safe / lazy-init.
func (h *Handler) recordDaily(tokens int, credits float64) {
	h.loadDailyOnce()
	day := cstDay(time.Now())
	h.dailyMu.Lock()
	if h.dailyStats == nil {
		h.dailyStats = make(map[string]*dayBucket)
	}
	b := h.dailyStats[day]
	if b == nil {
		b = &dayBucket{}
		h.dailyStats[day] = b
		h.pruneDailyLocked()
	}
	b.Requests++
	b.Tokens += int64(tokens)
	b.Credits += credits
	now := time.Now().Unix()
	due := now-h.dailySavedAt >= dailySaveThrottleSec
	if due {
		h.dailySavedAt = now
	}
	h.dailyMu.Unlock()
	if due {
		go h.saveDailyToDisk()
	}
}

// pruneDailyLocked drops buckets older than the retention window. Caller holds dailyMu.
func (h *Handler) pruneDailyLocked() {
	if len(h.dailyStats) <= dailyRetentionDays+1 {
		return
	}
	cutoff := cstDay(time.Now().AddDate(0, 0, -dailyRetentionDays))
	for d := range h.dailyStats {
		if d < cutoff {
			delete(h.dailyStats, d)
		}
	}
}

// dailySeries returns the last `days` CST days (ascending), filling absent days
// with zeros. Used in JSON mode (in-memory + disk-persisted).
func (h *Handler) dailySeries(days int) []map[string]interface{} {
	if days <= 0 {
		days = 14
	}
	h.loadDailyOnce()
	h.dailyMu.Lock()
	snap := make(map[string]dayBucket, len(h.dailyStats))
	for d, b := range h.dailyStats {
		snap[d] = *b
	}
	h.dailyMu.Unlock()

	out := make([]map[string]interface{}, 0, days)
	now := time.Now()
	for i := days - 1; i >= 0; i-- {
		d := cstDay(now.AddDate(0, 0, -i))
		b := snap[d]
		out = append(out, map[string]interface{}{
			"date":     d,
			"requests": b.Requests,
			"tokens":   b.Tokens,
			"credits":  b.Credits,
		})
	}
	return out
}

// ---- overview 重聚合 TTL 缓存 ----
//
// The admin page polls /admin/api/overview every second while the 概览 tab is
// open. Two parts of that response are expensive aggregations over the
// display-only usage_records log (~1e5 rows): the 7-day prompt-cache SUM
// (db.GetRecentCacheStats) and the daily GROUP BY series (db.GetDailyUsage).
// Both only move on a minutes scale, so they are served from a short
// in-process TTL cache: one open dashboard now costs one heavy scan per
// overviewHeavyTTL instead of one per second.
//
// Realtime fields (totalRequests/totalTokens/totalCredits/RPM/TPM/concurrency/
// account+key counts/sticky metrics) are NOT cached — they are recomputed on
// every request, which is the whole point of the 1s poll.
const overviewHeavyTTL = 10 * time.Second

// ovDailyEntry caches one computed daily series (keyed by the `days` argument).
// Cached slices are read-only by convention: every consumer only JSON-encodes them.
type ovDailyEntry struct {
	at   time.Time
	data []map[string]interface{}
}

// ovPromptEntry caches the deployment-wide promptCache stats block.
type ovPromptEntry struct {
	at   time.Time
	data map[string]interface{}
}

// overviewDaily returns the trend series for `days`, reusing the cached copy
// while it is fresh. `days` is already clamped to [1, dailyRetentionDays] by
// the caller, so the cache map is naturally bounded. Concurrent misses may
// recompute in parallel (last write wins) — acceptable for a 1s poll, and it
// avoids holding the mutex across a DB round-trip.
func (h *Handler) overviewDaily(days int) []map[string]interface{} {
	now := time.Now()
	h.ovHeavyMu.Lock()
	if e, ok := h.ovDailyCache[days]; ok && now.Sub(e.at) < overviewHeavyTTL {
		h.ovHeavyMu.Unlock()
		return e.data
	}
	h.ovHeavyMu.Unlock()

	daily, ok := h.dailySeriesFromDB(days)
	if !ok {
		daily = h.dailySeries(days)
	}

	h.ovHeavyMu.Lock()
	if h.ovDailyCache == nil {
		h.ovDailyCache = make(map[int]ovDailyEntry)
	}
	h.ovDailyCache[days] = ovDailyEntry{at: now, data: daily}
	h.ovHeavyMu.Unlock()
	return daily
}

// overviewPromptCache returns the promptCache stats block, reusing the cached
// copy while it is fresh.
func (h *Handler) overviewPromptCache() map[string]interface{} {
	now := time.Now()
	h.ovHeavyMu.Lock()
	if h.ovPromptCache.data != nil && now.Sub(h.ovPromptCache.at) < overviewHeavyTTL {
		data := h.ovPromptCache.data
		h.ovHeavyMu.Unlock()
		return data
	}
	h.ovHeavyMu.Unlock()

	data := h.computePromptCacheBlock()

	h.ovHeavyMu.Lock()
	h.ovPromptCache = ovPromptEntry{at: now, data: data}
	h.ovHeavyMu.Unlock()
	return data
}

// computePromptCacheBlock builds the deployment-wide prompt-cache stats block
// over ALL HISTORY (owner decision 2026-09-27: drop the 7-day window, operators
// reconcile this number against the lifetime billing ledger). Computed from the
// display-only usage_records log; hitRate is null when there's no input at all
// (the UI shows a dash rather than a misleading 0%). DB mode only; in JSON mode
// the zero-value block is returned as before.
func (h *Handler) computePromptCacheBlock() map[string]interface{} {
	// windowDays 0 = lifetime aggregate; the UI labels the KPI "累计".
	promptCache := map[string]interface{}{"hitRate": nil, "readTokens": 0, "creationTokens": 0, "inputTokens": 0, "windowDays": 0}
	if pool := config.DatabasePool(); pool != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if gs, err := db.GetRecentCacheStats(ctx, pool, 0); err == nil {
			promptCache["readTokens"] = gs.CacheReadTokens
			promptCache["creationTokens"] = gs.CacheCreationTokens
			promptCache["inputTokens"] = gs.InputTokens
			if gs.InputTokens > 0 {
				hr := float64(gs.CacheReadTokens) / float64(gs.InputTokens)
				if hr > 1 {
					hr = 1
				}
				promptCache["hitRate"] = hr
			}
		}
	}
	return promptCache
}

// dailySeriesFromDB aggregates the last `days` from usage_records (DB mode only),
// filling gaps. Returns (series, true) when the DB backend served it.
func (h *Handler) dailySeriesFromDB(days int) ([]map[string]interface{}, bool) {
	pool := config.DatabasePool()
	if pool == nil {
		return nil, false
	}
	if days <= 0 {
		days = 14
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := db.GetDailyUsage(ctx, pool, days)
	if err != nil {
		return nil, false
	}
	byDay := make(map[string]db.DailyUsage, len(rows))
	for _, r := range rows {
		byDay[r.Date] = r
	}
	out := make([]map[string]interface{}, 0, days)
	now := time.Now()
	for i := days - 1; i >= 0; i-- {
		d := cstDay(now.AddDate(0, 0, -i))
		r := byDay[d]
		out = append(out, map[string]interface{}{
			"date":     d,
			"requests": r.Requests,
			"tokens":   r.InputTokens + r.OutputTokens,
			"credits":  r.Credits,
		})
	}
	return out, true
}

// apiGetOverview returns everything the 概览 dashboard needs in one call.
func (h *Handler) apiGetOverview(w http.ResponseWriter, r *http.Request) {
	inflightAcct, inflightKey, inflightAll := h.pool.ConcurrencySnapshot()
	stickyHits, stickyMisses := h.pool.StickyMetrics()

	keys := config.ListApiKeys()
	activeKeys := 0
	for _, k := range keys {
		if k.Enabled && !config.IsApiKeyExpired(k) {
			activeKeys++
		}
	}

	accounts := config.GetAccounts()
	enabledAccounts := 0
	for _, a := range accounts {
		if a.Enabled {
			enabledAccounts++
		}
	}
	total := len(accounts)

	days := 14
	if v := r.URL.Query().Get("days"); v != "" {
		if parsed := atoiClamp(v, 1, dailyRetentionDays); parsed > 0 {
			days = parsed
		}
	}

	// Heavy usage_records aggregations (daily trend + 7d prompt-cache stats) are
	// served through a short TTL cache; everything else below stays realtime.
	daily := h.overviewDaily(days)
	promptCache := h.overviewPromptCache()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"totalRequests":   atomic.LoadInt64(&h.totalRequests),
		"successRequests": atomic.LoadInt64(&h.successRequests),
		"failedRequests":  atomic.LoadInt64(&h.failedRequests),
		"totalTokens":     atomic.LoadInt64(&h.totalTokens),
		"totalCredits":    h.getCredits(),
		"uptime":          time.Now().Unix() - h.startTime,
		"totalRPM":        h.pool.TotalRPM(),
		"totalTPM":        h.pool.TotalTPM(),
		"accounts": map[string]interface{}{
			"total":     total,
			"available": h.pool.AvailableCount(),
			"enabled":   enabledAccounts,
			"disabled":  total - enabledAccounts,
		},
		"keys": map[string]interface{}{
			"total":  len(keys),
			"active": activeKeys,
		},
		"concurrency": map[string]interface{}{
			"inflight":       inflightAll,
			"activeAccounts": len(inflightAcct),
			"activeKeys":     len(inflightKey),
		},
		"cache": map[string]interface{}{
			"stickyHits":     stickyHits,
			"stickyMisses":   stickyMisses,
			"stickySessions": h.pool.StickySessions(),
		},
		"promptCache": promptCache,
		"daily":       daily,
	})
}

// atoiClamp parses s as a base-10 int and clamps it to [min,max]; returns 0 on
// parse failure so the caller can keep its default.
func atoiClamp(s string, min, max int) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
		if n > 1_000_000 {
			break
		}
	}
	if n < min {
		return min
	}
	if n > max {
		return max
	}
	return n
}
