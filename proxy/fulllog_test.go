package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"kiro-go/config"
)

// ==================== payload encoding ====================

func TestEncodeFulllogPayloadPlainText(t *testing.T) {
	raw := encodeFulllogPayload([]byte(`{"a":1}`), 1024)
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("expected plain string encoding, got %s", raw)
	}
	if s != `{"a":1}` {
		t.Fatalf("payload round-trip mismatch: %q", s)
	}
}

func TestEncodeFulllogPayloadBinaryIsBase64(t *testing.T) {
	bin := []byte{0x00, 0xff, 0xfe, 0x01}
	raw := encodeFulllogPayload(bin, 1024)
	var obj struct {
		Encoding string `json:"encoding"`
		Data     string `json:"data"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("expected object encoding, got %s", raw)
	}
	if obj.Encoding != "base64" {
		t.Fatalf("encoding = %q, want base64", obj.Encoding)
	}
	want := "AP/+AQ==" // base64 of 00 ff fe 01
	if obj.Data != want {
		t.Fatalf("data = %q, want %q", obj.Data, want)
	}
}

func TestEncodeFulllogPayloadTruncatedTextKeepsUTF8(t *testing.T) {
	text := strings.Repeat("猫", 100) // 3 bytes per rune
	raw := encodeFulllogPayload([]byte(text), 50)
	var obj struct {
		Encoding  string `json:"encoding"`
		Data      string `json:"data"`
		Truncated bool   `json:"truncated"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("expected object encoding, got %s", raw)
	}
	if !obj.Truncated || obj.Encoding != "utf8" {
		t.Fatalf("encoding = %q truncated = %v, want utf8/true", obj.Encoding, obj.Truncated)
	}
	// Truncation at a byte cap must not leave a partial rune.
	if !strings.HasSuffix(obj.Data, "猫") && obj.Data != "" {
		t.Fatalf("truncated text ends mid-rune: %q", obj.Data[len(obj.Data)-6:])
	}
	if obj.Data != strings.Repeat("猫", 16) {
		t.Fatalf("unexpected truncated content: %d runes", len([]rune(obj.Data)))
	}
}

