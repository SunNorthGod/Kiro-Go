package proxy

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"kiro-go/config"
	"net/http"
	"net/url"
	"reflect"
	"testing"
	"time"
)

func TestNormalizeChunkBasicProgression(t *testing.T) {
	prev := ""

	if got := normalizeChunk("abc", &prev); got != "abc" {
		t.Fatalf("expected first chunk to pass through, got %q", got)
	}
	if got := normalizeChunk("abcde", &prev); got != "de" {
		t.Fatalf("expected appended delta, got %q", got)
	}
}

func TestNormalizeChunkPrefixRewindDoesNotReplay(t *testing.T) {
	prev := ""

	_ = normalizeChunk("abcde", &prev)
	if got := normalizeChunk("abc", &prev); got != "" {
		t.Fatalf("expected rewind chunk to be ignored, got %q", got)
	}
	if prev != "abcde" {
		t.Fatalf("expected previous snapshot to remain longest version, got %q", prev)
	}
	if got := normalizeChunk("abcdef", &prev); got != "f" {
		t.Fatalf("expected only unseen suffix after rewind, got %q", got)
	}
}

func TestNormalizeChunkOverlapDelta(t *testing.T) {
	prev := "hello world"

	if got := normalizeChunk("world!!!", &prev); got != "!!!" {
		t.Fatalf("expected overlap suffix delta, got %q", got)
	}
}

func TestParseEventStreamFinishesPendingToolUseOnEOF(t *testing.T) {
	stream := bytes.NewReader(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
		"toolUseId": "toolu_1",
		"name":      "mcpIdaProMcpStatus",
		"input":     `{"server":"ida-pro-mcp"}`,
	}))

	var toolUses []KiroToolUse
	var completed bool
	err := parseEventStream(context.Background(), stream, &KiroStreamCallback{
		OnToolUse: func(toolUse KiroToolUse) {
			toolUses = append(toolUses, toolUse)
		},
		OnComplete: func(_, _ int) {
			completed = true
		},
	})
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if !completed {
		t.Fatalf("expected stream completion callback")
	}
	if len(toolUses) != 1 {
		t.Fatalf("expected pending tool use to be emitted on EOF, got %d", len(toolUses))
	}
	if toolUses[0].ToolUseID != "toolu_1" || toolUses[0].Name != "mcpIdaProMcpStatus" {
		t.Fatalf("unexpected tool use: %#v", toolUses[0])
	}
	if got := toolUses[0].Input["server"]; got != "ida-pro-mcp" {
		t.Fatalf("expected parsed tool input, got %#v", toolUses[0].Input)
	}
}

func TestParseEventStreamNilCallbackIsNoOp(t *testing.T) {
	stream := bytes.NewReader(bytes.Join([][]byte{
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "hello"}),
		awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"text": "thinking"}),
		awsEventStreamFrame(t, "contextUsageEvent", map[string]interface{}{"contextUsagePercentage": 12.5}),
		awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{"usage": 1.25}),
		awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"name":  "mcpIdaProMcpStatus",
			"input": `{"server":"ida-pro-mcp"}`,
			"stop":  true,
		}),
	}, nil))

	if err := parseEventStream(context.Background(), stream, nil); err != nil {
		t.Fatalf("expected nil callback to be a no-op, got %v", err)
	}
}

func TestParseEventStreamNilCallbackFieldsAreNoOp(t *testing.T) {
	stream := bytes.NewReader(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
		"content": "hello",
	}))

	if err := parseEventStream(context.Background(), stream, &KiroStreamCallback{}); err != nil {
		t.Fatalf("expected empty callback to be a no-op, got %v", err)
	}
}

// TestParseEventStreamSurfacesCacheMetering: when the upstream meteringEvent
// carries prompt-cache token fields, OnCacheMetering must fire with them.
func TestParseEventStreamSurfacesCacheMetering(t *testing.T) {
	stream := bytes.NewReader(awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{
		"usage":                 1.25,
		"cacheReadInputTokens":  700,
		"cacheWriteInputTokens": 300,
	}))

	var gotRead, gotCreation int
	fired := false
	err := parseEventStream(context.Background(), stream, &KiroStreamCallback{
		OnCacheMetering: func(read, creation int) {
			gotRead, gotCreation = read, creation
			fired = true
		},
	})
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if !fired || gotRead != 700 || gotCreation != 300 {
		t.Fatalf("expected cache metering 700/300, got fired=%v %d/%d", fired, gotRead, gotCreation)
	}
}

// TestParseEventStreamIgnoresZeroCacheMetering: an explicit all-zero pair
// carries no information and must not fire (it would silence the simulation).
func TestParseEventStreamIgnoresZeroCacheMetering(t *testing.T) {
	stream := bytes.NewReader(awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{
		"usage":                 1.25,
		"cacheReadInputTokens":  0,
		"cacheWriteInputTokens": 0,
	}))

	err := parseEventStream(context.Background(), stream, &KiroStreamCallback{
		OnCacheMetering: func(read, creation int) {
			t.Fatalf("expected zero metering to be ignored, got %d/%d", read, creation)
		},
	})
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
}

