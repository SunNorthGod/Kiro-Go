package proxy

import (
	"strings"
	"testing"
)

func TestExtractOpenAIMessageTextStructured(t *testing.T) {
	content := []interface{}{
		map[string]interface{}{"type": "text", "text": "alpha"},
		map[string]interface{}{"type": "input_text", "text": "beta"},
	}

	if got := extractOpenAIMessageText(content); got != "alphabeta" {
		t.Fatalf("expected concatenated structured text, got %q", got)
	}

	nested := map[string]interface{}{
		"content": []interface{}{map[string]interface{}{"type": "text", "text": "nested"}},
	}
	if got := extractOpenAIMessageText(nested); got != "nested" {
		t.Fatalf("expected nested content extraction, got %q", got)
	}
}

func TestOpenAIToKiroPreservesStructuredAssistantAndToolContent(t *testing.T) {
	req := &OpenAIRequest{
		Model: "claude-sonnet-4.5",
		Messages: []OpenAIMessage{
			{
				Role: "system",
				Content: []interface{}{
					map[string]interface{}{"type": "text", "text": "system-a"},
					map[string]interface{}{"type": "text", "text": "system-b"},
				},
			},
			{Role: "user", Content: "first-question"},
			{
				Role: "assistant",
				Content: []interface{}{
					map[string]interface{}{"type": "text", "text": "assistant-structured"},
				},
			},
			{
				Role:       "tool",
				ToolCallID: "call_1",
				Content: []interface{}{
					map[string]interface{}{"type": "text", "text": "tool-result-structured"},
				},
			},
		},
	}

	payload := OpenAIToKiro(req, false)

	// History starts with a priming pair.
	if len(payload.ConversationState.History) != 4 {
		t.Fatalf("expected 4 history items (2 priming + 2 conversation), got %d", len(payload.ConversationState.History))
	}

	// history[0]: priming user
	primingUser := payload.ConversationState.History[0].UserInputMessage
	if primingUser == nil {
		t.Fatalf("expected history[0] to be priming user message")
	}
	if !strings.Contains(primingUser.Content, "system-a") || !strings.Contains(primingUser.Content, "system-b") {
		t.Fatalf("expected priming user message to contain system prompt, got %q", primingUser.Content)
	}
	if strings.Contains(primingUser.Content, "first-question") {
		t.Fatalf("expected system prompt priming not to contain user question, got %q", primingUser.Content)
	}

	// history[1]: priming assistant
	primingAssistant := payload.ConversationState.History[1].AssistantResponseMessage
	if primingAssistant == nil {
		t.Fatalf("expected history[1] to be priming assistant message")
	}
	if primingAssistant.Content != "I will follow these instructions." {
		t.Fatalf("expected priming assistant ack, got %q", primingAssistant.Content)
	}

	// history[2]: first user turn
	firstConvUser := payload.ConversationState.History[2].UserInputMessage
	if firstConvUser == nil {
		t.Fatalf("expected history[2] to be first conversation user message")
	}
	if !strings.Contains(firstConvUser.Content, "first-question") {
		t.Fatalf("expected history[2] to contain first-question, got %q", firstConvUser.Content)
	}

	// history[3]: assistant reply
	historyAssistant := payload.ConversationState.History[3].AssistantResponseMessage
	if historyAssistant == nil {
		t.Fatalf("expected history[3] to be assistant message")
	}
	if historyAssistant.Content != "assistant-structured" {
		t.Fatalf("expected assistant structured content to be preserved, got %q", historyAssistant.Content)
	}

	// The tool result answers call_1, but the last history assistant has no
	// matching structured tool call (it is text-only), so the tool result is an
	// orphan. Kiro's upstream rejects structured tool results that do not answer
	// the immediately preceding assistant tool call, so it must be narrated into
	// the current message text rather than kept structured.
	cur := payload.ConversationState.CurrentMessage.UserInputMessage
	if !strings.Contains(cur.Content, "tool-result-structured") {
		t.Fatalf("expected tool-result continuation content, got %q", cur.Content)
	}
	if cur.UserInputMessageContext != nil && len(cur.UserInputMessageContext.ToolResults) != 0 {
		t.Fatalf("expected orphan tool result to be flattened into text, not kept structured")
	}
}

