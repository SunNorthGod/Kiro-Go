package proxy

// protocol_detect.go — request-body shape detection for the two protocol
// entrypoints.
//
// 2026-11-05 production: NewAPI's request-body passthrough fed an OpenAI-shaped
// body to the Anthropic channel, it landed on /v1/messages, was parsed as
// Claude, and AWS rejected the translated Kiro payload with
// REQUEST_BODY_INVALID on all three faces. The route table assumed the PATH
// tells the truth about the BODY; passthrough breaks that assumption.
//
// The fix is protocol self-healing: before dispatch, peek at the body and, when
// it carries STRONG markers of the other protocol, route to that protocol's
// handler instead. Strong markers only — features that cannot occur in the
// path's native protocol — so well-formed clients are never re-routed:
//
//	OpenAI markers on /v1/messages     (impossible in Anthropic):
//	  - a message with role=="system"   (Anthropic: top-level "system" field)
//	  - a message with role=="tool"     (Anthropic: tool_result user blocks)
//	  - an assistant message with "tool_calls"
//
//	Anthropic markers on /v1/chat/completions (impossible in OpenAI):
//	  - a top-level "system" field      (OpenAI: system is a message role)
//	  - content blocks with type tool_result / tool_use / thinking
//
// Ambiguous bodies keep the path's native parser. The body is read once and
// replayed for the downstream handler, which reads it as usual.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"kiro-go/logger"
)

// maxBodyProbeBytes caps the body read for detection. The handlers read the
// whole body anyway; this only bounds how much a hostile peer can make the
// proxy buffer before the real size limits apply downstream.
const maxBodyProbeBytes = 64 << 20 // 64 MiB, same order as the largest legit translate request

// peekBody reads and replays the request body. On read failure it responds 400
// itself and returns ok=false (the request cannot proceed either way).
func (h *Handler) peekBody(w http.ResponseWriter, r *http.Request) (body []byte, ok bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyProbeBytes))
	_ = r.Body.Close()
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"failed to read request body","type":"invalid_request_error"}}`))
		return nil, false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return body, true
}

// openAIShapedBody reports whether the JSON body carries OpenAI markers that
// cannot appear in an Anthropic Messages body.
func openAIShapedBody(body []byte) bool {
	var parsed struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return false
	}
	for _, m := range parsed.Messages {
		switch role, _ := m["role"].(string); role {
		case "system", "tool":
			return true
		}
		if _, has := m["tool_calls"]; has {
			return true
		}
	}
	return false
}

// anthropicShapedBody reports whether the JSON body carries Anthropic markers
// that cannot appear in an OpenAI Chat Completions body.
func anthropicShapedBody(body []byte) bool {
	var parsed struct {
		System   json.RawMessage  `json:"system"`
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return false
	}
	if len(parsed.System) > 0 {
		// OpenAI keeps system inside messages; a top-level system field is
		// Anthropic-only. (A "system" MESSAGE is handled by the role scan in
		// the other direction.)
		return true
	}
	for _, m := range parsed.Messages {
		blocks, ok := m["content"].([]any)
		if !ok {
			continue
		}
		for _, b := range blocks {
			bm, ok := b.(map[string]any)
			if !ok {
				continue
			}
			switch t, _ := bm["type"].(string); t {
			case "tool_result", "tool_use", "thinking":
				return true
			}
		}
	}
	return false
}

// routeProtocolBody returns the effective protocol for the request: "openai"
// or "anthropic". pathProto is what the URL says; a body with strong markers
// of the other protocol overrides it (once — the decision is logged).
func (h *Handler) routeProtocolBody(w http.ResponseWriter, r *http.Request, pathProto string) string {
	body, ok := h.peekBody(w, r)
	if !ok {
		return "" // response already written
	}
	switch pathProto {
	case "anthropic":
		if openAIShapedBody(body) {
			logger.Warnf("[Protocol] OpenAI-shaped body on %s; routing to the OpenAI handler", r.URL.Path)
			return "openai"
		}
	case "openai":
		if anthropicShapedBody(body) {
			logger.Warnf("[Protocol] Anthropic-shaped body on %s; routing to the Claude handler", r.URL.Path)
			return "anthropic"
		}
	}
	return pathProto
}
