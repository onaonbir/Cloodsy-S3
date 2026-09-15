// Package image provides pure-Go, on-the-fly image transformation (resize /
// re-encode) for objects served by the S3 API. It has no knowledge of HTTP,
// the database or storage — callers feed it a reader plus parameters and get
// encoded bytes back. Everything here is CGO-free so the single-binary,
// zero-dependency deployment story is preserved.
package image

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"net/url"
	"strconv"
	"strings"

	"github.com/disintegration/imaging"

	// Register decoders for the formats we accept. JPEG/PNG come in via the
	// encoders imported above; GIF and WebP need explicit blank imports for
	// their decoder registration. WebP is decode-only (no pure-Go encoder).
	_ "golang.org/x/image/webp"
	_ "image/gif"
)

// Mode controls how the source image is fitted into the requested box.
type Mode string

const (
	// ModeFit scales proportionally so the image fits within w×h (default).
	ModeFit Mode = "f"
	// ModeFill scales + center-crops to cover exactly w×h (cover/crop).
	ModeFill Mode = "c"
	// ModeExact stretches to exactly w×h, ignoring aspect ratio.
	ModeExact Mode = "e"
)

// Limits guard against resource-exhaustion via crafted parameters or inputs.
const (
	MaxDimension    = 5000             // clamp for w/h
	MaxDecodePixels = 50 * 1000 * 1000 // ~50 MP source guard (checked before decode)
	DefaultQuality  = 75
	// DefaultMaxSourceBytes bounds how much of a source object is read.
	DefaultMaxSourceBytes = 32 << 20
)

// ErrSourceTooLarge is returned when the source exceeds the byte or pixel limits.
var ErrSourceTooLarge = errors.New("source image too large")

// Params is a parsed, validated transform request.
type Params struct {
	Width   int  // 0 = unset
	Height  int  // 0 = unset
	Mode    Mode // defaults to ModeFit
	Quality int  // 1..100
}

// ParseParams reads the resize query parameters (w, h, m, q) from a URL query.
// The second return value is true when an actual transform is requested, i.e.
// at least one of width or height is present. A bare ?q= without dimensions is
// treated as an "optimize" request (re-encode at the given quality) and also
// returns true so callers can honor it.
func ParseParams(q url.Values) (Params, bool) {
	p := Params{Mode: ModeFit, Quality: DefaultQuality}
	hasW := q.Has("w")
	hasH := q.Has("h")
	hasQ := q.Has("q")
	if !hasW && !hasH && !hasQ {
		return p, false
	}

	if v, err := strconv.Atoi(q.Get("w")); err == nil && v > 0 {
		p.Width = clamp(v, 0, MaxDimension)
	}
	if v, err := strconv.Atoi(q.Get("h")); err == nil && v > 0 {
		p.Height = clamp(v, 0, MaxDimension)
	}
	switch Mode(strings.ToLower(q.Get("m"))) {
	case ModeFill:
		p.Mode = ModeFill
	case ModeExact:
		p.Mode = ModeExact
	default:
		p.Mode = ModeFit
	}
	if v, err := strconv.Atoi(q.Get("q")); err == nil && v > 0 {
		p.Quality = clamp(v, 1, 100)
	}

	if p.Width == 0 && p.Height == 0 && !hasQ {
		return p, false
	}
	return p, true
}

// Spec returns a stable, canonical string describing this transform. It is used
// as the variant cache discriminator so identical requests share a cached file
// and the upload optimizer and the on-access resizer agree on keys.
func (p Params) Spec() string {
	return fmt.Sprintf("w%dh%dm%sq%d", p.Width, p.Height, p.Mode, p.Quality)
}

// IsImageContentType reports whether ct is an image format we can decode.
func IsImageContentType(ct string) bool {
	switch strings.ToLower(strings.TrimSpace(strings.Split(ct, ";")[0])) {
	case "image/jpeg", "image/jpg", "image/png", "image/gif", "image/webp":
		return true
	}
	return false
}