func TestOpenAIToKiroAssistantMapContentInHistory(t *testing.T) {
	req := &OpenAIRequest{
		Model: "claude-sonnet-4.5",
		Messages: []OpenAIMessage{
			{Role: "user", Content: "u1"},
			{Role: "assistant", Content: map[string]interface{}{"type": "text", "text": "assistant-map"}},
			{Role: "user", Content: "u2"},
		},
	}

	payload := OpenAIToKiro(req, false)

	if len(payload.ConversationState.History) != 2 {
		t.Fatalf("expected 2 history entries, got %d", len(payload.ConversationState.History))
	}
	assistant := payload.ConversationState.History[1].AssistantResponseMessage
	if assistant == nil {
		t.Fatalf("expected second history entry to be assistant")
	}
	if assistant.Content != "assistant-map" {
		t.Fatalf("expected assistant map content preserved, got %q", assistant.Content)
	}
}

func TestOpenAIToKiroAssistantToolCallsDoNotInjectPlaceholder(t *testing.T) {
	req := &OpenAIRequest{
		Model: "claude-sonnet-4.5",
		Messages: []OpenAIMessage{
			{Role: "user", Content: "find weather"},
			{
				Role:    "assistant",
				Content: nil,
				ToolCalls: []ToolCall{{
					ID:   "call_1",
					Type: "function",
					Function: struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					}{Name: "get_weather", Arguments: "{}"},
				}},
			},
			{Role: "user", Content: "continue"},
		},
	}

	payload := OpenAIToKiro(req, false)

	// The mid-history assistant turn carried ONLY a tool call (no text) and is
	// not the active tool turn, so its structured toolUses are cleared. That
	// leaves it hollow, and a hollow assistant turn is dropped entirely rather
	// than backfilled with a "." placeholder (which the model would imitate).
	// No surviving turn may contain tool-invocation text or structured toolUses.
	for i, h := range payload.ConversationState.History {
		a := h.AssistantResponseMessage
		if a == nil {
			continue
		}
		if len(a.ToolUses) != 0 {
			t.Fatalf("history[%d] retains structured toolUses", i)
		}
		if strings.Contains(a.Content, "get_weather") || strings.Contains(a.Content, "[Called tool") {
			t.Fatalf("history[%d] assistant contains tool-invocation text: %q", i, a.Content)
		}
		if strings.TrimSpace(a.Content) == "." || strings.TrimSpace(a.Content) == "" {
			t.Fatalf("history[%d] is a hollow assistant turn that should have been dropped", i)
		}
	}
}

func TestOpenAIConversationIDStableFromAnchor(t *testing.T) {
	baseMessages := []OpenAIMessage{
		{Role: "system", Content: "You are helpful"},
		{Role: "user", Content: "Build calculator"},
		{Role: "assistant", Content: "Sure"},
		{Role: "user", Content: "Continue"},
	}

	reqA := &OpenAIRequest{Model: "claude-sonnet-4.5", Messages: baseMessages}
	reqB := &OpenAIRequest{Model: "claude-sonnet-4.5", Messages: append(baseMessages, OpenAIMessage{Role: "assistant", Content: "Next step"})}

	payloadA := OpenAIToKiro(reqA, false)
	payloadB := OpenAIToKiro(reqB, false)

	if payloadA.ConversationState.ConversationID == "" || payloadB.ConversationState.ConversationID == "" {
		t.Fatalf("expected non-empty conversation IDs")
	}
	if payloadA.ConversationState.ConversationID != payloadB.ConversationState.ConversationID {
		t.Fatalf("expected stable conversation ID across turns, got %q vs %q", payloadA.ConversationState.ConversationID, payloadB.ConversationState.ConversationID)
	}
}

func TestClaudeConversationIDStableFromAnchor(t *testing.T) {
	reqA := &ClaudeRequest{
		Model:  "claude-sonnet-4.5",
		System: "sys",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "hello"},
		},
	}
	reqB := &ClaudeRequest{
		Model:  "claude-sonnet-4.5",
		System: "sys",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: "ok"},
			{Role: "user", Content: "next"},
		},
	}

	payloadA := ClaudeToKiro(reqA, false)
	payloadB := ClaudeToKiro(reqB, false)

	if payloadA.ConversationState.ConversationID == "" || payloadB.ConversationState.ConversationID == "" {
		t.Fatalf("expected non-empty conversation IDs")
	}
	if payloadA.ConversationState.ConversationID != payloadB.ConversationState.ConversationID {
		t.Fatalf("expected stable conversation ID across turns, got %q vs %q", payloadA.ConversationState.ConversationID, payloadB.ConversationState.ConversationID)
	}
}

