package pool

import (
	"kiro-go/config"
	"testing"
	"time"
)

// Model-scoped restrictions (upstream per-model risk control, empty streams) must
// cool the (account, model) pair and never the account. Production evidence
// 2026-09-28: an opus-5.5 empty-stream restriction on both healthy accounts put
// the whole pool into rolling 1-minute cooldowns via the transient path, 503ing
// opus-4.8 traffic that was serving fine on the same accounts. These tests pin
// the scoping so that regression can't re-freeze healthy models.

func TestModelRestrictedFailureScopesToPair(t *testing.T) {
	p := newSchedTestPool(config.Account{ID: "a"})
	const restricted = "claude-opus-5.5"
	const healthy = "claude-opus-4.8"

	// Below the strike threshold: still eligible for both models.
	p.ReportOutcome("a", "", restricted, OutcomeModelRestricted)
	p.ReportOutcome("a", "", restricted, OutcomeModelRestricted)
	snap, stats := p.eligibleSnapshot(restricted, nil, nil)
	if len(snap) != 1 {
		t.Fatalf("pair below strike threshold must stay eligible, stats=%+v", stats)
	}

	// Third strike: the pair cools — for THIS model only.
	p.ReportOutcome("a", "", restricted, OutcomeModelRestricted)
	if snap, stats = p.eligibleSnapshot(restricted, nil, nil); len(snap) != 0 {
		t.Fatalf("pair over strike threshold must be excluded, stats=%+v", stats)
	}
	if stats.modelCool != 1 {
		t.Fatalf("modelCool = %d, want 1", stats.modelCool)
	}
	// The account-level cooldown must stay untouched — the account keeps serving
	// every other model.
	if snap, stats = p.eligibleSnapshot(healthy, nil, nil); len(snap) != 1 {
		t.Fatalf("healthy model on the same account must stay eligible, stats=%+v", stats)
	}
	if !p.cooldowns["a"].IsZero() {
		t.Fatalf("account-level cooldown set by a model-scoped failure")
	}
	if p.errorCounts["a"] != 0 {
		t.Fatalf("account error count bumped by a model-scoped failure")
	}

	// Expiry doubles as the probe: once the cooldown lapses the pair is eligible
	// again without any bookkeeping from the request path.
	p.modelCooldowns[modelScopeKey("a", restricted)] = time.Now().Add(-time.Second)
	if snap, _ = p.eligibleSnapshot(restricted, nil, nil); len(snap) != 1 {
		t.Fatalf("expired model cooldown must restore eligibility")
	}
}

func TestSuccessClearsModelScopedCooldown(t *testing.T) {
	p := newSchedTestPool(config.Account{ID: "a"})
	const restricted = "claude-opus-5.5"
	const healthy = "claude-opus-4.8"

	for i := 0; i < modelRestrictedStrikes; i++ {
		p.ReportOutcome("a", "", restricted, OutcomeModelRestricted)
	}
	if _, stats := p.eligibleSnapshot(restricted, nil, nil); stats.modelCool != 1 {
		t.Fatalf("pair should be cooling before the success, stats=%+v", stats)
	}

	// A success on a DIFFERENT model of the same account must not clear the pair:
	// the account being healthy at opus-4.8 says nothing about the opus-5.5
	// restriction.
	p.ReportOutcome("a", "", healthy, OutcomeSuccess)
	if _, stats := p.eligibleSnapshot(restricted, nil, nil); stats.modelCool != 1 {
		t.Fatalf("success on another model must not clear the pair, stats=%+v", stats)
	}

	// A success on the pair itself clears it.
	p.ReportOutcome("a", "", restricted, OutcomeSuccess)
	if snap, _ := p.eligibleSnapshot(restricted, nil, nil); len(snap) != 1 {
		t.Fatalf("success on the pair must clear the model cooldown")
	}
}

func TestModelRestrictedWithoutModelIsNoop(t *testing.T) {
	p := newSchedTestPool(config.Account{ID: "a"})

	// Call sites with no model in scope (background refresh) report an empty model:
	// an unattributable failure must not penalise the account under any key.
	for i := 0; i < modelRestrictedStrikes+1; i++ {
		p.ReportOutcome("a", "", "", OutcomeModelRestricted)
	}
	if len(p.modelCooldowns) != 0 || len(p.modelErrorCounts) != 0 {
		t.Fatalf("empty-model outcome created state: %+v %+v", p.modelCooldowns, p.modelErrorCounts)
	}
	if !p.cooldowns["a"].IsZero() || p.errorCounts["a"] != 0 {
		t.Fatalf("empty-model outcome penalised the account")
	}
	if snap, _ := p.eligibleSnapshot("claude-opus-5.5", nil, nil); len(snap) != 1 {
		t.Fatalf("account must stay eligible after empty-model outcomes")
	}
}