// sniffFormat identifies the container from magic bytes; the declared
// content-type is not trusted for decoder selection.
func sniffFormat(b []byte) string {
	switch {
	case len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return "jpeg"
	case len(b) >= 8 && string(b[:8]) == "\x89PNG\r\n\x1a\n":
		return "png"
	case len(b) >= 6 && (string(b[:6]) == "GIF87a" || string(b[:6]) == "GIF89a"):
		return "gif"
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "webp"
	}
	return ""
}

// Transform decodes src, applies the resize/encode described by p, and returns
// the encoded bytes plus the resulting content-type. The source is read at
// most up to DefaultMaxSourceBytes (callers may pass a tighter LimitReader);
// dimensions are checked before the full decode; JPEG EXIF orientation is
// applied so rotated phone photos come out upright. Because there is no
// pure-Go WebP encoder, WebP (and GIF) inputs are transcoded to PNG.
func Transform(src io.Reader, srcContentType string, p Params) ([]byte, string, error) {
	raw, err := io.ReadAll(io.LimitReader(src, DefaultMaxSourceBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("read source: %w", err)
	}
	if len(raw) > DefaultMaxSourceBytes {
		return nil, "", ErrSourceTooLarge
	}
	format := sniffFormat(raw)
	if format == "" {
		return nil, "", fmt.Errorf("unsupported image format")
	}

	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, "", fmt.Errorf("decode config: %w", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > MaxDecodePixels {
		return nil, "", fmt.Errorf("%w: %dx%d", ErrSourceTooLarge, cfg.Width, cfg.Height)
	}

	var img image.Image
	if format == "jpeg" {
		img, err = imaging.Decode(bytes.NewReader(raw), imaging.AutoOrientation(true))
	} else {
		img, _, err = image.Decode(bytes.NewReader(raw))
	}
	if err != nil {
		return nil, "", fmt.Errorf("decode: %w", err)
	}

	img = resize(img, p)
	return encode(img, format, p.Quality)
}

// resize applies the requested mode. If neither dimension is set the image is
// returned unchanged (quality-only / optimize path).
func resize(img image.Image, p Params) image.Image {
	if p.Width == 0 && p.Height == 0 {
		return img
	}
	b := img.Bounds()
	switch p.Mode {
	case ModeFill:
		w, h := p.Width, p.Height
		if w == 0 {
			w = b.Dx()
		}
		if h == 0 {
			h = b.Dy()
		}
		return imaging.Fill(img, w, h, imaging.Center, imaging.Lanczos)
	case ModeExact:
		w, h := p.Width, p.Height
		if w == 0 {
			w = b.Dx()
		}
		if h == 0 {
			h = b.Dy()
		}
		return imaging.Resize(img, w, h, imaging.Lanczos)
	default: // ModeFit — proportional; never upscale beyond the source.
		w, h := p.Width, p.Height
		if w == 0 || w > b.Dx() {
			w = b.Dx()
		}
		if h == 0 || h > b.Dy() {
			h = b.Dy()
		}
		if w >= b.Dx() && h >= b.Dy() {
			return img
		}
		return imaging.Fit(img, w, h, imaging.Lanczos)
	}
}

// encode writes the image out, choosing an encoder based on the source format.
// PNG inputs stay PNG (lossless; quality is ignored). GIF and WebP inputs are
// transcoded to PNG so transparency survives; everything else encodes to JPEG
// at the requested quality.
func encode(img image.Image, srcFormat string, quality int) ([]byte, string, error) {
	if quality <= 0 || quality > 100 {
		quality = DefaultQuality
	}
	var buf bytes.Buffer
	switch srcFormat {
	case "png", "gif", "webp":
		if err := png.Encode(&buf, img); err != nil {
			return nil, "", fmt.Errorf("encode %s->png: %w", srcFormat, err)
		}
		return buf.Bytes(), "image/png", nil
	default: // jpeg or anything else → JPEG out
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
			return nil, "", fmt.Errorf("encode jpeg: %w", err)
		}
		return buf.Bytes(), "image/jpeg", nil
	}
}

// VariantContentType mirrors encode's choice for a stored source content-type.
func VariantContentType(srcCT string) string {
	switch strings.ToLower(strings.TrimSpace(strings.Split(srcCT, ";")[0])) {
	case "image/png", "image/gif", "image/webp":
		return "image/png"
	default:
		return "image/jpeg"
	}
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
