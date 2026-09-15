package image

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestQuantize(t *testing.T) {
	cases := []struct {
		in   Params
		step int
		want Params
	}{
		{Params{Width: 333, Quality: 73}, 16, Params{Width: 336, Quality: 75}},
		{Params{Width: 320, Height: 17, Quality: 75}, 16, Params{Width: 320, Height: 32, Quality: 75}},
		{Params{Width: 333, Quality: 73}, 1, Params{Width: 333, Quality: 75}},
		{Params{Width: 333, Quality: 73}, 0, Params{Width: 333, Quality: 75}},
		{Params{Width: MaxDimension, Height: MaxDimension - 1, Quality: 98}, 16, Params{Width: MaxDimension, Height: MaxDimension, Quality: 100}},
		{Params{Width: 4999, Quality: 100}, 100, Params{Width: MaxDimension, Quality: 100}},
		{Params{Quality: 1}, 16, Params{Quality: 5}},
		{Params{Quality: 0}, 16, Params{Quality: 0}}, // unset stays unset
		{Params{Width: 0, Height: 0, Quality: 50}, 16, Params{Width: 0, Height: 0, Quality: 50}},
	}
	for _, c := range cases {
		if got := c.in.Quantize(c.step); got != c.want {
			t.Errorf("%+v.Quantize(%d) = %+v want %+v", c.in, c.step, got, c.want)
		}
	}
}

func TestParseParamsAndSpec(t *testing.T) {
	if _, want := ParseParams(url.Values{}); want {
		t.Fatal("empty query wants transform")
	}
	p, want := ParseParams(url.Values{"w": {"100"}, "h": {"abc"}, "m": {"C"}, "q": {"500"}})
	if !want || p.Width != 100 || p.Height != 0 || p.Mode != ModeFill || p.Quality != 100 {
		t.Fatalf("%+v %v", p, want)
	}
	p, want = ParseParams(url.Values{"w": {"999999"}, "m": {"e"}})
	if !want || p.Width != MaxDimension || p.Mode != ModeExact || p.Quality != DefaultQuality {
		t.Fatalf("%+v %v", p, want)
	}
	// Bare ?q= is an optimize request.
	p, want = ParseParams(url.Values{"q": {"40"}})
	if !want || p.Width != 0 || p.Height != 0 || p.Quality != 40 {
		t.Fatalf("%+v %v", p, want)
	}
	// w=0 with no q: nothing to do.
	if _, want := ParseParams(url.Values{"w": {"0"}}); want {
		t.Fatal("w=0 wants transform")
	}
	if s := (Params{Width: 20, Height: 10, Mode: ModeFit, Quality: 75}).Spec(); s != "w20h10mfq75" {
		t.Fatalf("spec %q", s)
	}
	if !IsImageContentType("image/JPEG; charset=x") || IsImageContentType("text/plain") || IsImageContentType("image/svg+xml") {
		t.Fatal("IsImageContentType")
	}
	if VariantContentType("image/gif") != "image/png" || VariantContentType("image/jpeg") != "image/jpeg" {
		t.Fatal("VariantContentType")
	}
}

// testJPEG returns a 40x20 JPEG with a gradient.
func testImage() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, 40, 20))
	for y := 0; y < 20; y++ {
		for x := 0; x < 40; x++ {
			img.Set(x, y, color.RGBA{uint8(x * 6), uint8(y * 12), 128, 255})
		}
	}
	return img
}

func testJPEG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, testImage(), &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func testPNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, testImage()); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func decodeBounds(t *testing.T, data []byte, wantFormat string) image.Rectangle {
	t.Helper()
	img, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if format != wantFormat {
		t.Fatalf("result format %q want %q", format, wantFormat)
	}
	return img.Bounds()
}