func TestOpenAIConversationIDRandomForSyntheticAnchor(t *testing.T) {
	req := &OpenAIRequest{
		Model: "claude-sonnet-4.5",
		Messages: []OpenAIMessage{
			{Role: "assistant", Content: "prefill"},
		},
	}

	payloadA := OpenAIToKiro(req, false)
	payloadB := OpenAIToKiro(req, false)

	if payloadA.ConversationState.ConversationID == payloadB.ConversationState.ConversationID {
		t.Fatalf("expected synthetic anchor to generate non-deterministic conversation IDs")
	}
}

func TestClaudeToKiroDropsLeadingAssistantHistory(t *testing.T) {
	req := &ClaudeRequest{
		Model: "claude-sonnet-4.5",
		Messages: []ClaudeMessage{
			{Role: "assistant", Content: "prefill"},
			{Role: "user", Content: "real user message"},
		},
	}

	payload := ClaudeToKiro(req, false)

	if len(payload.ConversationState.History) != 0 {
		t.Fatalf("expected leading assistant-only history to be dropped, got %d entries", len(payload.ConversationState.History))
	}

	if strings.Contains(payload.ConversationState.CurrentMessage.UserInputMessage.Content, "Begin conversation") {
		t.Fatalf("unexpected synthetic Begin conversation injection in current content: %q", payload.ConversationState.CurrentMessage.UserInputMessage.Content)
	}
}

func TestKiroToClaudeResponseCanEmitEmptyThinkingBlock(t *testing.T) {
	resp := KiroToClaudeResponse("final answer", "", true, nil, 10, 20, "claude-sonnet-4.6", "")

	if len(resp.Content) != 2 {
		t.Fatalf("expected empty thinking block plus text block, got %d blocks", len(resp.Content))
	}
	if resp.Content[0].Type != "thinking" {
		t.Fatalf("expected first block to be thinking, got %#v", resp.Content[0])
	}
	if resp.Content[0].Thinking != "" {
		t.Fatalf("expected omitted thinking block to have empty content, got %#v", resp.Content[0].Thinking)
	}
	if resp.Content[1].Type != "text" || resp.Content[1].Text != "final answer" {
		t.Fatalf("expected text block to be preserved, got %#v", resp.Content[1])
	}
}

func TestMapClaudeStopReason(t *testing.T) {
	for _, tc := range []struct {
		name      string
		reason    string
		toolCount int
		want      string
	}{
		{name: "max tokens", reason: "MAX_TOKENS", want: "max_tokens"},
		{name: "max output tokens alias", reason: "max_output_tokens", want: "max_tokens"},
		{name: "length alias", reason: "length", want: "max_tokens"},
		{name: "context limit", reason: "CONTEXT_WINDOW_EXCEEDED", want: "model_context_window_exceeded"},
		{name: "model context limit alias", reason: "model_context_window_exceeded", want: "model_context_window_exceeded"},
		{name: "content filter", reason: "CONTENT_FILTERED", want: "refusal"},
		{name: "refusal alias", reason: "refusal", want: "refusal"},
		{name: "guardrail alias", reason: "guardrail_intervened", want: "refusal"},
		{name: "stop sequence", reason: "STOP_SEQUENCE", want: "stop_sequence"},
		{name: "pause", reason: "PAUSE_TURN", want: "pause_turn"},
		// END_TURN is the only stopReason literal the Kiro IDE bundle compares
		// against, so it is the realistic "finished normally" input.
		{name: "end turn", reason: "END_TURN", want: "end_turn"},
		// Absent metadataEvent leaves the reason empty. Mapping it to end_turn is
		// the deliberate fallback for clients that require a terminal reason; the
		// truncation itself is caught by the stream integrity layer, not here.
		{name: "empty falls back to end turn", reason: "", want: "end_turn"},
		{name: "unknown defaults to end turn", reason: "COMPLETE", want: "end_turn"},
		{name: "tool use takes priority", reason: "MAX_TOKENS", toolCount: 1, want: "tool_use"},
		{name: "tool use wins over empty reason", reason: "", toolCount: 2, want: "tool_use"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := mapClaudeStopReason(tc.reason, tc.toolCount); got != tc.want {
				t.Fatalf("mapClaudeStopReason(%q, %d) = %q, want %q", tc.reason, tc.toolCount, got, tc.want)
			}
		})
	}
}