// A frame whose declared headersLength is bogus (here the high bit is set, which
// is negative on a 32-bit int build and huge on 64-bit) must be skipped without
// panicking on the msgBuf[0:headersLength] slice.
func TestParseEventStreamRejectsBogusHeadersLength(t *testing.T) {
	frame := awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "hi"})
	// Corrupt only the headers_len field (bytes 4:8); leave total_len intact so
	// the whole frame is still consumed as one message.
	binary.BigEndian.PutUint32(frame[4:8], 0x80000000)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("parseEventStream panicked on bogus headersLength: %v", r)
		}
	}()
	if err := parseEventStream(context.Background(), bytes.NewReader(frame), &KiroStreamCallback{}); err != nil {
		t.Fatalf("expected bogus-header frame to be skipped cleanly, got %v", err)
	}
}

func TestHandleToolUseEventGeneratesMissingToolUseID(t *testing.T) {
	var toolUses []KiroToolUse
	current := handleToolUseEvent(map[string]interface{}{
		"name":  "mcpIdaProMcpStatus",
		"input": `{"server":"ida-pro-mcp"}`,
		"stop":  true,
	}, nil, &KiroStreamCallback{
		OnToolUse: func(toolUse KiroToolUse) {
			toolUses = append(toolUses, toolUse)
		},
	})

	if current != nil {
		t.Fatalf("expected stopped tool use to clear current state")
	}
	if len(toolUses) != 1 {
		t.Fatalf("expected one tool use, got %d", len(toolUses))
	}
	if toolUses[0].ToolUseID == "" {
		t.Fatalf("expected generated tool use id")
	}
	if toolUses[0].Name != "mcpIdaProMcpStatus" {
		t.Fatalf("unexpected tool name: %q", toolUses[0].Name)
	}
}

func TestHandleToolUseEventReplacesGeneratedIDWhenRealIDArrives(t *testing.T) {
	var toolUses []KiroToolUse
	callback := &KiroStreamCallback{
		OnToolUse: func(toolUse KiroToolUse) {
			toolUses = append(toolUses, toolUse)
		},
	}

	current := handleToolUseEvent(map[string]interface{}{
		"name":  "mcpIdaProMcpStatus",
		"input": `{"server":`,
	}, nil, callback)
	current = handleToolUseEvent(map[string]interface{}{
		"toolUseId": "toolu_real",
		"name":      "mcpIdaProMcpStatus",
		"input":     `"ida-pro-mcp"}`,
		"stop":      true,
	}, current, callback)

	if current != nil {
		t.Fatalf("expected stopped tool use to clear current state")
	}
	if len(toolUses) != 1 {
		t.Fatalf("expected one completed tool use, got %d", len(toolUses))
	}
	if toolUses[0].ToolUseID != "toolu_real" {
		t.Fatalf("expected real tool id to replace generated id, got %q", toolUses[0].ToolUseID)
	}
	if got := toolUses[0].Input["server"]; got != "ida-pro-mcp" {
		t.Fatalf("expected joined tool input, got %#v", toolUses[0].Input)
	}
}

func TestBuildKiroTransportUsesExplicitProxyURL(t *testing.T) {
	transport := buildKiroTransport("http://proxy.local:8080")
	req := &http.Request{URL: mustParseURL(t, "https://q.us-east-1.amazonaws.com")}

	got, err := transport.Proxy(req)
	if err != nil {
		t.Fatalf("unexpected proxy error: %v", err)
	}
	assertProxyURL(t, got, "http://proxy.local:8080")
}

func TestBuildKiroTransportFallsBackToEnvironmentProxy(t *testing.T) {
	// http.ProxyFromEnvironment caches env vars process-wide on first call, and
	// earlier tests can trigger HTTP requests through it — so resolving an env
	// proxy set via t.Setenv here is order-dependent (flaked in full runs).
	// Assert the wiring instead: empty proxyURL must fall back to the
	// environment resolver, non-empty must not.
	transport := buildKiroTransport("")
	if transport.Proxy == nil {
		t.Fatalf("expected env-proxy fallback to be wired, got nil Proxy")
	}
	got := reflect.ValueOf(transport.Proxy).Pointer()
	want := reflect.ValueOf(http.ProxyFromEnvironment).Pointer()
	if got != want {
		t.Fatalf("expected Proxy to be http.ProxyFromEnvironment")
	}
}

