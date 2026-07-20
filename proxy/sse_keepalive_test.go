package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"kiro-go/config"
	accountpool "kiro-go/pool"
)

func TestKeepaliveEmitsPingAfterSilence(t *testing.T) {
	rec := httptest.NewRecorder()
	var pings int64
	kw := newSSEKeepaliveWriter(rec, rec, claudeKeepalivePing, 30*time.Millisecond, &pings, "test")
	kw.Start()
	time.Sleep(150 * time.Millisecond)
	kw.Stop()

	got := atomic.LoadInt64(&pings)
	if got < 1 {
		t.Fatalf("expected at least one keepalive ping after silence, got %d", got)
	}
	body := rec.Body.String()
	if int64(strings.Count(body, "event: ping")) != got {
		t.Fatalf("counter (%d) does not match pings written (%d): %q", got, strings.Count(body, "event: ping"), body)
	}
	if !rec.Flushed {
		t.Fatalf("expected pings to be flushed")
	}
}

func TestKeepaliveRealWritesResetTimer(t *testing.T) {
	rec := httptest.NewRecorder()
	var pings int64
	kw := newSSEKeepaliveWriter(rec, rec, commentKeepalivePing, 100*time.Millisecond, &pings, "test")
	kw.Start()
	// Keep writing well below the idle threshold: no ping should ever fire.
	for i := 0; i < 30; i++ {
		if _, err := fmt.Fprintf(kw, "data: event-%d\n\n", i); err != nil {
			t.Fatalf("write %d failed: %v", i, err)
		}
		kw.Flush()
		time.Sleep(10 * time.Millisecond)
	}
	kw.Stop()

	if got := atomic.LoadInt64(&pings); got != 0 {
		t.Fatalf("expected no keepalive pings while real events flow, got %d", got)
	}
	if strings.Contains(rec.Body.String(), "keepalive") {
		t.Fatalf("expected no keepalive bytes in body, got %q", rec.Body.String())
	}
}

func TestKeepaliveStopPreventsFurtherWrites(t *testing.T) {
	rec := httptest.NewRecorder()
	var pings int64
	kw := newSSEKeepaliveWriter(rec, rec, commentKeepalivePing, 20*time.Millisecond, &pings, "test")
	kw.Start()
	time.Sleep(50 * time.Millisecond)
	kw.Stop()

	lenAtStop := rec.Body.Len()
	pingsAtStop := atomic.LoadInt64(&pings)
	time.Sleep(100 * time.Millisecond)
	if rec.Body.Len() != lenAtStop {
		t.Fatalf("keepalive wrote after Stop: %d -> %d bytes", lenAtStop, rec.Body.Len())
	}
	if got := atomic.LoadInt64(&pings); got != pingsAtStop {
		t.Fatalf("ping counter moved after Stop: %d -> %d", pingsAtStop, got)
	}
	// Stop is idempotent.
	kw.Stop()
}

func TestKeepaliveStartAfterStopIsNoop(t *testing.T) {
	rec := httptest.NewRecorder()
	var pings int64
	kw := newSSEKeepaliveWriter(rec, rec, commentKeepalivePing, 10*time.Millisecond, &pings, "test")
	kw.Stop()
	kw.Start() // must not spawn a goroutine after Stop
	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt64(&pings); got != 0 {
		t.Fatalf("expected no pings from a stopped keepalive, got %d", got)
	}
}

func TestKeepaliveDisabledIntervalZero(t *testing.T) {
	rec := httptest.NewRecorder()
	var pings int64
	kw := newSSEKeepaliveWriter(rec, rec, commentKeepalivePing, 0, &pings, "test")
	kw.Start() // no-op when disabled
	time.Sleep(30 * time.Millisecond)
	kw.Stop()
	if got := atomic.LoadInt64(&pings); got != 0 {
		t.Fatalf("expected disabled keepalive to send nothing, got %d pings", got)
	}
	// Still usable as a plain passthrough writer.
	if _, err := kw.Write([]byte("data: hi\n\n")); err != nil {
		t.Fatalf("passthrough write failed: %v", err)
	}
	if rec.Body.String() != "data: hi\n\n" {
		t.Fatalf("unexpected body: %q", rec.Body.String())
	}
}

