package pool

import (
	"kiro-go/config"
	"testing"
	"time"
)

// An empty eligibility snapshot has to say WHY. Six independent filters can empty
// it, each with a different fix, and the HTTP 503 they all produce is identical.
// Diagnosing a real burst of them meant inferring the cause from outside the
// process — the responses came back in 0.00s, no app log line accompanied them, no
// request_logs row exists (a row needs an account_id, and none was chosen), and
// every account reported error_count 0. These tests pin the attribution so that
// never has to be guessed again.

func TestSnapshotStatsAttributeEachExclusion(t *testing.T) {
	now := time.Now()

	cases := []struct {
		name    string
		build   func(*AccountPool)
		model   string
		exclude map[string]bool
		bound   []string
		want    func(snapshotStats) (bool, string)
	}{
		{
			name:  "bound elsewhere",
			build: func(p *AccountPool) {},
			bound: []string{"not-in-pool"},
			want: func(s snapshotStats) (bool, string) {
				return s.notBound == 2 && s.pooled == 2, "notBound"
			},
		},
		{
			name:    "already tried during failover",
			build:   func(p *AccountPool) {},
			exclude: map[string]bool{"a": true, "b": true},
			want: func(s snapshotStats) (bool, string) {
				return s.retried == 2, "alreadyTried"
			},
		},
		{
			name: "model not served",
			build: func(p *AccountPool) {
				p.modelLists["a"] = map[string]bool{"claude-opus-5": true}
				p.modelLists["b"] = map[string]bool{"claude-opus-5": true}
			},
			model: "some-model-nobody-has",
			want: func(s snapshotStats) (bool, string) {
				return s.noModel == 2, "noModel"
			},
		},
		{
			name: "cooling down",
			build: func(p *AccountPool) {
				p.cooldowns["a"] = now.Add(time.Minute)
				p.cooldowns["b"] = now.Add(time.Minute)
			},
			want: func(s snapshotStats) (bool, string) {
				return s.cooling == 2, "cooling"
			},
		},
		{
			name: "inside the token refresh skew window",
			build: func(p *AccountPool) {
				// Expiring in 30s: inside the 120s skew, so invisible to the
				// scheduler even though the token still works. Only
				// backgroundRefresh (a 5-minute ticker) can rescue it — the request
				// path's ensureValidToken runs after Acquire and never sees it.
				p.accounts[0].ExpiresAt = now.Unix() + 30
				p.accounts[1].ExpiresAt = now.Unix() + 30
			},
			want: func(s snapshotStats) (bool, string) {
				return s.nearExpiry == 2, "nearTokenExpiry"
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newSchedTestPool(config.Account{ID: "a"}, config.Account{ID: "b"})
			tc.build(p)

			snap, stats := p.eligibleSnapshot(tc.model, tc.exclude, tc.bound)
			if len(snap) != 0 {
				t.Fatalf("expected an empty snapshot, got %d accounts", len(snap))
			}
			if ok, field := tc.want(stats); !ok {
				t.Fatalf("%s not attributed: stats=%+v", field, stats)
			}
		})
	}
}

// A healthy pool must report every filter as zero, so a nonzero count in the log
// is always meaningful.
func TestSnapshotStatsAreZeroForAHealthyPool(t *testing.T) {
	p := newSchedTestPool(config.Account{ID: "a"}, config.Account{ID: "b"})

	snap, stats := p.eligibleSnapshot("", nil, nil)
	if len(snap) != 2 {
		t.Fatalf("expected both accounts eligible, got %d", len(snap))
	}
	if stats.pooled != 2 {
		t.Fatalf("pooled = %d, want 2", stats.pooled)
	}
	if stats.notBound|stats.retried|stats.noModel|stats.cooling|stats.nearExpiry|stats.quota != 0 {
		t.Fatalf("a healthy pool reported exclusions: %+v", stats)
	}
}

// The failover path is the case that most needs naming: a request that was tried
// on every account and rejected by all of them ends in the same ErrNoAccount as a
// cold pool, and the client is told "no available accounts" when the truth is
// "every account refused this request".
func TestExhaustedFailoverIsDistinguishableFromAColdPool(t *testing.T) {
	p := newSchedTestPool(config.Account{ID: "a"}, config.Account{ID: "b"})

	_, exhausted := p.eligibleSnapshot("", map[string]bool{"a": true, "b": true}, nil)
	if exhausted.retried != 2 || exhausted.cooling != 0 {
		t.Fatalf("exhausted failover misattributed: %+v", exhausted)
	}

	p2 := newSchedTestPool(config.Account{ID: "a"}, config.Account{ID: "b"})
	p2.cooldowns["a"] = time.Now().Add(time.Minute)
	p2.cooldowns["b"] = time.Now().Add(time.Minute)
	_, cold := p2.eligibleSnapshot("", nil, nil)
	if cold.cooling != 2 || cold.retried != 0 {
		t.Fatalf("cold pool misattributed: %+v", cold)
	}
}

