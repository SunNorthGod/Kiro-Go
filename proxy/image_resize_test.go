package proxy

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"
)

// buildTestImage renders a w x h gradient (with a varying alpha ramp so alpha
// handling is exercised) and returns it base64-encoded in the given container.
func buildTestImage(t *testing.T, w, h int, format string) string {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.NRGBA{
				R: uint8((x * 255) / max(1, w-1)),
				G: uint8((y * 255) / max(1, h-1)),
				B: 128,
				A: uint8(128 + (x*127)/max(1, w-1)),
			})
		}
	}

	var buf bytes.Buffer
	var err error
	switch format {
	case "jpeg":
		err = jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90})
	case "gif":
		err = gif.Encode(&buf, img, nil)
	default:
		err = png.Encode(&buf, img)
	}
	if err != nil {
		t.Fatalf("encode %s fixture: %v", format, err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// decodedDimensions reports the pixel size of a base64 payload.
func decodedDimensions(t *testing.T, data string) (int, int, string) {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		t.Fatalf("result is not valid standard base64: %v", err)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("result is not a decodable image: %v", err)
	}
	return cfg.Width, cfg.Height, format
}

// ---- the behaviour that matters most: untouched when it already fits ----

// TestShrinkLeavesFittingImagesByteIdentical is the guard against silently
// re-encoding (and degrading) every image that flows through the proxy. Only
// images that actually breach the upstream pixel ceiling may be touched.
func TestShrinkLeavesFittingImagesByteIdentical(t *testing.T) {
	for _, tc := range []struct{ w, h int; format string }{
		{64, 64, "png"},
		{8000, 120, "png"},   // exactly at the limit
		{120, 8000, "png"},   // exactly at the limit, other axis
		{300, 200, "jpeg"},
		{200, 150, "gif"},
	} {
		data := buildTestImage(t, tc.w, tc.h, tc.format)
		gotData, gotFormat := shrinkOversizedImage(data, tc.format)
		if gotData != data {
			t.Fatalf("%dx%d %s fits the limit but the bytes changed (%d -> %d)", tc.w, tc.h, tc.format, len(data), len(gotData))
		}
		if gotFormat != tc.format {
			t.Fatalf("%dx%d %s fits the limit but the format changed to %q", tc.w, tc.h, tc.format, gotFormat)
		}
	}
}

// ---- downscaling ----

func TestShrinkDownscalesOversizedImages(t *testing.T) {
	cases := []struct {
		name           string
		w, h           int
		format         string
		wantW, wantH   int
		wantFormat     string
	}{
		// 9000x200 -> width capped, height scaled by the same ratio (200*8000/9000).
		{"wide png", 9000, 200, "png", 8000, 177, "png"},
		// 200x9000 -> height capped.
		{"tall png", 200, 9000, "png", 177, 8000, "png"},
		{"wide jpeg", 9000, 300, "jpeg", 8000, 266, "jpeg"},
		// GIF is re-encoded as PNG on purpose (see encodeResizedImage).
		{"wide gif", 8600, 100, "gif", 8000, 93, "png"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := buildTestImage(t, tc.w, tc.h, tc.format)
			gotData, gotFormat := shrinkOversizedImage(data, tc.format)

			if gotData == data {
				t.Fatalf("%dx%d exceeds the %dpx limit but was left untouched", tc.w, tc.h, maxUpstreamImageDimension)
			}
			if gotFormat != tc.wantFormat {
				t.Fatalf("format: got %q, want %q", gotFormat, tc.wantFormat)
			}

			w, h, decodedFormat := decodedDimensions(t, gotData)
			if w > maxUpstreamImageDimension || h > maxUpstreamImageDimension {
				t.Fatalf("result %dx%d still breaches the %dpx limit", w, h, maxUpstreamImageDimension)
			}
			if w != tc.wantW || h != tc.wantH {
				t.Fatalf("dimensions: got %dx%d, want %dx%d", w, h, tc.wantW, tc.wantH)
			}
			// The declared format must describe the actual bytes, or the upstream
			// answers IMAGE_MIME_MISMATCH.
			if decodedFormat != gotFormat {
				t.Fatalf("returned format %q does not match the encoded container %q", gotFormat, decodedFormat)
			}
			if sniffed := sniffBase64ImageFormat(gotData); sniffed != gotFormat {
				t.Fatalf("magic-number sniff says %q but we declared %q", sniffed, gotFormat)
			}
		})
	}
}

