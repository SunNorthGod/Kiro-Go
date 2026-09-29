package proxy

import (
	"strings"
	"testing"
)

// The upstream backend prepends its own host-product system prompt ("You are
// Kiro...") before the model sees this gateway's request; it cannot be
// removed here. The identity-neutrality clause appended to the system priming
// keeps the model from introducing itself as that product to end users of
// third-party clients (leaked verbatim in production 2026-09-30: "I'm Kiro,
// an AI-powered development environment").

func TestIdentityNeutralityClauseAppendedOnce(t *testing.T) {
	for _, thinking := range []bool{true, false} {
		req := &ClaudeRequest{
			Model: "claude-opus-4.8", MaxTokens: 64,
			System:  "You are a coding helper.",
			Messages: []ClaudeMessage{{Role: "user", Content: "hi"}},
		}
		payload := ClaudeToKiro(req, thinking)
		priming := payload.ConversationState.History[0].UserInputMessage.Content
		if !strings.Contains(priming, identityNeutralityClause) {
			t.Fatalf("thinking=%v: clause missing from priming turn", thinking)
		}
		if n := strings.Count(priming, identityNeutralityClause); n != 1 {
			t.Fatalf("thinking=%v: clause appears %d times, want 1", thinking, n)
		}
		// recency: clause sits after the client's own prompt
		if strings.Index(priming, "You are a coding helper.") > strings.Index(priming, identityNeutralityClause) {
			t.Fatalf("thinking=%v: clause must come after the client system prompt", thinking)
		}
	}
}

func TestIdentityNeutralitySystemlessKeepsLegacyShape(t *testing.T) {
	// A request with no client system prompt must keep its exact legacy shape:
	// no priming turn (so history counts and the synthetic-anchor conversation
	// ID derivation stay untouched) and no clause anywhere.
	req := &ClaudeRequest{
		Model: "claude-opus-4.8", MaxTokens: 64,
		Messages: []ClaudeMessage{{Role: "user", Content: "hi"}},
	}
	payload := ClaudeToKiro(req, true)
	if got := payload.ConversationState.History[0].UserInputMessage.Content; strings.Contains(got, identityNeutralityClause) {
		t.Fatalf("systemless request gained the clause: %q", got)
	}
	// thinking-only (label, still no client system): label without clause
	req2 := &ClaudeRequest{
		Model: "claude-opus-4.8", MaxTokens: 64,
		Messages: []ClaudeMessage{{Role: "user", Content: "hi"}},
	}
	p2 := ClaudeToKiro(req2, true)
	got2 := p2.ConversationState.History[0].UserInputMessage.Content
	if strings.Contains(got2, identityNeutralityClause) || !strings.Contains(got2, ThinkingModePrompt) {
		t.Fatalf("thinking-only systemless shape changed: %q", got2)
	}

	// the OpenAI path with a real system prompt still carries the clause
	openai := &OpenAIRequest{
		Model: "claude-opus-4.8", MaxTokens: 64,
		Messages: []OpenAIMessage{
			{Role: "system", Content: "be terse"},
			{Role: "user", Content: "hi"},
		},
	}
	op := OpenAIToKiro(openai, true)
	if got := op.ConversationState.History[0].UserInputMessage.Content; !strings.Contains(got, identityNeutralityClause) {
		t.Fatalf("openai path lost the clause: %q", got)
	}
}
