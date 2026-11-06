package pool

import (
	"kiro-go/config"
	"testing"
	"time"
)

// CooldownWaitHint decides whether a handler parks a request or 503s it, so its
// three verdicts must stay exact: transient cooldown → wait the earliest lift;
// any enabled account without a cooldown → waiting cannot help; nothing enabled
// → nothing to wait for.
//
// Test accounts declare Enabled: true explicitly — Reload only ever loads
// enabled accounts into p.accounts, so the zero value here would silently model
// a pool the scheduler can never see.

func TestCooldownWaitHint(t *testing.T) {
	now := time.Now()

	t.Run("all cooling returns the earliest lift", func(t *testing.T) {
		p := newSchedTestPool(config.Account{ID: "a", Enabled: true}, config.Account{ID: "b", Enabled: true})
		p.cooldowns["a"] = now.Add(90 * time.Second)
		p.cooldowns["b"] = now.Add(2 * time.Minute)
		hint, ok := p.CooldownWaitHint(map[string]bool{})
		if !ok {
			t.Fatal("expected a hint for a fully cooling pool")
		}
		if hint < 80*time.Second || hint > 91*time.Second {
			t.Fatalf("hint should track the EARLIEST cooldown (a, ~90s), got %s", hint)
		}
	})

	t.Run("one idle account means waiting is pointless", func(t *testing.T) {
		p := newSchedTestPool(config.Account{ID: "a", Enabled: true}, config.Account{ID: "b", Enabled: true})
		p.cooldowns["a"] = now.Add(time.Minute)
		if hint, ok := p.CooldownWaitHint(map[string]bool{}); ok {
			t.Fatalf("b has no cooldown; expected (0,false), got (%s,true)", hint)
		}
	})

	t.Run("excluded accounts are invisible", func(t *testing.T) {
		p := newSchedTestPool(config.Account{ID: "a", Enabled: true}, config.Account{ID: "b", Enabled: true})
		p.cooldowns["a"] = now.Add(time.Minute)
		if _, ok := p.CooldownWaitHint(map[string]bool{"a": true}); ok {
			t.Fatal("the only non-excluded account has no cooldown; hint must be false")
		}
		if _, ok := p.CooldownWaitHint(map[string]bool{"a": true, "b": true}); ok {
			t.Fatal("everything excluded during failover; hint must be false")
		}
	})

	t.Run("disabled accounts never produce a hint", func(t *testing.T) {
		p := newSchedTestPool(config.Account{ID: "a", Enabled: true})
		p.accounts[0].Enabled = false
		p.cooldowns["a"] = now.Add(time.Minute)
		if _, ok := p.CooldownWaitHint(map[string]bool{}); ok {
			t.Fatal("a banned/empty pool must not park requests")
		}
	})

	t.Run("expired cooldowns count as idle", func(t *testing.T) {
		p := newSchedTestPool(config.Account{ID: "a", Enabled: true})
		p.cooldowns["a"] = now.Add(-time.Second)
		if _, ok := p.CooldownWaitHint(map[string]bool{}); ok {
			t.Fatal("a lifted cooldown means Acquire should succeed; no hint")
		}
	})

	t.Run("model-scoped restrictions are not account cooldowns", func(t *testing.T) {
		p := newSchedTestPool(config.Account{ID: "a", Enabled: true})
		p.modelCooldowns[modelScopeKey("a", "claude-opus-5.5")] = now.Add(time.Minute)
		if _, ok := p.CooldownWaitHint(map[string]bool{}); ok {
			t.Fatal("modelCooldowns are account-scoped hint blind spots by design")
		}
	})
}
