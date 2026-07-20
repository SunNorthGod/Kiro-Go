package proxy

import (
	"strings"
	"testing"
)

// helper: build a ClaudeRequest whose only history assistant turn carries a
// thinking block (text+signature) followed by a text block, plus a trailing user
// turn so the assistant turn is real history (not the current message).
func reqWithHistoryThinking(sig string) *ClaudeRequest {
	return &ClaudeRequest{
		Model:  "claude-opus-4.8",
		System: "sys",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "q1"},
			{Role: "assistant", Content: []interface{}{
				map[string]interface{}{"type": "thinking", "thinking": "prior reasoning", "signature": sig},
				map[string]interface{}{"type": "text", "text": "prior answer"},
			}},
			{Role: "user", Content: "q2"},
		},
	}
}

func historyReasoning(payload *KiroPayload) *KiroReasoningContent {
	for i := range payload.ConversationState.History {
		if am := payload.ConversationState.History[i].AssistantResponseMessage; am != nil {
			if am.ReasoningContent != nil {
				return am.ReasoningContent
			}
		}
	}
	return nil
}

func TestProvenanceSignatureRoundTrip(t *testing.T) {
	real := "EuABCnEIDxABGAIqQG6flrDimnw7realbase64sig=="
	wrapped := wrapProvenanceSignature(real, "acct-A")
	if wrapped == real {
		t.Fatalf("expected wrapped signature to differ from raw")
	}
	if !strings.HasPrefix(wrapped, RealSignatureProvenancePrefix) {
		t.Fatalf("wrapped signature missing provenance prefix: %q", wrapped)
	}
	gotSig, gotTok, ok := parseProvenanceSignature(wrapped)
	if !ok {
		t.Fatalf("expected parse ok for our own wrapped signature")
	}
	if gotSig != real {
		t.Fatalf("round-trip signature mismatch: got %q want %q", gotSig, real)
	}
	if gotTok != accountSignatureToken("acct-A") {
		t.Fatalf("round-trip token mismatch: got %q want %q", gotTok, accountSignatureToken("acct-A"))
	}
}

func TestAccountSignatureTokenStableAndDistinct(t *testing.T) {
	if accountSignatureToken("acct-A") != accountSignatureToken("acct-A") {
		t.Fatalf("token must be stable for the same account id")
	}
	if accountSignatureToken("acct-A") == accountSignatureToken("acct-B") {
		t.Fatalf("token must differ across account ids")
	}
	if accountSignatureToken("") != "" {
		t.Fatalf("empty account id must map to empty token")
	}
}

func TestParseProvenanceRejectsForeignAndFake(t *testing.T) {
	// A real Anthropic-style base64 signature is NOT ours.
	if _, _, ok := parseProvenanceSignature("EuABCnEIDxABGAIqQG6flrDimnw7=="); ok {
		t.Fatalf("foreign base64 signature must not parse as provenance-tagged")
	}
	// Fake placeholder signatures are not provenance-tagged either.
	if _, _, ok := parseProvenanceSignature(FakeSignatureMarker + "deadbeef"); ok {
		t.Fatalf("fake signature must not parse as provenance-tagged")
	}
	if _, _, ok := parseProvenanceSignature(""); ok {
		t.Fatalf("empty signature must not parse")
	}
	// Malformed (prefix but no token/sig separator).
	if _, _, ok := parseProvenanceSignature(RealSignatureProvenancePrefix + "abcd"); ok {
		t.Fatalf("prefix without ':' must not parse")
	}
}

func TestApplyThinkingProvenanceSameAccountKeeps(t *testing.T) {
	real := "realSIGNATUREvalue1234567890"
	req := reqWithHistoryThinking(wrapProvenanceSignature(real, "acct-A"))
	payload := ClaudeToKiro(req, true)

	// Before the gate runs, nothing is emitted upstream yet.
	if historyReasoning(payload) != nil {
		t.Fatalf("ReasoningContent must stay nil until applyThinkingProvenance runs")
	}

	applyThinkingProvenance(payload, "acct-A") // same account that produced it
	rc := historyReasoning(payload)
	if rc == nil {
		t.Fatalf("same-account replay must keep the reasoning block")
	}
	if rc.ReasoningText.Signature != real {
		t.Fatalf("same-account replay must forward the UNWRAPPED real signature, got %q", rc.ReasoningText.Signature)
	}
}

func TestApplyThinkingProvenanceCrossAccountStrips(t *testing.T) {
	real := "realSIGNATUREvalue1234567890"
	req := reqWithHistoryThinking(wrapProvenanceSignature(real, "acct-A"))
	payload := ClaudeToKiro(req, true)

	applyThinkingProvenance(payload, "acct-B") // different account → would 400
	if historyReasoning(payload) != nil {
		t.Fatalf("cross-account replay must strip the reasoning block")
	}
}

func TestApplyThinkingProvenanceUntaggedForeignStrips(t *testing.T) {
	// Foreign (Kiro IDE native / real Anthropic / pre-v1.1.9) real signature: no
	// provenance marker → unknown producer → conservatively stripped.
	req := reqWithHistoryThinking("EuABCnEIDxABforeignRealSig==")
	payload := ClaudeToKiro(req, true)

	applyThinkingProvenance(payload, "acct-A")
	if historyReasoning(payload) != nil {
		t.Fatalf("untagged foreign signature must be stripped (unknown provenance)")
	}
}