func TestMapOpenAIFinishReason(t *testing.T) {
	for _, tc := range []struct {
		name      string
		reason    string
		toolCount int
		want      string
	}{
		{name: "max tokens", reason: "MAX_TOKENS", want: "length"},
		{name: "max output tokens alias", reason: "max_output_tokens", want: "length"},
		{name: "length alias", reason: "length", want: "length"},
		{name: "context limit", reason: "CONTEXT_WINDOW_EXCEEDED", want: "length"},
		{name: "model context limit alias", reason: "model_context_window_exceeded", want: "length"},
		{name: "content filter", reason: "CONTENT_FILTERED", want: "content_filter"},
		{name: "refusal alias", reason: "refusal", want: "content_filter"},
		{name: "guardrail alias", reason: "guardrail_intervened", want: "content_filter"},
		{name: "end turn", reason: "END_TURN", want: "stop"},
		{name: "empty falls back to stop", reason: "", want: "stop"},
		{name: "unknown defaults to stop", reason: "COMPLETE", want: "stop"},
		{name: "tool calls take priority", reason: "MAX_TOKENS", toolCount: 1, want: "tool_calls"},
		{name: "tool calls win over empty reason", reason: "", toolCount: 2, want: "tool_calls"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := mapOpenAIFinishReason(tc.reason, tc.toolCount); got != tc.want {
				t.Fatalf("mapOpenAIFinishReason(%q, %d) = %q, want %q", tc.reason, tc.toolCount, got, tc.want)
			}
		})
	}
}

func TestToolResultsContinuationIncludesInstructionPrefix(t *testing.T) {
	req := &OpenAIRequest{
		Model: "claude-sonnet-4.5",
		Messages: []OpenAIMessage{
			{Role: "user", Content: "find data"},
			{Role: "assistant", ToolCalls: []ToolCall{{
				ID:   "call_1",
				Type: "function",
				Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{Name: "fetch", Arguments: "{}"},
			}}},
			{Role: "tool", ToolCallID: "call_1", Content: "result-1"},
		},
	}

	payload := OpenAIToKiro(req, false)
	cur := payload.ConversationState.CurrentMessage.UserInputMessage

	// Active tool turn (last assistant issued call_1, the current message answers
	// it): the tool output rides the STRUCTURED ToolResults channel and Content is
	// the minimal placeholder. It must NOT also be narrated into Content, which
	// would duplicate the output in the context window (dedup contract).
	if cur.UserInputMessageContext == nil || len(cur.UserInputMessageContext.ToolResults) != 1 {
		t.Fatalf("expected one structured tool result, got %+v", cur.UserInputMessageContext)
	}
	if got := cur.UserInputMessageContext.ToolResults[0].Content[0].Text; got != "result-1" {
		t.Fatalf("expected tool result text in structured channel, got %q", got)
	}
	if strings.Contains(cur.Content, "result-1") {
		t.Fatalf("tool result text must not be duplicated into Content, got %q", cur.Content)
	}
}

func TestKiroToOpenAIResponseWithReasoningPreservesFinishReason(t *testing.T) {
	response := KiroToOpenAIResponseWithReasoning("partial answer", "", nil, 10, 20, "claude-sonnet-4.5", "reasoning_content", "MAX_TOKENS", promptCacheUsage{})
	choices, ok := response["choices"].([]map[string]interface{})
	if !ok || len(choices) != 1 {
		t.Fatalf("unexpected choices: %#v", response["choices"])
	}
	if got := choices[0]["finish_reason"]; got != "length" {
		t.Fatalf("finish_reason=%#v, want length", got)
	}
}