// TestShrinkPreservesAspectRatio checks the ratio survives across a range of
// shapes, allowing one pixel of integer-division slack.
func TestShrinkPreservesAspectRatio(t *testing.T) {
	// All fixtures stay under maxResizeSourcePixels; anything above it is
	// deliberately NOT resized (see TestShrinkSkipsImagesBeyondTheDecodeBudget).
	for _, tc := range []struct{ w, h int }{
		{9000, 100}, {12000, 900}, {9000, 2200}, {500, 10000}, {8001, 1},
	} {
		data := buildTestImage(t, tc.w, tc.h, "png")
		gotData, _ := shrinkOversizedImage(data, "png")
		w, h, _ := decodedDimensions(t, gotData)

		if w > maxUpstreamImageDimension || h > maxUpstreamImageDimension {
			t.Fatalf("%dx%d -> %dx%d still over the limit", tc.w, tc.h, w, h)
		}
		srcRatio := float64(tc.w) / float64(tc.h)
		dstRatio := float64(w) / float64(h)
		// One pixel of slack on the short side can move the ratio noticeably when
		// that side is tiny, so compare with a tolerance scaled to that pixel.
		tolerance := srcRatio / float64(min(w, h))
		if diff := srcRatio - dstRatio; diff > tolerance || diff < -tolerance {
			t.Fatalf("%dx%d -> %dx%d: aspect ratio drifted %.4f vs %.4f (tolerance %.4f)",
				tc.w, tc.h, w, h, dstRatio, srcRatio, tolerance)
		}
	}
}

// TestShrinkPreservesAlpha confirms transparency survives the resample: the
// fixture ramps alpha from 128 to 255, so the result must be neither fully
// opaque nor fully transparent.
func TestShrinkPreservesAlpha(t *testing.T) {
	data := buildTestImage(t, 9000, 200, "png")
	gotData, gotFormat := shrinkOversizedImage(data, "png")
	if gotFormat != "png" {
		t.Fatalf("expected png, got %q", gotFormat)
	}
	raw, err := base64.StdEncoding.DecodeString(gotData)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("png decode: %v", err)
	}
	b := img.Bounds()
	sawPartial := false
	for x := b.Min.X; x < b.Max.X && !sawPartial; x += 7 {
		_, _, _, a := img.At(x, b.Min.Y+b.Dy()/2).RGBA()
		if a > 0 && a < 0xffff {
			sawPartial = true
		}
	}
	if !sawPartial {
		t.Fatal("alpha channel was lost during downscaling")
	}
}

// ---- refusing to act, safely ----

// TestShrinkLeavesUndecodableInputUntouched covers the inputs that must never
// panic and must never be rewritten: corrupt payloads, non-image data, and webp
// (accepted by the upstream but undecodable with the standard library).
func TestShrinkLeavesUndecodableInputUntouched(t *testing.T) {
	// A minimal RIFF/WEBP header — enough for our sniffer, useless to image.Decode.
	webp := base64.StdEncoding.EncodeToString([]byte("RIFF\x24\x00\x00\x00WEBPVP8 abcdefgh"))

	cases := []struct{ name, data, format string }{
		{"empty", "", "png"},
		{"not base64", "!!!! not base64 !!!!", "png"},
		{"base64 but not an image", base64.StdEncoding.EncodeToString([]byte("hello world, definitely not a picture")), "png"},
		{"truncated png", buildTestImage(t, 40, 40, "png")[:20], "png"},
		{"webp", webp, "webp"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotData, gotFormat := shrinkOversizedImage(tc.data, tc.format)
			if gotData != tc.data {
				t.Fatalf("undecodable input was rewritten (%d -> %d bytes)", len(tc.data), len(gotData))
			}
			if gotFormat != tc.format {
				t.Fatalf("undecodable input had its format changed to %q", gotFormat)
			}
		})
	}
}

// TestShrinkRespectsPixelCap exercises the OOM guard: an oversized image beyond
// the decode budget must be forwarded untouched rather than decoded.
func TestShrinkRespectsPixelCap(t *testing.T) {
	data := buildTestImage(t, 9000, 200, "png") // 1.8M pixels

	original := maxResizeSourcePixels
	maxResizeSourcePixels = 100_000 // below the fixture, so the guard must trip
	defer func() { maxResizeSourcePixels = original }()

	gotData, gotFormat := shrinkOversizedImage(data, "png")
	if gotData != data || gotFormat != "png" {
		t.Fatal("an image beyond the decode budget must be forwarded untouched")
	}
}

// TestShrinkSkipsImagesBeyondTheDecodeBudget documents the deliberate limit of
// this feature on the production host (1.9 GiB RAM, no swap): an oversized image
// whose pixel count exceeds the decode budget is NOT rescued, because decoding it
// would cost more memory than the box can spare. It must be passed through
// unchanged — the upstream then rejects it, which is a visible failure rather
// than an OOM that takes down every other request.
//
// 8500x4000 is 34M pixels, above the 20M budget, and is the shape that makes this
// tradeoff concrete: a large screenshot or photo that we knowingly do not fix.
func TestShrinkSkipsImagesBeyondTheDecodeBudget(t *testing.T) {
	if maxResizeSourcePixels >= 34_000_000 {
		t.Skip("decode budget was raised; this fixture no longer exercises the guard")
	}
	data := buildTestImage(t, 8500, 4000, "png")
	gotData, gotFormat := shrinkOversizedImage(data, "png")
	if gotData != data || gotFormat != "png" {
		t.Fatal("an image above the decode budget must be forwarded untouched, not resized")
	}
}

