package pool

import (
	"errors"
	"kiro-go/config"
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

// Tunables. perKeyConcurrencyFloor / perAccountSoftConcurrency are the *baselines*
// used by the work-conserving fair scheduler, not hard ceilings: idle capacity is
// always lent out. They can later be sourced from config; the defaults match the
// agreed values (5 per key, 20 per account, global unbounded).
const (
	defaultPerKeyConcurrencyFloor = 5
	perAccountSoftConcurrency     = 20

	admitTimeout         = 8 * time.Second
	rateLimitBackoffBase = 1500 * time.Millisecond
	rateLimitBackoffMax  = 60 * time.Second
	authDisableThreshold = 3
	quotaCooldown        = 10 * time.Minute
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
)

// snapAcct is an eligible account value copy plus its priority tier.
type snapAcct struct {
	acct     config.Account
	priority int
}

// eligibleSnapshot returns value copies of every currently-eligible account for
// the given model (deduped by ID), captured under mu.RLock. Returning copies —
// never &p.accounts[i] — is what fixes the data race where the request path read
// a pool-internal account while a token refresh wrote it.
//
// boundAccountIDs, when non-empty, restricts selection to the intersection with
// the pool (a key bound to specific accounts may only use those); empty means any
// account is allowed.
func (p *AccountPool) eligibleSnapshot(model string, excluded map[string]bool, boundAccountIDs []string) []snapAcct {
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

	for i := range p.accounts {
		a := p.accounts[i]
		if seen[a.ID] {
			continue
		}
		seen[a.ID] = true
		if boundSet != nil && !boundSet[a.ID] {
			continue
		}
		if excluded != nil && excluded[a.ID] {
			continue
		}
		if model != "" && !p.accountHasModel(a.ID, model) {
			continue
		}
		if cd, ok := p.cooldowns[a.ID]; ok && now.Before(cd) {
			continue
		}
		if a.ExpiresAt > 0 && now.Unix() > a.ExpiresAt-tokenRefreshSkewSeconds {
			continue
		}
		if isQuotaBlocked(a, allowOverUsage) {
			continue
		}
		out = append(out, snapAcct{acct: a, priority: a.Weight})
	}
	return out
}

// Acquire admits the request (per-key fairness), selects an account
// (sticky → priority-tier least-loaded), increments in-flight counters, and
// returns the chosen account (a copy) plus a release func that MUST be called
// exactly once when the request finishes (success, error, or client disconnect).
//
// keyConcurrencyFloor is the per-key guaranteed baseline (0 → default 5); the
// effective fair cap under contention is max(floor, poolCapacity/activeKeys).
// isAdmin bypasses the fairness gate entirely (still counted for observability).
// boundAccountIDs restricts selection to those accounts (empty = any).
func (p *AccountPool) Acquire(keyID string, keyConcurrencyFloor int, isAdmin bool, conversationID, model string, excluded map[string]bool, boundAccountIDs []string) (config.Account, func(), error) {
	snap := p.eligibleSnapshot(model, excluded, boundAccountIDs)
	if len(snap) == 0 {
		return config.Account{}, nil, ErrNoAccount
	}
	distinct := len(snap)
	// A negative floor is the "unlimited" tier: the key bypasses the fairness
	// gate entirely (still counted for observability), like an admin/master key.
	unlimited := keyConcurrencyFloor < 0
	if keyConcurrencyFloor <= 0 {
		keyConcurrencyFloor = defaultPerKeyConcurrencyFloor
	}

	p.schedMu.Lock()
	if !p.admitLocked(keyID, keyConcurrencyFloor, isAdmin || unlimited, distinct) {
		p.schedMu.Unlock()
		return config.Account{}, nil, ErrTooBusy
	}
	// Admitted: inflightKey[keyID] already incremented by admitLocked.
	acct := p.selectLocked(snap, conversationID)
	p.inflightAcct[acct.ID]++
	p.inflightAll++
	nowSec := time.Now().Unix()
	p.rpmTickLocked(p.rpmAcct, acct.ID, nowSec)
	if keyID != "" {
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
// consecutive, 402/quota → longer cooldown, success → clear error/cooldown.
func (p *AccountPool) ReportOutcome(accountID, conversationID string, outcome Outcome) {
	switch outcome {
	case OutcomeSuccess:
		p.mu.Lock()
		delete(p.cooldowns, accountID)
		p.errorCounts[accountID] = 0
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

// RecordTokens folds one completed request's token total into the trailing-60s
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

// TotalTPM returns the tokens recorded (at request completion) in the trailing
// 60 seconds, deployment-wide. Serves the dashboard's 实时 TPM tile so the
// frontend renders a backend-computed figure instead of sampling deltas itself.
func (p *AccountPool) TotalTPM() int {
	now := time.Now().Unix()
	p.schedMu.Lock()
	defer p.schedMu.Unlock()
	return p.tpmAll.sum(now)
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
