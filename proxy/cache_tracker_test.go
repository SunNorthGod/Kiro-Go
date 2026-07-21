package proxy

import (
	"strings"
	"testing"
	"time"
)

func TestPromptCacheTrackerComputeAndUpdate(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	longSystem := strings.Repeat("You are a helpful coding assistant with deep knowledge of Go, Rust, Python, and TypeScript. ", 80)
	req := &ClaudeRequest{
		Model: "claude-sonnet-4.5",
		System: []interface{}{
			map[string]interface{}{
				"type": "text",
				"text": longSystem,
				"cache_control": map[string]interface{}{
					"type": "ephemeral",
				},
			},
		},
		Messages: []ClaudeMessage{{Role: "user", Content: "hello world"}},
	}

	profile := tracker.BuildClaudeProfile(req, 120)
	if profile == nil {
		t.Fatalf("expected cache profile to be built")
	}

	first := tracker.Compute("acct-1", profile)
	if first.CacheCreationInputTokens <= 0 {
		t.Fatalf("expected first request to create cache tokens, got %+v", first)
	}
	if first.CacheReadInputTokens != 0 {
		t.Fatalf("expected first request to have zero cache reads, got %+v", first)
	}

	tracker.Update("acct-1", profile)
	second := tracker.Compute("acct-1", profile)
	if second.CacheReadInputTokens <= 0 {
		t.Fatalf("expected repeated request to read cache tokens, got %+v", second)
	}
	if second.CacheCreationInputTokens != 0 {
		t.Fatalf("expected repeated request to avoid cache creation, got %+v", second)
	}
}

func TestBuildClaudeUsageMapIncludesCacheFields(t *testing.T) {
	usage := promptCacheUsage{
		CacheCreationInputTokens:   30,
		CacheReadInputTokens:       20,
		CacheCreation5mInputTokens: 10,
		CacheCreation1hInputTokens: 20,
	}

	m := buildClaudeUsageMap(100, 50, usage, true, 0)

	if got := m["input_tokens"]; got != 50 {
		t.Fatalf("expected billed input tokens 50, got %#v", got)
	}
	if got := m["cache_creation_input_tokens"]; got != 30 {
		t.Fatalf("expected cache creation tokens 30, got %#v", got)
	}
	if got := m["cache_read_input_tokens"]; got != 20 {
		t.Fatalf("expected cache read tokens 20, got %#v", got)
	}
	creation, ok := m["cache_creation"].(map[string]int)
	if !ok {
		t.Fatalf("expected typed cache creation map, got %#v", m["cache_creation"])
	}
	if creation["ephemeral_5m_input_tokens"] != 10 || creation["ephemeral_1h_input_tokens"] != 20 {
		t.Fatalf("unexpected ttl breakdown: %#v", creation)
	}
	// credits(#6): omitted entirely when 0 (no upstream metering this turn).
	if _, ok := m["credits"]; ok {
		t.Fatalf("expected credits to be omitted when zero, got %#v", m["credits"])
	}
}

// TestBuildClaudeUsageMapIncludesCredits verifies the #6 contract: the upstream
// meteringEvent credits are surfaced as a top-level float64 "credits" field in
// the Anthropic usage object when > 0.
func TestBuildClaudeUsageMapIncludesCredits(t *testing.T) {
	m := buildClaudeUsageMap(100, 50, promptCacheUsage{}, false, 12.5)

	got, ok := m["credits"]
	if !ok {
		t.Fatalf("expected credits field present when > 0, got %#v", m)
	}
	c, isFloat := got.(float64)
	if !isFloat {
		t.Fatalf("expected credits to be float64, got %T (%#v)", got, got)
	}
	if c != 12.5 {
		t.Fatalf("expected credits 12.5, got %v", c)
	}
	// Token fields still present; cache fields omitted when includeCache=false.
	if m["input_tokens"] != 100 || m["output_tokens"] != 50 {
		t.Fatalf("unexpected token fields: %#v", m)
	}
	if _, ok := m["cache_read_input_tokens"]; ok {
		t.Fatalf("did not expect cache fields when includeCache=false: %#v", m)
	}
}