// TestShrinkSerializesConcurrentResizes checks the memory bound actually holds
// under concurrency: the decode+resample section is guarded by a depth-1
// semaphore, so N callers must not run it simultaneously (N x 144 MiB would OOM
// the host). Every caller must still get a correct, in-limit result.
func TestShrinkSerializesConcurrentResizes(t *testing.T) {
	data := buildTestImage(t, 9000, 150, "png")

	const callers = 6
	results := make(chan string, callers)
	for i := 0; i < callers; i++ {
		go func() {
			got, _ := shrinkOversizedImage(data, "png")
			results <- got
		}()
	}

	for i := 0; i < callers; i++ {
		got := <-results
		if got == data {
			t.Fatal("concurrent caller got an unresized image")
		}
		w, h, _ := decodedDimensions(t, got)
		if w > maxUpstreamImageDimension || h > maxUpstreamImageDimension {
			t.Fatalf("concurrent caller got %dx%d, over the limit", w, h)
		}
	}
}

// ---- helpers ----

func TestFitWithinDimension(t *testing.T) {
	cases := []struct{ w, h, maxDim, wantW, wantH int }{
		{9000, 200, 8000, 8000, 177},
		{200, 9000, 8000, 177, 8000},
		{10000, 12000, 8000, 6666, 8000}, // both sides over: ratio math still holds
		{12000, 900, 8000, 8000, 600},
		{8000, 8000, 8000, 8000, 8000},
		{100000, 1, 8000, 8000, 1}, // short side clamped to at least 1 pixel
	}
	for _, tc := range cases {
		w, h := fitWithinDimension(tc.w, tc.h, tc.maxDim)
		if w != tc.wantW || h != tc.wantH {
			t.Fatalf("fitWithinDimension(%d,%d,%d) = %dx%d, want %dx%d", tc.w, tc.h, tc.maxDim, w, h, tc.wantW, tc.wantH)
		}
		if w > tc.maxDim || h > tc.maxDim {
			t.Fatalf("fitWithinDimension(%d,%d,%d) returned %dx%d, above the cap", tc.w, tc.h, tc.maxDim, w, h)
		}
	}
}

// TestBase64ImageCodecHandlesEveryVariant covers the four alphabet/padding
// combinations plus line-wrapped payloads, since clients send all of them.
func TestBase64ImageCodecHandlesEveryVariant(t *testing.T) {
	payload := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0xfb, 0xff, 0x3e, 0x3f, 0x01}

	variants := map[string]string{
		"std":     base64.StdEncoding.EncodeToString(payload),
		"rawStd":  base64.RawStdEncoding.EncodeToString(payload),
		"url":     base64.URLEncoding.EncodeToString(payload),
		"rawURL":  base64.RawURLEncoding.EncodeToString(payload),
		"wrapped": insertEveryN(base64.StdEncoding.EncodeToString(payload), 4, "\n"),
		"spaced":  insertEveryN(base64.StdEncoding.EncodeToString(payload), 4, " "),
	}

	for name, encoded := range variants {
		normalized, enc := base64ImageCodec(encoded)
		got, err := decodeBase64Image(normalized, enc)
		if err != nil {
			t.Fatalf("%s: decode failed: %v", name, err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("%s: round-trip mismatch: got % x, want % x", name, got, payload)
		}
	}
}

func insertEveryN(s string, n int, sep string) string {
	var b strings.Builder
	for i, r := range s {
		if i > 0 && i%n == 0 {
			b.WriteString(sep)
		}
		b.WriteRune(r)
	}
	return b.String()
}

// ---- end-to-end through the single KiroImage construction point ----

// TestParseBase64ImageDownscalesOversized proves the resize is actually wired
// into parseBase64Image, which every image path (Claude content, tool results,
// OpenAI content) funnels through.
func TestParseBase64ImageDownscalesOversized(t *testing.T) {
	data := buildTestImage(t, 9000, 250, "png")

	img := parseBase64Image(data, "png")
	if img == nil {
		t.Fatal("parseBase64Image returned nil for a valid oversized png")
	}
	w, h, _ := decodedDimensions(t, img.Source.Bytes)
	if w > maxUpstreamImageDimension || h > maxUpstreamImageDimension {
		t.Fatalf("parseBase64Image emitted %dx%d, still over the %dpx limit", w, h, maxUpstreamImageDimension)
	}
	if img.Format != "png" {
		t.Fatalf("format: got %q, want png", img.Format)
	}

	// And a fitting image must pass through byte-identically.
	small := buildTestImage(t, 100, 100, "png")
	if got := parseBase64Image(small, "png"); got == nil || got.Source.Bytes != small {
		t.Fatal("a fitting image must reach the payload unmodified")
	}
}

