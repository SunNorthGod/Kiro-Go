package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newProbeRequest(body string) *http.Request {
	return httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
}

// The 2026-11-05 passthrough incident: an OpenAI-shaped body landed on
// /v1/messages, was parsed as Claude, and AWS rejected the translated Kiro
// payload on all three faces. The detectors below only fire on STRONG markers
// — features the path's native protocol cannot express — so well-formed
// clients are never re-routed.

func TestOpenAIShapedBodyDetection(t *testing.T) {
	openAI := []string{
		`{"model":"m","messages":[{"role":"system","content":"be nice"},{"role":"user","content":"hi"}]}`,
		`{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","tool_calls":[{"id":"t1"}]}]}`,
		`{"model":"m","messages":[{"role":"tool","tool_call_id":"t1","content":"result"}]}`,
	}
	for _, body := range openAI {
		if !openAIShapedBody([]byte(body)) {
			t.Errorf("OpenAI marker not detected: %s", body)
		}
	}

	neutral := []string{
		// Pure Anthropic: top-level system, no system/tool roles.
		`{"model":"m","system":"be nice","messages":[{"role":"user","content":"hi"}]}`,
		// Pure Anthropic tool round-trip: tool_result lives in a user block.
		`{"model":"m","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"r"}]}]}`,
		// Single plain user message: legal in BOTH protocols — must NOT fire,
		// path decides.
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		// Garbage: never re-route on unparseable bodies.
		`not json`,
	}
	for _, body := range neutral {
		if openAIShapedBody([]byte(body)) {
			t.Errorf("false OpenAI detection: %s", body)
		}
	}
}

func TestAnthropicShapedBodyDetection(t *testing.T) {
	anthropic := []string{
		`{"model":"m","system":"be nice","messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"m","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"r"}]}]}`,
		`{"model":"m","messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"..."}]}]}`,
	}
	for _, body := range anthropic {
		if !anthropicShapedBody([]byte(body)) {
			t.Errorf("Anthropic marker not detected: %s", body)
		}
	}

	neutral := []string{
		// Pure OpenAI: system travels as a message role, not a top-level field.
		`{"model":"m","messages":[{"role":"system","content":"be nice"},{"role":"user","content":"hi"}]}`,
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		`not json`,
	}
	for _, body := range neutral {
		if anthropicShapedBody([]byte(body)) {
			t.Errorf("false Anthropic detection: %s", body)
		}
	}
}

// The route decision must match the detector matrix: strong markers re-route,
// everything else keeps the path's native parser.
func TestRouteProtocolBodyDecisions(t *testing.T) {
	h := &Handler{}

	cases := []struct {
		pathProto string
		body      string
		want      string
	}{
		{"anthropic", `{"model":"m","messages":[{"role":"system","content":"s"}]}`, "openai"},
		{"anthropic", `{"model":"m","system":"s","messages":[{"role":"user","content":"hi"}]}`, "anthropic"},
		{"openai", `{"model":"m","system":"s","messages":[{"role":"user","content":"hi"}]}`, "anthropic"},
		{"openai", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, "openai"},
		// Unparseable body keeps the native parser — the normal error path will
		// surface the malformed JSON to the client.
		{"anthropic", `garbage`, "anthropic"},
	}
	for _, tc := range cases {
		got := h.routeProtocolBody(nil, newProbeRequest(tc.body), tc.pathProto)
		if got != tc.want {
			t.Errorf("path=%s body=%.60s: got %s want %s", tc.pathProto, strings.ReplaceAll(tc.body, "\n", " "), got, tc.want)
		}
	}
}
