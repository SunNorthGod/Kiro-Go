package proxy

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// ---- jsonEscapedLen must agree with the real encoder ----

// TestJSONEscapedLenMatchesMarshal pins the allocation-free escape accounting
// against encoding/json. The whole truncation budget rests on it: if it
// under-counts, a "shrunk to fit" payload still goes upstream oversized.
func TestJSONEscapedLenMatchesMarshal(t *testing.T) {
	cases := []string{
		"",
		"plain ascii text",
		`quotes " and backslash \ mixed`,
		"newlines\nand\ttabs\r\n",
		"html sensitive < > & chars",
		"control \x00\x01\x1f bytes",
		"中文对话记录与工具输出",
		"emoji 🚀 and combining é",
		"line separators \u2028 \u2029 here",
		"invalid utf8: \xff\xfe tail",
		"literal replacement char \ufffd here",
		`transcript: assistant: "I will read the file"` + "\n\ttool_result: {\"path\": \"/a/b.go\"}\n",
	}
	for _, s := range cases {
		raw, err := json.Marshal(s)
		if err != nil {
			t.Fatalf("marshal %q: %v", s, err)
		}
		want := len(raw) - 2 // strip surrounding quotes
		got := jsonEscapedLen(s)
		// Over-estimating is safe (shrinks more than needed); under-estimating is
		// the dangerous direction because it lets an oversized payload through.
		if got < want {
			t.Fatalf("jsonEscapedLen under-counted for %q: got %d, encoder needs %d", s, got, want)
		}
		if got != want && !strings.ContainsRune(s, utf8.RuneError) {
			t.Fatalf("jsonEscapedLen mismatch for %q: got %d, want %d", s, got, want)
		}
	}
}

// ---- the overhead arithmetic that produced the compaction 400 ----

// TestTruncateCurrentMessageOverheadIgnoresEscapeExpansion is the direct
// regression for the root cause: the fixed overhead used to be computed as
// payloadByteSize(payload) - len(cur.Content), mixing an escaped size with a raw
// one, so the body's entire escape expansion was charged as overhead. Measured
// before the fix: 100 KiB of quotes reported ~102,566 bytes of overhead instead
// of ~100, and at 900 KiB the budget went negative and the body was replaced
// with ".".
func TestTruncateCurrentMessageOverheadIgnoresEscapeExpansion(t *testing.T) {
	for _, n := range []int{10 * 1024, 100 * 1024, 900 * 1024} {
		payload := &KiroPayload{}
		payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
			Content: strings.Repeat(`"`, n),
			ModelID: "claude-opus-4.8",
			Origin:  "AI_EDITOR",
		}

		cur := &payload.ConversationState.CurrentMessage.UserInputMessage
		original := cur.Content
		cur.Content = ""
		overhead := payloadByteSize(payload)
		cur.Content = original

		// The real fixed overhead is the JSON scaffolding only — a few hundred
		// bytes — and must not scale with the body.
		if overhead > 1024 {
			t.Fatalf("content=%d: overhead %d scales with the body (escape expansion leaked in)", n, overhead)
		}
	}
}

// TestEscapeDenseBodyIsNotDestroyed covers a transcript-shaped compaction
// request (quote/newline dense, so ~2x escape expansion). The body must be
// trimmed to fit, never swapped for the "." placeholder, and the payload must
// actually use its budget instead of being over-truncated.
func TestEscapeDenseBodyIsNotDestroyed(t *testing.T) {
	unit := "assistant: \"I will read the file\"\n\ttool_result: {\"path\": \"/a/b.go\", \"content\": \"line one\\nline two\"}\n"
	transcript := strings.Repeat(unit, 30000) // ~3 MB raw
	instruction := "Please summarize the conversation above into a compact summary."

	payload := ClaudeToKiro(&ClaudeRequest{
		Model:     "claude-opus-4.8",
		MaxTokens: 512,
		Messages:  []ClaudeMessage{{Role: "user", Content: transcript + "\n\n" + instruction}},
	}, false)

	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	cur := payload.ConversationState.CurrentMessage.UserInputMessage.Content

	if len(raw) > maxPayloadBytes {
		t.Fatalf("payload %d exceeds limit %d", len(raw), maxPayloadBytes)
	}
	if cur == minimalFallbackUserContent {
		t.Fatal("compaction instruction was destroyed and replaced with the \".\" placeholder")
	}
	// The trailing instruction must survive: it is what the request is FOR.
	if !strings.Contains(cur, instruction) {
		t.Fatal("trailing instruction lost; head-only truncation regressed")
	}
	// Over-truncation guard: the budget must be mostly used, not thrown away.
	// Before the fix this payload came out at ~571 KB against a 900 KB limit.
	if len(raw) < maxPayloadBytes*3/4 {
		t.Fatalf("payload over-truncated: %d bytes for a %d limit", len(raw), maxPayloadBytes)
	}
}

// ---- the min-turn floor must never beat the byte limit ----

