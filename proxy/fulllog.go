package proxy

// fulllog.go implements opt-in full request/response body capture on the relay
// API endpoints for troubleshooting and distillation data extraction.
//
// Wiring: Handler.ServeHTTP mounts the capture only when config.FullLog is on
// and the path is a relay API endpoint. The captureWriter is created inside
// WithGzip (which is the outermost middleware in main.go), so captured
// response bytes are pre-compression plaintext; for SSE that is the raw
// event-stream text. The request body is read once into memory and replaced
// with a NopCloser, so downstream handlers are unaffected; the
// http.MaxBytesReader limit installed in ServeHTTP still applies and a body
// that fails to read is not captured.
//
// Persistence mirrors the async queue style of logs_db.go: one background
// goroutine drains a 2048-entry channel; a full queue drops the record and
// bumps a counter instead of blocking the request path. Records are JSONL,
// one line per request, written to data/fulllog/fulllog-YYYYMMDD-HH.log
// (local time, hourly files). A file rotated at fulllogMaxFileBytes gets a
// -N suffix. Files older than fulllogRetention are removed at writer startup
// and on every hourly tick. Directory mode 0700, file mode 0600.
//
// Payload encoding: request/response bytes are stored as a JSON string when
// they are valid UTF-8, otherwise as {"encoding":"base64","data":...}.
// Truncated payloads always become an object and carry "truncated":true so
// consumers can exclude partial bodies.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"kiro-go/config"
	"kiro-go/logger"
)

const (
	fulllogDirName = "fulllog"

	// fulllogQueueCap bounds the async write queue (records pending on disk).
	fulllogQueueCap = 2048
	// fulllogMaxResponseBytes caps captured response bytes per request (2 MiB);
	// writes beyond it still reach the client but are truncated in the record.
	fulllogMaxResponseBytes = 2 << 20
	// fulllogMaxFileBytes rotates the current hourly file at 50 MiB.
	fulllogMaxFileBytes = int64(50) << 20
	// fulllogRetention deletes capture files older than 7 days.
	fulllogRetention = 7 * 24 * time.Hour
)

// fulllogDir returns the capture directory: <config dir>/fulllog. The config
// dir is the data directory (see config.GetConfigDir), resolved per call so
// tests that re-point the config land in their own directory.
func fulllogDir() string {
	return filepath.Join(config.GetConfigDir(), fulllogDirName)
}

// ==================== response capture ====================

// captureWriter wraps an http.ResponseWriter and records every byte written to
// the client, up to a byte cap. It forwards WriteHeader/Flush to the inner
// writer and supports Unwrap for http.ResponseController. The first Write
// without an explicit WriteHeader records an implicit 200, matching net/http.
type captureWriter struct {
	http.ResponseWriter
	cap    int
	buf    bytes.Buffer
	full   bool
	status int
}

func newCaptureWriter(w http.ResponseWriter, capBytes int) *captureWriter {
	return &captureWriter{ResponseWriter: w, cap: capBytes}
}

func (c *captureWriter) Write(p []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	if !c.full {
		if room := c.cap - c.buf.Len(); room > 0 {
			if len(p) <= room {
				c.buf.Write(p)
			} else {
				c.buf.Write(p[:room])
				c.full = true
			}
		}
		if c.buf.Len() >= c.cap {
			c.full = true
		}
	}
	return c.ResponseWriter.Write(p)
}

func (c *captureWriter) WriteHeader(code int) {
	if c.status == 0 {
		c.status = code
	}
	c.ResponseWriter.WriteHeader(code)
}

