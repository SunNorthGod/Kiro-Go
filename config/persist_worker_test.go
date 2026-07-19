package config

import (
	"path/filepath"
	"testing"
	"time"
)

// The coalescing stats worker must eventually persist the latest snapshot for an
// account into the in-memory mirror (JSON mode), and coalesce a burst so the
// final value is the last one enqueued.
func TestEnqueueAccountStatsCoalesces(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := Init(cfgFile); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := AddAccount(Account{ID: "acct-1", Email: "a@b.c", Enabled: true}); err != nil {
		t.Fatalf("add account: %v", err)
	}

	// Burst of increasing cumulative snapshots; the latest (100) must win.
	for i := 1; i <= 100; i++ {
		EnqueueAccountStats("acct-1", i, 0, i*10, float64(i), int64(i))
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		accounts := GetAccounts()
		var got *Account
		for i := range accounts {
			if accounts[i].ID == "acct-1" {
				got = &accounts[i]
				break
			}
		}
		if got != nil && got.RequestCount == 100 {
			if got.TotalTokens != 1000 || got.TotalCredits != 100 {
				t.Fatalf("unexpected persisted stats: %+v", got)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("stats worker did not persist latest snapshot in time (got %+v)", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
