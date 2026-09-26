package proxy

import (
	"testing"
)

// 钉死"裸模型名请求写死注入 effort=high"语义(2026-09-27 主人拍板,不做配置化)。
// 背景:Kiro IDE agent 流量不发 thinking 也不发 output_config(原生后端思考是
// 协议内部的),不注入时生产 fulllog 里 83% 工具轮零思考。

// 裸名请求经 handler 的 effective-thinking 置位后 thinking=true,注入 effort=high。
func TestBareModelInjectsHighEffort(t *testing.T) {
	req := &ClaudeRequest{Model: "claude-opus-5.5", MaxTokens: 4096,
		Messages: []ClaudeMessage{{Role: "user", Content: "hi"}}}
	if !resolveEffectiveThinking(false, req) {
		t.Fatal("bare model request must resolve to thinking=true")
	}
	fields := buildAdditionalModelRequestFields(req, true)
	oc, ok := fields["output_config"].(map[string]interface{})
	if !ok {
		t.Fatalf("bare model request: output_config missing: %v", fields)
	}
	if oc["effort"] != "high" {
		t.Fatalf("effort = %v, want high", oc["effort"])
	}
}

// 显式 thinking.disabled 永远赢过写死注入(一键关思考不被复活)。
func TestBareModelDisabledWins(t *testing.T) {
	req := &ClaudeRequest{Model: "claude-opus-5.5", MaxTokens: 4096,
		Thinking: &ClaudeThinkingConfig{Type: "disabled"},
		Messages: []ClaudeMessage{{Role: "user", Content: "hi"}}}
	if resolveEffectiveThinking(false, req) {
		t.Fatal("explicit disabled must resolve to thinking=false")
	}
	fields := buildAdditionalModelRequestFields(req, false)
	if _, ok := fields["output_config"]; ok {
		t.Fatalf("explicit disabled must suppress default effort: %v", fields)
	}
	if f, ok := fields["thinking"].(map[string]interface{}); !ok || f["type"] != "disabled" {
		t.Fatalf("disabled passthrough missing: %v", fields)
	}
}

// 客户端显式 budget 优先于写死档(budget=1k → low,而非 high)。
// thinking=true 对齐真实链路:handler 对 thinking.type=enabled 置 thinking bool。
func TestBareModelBudgetWins(t *testing.T) {
	req := &ClaudeRequest{Model: "claude-opus-5.5", MaxTokens: 4096,
		Thinking: &ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 1024},
		Messages: []ClaudeMessage{{Role: "user", Content: "hi"}}}
	fields := buildAdditionalModelRequestFields(req, true)
	oc, ok := fields["output_config"].(map[string]interface{})
	if !ok {
		t.Fatalf("budget request: output_config missing: %v", fields)
	}
	if oc["effort"] != "low" {
		t.Fatalf("budget 1024 → effort %v, want low (explicit budget beats default)", oc["effort"])
	}
}