// Flush forwards to the inner writer so the handler stays an http.Flusher and
// SSE flushing keeps working through the capture layer.
func (c *captureWriter) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the real writer.
func (c *captureWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

// capturedStatus returns the recorded status code; 0 when the handler never
// wrote headers or body (e.g. panicked early).
func (c *captureWriter) capturedStatus() int { return c.status }

// capturedBytes returns the captured response bytes (never beyond the cap).
func (c *captureWriter) capturedBytes() []byte { return c.buf.Bytes() }

// ==================== per-request account attribution ====================

type fulllogCtxKey struct{}

// fulllogRequestMeta is the per-request account attribution slot. The serving
// account is selected deep inside the relay handlers (pool.Acquire), so those
// handlers report it via fulllogNoteAccount and ServeHTTP reads it when the
// request completes. The meta lives in the request context only while capture
// is active, so the note call is a no-op map lookup otherwise.
type fulllogRequestMeta struct {
	mu        sync.Mutex
	accountID string
}

// fulllogNoteAccount records the account serving the current request.
// Safe to call with any context; it does nothing when capture is inactive.
func fulllogNoteAccount(ctx context.Context, accountID string) {
	if accountID == "" || ctx == nil {
		return
	}
	m, ok := ctx.Value(fulllogCtxKey{}).(*fulllogRequestMeta)
	if !ok {
		return
	}
	m.mu.Lock()
	m.accountID = accountID
	m.mu.Unlock()
}

// account returns the last account reported for this request (failover overwrites).
func (m *fulllogRequestMeta) account() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.accountID
}

// ==================== record and payload encoding ====================

// fulllogRecord is one captured request in memory. request/response hold raw
// bytes; JSON encoding happens in the writer goroutine.
type fulllogRecord struct {
	ts         time.Time
	method     string
	path       string
	model      string
	apiKeyID   string
	accountID  string
	status     int
	durationMs int64
	request    []byte
	response   []byte
}

// fulllogRecordJSON is the on-disk JSONL shape (field order = struct order).
type fulllogRecordJSON struct {
	TS         int64           `json:"ts"` // unix milliseconds
	Method     string          `json:"method"`
	Path       string          `json:"path"`
	Model      string          `json:"model,omitempty"`
	ApiKeyID   string          `json:"apiKeyID,omitempty"`
	AccountID  string          `json:"accountID,omitempty"`
	Status     int             `json:"status"`
	DurationMs int64           `json:"durationMs"`
	Request    json.RawMessage `json:"request"`
	Response   json.RawMessage `json:"response"`
}

// encodeFulllogPayload marshals captured bytes as a JSON string when they are
// valid UTF-8 (original text), or as {"encoding":"base64","data":...} when
// they are not. When the cap truncated the input, the value becomes an object
// carrying "truncated":true; a partial trailing rune is dropped so truncated
// text stays valid UTF-8.
func encodeFulllogPayload(b []byte, limit int) json.RawMessage {
	truncated := false
	if len(b) > limit {
		b = b[:limit]
		truncated = true
		for len(b) > 0 && !utf8.Valid(b) {
			b = b[:len(b)-1] // drop a partial trailing rune (at most 3 bytes)
		}
	}
	if !truncated {
		if utf8.Valid(b) {
			s, _ := json.Marshal(string(b))
			return s
		}
	} else if utf8.Valid(b) {
		out, _ := json.Marshal(struct {
			Encoding  string `json:"encoding"`
			Data      string `json:"data"`
			Truncated bool   `json:"truncated"`
		}{Encoding: "utf8", Data: string(b), Truncated: true})
		return out
	}
	out, _ := json.Marshal(struct {
		Encoding   string `json:"encoding"`
		Data       string `json:"data"`
		Truncated  bool   `json:"truncated,omitempty"`
	}{Encoding: "base64", Data: base64.StdEncoding.EncodeToString(b), Truncated: truncated})
	return out
}

// marshalRecord converts an in-memory record to one JSONL line. When the model
// is unknown it is extracted from the request body (relay request shapes all
// carry a top-level "model" field).
func marshalRecord(rec fulllogRecord) []byte {
	if rec.model == "" && len(rec.request) > 0 {
		var probe struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(rec.request, &probe); err == nil {
			rec.model = strings.TrimSpace(probe.Model)
		}
	}
	status := rec.status
	if status == 0 {
		// Handler never wrote anything (e.g. early panic): net/http answers 500.
		status = http.StatusInternalServerError
	}
	line := fulllogRecordJSON{
		TS:         rec.ts.UnixMilli(),
		Method:     rec.method,
		Path:       rec.path,
		Model:      rec.model,
		ApiKeyID:   rec.apiKeyID,
		AccountID:  rec.accountID,
		Status:     status,
		DurationMs: rec.durationMs,
		Request:    encodeFulllogPayload(rec.request, fulllogMaxResponseBytes),
		Response:   encodeFulllogPayload(rec.response, fulllogMaxResponseBytes),
	}
	out, err := json.Marshal(line)
	if err != nil {
		return nil
	}
	return out
}