func TestTransform(t *testing.T) {
	src := testJPEG(t)

	t.Run("fit width", func(t *testing.T) {
		out, ct, err := Transform(bytes.NewReader(src), "image/jpeg", Params{Width: 20, Mode: ModeFit, Quality: 80})
		if err != nil {
			t.Fatal(err)
		}
		if ct != "image/jpeg" {
			t.Fatalf("content type %q", ct)
		}
		if b := decodeBounds(t, out, "jpeg"); b.Dx() != 20 || b.Dy() != 10 {
			t.Fatalf("bounds %v want 20x10", b)
		}
	})
	t.Run("fit height", func(t *testing.T) {
		out, _, err := Transform(bytes.NewReader(src), "image/jpeg", Params{Height: 5, Mode: ModeFit, Quality: 80})
		if err != nil {
			t.Fatal(err)
		}
		if b := decodeBounds(t, out, "jpeg"); b.Dx() != 10 || b.Dy() != 5 {
			t.Fatalf("bounds %v want 10x5", b)
		}
	})
	t.Run("fit never upscales", func(t *testing.T) {
		out, _, err := Transform(bytes.NewReader(src), "image/jpeg", Params{Width: 1000, Height: 1000, Mode: ModeFit, Quality: 80})
		if err != nil {
			t.Fatal(err)
		}
		if b := decodeBounds(t, out, "jpeg"); b.Dx() != 40 || b.Dy() != 20 {
			t.Fatalf("bounds %v want 40x20", b)
		}
		out, _, err = Transform(bytes.NewReader(src), "image/jpeg", Params{Width: 1000, Mode: ModeFit, Quality: 80})
		if err != nil {
			t.Fatal(err)
		}
		if b := decodeBounds(t, out, "jpeg"); b.Dx() != 40 || b.Dy() != 20 {
			t.Fatalf("w-only bounds %v want 40x20", b)
		}
	})
	t.Run("fill crops", func(t *testing.T) {
		out, _, err := Transform(bytes.NewReader(src), "image/jpeg", Params{Width: 10, Height: 10, Mode: ModeFill, Quality: 80})
		if err != nil {
			t.Fatal(err)
		}
		if b := decodeBounds(t, out, "jpeg"); b.Dx() != 10 || b.Dy() != 10 {
			t.Fatalf("bounds %v want 10x10", b)
		}
	})
	t.Run("exact stretches", func(t *testing.T) {
		out, _, err := Transform(bytes.NewReader(src), "image/jpeg", Params{Width: 7, Height: 30, Mode: ModeExact, Quality: 80})
		if err != nil {
			t.Fatal(err)
		}
		if b := decodeBounds(t, out, "jpeg"); b.Dx() != 7 || b.Dy() != 30 {
			t.Fatalf("bounds %v want 7x30", b)
		}
	})
	t.Run("quality only keeps size", func(t *testing.T) {
		out, _, err := Transform(bytes.NewReader(src), "image/jpeg", Params{Quality: 30})
		if err != nil {
			t.Fatal(err)
		}
		if b := decodeBounds(t, out, "jpeg"); b.Dx() != 40 || b.Dy() != 20 {
			t.Fatalf("bounds %v", b)
		}
	})
	t.Run("png stays png", func(t *testing.T) {
		out, ct, err := Transform(bytes.NewReader(testPNG(t)), "image/png", Params{Width: 20, Mode: ModeFit, Quality: 75})
		if err != nil {
			t.Fatal(err)
		}
		if ct != "image/png" || !bytes.HasPrefix(out, []byte("\x89PNG\r\n\x1a\n")) {
			t.Fatalf("ct %q prefix %q", ct, out[:8])
		}
		if b := decodeBounds(t, out, "png"); b.Dx() != 20 || b.Dy() != 10 {
			t.Fatalf("bounds %v", b)
		}
	})
	t.Run("content type is not trusted", func(t *testing.T) {
		// PNG bytes declared as JPEG still come out as PNG (magic wins).
		_, ct, err := Transform(bytes.NewReader(testPNG(t)), "image/jpeg", Params{Width: 20, Quality: 75})
		if err != nil || ct != "image/png" {
			t.Fatalf("%q %v", ct, err)
		}
	})
	t.Run("garbage", func(t *testing.T) {
		if _, _, err := Transform(bytes.NewReader([]byte("this is not an image at all")), "image/jpeg", Params{Width: 10}); err == nil {
			t.Fatal("garbage accepted")
		}
		// Valid magic, truncated body.
		if _, _, err := Transform(bytes.NewReader(src[:40]), "image/jpeg", Params{Width: 10}); err == nil {
			t.Fatal("truncated jpeg accepted")
		}
		if _, _, err := Transform(bytes.NewReader(nil), "image/jpeg", Params{Width: 10}); err == nil {
			t.Fatal("empty accepted")
		}
	})
}

