package proxy

import (
	"kiro-go/logger"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// sseKeepaliveInterval bounds how long a streaming response may stay SILENT
// towards the client before a protocol-appropriate heartbeat is written.
// Middleboxes with idle-read timeouts (Cloudflare free plan cuts a proxied
// response after ~100s without bytes, non-configurable) otherwise kill streams
// during kirogo's three known silent windows: swallowed reasoning events
// (forwardReasoning=false / OmitDisplay), tool_use input buffered until stop,
// and the pre-first-token TTFB. 15s leaves several heartbeats of margin below
// the ~100s ceiling. Override via KIRO_SSE_KEEPALIVE_SECONDS; 0 (or negative)
// disables the keepalive entirely.
var sseKeepaliveInterval = func() time.Duration {
	if v := os.Getenv("KIRO_SSE_KEEPALIVE_SECONDS"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil {
			if secs <= 0 {
				return 0
			}
			return time.Duration(secs) * time.Second
		}
	}
	return 15 * time.Second
}()

// Heartbeat payloads per protocol. The Claude path uses the Anthropic-native
// ping event (official streams emit exactly this shape, so every client is
// compatible); the OpenAI chat / responses paths use an SSE comment line,
// which the SSE spec requires parsers to ignore.
var (
	claudeKeepalivePing  = []byte("event: ping\ndata: {\"type\": \"ping\"}\n\n")
	commentKeepalivePing = []byte(": keepalive\n\n")
)

// sseKeepaliveWriter serializes every write to a streaming response (real
// events, heartbeats, header commit and flushes) behind one mutex, and — once
// Start()ed — emits a heartbeat whenever the stream has been silent for
// `interval`. It implements http.ResponseWriter and http.Flusher so a stream
// handler can shadow its `w`/`flusher` variables with it and leave every
// existing write site untouched.
//
// Lifecycle contract:
//   - Start() only after the response headers have been written and flushed —
//     a heartbeat would otherwise commit an implicit 200 and break pre-stream
//     HTTP error semantics (plugin retries depend on real 5xx status codes).
//   - Stop() before the handler returns (defer). Stop blocks until the
//     heartbeat goroutine has exited, so after Stop no keepalive byte can ever
//     race the server's post-handler teardown.
//   - A failed heartbeat write (client gone) latches writeFailed and stops the
//     goroutine; the event pump discovers the disconnect through its own
//     context/write path (clientGone), which stays untouched.
type sseKeepaliveWriter struct {
	mu      sync.Mutex
	dst     http.ResponseWriter
	flusher http.Flusher

	ping     []byte
	interval time.Duration
	counter  *int64 // process-wide atomic ping counter (nil = don't count)
	label    string // endpoint label for debug logs ("claude"/"openai"/"responses")

	lastActivity time.Time // guarded by mu; any write counts as activity
	started      bool
	stopped      bool
	writeFailed  bool

	stopCh chan struct{} // created by Start
	done   chan struct{} // closed when the heartbeat goroutine exits
}

func newSSEKeepaliveWriter(w http.ResponseWriter, flusher http.Flusher, ping []byte, interval time.Duration, counter *int64, label string) *sseKeepaliveWriter {
	return &sseKeepaliveWriter{
		dst:          w,
		flusher:      flusher,
		ping:         ping,
		interval:     interval,
		counter:      counter,
		label:        label,
		lastActivity: time.Now(),
	}
}

func (k *sseKeepaliveWriter) Header() http.Header { return k.dst.Header() }

func (k *sseKeepaliveWriter) WriteHeader(statusCode int) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.dst.WriteHeader(statusCode)
	k.lastActivity = time.Now()
}

func (k *sseKeepaliveWriter) Write(p []byte) (int, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	n, err := k.dst.Write(p)
	k.lastActivity = time.Now()
	if err != nil {
		k.writeFailed = true
	}
	return n, err
}

func (k *sseKeepaliveWriter) Flush() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.flusher.Flush()
}

// Start launches the heartbeat goroutine. Idempotent; no-op when the keepalive
// is disabled (interval <= 0) or already stopped. See the type comment for the
// "headers must already be flushed" precondition.
func (k *sseKeepaliveWriter) Start() {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.started || k.stopped || k.interval <= 0 {
		return
	}
	k.started = true
	k.lastActivity = time.Now()
	k.stopCh = make(chan struct{})
	k.done = make(chan struct{})
	go k.loop()
}

// Stop terminates the heartbeat goroutine and waits for it to exit: after Stop
// returns it is guaranteed that no further keepalive byte will be written.
// Safe to call multiple times and when the keepalive was never started.
func (k *sseKeepaliveWriter) Stop() {
	k.mu.Lock()
	k.stopped = true
	done := k.done
	if k.stopCh != nil {
		select {
		case <-k.stopCh: // already closed by a previous Stop
		default:
			close(k.stopCh)
		}
	}
	k.mu.Unlock()
	if done != nil {
		<-done
	}
}

func (k *sseKeepaliveWriter) loop() {
	defer close(k.done)
	timer := time.NewTimer(k.interval)
	defer timer.Stop()
	for {
		select {
		case <-k.stopCh:
			return
		case <-timer.C:
		}

		k.mu.Lock()
		if k.stopped || k.writeFailed {
			k.mu.Unlock()
			return
		}
		idle := time.Since(k.lastActivity)
		if idle < k.interval {
			// A real event went out recently — sleep only the remainder.
			k.mu.Unlock()
			timer.Reset(k.interval - idle)
			continue
		}
		if _, err := k.dst.Write(k.ping); err != nil {
			k.writeFailed = true
			k.mu.Unlock()
			return
		}
		k.flusher.Flush()
		k.lastActivity = time.Now()
		k.mu.Unlock()

		var n int64
		if k.counter != nil {
			n = atomic.AddInt64(k.counter, 1)
		}
		logger.Debugf("[SSE] keepalive ping sent on %s stream after %.1fs silence (total=%d)", k.label, idle.Seconds(), n)
		timer.Reset(k.interval)
	}
}