// ==================== file sink (hourly file + rotation) ====================

// fulllogFileSink owns the currently open hourly capture file. Not safe for
// concurrent use; the single writer goroutine owns it (tests drive it directly).
type fulllogFileSink struct {
	dir          string
	maxFileBytes int64
	now          func() time.Time // injectable clock for tests

	f    *os.File
	name string
	size int64
}

func newFulllogFileSink(dir string, maxFileBytes int64) *fulllogFileSink {
	return &fulllogFileSink{dir: dir, maxFileBytes: maxFileBytes, now: time.Now}
}

// fulllogFileName is the hourly file name in local time.
func fulllogFileName(t time.Time) string {
	return t.Format("fulllog-20060102-15") + ".log"
}

// fulllogFilenameRe accepts only generated capture names: the hourly base
// name plus optional rotation suffixes. Anything else (path separators,
// traversal, unexpected extensions) is rejected.
var fulllogFilenameRe = regexp.MustCompile(`^fulllog-\d{8}-\d{2}(-\d+)?\.log$`)

func fulllogValidFilename(name string) bool {
	return fulllogFilenameRe.MatchString(name)
}

// WriteRecord appends one record to the current hourly file, opening or
// rotating files as needed. A rotate rename error is reported but does not
// lose the record (the base file keeps growing until rotation succeeds).
func (s *fulllogFileSink) WriteRecord(rec fulllogRecord) error {
	line := marshalRecord(rec)
	if line == nil {
		return fmt.Errorf("fulllog: record marshal failed")
	}
	if err := os.MkdirAll(s.dir, 0700); err != nil {
		return fmt.Errorf("fulllog: mkdir %s: %w", s.dir, err)
	}
	name := fulllogFileName(s.now())
	if s.f == nil || s.name != name {
		if err := s.open(name); err != nil {
			return err
		}
	}
	if _, err := s.f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("fulllog: write %s: %w", s.name, err)
	}
	s.size += int64(len(line)) + 1
	if s.size >= s.maxFileBytes {
		s.rotate()
	}
	return nil
}

func (s *fulllogFileSink) open(name string) error {
	s.closeFile()
	path := filepath.Join(s.dir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("fulllog: open %s: %w", path, err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("fulllog: stat %s: %w", path, err)
	}
	s.f = f
	s.name = name
	s.size = info.Size()
	return nil
}

// rotate renames the current hourly file to its next free -N suffix; the next
// record reopens the base name. The file must be closed before renaming
// (rename of an open file fails on Windows).
func (s *fulllogFileSink) rotate() {
	if s.f == nil {
		return
	}
	s.closeFile()
	base := strings.TrimSuffix(s.name, ".log")
	for n := 1; ; n++ {
		rotated := fmt.Sprintf("%s-%d.log", base, n)
		target := filepath.Join(s.dir, rotated)
		if _, err := os.Stat(target); err == nil {
			continue
		}
		if err := os.Rename(filepath.Join(s.dir, s.name), target); err != nil {
			logger.Warnf("[FullLog] rotate %s -> %s failed: %v", s.name, rotated, err)
		}
		break
	}
	s.name = ""
	s.size = 0
}

func (s *fulllogFileSink) closeFile() {
	if s.f != nil {
		s.f.Close()
		s.f = nil
	}
}

// Close flushes and closes the current file.
func (s *fulllogFileSink) Close() error {
	s.closeFile()
	return nil
}

// ==================== retention ====================

// fulllogCleanupDir removes capture files older than fulllogRetention.
func fulllogCleanupDir(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-fulllogRetention)
	for _, e := range entries {
		if e.IsDir() || !fulllogValidFilename(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err == nil {
			logger.Infof("[FullLog] removed expired capture file %s", e.Name())
		}
	}
}

// ==================== async queue + writer goroutine ====================

var (
	fulllogQueue     chan fulllogRecord
	fulllogStartOnce sync.Once
	fulllogDropped   int64 // atomic; records dropped on a full queue
)

func startFulllogWriter() {
	fulllogQueue = make(chan fulllogRecord, fulllogQueueCap)
	fulllogCleanupDir(fulllogDir())
	go fulllogWriterLoop()
}

// fulllogWriterLoop drains the queue into the hourly capture file. The config
// directory is re-resolved per record so a re-pointed config (tests) switches
// files; the hourly ticker also runs retention cleanup.
func fulllogWriterLoop() {
	var sink *fulllogFileSink
	defer func() {
		if sink != nil {
			sink.Close()
		}
	}()
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case rec, ok := <-fulllogQueue:
			if !ok {
				return
			}
			dir := fulllogDir()
			if sink == nil || sink.dir != dir {
				if sink != nil {
					sink.Close()
				}
				sink = newFulllogFileSink(dir, fulllogMaxFileBytes)
			}
			if err := sink.WriteRecord(rec); err != nil {
				logger.Warnf("[FullLog] %v", err)
			}
		case <-ticker.C:
			fulllogCleanupDir(fulllogDir())
		}
	}
}