func TestInitKiroHttpClientTimeouts(t *testing.T) {
	InitKiroHttpClient("")
	t.Cleanup(func() { InitKiroHttpClient("") })

	streamClient := kiroHttpStore.Load()
	restClient := kiroRestHttpStore.Load()

	// The streaming client must have NO whole-request timeout so long streams
	// are not hard-cut; it relies on transport-level limits + the per-read idle
	// deadline applied around the response body in CallKiroAPI.
	if streamClient.Timeout != 0 {
		t.Fatalf("expected streaming client to have no whole-request timeout, got %s", streamClient.Timeout)
	}
	// The REST client keeps its short whole-request timeout.
	if restClient.Timeout != 30*time.Second {
		t.Fatalf("expected REST timeout to stay 30s, got %s", restClient.Timeout)
	}
	// Transport-level guards bound connection setup / time-to-headers.
	tr, ok := streamClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport, got %T", streamClient.Transport)
	}
	if tr.TLSHandshakeTimeout == 0 || tr.ResponseHeaderTimeout == 0 {
		t.Fatalf("expected transport TLS/response-header timeouts to be set, got tls=%s hdr=%s",
			tr.TLSHandshakeTimeout, tr.ResponseHeaderTimeout)
	}
}

// TestIdleTimeoutReaderAbortsOnIdle: a body that stalls (no bytes) must trigger
// the idle callback within roughly the configured timeout.
func TestIdleTimeoutReaderAbortsOnIdle(t *testing.T) {
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })

	fired := make(chan struct{}, 1)
	r := newIdleTimeoutReader(pr, 50*time.Millisecond, func() {
		select {
		case fired <- struct{}{}:
		default:
		}
	})
	defer r.Close()

	go func() { _, _ = r.Read(make([]byte, 8)) }()

	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatalf("expected idle callback to fire on a stalled body")
	}
}

// TestIdleTimeoutReaderRenewsOnData: steady data must keep renewing the deadline
// so the idle callback does NOT fire while bytes flow.
func TestIdleTimeoutReaderRenewsOnData(t *testing.T) {
	pr, pw := io.Pipe()

	fired := make(chan struct{}, 1)
	r := newIdleTimeoutReader(pr, 80*time.Millisecond, func() {
		select {
		case fired <- struct{}{}:
		default:
		}
	})
	defer r.Close()

	go func() {
		buf := make([]byte, 8)
		for {
			if _, err := r.Read(buf); err != nil {
				return
			}
		}
	}()

	// Feed a byte every 20ms for ~200ms; well under the 80ms idle window.
	for i := 0; i < 10; i++ {
		if _, err := pw.Write([]byte{'x'}); err != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case <-fired:
		t.Fatalf("idle callback fired while data was still flowing")
	default:
	}
	_ = pw.Close()
}

func TestSetPayloadProfileArnForAccountUsesAccountArn(t *testing.T) {
	payload := &KiroPayload{ProfileArn: "arn:aws:codewhisperer:profile/stale"}

	setPayloadProfileArnForAccount(payload, &config.Account{ProfileArn: " arn:aws:codewhisperer:profile/current "})
	if payload.ProfileArn != "arn:aws:codewhisperer:profile/current" {
		t.Fatalf("expected current account profile ARN, got %q", payload.ProfileArn)
	}
}

func TestSetPayloadProfileArnForAccountPreservesExplicitPayloadArn(t *testing.T) {
	payload := &KiroPayload{ProfileArn: " arn:aws:codewhisperer:profile/explicit "}

	setPayloadProfileArnForAccount(payload, &config.Account{})
	if payload.ProfileArn != "arn:aws:codewhisperer:profile/explicit" {
		t.Fatalf("expected explicit payload profile ARN to be preserved, got %q", payload.ProfileArn)
	}
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("invalid test URL: %v", err)
	}
	return parsed
}

func assertProxyURL(t *testing.T, got *url.URL, want string) {
	t.Helper()
	if got == nil {
		t.Fatalf("expected proxy URL %q, got nil", want)
	}
	if got.String() != want {
		t.Fatalf("expected proxy URL %q, got %q", want, got.String())
	}
}

func awsEventStreamFrame(t *testing.T, eventType string, payload map[string]interface{}) []byte {
	t.Helper()

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	headerValue := []byte(eventType)
	headers := make([]byte, 0, 1+len(":event-type")+1+2+len(headerValue))
	headers = append(headers, byte(len(":event-type")))
	headers = append(headers, []byte(":event-type")...)
	headers = append(headers, byte(7))
	headers = append(headers, byte(len(headerValue)>>8), byte(len(headerValue)))
	headers = append(headers, headerValue...)

	totalLength := 12 + len(headers) + len(payloadBytes) + 4
	frame := make([]byte, 12, totalLength)
	binary.BigEndian.PutUint32(frame[0:4], uint32(totalLength))
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(headers)))
	frame = append(frame, headers...)
	frame = append(frame, payloadBytes...)
	frame = append(frame, 0, 0, 0, 0)
	return frame
}