func TestEnsureObjectSchemaRemovesKiroRejectedFieldsRecursively(t *testing.T) {
	input := map[string]interface{}{
		"type":                 "object",
		"required":             []interface{}{},
		"additionalProperties": false,
		"properties": map[string]interface{}{
			"path": map[string]interface{}{
				"type":                 "string",
				"required":             nil,
				"additionalProperties": map[string]interface{}{"type": "string"},
			},
			"options": map[string]interface{}{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]interface{}{
					"force": map[string]interface{}{"type": "boolean"},
				},
			},
		},
		"anyOf": []interface{}{
			map[string]interface{}{
				"type":                 "object",
				"required":             []interface{}{},
				"additionalProperties": false,
			},
		},
	}

	got := ensureObjectSchema(input).(map[string]interface{})
	if schemaContainsKey(got, "additionalProperties") {
		t.Fatalf("expected additionalProperties to be removed recursively, got %#v", got)
	}
	if schemaContainsKey(got, "required") {
		t.Fatalf("expected empty/nil required fields to be removed recursively, got %#v", got)
	}
	if _, stillPresent := input["additionalProperties"]; !stillPresent {
		t.Fatalf("expected sanitizer not to mutate caller schema")
	}
}

func TestConvertOpenAIToolsSanitizesSchemaAndDescription(t *testing.T) {
	var tool OpenAITool
	tool.Type = "function"
	tool.Function.Name = "read_file"
	tool.Function.Parameters = map[string]interface{}{
		"type":                 "object",
		"required":             []string{},
		"additionalProperties": false,
	}

	tools := convertOpenAITools([]OpenAITool{tool})
	if len(tools) != 1 {
		t.Fatalf("expected one converted tool, got %d", len(tools))
	}
	if strings.TrimSpace(tools[0].ToolSpecification.Description) == "" {
		t.Fatalf("expected fallback tool description")
	}
	schema := tools[0].ToolSpecification.InputSchema.JSON.(map[string]interface{})
	if schemaContainsKey(schema, "additionalProperties") {
		t.Fatalf("expected OpenAI tool schema to be sanitized, got %#v", schema)
	}
	if schemaContainsKey(schema, "required") {
		t.Fatalf("expected empty required field to be removed, got %#v", schema)
	}
}

func schemaContainsKey(value interface{}, key string) bool {
	switch v := value.(type) {
	case map[string]interface{}:
		if _, ok := v[key]; ok {
			return true
		}
		for _, child := range v {
			if schemaContainsKey(child, key) {
				return true
			}
		}
	case []interface{}:
		for _, child := range v {
			if schemaContainsKey(child, key) {
				return true
			}
		}
	}
	return false
}

func TestParseModelAndThinking(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantModel string
	}{
		// Format normalization: dash → dot for new versions without code changes.
		{"new opus dash form", "claude-opus-4-8", "claude-opus-4.8"},
		{"new opus dot form", "claude-opus-4.8", "claude-opus-4.8"},
		{"existing opus dash form", "claude-opus-4-7", "claude-opus-4.7"},
		{"existing opus dot form", "claude-opus-4.7", "claude-opus-4.7"},
		{"sonnet dash form", "claude-sonnet-4-6", "claude-sonnet-4.6"},
		{"sonnet dot form", "claude-sonnet-4.6", "claude-sonnet-4.6"},
		{"haiku dash form", "claude-haiku-4-5", "claude-haiku-4.5"},
		{"haiku dot form", "claude-haiku-4.5", "claude-haiku-4.5"},
		{"future major bump", "claude-sonnet-5-0", "claude-sonnet-5.0"},

		// Bare family name passes through (no minor to normalize).
		{"bare sonnet 4", "claude-sonnet-4", "claude-sonnet-4"},

		// Dated snapshot must hit the alias before the regex rewrites it.
		{"dated sonnet snapshot", "claude-sonnet-4-20250514", "claude-sonnet-4"},

		// Cross-family legacy IDs.
		{"claude 3.5 sonnet", "claude-3-5-sonnet", "claude-sonnet-4.5"},
		{"claude 3 opus", "claude-3-opus", "claude-sonnet-4.5"},
		{"claude 3 sonnet", "claude-3-sonnet", "claude-sonnet-4"},
		{"claude 3 haiku", "claude-3-haiku", "claude-haiku-4.5"},

		// Non-Anthropic names pass through untouched (no fabrication).
		{"gpt-4o passes through", "gpt-4o", "gpt-4o"},

		// Thinking suffix is no longer special: the name passes through untouched.
		{"thinking suffix passes through", "claude-opus-4.8-thinking", "claude-opus-4.8-thinking"},

		// Unknown models pass through unchanged.
		{"unknown model", "some-other-model", "some-other-model"},
		{"misspelled claude family", "claude-opux-4-8", "claude-opux-4-8"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotModel := ParseModelAndThinking(tc.input)
			if gotModel != tc.wantModel {
				t.Errorf("model: got %q, want %q", gotModel, tc.wantModel)
			}
		})
	}
}

