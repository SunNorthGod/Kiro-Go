package pool

import (
	"errors"
	"fmt"
	"kiro-go/config"
	"kiro-go/logger"
	"sync"
	"time"
)

// scheduler.go implements the account dispatch layer:
//
//   - Soft, work-conserving per-key fairness: when the pool has spare capacity a
//     single key/account may use as much as it wants (no ceiling); only under
//     contention is a key bounded to its fair share so it cannot starve others.
//     Admin/master keys are never limited.
//   - Session stickiness: a conversation (agentContinuationId) is bound to one
//     account for the life of that account, so Kiro's per-account prompt cache
//     keeps hitting across turns. The binding is dropped only when the account
//     becomes ineligible (cooled/disabled/expired/over-quota) or fails auth.
//   - Priority-tier + least-loaded selection ("auto" mode): pick the highest
//     priority tier (Weight) that has an eligible account, and within that tier
//     the account with the fewest in-flight requests → even split within a tier.
//   - 429 → short exponential backoff (not a 1h cooldown); 401/403 → disable only
//     after N consecutive failures (see ReportOutcome).
//
// Concurrency: schedMu guards the in-flight/sticky state and is completely
// separate from mu (which guards the account list). Selection first snapshots
// eligible accounts under mu.RLock, releases it, then does admission + counting
// under schedMu — the two locks are never held at the same time, so there is no
// lock-ordering hazard.

// Tunables. perAccountSoftConcurrency is a *baseline* used by the work-conserving
// fair scheduler, not a hard ceiling: idle capacity is always lent out. The
// per-key concurrency baseline is admin-tunable (config.GetDefaultMaxConcurrency,
// factory default 5) and resolved per request in Acquire; this per-account value
// stays hardcoded at 20 by design.
const (
	perAccountSoftConcurrency = 20

	admitTimeout         = 8 * time.Second
	rateLimitBackoffBase = 1500 * time.Millisecond
	rateLimitBackoffMax  = 60 * time.Second
	authDisableThreshold = 3
	quotaCooldown        = 10 * time.Minute

	// Model-scoped restriction handling: three consecutive failures on the same
	// (account, model) cool THAT PAIR for modelRestrictedCooldown. The account
	// itself stays fully eligible for every other model. Measured in production
	// 2026-09-28: an upstream per-model risk-control on opus-5.5 (empty streams)
	// put both healthy accounts into rolling 1-minute cooldowns via the transient
	// path, 503ing even opus-4.8 traffic that was serving fine.
	modelRestrictedStrikes  = 3
	modelRestrictedCooldown = time.Minute
)

var (
	// ErrNoAccount means no eligible account serves the request right now.
	ErrNoAccount = errors.New("no available account")
	// ErrTooBusy means the caller's key exceeded its fair share while the pool was
	// saturated and no slot freed within admitTimeout → surface as HTTP 429.
	ErrTooBusy = errors.New("too many concurrent requests")
)

// Outcome classifies the result of a dispatched request for feedback.
type Outcome int

const (
	OutcomeSuccess        Outcome = iota // 2xx
	OutcomeRateLimited                   // 429: transient, short backoff
	OutcomeAuthError                     // 401/403: disable after N consecutive
	OutcomeQuotaExhausted                // 402 / monthly quota: cooldown until it likely resets
	OutcomeTransient                     // 5xx / network: brief cooldown after repeats
	OutcomeModelRestricted               // model-scoped risk control (empty stream etc.): cool (account, model), never the account
)

// snapAcct is an eligible account value copy plus its priority tier.
type snapAcct struct {
	acct     config.Account
	priority int
}

