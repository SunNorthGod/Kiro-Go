package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

// The current message of a structured tool-result round must carry an EMPTY
// content, matching native Kiro IDE (kiro.kiro-agent bundle:
// userInputMessage:{content:"",origin:"AI_EDITOR",userInputMessageContext:{toolResults:...}}).
// The gateway used to place a bare "." there; the backend rendered it as a user
// turn containing a single period, which buyer models read (and reported) as a
// nudge message — ticket 2026-09-30. The upstream accepts the empty shape and
// answers normally (live-verified 2026-09-30 on generateAssistantResponse).
func TestToolRoundCurrentMessageIsEmptyLikeNativeIDE(t *testing.T) {
	req := &ClaudeRequest{
		Model: "claude-opus-4.8", MaxTokens: 1024,
		System: "You are a helpful assistant.",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "Read the file and tell me the first line."},
			{Role: "assistant", Content: []interface{}{map[string]interface{}{
				"type": "tool_use", "id": "toolu_01", "name": "read_file",
				"input": map[string]interface{}{"path": "/tmp/x"},
			}}},
			{Role: "user", Content: []interface{}{map[string]interface{}{
				"type": "tool_result", "tool_use_id": "toolu_01",
				"content": []interface{}{map[string]interface{}{"type": "text", "text": "line one"}},
			}}},
		},
		Tools: []ClaudeTool{{
			Name:        "read_file",
			Description: "Read a text file.",
			InputSchema: map[string]interface{}{"type": "object"},
		}},
	}

	payload := ClaudeToKiro(req, true)
	cur := payload.ConversationState.CurrentMessage.UserInputMessage
	if cur.Content != nativeToolRoundContent {
		t.Fatalf("tool-round current content = %q, want %q (native IDE shape)", cur.Content, nativeToolRoundContent)
	}
	ctx := cur.UserInputMessageContext
	if ctx == nil || len(ctx.ToolResults) != 1 {
		t.Fatalf("tool results not attached structurally: %+v", ctx)
	}
	if ctx.ToolResults[0].ToolUseID != "toolu_01" {
		t.Fatalf("toolUseId mismatch: %q", ctx.ToolResults[0].ToolUseID)
	}

	// Same contract on the OpenAI path.
	var tc ToolCall
	tc.ID = "call_01"
	tc.Type = "function"
	tc.Function.Name = "read_file"
	tc.Function.Arguments = `{"path":"/tmp/x"}`
	var ot OpenAITool
	ot.Type = "function"
	ot.Function.Name = "read_file"
	ot.Function.Description = "Read a text file."
	ot.Function.Parameters = map[string]interface{}{"type": "object"}
	openaiReq := &OpenAIRequest{
		Model: "claude-opus-4.8", MaxTokens: 1024,
		Messages: []OpenAIMessage{
			{Role: "user", Content: "Read the file and tell me the first line."},
			{Role: "assistant", Content: nil, ToolCalls: []ToolCall{tc}},
			{Role: "tool", ToolCallID: "call_01", Content: "line one"},
		},
		Tools: []OpenAITool{ot},
	}
	op := OpenAIToKiro(openaiReq, true)
	ocur := op.ConversationState.CurrentMessage.UserInputMessage
	if ocur.Content != nativeToolRoundContent {
		t.Fatalf("openai tool-round current content = %q, want %q", ocur.Content, nativeToolRoundContent)
	}
}

// The fold path (tool results flattened into text) must never fall back to the
// bare "." either: a textless round carries an honest notice instead.
func TestFoldPathTextlessResultsUseHonestNotice(t *testing.T) {
	if got := buildToolResultsContinuation(nil); got != emptyToolResultNotice {
		t.Fatalf("empty continuation = %q, want %q", got, emptyToolResultNotice)
	}
	textless := []KiroToolResult{{ToolUseID: "t1", Content: []KiroResultContent{{Text: "  "}}, Status: "success"}}
	if got := buildToolResultsContinuation(textless); got != emptyToolResultNotice {
		t.Fatalf("textless continuation = %q, want %q", got, emptyToolResultNotice)
	}
	withText := []KiroToolResult{{ToolUseID: "t1", Content: []KiroResultContent{{Text: "output body"}}, Status: "success"}}
	got := buildToolResultsContinuation(withText)
	if !strings.HasPrefix(got, toolResultsContinuationPrefix) || !strings.Contains(got, "output body") {
		t.Fatalf("continuation with text = %q, want prefix %q plus the body", got, toolResultsContinuationPrefix)
	}
	_ = json.Marshal // keep encoding/json import stable if assertions above change
}