// TestPromptCacheStableAcrossBillingHeaderDrift verifies that Claude Code's
// per-request "x-anthropic-billing-header: cc_version=...; cch=...;" system
// block (whose content drifts on every request) does not break cache hits.
// The tracker should ignore that metadata when fingerprinting cached prefixes.
func TestPromptCacheStableAcrossBillingHeaderDrift(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	mainSystem := strings.Repeat("You are a helpful coding assistant with deep knowledge of Go, Rust, Python, and TypeScript. ", 80)

	build := func(billingHdr string) *ClaudeRequest {
		return &ClaudeRequest{
			Model: "claude-sonnet-4.5",
			System: []interface{}{
				map[string]interface{}{
					"type": "text",
					"text": billingHdr,
				},
				map[string]interface{}{
					"type": "text",
					"text": mainSystem,
					"cache_control": map[string]interface{}{
						"type": "ephemeral",
					},
				},
			},
			Messages: []ClaudeMessage{{Role: "user", Content: "hello world"}},
		}
	}

	req1 := build("x-anthropic-billing-header: cc_version=2.1.87.1; cch=aaaa;")
	profile1 := tracker.BuildClaudeProfile(req1, 2048)
	if profile1 == nil {
		t.Fatalf("profile1 should be built")
	}
	first := tracker.Compute("acct-1", profile1)
	if first.CacheReadInputTokens != 0 {
		t.Fatalf("expected no cache read on first request, got %+v", first)
	}
	tracker.Update("acct-1", profile1)

	req2 := build("x-anthropic-billing-header: cc_version=2.1.87.42; cch=bbbb; padding=xxyyzz;")
	profile2 := tracker.BuildClaudeProfile(req2, 2048)
	if profile2 == nil {
		t.Fatalf("profile2 should be built")
	}
	second := tracker.Compute("acct-1", profile2)
	if second.CacheReadInputTokens == 0 {
		t.Fatalf("expected cache read after billing header drift, got %+v", second)
	}
}

func TestPromptCacheStableWhenBillingHeaderAppearsOrDisappears(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	mainSystem := strings.Repeat("You are a helpful coding assistant with deep knowledge of Go, Rust, Python, and TypeScript. ", 80)

	build := func(includeBilling bool) *ClaudeRequest {
		system := []interface{}{}
		if includeBilling {
			system = append(system, map[string]interface{}{
				"type": "text",
				"text": "x-anthropic-billing-header: cc_version=2.1.87.1; cch=aaaa;",
			})
		}
		system = append(system, map[string]interface{}{
			"type": "text",
			"text": mainSystem,
			"cache_control": map[string]interface{}{
				"type": "ephemeral",
			},
		})
		return &ClaudeRequest{
			Model:    "claude-sonnet-4.5",
			System:   system,
			Messages: []ClaudeMessage{{Role: "user", Content: "hello world"}},
		}
	}

	withBilling := tracker.BuildClaudeProfile(build(true), 2048)
	if withBilling == nil {
		t.Fatalf("profile with billing header should be built")
	}
	tracker.Update("acct-1", withBilling)

	withoutBilling := tracker.BuildClaudeProfile(build(false), 2048)
	if withoutBilling == nil {
		t.Fatalf("profile without billing header should be built")
	}
	result := tracker.Compute("acct-1", withoutBilling)
	if result.CacheReadInputTokens == 0 {
		t.Fatalf("expected cache read when billing header disappears, got %+v", result)
	}
}

func TestCanonicalCacheValueIgnoresPositionKeys(t *testing.T) {
	first := canonicalizeCacheValue(stripCachePositionKeys(map[string]interface{}{
		"kind":         "system",
		"system_index": 0,
		"block": map[string]interface{}{
			"type": "text",
			"text": "stable",
		},
	}))
	second := canonicalizeCacheValue(stripCachePositionKeys(map[string]interface{}{
		"kind":         "system",
		"system_index": 1,
		"block": map[string]interface{}{
			"type": "text",
			"text": "stable",
		},
	}))
	if first != second {
		t.Fatalf("expected position keys to be ignored, got %q vs %q", first, second)
	}
}

func TestCanonicalCacheValuePreservesSemanticPositionKeys(t *testing.T) {
	first := canonicalizeCacheValue(map[string]interface{}{
		"kind": "system",
		"block": map[string]interface{}{
			"type":        "text",
			"text":        "stable",
			"block_index": 1,
		},
	})
	second := canonicalizeCacheValue(map[string]interface{}{
		"kind": "system",
		"block": map[string]interface{}{
			"type":        "text",
			"text":        "stable",
			"block_index": 2,
		},
	})
	if first == second {
		t.Fatalf("expected semantic block_index fields to remain fingerprinted")
	}
}

