package proxy

import "testing"

func TestThinkingReplayHitAndMiss(t *testing.T) {
	rememberThinkingForReplay("conv-1", "assistant answer one", "my reasoning", "REALSIG", "acct-1")
	reasoning, sig, producer, ok := replayThinkingCandidate("conv-1", "assistant answer one")
	if !ok {
		t.Fatal("expected hit for identical assistant content")
	}
	if reasoning != "my reasoning" {
		t.Fatalf("reasoning = %q", reasoning)
	}
	if sig == "REALSIG" || sig == "" {
		t.Fatalf("signature must be returned provenance-wrapped, got %q", sig)
	}
	if producer != accountSignatureToken("acct-1") {
		t.Fatalf("producer token mismatch: %q", producer)
	}
	// rewritten history (context compaction, truncation) must miss
	if _, _, _, ok := replayThinkingCandidate("conv-1", "assistant answer one (edited)"); ok {
		t.Fatal("edited assistant content must not hit")
	}
	// unknown conversation must miss
	if _, _, _, ok := replayThinkingCandidate("conv-2", "assistant answer one"); ok {
		t.Fatal("unknown conversation must not hit")
	}
}

func TestThinkingReplayRejectsIncompleteEntries(t *testing.T) {
	rememberThinkingForReplay("conv-3", "answer", "", "REALSIG", "acct-1")
	rememberThinkingForReplay("conv-4", "answer", "reasoning", "", "acct-1")
	rememberThinkingForReplay("conv-5", "", "reasoning", "REALSIG", "acct-1")
	if _, _, _, ok := replayThinkingCandidate("conv-3", "answer"); ok {
		t.Fatal("empty reasoning must not be stored")
	}
	if _, _, _, ok := replayThinkingCandidate("conv-4", "answer"); ok {
		t.Fatal("empty signature must not be stored")
	}
	if _, _, _, ok := replayThinkingCandidate("conv-5", ""); ok {
		t.Fatal("empty assistant content must not be stored")
	}
}
