package proxy

import (
	"encoding/json"
	"testing"
)

// resetModelMetaRegistry 清空全局注册表并在用例结束后再清一次,避免用例之间串味。
func resetModelMetaRegistry(t *testing.T) {
	t.Helper()
	clear := func() {
		modelMetaMu.Lock()
		modelMetaRegistry = map[string]modelMeta{}
		modelMetaMu.Unlock()
	}
	clear()
	t.Cleanup(clear)
}

// newTestModelInfo 造一条 ListAvailableModels 风格的模型信息。effortLevels 为 nil
// 表示该模型没有 effort schema(即不支持思考档位)。
func newTestModelInfo(id string, maxIn, maxOut int, effortLevels []string) ModelInfo {
	m := ModelInfo{ModelId: id}
	m.TokenLimits = &struct {
		MaxInputTokens  int `json:"maxInputTokens"`
		MaxOutputTokens int `json:"maxOutputTokens"`
	}{MaxInputTokens: maxIn, MaxOutputTokens: maxOut}
	if len(effortLevels) > 0 {
		schema := map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"output_config": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"effort": map[string]interface{}{
							"type":    "string",
							"enum":    effortLevels,
							"default": "high",
						},
					},
				},
			},
		}
		raw, _ := json.Marshal(schema)
		m.AdditionalModelRequestFieldsSchema = raw
	}
	return m
}

func TestParseClaudeVersion(t *testing.T) {
	cases := []struct {
		model     string
		wantMajor int
		wantMinor int
		wantOK    bool
	}{
		// 5 代无小版本号 —— 旧正则在这里整个匹配失败。
		{"claude-opus-5", 5, 0, true},
		{"claude-sonnet-5", 5, 0, true},
		{"claude-opus-5-thinking", 5, 0, true},
		{"claude-opus-5-20260724", 5, 0, true},
		{"claude-opus-4.8", 4, 8, true},
		{"claude-opus-4-8", 4, 8, true},
		{"claude-sonnet-4.6", 4, 6, true},
		{"claude-sonnet-4", 4, 0, true},
		// 日期快照不能被当成小版本号。
		{"claude-sonnet-4-20250514", 4, 0, true},
		{"claude-fable-5", 5, 0, true},
		{"claude-3-5-sonnet", 0, 0, false},
		{"gpt-5.6-sol", 0, 0, false},
		{"unknown-model", 0, 0, false},
	}
	for _, c := range cases {
		major, minor, ok := parseClaudeVersion(c.model)
		if ok != c.wantOK || major != c.wantMajor || minor != c.wantMinor {
			t.Errorf("parseClaudeVersion(%q) = (%d, %d, %v), want (%d, %d, %v)",
				c.model, major, minor, ok, c.wantMajor, c.wantMinor, c.wantOK)
		}
	}
}

func TestModelMaxOutputTokens(t *testing.T) {
	resetModelMetaRegistry(t)

	cases := []struct {
		model string
		want  int
	}{
		// opus 5 代与 4.7/4.8 同档 128K(官方规格)。
		{"claude-opus-5", 128000},
		{"claude-opus-5-thinking", 128000},
		{"claude-opus-4.8", 128000},
		{"claude-opus-4-7", 128000},
		{"claude-opus-4.6", 64000},
		{"claude-opus-4.5", 64000},
		{"claude-sonnet-4.6", 64000},
		{"claude-haiku-4.5", 64000},
		{"unknown-model", 64000},
	}
	for _, c := range cases {
		if got := modelMaxOutputTokens(c.model); got != c.want {
			t.Errorf("modelMaxOutputTokens(%q) = %d, want %d", c.model, got, c.want)
		}
	}
}

func TestModelMaxOutputTokensPrefersRegistry(t *testing.T) {
	resetModelMetaRegistry(t)
	registerModelMeta([]ModelInfo{
		newTestModelInfo("claude-sonnet-5", 1_000_000, 128000, nil),
		// 异常小的上游值(< 1024 下限)必须被忽略,否则 max_tokens 会被夹到非法区间。
		newTestModelInfo("claude-opus-4.6", 1_000_000, 512, nil),
	})

	if got := modelMaxOutputTokens("claude-sonnet-5"); got != 128000 {
		t.Errorf("registry max output for sonnet-5 = %d, want 128000", got)
	}
	if got := modelMaxOutputTokens("claude-opus-4.6"); got != 64000 {
		t.Errorf("sub-1024 registry value should be ignored, got %d, want 64000", got)
	}
}

