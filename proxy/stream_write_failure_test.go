package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"kiro-go/config"
	accountpool "kiro-go/pool"
)

// truncatingWriter accepts the first N writes and then fails every subsequent
// one, reproducing a client (or an intermediate CDN / reverse-proxy hop) that
// drops the response mid-stream.
type truncatingWriter struct {
	hdr      http.Header
	allow    int
	written  int
	failures int
	status   int
}

func newTruncatingWriter(allow int) *truncatingWriter {
	return &truncatingWriter{hdr: http.Header{}, allow: allow}
}

func (t *truncatingWriter) Header() http.Header  { return t.hdr }
func (t *truncatingWriter) WriteHeader(code int) { t.status = code }
func (t *truncatingWriter) Flush()               {}
func (t *truncatingWriter) Write(p []byte) (int, error) {
	if t.written >= t.allow {
		t.failures++
		return 0, errors.New("connection reset by peer")
	}
	t.written++
	return len(p), nil
}

// TestSSEKeepaliveWriterExposesWriteFailure pins the accessor that used to be
// missing: writeFailed was latched internally and never readable, so a stream
// dying mid-flight was indistinguishable from a healthy one.
func TestSSEKeepaliveWriterExposesWriteFailure(t *testing.T) {
	tw := newTruncatingWriter(1)
	var pings int64
	kw := newSSEKeepaliveWriter(tw, tw, claudeKeepalivePing, 0, &pings, "test")

	if kw.WriteFailed() {
		t.Fatal("a fresh writer must not report a write failure")
	}
	if _, err := kw.Write([]byte("first\n\n")); err != nil {
		t.Fatalf("first write should succeed: %v", err)
	}
	if kw.WriteFailed() {
		t.Fatal("successful write must not latch the failure flag")
	}
	if _, err := kw.Write([]byte("second\n\n")); err == nil {
		t.Fatal("second write was expected to fail")
	}
	if !kw.WriteFailed() {
		t.Fatal("a failed write must latch WriteFailed()")
	}
}

// TestClaudeStreamObservesTruncatedDelivery is the regression for the silent
// path: the upstream stream completes normally, but writes to the client have
// been failing, so the handler walks its success path (recording success and
// billing) while the client only received a truncated stream. That case used to
// be completely unobserved; it must now bump the interrupted counter.
func TestClaudeStreamObservesTruncatedDelivery(t *testing.T) {
	frames := [][]byte{
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "part one "}),
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "part two "}),
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "part three"}),
		awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{"stopReason": "END_TURN"}),
	}

	h, restore := newTestStreamHandler(t, frames...)
	defer restore()

	// Allow the header commit plus a couple of events, then fail everything else.
	tw := newTruncatingWriter(3)

	payload := &KiroPayload{}
	payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		Content: "go", ModelID: "claude-opus-4.8", Origin: "AI_EDITOR",
	}

	h.handleClaudeStream(context.Background(), tw, payload, "claude-opus-4.8", false,
		claudeThinkingResponseOptions{}, 1, nil, "")

	if tw.failures == 0 {
		t.Fatal("test setup failed: no write to the client actually failed")
	}
	if got := atomic.LoadInt64(&h.clientDisconnects); got == 0 {
		t.Fatal("a truncated delivery went completely unobserved (interrupted counter never advanced)")
	}
}

// newTestStreamHandler wires a Handler against a fake upstream that replays the
// given AWS event-stream frames. Returns the handler and a restore func for the
// swapped package globals. Shared with claude_stream_signature_test.go.
func newTestStreamHandler(t *testing.T, frames ...[]byte) (*Handler, func()) {
	t.Helper()

	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:          "acct-stream-test",
		Enabled:     true,
		AccessToken: "token",
		ProfileArn:  "arn:aws:codewhisperer:profile/test",
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}
	if err := config.UpdatePreferredEndpoint("kiro"); err != nil {
		t.Fatalf("preferred endpoint: %v", err)
	}
	if err := config.UpdateEndpointFallback(false); err != nil {
		t.Fatalf("endpoint fallback: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		for _, f := range frames {
			_, _ = w.Write(f)
			w.(http.Flusher).Flush()
		}
	}))

	oldEndpoints := kiroEndpoints
	kiroEndpoints = []kiroEndpoint{{URL: server.URL, Origin: "AI_EDITOR", Name: "test"}}

	oldClient := kiroHttpStore.Load()
	kiroHttpStore.Store(&http.Client{Transport: &http.Transport{}})

	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p, promptCache: newPromptCacheTracker(defaultPromptCacheTTL)}

	return h, func() {
		kiroEndpoints = oldEndpoints
		kiroHttpStore.Store(oldClient)
		server.Close()
	}
}