func TestEncodeFulllogPayloadTruncatedBinaryIsBase64(t *testing.T) {
	bin := bytes_Repeat(0xff, 100)
	raw := encodeFulllogPayload(bin, 10)
	var obj struct {
		Encoding  string `json:"encoding"`
		Truncated bool   `json:"truncated"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("expected object encoding, got %s", raw)
	}
	if obj.Encoding != "base64" || !obj.Truncated {
		t.Fatalf("encoding = %q truncated = %v, want base64/true", obj.Encoding, obj.Truncated)
	}
}

func bytes_Repeat(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}

// ==================== capture writer ====================

// TestCaptureWriterCapturesPlaintextInsideGzip mounts the capture writer on
// the inner side of WithGzip, exactly like the ServeHTTP wiring: the wire
// carries gzip while the captured bytes are the pre-compression plaintext.
func TestCaptureWriterCapturesPlaintextInsideGzip(t *testing.T) {
	plaintext := "event: message_start\ndata: {\"type\":\"message_start\"}\n\n" +
		strings.Repeat("event: content_block_delta\n", 200)
	var captured []byte
	var capturedStatus int

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cw := newCaptureWriter(w, 1<<20)
		w = cw
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		fmt.Fprint(w, plaintext)
		f, ok := w.(http.Flusher)
		if !ok {
			t.Error("captureWriter must satisfy http.Flusher")
			return
		}
		f.Flush()
		captured = append([]byte(nil), cw.capturedBytes()...)
		capturedStatus = cw.capturedStatus()
	})

	srv := httptest.NewServer(WithGzip(inner))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip (outer middleware must still compress)", resp.Header.Get("Content-Encoding"))
	}
	if string(captured) != plaintext {
		t.Fatalf("captured plaintext mismatch: %d bytes vs %d", len(captured), len(plaintext))
	}
	if capturedStatus != http.StatusOK {
		t.Fatalf("captured status = %d, want implicit 200", capturedStatus)
	}
}

func TestCaptureWriterStatusAndCap(t *testing.T) {
	rec := httptest.NewRecorder()
	cw := newCaptureWriter(rec, 16)
	cw.WriteHeader(201)
	if got := cw.capturedStatus(); got != 201 {
		t.Fatalf("status = %d, want 201", got)
	}
	big := bytes_Repeat('x', 40)
	if _, err := cw.Write(big); err != nil {
		t.Fatalf("write: %v", err)
	}
	if rec.Body.Len() != 40 {
		t.Fatalf("passthrough body = %d bytes, want 40 (cap must not affect the client)", rec.Body.Len())
	}
	if len(cw.capturedBytes()) != 16 {
		t.Fatalf("captured %d bytes, want capped 16", len(cw.capturedBytes()))
	}
	if cw.Unwrap() == nil {
		t.Fatal("Unwrap must return the inner writer")
	}

	// No WriteHeader at all: first Write records an implicit 200.
	rec2 := httptest.NewRecorder()
	cw2 := newCaptureWriter(rec2, 1024)
	_, _ = cw2.Write([]byte("hi"))
	if got := cw2.capturedStatus(); got != http.StatusOK {
		t.Fatalf("implicit status = %d, want 200", got)
	}

	// Nothing written at all: status stays 0 (recorded as 500 by marshalRecord).
	cw3 := newCaptureWriter(httptest.NewRecorder(), 1024)
	if got := cw3.capturedStatus(); got != 0 {
		t.Fatalf("untouched status = %d, want 0", got)
	}
}

// ==================== record marshalling ====================

func TestMarshalRecordExtractsModelFromRequest(t *testing.T) {
	line := marshalRecord(fulllogRecord{
		ts:         time.Unix(1700000000, 0),
		method:     "POST",
		path:       "/v1/messages",
		status:     200,
		durationMs: 42,
		request:    []byte(`{"model":"claude-sonnet-4","messages":[{"role":"user"}]}`),
		response:   []byte(`{"type":"message"}`),
	})
	if line == nil {
		t.Fatal("marshalRecord returned nil")
	}
	var rec fulllogRecordJSON
	if err := json.Unmarshal(line, &rec); err != nil {
		t.Fatalf("line is not JSON: %v", err)
	}
	if rec.Model != "claude-sonnet-4" {
		t.Fatalf("model = %q, want extracted from request body", rec.Model)
	}
	if rec.TS != int64(1700000000)*1000 {
		t.Fatalf("ts = %d, want unix millis", rec.TS)
	}
	var req string
	if err := json.Unmarshal(rec.Request, &req); err != nil {
		t.Fatalf("request must encode as plain string, got %s", rec.Request)
	}
}

func TestMarshalRecordDefaultsStatusZeroTo500(t *testing.T) {
	line := marshalRecord(fulllogRecord{ts: time.Now(), method: "POST", path: "/v1/messages"})
	var rec fulllogRecordJSON
	if err := json.Unmarshal(line, &rec); err != nil {
		t.Fatalf("line is not JSON: %v", err)
	}
	if rec.Status != 500 {
		t.Fatalf("status = %d, want 500 for a handler that never wrote", rec.Status)
	}
}

// ==================== file sink: hourly names + rotation ====================

func TestFulllogFileSinkHourlyFilesAndRotation(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 9, 26, 10, 0, 0, 0, time.Local)
	sink := newFulllogFileSink(dir, 128) // tiny cap to force rotation
	clock := now
	sink.now = func() time.Time { return clock }

	for i := 0; i < 4; i++ {
		if err := sink.WriteRecord(fulllogRecord{
			ts: clock, method: "POST", path: "/v1/messages", status: 200,
			request: []byte(`{"model":"m","messages":[]}`),
		}); err != nil {
			t.Fatalf("write record %d: %v", i, err)
		}
		clock = clock.Add(time.Minute)
	}
	// Advance past the hour: the next record must land in a new hourly file.
	clock = now.Add(2 * time.Hour)
	if err := sink.WriteRecord(fulllogRecord{ts: clock, method: "POST", path: "/v1/messages", status: 200}); err != nil {
		t.Fatalf("write next-hour record: %v", err)
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	files, err := fulllogListFiles(dir)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	byName := map[string]fulllogFileInfo{}
	for _, f := range files {
		byName[f.Name] = f
	}
	base1 := byName["fulllog-20260926-10.log"]
	if base1.Name == "" || base1.Lines < 1 {
		t.Fatalf("hourly base file missing or empty: %+v (all: %v)", base1, files)
	}
	rotated := false
	for name, f := range byName {
		if strings.HasPrefix(name, "fulllog-20260926-10-") {
			rotated = true
			if f.Lines < 1 {
				t.Fatalf("rotated file %s has no records", name)
			}
		}
	}
	if !rotated {
		t.Fatalf("expected at least one -N rotated file, got %v", files)
	}
	if byName["fulllog-20260926-12.log"].Lines != 1 {
		t.Fatalf("next-hour file must hold exactly the post-rotation record: %+v", files)
	}
	// Every listed line count must match a physical line count.
	for _, f := range files {
		n, err := countFulllogLines(filepath.Join(dir, f.Name))
		if err != nil || n != f.Lines {
			t.Fatalf("line count mismatch for %s: listed %d counted %d err %v", f.Name, f.Lines, n, err)
		}
	}
}

// ==================== retention ====================

func TestFulllogCleanupDirRemovesExpiredOnly(t *testing.T) {
	dir := t.TempDir()
	fresh := filepath.Join(dir, "fulllog-20990101-00.log")
	stale := filepath.Join(dir, "fulllog-20000101-00.log")
	other := filepath.Join(dir, "notes.txt")
	for _, p := range []string{fresh, stale, other} {
		if err := os.WriteFile(p, []byte("{}\n"), 0600); err != nil {
			t.Fatalf("seed %s: %v", p, err)
		}
	}
	old := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	fulllogCleanupDir(dir)
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("fresh capture file must survive cleanup: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("non-capture files must be left alone: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale capture file must be removed, stat err = %v", err)
	}
}

// ==================== filename validation ====================

func TestFulllogValidFilename(t *testing.T) {
	valid := []string{
		"fulllog-20260926-10.log",
		"fulllog-20260926-10-1.log",
		"fulllog-20260926-10-42.log",
	}
	invalid := []string{
		"",
		"config.json",
		"fulllog-20260926-10.log.exe",
		"../fulllog-20260926-10.log",
		"sub/fulllog-20260926-10.log",
		`..\fulllog-20260926-10.log`,
		"fulllog-20260926-1.log",   // hour must be zero-padded
		"fulllog-x0260926-10.log",  // date must be digits
		"fulllog-20260926-10.log ", // trailing space
		".log",
	}
	for _, name := range valid {
		if !fulllogValidFilename(name) {
			t.Errorf("%q should be valid", name)
		}
	}
	for _, name := range invalid {
		if fulllogValidFilename(name) {
			t.Errorf("%q should be invalid", name)
		}
	}
}

// ==================== admin endpoints ====================

func TestFulllogAdminListAndDownload(t *testing.T) {
	dir := t.TempDir()
	if err := config.Init(filepath.Join(dir, "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	capDir := fulllogDir()
	if err := os.MkdirAll(capDir, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	content := "{\"a\":1}\n{\"b\":2}\n"
	if err := os.WriteFile(filepath.Join(capDir, "fulllog-20260926-10.log"), []byte(content), 0600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	h := &Handler{}
	rec := httptest.NewRecorder()
	h.apiFulllogList(rec, httptest.NewRequest(http.MethodGet, "/admin/api/fulllog", nil))
	if rec.Code != 200 {
		t.Fatalf("list status = %d", rec.Code)
	}
	var listed struct {
		Files   []fulllogFileInfo `json:"files"`
		Dropped int64             `json:"dropped"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("list body: %v", err)
	}
	if len(listed.Files) != 1 || listed.Files[0].Name != "fulllog-20260926-10.log" ||
		listed.Files[0].Lines != 2 || listed.Files[0].Size != int64(len(content)) {
		t.Fatalf("unexpected listing: %+v", listed.Files)
	}

	// Download accepts only exact capture names.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/api/fulllog/download?file="+filepath.Join("..", "config.json"), nil)
	h.apiFulllogDownload(rec, req)
	if rec.Code != 400 {
		t.Fatalf("traversal attempt must be rejected with 400, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/admin/api/fulllog/download?file=fulllog-20260926-11.log", nil)
	h.apiFulllogDownload(rec, req)
	if rec.Code != 404 {
		t.Fatalf("missing file must 404, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/admin/api/fulllog/download?file=fulllog-20260926-10.log", nil)
	h.apiFulllogDownload(rec, req)
	if rec.Code != 200 {
		t.Fatalf("download status = %d", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	if string(body) != content {
		t.Fatalf("downloaded content mismatch: %q", body)
	}
}

// ==================== queue saturation ====================

func TestFulllogQueueDropCounting(t *testing.T) {
	saved := fulllogQueue
	defer func() { fulllogQueue = saved }()
	// A fresh channel nobody drains: enqueue must drop instead of blocking.
	fulllogQueue = make(chan fulllogRecord, 1)
	fulllogQueue <- fulllogRecord{} // fill the slot

	before := atomic.LoadInt64(&fulllogDropped)
	enqueueFulllogRecord(fulllogRecord{ts: time.Now(), path: "/v1/messages"})
	if got := atomic.LoadInt64(&fulllogDropped); got != before+1 {
		t.Fatalf("dropped = %d, want %d+1 (full queue must drop, never block)", got, before)
	}
}

// ==================== end-to-end through ServeHTTP ====================

// TestFulllogServeHTTPCaptureRunsFullPipeline drives the real ServeHTTP with
// the switch on: /v1/messages without a valid key answers 401, and the record
// must land on disk carrying the request body and the plaintext response.
func TestFulllogServeHTTPCaptureRunsFullPipeline(t *testing.T) {
	dir := t.TempDir()
	if err := config.Init(filepath.Join(dir, "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.SetFullLog(true); err != nil {
		t.Fatalf("enable fulllog: %v", err)
	}
	if !config.GetFullLogEnabled() {
		t.Fatal("fulllog switch must read back enabled")
	}

	body := `{"model":"claude-sonnet-4","messages":[{"role":"user","content":"hi"}]}`
	h := &Handler{}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 from missing API key, got %d", rec.Code)
	}

	var lines []fulllogRecordJSON
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		files, _ := fulllogListFiles(fulllogDir())
		for _, f := range files {
			data, err := os.ReadFile(filepath.Join(fulllogDir(), f.Name))
			if err != nil {
				continue
			}
			lines = lines[:0]
			for _, ln := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
				if ln == "" {
					continue
				}
				var r fulllogRecordJSON
				if json.Unmarshal([]byte(ln), &r) == nil {
					lines = append(lines, r)
				}
			}
			if len(lines) > 0 {
				break
			}
		}
		if len(lines) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(lines) == 0 {
		t.Fatal("no capture record landed on disk within deadline")
	}
	r := lines[0]
	if r.Method != "POST" || r.Path != "/v1/messages" {
		t.Fatalf("method/path = %s/%s", r.Method, r.Path)
	}
	if r.Status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", r.Status)
	}
	if r.Model != "claude-sonnet-4" {
		t.Fatalf("model = %q, want extracted from the captured body", r.Model)
	}
	var reqText string
	if err := json.Unmarshal(r.Request, &reqText); err != nil || reqText != body {
		t.Fatalf("request payload = %s (err %v), want the exact request body as text", r.Request, err)
	}
	var respText string
	if err := json.Unmarshal(r.Response, &respText); err != nil {
		t.Fatalf("response payload must be plaintext text, got %s", r.Response)
	}
	if !strings.Contains(respText, "authentication_error") {
		t.Fatalf("response payload must carry the plaintext error body, got %q", respText)
	}
	if r.DurationMs < 0 {
		t.Fatalf("durationMs = %d", r.DurationMs)
	}
}

// TestFulllogServeHTTPDisabledDoesNotCapture verifies the switch is a hard
// gate: with fullLog off, no capture directory may appear.
func TestFulllogServeHTTPDisabledDoesNotCapture(t *testing.T) {
	dir := t.TempDir()
	if err := config.Init(filepath.Join(dir, "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.SetFullLog(false); err != nil {
		t.Fatalf("disable fulllog: %v", err)
	}
	h := &Handler{}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
	if _, err := os.Stat(fulllogDir()); !os.IsNotExist(err) {
		t.Fatalf("capture directory must not exist when disabled, stat err = %v", err)
	}
}
