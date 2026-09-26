package proxy

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/jpeg"
	"image/png"
	"os"
	"strconv"
	"strings"
	"time"

	// Registers the GIF decoder with image.Decode / image.DecodeConfig. GIF is in
	// the upstream's accepted set, so an oversized GIF must still be measurable
	// and decodable here; it is re-encoded as PNG (see encodeResizedImage).
	_ "image/gif"

	"kiro-go/logger"
)

// image_resize.go downscales images that exceed the upstream's pixel ceiling.
//
// Kiro/AmazonQ rejects any image whose width OR height is above 8000 pixels:
//
//	HTTP 400 {"message":"messages.N.content.M.image.source.base64.data: At least
//	one of the image dimensions exceed max allowed size: 8000 pixels",
//	"reason":"IMAGE_DIMENSION_EXCEEDED"}
//
// The limit is on DIMENSIONS, not on byte size — a 12000x900 long screenshot is
// only a few hundred KB yet is refused outright. The proxy used to forward image
// bytes untouched, so the user simply saw the request fail (and, before the
// companion fix in account_failover.go, the same doomed request was retried
// across all three endpoints and every account in the pool).
//
// Downscaling here fixes the user-visible failure and, as a side effect, shrinks
// the payload — which also relieves the maxPayloadBytes budget that
// truncatePayloadToLimit cannot otherwise reclaim from images.

// maxUpstreamImageDimension is the upstream's per-side pixel ceiling.
const maxUpstreamImageDimension = 8000

// maxResizeSourcePixels caps the pixel budget we are willing to decode, and
// resizeSlots caps how many resizes may run at once. Together they bound the
// memory this feature can ever hold: peak ≈ pixels × 7.2 bytes × slots (4 bytes
// for the decoded source plus ~4 for the smaller destination RGBA buffer).
//
// Both are host-dependent, so both are read from the environment with a
// conservative default sized for the SMALLER of the two production boxes. That
// keeps one binary valid everywhere — the alternative (hardcoding per-host values)
// would mean two different builds and a rollback that silently changes behaviour.
//
//	KIRO_MAX_RESIZE_PIXELS   default 20000000
//	KIRO_RESIZE_SLOTS        default 1
//
// Reference points, measured:
//
//	1.9 GiB box (6 containers, no swap, ~830 MiB free): 20M × 1 slot ≈ 144 MiB
//	7.8 GiB box (dedicated, no swap):                   70M × 2 slots ≈ 1.0 GiB
//
// A picture beyond the pixel cap is forwarded untouched (and logged) rather than
// risking every other request on the box.
//
// Note the consequence of a low cap: an image with BOTH sides above 8000 is at
// least 8001x8001 = 64M pixels, so it is only rescuable once the cap is raised
// past that — at 20M it is always forwarded as-is, at 70M it is fixed.
var maxResizeSourcePixels = envInt64("KIRO_MAX_RESIZE_PIXELS", 20_000_000)

// resizeSlots serializes the decode+resample step. Without it, N concurrent
// oversized images would each claim their own buffer and trivially OOM a small
// box. Oversized images are rare in practice (18 across all of 2026-07-26), so
// even depth 1 is never actually contended; the point is that the memory bound
// becomes a guarantee instead of a hope.
var resizeSlots = make(chan struct{}, envInt("KIRO_RESIZE_SLOTS", 1))

// envInt64 reads a positive int64 from the environment, falling back to def on
// absence, garbage or a non-positive value. Mirrors the style already used for
// KIRO_STREAM_IDLE_TIMEOUT_SECONDS / KIRO_SSE_KEEPALIVE_SECONDS.
func envInt64(name string, def int64) int64 {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
		logger.Warnf("[Image] ignoring invalid %s=%q, using %d", name, v, def)
	}
	return def
}

func envInt(name string, def int) int {
	return int(envInt64(name, int64(def)))
}

// resizeSlotWait bounds how long a request will wait for the resize slot. On
// timeout the image is forwarded untouched (the upstream will reject it) rather
// than holding the caller indefinitely: a slow answer for one image must not turn
// into unbounded latency for a queue of them.
const resizeSlotWait = 5 * time.Second

// jpegRecodeQuality is used when re-encoding a downscaled JPEG. 88 is visually
// near-transparent while keeping the payload well under the original.
const jpegRecodeQuality = 88

