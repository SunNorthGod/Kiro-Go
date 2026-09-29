package proxy

import (
	"encoding/json"
	"strconv"
	"strings"
)

// Schema-aware tool-input coercion.
//
// Models occasionally emit a tool_use whose non-string argument values arrive
// as JSON-encoded strings — {"todos": "[{\"content\":...}]"} instead of an
// array, "limit": "25" instead of 25 (buyer ticket 2026-09-30, opus-4.8: one
// clean TodoWrite then three deformed ones minutes apart, same account, same
// schema). The deformed call round-trips in the client's history, the model
// sees its own "previous call" in that shape and imitates it — the failure
// locks in until compaction drops the polluted turns. The translation itself
// is type-preserving (verified against fulllog), so the fix is applied at the
// data level: wherever a value is a string but the tool's declared schema says
// array/object/number/boolean, and the string parses to exactly that type,
// unwrap it. Fields the schema types as string (Write content, Bash command)
// are never touched, so JSON-bearing string arguments stay intact.

const coerceMaxDepth = 4

// coerceToolUseInputsInRequest rewrites assistant tool_use inputs in a captured
// request so a deformed call already replayed by the client no longer teaches
// the model the wrong shape.
func coerceToolUseInputsInRequest(req *ClaudeRequest) {
	if req == nil || len(req.Tools) == 0 {
		return
	}
	schemas := make(map[string]map[string]interface{}, len(req.Tools))
	for _, t := range req.Tools {
		if m, ok := t.InputSchema.(map[string]interface{}); ok {
			schemas[t.Name] = ensureObjectSchemaMap(m)
		}
	}
	if len(schemas) == 0 {
		return
	}
	for mi := range req.Messages {
		blocks, ok := req.Messages[mi].Content.([]interface{})
		if !ok {
			continue
		}
		for bi := range blocks {
			blk, ok := blocks[bi].(map[string]interface{})
			if !ok || blk["type"] != "tool_use" {
				continue
			}
			name, _ := blk["name"].(string)
			schema := schemas[name]
			if schema == nil {
				continue
			}
			input, ok := blk["input"].(map[string]interface{})
			if !ok || input == nil {
				continue
			}
			blk["input"] = coerceInputToSchema(input, schema, 0)
		}
	}
}

// coerceInputToSchema returns input with stringified values unwrapped wherever
// the schema declares a matching non-string type. The input map is mutated in
// place and also returned.
func coerceInputToSchema(input map[string]interface{}, schema map[string]interface{}, depth int) map[string]interface{} {
	if depth > coerceMaxDepth || input == nil || schema == nil {
		return input
	}
	props, _ := schema["properties"].(map[string]interface{})
	if props == nil {
		return input
	}
	for key, val := range input {
		ps, _ := props[key].(map[string]interface{})
		if ps == nil {
			continue // no declared type: leave the value alone
		}
		input[key] = coerceValueToSchema(val, ps, depth)
	}
	return input
}

func coerceValueToSchema(val interface{}, ps map[string]interface{}, depth int) interface{} {
	t := schemaDeclaredType(ps)
	switch t {
	case "array":
		if s, ok := val.(string); ok {
			var arr []interface{}
			if json.Unmarshal([]byte(s), &arr) == nil {
				out := make([]interface{}, len(arr))
				item := itemSchema(ps)
				for i := range arr {
					if m, ok := arr[i].(map[string]interface{}); ok && item != nil {
						out[i] = coerceInputToSchema(m, item, depth+1)
					} else if item != nil {
						out[i] = coerceValueToSchema(arr[i], item, depth+1)
					} else {
						out[i] = arr[i]
					}
				}
				return out
			}
		}
		if arr, ok := val.([]interface{}); ok && depth < coerceMaxDepth {
			if item := itemSchema(ps); item != nil {
				for i := range arr {
					if m, ok := arr[i].(map[string]interface{}); ok {
						arr[i] = coerceInputToSchema(m, item, depth+1)
					}
				}
			}
		}
	case "object":
		if s, ok := val.(string); ok {
			var m map[string]interface{}
			if json.Unmarshal([]byte(s), &m) == nil {
				return coerceInputToSchema(m, ps, depth+1)
			}
		}
		if m, ok := val.(map[string]interface{}); ok && depth < coerceMaxDepth {
			return coerceInputToSchema(m, ps, depth+1)
		}
	case "number", "integer":
		if s, ok := val.(string); ok {
			if n, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
				return n
			}
		}
	case "boolean":
		if s, ok := val.(string); ok {
			if b, err := strconv.ParseBool(strings.TrimSpace(s)); err == nil {
				return b
			}
		}
	}
	return val
}

func ensureObjectSchemaMap(m map[string]interface{}) map[string]interface{} {
	if m == nil {
		return map[string]interface{}{"type": "object"}
	}
	return m
}

func schemaDeclaredType(ps map[string]interface{}) string {
	if t, ok := ps["type"].(string); ok {
		return t
	}
	// No plain "type": only trust an anyOf/oneOf branch when every branch agrees.
	for _, wrapper := range []string{"anyOf", "oneOf"} {
		branches, ok := ps[wrapper].([]interface{})
		if !ok || len(branches) == 0 {
			continue
		}
		var agreed string
		unanimous := true
		for i, b := range branches {
			bm, _ := b.(map[string]interface{})
			bt := schemaDeclaredType(bm)
			if bt == "" {
				unanimous = false
				break
			}
			if i == 0 {
				agreed = bt
			} else if bt != agreed {
				unanimous = false
				break
			}
		}
		if unanimous {
			return agreed
		}
	}
	return ""
}

func itemSchema(ps map[string]interface{}) map[string]interface{} {
	m, _ := ps["items"].(map[string]interface{})
	return m
}
