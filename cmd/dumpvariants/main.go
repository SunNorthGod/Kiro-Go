package main

// Dump translated Kiro payload variants for upstream field-bisect experiments.
// V1 = current OpenAI-path shape (no additionalModelRequestFields)
// V2 = current Claude-path shape (effort + max_tokens + thinking label)
// V3 = V2 without additionalModelRequestFields
// V4 = effort only (native shape)
// V5 = effort + thinking adaptive (native toggle-on shape)
// V6 = max_tokens only
import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"kiro-go/proxy"
)

func main() {
	sys := "You are Kiro, an agentic AI software engineer."
	msgs := []proxy.ClaudeMessage{{Role: "user", Content: "Say OK"}}
	base := func() *proxy.ClaudeRequest {
		return &proxy.ClaudeRequest{
			Model: "claude-opus-5.5", MaxTokens: 1024,
			System:   sys,
			Messages: msgs,
		}
	}

	out := map[string]*proxy.KiroPayload{}

	// V1: OpenAI shape
	out["v1_openai_shape"] = proxy.OpenAIToKiro(&proxy.OpenAIRequest{
		Model: "claude-opus-5.5", MaxTokens: 1024,
		Messages: []proxy.OpenAIMessage{{Role: "user", Content: "Say OK"}},
	}, false)

	// V2: current Claude shape (thinking=true → label + fields)
	out["v2_claude_current"] = proxy.ClaudeToKiro(base(), true)

	// V3: Claude shape, thinking=false → no label, no fields
	out["v3_claude_nofields"] = proxy.ClaudeToKiro(base(), false)

	// V4: native shape — effort only
	v4 := proxy.ClaudeToKiro(base(), false)
	v4.AdditionalModelRequestFields = map[string]interface{}{
		"output_config": map[string]interface{}{"effort": "high"},
	}
	out["v4_effort_only"] = v4

	// V5: native shape — effort + thinking adaptive
	v5 := proxy.ClaudeToKiro(base(), false)
	v5.AdditionalModelRequestFields = map[string]interface{}{
		"output_config": map[string]interface{}{"effort": "high"},
		"thinking":      map[string]interface{}{"type": "adaptive"},
	}
	out["v5_effort_thinking_adaptive"] = v5

	// V6: kirogo-style — max_tokens + effort (what the Claude path actually sends)
	v6 := proxy.ClaudeToKiro(base(), false)
	v6.AdditionalModelRequestFields = map[string]interface{}{
		"output_config": map[string]interface{}{"effort": "high"},
		"max_tokens":    1024,
	}
	out["v6_effort_maxtokens"] = v6

	// Tool-round shape: last message is a tool_result → structured ToolResults
	// attached, currentMessage content is the "." placeholder under test
	// (2026-09-30 buyer ticket). The fire script rewrites content per variant.
	toolReq := &proxy.ClaudeRequest{
		Model: "claude-opus-4.8", MaxTokens: 1024,
		System: sys,
		Messages: []proxy.ClaudeMessage{
			{Role: "user", Content: "Read the file /tmp/demo.txt and tell me the first line."},
			{Role: "assistant", Content: []interface{}{map[string]interface{}{
				"type": "tool_use", "id": "toolu_01ABC", "name": "read_file",
				"input": map[string]interface{}{"path": "/tmp/demo.txt"},
			}}},
			{Role: "user", Content: []interface{}{map[string]interface{}{
				"type": "tool_result", "tool_use_id": "toolu_01ABC",
				"content": []interface{}{map[string]interface{}{"type": "text", "text": "line one: hello from demo.txt"}},
			}}},
		},
		Tools: []proxy.ClaudeTool{{
			Name: "read_file",
			Description: "Read a text file from disk.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path": map[string]interface{}{"type": "string"},
				},
				"required": []string{"path"},
			},
		}},
	}
	out["toolround_current"] = proxy.ClaudeToKiro(toolReq, true)

	dir := os.Args[1]
	_ = os.MkdirAll(dir, 0o755)
	for name, p := range out {
		b, _ := json.Marshal(p)
		// keep bodies small for raw curl: strip history (empty anyway)
		if err := os.WriteFile(dir+"/"+name+".json", b, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(name, len(b), "bytes")
	}
	_ = strings.TrimSpace("")
}
