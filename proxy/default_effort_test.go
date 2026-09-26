package proxy

import (
	"path/filepath"
	"strings"
	"testing"

	"kiro-go/config"
)

// 钉死 DefaultEffort(无思考信号请求的默认档注入)语义。
// 背景:Kiro IDE agent 流量不发 thinking 也不发 output_config(原生后端思考是
// 协议内部的),不注入默认档时生产 fulllog 里 83% 工具轮零思考。

func setDefaultEffort(t *testing.T, tier string) {
	t.Helper()
	// 每个测试各自 Init 到自己的 TempDir:包内其它测试(config.Init + t.TempDir)
	// 会把全局 cfgPath 抢到它们已删除的目录上,共享 TestMain 初始化会被踩。
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.UpdateDefaultEffort(tier); err != nil {
		t.Fatalf("UpdateDefaultEffort(%q): %v", tier, err)
	}
	t.Cleanup(func() { _ = config.UpdateDefaultEffort("") })
}

// 无信号 + 未配置默认档 → 不注入(严格 Anthropic 语义,回归保护)。
func TestDefaultEffortOffNoInjection(t *testing.T) {
	setDefaultEffort(t, "")
	req := &ClaudeRequest{Model: "claude-opus-5.5", MaxTokens: 4096,
		Messages: []ClaudeMessage{{Role: "user", Content: "hi"}}}
	fields := buildAdditionalModelRequestFields(req, false)
	for k := range fields {
		if k == "output_config" {
			t.Fatalf("no signal + no defaultEffort: output_config injected: %v", fields)
		}
	}
}

// 无信号 + 配置 defaultEffort → 注入 output_config.effort。
func TestDefaultEffortInjects(t *testing.T) {
	setDefaultEffort(t, "high")
	req := &ClaudeRequest{Model: "claude-opus-5.5", MaxTokens: 4096,
		Messages: []ClaudeMessage{{Role: "user", Content: "hi"}}}
	fields := buildAdditionalModelRequestFields(req, false)
	oc, ok := fields["output_config"].(map[string]interface{})
	if !ok {
		t.Fatalf("defaultEffort=high: output_config missing: %v", fields)
	}
	if oc["effort"] != "high" {
		t.Fatalf("effort = %v, want high", oc["effort"])
	}
}

// 显式 thinking.disabled 永远赢过默认档(一键关思考不被默认注入复活)。
func TestDefaultEffortDisabledWins(t *testing.T) {
	setDefaultEffort(t, "xhigh")
	req := &ClaudeRequest{Model: "claude-opus-5.5", MaxTokens: 4096,
		Thinking: &ClaudeThinkingConfig{Type: "disabled"},
		Messages: []ClaudeMessage{{Role: "user", Content: "hi"}}}
	fields := buildAdditionalModelRequestFields(req, false)
	if _, ok := fields["output_config"]; ok {
		t.Fatalf("explicit disabled must suppress default effort: %v", fields)
	}
	if f, ok := fields["thinking"].(map[string]interface{}); !ok || f["type"] != "disabled" {
		t.Fatalf("disabled passthrough missing: %v", fields)
	}
}

// 客户端显式 budget 优先于默认档(budget=1k → low,而非默认档)。
func TestDefaultEffortBudgetWins(t *testing.T) {
	setDefaultEffort(t, "xhigh")
	req := &ClaudeRequest{Model: "claude-opus-5.5", MaxTokens: 4096,
		Thinking: &ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 1024},
		Messages: []ClaudeMessage{{Role: "user", Content: "hi"}}}
	fields := buildAdditionalModelRequestFields(req, false)
	oc, ok := fields["output_config"].(map[string]interface{})
	if !ok {
		t.Fatalf("budget request: output_config missing: %v", fields)
	}
	if oc["effort"] != "low" {
		t.Fatalf("budget 1024 → effort %v, want low (explicit budget beats default)", oc["effort"])
	}
}

// 非法档位在 UpdateDefaultEffort 就拒绝,不进配置。
func TestDefaultEffortRejectsInvalidTier(t *testing.T) {
	if err := config.UpdateDefaultEffort("ultra"); err == nil {
		t.Fatal("UpdateDefaultEffort(ultra) should fail")
	}
	if err := config.UpdateDefaultEffort("max"); err == nil {
		t.Fatal("UpdateDefaultEffort(max) should fail: max is Kiro-native, not a global default")
	}
	if got := config.GetDefaultEffort(); got != "" {
		t.Fatalf("failed updates must not mutate config, got %q", got)
	}
}

// 大小写与空白归一。
func TestDefaultEffortNormalizes(t *testing.T) {
	setDefaultEffort(t, "  XHigh  ")
	if got := config.GetDefaultEffort(); got != "xhigh" {
		t.Fatalf("GetDefaultEffort = %q, want xhigh", got)
	}
	req := &ClaudeRequest{Model: "claude-opus-5.5", MaxTokens: 4096,
		Messages: []ClaudeMessage{{Role: "user", Content: "hi"}}}
	fields := buildAdditionalModelRequestFields(req, false)
	oc := fields["output_config"].(map[string]interface{})
	if !strings.EqualFold(oc["effort"].(string), "xhigh") {
		t.Fatalf("effort = %v, want xhigh", oc["effort"])
	}
}