// TestPromptCacheWithoutCacheControl verifies the real-semantics simulation for
// plain clients that never send cache_control: the first request creates the
// full cacheable prefix, and a multi-turn continuation reads the stored prefix
// while only the new tail counts as creation.
func TestPromptCacheWithoutCacheControl(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	systemText := strings.Repeat("You are a helpful coding assistant with deep knowledge of Go, Rust, Python, and TypeScript. ", 80)

	req1 := &ClaudeRequest{
		Model:    "claude-sonnet-4.5",
		System:   systemText,
		Messages: []ClaudeMessage{{Role: "user", Content: "question one"}},
	}
	profile1 := tracker.BuildClaudeProfile(req1, 2048)
	if profile1 == nil {
		t.Fatalf("profile should be built without any cache_control")
	}

	first := tracker.Compute("acct-1", profile1)
	if first.CacheCreationInputTokens <= 0 || first.CacheReadInputTokens != 0 {
		t.Fatalf("expected first request to be pure creation, got %+v", first)
	}
	if first.CacheCreation5mInputTokens+first.CacheCreation1hInputTokens != first.CacheCreationInputTokens {
		t.Fatalf("expected 5m+1h == creation, got %+v", first)
	}
	tracker.Update("acct-1", profile1)

	req2 := &ClaudeRequest{
		Model:  "claude-sonnet-4.5",
		System: systemText,
		Messages: []ClaudeMessage{
			{Role: "user", Content: "question one"},
			{Role: "assistant", Content: "answer one"},
			{Role: "user", Content: "follow-up question"},
		},
	}
	profile2 := tracker.BuildClaudeProfile(req2, 4096)
	second := tracker.Compute("acct-1", profile2)
	if second.CacheReadInputTokens == 0 {
		t.Fatalf("expected multi-turn continuation to read the stored prefix, got %+v", second)
	}
	if second.CacheCreationInputTokens <= 0 {
		t.Fatalf("expected the new tail to count as creation, got %+v", second)
	}
	lastTokens := profile2.Breakpoints[len(profile2.Breakpoints)-1].CumulativeTokens
	if lastTokens > profile2.TotalInputTokens {
		lastTokens = profile2.TotalInputTokens
	}
	if got := second.CacheReadInputTokens + second.CacheCreationInputTokens; got != lastTokens {
		t.Fatalf("expected read+creation to cover the cacheable prefix (%d), got %d", lastTokens, got)
	}
	if second.CacheCreation5mInputTokens+second.CacheCreation1hInputTokens != second.CacheCreationInputTokens {
		t.Fatalf("expected 5m+1h == creation, got %+v", second)
	}
}

// TestPromptCacheBelowMinimumIsNotCached: prompts under the model's minimum
// cacheable size (1024, opus 4096) must report all-zero cache usage.
func TestPromptCacheBelowMinimumIsNotCached(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	req := &ClaudeRequest{
		Model:    "claude-sonnet-4.5",
		System:   "tiny system",
		Messages: []ClaudeMessage{{Role: "user", Content: "hi"}},
	}
	profile := tracker.BuildClaudeProfile(req, 64)
	if profile == nil {
		t.Fatalf("profile should still be built")
	}
	if got := tracker.Compute("acct-1", profile); got != (promptCacheUsage{}) {
		t.Fatalf("expected all-zero usage below the cacheable minimum, got %+v", got)
	}
	tracker.Update("acct-1", profile)
	if got := tracker.Compute("acct-1", profile); got != (promptCacheUsage{}) {
		t.Fatalf("expected repeat below minimum to stay zero, got %+v", got)
	}
}

func TestClampPromptCacheUsageInvariants(t *testing.T) {
	// read+creation exceed the (smaller) final input: read wins, creation gets
	// the remainder, tiers re-sum to creation.
	clamped := clampPromptCacheUsage(promptCacheUsage{
		CacheCreationInputTokens:   500,
		CacheReadInputTokens:       800,
		CacheCreation5mInputTokens: 400,
		CacheCreation1hInputTokens: 100,
	}, 1000)
	if clamped.CacheReadInputTokens != 800 {
		t.Fatalf("expected read preserved at 800, got %+v", clamped)
	}
	if clamped.CacheCreationInputTokens != 200 {
		t.Fatalf("expected creation squeezed to 200, got %+v", clamped)
	}
	if clamped.CacheCreation5mInputTokens+clamped.CacheCreation1hInputTokens != clamped.CacheCreationInputTokens {
		t.Fatalf("expected tiers to re-sum to creation, got %+v", clamped)
	}

	// read alone exceeds total: clamped to total, creation zeroed.
	clamped = clampPromptCacheUsage(promptCacheUsage{
		CacheCreationInputTokens: 50,
		CacheReadInputTokens:     2000,
	}, 1000)
	if clamped.CacheReadInputTokens != 1000 || clamped.CacheCreationInputTokens != 0 {
		t.Fatalf("expected read clamped to total and creation zeroed, got %+v", clamped)
	}
	if billed := billedClaudeInputTokens(1000, clamped); billed != 0 {
		t.Fatalf("expected billed input floor at 0, got %d", billed)
	}
}

