package proxy

// edge.go centralizes concerns for running behind a CDN / reverse proxy chain
// (Cloudflare 橙云 → Caddy → this server, which listens only on 127.0.0.1). It
// covers three things the CF setup needs:
//
//   - clientIP: recover the REAL visitor IP from the proxy headers instead of
//     seeing only Caddy's container address (r.RemoteAddr).
//   - isEdgeUncacheablePath: mark dynamic / auth-scoped responses so the CDN
//     never caches or shares them across API keys.
//   - handleSpeedTestDownload: a self-hosted, CDN-cacheable speed-test target so
//     CloudflareSpeedTest (CFST) users can rank CF IPs against THIS domain rather
//     than the flaky shared default URL.

import (
	"net"
	"net/http"
	"strconv"
	"strings"
)

// clientIP returns the best-effort real client IP, honoring the proxy chain in
// front of the server. Order of trust (most authoritative first):
//
//  1. CF-Connecting-IP — set by Cloudflare, the real visitor.
//  2. X-Real-IP        — set by our Caddy from CF-Connecting-IP.
//  3. X-Forwarded-For  — first hop is the original client.
//  4. RemoteAddr       — direct connection (no proxy).
//
// This is safe against client spoofing in this deployment because the server
// binds to 127.0.0.1 only: nothing but Caddy can reach it, so inbound
// CF-Connecting-IP / X-Real-IP / X-Forwarded-For headers are always proxy-set.
func clientIP(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); v != "" {
		return v
	}
	if v := strings.TrimSpace(r.Header.Get("X-Real-IP")); v != "" {
		return v
	}
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		// XFF is "client, proxy1, proxy2, ..."; the first entry is the origin client.
		if i := strings.IndexByte(v, ','); i >= 0 {
			v = v[:i]
		}
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// isEdgeUncacheablePath reports whether a path serves dynamic or auth-scoped data
// that a CDN (Cloudflare) must never cache — API responses keyed only by URL
// would otherwise be shared across different API keys / users. Static assets and
// the speed-test endpoint are intentionally excluded so they stay cacheable.
func isEdgeUncacheablePath(path string) bool {
	switch {
	case strings.HasPrefix(path, "/admin/api/"),
		strings.HasPrefix(path, "/user/api/"),
		strings.HasPrefix(path, "/v1/"),
		strings.HasPrefix(path, "/messages"),
		strings.HasPrefix(path, "/chat/"),
		strings.HasPrefix(path, "/responses"),
		strings.HasPrefix(path, "/anthropic/"),
		path == "/health",
		path == "/",
		path == "/models":
		return true
	}
	return false
}

// speedTestMaxBytes caps a single /__down response so the unauthenticated
// endpoint can't be turned into an unbounded bandwidth drain. 100 MiB is plenty
// for CFST to rank IPs, and the CDN caches the body so repeats never hit origin.
const speedTestMaxBytes = 100 << 20

// handleSpeedTestDownload streams up to `bytes` (default 1 MiB, capped at 100
// MiB) of zero bytes for CDN speed-testing tools such as CloudflareSpeedTest
// (`cfst -url https://<domain>/__down?bytes=100000000`). It is unauthenticated
// and marked publicly cacheable so the CDN serves repeats from the edge, giving
// the community a reliable self-hosted replacement for the flaky default test URL.
func (h *Handler) handleSpeedTestDownload(w http.ResponseWriter, r *http.Request) {
	n := int64(1 << 20) // default 1 MiB
	if v := r.URL.Query().Get("bytes"); v != "" {
		if parsed, err := strconv.ParseInt(v, 10, 64); err == nil && parsed >= 0 {
			n = parsed
		}
	}
	if n > speedTestMaxBytes {
		n = speedTestMaxBytes
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(n, 10))
	// Publicly cacheable so Cloudflare absorbs repeated speed tests at the edge.
	w.Header().Set("Cache-Control", "public, max-age=86400")
	if r.Method == http.MethodHead {
		return
	}
	buf := make([]byte, 32*1024) // zero-filled; content is irrelevant for a speed test
	for n > 0 {
		chunk := int64(len(buf))
		if chunk > n {
			chunk = n
		}
		if _, err := w.Write(buf[:chunk]); err != nil {
			return // client hung up (CFST closes early once it has enough)
		}
		n -= chunk
	}
}
