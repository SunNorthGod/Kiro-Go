package pool

import (
	"errors"
	"kiro-go/config"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// newSchedTestPool builds a pool with the scheduler state (cond + maps)
// initialized, mirroring GetPool, so Acquire/release can be exercised (the
// lighter newTestPool omits the scheduler fields).
func newSchedTestPool(accounts ...config.Account) *AccountPool {
	p := &AccountPool{
		cooldowns:    make(map[string]time.Time),
		errorCounts:  make(map[string]int),
		modelLists:   make(map[string]map[string]bool),
		inflightAcct: make(map[string]int),
		inflightKey:  make(map[string]int),
		sticky:       make(map[string]stickyRef),
		rpmAcct:      make(map[string]*rpmRing),
		rpmKey:       make(map[string]*rpmRing),
		rpmAll:       &rpmRing{},
		tpmAll:       &tpmRing{},
		accounts:     accounts,
	}
	p.schedCond = sync.NewCond(&p.schedMu)
	return p
}

// A key bound to specific accounts may only be routed to those accounts; an
// empty binding allows any; a binding to an out-of-pool id yields no account.
func TestAcquireRespectsBoundAccountIDs(t *testing.T) {
	p := newSchedTestPool(config.Account{ID: "a"}, config.Account{ID: "b"}, config.Account{ID: "c"})

	for i := 0; i < 10; i++ {
		acc, release, err := p.Acquire("key1", 0, false, "", "", nil, []string{"b"})
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		if acc.ID != "b" {
			t.Fatalf("bound key routed to %q, want b", acc.ID)
		}
		release()
	}

	if _, _, err := p.Acquire("key1", 0, false, "", "", nil, []string{"zzz"}); err != ErrNoAccount {
		t.Fatalf("expected ErrNoAccount for out-of-pool binding, got %v", err)
	}

	acc, release, err := p.Acquire("key1", 0, false, "", "", nil, nil)
	if err != nil {
		t.Fatalf("unbound acquire failed: %v", err)
	}
	release()
	if acc.ID == "" {
		t.Fatalf("expected an account for an unbound key")
	}
}

func TestEvictStaleStickySessions(t *testing.T) {
	p := newSchedTestPool(config.Account{ID: "a"})
	now := time.Now().Unix()
	staleAge := int64(48*time.Hour/time.Second)
	p.sticky["fresh"] = stickyRef{accountID: "a", lastSeen: now}
	p.sticky["stale"] = stickyRef{accountID: "a", lastSeen: now - staleAge}

	if removed := p.EvictStaleStickySessions(); removed != 1 {
		t.Fatalf("expected 1 stale binding evicted, got %d", removed)
	}
	if _, ok := p.sticky["fresh"]; !ok {
		t.Fatalf("fresh binding must survive eviction")
	}
	if _, ok := p.sticky["stale"]; ok {
		t.Fatalf("stale binding must be evicted")
	}
}

func TestOverLimitAccountsAreSkippedByDefault(t *testing.T) {
	p := &AccountPool{}
	normal := config.Account{ID: "normal"}
	overLimit := config.Account{ID: "over", UsageCurrent: 10, UsageLimit: 10}

	p.accounts = []config.Account{normal, overLimit}

	for i := 0; i < 5; i++ {
		acc := p.GetNext()
		if acc == nil {
			t.Fatalf("expected an account")
		}
		if acc.ID == "over" {
			t.Fatalf("expected over-limit account to be skipped when upstream OverageStatus is empty")
		}
	}
}

func TestOverLimitAccountsCanBeSelectedWhenUpstreamOverageEnabled(t *testing.T) {
	p := &AccountPool{}
	overLimit := config.Account{
		ID:            "over",
		UsageCurrent:  10,
		UsageLimit:    10,
		OverageStatus: "ENABLED",
	}

	p.accounts = []config.Account{overLimit}

	acc := p.GetNext()
	if acc == nil {
		t.Fatalf("expected upstream-enabled overage account to be selectable")
	}
	if acc.ID != "over" {
		t.Fatalf("expected overage account, got %q", acc.ID)
	}
}

func TestOverLimitAccountsRemainSkippedWhenUpstreamOverageDisabled(t *testing.T) {
	p := &AccountPool{}
	overLimit := config.Account{
		ID:            "over",
		UsageCurrent:  10,
		UsageLimit:    10,
		OverageStatus: "DISABLED",
	}

	p.accounts = []config.Account{overLimit}

	if acc := p.GetNext(); acc != nil {
		t.Fatalf("expected nil when upstream OverageStatus=DISABLED, got %q", acc.ID)
	}
}

func TestGetNextKeepsFiveMinuteTokenAvailable(t *testing.T) {
	p := &AccountPool{}
	account := config.Account{
		ID:          "acct-1",
		AccessToken: "access-token",
		ExpiresAt:   time.Now().Unix() + 300,
	}

	p.accounts = []config.Account{account}

	got := p.GetNext()
	if got == nil {
		t.Fatalf("expected five-minute token to be available")
	}
	if got.ID != account.ID {
		t.Fatalf("expected account %q, got %q", account.ID, got.ID)
	}
}

// ---------------------------------------------------------------------------
// IsAuthFailure
// ---------------------------------------------------------------------------

func TestIsAuthFailureRecognizes401And403(t *testing.T) {
	positives := []string{
		"HTTP 401 from server",
		"received 403 Forbidden",
		"bad credentials",
		"invalid_grant",
		"invalid_token",
		"token expired",
		"token has expired",
		"unauthorized",
	}
	for _, msg := range positives {
		if !IsAuthFailure(errors.New(msg)) {
			t.Errorf("IsAuthFailure(%q) = false, want true", msg)
		}
	}
}

func TestIsAuthFailureIgnoresFalsePositives(t *testing.T) {
	// hasStatusToken only excludes digit boundaries; e.g. "4011" contains "401"
	// but the trailing '1' is a digit so it does NOT match.
	negatives := []string{
		"status code 4011 found", // digit immediately after 401 → not a standalone token
		"error 14013 exceeded",   // digit before and after 401
		"some random error",
		"status 200 OK",
	}
	for _, msg := range negatives {
		if IsAuthFailure(errors.New(msg)) {
			t.Errorf("IsAuthFailure(%q) = true, want false", msg)
		}
	}
}

func TestIsAuthFailureNilError(t *testing.T) {
	if IsAuthFailure(nil) {
		t.Fatal("IsAuthFailure(nil) = true, want false")
	}
}

// ---------------------------------------------------------------------------
// IsSuspensionError
// ---------------------------------------------------------------------------

func TestIsSuspensionErrorDetectsKnownMessages(t *testing.T) {
	positives := []string{
		"account temporarily_suspended",
		"account temporarily suspended",
		"no available kiro profile",
		"No Available Kiro Profile", // case-insensitive
	}
	for _, msg := range positives {
		if !IsSuspensionError(errors.New(msg)) {
			t.Errorf("IsSuspensionError(%q) = false, want true", msg)
		}
	}
}

func TestIsSuspensionErrorIgnoresUnrelatedErrors(t *testing.T) {
	negatives := []string{
		"some other error",
		"unauthorized",
		"429 too many requests",
	}
	for _, msg := range negatives {
		if IsSuspensionError(errors.New(msg)) {
			t.Errorf("IsSuspensionError(%q) = true, want false", msg)
		}
	}
}

func TestIsSuspensionErrorNilError(t *testing.T) {
	if IsSuspensionError(nil) {
		t.Fatal("IsSuspensionError(nil) = true, want false")
	}
}

// ---------------------------------------------------------------------------
// GetNextForModelExcluding
// ---------------------------------------------------------------------------

func newTestPool(accounts ...config.Account) *AccountPool {
	p := &AccountPool{
		cooldowns:   make(map[string]time.Time),
		errorCounts: make(map[string]int),
		modelLists:  make(map[string]map[string]bool),
	}
	p.accounts = accounts
	return p
}

func TestGetNextForModelExcludingSkipsExcludedAccounts(t *testing.T) {
	p := newTestPool(
		config.Account{ID: "a"},
		config.Account{ID: "b"},
	)
	excluded := map[string]bool{"a": true}
	for i := 0; i < 5; i++ {
		acc := p.GetNextForModelExcluding("model", excluded)
		if acc == nil {
			t.Fatal("expected account b, got nil")
		}
		if acc.ID == "a" {
			t.Fatalf("excluded account a was returned on iteration %d", i)
		}
	}
}

func TestGetNextForModelExcludingReturnsNilWhenAllExcluded(t *testing.T) {
	p := newTestPool(config.Account{ID: "only"})
	acc := p.GetNextForModelExcluding("model", map[string]bool{"only": true})
	if acc != nil {
		t.Fatalf("expected nil when only account is excluded, got %q", acc.ID)
	}
}

func TestGetNextForModelExcludingReturnsNilOnEmptyPool(t *testing.T) {
	p := newTestPool()
	acc := p.GetNextForModelExcluding("model", map[string]bool{})
	if acc != nil {
		t.Fatalf("expected nil for empty pool, got %q", acc.ID)
	}
}

// ---------------------------------------------------------------------------
// DisableAccount
// ---------------------------------------------------------------------------

func TestDisableAccountSetsCooldown(t *testing.T) {
	// Initialize a temporary config so SetAccountBanStatus can persist safely.
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	p := newTestPool()
	p.DisableAccount("test-id", "test reason")

	p.mu.RLock()
	cooldown, ok := p.cooldowns["test-id"]
	p.mu.RUnlock()

	if !ok {
		t.Fatal("expected cooldown to be set after DisableAccount")
	}
	// Safety-net cooldown must be at least 23 hours from now.
	minExpected := time.Now().Add(23 * time.Hour)
	if cooldown.Before(minExpected) {
		t.Fatalf("expected cooldown >= 23h in future, got %v", cooldown)
	}
}

func TestGetNextExcludingSkipsExcludedAccount(t *testing.T) {
	p := &AccountPool{
		accounts: []config.Account{
			{ID: "a", Enabled: true},
			{ID: "b", Enabled: true},
		},
		cooldowns:    make(map[string]time.Time),
		errorCounts:  make(map[string]int),
		modelLists:   make(map[string]map[string]bool),
		currentIndex: ^uint64(0),
	}

	acc := p.GetNextExcluding(map[string]bool{"a": true})
	if acc == nil || acc.ID != "b" {
		t.Fatalf("expected account b, got %#v", acc)
	}
}

func TestGetNextForModelExcludingSkipsExcludedAccount(t *testing.T) {
	p := &AccountPool{
		accounts: []config.Account{
			{ID: "a", Enabled: true},
			{ID: "b", Enabled: true},
		},
		cooldowns:    make(map[string]time.Time),
		errorCounts:  make(map[string]int),
		modelLists:   make(map[string]map[string]bool),
		currentIndex: ^uint64(0),
	}
	p.SetModelList("a", []string{"claude-sonnet-4.5"})
	p.SetModelList("b", []string{"claude-sonnet-4.5"})

	acc := p.GetNextForModelExcluding("claude-sonnet-4.5", map[string]bool{"a": true})
	if acc == nil || acc.ID != "b" {
		t.Fatalf("expected account b, got %#v", acc)
	}
}

// ---------------------------------------------------------------------------
// Reload over-usage filtering
// ---------------------------------------------------------------------------

// Over-quota accounts stay routable only via the per-account upstream Overages
// switch (OverageStatus=ENABLED). The former global allowOverUsage override was
// removed, so overage is now strictly account-level.
func TestReloadKeepsOverQuotaAccountWhenOverageEnabled(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:            "over",
		Enabled:       true,
		UsageCurrent:  10,
		UsageLimit:    10,
		OverageStatus: "ENABLED",
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	p := newTestPool()
	p.Reload()

	if got := p.GetNext(); got == nil || got.ID != "over" {
		t.Fatalf("expected over-quota account to remain routable when OverageStatus=ENABLED, got %#v", got)
	}
}

func TestReloadDropsOverQuotaAccountWhenAllowOverUsageDisabled(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:           "over",
		Enabled:      true,
		UsageCurrent: 10,
		UsageLimit:   10,
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	p := newTestPool()
	p.Reload()

	if got := p.GetNext(); got != nil {
		t.Fatalf("expected over-quota account to be dropped, got %q", got.ID)
	}
}

// ---------------------------------------------------------------------------
// TPM ring (trailing-300s token window, ÷5-normalized to per-minute)
// ---------------------------------------------------------------------------

// A completed request's token pulse must stay in the ring for the full 300s
// window and age out afterwards — the old 60s window turned each pulse into a
// square wave on the dashboard tile.
func TestTpmRingWindowAndAging(t *testing.T) {
	r := &tpmRing{}
	base := time.Now().Unix()

	r.addN(base, 50000)
	if got := r.sum(base); got != 50000 {
		t.Fatalf("sum at t0 = %d, want 50000", got)
	}
	if got := r.sum(base + 299); got != 50000 {
		t.Fatalf("sum at t+299 = %d, want 50000 (still inside the 300s window)", got)
	}
	if got := r.sum(base + 300); got != 0 {
		t.Fatalf("sum at t+300 = %d, want 0 (aged out)", got)
	}

	// Pulses landing in different seconds accumulate; each ages out on its own.
	r2 := &tpmRing{}
	r2.addN(base, 1000)
	r2.addN(base+120, 2000)
	if got := r2.sum(base + 120); got != 3000 {
		t.Fatalf("sum at t+120 = %d, want 3000 (both pulses in window)", got)
	}
	if got := r2.sum(base + 350); got != 2000 {
		t.Fatalf("sum at t+350 = %d, want 2000 (first pulse aged out)", got)
	}
}

// TotalTPM must report the 300s token sum normalized to a per-minute rate
// (÷5), so a lone 50k-token completion reads as 10k TPM — the 5-minute mean —
// rather than 50k for exactly 60s and then 0.
func TestTotalTPMNormalizesToPerMinute(t *testing.T) {
	p := newSchedTestPool()
	p.RecordTokens(50000)
	if got := p.TotalTPM(); got != 10000 {
		t.Fatalf("TotalTPM = %d, want 10000 (50000 over 300s ÷ 5)", got)
	}
	p.RecordTokens(25000)
	if got := p.TotalTPM(); got != 15000 {
		t.Fatalf("TotalTPM = %d, want 15000 after second pulse", got)
	}
}
