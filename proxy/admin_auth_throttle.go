package proxy

import (
	"sync"
	"time"
)

// admin_auth_throttle.go blunts brute-forcing of the admin password. The admin
// API authenticates with a single shared password compared in constant time,
// but without rate limiting an attacker could try passwords as fast as the
// network allows. This adds per-client-IP failure counting with exponential
// backoff: after a few misses an IP is temporarily blocked, and the block window
// grows with each further miss (capped), while a correct password clears it.

const (
	adminAuthFreeAttempts = 5                // misses allowed before backoff starts
	adminAuthBaseBackoff  = 1 * time.Second  // first backoff step
	adminAuthMaxBackoff   = 5 * time.Minute  // cap on a single block window
	adminAuthEntryTTL     = 1 * time.Hour    // idle entries pruned after this
)

type adminAuthState struct {
	failures     int
	blockedUntil time.Time
	lastSeen     time.Time
}

var (
	adminAuthMu      sync.Mutex
	adminAuthByIP    = map[string]*adminAuthState{}
	adminAuthLastGC  time.Time
)

// adminAuthAllowed reports whether ip may attempt an admin auth right now. When
// blocked it returns the remaining wait so the caller can send Retry-After.
func adminAuthAllowed(ip string) (bool, time.Duration) {
	now := time.Now()
	adminAuthMu.Lock()
	defer adminAuthMu.Unlock()
	adminAuthGCLocked(now)
	st := adminAuthByIP[ip]
	if st == nil {
		return true, 0
	}
	st.lastSeen = now
	if now.Before(st.blockedUntil) {
		return false, st.blockedUntil.Sub(now)
	}
	return true, 0
}

// adminAuthRecordFailure records a failed attempt and, past the free-attempt
// budget, sets an exponentially growing block window.
func adminAuthRecordFailure(ip string) {
	now := time.Now()
	adminAuthMu.Lock()
	defer adminAuthMu.Unlock()
	st := adminAuthByIP[ip]
	if st == nil {
		st = &adminAuthState{}
		adminAuthByIP[ip] = st
	}
	st.failures++
	st.lastSeen = now
	if st.failures > adminAuthFreeAttempts {
		shift := st.failures - adminAuthFreeAttempts - 1
		if shift > 30 {
			shift = 30
		}
		backoff := adminAuthBaseBackoff << uint(shift)
		if backoff <= 0 || backoff > adminAuthMaxBackoff {
			backoff = adminAuthMaxBackoff
		}
		st.blockedUntil = now.Add(backoff)
	}
}

// adminAuthRecordSuccess clears any accrued failures for ip.
func adminAuthRecordSuccess(ip string) {
	adminAuthMu.Lock()
	defer adminAuthMu.Unlock()
	delete(adminAuthByIP, ip)
}

// adminAuthGCLocked drops idle entries so the map stays bounded. Caller holds the mutex.
func adminAuthGCLocked(now time.Time) {
	if now.Sub(adminAuthLastGC) < adminAuthEntryTTL {
		return
	}
	adminAuthLastGC = now
	for ip, st := range adminAuthByIP {
		if now.Sub(st.lastSeen) > adminAuthEntryTTL && now.After(st.blockedUntil) {
			delete(adminAuthByIP, ip)
		}
	}
}