// TestKeepaliveConcurrentWrites hammers the writer from several goroutines
// while pings fire aggressively; run with -race. Every SSE frame must stay
// atomic (no interleaved bytes) because all writes share one lock.
func TestKeepaliveConcurrentWrites(t *testing.T) {
	rec := httptest.NewRecorder()
	var pings int64
	kw := newSSEKeepaliveWriter(rec, rec, commentKeepalivePing, time.Millisecond, &pings, "test")
	kw.Start()

	const writers = 4
	const writesEach = 150
	var wg sync.WaitGroup
	for g := 0; g < writers; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < writesEach; i++ {
				fmt.Fprintf(kw, "data: EVENT\n\n")
				kw.Flush()
			}
		}()
	}
	wg.Wait()
	time.Sleep(10 * time.Millisecond) // leave room for a final ping
	kw.Stop()

	body := rec.Body.String()
	events := 0
	for _, seg := range strings.Split(strings.TrimSuffix(body, "\n\n"), "\n\n") {
		switch seg {
		case "data: EVENT":
			events++
		case ": keepalive":
		default:
			t.Fatalf("corrupted SSE frame %q (writes interleaved?)", seg)
		}
	}
	if events != writers*writesEach {
		t.Fatalf("expected %d intact event frames, got %d", writers*writesEach, events)
	}
}

// failingWriter always errors; the keepalive must latch the failure, stop
// pinging and never panic.
type failingWriter struct{ writes int64 }

func (f *failingWriter) Header() http.Header       { return http.Header{} }
func (f *failingWriter) WriteHeader(int)           {}
func (f *failingWriter) Flush()                    {}
func (f *failingWriter) Write(p []byte) (int, error) {
	atomic.AddInt64(&f.writes, 1)
	return 0, errors.New("client gone")
}

func TestKeepaliveWriteFailureStopsPings(t *testing.T) {
	fw := &failingWriter{}
	var pings int64
	kw := newSSEKeepaliveWriter(fw, fw, commentKeepalivePing, 10*time.Millisecond, &pings, "test")
	kw.Start()
	time.Sleep(80 * time.Millisecond)
	kw.Stop()

	if got := atomic.LoadInt64(&fw.writes); got != 1 {
		t.Fatalf("expected exactly one failed ping attempt before latching, got %d", got)
	}
	if got := atomic.LoadInt64(&pings); got != 0 {
		t.Fatalf("failed pings must not be counted, got %d", got)
	}
}

// TestClaudeStreamEarlyHeadersAndKeepalive is the end-to-end shape of the fix:
// upstream returns 2xx immediately but stays silent before the first content
// event; the handler must flush the 200 headers at once and fill the silence
// with Anthropic-native ping events, then deliver the real content.
func TestClaudeStreamEarlyHeadersAndKeepalive(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:          "only",
		Enabled:     true,
		AccessToken: "token-only",
		ProfileArn:  "arn:aws:codewhisperer:profile/only",
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}
	if err := config.UpdatePreferredEndpoint("kiro"); err != nil {
		t.Fatalf("set preferred endpoint: %v", err)
	}
	if err := config.UpdateEndpointFallback(false); err != nil {
		t.Fatalf("disable endpoint fallback: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(120 * time.Millisecond) // silent window after the 2xx
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "hello after silence",
		}))
	}))
	defer server.Close()

	oldEndpoints := kiroEndpoints
	kiroEndpoints = []kiroEndpoint{{
		URL:    server.URL,
		Origin: "AI_EDITOR",
		Name:   "test",
	}}
	defer func() { kiroEndpoints = oldEndpoints }()

	oldClient := kiroHttpStore.Load()
	kiroHttpStore.Store(&http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{}})
	defer kiroHttpStore.Store(oldClient)

	oldInterval := sseKeepaliveInterval
	sseKeepaliveInterval = 25 * time.Millisecond
	defer func() { sseKeepaliveInterval = oldInterval }()

	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{
		pool:        p,
		promptCache: newPromptCacheTracker(defaultPromptCacheTTL),
	}

	payload := &KiroPayload{}
	payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		Content: "hello",
		ModelID: "claude-sonnet-4.5",
		Origin:  "AI_EDITOR",
	}

	rec := httptest.NewRecorder()
	h.handleClaudeStream(context.Background(), rec, payload, "claude-sonnet-4.5", false, claudeThinkingResponseOptions{}, 1, nil, "")

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	pingIdx := strings.Index(body, "event: ping")
	startIdx := strings.Index(body, "event: message_start")
	if pingIdx == -1 {
		t.Fatalf("expected keepalive pings during the silent window, body=%q", body)
	}
	if startIdx == -1 || !strings.Contains(body, "hello after silence") {
		t.Fatalf("expected real content after the silence, body=%q", body)
	}
	if pingIdx > startIdx {
		t.Fatalf("expected ping before message_start (early flush), ping@%d start@%d", pingIdx, startIdx)
	}
	if got := atomic.LoadInt64(&h.keepalivePings); got < 1 {
		t.Fatalf("expected keepalivePings counter to advance, got %d", got)
	}
}
