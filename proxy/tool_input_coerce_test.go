package proxy

import "testing"

// Regression fixture for the 2026-09-30 buyer ticket: a model occasionally
// emits tool arguments as JSON-encoded strings ({"todos": "[...]"},
// "limit": "25"); the deformed call fails client schema validation, gets
// replayed in history, and the model imitates its own malformed example.

var todoWriteSchema = map[string]interface{}{
	"type": "object",
	"properties": map[string]interface{}{
		"todos": map[string]interface{}{
			"type": "array",
			"items": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"content":  map[string]interface{}{"type": "string"},
					"status":   map[string]interface{}{"type": "string"},
					"priority": map[string]interface{}{"type": "string"},
				},
			},
		},
	},
}

func TestCoerceStringifiedArrayBackToArray(t *testing.T) {
	input := map[string]interface{}{
		"todos": `[{"content":"a","status":"in_progress","priority":"high"}]`,
	}
	got := coerceInputToSchema(input, todoWriteSchema, 0)
	arr, ok := got["todos"].([]interface{})
	if !ok {
		t.Fatalf("todos still %T, want []interface{}", got["todos"])
	}
	first, _ := arr[0].(map[string]interface{})
	if first["content"] != "a" {
		t.Fatalf("nested item lost: %v", first)
	}
}

func TestCoerceStringifiedNumberAndBool(t *testing.T) {
	schema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"limit":  map[string]interface{}{"type": "number"},
			"offset": map[string]interface{}{"type": "integer"},
			"flush":  map[string]interface{}{"type": "boolean"},
		},
	}
	input := map[string]interface{}{"limit": "25", "offset": "3", "flush": "true"}
	got := coerceInputToSchema(input, schema, 0)
	if n, ok := got["limit"].(float64); !ok || n != 25 {
		t.Fatalf("limit = %v (%T), want 25", got["limit"], got["limit"])
	}
	if n, ok := got["offset"].(float64); !ok || n != 3 {
		t.Fatalf("offset = %v (%T), want 3", got["offset"], got["offset"])
	}
	if b, ok := got["flush"].(bool); !ok || !b {
		t.Fatalf("flush = %v (%T), want true", got["flush"], got["flush"])
	}
}

func TestStringTypedFieldIsNeverCoerced(t *testing.T) {
	// Write.content / Bash.command carry arbitrary text that may itself be
	// valid JSON — the schema types them string, so they must pass through.
	schema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"content": map[string]interface{}{"type": "string"},
			"config":  map[string]interface{}{"type": "object"},
		},
	}
	jsonText := `{"key": "value"}`
	input := map[string]interface{}{"content": jsonText, "config": jsonText}
	got := coerceInputToSchema(input, schema, 0)
	if s, ok := got["content"].(string); !ok || s != jsonText {
		t.Fatalf("string-typed field was coerced: %v (%T)", got["content"], got["content"])
	}
	if _, ok := got["config"].(map[string]interface{}); !ok {
		t.Fatalf("object-typed field not unwrapped: %T", got["config"])
	}
}

func TestNoDeclaredTypeLeavesValueAlone(t *testing.T) {
	schema := map[string]interface{}{"type": "object", "properties": map[string]interface{}{
		"free": map[string]interface{}{"description": "anything"},
	}}
	input := map[string]interface{}{"free": `["not","coerced"]`}
	got := coerceInputToSchema(input, schema, 0)
	if s, ok := got["free"].(string); !ok || s != `["not","coerced"]` {
		t.Fatalf("undeclared field coerced: %v (%T)", got["free"], got["free"])
	}
}

