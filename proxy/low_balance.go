package proxy

import (
	"kiro-go/config"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

// low_balance.go is a CONSERVATIVE mitigation for the credit check-then-act
// overdraft window: authenticate() checks the balance before the request, but
// the real cost only lands at stream end (meteringEvent), so N concurrent
// requests can all pass the check on the same nearly-empty card and drive the
// balance negative. Instead of rewriting billing into a pre-hold/escrow ledger,
// we serialize (concurrency=1) the requests of any card whose remaining balance
// is below a threshold: at most ONE request's cost can race past the balance
// check at a time, bounding the worst-case overdraft to roughly one request.
// Overdrafts that still occur are logged (config.recordApiKeyUsage) and NOT
// clawed back. This is explicitly a partial mitigation, not a full fix.

// defaultLowBalanceThreshold is the remaining balance (credits) below which a
// card's requests serialize. Override with env LOW_BALANCE_SERIALIZE_THRESHOLD
// (float, credits; 0 disables the gate).
const defaultLowBalanceThreshold = 10.0

// lowBalanceWaitTimeout bounds how long a serialized request waits for its turn
// before being rejected with 429.
const lowBalanceWaitTimeout = 30 * time.Second

var lowBalanceThreshold = func() float64 {
	if v := os.Getenv("LOW_BALANCE_SERIALIZE_THRESHOLD"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
			return f
		}
	}
	return defaultLowBalanceThreshold
}()

// lowBalanceGate is a one-slot semaphore for a single key, with a last-touch
// timestamp so idle gates can be swept.
type lowBalanceGate struct {
	slot      chan struct{}
	lastTouch int64 // unix seconds, guarded by lowBalanceMu on write
}

var (
	lowBalanceMu    sync.Mutex
	lowBalanceGates = make(map[string]*lowBalanceGate)
)

func lowBalanceGateFor(keyID string) *lowBalanceGate {
	lowBalanceMu.Lock()
	defer lowBalanceMu.Unlock()
	g := lowBalanceGates[keyID]
	if g == nil {
		g = &lowBalanceGate{slot: make(chan struct{}, 1)}
		lowBalanceGates[keyID] = g
	}
	g.lastTouch = time.Now().Unix()
	return g
}

// sweepLowBalanceGates drops gates not touched within maxIdle (called from the
// periodic background refresh so the map cannot grow unbounded).
func sweepLowBalanceGates(maxIdle time.Duration) {
	cutoff := time.Now().Add(-maxIdle).Unix()
	lowBalanceMu.Lock()
	defer lowBalanceMu.Unlock()
	for id, g := range lowBalanceGates {
		if g.lastTouch < cutoff && len(g.slot) == 0 {
			delete(lowBalanceGates, id)
		}
	}
}

// gateLowBalance serializes the request when the caller's key balance is below
// the threshold. Returns (release, true) — release is nil when no gating
// applies — or (nil, false) after a wait timeout (caller must respond 429).
func gateLowBalance(r *http.Request) (func(), bool) {
	if lowBalanceThreshold <= 0 {
		return nil, true
	}
	keyID := apiKeyIDFromContext(r.Context())
	if keyID == "" {
		return nil, true
	}
	entry := config.GetApiKeyEntry(keyID)
	if entry == nil || entry.CreditsGranted <= 0 {
		return nil, true
	}
	if entry.CreditsGranted-entry.CreditsUsed >= lowBalanceThreshold {
		return nil, true
	}
	g := lowBalanceGateFor(keyID)
	timer := time.NewTimer(lowBalanceWaitTimeout)
	defer timer.Stop()
	select {
	case g.slot <- struct{}{}:
		return func() { <-g.slot }, true
	case <-timer.C:
		return nil, false
	case <-r.Context().Done():
		return nil, false
	}
}

const lowBalanceRejectMessage = "credit balance is nearly exhausted; requests for this key are serialized — retry shortly"
