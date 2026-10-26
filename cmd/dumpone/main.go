// Dump the Kiro payload for a captured /v1/messages request, focused on
// structured tool_use/tool_result remnants (id pairing audit).
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"kiro-go/proxy"
)

func main() {
	b, err := os.ReadFile(os.Args[1])
	if err != nil {
		panic(err)
	}
	var req proxy.ClaudeRequest
	if err := json.Unmarshal(b, &req); err != nil {
		panic(err)
	}
	payload := proxy.ClaudeToKiro(&req, true)
	cs := payload.ConversationState
	for i := range cs.History {
		m := cs.History[i]
		if u := m.UserInputMessage; u != nil {
			if ctx := u.UserInputMessageContext; ctx != nil {
				for _, tr := range ctx.ToolResults {
					fmt.Printf("history[%d] USER structured ToolResult id=%q\n", i, tr.ToolUseID)
				}
			}
		}
		if a := m.AssistantResponseMessage; a != nil {
			for _, tu := range a.ToolUses {
				fmt.Printf("history[%d] ASSISTANT toolUse id=%q name=%q\n", i, tu.ToolUseID, tu.Name)
			}
		}
	}
	cm := cs.CurrentMessage.UserInputMessage
	fmt.Printf("currentMessage contentBytes=%d\n", len(cm.Content))
	if ctx := cm.UserInputMessageContext; ctx != nil {
		for _, tr := range ctx.ToolResults {
			fmt.Printf("current structured ToolResult id=%q\n", tr.ToolUseID)
		}
	}
	fmt.Printf("history total=%d\n", len(cs.History))
	// also show first 120 chars of each history entry for orientation
	for i := range cs.History {
		m := cs.History[i]
		if u := m.UserInputMessage; u != nil {
			c := u.Content
			if len(c) > 100 {
				c = c[:100]
			}
			fmt.Printf("  [%d] USER: %s\n", i, c)
		}
		if a := m.AssistantResponseMessage; a != nil {
			c := a.Content
			if len(c) > 100 {
				c = c[:100]
			}
			fmt.Printf("  [%d] ASST: %s\n", i, c)
		}
	}
}
