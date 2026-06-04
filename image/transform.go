// Package image provides pure-Go, on-the-fly image transformation (resize /
// re-encode) for objects served by the S3 API. It has no knowledge of HTTP,
// the database or storage — callers feed it a reader plus parameters and get
// encoded bytes back. Everything here is CGO-free so the single-binary,
// zero-dependency deployment story is preserved.
package image

import (
	"bytes"
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
)

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

	// Only a meaningful transform if we have at least one dimension OR an
	// explicit quality (re-encode). Width/height==0 with no q means no-op.
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

// Transform decodes src, applies the resize/encode described by p, and returns
// the encoded bytes plus the resulting content-type. srcContentType selects the
// output encoder. Because there is no pure-Go WebP encoder, WebP (and GIF)
// inputs are transcoded to JPEG (or PNG when alpha must be preserved).
func Transform(src io.Reader, srcContentType string, p Params) ([]byte, string, error) {
	// Read fully so we can inspect config before committing to a full decode.
	raw, err := io.ReadAll(src)
	if err != nil {
		return nil, "", fmt.Errorf("read source: %w", err)
	}

	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, "", fmt.Errorf("decode config: %w", err)
	}
	if int64(cfg.Width)*int64(cfg.Height) > MaxDecodePixels {
		return nil, "", fmt.Errorf("source image too large: %dx%d", cfg.Width, cfg.Height)
	}

	img, format, err := image.Decode(bytes.NewReader(raw))
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
	switch p.Mode {
	case ModeFill:
		// Fill needs both dimensions; fall back to the missing one from source.
		w, h := p.Width, p.Height
		b := img.Bounds()
		if w == 0 {
			w = b.Dx()
		}
		if h == 0 {
			h = b.Dy()
		}
		return imaging.Fill(img, w, h, imaging.Center, imaging.Lanczos)
	case ModeExact:
		// Resize with a zero dimension preserves aspect ratio in imaging, which
		// is not "exact"; supply source size for the unset axis to truly stretch.
		w, h := p.Width, p.Height
		b := img.Bounds()
		if w == 0 {
			w = b.Dx()
		}
		if h == 0 {
			h = b.Dy()
		}
		return imaging.Resize(img, w, h, imaging.Lanczos)
	default: // ModeFit — proportional; imaging.Fit treats 0 as "no bound".
		w, h := p.Width, p.Height
		if w == 0 {
			w = MaxDimension
		}
		if h == 0 {
			h = MaxDimension
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

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