func TestFallbackSchemaPath(t *testing.T) {
	cases := []struct {
		model string
		want  string
	}{
		// 5 代 opus/sonnet 属于 Claude effort 家族:漏掉会让 output_config.effort
		// 整个不下发,思考档位被静默丢弃。
		{"claude-opus-5", "output_config"},
		{"claude-sonnet-5", "output_config"},
		{"claude-opus-4.8", "output_config"},
		{"claude-opus-4.7", "output_config"},
		{"claude-opus-4.6", "output_config"},
		{"claude-sonnet-4.6", "output_config"},
		// 4.5 及更早 schema 为空,发了会被上游 400。
		{"claude-opus-4.5", ""},
		{"claude-sonnet-4.5", ""},
		{"claude-sonnet-4", ""},
		{"claude-haiku-4.5", ""},
		{"claude-fable-5", ""},
		{"deepseek-3.2", ""},
		{"gpt-5.6-sol", "reasoning"},
	}
	for _, c := range cases {
		if got := fallbackSchemaPath(c.model); got != c.want {
			t.Errorf("fallbackSchemaPath(%q) = %q, want %q", c.model, got, c.want)
		}
	}
}

// TestResolveSchemaPathPrefersRegistry: 已登记模型以 Kiro 透出的真实 schema 为准
// (空串表示该模型不支持 additionalModelRequestFields),未登记才走家族兜底。
func TestResolveSchemaPathPrefersRegistry(t *testing.T) {
	resetModelMetaRegistry(t)
	registerModelMeta([]ModelInfo{
		newTestModelInfo("claude-opus-5", 1_000_000, 128000, []string{"low", "medium", "high", "xhigh", "max"}),
		newTestModelInfo("claude-sonnet-4.6", 1_000_000, 64000, nil),
	})

	if got := resolveSchemaPath("claude-opus-5"); got != "output_config" {
		t.Errorf("resolveSchemaPath(claude-opus-5) = %q, want output_config", got)
	}
	// 注册表说没有 effort schema → 不发,哪怕家族兜底会判 output_config。
	if got := resolveSchemaPath("claude-sonnet-4.6"); got != "" {
		t.Errorf("registered model without effort schema should resolve to empty, got %q", got)
	}
	// 未登记 → 家族兜底。
	if got := resolveSchemaPath("claude-opus-4.8"); got != "output_config" {
		t.Errorf("unregistered model should fall back, got %q", got)
	}
}

// TestBuildAdditionalModelRequestFieldsOpus5 是本次修复的核心回归:opus-5 必须拿到
// output_config.effort 与 128K 的 max_tokens。旧实现因白名单漏登记直接返回 nil。
func TestBuildAdditionalModelRequestFieldsOpus5(t *testing.T) {
	resetModelMetaRegistry(t)

	req := &ClaudeRequest{
		Model:        "claude-opus-5",
		MaxTokens:    128000,
		OutputConfig: &ClaudeOutputConfig{Effort: "max"},
	}
	fields := buildAdditionalModelRequestFields(req, false)
	if fields == nil {
		t.Fatal("claude-opus-5 应下发 additionalModelRequestFields,实际为 nil")
	}
	oc, ok := fields["output_config"].(map[string]interface{})
	if !ok {
		t.Fatalf("缺少 output_config,实际字段: %#v", fields)
	}
	if oc["effort"] != "max" {
		t.Errorf("effort = %v, want max", oc["effort"])
	}
	if fields["max_tokens"] != 128000 {
		t.Errorf("max_tokens = %v, want 128000", fields["max_tokens"])
	}
}

// TestResolveModelEffortClampsToRegistry: 客户端发了该模型不支持的档位时,退回官方
// 默认档而不是原样透传(原样透传会被上游 400)。
func TestResolveModelEffortClampsToRegistry(t *testing.T) {
	resetModelMetaRegistry(t)
	registerModelMeta([]ModelInfo{
		// opus-4.6 实测无 xhigh。
		newTestModelInfo("claude-opus-4.6", 1_000_000, 64000, []string{"low", "medium", "high", "max"}),
		newTestModelInfo("claude-opus-5", 1_000_000, 128000, []string{"low", "medium", "high", "xhigh", "max"}),
	})

	req := &ClaudeRequest{OutputConfig: &ClaudeOutputConfig{Effort: "xhigh"}}
	if got := resolveModelEffort(req, "claude-opus-4.6"); got != "high" {
		t.Errorf("unsupported effort should fall back to default, got %q, want high", got)
	}
	if got := resolveModelEffort(req, "claude-opus-5"); got != "xhigh" {
		t.Errorf("supported effort should pass through, got %q, want xhigh", got)
	}
	// 未登记模型保持原行为(已知超集校验)。
	if got := resolveModelEffort(req, "claude-opus-4.8"); got != "xhigh" {
		t.Errorf("unregistered model should keep superset behaviour, got %q", got)
	}
}