// shrinkOversizedImage returns the image to actually send upstream, downscaled to
// fit maxUpstreamImageDimension when necessary.
//
// It returns the ORIGINAL data and format byte-for-byte whenever no resize is
// needed or possible — no re-encode, no quality loss, no size change. That is the
// overwhelmingly common path, so the dimension check reads only the file header
// (image.DecodeConfig over a streaming base64 decoder) and never decodes pixels.
//
// The returned format always matches the returned bytes: a downscaled GIF comes
// back as PNG (see below), and callers must persist the returned format, or the
// upstream's declared-vs-actual check would fail with IMAGE_MIME_MISMATCH.
func shrinkOversizedImage(data, format string) (string, string) {
	if data == "" {
		return data, format
	}

	normalized, enc := base64ImageCodec(data)

	cfg, decodedFormat, err := image.DecodeConfig(base64.NewDecoder(enc, strings.NewReader(normalized)))
	if err != nil {
		// webp is in the upstream's allow-list but has no standard-library
		// decoder, so it can never be inspected here; that is a known limitation
		// rather than bad data. Anything else failing to parse means the payload
		// is corrupt or an unsupported container.
		if format == "webp" {
			logger.Debugf("[Image] webp cannot be measured with the standard library; if it exceeds %dpx the upstream will reject it", maxUpstreamImageDimension)
		} else {
			logger.Debugf("[Image] could not read dimensions (format=%q): %v", format, err)
		}
		return data, format
	}

	if cfg.Width <= maxUpstreamImageDimension && cfg.Height <= maxUpstreamImageDimension {
		return data, format // fits — leave the bytes exactly as the client sent them
	}

	if pixels := int64(cfg.Width) * int64(cfg.Height); pixels > maxResizeSourcePixels {
		logger.Warnf("[Image] %dx%d exceeds the %dpx upstream limit but is too large to downscale safely (%d pixels > %d cap); forwarding as-is, upstream will reject it",
			cfg.Width, cfg.Height, maxUpstreamImageDimension, pixels, maxResizeSourcePixels)
		return data, format
	}

	// Hold the single resize slot across decode + resample, the only memory-heavy
	// part. Released before base64/encode bookkeeping below is irrelevant — the
	// big buffers are alive until this function returns, so keep it for the whole
	// body via defer.
	select {
	case resizeSlots <- struct{}{}:
		defer func() { <-resizeSlots }()
	case <-time.After(resizeSlotWait):
		logger.Warnf("[Image] timed out waiting %s for a resize slot; forwarding %dx%d untouched (upstream will reject it)",
			resizeSlotWait, cfg.Width, cfg.Height)
		return data, format
	}

	raw, decodeErr := decodeBase64Image(normalized, enc)
	if decodeErr != nil {
		logger.Warnf("[Image] base64 decode failed while downscaling a %dx%d image: %v", cfg.Width, cfg.Height, decodeErr)
		return data, format
	}

	src, _, imgErr := image.Decode(bytes.NewReader(raw))
	if imgErr != nil {
		logger.Warnf("[Image] decode failed while downscaling a %dx%d %s image: %v", cfg.Width, cfg.Height, decodedFormat, imgErr)
		return data, format
	}

	dstW, dstH := fitWithinDimension(cfg.Width, cfg.Height, maxUpstreamImageDimension)
	resized := downscaleImage(src, dstW, dstH)

	encoded, outFormat, encErr := encodeResizedImage(resized, format)
	if encErr != nil {
		logger.Warnf("[Image] re-encode failed while downscaling a %dx%d %s image: %v", cfg.Width, cfg.Height, format, encErr)
		return data, format
	}

	out := base64.StdEncoding.EncodeToString(encoded)
	logger.Infof("[Image] downscaled %dx%d -> %dx%d (%s -> %s, base64 %d -> %d bytes) to satisfy the %dpx upstream limit",
		cfg.Width, cfg.Height, dstW, dstH, format, outFormat, len(data), len(out), maxUpstreamImageDimension)
	if len(out) > len(data) {
		// Possible when a heavily-compressed paletted source is re-encoded as
		// full-colour PNG. Still required (the dimension limit is hard), but worth
		// surfacing because it pushes against the payload budget.
		logger.Warnf("[Image] downscaled payload grew from %d to %d base64 bytes (%s -> %s)", len(data), len(out), format, outFormat)
	}
	return out, outFormat
}

// fitWithinDimension scales w x h down so both sides are <= maxDim, preserving
// the aspect ratio. Integer division floors, so neither side can land above the
// cap; both are clamped to at least 1 pixel.
func fitWithinDimension(w, h, maxDim int) (int, int) {
	dstW, dstH := w, h
	if w >= h {
		dstW = maxDim
		dstH = h * maxDim / w
	} else {
		dstH = maxDim
		dstW = w * maxDim / h
	}
	if dstW < 1 {
		dstW = 1
	}
	if dstH < 1 {
		dstH = 1
	}
	return dstW, dstH
}