// craftedPNG builds a syntactically valid PNG signature + IHDR chunk claiming
// the given dimensions (no pixel data).
func craftedPNG(width, height uint32) []byte {
	var b bytes.Buffer
	b.WriteString("\x89PNG\r\n\x1a\n")
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], width)
	binary.BigEndian.PutUint32(ihdr[4:], height)
	ihdr[8] = 8  // bit depth
	ihdr[9] = 6  // RGBA
	ihdr[10] = 0 // compression
	ihdr[11] = 0 // filter
	ihdr[12] = 0 // interlace
	binary.Write(&b, binary.BigEndian, uint32(len(ihdr)))
	chunk := append([]byte("IHDR"), ihdr...)
	b.Write(chunk)
	binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(chunk))
	return b.Bytes()
}

func TestTransform_RejectsHugeDimensionsBeforeDecode(t *testing.T) {
	src := craftedPNG(60000, 60000)
	start := time.Now()
	_, _, err := Transform(bytes.NewReader(src), "image/png", Params{Width: 10, Quality: 75})
	if !errors.Is(err, ErrSourceTooLarge) {
		t.Fatalf("want ErrSourceTooLarge, got %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("rejection took %v", d)
	}
	// Right at the pixel budget is allowed past the config check (it then
	// fails on the missing pixel data, which is a decode error, not TooLarge).
	ok := craftedPNG(5000, 10000) // 50 MP
	_, _, err = Transform(bytes.NewReader(ok), "image/png", Params{Width: 10})
	if errors.Is(err, ErrSourceTooLarge) {
		t.Fatalf("50MP source rejected as too large: %v", err)
	}
	if err == nil {
		t.Fatal("header-only png decoded successfully?")
	}
	// Zero-sized image is rejected (by the decoder or by our dimension guard).
	if _, _, err = Transform(bytes.NewReader(craftedPNG(0, 10)), "image/png", Params{Width: 10}); err == nil {
		t.Fatal("zero-width png accepted")
	}
}

func TestTransform_SourceByteLimit(t *testing.T) {
	// A reader that yields more than DefaultMaxSourceBytes is rejected without
	// decoding; use a repeating reader so no giant buffer is needed here.
	r := &repeatReader{n: DefaultMaxSourceBytes + 10}
	_, _, err := Transform(r, "image/jpeg", Params{Width: 10})
	if !errors.Is(err, ErrSourceTooLarge) {
		t.Fatalf("want ErrSourceTooLarge, got %v", err)
	}
}

type repeatReader struct{ n int64 }

func (r *repeatReader) Read(p []byte) (int, error) {
	if r.n <= 0 {
		return 0, errors.New("unexpected read past limit")
	}
	if int64(len(p)) > r.n {
		p = p[:r.n]
	}
	for i := range p {
		p[i] = 0xFF
	}
	r.n -= int64(len(p))
	return len(p), nil
}

func TestLimiter(t *testing.T) {
	var nilL *Limiter
	if !nilL.Acquire(context.Background()) || !nilL.TryAcquire() {
		t.Fatal("nil limiter must never block")
	}
	nilL.Release() // must not panic

	l := NewLimiter(2)
	if !l.TryAcquire() || !l.TryAcquire() {
		t.Fatal("first two slots")
	}
	if l.TryAcquire() {
		t.Fatal("third slot granted")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if l.Acquire(ctx) {
		t.Fatal("Acquire succeeded while full")
	}
	l.Release()
	if !l.Acquire(context.Background()) {
		t.Fatal("Acquire after release")
	}
	// Release from another goroutine unblocks a waiting Acquire.
	done := make(chan bool, 1)
	go func() { done <- l.Acquire(context.Background()) }()
	select {
	case <-done:
		t.Fatal("Acquire returned while full")
	case <-time.After(30 * time.Millisecond):
	}
	l.Release()
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("Acquire false")
		}
	case <-time.After(time.Second):
		t.Fatal("waiter not released")
	}
	// Default capacity for n <= 0 is 4.
	d := NewLimiter(0)
	for i := 0; i < 4; i++ {
		if !d.TryAcquire() {
			t.Fatalf("default limiter slot %d", i)
		}
	}
	if d.TryAcquire() {
		t.Fatal("default limiter has more than 4 slots")
	}
}

