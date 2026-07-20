package proxy

import (
	"encoding/base64"
	"strings"
	"testing"
)

// tinyPNG is a real 1x1 PNG (same fixture as the tool_result image tests).
const tinyPNGB64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="

func b64(raw []byte) string {
	return base64.StdEncoding.EncodeToString(raw)
}

func TestDetectImageFormatMagicNumbers(t *testing.T) {
	pad := make([]byte, 16)
	cases := []struct {
		name string
		head []byte
		want string
	}{
		{"png", []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}, "png"},
		{"jpeg", []byte{0xff, 0xd8, 0xff, 0xe0}, "jpeg"},
		{"gif87a", []byte("GIF87a"), "gif"},
		{"gif89a", []byte("GIF89a"), "gif"},
		{"webp", []byte{'R', 'I', 'F', 'F', 0x10, 0x00, 0x00, 0x00, 'W', 'E', 'B', 'P'}, "webp"},
		{"bmp", []byte{'B', 'M', 0x36, 0x00}, "bmp"},
		{"tiff-le", []byte{'I', 'I', 0x2a, 0x00}, "tiff"},
		{"tiff-be", []byte{'M', 'M', 0x00, 0x2a}, "tiff"},
		{"unknown", []byte{0x00, 0x01, 0x02, 0x03}, ""},
		{"riff-not-webp", []byte{'R', 'I', 'F', 'F', 0x10, 0x00, 0x00, 0x00, 'W', 'A', 'V', 'E'}, ""},
	}
	for _, tc := range cases {
		if got := detectImageFormat(append(append([]byte{}, tc.head...), pad...)); got != tc.want {
			t.Errorf("%s: detectImageFormat = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestSniffBase64ImageFormat(t *testing.T) {
	jpegBytes := append([]byte{0xff, 0xd8, 0xff, 0xe0}, make([]byte, 32)...)

	if got := sniffBase64ImageFormat(tinyPNGB64); got != "png" {
		t.Fatalf("real png: got %q", got)
	}
	if got := sniffBase64ImageFormat(b64(jpegBytes)); got != "jpeg" {
		t.Fatalf("jpeg: got %q", got)
	}

	// Line-wrapped base64 (whitespace inside the sniff window).
	wrapped := tinyPNGB64[:8] + "\r\n  " + tinyPNGB64[8:20] + "\n" + tinyPNGB64[20:]
	if got := sniffBase64ImageFormat(wrapped); got != "png" {
		t.Fatalf("wrapped png: got %q", got)
	}

	// URL-safe alphabet must decode the same as standard.
	urlSafe := strings.NewReplacer("+", "-", "/", "_").Replace(tinyPNGB64)
	if got := sniffBase64ImageFormat(urlSafe); got != "png" {
		t.Fatalf("url-safe png: got %q", got)
	}

	// Malformed / tiny inputs must not panic and must not claim a format.
	for _, s := range []string{"", "A", "AAA", "Q===", "!!!!not-base64!!!!", "data:", strings.Repeat(" ", 100)} {
		if got := sniffBase64ImageFormat(s); got != "" {
			t.Fatalf("degenerate input %q: got %q, want empty", s, got)
		}
	}
}

func TestCorrectImageFormat(t *testing.T) {
	jpegB64 := b64(append([]byte{0xff, 0xd8, 0xff, 0xe0}, make([]byte, 32)...))
	bmpB64 := b64(append([]byte{'B', 'M', 0x36, 0x00}, make([]byte, 32)...))
	unknownB64 := b64(make([]byte, 32))

	// Declared jpeg, actual png -> corrected (the production IMAGE_MIME_MISMATCH case).
	if got := correctImageFormat(tinyPNGB64, "jpeg"); got != "png" {
		t.Fatalf("jpeg->png: got %q", got)
	}
	// Declaration already matches the bytes -> untouched.
	if got := correctImageFormat(tinyPNGB64, "png"); got != "png" {
		t.Fatalf("png->png: got %q", got)
	}
	if got := correctImageFormat(jpegB64, "jpeg"); got != "jpeg" {
		t.Fatalf("jpeg stays: got %q", got)
	}
	// Unrecognizable bytes -> keep the declaration (never guess).
	if got := correctImageFormat(unknownB64, "jpeg"); got != "jpeg" {
		t.Fatalf("unknown keeps declared: got %q", got)
	}
	// Sniffed format outside the Kiro allow-list (bmp) -> keep the declaration.
	if got := correctImageFormat(bmpB64, "jpeg"); got != "jpeg" {
		t.Fatalf("bmp must not override: got %q", got)
	}
	// Missing declaration adopts the sniffed format.
	if got := correctImageFormat(tinyPNGB64, ""); got != "png" {
		t.Fatalf("empty declared: got %q", got)
	}
}

// Anthropic /v1/messages path: image block declaring image/jpeg with real PNG
// bytes must reach Kiro as png.
func TestClaudeImageMediaTypeMismatchCorrected(t *testing.T) {
	req := &ClaudeRequest{
		Model: "claude-opus-4.8",
		Messages: []ClaudeMessage{
			{
				Role: "user",
				Content: []interface{}{
					map[string]interface{}{"type": "text", "text": "what is in this image?"},
					map[string]interface{}{
						"type": "image",
						"source": map[string]interface{}{
							"type":       "base64",
							"media_type": "image/jpeg",
							"data":       tinyPNGB64,
						},
					},
				},
			},
		},
	}

	payload := ClaudeToKiro(req, false)
	cur := payload.ConversationState.CurrentMessage.UserInputMessage
	if len(cur.Images) != 1 {
		t.Fatalf("expected 1 image, got %d", len(cur.Images))
	}
	if cur.Images[0].Format != "png" {
		t.Fatalf("expected mislabeled jpeg corrected to png, got %q", cur.Images[0].Format)
	}
	if cur.Images[0].Source.Bytes != tinyPNGB64 {
		t.Fatalf("image bytes must be forwarded untouched")
	}
}

// OpenAI /v1/chat/completions path: data URI claiming image/jpeg with real PNG
// bytes must reach Kiro as png.
func TestOpenAIDataURIMimeMismatchCorrected(t *testing.T) {
	req := &OpenAIRequest{
		Model: "claude-sonnet-4.5",
		Messages: []OpenAIMessage{
			{
				Role: "user",
				Content: []interface{}{
					map[string]interface{}{"type": "text", "text": "describe"},
					map[string]interface{}{
						"type":      "image_url",
						"image_url": map[string]interface{}{"url": "data:image/jpeg;base64," + tinyPNGB64},
					},
				},
			},
		},
	}

	payload := OpenAIToKiro(req, false)
	cur := payload.ConversationState.CurrentMessage.UserInputMessage
	if len(cur.Images) != 1 {
		t.Fatalf("expected 1 image, got %d", len(cur.Images))
	}
	if cur.Images[0].Format != "png" {
		t.Fatalf("expected data-URI jpeg corrected to png, got %q", cur.Images[0].Format)
	}
}