// enqueueFulllogRecord fire-and-forget queues one captured request. When the
// queue is saturated the record is dropped and counted; the request path
// never blocks on logging.
func enqueueFulllogRecord(rec fulllogRecord) {
	fulllogStartOnce.Do(startFulllogWriter)
	select {
	case fulllogQueue <- rec:
	default:
		atomic.AddInt64(&fulllogDropped, 1)
	}
}

// ==================== admin endpoints ====================

// fulllogFileInfo is one entry of GET /admin/api/fulllog.
type fulllogFileInfo struct {
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	Lines int64  `json:"lines"`
}

// fulllogListFiles lists capture files in dir sorted by name (chronological).
// A missing directory yields an empty list, not an error.
func fulllogListFiles(dir string) ([]fulllogFileInfo, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return []fulllogFileInfo{}, nil
	}
	files := make([]fulllogFileInfo, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !fulllogValidFilename(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		lines, _ := countFulllogLines(filepath.Join(dir, e.Name()))
		files = append(files, fulllogFileInfo{Name: e.Name(), Size: info.Size(), Lines: lines})
	}
	return files, nil
}

// countFulllogLines counts records by streaming the file for newlines.
func countFulllogLines(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	var lines int64
	buf := make([]byte, 64<<10)
	for {
		n, err := f.Read(buf)
		for _, b := range buf[:n] {
			if b == '\n' {
				lines++
			}
		}
		if err == io.EOF {
			return lines, nil
		}
		if err != nil {
			return lines, err
		}
	}
}

// apiFulllogList serves GET /admin/api/fulllog: capture files with size and
// record counts, plus the queue-drop counter.
func (h *Handler) apiFulllogList(w http.ResponseWriter, r *http.Request) {
	files, _ := fulllogListFiles(fulllogDir())
	json.NewEncoder(w).Encode(map[string]interface{}{
		"files":   files,
		"dropped": atomic.LoadInt64(&fulllogDropped),
	})
}

// apiFulllogDownload serves GET /admin/api/fulllog/download?file=NAME.
// The name must match the generated capture file pattern exactly, which rules
// out path separators and traversal.
func (h *Handler) apiFulllogDownload(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("file")
	if !fulllogValidFilename(name) {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid file name"})
		return
	}
	f, err := os.Open(filepath.Join(fulllogDir(), name))
	if err != nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "file not found"})
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	io.Copy(w, f)
}

// isFulllogCapturePath reports whether the path is a relay API endpoint
// subject to full capture. Alias forms route to the same handlers as the
// canonical paths, so they are captured too.
func isFulllogCapturePath(path string) bool {
	switch path {
	case "/v1/messages", "/messages", "/anthropic/v1/messages",
		"/v1/chat/completions", "/chat/completions",
		"/v1/responses", "/responses":
		return true
	default:
		return false
	}
}