// snapshotStats counts, per filter, why accounts were left out of an eligibility
// snapshot. It exists so an empty snapshot can explain itself.
//
// Before this, Acquire returned ErrNoAccount — which the handlers turn into HTTP
// 503 "No available accounts" — without logging anything at all. Diagnosing a
// production burst of 503s therefore meant inferring the cause from the outside:
// the responses came back in 0.00s, no app log line accompanied them, no
// request_logs row is written (a row needs an account_id, and none was chosen),
// and every account showed error_count 0. Six filters can empty the set and they
// have completely different fixes, so the symptom was unattributable. Now it
// names itself.
type snapshotStats struct {
	pooled     int // accounts in the pool (after dedupe)
	notBound   int // excluded by the key's bound_account_ids
	retried    int // already tried and failed during this request's failover
	noModel    int // does not serve the requested model
	cooling    int // inside an error/rate-limit cooldown
	modelCool  int // (account, model) pair inside a model-scoped restriction cooldown
	nearExpiry int // inside the token refresh skew window
	quota      int // over usage quota / overage-blocked
}

// eligibleSnapshot returns value copies of every currently-eligible account for
// the given model (deduped by ID), captured under mu.RLock. Returning copies —
// never &p.accounts[i] — is what fixes the data race where the request path read
// a pool-internal account while a token refresh wrote it.
//
// boundAccountIDs, when non-empty, restricts selection to the intersection with
// the pool (a key bound to specific accounts may only use those); empty means any
// account is allowed.
//
// The second return value reports why each excluded account was excluded; it is
// only consumed when the snapshot comes back empty.
func (p *AccountPool) eligibleSnapshot(model string, excluded map[string]bool, boundAccountIDs []string) ([]snapAcct, snapshotStats) {
	var boundSet map[string]bool
	if len(boundAccountIDs) > 0 {
		boundSet = make(map[string]bool, len(boundAccountIDs))
		for _, id := range boundAccountIDs {
			boundSet[id] = true
		}
	}

	p.mu.RLock()
	defer p.mu.RUnlock()

	allowOverUsage := config.GetAllowOverUsage()
	now := time.Now()
	seen := make(map[string]bool, len(p.accounts))
	out := make([]snapAcct, 0, len(p.accounts))
	var stats snapshotStats

	for i := range p.accounts {
		a := p.accounts[i]
		if seen[a.ID] {
			continue
		}
		seen[a.ID] = true
		stats.pooled++
		if boundSet != nil && !boundSet[a.ID] {
			stats.notBound++
			continue
		}
		if excluded != nil && excluded[a.ID] {
			stats.retried++
			continue
		}
		if model != "" && !p.accountHasModel(a.ID, model) {
			stats.noModel++
			continue
		}
		if cd, ok := p.cooldowns[a.ID]; ok && now.Before(cd) {
			stats.cooling++
			continue
		}
		if model != "" {
			if cd, ok := p.modelCooldowns[modelScopeKey(a.ID, model)]; ok && now.Before(cd) {
				stats.modelCool++
				continue
			}
		}
		if a.ExpiresAt > 0 && now.Unix() > a.ExpiresAt-tokenRefreshSkewSeconds {
			stats.nearExpiry++
			continue
		}
		if isQuotaBlocked(a, allowOverUsage) {
			stats.quota++
			continue
		}
		out = append(out, snapAcct{acct: a, priority: a.Weight})
	}
	return out, stats
}

// noAccountLogState rate-limits the empty-snapshot explanation. A burst of 503s can
// be dozens per second (56 in one minute was measured), and one line each would
// bury the very information it is meant to surface — so identical bursts collapse
// into one line carrying the suppressed count.
var noAccountLogState struct {
	mu         sync.Mutex
	last       time.Time
	suppressed int
}

const noAccountLogInterval = 2 * time.Second

// modelRelaxLogState rate-limits the model-filter fallback notice, which fires once
// per request for a model nobody advertises — potentially every request from one
// misconfigured client.
var modelRelaxLogState struct {
	mu         sync.Mutex
	last       time.Time
	suppressed int
}