func TestResolvePromptCacheUsageMeteringWins(t *testing.T) {
	simulated := promptCacheUsage{
		CacheCreationInputTokens:   300,
		CacheReadInputTokens:       200,
		CacheCreation5mInputTokens: 300,
	}
	// Upstream metering present → it replaces the simulation entirely.
	got := resolvePromptCacheUsage(simulated, true, 600, 100, 1000)
	if got.CacheReadInputTokens != 600 || got.CacheCreationInputTokens != 100 {
		t.Fatalf("expected metering values to win, got %+v", got)
	}
	if got.CacheCreation5mInputTokens != 100 || got.CacheCreation1hInputTokens != 0 {
		t.Fatalf("expected metering creation attributed to 5m, got %+v", got)
	}

	// Metering absent → simulation passes through (clamped).
	got = resolvePromptCacheUsage(simulated, false, 0, 0, 1000)
	if got.CacheReadInputTokens != 200 || got.CacheCreationInputTokens != 300 {
		t.Fatalf("expected simulation passthrough, got %+v", got)
	}

	// Metering larger than the final input → clamped with read priority.
	got = resolvePromptCacheUsage(simulated, true, 80, 50, 100)
	if got.CacheReadInputTokens != 80 || got.CacheCreationInputTokens != 20 {
		t.Fatalf("expected clamp with read priority, got %+v", got)
	}
}

// TestPromptCacheOpenAIProfileMultiTurn mirrors the plain-client Claude test
// for the OpenAI request shape used by /v1/chat/completions and /v1/responses.
func TestPromptCacheOpenAIProfileMultiTurn(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	systemText := strings.Repeat("You are a helpful coding assistant with deep knowledge of Go, Rust, Python, and TypeScript. ", 80)

	req1 := &OpenAIRequest{
		Model: "claude-sonnet-4.5",
		Messages: []OpenAIMessage{
			{Role: "system", Content: systemText},
			{Role: "user", Content: "question one"},
		},
	}
	profile1 := tracker.BuildOpenAIProfile(req1, 2048)
	if profile1 == nil {
		t.Fatalf("openai profile should be built")
	}
	first := tracker.Compute("acct-1", profile1)
	if first.CacheCreationInputTokens <= 0 || first.CacheReadInputTokens != 0 {
		t.Fatalf("expected first openai request to be pure creation, got %+v", first)
	}
	tracker.Update("acct-1", profile1)

	req2 := &OpenAIRequest{
		Model: "claude-sonnet-4.5",
		Messages: []OpenAIMessage{
			{Role: "system", Content: systemText},
			{Role: "user", Content: "question one"},
			{Role: "assistant", Content: "answer one"},
			{Role: "user", Content: "follow-up question"},
		},
	}
	profile2 := tracker.BuildOpenAIProfile(req2, 4096)
	second := tracker.Compute("acct-1", profile2)
	if second.CacheReadInputTokens == 0 {
		t.Fatalf("expected openai multi-turn continuation to hit the prefix, got %+v", second)
	}
}

// TestPromptCacheImplicitBreakpointAtMessageEnd verifies that once any
// explicit cache_control breakpoint has been seen, subsequent message-end
// boundaries act as implicit breakpoints. This allows multi-turn conversations
// to hit earlier stored prefix fingerprints even when the newest messages
// lack explicit cache_control.
func TestPromptCacheImplicitBreakpointAtMessageEnd(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	systemText := strings.Repeat("You are a helpful coding assistant with deep knowledge of Go, Rust, Python, and TypeScript. ", 80)

	baseSystem := []interface{}{
		map[string]interface{}{
			"type": "text",
			"text": systemText,
			"cache_control": map[string]interface{}{
				"type": "ephemeral",
			},
		},
	}

	// Round 1: single user message.
	req1 := &ClaudeRequest{
		Model:    "claude-sonnet-4.5",
		System:   baseSystem,
		Messages: []ClaudeMessage{{Role: "user", Content: "question one"}},
	}
	profile1 := tracker.BuildClaudeProfile(req1, 2048)
	if profile1 == nil {
		t.Fatalf("profile1 should be built")
	}
	tracker.Update("acct-1", profile1)

	// Round 2: conversation continues with new messages. The latest user
	// message has no explicit cache_control; it should still hit the stored
	// prefix via the implicit message-end breakpoint.
	req2 := &ClaudeRequest{
		Model:  "claude-sonnet-4.5",
		System: baseSystem,
		Messages: []ClaudeMessage{
			{Role: "user", Content: "question one"},
			{Role: "assistant", Content: "answer one"},
			{Role: "user", Content: "follow-up question"},
		},
	}
	profile2 := tracker.BuildClaudeProfile(req2, 4096)
	if profile2 == nil {
		t.Fatalf("profile2 should be built")
	}
	result := tracker.Compute("acct-1", profile2)
	if result.CacheReadInputTokens == 0 {
		t.Fatalf("expected cache read via implicit message-end breakpoint, got %+v", result)
	}
}