// The output-side fix: a deformed tool input parsed from the upstream stream
// must be coerced against the declared schema before OnToolUse hands it to the
// client. This is the point that breaks the failure loop — the client
// validates the coerced object, so its error text (which echoed the malformed
// input back to the model) never comes into existence.
func TestFinishToolUseCoercesDeformedInput(t *testing.T) {
	state := &toolUseState{ToolUseID: "toolu_x", Name: "todoWrite"}
	state.InputBuffer.WriteString(`{"todos": "[{\"content\":\"a\",\"status\":\"in_progress\",\"priority\":\"high\"}]"}`)

	var got KiroToolUse
	cb := &KiroStreamCallback{
		OnToolUse: func(tu KiroToolUse) { got = tu },
		ToolSchemas: map[string]map[string]interface{}{
			"todoWrite": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"todos": map[string]interface{}{
						"type": "array",
						"items": map[string]interface{}{
							"type": "object",
							"properties": map[string]interface{}{
								"content":  map[string]interface{}{"type": "string"},
								"status":   map[string]interface{}{"type": "string"},
								"priority": map[string]interface{}{"type": "string"},
							},
						},
					},
				},
			},
		},
	}
	if err := finishToolUse(state, cb); err != nil {
		t.Fatalf("finishToolUse: %v", err)
	}
	arr, ok := got.Input["todos"].([]interface{})
	if !ok {
		t.Fatalf("todos still %T: %v", got.Input["todos"], got.Input["todos"])
	}
	first, _ := arr[0].(map[string]interface{})
	if first["content"] != "a" {
		t.Fatalf("nested item lost: %v", first)
	}
}

// And the no-schema fallback: without a declared schema the input is relayed
// exactly as parsed.
func TestFinishToolUseWithoutSchemaRelaysVerbatim(t *testing.T) {
	state := &toolUseState{ToolUseID: "toolu_y", Name: "mystery"}
	state.InputBuffer.WriteString(`{"stuff": "[1,2]"}`)
	var got KiroToolUse
	cb := &KiroStreamCallback{OnToolUse: func(tu KiroToolUse) { got = tu }}
	if err := finishToolUse(state, cb); err != nil {
		t.Fatalf("finishToolUse: %v", err)
	}
	if s, ok := got.Input["stuff"].(string); !ok || s != "[1,2]" {
		t.Fatalf("relay altered: %v (%T)", got.Input["stuff"], got.Input["stuff"])
	}
}

// The upstream echoes its own name normalization: we send "todoWrite", the
// model answers "todo_write" — measured in production 2026-09-30, where the
// exact-key lookup missed and output-side coercion never fired for such tools.
// Both map indexing and lookup go through the canonical form.
func TestCanonicalToolKeyResolvesUpstreamNameVariants(t *testing.T) {
	cases := []struct{ wire, echoed string }{
		{"todoWrite", "todo_write"},
		{"Read", "read"},
		{"Read", "Read"},
		{"mcpIdaDecompile", "mcp_ida_decompile"},
		{"mcp__ida__decompile", "mcpIdaDecompile"},
	}
	for _, c := range cases {
		if canonicalToolKey(c.wire) != canonicalToolKey(c.echoed) {
			t.Errorf("canonical(%q) != canonical(%q)", c.wire, c.echoed)
		}
	}
}

func TestFinishToolUseCoercesUnderCanonicalName(t *testing.T) {
	state := &toolUseState{ToolUseID: "toolu_c", Name: "todo_write"}
	state.InputBuffer.WriteString(`{"todos": "[{\"content\":\"a\",\"status\":\"in_progress\",\"priority\":\"high\"}]"}`)

	schema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"todos": map[string]interface{}{
				"type": "array",
				"items": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"content":  map[string]interface{}{"type": "string"},
						"status":   map[string]interface{}{"type": "string"},
						"priority": map[string]interface{}{"type": "string"},
					},
				},
			},
		},
	}
	var got KiroToolUse
	cb := &KiroStreamCallback{
		OnToolUse: func(tu KiroToolUse) { got = tu },
		// keyed the way CallKiroAPI now indexes: wire name + canonical form
		ToolSchemas: map[string]map[string]interface{}{
			"todoWrite":                       schema,
			canonicalToolKey("todoWrite"): schema,
		},
	}
	if err := finishToolUse(state, cb); err != nil {
		t.Fatalf("finishToolUse: %v", err)
	}
	if _, ok := got.Input["todos"].([]interface{}); !ok {
		t.Fatalf("todos still %T under upstream-normalized name", got.Input["todos"])
	}
}
