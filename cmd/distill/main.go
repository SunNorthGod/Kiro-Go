// Command distill converts fulllog capture files into supervised fine-tuning
// JSONL records. It is a pure offline tool using only the standard library.
//
// Input: every fulllog-*.log file in -dir (JSONL, one captured request per
// line, as written by the proxy fulllog writer). Each record must satisfy:
//
//   - path == /v1/messages
//   - status == 200
//   - request/response payloads not truncated
//   - request decodes to a JSON object with non-empty "messages"
//   - response decodes either to a JSON object (non-streaming Anthropic
//     response, kept as-is) or to SSE event-stream text, which is aggregated
//     by concatenating every content_block_delta text_delta fragment into the
//     final assistant text.
//
// Output: one line per accepted record in <out>/distill.jsonl:
//
//	{"model": <requested model>, "messages": <original request messages>, "response": <response object or aggregated text>}
//
// Lines that fail any requirement are skipped and counted; the summary is
// printed to stdout.
package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// fulllogLine mirrors proxy/fulllogRecordJSON (fields needed here only).
type fulllogLine struct {
	Path     string          `json:"path"`
	Model    string          `json:"model"`
	Status   int             `json:"status"`
	Request  json.RawMessage `json:"request"`
	Response json.RawMessage `json:"response"`
}

// trainingRecord is the output JSONL shape.
type trainingRecord struct {
	Model    string          `json:"model"`
	Messages json.RawMessage `json:"messages"`
	Response json.RawMessage `json:"response"`
}

// stats counts every record by outcome.
type stats struct {
	lines           int
	skippedBadLine  int
	skippedPath     int
	skippedStatus   int
	skippedTrunc    int
	skippedRequest  int
	skippedResponse int
	exported        int
}

const (
	// maxLineBytes bounds one JSONL input line: response payloads are capped
	// at 2 MiB by the writer; escaping and base64 can expand the encoding.
	maxLineBytes = 32 << 20
	// fulllogTargetPath is the only endpoint distilled into training data.
	fulllogTargetPath = "/v1/messages"
	// fulllogSuccessStatus selects only fully served requests.
	fulllogSuccessStatus = 200
)

var fulllogFileRe = regexp.MustCompile(`^fulllog-\d{8}-\d{2}(-\d+)?\.log$`)