// Acquire must still return ErrNoAccount, and logging must not panic or block on
// the burst path (the rate limiter is exercised by calling it repeatedly).
func TestAcquireStillReturnsErrNoAccountAndLogsWithoutBlocking(t *testing.T) {
	p := newSchedTestPool(config.Account{ID: "a"})
	p.cooldowns["a"] = time.Now().Add(time.Minute)

	for i := 0; i < 50; i++ {
		if _, _, err := p.Acquire("k", nil, false, "", "", nil, nil); err != ErrNoAccount {
			t.Fatalf("iteration %d: got %v, want ErrNoAccount", i, err)
		}
	}
}

// ---- the model filter must not be the sole reason for a 503 ----

// TestUnadvertisedModelRoutesAnywayInsteadOf503 is the regression for the measured
// cause of every 503 on this build: model="simple-task", noModel=10, with every
// other filter at zero. ListAvailableModels did not advertise it, so the pool
// refused to route it — yet all 12 requests for it that reached the upstream (while
// the model cache was still cold) succeeded. Blocking a model the backend actually
// serves, and reporting it as a capacity problem, is wrong twice over.
func TestUnadvertisedModelRoutesAnywayInsteadOf503(t *testing.T) {
	p := newSchedTestPool(config.Account{ID: "a"}, config.Account{ID: "b"})
	// Warm, complete-looking model lists that simply do not mention the model.
	p.modelLists["a"] = map[string]bool{"claude-opus-5": true}
	p.modelLists["b"] = map[string]bool{"claude-opus-5": true}

	acc, release, err := p.Acquire("k", nil, false, "", "simple-task", nil, nil)
	if err != nil {
		t.Fatalf("unadvertised model still yields %v; the client gets a 503 instead of the upstream's answer", err)
	}
	release()
	if acc.ID == "" {
		t.Fatal("no account returned")
	}

	// An advertised model must still route normally.
	acc2, release2, err := p.Acquire("k", nil, false, "", "claude-opus-5", nil, nil)
	if err != nil {
		t.Fatalf("advertised model failed to route: %v", err)
	}
	release2()
	if acc2.ID == "" {
		t.Fatal("no account returned for an advertised model")
	}
}

// The relaxation must apply ONLY to the model filter. A pool that is empty for any
// other reason must still report ErrNoAccount — otherwise this would route to
// cooling, over-quota or out-of-binding accounts.
func TestModelRelaxationDoesNotBypassOtherFilters(t *testing.T) {
	cases := []struct {
		name  string
		build func(*AccountPool)
		bound []string
		excl  map[string]bool
	}{
		{
			name: "cooling accounts stay out",
			build: func(p *AccountPool) {
				p.modelLists["a"] = map[string]bool{"other": true}
				p.cooldowns["a"] = time.Now().Add(time.Minute)
			},
		},
		{
			name: "token near expiry stays out",
			build: func(p *AccountPool) {
				p.modelLists["a"] = map[string]bool{"other": true}
				p.accounts[0].ExpiresAt = time.Now().Unix() + 30
			},
		},
		{
			name: "out-of-binding stays out",
			build: func(p *AccountPool) {
				p.modelLists["a"] = map[string]bool{"other": true}
			},
			bound: []string{"not-in-pool"},
		},
		{
			name: "already-tried stays out",
			build: func(p *AccountPool) {
				p.modelLists["a"] = map[string]bool{"other": true}
			},
			excl: map[string]bool{"a": true},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newSchedTestPool(config.Account{ID: "a"})
			tc.build(p)
			if _, _, err := p.Acquire("k", nil, false, "", "unknown-model", tc.excl, tc.bound); err != ErrNoAccount {
				t.Fatalf("got %v, want ErrNoAccount — the model relaxation bypassed another filter", err)
			}
		})
	}
}

// With no accounts at all there is nothing to relax to.
func TestEmptyPoolStillReportsNoAccount(t *testing.T) {
	p := newSchedTestPool()
	if _, _, err := p.Acquire("k", nil, false, "", "anything", nil, nil); err != ErrNoAccount {
		t.Fatalf("got %v, want ErrNoAccount for an empty pool", err)
	}
}