// downscaleImage resamples src to dstW x dstH with a box (area-average) filter.
//
// A box filter is the right choice here: this path only ever SHRINKS, often by a
// large factor, where averaging every source pixel that maps into a destination
// pixel both avoids aliasing and beats nearest-neighbour without needing a
// third-party resampler (the project deliberately carries almost no
// dependencies). Accumulation happens on the alpha-premultiplied values returned
// by RGBA(), which is the correct space to average in.
func downscaleImage(src image.Image, dstW, dstH int) *image.RGBA {
	b := src.Bounds()
	srcW, srcH := b.Dx(), b.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	if srcW <= 0 || srcH <= 0 {
		return dst
	}

	for dy := 0; dy < dstH; dy++ {
		y0 := b.Min.Y + dy*srcH/dstH
		y1 := b.Min.Y + (dy+1)*srcH/dstH
		if y1 <= y0 {
			y1 = y0 + 1
		}
		if y1 > b.Max.Y {
			y1 = b.Max.Y
		}
		for dx := 0; dx < dstW; dx++ {
			x0 := b.Min.X + dx*srcW/dstW
			x1 := b.Min.X + (dx+1)*srcW/dstW
			if x1 <= x0 {
				x1 = x0 + 1
			}
			if x1 > b.Max.X {
				x1 = b.Max.X
			}

			var sr, sg, sb, sa uint64
			var n uint64
			for y := y0; y < y1; y++ {
				for x := x0; x < x1; x++ {
					r, g, bl, a := src.At(x, y).RGBA() // premultiplied, 0..65535
					sr += uint64(r)
					sg += uint64(g)
					sb += uint64(bl)
					sa += uint64(a)
					n++
				}
			}
			if n == 0 {
				continue
			}
			i := dst.PixOffset(dx, dy)
			dst.Pix[i+0] = uint8(sr / n >> 8)
			dst.Pix[i+1] = uint8(sg / n >> 8)
			dst.Pix[i+2] = uint8(sb / n >> 8)
			dst.Pix[i+3] = uint8(sa / n >> 8)
		}
	}
	return dst
}

// encodeResizedImage re-encodes a downscaled image, returning the bytes and the
// format label that now describes them.
//
// GIF comes back as PNG on purpose: image/gif can only give us the first frame,
// so animation is already lost by the time we get here, and PNG re-encodes that
// frame losslessly instead of putting it through GIF's 256-colour quantisation.
// PNG is in the upstream's accepted set, and the caller stores the returned
// format so the declared media type still matches the bytes.
func encodeResizedImage(img image.Image, format string) ([]byte, string, error) {
	var buf bytes.Buffer
	switch format {
	case "jpeg":
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: jpegRecodeQuality}); err != nil {
			return nil, format, err
		}
		return buf.Bytes(), "jpeg", nil
	case "gif":
		if err := png.Encode(&buf, img); err != nil {
			return nil, format, err
		}
		return buf.Bytes(), "png", nil
	default: // png, and anything we managed to decode but cannot name
		if err := png.Encode(&buf, img); err != nil {
			return nil, format, err
		}
		return buf.Bytes(), "png", nil
	}
}

// base64ImageCodec resolves which base64 variant the client used and returns a
// whitespace-free payload plus the matching encoding.
//
// Clients send all four combinations of alphabet (standard / URL-safe) and
// padding (present / absent), and line-wrapped payloads are common. Resolving it
// once lets both the cheap header read and the full decode share the answer.
// The whitespace scan is done with IndexAny first so the common case (unwrapped
// standard base64) costs one pass and zero allocations instead of copying a
// megabyte per request.
func base64ImageCodec(data string) (string, *base64.Encoding) {
	s := data
	if strings.IndexAny(s, " \t\r\n") >= 0 {
		var b strings.Builder
		b.Grow(len(s))
		for i := 0; i < len(s); i++ {
			switch c := s[i]; c {
			case ' ', '\t', '\r', '\n':
			default:
				b.WriteByte(c)
			}
		}
		s = b.String()
	}

	urlSafe := strings.IndexAny(s, "-_") >= 0
	padded := len(s)%4 == 0
	switch {
	case urlSafe && padded:
		return s, base64.URLEncoding
	case urlSafe:
		return s, base64.RawURLEncoding
	case padded:
		return s, base64.StdEncoding
	default:
		return s, base64.RawStdEncoding
	}
}

// decodeBase64Image fully decodes a payload that base64ImageCodec has normalized.
func decodeBase64Image(normalized string, enc *base64.Encoding) ([]byte, error) {
	return enc.DecodeString(normalized)
}