// logModelFilterRelaxed records that routing ignored the per-account model list
// because honouring it would have produced a 503. Worth a WARN, not silence: it
// means either a client is asking for something that does not exist, or the model
// cache is missing a model the upstream really serves. Both need a human eventually.
func (p *AccountPool) logModelFilterRelaxed(model string, stats snapshotStats) {
	modelRelaxLogState.mu.Lock()
	if !modelRelaxLogState.last.IsZero() && time.Since(modelRelaxLogState.last) < noAccountLogInterval {
		modelRelaxLogState.suppressed++
		modelRelaxLogState.mu.Unlock()
		return
	}
	suppressed := modelRelaxLogState.suppressed
	modelRelaxLogState.suppressed = 0
	modelRelaxLogState.last = time.Now()
	modelRelaxLogState.mu.Unlock()

	extra := ""
	if suppressed > 0 {
		extra = fmt.Sprintf(" (+%d more suppressed in the last %s)", suppressed, noAccountLogInterval)
	}
	logger.Warnf("[Pool] model %q is advertised by none of %d accounts; routing anyway and "+
		"letting the upstream decide (would otherwise have been HTTP 503)%s",
		model, stats.noModel, extra)
}

// logNoAccount explains an empty eligibility snapshot. Called only on the
// ErrNoAccount path, so it costs nothing on a healthy request.
func (p *AccountPool) logNoAccount(model string, stats snapshotStats, boundAccountIDs []string) {
	noAccountLogState.mu.Lock()
	since := time.Since(noAccountLogState.last)
	if since < noAccountLogInterval && !noAccountLogState.last.IsZero() {
		noAccountLogState.suppressed++
		noAccountLogState.mu.Unlock()
		return
	}
	suppressed := noAccountLogState.suppressed
	noAccountLogState.suppressed = 0
	noAccountLogState.last = time.Now()
	noAccountLogState.mu.Unlock()

	extra := ""
	if suppressed > 0 {
		extra = fmt.Sprintf(" (+%d more suppressed in the last %s)", suppressed, noAccountLogInterval)
	}
	bound := "any"
	if len(boundAccountIDs) > 0 {
		bound = fmt.Sprintf("%d bound", len(boundAccountIDs))
	}
	// Reported even when a count is zero: which filters did NOT fire is as
	// diagnostic as which did.
	logger.Warnf("[Pool] no eligible account -> HTTP 503: model=%q key=%s pooled=%d "+
		"notBound=%d alreadyTried=%d noModel=%d cooling=%d modelCooling=%d nearTokenExpiry=%d quotaBlocked=%d%s",
		model, bound, stats.pooled, stats.notBound, stats.retried, stats.noModel,
		stats.cooling, stats.modelCool, stats.nearExpiry, stats.quota, extra)
}

