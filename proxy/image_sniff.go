package proxy

import (
	"bytes"
	"encoding/base64"

	"kiro-go/logger"
)

// kiroImageFormats is the set of image formats the Kiro/CodeWhisperer upstream
// accepts (mirrors the old Rust proxy's get_image_format allow-list:
// jpeg/png/gif/webp).
var kiroImageFormats = map[string]bool{
	"jpeg": true,
	"png":  true,
	"gif":  true,
	"webp": true,
}

// correctImageFormat reconciles the client-declared image format with the real
// base64 payload. AmazonQ validates the declared media type against the actual
// bytes and rejects mismatches with HTTP 400 IMAGE_MIME_MISMATCH; clients
// routinely mislabel (e.g. a PNG screenshot declared image/jpeg), so when the
// magic number identifies a supported format that differs from the declaration
// the sniffed format wins. Unrecognizable bytes keep the declared format, and
// a sniffed format outside the upstream allow-list (bmp/tiff) never overrides
// — that would only trade the mismatch 400 for an unsupported-format 400.
func correctImageFormat(data, declared string) string {
	sniffed := sniffBase64ImageFormat(data)
	if sniffed == "" || sniffed == declared || !kiroImageFormats[sniffed] {
		return declared
	}
	if declared != "" {
		logger.Debugf("[Image] declared media type %q does not match actual bytes, corrected to %q", declared, sniffed)
	}
	return sniffed
}

// sniffBase64ImageFormat decodes just the head of a base64 image payload and
// identifies the format by magic number. Whitespace (line-wrapped base64) and
// the URL-safe alphabet are tolerated; anything undecodable yields "".
func sniffBase64ImageFormat(data string) string {
	// 64 significant chars decode to 48 bytes; the longest magic (WEBP) needs 12.
	const prefixChars = 64
	quad := make([]byte, 0, prefixChars)
collect:
	for i := 0; i < len(data) && len(quad) < prefixChars; i++ {
		switch c := data[i]; c {
		case ' ', '\t', '\r', '\n':
			continue
		case '=':
			break collect
		case '-':
			quad = append(quad, '+')
		case '_':
			quad = append(quad, '/')
		default:
			quad = append(quad, c)
		}
	}
	if trim := len(quad) % 4; trim != 0 {
		quad = quad[:len(quad)-trim]
	}
	if len(quad) < 4 {
		return ""
	}
	raw, err := base64.RawStdEncoding.DecodeString(string(quad))
	if err != nil {
		return ""
	}
	return detectImageFormat(raw)
}

var (
	pngMagic    = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	gifMagic    = []byte("GIF8") // covers GIF87a and GIF89a
	riffMagic   = []byte("RIFF")
	webpMagic   = []byte("WEBP")
	tiffLEMagic = []byte{'I', 'I', 0x2a, 0x00}
	tiffBEMagic = []byte{'M', 'M', 0x00, 0x2a}
)

// detectImageFormat identifies an image container by its magic number.
// Returns "" when the bytes match no known signature.
func detectImageFormat(b []byte) string {
	switch {
	case bytes.HasPrefix(b, pngMagic):
		return "png"
	case len(b) >= 3 && b[0] == 0xff && b[1] == 0xd8 && b[2] == 0xff:
		return "jpeg"
	case bytes.HasPrefix(b, gifMagic):
		return "gif"
	case len(b) >= 12 && bytes.HasPrefix(b, riffMagic) && bytes.Equal(b[8:12], webpMagic):
		return "webp"
	case len(b) >= 2 && b[0] == 'B' && b[1] == 'M':
		return "bmp"
	case bytes.HasPrefix(b, tiffLEMagic) || bytes.HasPrefix(b, tiffBEMagic):
		return "tiff"
	default:
		return ""
	}
}