func TestParseModelAndThinkingDoesNotRewriteDatedSnapshotMinor(t *testing.T) {
	// Guards the \b boundary in claudeVersionPattern: without it, the regex would
	// rewrite "claude-sonnet-4-20250514" to "claude-sonnet-4.20250514" before the
	// alias table could redirect it.
	got := ParseModelAndThinking("claude-sonnet-4-20250514")
	if got != "claude-sonnet-4" {
		t.Fatalf("dated snapshot must alias to claude-sonnet-4, got %q", got)
	}
	if strings.Contains(got, ".") {
		t.Fatalf("dated snapshot must not be rewritten with a dot, got %q", got)
	}
}

func TestClaudeToolResultImageAttachedToCurrentMessage(t *testing.T) {
	const imgData = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
	req := &ClaudeRequest{
		Model: "claude-opus-4.8",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "read this image"},
			{
				Role: "assistant",
				Content: []interface{}{
					map[string]interface{}{"type": "tool_use", "id": "tool_1", "name": "read", "input": map[string]interface{}{"path": "a.png"}},
				},
			},
			{
				Role: "user",
				Content: []interface{}{
					map[string]interface{}{
						"type":        "tool_result",
						"tool_use_id": "tool_1",
						"content": []interface{}{
							map[string]interface{}{
								"type": "image",
								"source": map[string]interface{}{
									"type":       "base64",
									"media_type": "image/png",
									"data":       imgData,
								},
							},
						},
					},
				},
			},
		},
	}

	payload := ClaudeToKiro(req, false)
	cur := payload.ConversationState.CurrentMessage.UserInputMessage
	if len(cur.Images) != 1 {
		t.Fatalf("expected tool_result image attached to current message, got %d images", len(cur.Images))
	}
	if cur.Images[0].Format != "png" || cur.Images[0].Source.Bytes != imgData {
		t.Fatalf("unexpected image payload: %+v", cur.Images[0])
	}
	if cur.UserInputMessageContext == nil || len(cur.UserInputMessageContext.ToolResults) != 1 {
		t.Fatalf("expected one tool result preserved")
	}
	if strings.TrimSpace(cur.UserInputMessageContext.ToolResults[0].Content[0].Text) == "" {
		t.Fatalf("expected non-empty placeholder text for image-only tool result")
	}
}

func TestClaudeToolResultMixedTextAndImage(t *testing.T) {
	const imgData = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
	req := &ClaudeRequest{
		Model: "claude-opus-4.8",
		Messages: []ClaudeMessage{
			{
				Role: "user",
				Content: []interface{}{
					map[string]interface{}{
						"type":        "tool_result",
						"tool_use_id": "tool_2",
						"content": []interface{}{
							map[string]interface{}{"type": "text", "text": "here is the screenshot"},
							map[string]interface{}{
								"type": "image",
								"source": map[string]interface{}{
									"type":       "base64",
									"media_type": "image/png",
									"data":       imgData,
								},
							},
						},
					},
				},
			},
		},
	}

	payload := ClaudeToKiro(req, false)
	cur := payload.ConversationState.CurrentMessage.UserInputMessage
	if len(cur.Images) != 1 {
		t.Fatalf("expected one image extracted, got %d", len(cur.Images))
	}
	// This is an ORPHAN tool_result: the only message, no preceding assistant
	// tool_use. Kiro rejects a lone structured tool_result as "improperly formed",
	// so it is NOT attached structurally (upstream-safety); the image — the payload
	// that matters — is carried, and Content is a safe non-empty placeholder. Real
	// clients always send the assistant tool_use turn first, which keeps the result
	// structured (see TestClaudeToKiroKeepsActiveToolTurnStructured).
	if strings.TrimSpace(cur.Content) == "" {
		t.Fatalf("expected non-empty content alongside the carried image, got %q", cur.Content)
	}
	if cur.UserInputMessageContext != nil && len(cur.UserInputMessageContext.ToolResults) != 0 {
		t.Fatalf("orphan tool_result must not be attached structurally (upstream 400 risk)")
	}
}