// TestParseBase64ImageOversizedGifBecomesPng pins the format/bytes agreement for
// the container-changing case: a mislabelled declaration would earn an
// IMAGE_MIME_MISMATCH from the upstream.
func TestParseBase64ImageOversizedGifBecomesPng(t *testing.T) {
	data := buildTestImage(t, 8500, 80, "gif")

	img := parseBase64Image(data, "gif")
	if img == nil {
		t.Fatal("parseBase64Image returned nil for a valid oversized gif")
	}
	if img.Format != "png" {
		t.Fatalf("an oversized gif must be re-encoded as png, got format %q", img.Format)
	}
	if sniffed := sniffBase64ImageFormat(img.Source.Bytes); sniffed != "png" {
		t.Fatalf("declared png but the bytes sniff as %q", sniffed)
	}
	w, h, _ := decodedDimensions(t, img.Source.Bytes)
	if w > maxUpstreamImageDimension || h > maxUpstreamImageDimension {
		t.Fatalf("result %dx%d still over the limit", w, h)
	}
}

// TestResizeLimitsAreEnvConfigurable pins that the two memory-bounding knobs come
// from the environment, so ONE binary can serve hosts with very different memory
// budgets (the 1.9 GiB shared box wants 20M pixels / 1 slot; the 7.8 GiB dedicated
// box wants 70M / 2). Hardcoding per-host values would mean two builds and a
// rollback that silently changes behaviour.
func TestResizeLimitsAreEnvConfigurable(t *testing.T) {
	cases := []struct {
		raw  string
		want int64
	}{
		{"70000000", 70_000_000},
		{"  70000000  ", 70_000_000}, // whitespace tolerated
		{"", 20_000_000},             // unset -> conservative default
		{"0", 20_000_000},            // non-positive rejected
		{"-5", 20_000_000},
		{"abc", 20_000_000}, // garbage rejected, not silently zero
	}
	for _, tc := range cases {
		t.Setenv("KIRO_MAX_RESIZE_PIXELS", tc.raw)
		if got := envInt64("KIRO_MAX_RESIZE_PIXELS", 20_000_000); got != tc.want {
			t.Fatalf("KIRO_MAX_RESIZE_PIXELS=%q -> %d, want %d", tc.raw, got, tc.want)
		}
	}

	t.Setenv("KIRO_RESIZE_SLOTS", "2")
	if got := envInt("KIRO_RESIZE_SLOTS", 1); got != 2 {
		t.Fatalf("KIRO_RESIZE_SLOTS=2 -> %d, want 2", got)
	}
	t.Setenv("KIRO_RESIZE_SLOTS", "nonsense")
	if got := envInt("KIRO_RESIZE_SLOTS", 1); got != 1 {
		t.Fatalf("invalid KIRO_RESIZE_SLOTS must fall back to 1, got %d", got)
	}
}

// TestRaisedPixelCapRescuesSquareOversizedImages is the behavioural payoff of the
// bigger box: with the default 20M budget an image over 8000px on BOTH sides can
// never be decoded safely, so it is forwarded as-is. Raising the cap makes exactly
// that case fixable. Uses a modest fixture and a lowered cap so the assertion is
// about the DECISION, not about allocating a real 64M-pixel buffer in CI.
func TestRaisedPixelCapRescuesSquareOversizedImages(t *testing.T) {
	// 8200x2600 = 21.3M pixels: above the 20M default, below a raised budget.
	data := buildTestImage(t, 8200, 2600, "png")

	original := maxResizeSourcePixels
	defer func() { maxResizeSourcePixels = original }()

	maxResizeSourcePixels = 20_000_000
	if got, _ := shrinkOversizedImage(data, "png"); got != data {
		t.Fatal("with the 20M budget this image must be forwarded untouched")
	}

	maxResizeSourcePixels = 70_000_000
	got, format := shrinkOversizedImage(data, "png")
	if got == data {
		t.Fatal("with a 70M budget the same image must be downscaled")
	}
	w, h, _ := decodedDimensions(t, got)
	if w > maxUpstreamImageDimension || h > maxUpstreamImageDimension {
		t.Fatalf("downscaled to %dx%d, still over the %dpx limit", w, h, maxUpstreamImageDimension)
	}
	if format != "png" {
		t.Fatalf("format changed to %q", format)
	}
}