// Acquire admits the request (per-key fairness), selects an account
// (sticky → priority-tier least-loaded), increments in-flight counters, and
// returns the chosen account (a copy) plus a release func that MUST be called
// exactly once when the request finishes (success, error, or client disconnect).
//
// keyConcurrency is the per-key concurrency override, nullable with unified
// semantics: nil → inherit the system default (config.GetDefaultMaxConcurrency,
// factory 5); 0 → unlimited (bypass the fairness gate, like an admin key); N →
// guaranteed baseline, effective fair cap under contention max(N, poolCap/keys).
// isAdmin bypasses the fairness gate entirely (still counted for observability).
// boundAccountIDs restricts selection to those accounts (empty = any).
//
// The concurrency gate and the RPM gate are INDEPENDENT: a key that is unlimited
// on concurrency is still RPM-gated (and vice-versa). Only isAdmin bypasses both.
func (p *AccountPool) Acquire(keyID string, keyConcurrency *int, isAdmin bool, conversationID, model string, excluded map[string]bool, boundAccountIDs []string) (config.Account, func(), error) {
	snap, stats := p.eligibleSnapshot(model, excluded, boundAccountIDs)
	if len(snap) == 0 && model != "" && stats.noModel > 0 {
		// The model filter ALONE emptied the pool, so the request is about to be told
		// "No available accounts" when the truth is "nobody advertises this model".
		//
		// ListAvailableModels is an advertisement, not an authority. Measured: every
		// one of the 12 requests for model "simple-task" that reached the upstream
		// (during the window after a restart when the model cache is still cold and
		// accountHasModel passes optimistically) SUCCEEDED. So the backend serves it
		// and the filter is wrong — yet once the cache warmed, the same request became
		// an instant 503. Every 503 observed on this build had exactly this shape:
		// noModel=10, with cooling, nearTokenExpiry, quotaBlocked and the bindings all
		// zero.
		//
		// So fall back to routing without the model filter and let the upstream decide.
		// This is the same optimism accountHasModel already applies to a cold cache,
		// extended to a warm-but-incomplete one. A model the upstream genuinely does
		// not have now comes back as its own 400 with the upstream's message, which
		// tells the client something true, instead of a 503 blaming our capacity.
		if relaxed, _ := p.eligibleSnapshot("", excluded, boundAccountIDs); len(relaxed) > 0 {
			p.logModelFilterRelaxed(model, stats)
			snap = relaxed
		}
	}
	if len(snap) == 0 {
		p.logNoAccount(model, stats, boundAccountIDs)
		return config.Account{}, nil, ErrNoAccount
	}
	distinct := len(snap)

	// Resolve the effective concurrency baseline. nil → system default; a value of
	// 0 means "unlimited" (bypass the fairness gate). This is decoupled from the
	// RPM gate below so unlimited concurrency does NOT disable RPM enforcement.
	concValue := config.GetDefaultMaxConcurrency()
	if keyConcurrency != nil {
		concValue = *keyConcurrency
	}
	concUnlimited := concValue == 0
	keyConcurrencyFloor := concValue

	// firstAttempt: this Acquire is the client request's FIRST dispatch (no prior
	// account excluded by a failover retry). The per-key RPM meter must count
	// CLIENT REQUESTS, not upstream dispatch attempts — otherwise a request that
	// fails over across K accounts would burn K RPM units and could even 429
	// itself mid-failover. So the RPM gate + the per-key RPM tick both run only on
	// the first attempt; retries are already-admitted work and skip both.
	firstAttempt := len(excluded) == 0

	// Per-key RPM ceiling (hard cap), enforced INDEPENDENTLY of the concurrency
	// gate: only isAdmin keys bypass it. Effective cap comes from the key's own
	// MaxRPM override else the system default (0 == unlimited on either level).
	// Enforced BEFORE admission so a throttled key surfaces as 429 without
	// occupying a slot.
	if firstAttempt && keyID != "" && !isAdmin {
		if cap := effectiveKeyRPMCap(keyID); cap > 0 {
			p.schedMu.Lock()
			cur := p.rpmKey[keyID]
			used := 0
			if cur != nil {
				used = cur.sum(time.Now().Unix())
			}
			p.schedMu.Unlock()
			if used >= cap {
				return config.Account{}, nil, ErrTooBusy
			}
		}
	}

	p.schedMu.Lock()
	if !p.admitLocked(keyID, keyConcurrencyFloor, isAdmin || concUnlimited, distinct) {
		p.schedMu.Unlock()
		return config.Account{}, nil, ErrTooBusy
	}
	// Admitted: inflightKey[keyID] already incremented by admitLocked.
	acct := p.selectLocked(snap, conversationID)
	p.inflightAcct[acct.ID]++
	p.inflightAll++
	nowSec := time.Now().Unix()
	// Per-account RPM (observability) ticks on every dispatch, but the per-key RPM
	// meter (which enforces the cap) ticks once per client request — see firstAttempt.
	p.rpmTickLocked(p.rpmAcct, acct.ID, nowSec)
	if keyID != "" && firstAttempt {
		p.rpmTickLocked(p.rpmKey, keyID, nowSec)
	}
	p.rpmAll.add(nowSec)
	if conversationID != "" {
		p.sticky[conversationID] = stickyRef{accountID: acct.ID, lastSeen: nowSec}
	}
	p.schedMu.Unlock()

	var once sync.Once
	release := func() {
		once.Do(func() {
			p.schedMu.Lock()
			if p.inflightKey[keyID] > 0 {
				p.inflightKey[keyID]--
				if p.inflightKey[keyID] == 0 {
					delete(p.inflightKey, keyID)
				}
			}
			if p.inflightAcct[acct.ID] > 0 {
				p.inflightAcct[acct.ID]--
				if p.inflightAcct[acct.ID] == 0 {
					delete(p.inflightAcct, acct.ID)
				}
			}
			if p.inflightAll > 0 {
				p.inflightAll--
			}
			p.schedCond.Broadcast() // a slot freed → wake fair-admission waiters
			p.schedMu.Unlock()
		})
	}
	return acct, release, nil
}

