package proxy

import (
	"bufio"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestGzipJSONCompressedRoundTrip(t *testing.T) {
	body := `{"message":"` + strings.Repeat("kirogo-gzip", 500) + `"}`
	srv := httptest.NewServer(WithGzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	})))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	// Note: small bodies compressed below the server's output buffer get a
	// legitimate Content-Length back from net/http; large ones are chunked.
	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatalf("body is not gzip: %v", err)
	}
	got, _ := io.ReadAll(zr)
	if string(got) != body {
		t.Fatalf("round-trip mismatch: got %d bytes want %d", len(got), len(body))
	}
}

func TestGzipNotAcceptedPassesThrough(t *testing.T) {
	srv := httptest.NewServer(WithGzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "plain")
	})))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Encoding") != "" {
		t.Fatalf("must not compress without Accept-Encoding: gzip")
	}
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "plain" {
		t.Fatalf("body mutated: %q", b)
	}
}

func TestGzipQZeroNotCompressed(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "plain")
	})
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Accept-Encoding", "gzip;q=0, br")
	WithGzip(handler).ServeHTTP(w, r)
	if w.Header().Get("Content-Encoding") != "" {
		t.Fatalf("gzip;q=0 must disable compression")
	}
}

func TestGzipSSEStreamsEventByEvent(t *testing.T) {
	// The SSE path must stay interactive: after the handler writes event 1 and
	// Flushes, a client must be able to decompress that event without waiting
	// for the stream to end.
	eventCh := make(chan struct{}, 2)
	srv := httptest.NewServer(WithGzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, "event: one\ndata: 1\n\n")
		flusher.Flush()
		eventCh <- struct{}{}
		<-r.Context().Done() // hold the stream open; client exits after reading event 1
	})))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("SSE must be compressed too, got %q", got)
	}

	<-eventCh // handler finished event 1 + Flush
	zr, err := gzip.NewReader(bufio.NewReader(resp.Body))
	if err != nil {
		t.Fatalf("stream is not gzip: %v", err)
	}
	buffered := bufio.NewReader(zr)
	lineCh := make(chan string, 1)
	go func() {
		for {
			line, err := buffered.ReadString('\n')
			if err != nil {
				return
			}
			if strings.HasPrefix(line, "data: ") {
				lineCh <- strings.TrimSpace(line)
				return
			}
		}
	}()
	select {
	case got := <-lineCh:
		if got != "data: 1" {
			t.Fatalf("first event = %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("event 1 not readable after Flush: gzip stream is buffering")
	}
}

func TestGzipRangeAndHeadPassthrough(t *testing.T) {
	handler := WithGzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "payload")
	}))
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	r.Header.Set("Range", "bytes=0-3")
	handler.ServeHTTP(w, r)
	if w.Header().Get("Content-Encoding") != "" {
		t.Fatalf("Range requests must not be compressed")
	}

	w = httptest.NewRecorder()
	r = httptest.NewRequest("HEAD", "/", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	handler.ServeHTTP(w, r)
	if w.Header().Get("Content-Encoding") != "" {
		t.Fatalf("HEAD must not be compressed")
	}
}

func TestGzipNoBodyStatusStaysClean(t *testing.T) {
	handler := WithGzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	w := httptest.NewRecorder()
	r := httptest.NewRequest("DELETE", "/", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	handler.ServeHTTP(w, r)
	if w.Header().Get("Content-Encoding") != "" {
		t.Fatalf("204 must not carry Content-Encoding: gzip")
	}
	if w.Body.Len() != 0 {
		t.Fatalf("204 must stay body-less, got %d bytes", w.Body.Len())
	}
}