// TestOversizedRecentTurnsStillFitTheLimit is the regression for the measured
// 2.28x overshoot: without a system prompt, two 1 MiB turns fell inside the
// minRecentHistoryTurns floor, so the floor pinned them in place, the current
// message ("compact") had nothing to give back, and a 2,097,806-byte payload
// went upstream to be rejected with 400 "Input is too long.".
func TestOversizedRecentTurnsStillFitTheLimit(t *testing.T) {
	cases := []struct {
		name   string
		system interface{}
	}{
		{"no priming", nil},
		{"with priming", "You are a helpful assistant."},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := ClaudeToKiro(&ClaudeRequest{
				Model:     "claude-opus-4.8",
				MaxTokens: 512,
				System:    tc.system,
				Messages: []ClaudeMessage{
					{Role: "user", Content: strings.Repeat("p", 1024*1024)},
					{Role: "assistant", Content: "ok"},
					{Role: "user", Content: strings.Repeat("q", 1024*1024)},
					{Role: "assistant", Content: "ok"},
					{Role: "user", Content: "compact the conversation"},
				},
			}, false)

			raw, err := json.Marshal(payload)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if len(raw) > maxPayloadBytes {
				t.Fatalf("payload %d exceeds limit %d (min-turn floor beat the byte limit)", len(raw), maxPayloadBytes)
			}
			cur := payload.ConversationState.CurrentMessage.UserInputMessage.Content
			if !strings.Contains(cur, "compact the conversation") {
				t.Fatalf("instruction lost, current message = %q", cur)
			}
		})
	}
}

// TestOversizedActiveToolTurnFitsTheLimit covers the other compaction shape: the
// most recent turn carries a multi-megabyte tool result (big file read / grep),
// which only the history-shrinking stage can absorb.
func TestOversizedActiveToolTurnFitsTheLimit(t *testing.T) {
	huge := strings.Repeat("y", 1500*1024)
	payload := ClaudeToKiro(&ClaudeRequest{
		Model:     "claude-opus-4.8",
		MaxTokens: 512,
		System:    "You are helpful.",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "read the file"},
			{Role: "assistant", Content: []ClaudeContentBlock{
				{Type: "tool_use", ID: "t1", Name: "readFile", Input: map[string]interface{}{"path": "/big"}},
			}},
			{Role: "user", Content: []ClaudeContentBlock{
				{Type: "tool_result", ToolUseID: "t1", Content: []ClaudeContentBlock{{Type: "text", Text: huge}}},
			}},
			{Role: "assistant", Content: "done reading"},
			{Role: "user", Content: "now summarize it"},
		},
	}, false)

	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(raw) > maxPayloadBytes {
		t.Fatalf("payload %d exceeds limit %d", len(raw), maxPayloadBytes)
	}
	if cur := payload.ConversationState.CurrentMessage.UserInputMessage.Content; !strings.Contains(cur, "now summarize it") {
		t.Fatalf("instruction lost, current message = %q", cur)
	}
}

// ---- UTF-8 safety ----

// TestCJKTruncationStaysValidAndFits is the regression for byte-slice
// truncation: s[:budget] split a multi-byte rune, encoding/json replaced the
// partial rune with the 6-byte \ufffd escape, and the "shrunk" payload came out
// at 921,610 bytes against a 921,600 limit — larger than the budget it was
// clamped to, with U+FFFD corruption in the text the model reads.
func TestCJKTruncationStaysValidAndFits(t *testing.T) {
	for pad := 0; pad < 4; pad++ {
		body := strings.Repeat("x", pad) + strings.Repeat("中文对话记录", 200000)
		payload := ClaudeToKiro(&ClaudeRequest{
			Model:     "claude-opus-4.8",
			MaxTokens: 512,
			Messages:  []ClaudeMessage{{Role: "user", Content: body}},
		}, false)

		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("pad=%d marshal: %v", pad, err)
		}
		cur := payload.ConversationState.CurrentMessage.UserInputMessage.Content

		if len(raw) > maxPayloadBytes {
			t.Fatalf("pad=%d: payload %d exceeds limit %d", pad, len(raw), maxPayloadBytes)
		}
		if !utf8.ValidString(cur) {
			t.Fatalf("pad=%d: truncation produced invalid UTF-8", pad)
		}
		if strings.ContainsRune(cur, utf8.RuneError) {
			t.Fatalf("pad=%d: truncation corrupted a character into U+FFFD", pad)
		}
	}
}

// TestRuneAlignedSliceNeverSplitsRunes exercises both directions across every
// offset around a multi-byte boundary.
func TestRuneAlignedSliceNeverSplitsRunes(t *testing.T) {
	s := "aé中🚀bc"
	for n := 0; n <= len(s)+2; n++ {
		for _, fromHead := range []bool{true, false} {
			got := runeAlignedSlice(s, n, fromHead)
			if !utf8.ValidString(got) {
				t.Fatalf("n=%d fromHead=%v produced invalid UTF-8 %q", n, fromHead, got)
			}
			if len(got) > n && n < len(s) {
				t.Fatalf("n=%d fromHead=%v returned %d bytes, over the cap", n, fromHead, len(got))
			}
		}
	}
}

// TestShrinkToEscapedBudgetRespectsBudget checks the convergence helper directly:
// whatever comes out must serialize within the budget it was given.
func TestShrinkToEscapedBudgetRespectsBudget(t *testing.T) {
	bodies := map[string]string{
		"ascii":      strings.Repeat("a", 200000),
		"quotes":     strings.Repeat(`"`, 200000),
		"cjk":        strings.Repeat("中文", 100000),
		"transcript": strings.Repeat("x: \"y\"\n\t{\"k\": \"v\"}\n", 20000),
		"html":       strings.Repeat("<div>&amp;</div>", 20000),
	}
	for name, body := range bodies {
		for _, budget := range []int{0, 1, 300, 4096, 64 * 1024, 900 * 1024} {
			got := shrinkToEscapedBudget(body, budget)
			if !utf8.ValidString(got) {
				t.Fatalf("%s budget=%d: invalid UTF-8", name, budget)
			}
			if n := jsonEscapedLen(got); n > budget {
				t.Fatalf("%s budget=%d: result serializes to %d bytes, over budget", name, budget, n)
			}
		}
	}
}