func TestOpenAIToolResultImageAttachedToCurrentMessage(t *testing.T) {
	const dataURL = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
	req := &OpenAIRequest{
		Model: "claude-sonnet-4.5",
		Messages: []OpenAIMessage{
			{Role: "user", Content: "look at the file"},
			{
				Role:       "tool",
				ToolCallID: "call_img",
				Content: []interface{}{
					map[string]interface{}{
						"type":      "image_url",
						"image_url": map[string]interface{}{"url": dataURL},
					},
				},
			},
		},
	}

	payload := OpenAIToKiro(req, false)
	cur := payload.ConversationState.CurrentMessage.UserInputMessage
	if len(cur.Images) != 1 {
		t.Fatalf("expected tool image attached to current message, got %d", len(cur.Images))
	}
	if cur.Images[0].Format != "png" {
		t.Fatalf("expected png format, got %q", cur.Images[0].Format)
	}
}

func TestOpenAIToolResultImageCarriedWhenFollowedByUser(t *testing.T) {
	const dataURL = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
	req := &OpenAIRequest{
		Model: "claude-sonnet-4.5",
		Messages: []OpenAIMessage{
			{Role: "user", Content: "look at the file"},
			{
				Role: "assistant",
				ToolCalls: []ToolCall{
					{
						ID:   "call_img",
						Type: "function",
						Function: struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						}{Name: "read", Arguments: `{"path":"a.png"}`},
					},
				},
			},
			{
				Role:       "tool",
				ToolCallID: "call_img",
				Content: []interface{}{
					map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": dataURL}},
				},
			},
			{Role: "user", Content: "what do you see?"},
		},
	}

	payload := OpenAIToKiro(req, false)

	// The completed tool cycle is flattened into a history text turn for
	// compaction-safety (structured ToolResults are narrated into Content and the
	// context is cleared), but the tool's image must ride along on that history
	// entry — neither dropped nor leaked into the later user turn.
	var toolHistImages, structuredToolResults int
	for _, h := range payload.ConversationState.History {
		if h.UserInputMessage == nil {
			continue
		}
		toolHistImages += len(h.UserInputMessage.Images)
		if context := h.UserInputMessage.UserInputMessageContext; context != nil {
			structuredToolResults += len(context.ToolResults)
		}
	}
	if toolHistImages != 1 {
		t.Fatalf("expected tool image carried on a history entry, got %d", toolHistImages)
	}
	if structuredToolResults != 0 {
		t.Fatalf("history must not retain structured tool results, got %d", structuredToolResults)
	}

	cur := payload.ConversationState.CurrentMessage.UserInputMessage
	if len(cur.Images) != 0 {
		t.Fatalf("tool image should not leak into a later user message, got %d on current", len(cur.Images))
	}
}

func TestBudgetToEffortMapping(t *testing.T) {
	cases := map[int]string{
		1024: "low", 8191: "low",
		8192: "medium", 16383: "medium",
		16384: "high", 32767: "high",
		32768: "xhigh", 65536: "xhigh",
	}
	for budget, want := range cases {
		if got := budgetToEffort(budget); got != want {
			t.Fatalf("budgetToEffort(%d) = %q, want %q", budget, got, want)
		}
	}
}

// A standard-Anthropic client (thinking.enabled + budget_tokens, no
// output_config) must still drive the effort tier through the mapping, then
// fall through the existing per-model clamp.
func TestResolveReasoningEffortFromBudget(t *testing.T) {
	req := &ClaudeRequest{
		Thinking: &ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 9000},
	}
	if got := resolveReasoningEffort(req); got != "medium" {
		t.Fatalf("effort from budget 9000 = %q, want medium", got)
	}
	// Explicit output_config still wins over the budget.
	req.OutputConfig = &ClaudeOutputConfig{Effort: "low"}
	if got := resolveReasoningEffort(req); got != "low" {
		t.Fatalf("explicit effort overridden by budget, got %q", got)
	}
	// Disabled thinking wins over everything.
	req.Thinking.Type = "disabled"
	if got := resolveReasoningEffort(req); got != "none" {
		t.Fatalf("disabled thinking = %q, want none", got)
	}
}