// decodePayload resolves a captured request/response value: a JSON string
// (plain text payload) or {"encoding":"utf8"|"base64","data":...}. Returns
// the raw bytes and whether the payload was truncated at capture time.
func decodePayload(raw json.RawMessage) ([]byte, bool, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []byte(s), false, nil
	}
	var obj struct {
		Encoding  string `json:"encoding"`
		Data      string `json:"data"`
		Truncated bool   `json:"truncated"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, false, fmt.Errorf("payload is neither string nor object")
	}
	switch obj.Encoding {
	case "utf8":
		return []byte(obj.Data), obj.Truncated, nil
	case "base64":
		b, err := base64.StdEncoding.DecodeString(obj.Data)
		if err != nil {
			return nil, false, fmt.Errorf("bad base64 payload: %v", err)
		}
		return b, obj.Truncated, nil
	default:
		return nil, false, fmt.Errorf("unknown payload encoding %q", obj.Encoding)
	}
}

// aggregateSSEText concatenates all content_block_delta text_delta fragments
// from an SSE event stream. Returns ok=false when the text contains no such
// fragment (error streams, tool-only turns, non-SSE garbage).
func aggregateSSEText(text string) (string, bool) {
	var b strings.Builder
	found := false
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 64<<10), maxLineBytes)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var ev struct {
			Type  string `json:"type"`
			Delta *struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(data), &ev) != nil {
			continue
		}
		if ev.Type == "content_block_delta" && ev.Delta != nil && ev.Delta.Type == "text_delta" {
			b.WriteString(ev.Delta.Text)
			found = true
		}
	}
	if !found {
		return "", false
	}
	return b.String(), true
}

// processFile scans one capture file, emitting training records to out.
func processFile(path string, out *bufio.Writer, st *stats) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), maxLineBytes)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		st.lines++
		if err := processLine(line, out, st); err != nil {
			st.skippedBadLine++
		}
	}
	return sc.Err()
}

func processLine(line []byte, out *bufio.Writer, st *stats) error {
	var rec fulllogLine
	if err := json.Unmarshal(line, &rec); err != nil {
		return err
	}
	if rec.Path != fulllogTargetPath {
		st.skippedPath++
		return nil
	}
	if rec.Status != fulllogSuccessStatus {
		st.skippedStatus++
		return nil
	}
	reqBytes, reqTrunc, err := decodePayload(rec.Request)
	if err != nil {
		return err
	}
	respBytes, respTrunc, err := decodePayload(rec.Response)
	if err != nil {
		return err
	}
	if reqTrunc || respTrunc {
		st.skippedTrunc++
		return nil
	}
	var reqObj struct {
		Model    string          `json:"model"`
		Messages json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(reqBytes, &reqObj); err != nil ||
		reqObj.Model == "" || len(reqObj.Messages) == 0 || string(reqObj.Messages) == "null" {
		st.skippedRequest++
		return nil
	}
	model := rec.Model
	if model == "" {
		model = reqObj.Model
	}

	var response json.RawMessage
	if json.Valid(respBytes) && json.Unmarshal(respBytes, new(map[string]interface{})) == nil {
		response = json.RawMessage(respBytes)
	} else if text, ok := aggregateSSEText(string(respBytes)); ok {
		enc, err := json.Marshal(text)
		if err != nil {
			return err
		}
		response = enc
	} else {
		st.skippedResponse++
		return nil
	}

	encoded, err := json.Marshal(trainingRecord{Model: model, Messages: reqObj.Messages, Response: response})
	if err != nil {
		return err
	}
	if _, err := out.Write(append(encoded, '\n')); err != nil {
		return err
	}
	st.exported++
	return nil
}

func listCaptureFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && fulllogFileRe.MatchString(e.Name()) {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	return files, nil
}

func main() {
	dir := flag.String("dir", "data/fulllog", "fulllog capture directory")
	outDir := flag.String("out", "distill", "output directory for distill.jsonl")
	flag.Parse()

	files, err := listCaptureFiles(*dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "distill: read %s: %v\n", *dir, err)
		os.Exit(1)
	}
	if len(files) == 0 {
		fmt.Fprintf(os.Stderr, "distill: no fulllog-*.log files in %s\n", *dir)
		os.Exit(1)
	}
	if err := os.MkdirAll(*outDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "distill: create %s: %v\n", *outDir, err)
		os.Exit(1)
	}
	outPath := filepath.Join(*outDir, "distill.jsonl")
	outFile, err := os.OpenFile(outPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "distill: open %s: %v\n", outPath, err)
		os.Exit(1)
	}

	var st stats
	out := bufio.NewWriter(outFile)
	for _, path := range files {
		if err := processFile(path, out, &st); err != nil {
			fmt.Fprintf(os.Stderr, "distill: scan %s: %v\n", path, err)
		}
	}
	if err := out.Flush(); err != nil {
		fmt.Fprintf(os.Stderr, "distill: write %s: %v\n", outPath, err)
		outFile.Close()
		os.Exit(1)
	}
	if err := outFile.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "distill: close %s: %v\n", outPath, err)
		os.Exit(1)
	}

	fmt.Printf("scanned %d record(s) from %d file(s)\n", st.lines, len(files))
	fmt.Printf("exported %d -> %s\n", st.exported, outPath)
	fmt.Printf("skipped: %d other path, %d non-200 status, %d truncated, %d bad request, %d unusable response, %d malformed line\n",
		st.skippedPath, st.skippedStatus, st.skippedTrunc, st.skippedRequest, st.skippedResponse, st.skippedBadLine)
}