func TestTrimDir(t *testing.T) {
	dir := t.TempDir()
	write := func(rel string, size int, age time.Duration) string {
		p := filepath.Join(dir, rel)
		os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, bytes.Repeat([]byte("v"), size), 0o600); err != nil {
			t.Fatal(err)
		}
		mt := time.Now().Add(-age)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
		return p
	}
	oldest := write("k1/a.ext", 100, 3*time.Hour)
	middle := write("k2/b.ext", 100, 2*time.Hour)
	newest := write("k3/c.ext", 100, 1*time.Hour)

	// Under the limit: nothing happens.
	if n, freed := trimDir(dir, 300); n != 0 || freed != 0 {
		t.Fatalf("under limit: removed %d freed %d", n, freed)
	}
	// 300 > 250 → drop the oldest only.
	n, freed := trimDir(dir, 250)
	if n != 1 || freed != 100 {
		t.Fatalf("removed %d freed %d", n, freed)
	}
	if _, err := os.Stat(oldest); !os.IsNotExist(err) {
		t.Fatal("oldest file still present")
	}
	if _, err := os.Stat(filepath.Dir(oldest)); !os.IsNotExist(err) {
		t.Fatal("empty key dir not removed")
	}
	for _, p := range []string{middle, newest} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s removed: %v", p, err)
		}
	}
	// 200 > 50 → drop middle, then newest (both needed).
	n, freed = trimDir(dir, 50)
	if n != 2 || freed != 200 {
		t.Fatalf("removed %d freed %d", n, freed)
	}
	// Missing dir is harmless.
	if n, _ := trimDir(filepath.Join(dir, "nope"), 1); n != 0 {
		t.Fatal("missing dir")
	}
}

// memStore is a VariantStore over in-memory maps for worker tests.
type memStore struct {
	objects  map[string][]byte
	variants map[string][]byte
}

func (m *memStore) GetObject(bucket, key string) (io.ReadCloser, error) {
	b, ok := m.objects[bucket+"/"+key]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (m *memStore) GetVersionedObject(bucket, key, versionID string) (io.ReadCloser, error) {
	return m.GetObject(bucket, key+"@"+versionID)
}

func (m *memStore) PutVariant(bucket, cacheKey string, data []byte) error {
	m.variants[bucket+"/"+cacheKey] = append([]byte(nil), data...)
	return nil
}

func TestOptimizeOne(t *testing.T) {
	store := &memStore{objects: map[string][]byte{"b/pic.jpg": testJPEG(t), "b/text.txt": []byte("nope")}, variants: map[string][]byte{}}
	if err := OptimizeOne(store, Job{Bucket: "b", Key: "text.txt", ContentType: "text/plain"}, 75, 0); err != nil {
		t.Fatalf("non-image should be a no-op: %v", err)
	}
	if len(store.variants) != 0 {
		t.Fatal("variant written for non-image")
	}
	if err := OptimizeOne(store, Job{Bucket: "b", Key: "pic.jpg", ETag: "\"e\"", ContentType: "image/jpeg"}, 60, 0); err != nil {
		t.Fatal(err)
	}
	if len(store.variants) != 1 {
		t.Fatalf("variants %d", len(store.variants))
	}
	for _, v := range store.variants {
		if b := decodeBounds(t, v, "jpeg"); b.Dx() != 40 || b.Dy() != 20 {
			t.Fatalf("optimized bounds %v", b)
		}
	}
	// Missing object → error, no variant.
	if err := OptimizeOne(store, Job{Bucket: "b", Key: "missing.jpg", ContentType: "image/jpeg"}, 60, 0); err == nil {
		t.Fatal("missing source accepted")
	}
}