// admitLocked implements work-conserving per-key fairness. Caller holds schedMu.
// Returns true (and increments inflightKey) when admitted, false on timeout.
func (p *AccountPool) admitLocked(keyID string, floor int, isAdmin bool, distinctAccounts int) bool {
	if isAdmin {
		p.inflightKey[keyID]++ // counted but never gated
		return true
	}

	deadline := time.Now().Add(admitTimeout)
	// Ensure a waiter is woken at the deadline to re-check the timeout, since
	// sync.Cond has no timed wait.
	timer := time.AfterFunc(admitTimeout, func() {
		p.schedMu.Lock()
		p.schedCond.Broadcast()
		p.schedMu.Unlock()
	})
	defer timer.Stop()

	poolCap := distinctAccounts * perAccountSoftConcurrency
	for {
		// Work-conserving: spare capacity in the pool → admit immediately,
		// regardless of this key's current load (idle → no per-key limit).
		if p.inflightAll < poolCap {
			p.inflightKey[keyID]++
			return true
		}
		// Contended: bound the key to its fair share (floored at its baseline).
		activeKeys := len(p.inflightKey)
		if _, ok := p.inflightKey[keyID]; !ok {
			activeKeys++ // this key is about to become active
		}
		if activeKeys < 1 {
			activeKeys = 1
		}
		fairCap := poolCap / activeKeys
		if fairCap < floor {
			fairCap = floor
		}
		if p.inflightKey[keyID] < fairCap {
			p.inflightKey[keyID]++
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		p.schedCond.Wait() // releases schedMu; re-acquires on wake
	}
}

// selectLocked picks the account for this request. Caller holds schedMu.
// Sticky binding wins when its account is still eligible; otherwise the highest
// priority tier's least-loaded account is chosen and the binding is (re)set by
// the caller.
func (p *AccountPool) selectLocked(snap []snapAcct, conversationID string) config.Account {
	if conversationID != "" {
		if ref, ok := p.sticky[conversationID]; ok {
			for i := range snap {
				if snap[i].acct.ID == ref.accountID {
					p.stickyHits.Add(1)
					return snap[i].acct
				}
			}
			// Bound account no longer eligible → drop stale binding, re-select.
			delete(p.sticky, conversationID)
		}
		p.stickyMisses.Add(1)
	}

	// Highest priority tier present in the snapshot.
	maxPrio := snap[0].priority
	for i := range snap {
		if snap[i].priority > maxPrio {
			maxPrio = snap[i].priority
		}
	}
	// Least in-flight within that tier (even split; soft per-account target).
	best := snap[0].acct
	bestLoad := -1
	for i := range snap {
		if snap[i].priority != maxPrio {
			continue
		}
		load := p.inflightAcct[snap[i].acct.ID]
		if bestLoad < 0 || load < bestLoad {
			bestLoad = load
			best = snap[i].acct
		}
	}
	return best
}

// ReportOutcome feeds the result of a dispatched request back into the pool so
// selection adapts: 429 → short exponential backoff, 401/403 → disable after N
// consecutive, 402/quota → longer cooldown, model-scoped restriction → cool the
// (account, model) pair only, success → clear error/cooldown.
func (p *AccountPool) ReportOutcome(accountID, conversationID, model string, outcome Outcome) {
	switch outcome {
	case OutcomeSuccess:
		p.mu.Lock()
		delete(p.cooldowns, accountID)
		p.errorCounts[accountID] = 0
		if model != "" {
			key := modelScopeKey(accountID, model)
			delete(p.modelCooldowns, key)
			delete(p.modelErrorCounts, key)
		}
		p.mu.Unlock()

	case OutcomeRateLimited:
		p.mu.Lock()
		p.errorCounts[accountID]++
		n := p.errorCounts[accountID]
		p.mu.Unlock()
		shift := n - 1
		if shift > 5 {
			shift = 5
		}
		backoff := rateLimitBackoffBase << uint(shift)
		if backoff > rateLimitBackoffMax {
			backoff = rateLimitBackoffMax
		}
		p.mu.Lock()
		p.cooldowns[accountID] = time.Now().Add(backoff)
		p.mu.Unlock()

	case OutcomeAuthError:
		p.mu.Lock()
		p.errorCounts[accountID]++
		n := p.errorCounts[accountID]
		p.mu.Unlock()
		if n >= authDisableThreshold {
			p.unbindAccount(accountID)
			p.DisableAccount(accountID, "repeated auth failure (401/403)")
		} else {
			p.mu.Lock()
			p.cooldowns[accountID] = time.Now().Add(30 * time.Second)
			p.mu.Unlock()
		}

	case OutcomeQuotaExhausted:
		p.mu.Lock()
		p.cooldowns[accountID] = time.Now().Add(quotaCooldown)
		p.mu.Unlock()

	case OutcomeTransient:
		p.mu.Lock()
		p.errorCounts[accountID]++
		if p.errorCounts[accountID] >= 3 {
			p.cooldowns[accountID] = time.Now().Add(time.Minute)
		}
		p.mu.Unlock()

	case OutcomeModelRestricted:
		// The upstream failure is scoped to this MODEL on this account (per-model
		// risk control, empty streams). Freezing the account here punished healthy
		// models sharing it — measured 2026-09-28 as rolling whole-pool 503s while
		// opus-4.8 kept serving on the same accounts. Cool the pair only; a success
		// on the same pair clears it, expiry doubles as the periodic probe.
		// Without a model there is no pair to scope to and the message names no
		// account property — unattributable failures never penalise the account.
		if model == "" {
			return
		}
		key := modelScopeKey(accountID, model)
		p.mu.Lock()
		p.modelErrorCounts[key]++
		if p.modelErrorCounts[key] >= modelRestrictedStrikes {
			p.modelCooldowns[key] = time.Now().Add(modelRestrictedCooldown)
		}
		p.mu.Unlock()
	}
}

// unbindAccount removes every sticky binding pointing at accountID (used when the
// account is disabled so its conversations re-route on the next request).
func (p *AccountPool) unbindAccount(accountID string) {
	p.schedMu.Lock()
	for cid, ref := range p.sticky {
		if ref.accountID == accountID {
			delete(p.sticky, cid)
		}
	}
	p.schedMu.Unlock()
}

// stickySessionTTL bounds how long an idle conversation→account binding is kept.
// A binding is only touched (lastSeen updated) while its conversation is active;
// once a conversation goes quiet its binding is dead weight, so the map grew
// without bound (one entry per conversation id ever seen). Evict bindings unseen
// for this long so a long-running server's sticky map stays proportional to
// ACTIVE conversations, not all-time conversations.
const stickySessionTTL = 24 * time.Hour

// EvictStaleStickySessions drops conversation bindings not seen within
// stickySessionTTL and returns how many were removed. Meant to be called
// periodically from the background maintenance loop.
func (p *AccountPool) EvictStaleStickySessions() int {
	cutoff := time.Now().Add(-stickySessionTTL).Unix()
	removed := 0
	p.schedMu.Lock()
	for cid, ref := range p.sticky {
		if ref.lastSeen < cutoff {
			delete(p.sticky, cid)
			removed++
		}
	}
	p.schedMu.Unlock()
	return removed
}

// StickyMetrics returns cumulative sticky-cache hit/miss counters (for /admin observability).
func (p *AccountPool) StickyMetrics() (hits, misses uint64) {
	return p.stickyHits.Load(), p.stickyMisses.Load()
}

// StickySessions returns the number of currently-bound conversations (live sticky
// sessions) for the /admin overview cache card.
func (p *AccountPool) StickySessions() int {
	p.schedMu.Lock()
	defer p.schedMu.Unlock()
	return len(p.sticky)
}

// ConcurrencySnapshot returns a copy of current in-flight counts (for /admin observability).
func (p *AccountPool) ConcurrencySnapshot() (inflightByAccount map[string]int, inflightByKey map[string]int, total int) {
	p.schedMu.Lock()
	defer p.schedMu.Unlock()
	a := make(map[string]int, len(p.inflightAcct))
	for k, v := range p.inflightAcct {
		a[k] = v
	}
	kk := make(map[string]int, len(p.inflightKey))
	for k, v := range p.inflightKey {
		kk[k] = v
	}
	return a, kk, p.inflightAll
}

// ---- 实时 RPM(最近 60 秒滚动请求数)----
//
// rpmRing is a lock-free-by-convention (guarded by schedMu) ring of 60 one-second
// buckets. add() records one request in the current second; sum() totals only the
// buckets whose recorded second falls within the trailing 60s window, so stale
// buckets from >60s ago are naturally excluded without any background sweeping.
type rpmRing struct {
	buckets [60]int32
	sec     [60]int64 // the unix-second currently stored in each bucket slot
}

func (r *rpmRing) add(now int64) {
	r.addN(now, 1)
}

// addN records n units (e.g. tokens) in the current second's bucket. Shared by
// the request ring (n=1) and the token ring (n=tokens of one completed request).
func (r *rpmRing) addN(now int64, n int32) {
	i := now % 60
	if r.sec[i] != now {
		r.sec[i] = now
		r.buckets[i] = 0
	}
	r.buckets[i] += n
}

func (r *rpmRing) sum(now int64) int {
	var total int32
	for i := 0; i < 60; i++ {
		if now-r.sec[i] < 60 {
			total += r.buckets[i]
		}
	}
	return int(total)
}

// rpmTickLocked records one dispatched request for id in the given ring map.
// Caller holds schedMu.
func (p *AccountPool) rpmTickLocked(m map[string]*rpmRing, id string, now int64) {
	r := m[id]
	if r == nil {
		r = &rpmRing{}
		m[id] = r
	}
	r.add(now)
}

// AccountRPM returns the approximate requests-in-the-last-60s for one account.
func (p *AccountPool) AccountRPM(id string) int {
	now := time.Now().Unix()
	p.schedMu.Lock()
	defer p.schedMu.Unlock()
	if r := p.rpmAcct[id]; r != nil {
		return r.sum(now)
	}
	return 0
}

// effectiveKeyRPMCap resolves the requests-per-60s ceiling for a key under the
// unified nullable semantics:
//
//	per-key MaxRPM nil → inherit the system default (config.GetDefaultMaxRPM)
//	per-key MaxRPM 0   → unlimited
//	per-key MaxRPM N   → that value
//
// A return of 0 means "no RPM gating" (unlimited).
func effectiveKeyRPMCap(keyID string) int {
	if e := config.GetApiKeyEntry(keyID); e != nil && e.MaxRPM != nil {
		v := *e.MaxRPM
		if v < 0 {
			v = 0
		}
		return v // 0 == explicit per-key unlimited, N == that cap
	}
	return config.GetDefaultMaxRPM() // 0 == unlimited
}

// KeyRPM returns the approximate requests-in-the-last-60s for one API key (card).
func (p *AccountPool) KeyRPM(id string) int {
	now := time.Now().Unix()
	p.schedMu.Lock()
	defer p.schedMu.Unlock()
	if r := p.rpmKey[id]; r != nil {
		return r.sum(now)
	}
	return 0
}

// TotalRPM returns the global requests-in-the-last-60s across all accounts.
func (p *AccountPool) TotalRPM() int {
	now := time.Now().Unix()
	p.schedMu.Lock()
	defer p.schedMu.Unlock()
	return p.rpmAll.sum(now)
}

// ---- 实时 TPM(最近 300 秒滚动 token 数,÷5 归一化为每分钟)----
//
// Tokens land in the ring in one lump when a request COMPLETES (unlike RPM,
// which ticks at dispatch), so completions are sparse pulses: with a 60s
// window the TPM tile was a square wave — a big request's full token count
// rode the window for exactly 60s and then dropped off a cliff. A 5-minute
// window with per-minute normalization spreads each pulse over 300s, giving a
// smooth rise on completion and a gentle decay when traffic stops. The shared
// rpmRing (60s, request counts) is untouched.
const tpmWindowSeconds = 300

// tpmRing is the trailing-300s token counterpart of rpmRing (same convention:
// guarded by schedMu, stale buckets excluded by their recorded second).
type tpmRing struct {
	buckets [tpmWindowSeconds]int32
	sec     [tpmWindowSeconds]int64 // the unix-second currently stored in each bucket slot
}

func (r *tpmRing) addN(now int64, n int32) {
	i := now % tpmWindowSeconds
	if r.sec[i] != now {
		r.sec[i] = now
		r.buckets[i] = 0
	}
	r.buckets[i] += n
}

func (r *tpmRing) sum(now int64) int64 {
	var total int64
	for i := 0; i < tpmWindowSeconds; i++ {
		if now-r.sec[i] < tpmWindowSeconds {
			total += int64(r.buckets[i])
		}
	}
	return total
}

// RecordTokens folds one completed request's token total into the trailing-300s
// token ring (the TPM counterpart of the per-dispatch RPM tick). Tokens are
// recorded at completion time — same semantics the admin UI previously
// approximated client-side by differencing the totalTokens counter.
func (p *AccountPool) RecordTokens(tokens int) {
	if tokens <= 0 {
		return
	}
	now := time.Now().Unix()
	p.schedMu.Lock()
	p.tpmAll.addN(now, int32(tokens))
	p.schedMu.Unlock()
}

// TotalTPM returns the deployment-wide tokens-per-minute figure for the
// dashboard's 实时 TPM tile: tokens recorded (at request completion) over the
// trailing 300s, divided by 5 to normalize to a per-minute rate. See the
// tpmRing comment for why the window is wider than RPM's.
func (p *AccountPool) TotalTPM() int {
	now := time.Now().Unix()
	p.schedMu.Lock()
	defer p.schedMu.Unlock()
	return int(p.tpmAll.sum(now) / (tpmWindowSeconds / 60))
}

// RPMSnapshot returns per-account and per-key RPM plus the global total in one
// pass (for the dashboard / list views), omitting zero entries.
func (p *AccountPool) RPMSnapshot() (byAccount map[string]int, byKey map[string]int, total int) {
	now := time.Now().Unix()
	p.schedMu.Lock()
	defer p.schedMu.Unlock()
	byAccount = make(map[string]int, len(p.rpmAcct))
	for id, r := range p.rpmAcct {
		if v := r.sum(now); v > 0 {
			byAccount[id] = v
		}
	}
	byKey = make(map[string]int, len(p.rpmKey))
	for id, r := range p.rpmKey {
		if v := r.sum(now); v > 0 {
			byKey[id] = v
		}
	}
	total = p.rpmAll.sum(now)
	return
}
