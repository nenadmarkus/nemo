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
	"reflect"
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

func TestLoadConfig(t *testing.T) {
	temp := 0.5
	filePath := writeTemp(t, "config.json", []byte(
		`{"base_url":"http://file","model":"file-model","api_key":"file-key",`+
			`"temperature":0.5,"max_tokens":128}`))

	tests := []struct {
		name    string
		value   string
		want    config
		wantErr string // substring; "" = no error
	}{
		{
			name:  "file path",
			value: filePath,
			want: config{
				BaseURL: "http://file", Model: "file-model", APIKey: "file-key",
				Temperature: &temp, MaxTokens: 128,
			},
		},
		{
			name:  "inline JSON",
			value: `{"base_url":"http://inline","model":"inline-model","api_key":"inline-key"}`,
			want:  config{BaseURL: "http://inline", Model: "inline-model", APIKey: "inline-key"},
		},
		{
			name:  "inline JSON with surrounding whitespace",
			value: "\n\t {\"model\":\"ws-model\"} \n",
			want:  config{Model: "ws-model"},
		},
		{
			name:  "inline empty object",
			value: "{}",
			want:  config{},
		},
		{
			name:    "inline invalid JSON does not echo the value",
			value:   `{"api_key":"super-secret-sentinel",`,
			wantErr: "config: invalid JSON",
		},
		{
			name:    "missing file path",
			value:   filepath.Join(t.TempDir(), "nope.json"),
			wantErr: "no such file",
		},
		{
			name:    "empty value",
			value:   "",
			wantErr: "config : open",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := loadConfig(tt.value)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("loadConfig(%q) err = %v, want containing %q", tt.value, err, tt.wantErr)
				}
				if strings.Contains(err.Error(), "super-secret-sentinel") {
					t.Fatalf("loadConfig(%q) error leaks the value: %v", tt.value, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadConfig(%q): %v", tt.value, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("loadConfig(%q) = %+v, want %+v", tt.value, got, tt.want)
			}
		})
	}
}
