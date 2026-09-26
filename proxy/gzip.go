package proxy

import (
	"compress/gzip"
	"net/http"
	"strconv"
	"strings"
)

// WithGzip adds transparent response compression for clients advertising
// Accept-Encoding: gzip. Bandwidth between kirogo and its front gateway (e.g.
// NewAPI on another host) is the target: Go HTTP clients decompress
// transparently, so the gateway sees the same bytes as before.
//
// SSE stays real-time: every Flush forwarded by the handler first flushes the
// gzip stream, so each event leaves as its own compressed chunk instead of
// buffering until stream end. Requests carrying a Range header and HEAD
// requests pass through uncompressed (range semantics and gzip do not compose;
// HEAD has no body to compress).

type gzipResponseWriter struct {
	http.ResponseWriter
	gz      *gzip.Writer
	started bool // gzip header emitted (implied Content-Encoding: gzip)
	code    int  // status code captured from WriteHeader, 0 until called
}

func (g *gzipResponseWriter) start() {
	if g.started {
		return
	}
	g.started = true
	h := g.Header()
	h.Set("Content-Encoding", "gzip")
	h.Add("Vary", "Accept-Encoding")
	h.Del("Content-Length")
	g.gz = gzip.NewWriter(g.ResponseWriter)
}

func (g *gzipResponseWriter) Write(b []byte) (int, error) {
	g.start()
	return g.gz.Write(b)
}

func (g *gzipResponseWriter) WriteHeader(code int) {
	g.code = code
	// 204/304 and 1xx have no body: never emit a gzip header for them, or the
	// empty gzip trailer would confuse strict clients.
	if compressibleStatus(code) {
		g.start()
	}
	g.ResponseWriter.WriteHeader(code)
}

// Flush forwards as: flush gzip stream (emit compressed bytes) then flush the
// underlying writer (put them on the wire). This is what keeps SSE interactive.
func (g *gzipResponseWriter) Flush() {
	if g.started {
		g.gz.Flush()
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the real writer (deadline
// propagation, early flush by inner handlers).
func (g *gzipResponseWriter) Unwrap() http.ResponseWriter { return g.ResponseWriter }

func compressibleStatus(code int) bool {
	return code >= 200 && code != 204 && code != 304
}

// acceptsGzip reports whether the client explicitly offered gzip with a
// non-zero qvalue. Wildcard alone is not honoured: for an API gateway the
// explicit form is the only one observed in practice.
func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		enc := strings.TrimSpace(part)
		if enc == "" {
			continue
		}
		if name, params, found := strings.Cut(enc, ";"); found {
			if !strings.EqualFold(strings.TrimSpace(name), "gzip") {
				continue
			}
			if q := parseQValue(params); q > 0 {
				return true
			}
			continue
		}
		if strings.EqualFold(enc, "gzip") {
			return true
		}
	}
	return false
}

func parseQValue(params string) float64 {
	for _, p := range strings.Split(params, ";") {
		p = strings.TrimSpace(p)
		if strings.HasPrefix(p, "q=") {
			if q, err := strconv.ParseFloat(strings.TrimPrefix(p, "q="), 64); err == nil {
				return q
			}
		}
	}
	return 1 // params without q= default to 1.0 per RFC 7231
}

func WithGzip(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !acceptsGzip(r) || r.Method == http.MethodHead || r.Header.Get("Range") != "" {
			next.ServeHTTP(w, r)
			return
		}
		gw := &gzipResponseWriter{ResponseWriter: w}
		defer func() {
			if gw.started {
				gw.gz.Close() // writes the gzip trailer; body-less paths never start
			}
		}()
		next.ServeHTTP(gw, r)
	})
}
