package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/image/webp"
)

// gradientPNG renders a w×h PNG with pixel-varying content so WebP has
// something to compress and decode round-trips meaningfully.
func gradientPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{uint8(x % 256), uint8(y % 256), 128, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func writeTemp(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// decodeDataURL pulls the payload out of a base64 data: URL.
func decodeDataURL(t *testing.T, url, mediaType string) []byte {
	t.Helper()
	prefix := "data:" + mediaType + ";base64,"
	if !strings.HasPrefix(url, prefix) {
		t.Fatalf("url = %.60q..., want %q prefix", url, prefix)
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(url, prefix))
	if err != nil {
		t.Fatalf("decode base64: %v", err)
	}
	return data
}

func TestWebPImageURLResizesLargeImages(t *testing.T) {
	// 3000×2000 PNG → WebP with the longest side capped at 2000
	// (2000×1333: scale = 2000/3000).
	p := writeTemp(t, "big.png", gradientPNG(t, 3000, 2000))
	url, err := webpImageURL(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	data := decodeDataURL(t, url, "image/webp")

	dec, err := webp.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode webp: %v", err)
	}
	if b := dec.Bounds(); b.Dx() != 2000 || b.Dy() != 1333 {
		t.Errorf("resized bounds = %dx%d, want 2000x1333", b.Dx(), b.Dy())
	}
	// Re-encoding must actually shrink a photo-like image meaningfully.
	if src := 3000 * 2000 * 4; len(data) > src/10 {
		t.Errorf("webp payload = %d bytes, want well under a tenth of the raw %d", len(data), src)
	}
}

func TestWebPImageURLKeepsSmallImages(t *testing.T) {
	// Already under the cap: dimensions pass through; format is still
	// normalized to WebP.
	p := writeTemp(t, "small.png", gradientPNG(t, 500, 400))
	url, err := webpImageURL(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := webp.Decode(bytes.NewReader(decodeDataURL(t, url, "image/webp")))
	if err != nil {
		t.Fatalf("decode webp: %v", err)
	}
	if b := dec.Bounds(); b.Dx() != 500 || b.Dy() != 400 {
		t.Errorf("bounds = %dx%d, want 500x400", b.Dx(), b.Dy())
	}
}

func TestWebPImageURLRejectsNonImages(t *testing.T) {
	// A text file with an image extension: admitted by neither the sniff
	// nor the decoder.
	p := writeTemp(t, "fake.png", []byte("one\ntwo"))
	if _, err := webpImageURL(context.Background(), p); err == nil || !strings.Contains(err.Error(), "not a supported image") {
		t.Errorf("fake.png err = %v, want not-a-supported-image error", err)
	}
}
