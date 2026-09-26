package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// sseEvent is one parsed "event: X / data: {...}" pair from a recorded SSE body.
type sseEvent struct {
	name string
	data map[string]interface{}
}

// parseSSEEvents parses a recorded SSE body into ordered events, skipping pings.
func parseSSEEvents(t *testing.T, body string) []sseEvent {
	t.Helper()
	var out []sseEvent
	for _, block := range strings.Split(body, "\n\n") {
		var name, data string
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		if name == "" || name == "ping" || data == "" {
			continue
		}
		parsed := map[string]interface{}{}
		if err := json.Unmarshal([]byte(data), &parsed); err != nil {
			t.Fatalf("event %q has unparseable data %q: %v", name, data, err)
		}
		out = append(out, sseEvent{name: name, data: parsed})
	}
	return out
}

// signatureDeltas returns every signature value carried by signature_delta events.
func signatureDeltas(events []sseEvent) []string {
	var sigs []string
	for _, e := range events {
		if e.name != "content_block_delta" {
			continue
		}
		delta, _ := e.data["delta"].(map[string]interface{})
		if delta == nil || delta["type"] != "signature_delta" {
			continue
		}
		sig, _ := delta["signature"].(string)
		sigs = append(sigs, sig)
	}
	return sigs
}

// runClaudeStreamAgainstFrames drives handleClaudeStream against a fake upstream
// that replays the given AWS event-stream frames, and returns the SSE body.
func runClaudeStreamAgainstFrames(t *testing.T, thinking bool, frames ...[]byte) string {
	t.Helper()

	h, restore := newTestStreamHandler(t, frames...)
	defer restore()

	payload := &KiroPayload{}
	payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		Content: "go", ModelID: "claude-opus-4.8", Origin: "AI_EDITOR",
	}

	rec := httptest.NewRecorder()
	h.handleClaudeStream(context.Background(), rec, payload, "claude-opus-4.8", thinking,
		claudeThinkingResponseOptions{Format: "thinking"}, 1, nil, "")

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// TestThinkingSignatureSentWholeNotChunked is the regression for the 40-byte
// chunking bug. Anthropic's signature_delta is ASSIGNMENT semantics (unlike
// text_delta / thinking_delta / input_json_delta, which append), so emitting the
// signature as N chunks left the client holding only the last <=40 bytes: the
// provenance prefix (which lives at the head) was always lost, and the >=100
// character length guarantee was broken.
func TestThinkingSignatureSentWholeNotChunked(t *testing.T) {
	realSig := strings.Repeat("Zm9vYmFy", 40) // 320 chars, base64-ish
	body := runClaudeStreamAgainstFrames(t, true,
		awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"text": "let me think about it"}),
		awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"signature": realSig}),
		awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"toolUseId": "tooluse_1", "name": "fsWrite",
			"input": `{"path":"/tmp/x"}`, "stop": true,
		}),
	)

	events := parseSSEEvents(t, body)
	sigs := signatureDeltas(events)

	if len(sigs) != 1 {
		t.Fatalf("expected exactly 1 signature_delta (assignment semantics), got %d", len(sigs))
	}
	// Under assignment semantics the client keeps sigs[len-1]; it must be complete.
	got := sigs[len(sigs)-1]
	if !strings.HasPrefix(got, RealSignatureProvenancePrefix) {
		t.Fatalf("signature lost its provenance prefix %q: %q", RealSignatureProvenancePrefix, got)
	}
	if !strings.HasSuffix(got, realSig) {
		t.Fatal("signature was truncated: the upstream signature is not intact")
	}
}

// TestFallbackSignatureSurvivesWhole covers the no-native-signature path: the
// fabricated placeholder is padded past 100 characters on purpose, and chunking
// used to destroy exactly that guarantee.
func TestFallbackSignatureSurvivesWhole(t *testing.T) {
	body := runClaudeStreamAgainstFrames(t, true,
		awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"text": "thinking with no signature"}),
		awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"toolUseId": "tooluse_1", "name": "fsRead", "input": `{}`, "stop": true,
		}),
	)

	sigs := signatureDeltas(parseSSEEvents(t, body))
	if len(sigs) != 1 {
		t.Fatalf("expected exactly 1 signature_delta, got %d", len(sigs))
	}
	got := sigs[0]
	if !isFakeSignature(got) {
		t.Fatalf("expected the fabricated fallback marker, got %q", got)
	}
	if len(got) < 100 {
		t.Fatalf("fallback signature must stay >=100 chars, got %d", len(got))
	}
}

// TestThinkingBlockOrderingAroundToolUse pins the event order at the exact moment
// users reported the stream dying: thinking closes (emitting its signature) and
// the tool_use block opens right after.
func TestThinkingBlockOrderingAroundToolUse(t *testing.T) {
	body := runClaudeStreamAgainstFrames(t, true,
		awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"text": "plan the work"}),
		awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"signature": strings.Repeat("A", 200)}),
		awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"toolUseId": "tooluse_1", "name": "fsWrite",
			"input": `{"path":"/tmp/x"}`, "stop": true,
		}),
	)

	events := parseSSEEvents(t, body)
	var seq []string
	for _, e := range events {
		label := e.name
		if e.name == "content_block_delta" {
			if d, _ := e.data["delta"].(map[string]interface{}); d != nil {
				label += ":" + toStr(d["type"])
			}
		}
		if e.name == "content_block_start" {
			if cb, _ := e.data["content_block"].(map[string]interface{}); cb != nil {
				label += ":" + toStr(cb["type"])
			}
		}
		seq = append(seq, label)
	}
	joined := strings.Join(seq, " -> ")

	// The signature must land inside the thinking block, before it is closed, and
	// the tool_use block must open only afterwards.
	sigAt := strings.Index(joined, "content_block_delta:signature_delta")
	toolAt := strings.Index(joined, "content_block_start:tool_use")
	stopAt := strings.Index(joined, "content_block_stop")
	if sigAt < 0 || toolAt < 0 || stopAt < 0 {
		t.Fatalf("missing expected events in sequence: %s", joined)
	}
	if !(sigAt < stopAt && stopAt < toolAt) {
		t.Fatalf("bad ordering (signature must precede the thinking stop, which precedes tool_use): %s", joined)
	}
	if !strings.HasSuffix(joined, "message_stop") {
		t.Fatalf("stream did not finish with message_stop: %s", joined)
	}
}

func toStr(v interface{}) string {
	s, _ := v.(string)
	return s
}