func TestApplyThinkingProvenanceFakeNeverCandidate(t *testing.T) {
	req := reqWithHistoryThinking(FakeSignatureMarker + "0123456789abcdef")
	payload := ClaudeToKiro(req, true)

	// Fake signatures are dropped at translate time (never become a candidate).
	for i := range payload.ConversationState.History {
		if am := payload.ConversationState.History[i].AssistantResponseMessage; am != nil {
			if am.ReasoningCandidate != nil {
				t.Fatalf("fake signature must not produce a reasoning candidate")
			}
		}
	}
	applyThinkingProvenance(payload, "acct-A")
	if historyReasoning(payload) != nil {
		t.Fatalf("fake signature must never be forwarded upstream")
	}
}

// The tool_use loop shape: an assistant turn with thinking + tool_use, whose
// tool_use is answered by the current message's tool_result. Provenance must
// leave the active structured tool turn intact while independently deciding the
// thinking signature (keep on same account, strip on cross account) — matching
// SelfHeal's proven behaviour.
func toolLoopReq(sig string) *ClaudeRequest {
	return &ClaudeRequest{
		Model:  "claude-opus-4.8",
		System: "sys",
		Tools:  []ClaudeTool{{Name: "get_weather", Description: "d"}},
		Messages: []ClaudeMessage{
			{Role: "user", Content: "weather?"},
			{Role: "assistant", Content: []interface{}{
				map[string]interface{}{"type": "thinking", "thinking": "let me check", "signature": sig},
				map[string]interface{}{"type": "text", "text": "checking"},
				map[string]interface{}{"type": "tool_use", "id": "tu_1", "name": "get_weather", "input": map[string]interface{}{"city": "SF"}},
			}},
			{Role: "user", Content: []interface{}{
				map[string]interface{}{"type": "tool_result", "tool_use_id": "tu_1", "content": "sunny"},
			}},
		},
	}
}

func lastAssistantToolUses(payload *KiroPayload) []KiroToolUse {
	var out []KiroToolUse
	for i := range payload.ConversationState.History {
		if am := payload.ConversationState.History[i].AssistantResponseMessage; am != nil && len(am.ToolUses) > 0 {
			out = am.ToolUses
		}
	}
	return out
}

func TestApplyThinkingProvenanceToolLoopSameAccountKeepsBoth(t *testing.T) {
	real := "toolLoopREALsig9876543210"
	payload := ClaudeToKiro(toolLoopReq(wrapProvenanceSignature(real, "acct-A")), true)

	applyThinkingProvenance(payload, "acct-A")

	if tus := lastAssistantToolUses(payload); len(tus) == 0 {
		t.Fatalf("active structured tool turn must be preserved regardless of provenance")
	}
	rc := historyReasoning(payload)
	if rc == nil || rc.ReasoningText.Signature != real {
		t.Fatalf("same-account tool-loop thinking must be kept with its real signature")
	}
}

func TestApplyThinkingProvenanceToolLoopCrossAccountStripsThinkingOnly(t *testing.T) {
	real := "toolLoopREALsig9876543210"
	payload := ClaudeToKiro(toolLoopReq(wrapProvenanceSignature(real, "acct-A")), true)

	applyThinkingProvenance(payload, "acct-B")

	// tool_use turn stays structured (tool continuity), only the foreign-account
	// thinking signature is stripped — exactly what SelfHeal does, done proactively.
	if tus := lastAssistantToolUses(payload); len(tus) == 0 {
		t.Fatalf("cross-account strip must NOT remove the active structured tool turn")
	}
	if historyReasoning(payload) != nil {
		t.Fatalf("cross-account tool-loop thinking signature must be stripped")
	}
}

// Re-evaluation across account retries: the candidate is preserved so a later
// attempt that lands back on the producing account can still forward the sig.
func TestApplyThinkingProvenanceReevaluatesAcrossAttempts(t *testing.T) {
	real := "reevalREALsig000111222"
	payload := ClaudeToKiro(reqWithHistoryThinking(wrapProvenanceSignature(real, "acct-A")), true)

	applyThinkingProvenance(payload, "acct-B") // attempt 1: cross → strip
	if historyReasoning(payload) != nil {
		t.Fatalf("attempt 1 (cross-account) should strip")
	}
	applyThinkingProvenance(payload, "acct-A") // attempt 2: back on producer → restore
	rc := historyReasoning(payload)
	if rc == nil || rc.ReasoningText.Signature != real {
		t.Fatalf("attempt landing back on producer must restore the reasoning block")
	}
}

// SelfHeal remains a backstop: even a kept (same-account) block is stripped by
// stripSelfHealFields when a 400 still occurs for some other reason.
func TestSelfHealStillStripsKeptReasoning(t *testing.T) {
	real := "backstopREALsig33344455"
	payload := ClaudeToKiro(reqWithHistoryThinking(wrapProvenanceSignature(real, "acct-A")), true)
	applyThinkingProvenance(payload, "acct-A")
	if historyReasoning(payload) == nil {
		t.Fatalf("precondition: same-account block should be kept")
	}
	if !stripSelfHealFields(payload) {
		t.Fatalf("stripSelfHealFields should report it removed reasoning content")
	}
	if historyReasoning(payload) != nil {
		t.Fatalf("SelfHeal backstop must still be able to strip a kept block")
	}
}
