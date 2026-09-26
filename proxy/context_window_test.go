package proxy

import "testing"

// TestGetContextWindowSize verifies models are classified into the correct
// context window. This drives the input-token count that clients use to decide
// when to compact; misclassifying opus-4.8 or opus-5 (1M) as 200K under-reports
// tokens by 5x and prevents timely compaction.
func TestGetContextWindowSize(t *testing.T) {
	resetModelMetaRegistry(t)

	cases := []struct {
		model string
		want  int
	}{
		{"claude-opus-5", 1_000_000},
		{"claude-opus-5-thinking", 1_000_000},
		{"CLAUDE-OPUS-5", 1_000_000},
		{"claude-opus-5-20260724", 1_000_000},
		{"claude-sonnet-5", 1_000_000},
		{"claude-opus-4.8", 1_000_000},
		{"claude-opus-4-8", 1_000_000},
		{"claude-opus-4.7", 1_000_000},
		{"claude-opus-4.6", 1_000_000},
		{"claude-sonnet-4.6", 1_000_000},
		{"claude-opus-4.8-thinking", 1_000_000},
		{"CLAUDE-OPUS-4.8", 1_000_000},
		{"claude-opus-5", 1_000_000},
		{"claude-opus-5-thinking", 1_000_000},
		{"CLAUDE-OPUS-5", 1_000_000},
		{"claude-opus-5.1", 1_000_000},
		{"claude-opus-5-1", 1_000_000},
		{"claude-sonnet-5", 1_000_000},
		{"claude-haiku-5", 1_000_000},
		{"claude-opus-6", 1_000_000},
		{"claude-opus-4.5", 200_000},
		{"claude-sonnet-4.5", 200_000},
		{"claude-sonnet-4", 200_000},
		{"claude-sonnet-4-20250514", 200_000},
		{"claude-haiku-4.5", 200_000},
		{"claude-3-5-sonnet", 200_000},
		{"unknown-model", 200_000},
	}
	for _, c := range cases {
		if got := getContextWindowSize(c.model); got != c.want {
			t.Errorf("getContextWindowSize(%q) = %d, want %d", c.model, got, c.want)
		}
	}
}

// TestGetContextWindowSizePrefersRegistry verifies Kiro's own tokenLimits win
// over the name heuristic, so a model whose window changes upstream (or a model
// we never hardcoded) reports the real window without a code change.
func TestGetContextWindowSizePrefersRegistry(t *testing.T) {
	resetModelMetaRegistry(t)
	registerModelMeta([]ModelInfo{
		newTestModelInfo("claude-opus-5", 1_000_000, 128000, nil),
		// 名字启发式会判 200K,注册表说 400K —— 应以注册表为准。
		newTestModelInfo("claude-haiku-4.5", 400_000, 64000, nil),
	})

	if got := getContextWindowSize("claude-opus-5"); got != 1_000_000 {
		t.Errorf("registry window for opus-5 = %d, want 1000000", got)
	}
	if got := getContextWindowSize("claude-haiku-4.5"); got != 400_000 {
		t.Errorf("registry window should override heuristic, got %d, want 400000", got)
	}
	// 连字符别名与 thinking 后缀都要命中同一条登记。
	if got := getContextWindowSize("claude-haiku-4-5-thinking"); got != 400_000 {
		t.Errorf("dashed alias + thinking suffix window = %d, want 400000", got)
	}
}
