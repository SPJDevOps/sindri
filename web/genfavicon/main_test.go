package main

import (
	"bytes"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

func TestFavicon_CommittedFilesMatchTheGenerator(t *testing.T) {
	p, err := pngImage()
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string][]byte{"favicon.svg": svg(), "favicon.png": p} {
		got, err := os.ReadFile(filepath.Join("..", "static", name)) //nolint:gosec // G304: a fixed name
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("web/static/%s is out of step with web/genfavicon; run `go run ./web/genfavicon` from the repository root", name)
		}
	}
}

func TestFavicon_PNGIs32SquareWithRoundedCornersAndAnS(t *testing.T) {
	b, err := pngImage()
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if got := img.Bounds(); got.Dx() != 32 || got.Dy() != 32 {
		t.Fatalf("size = %v, want 32x32", got)
	}
	// The 2px radius leaves the corner pixel about a third covered.
	if _, _, _, a := img.At(0, 0).RGBA(); a > 0x8000 {
		t.Errorf("the corner pixel has alpha %#x, want it mostly transparent", a)
	}
	// Inside the square: background at (2,2), white inside the top bar of the S.
	for _, c := range []struct {
		x, y    int
		r, g, b uint32
	}{{2, 2, 0x1f, 0x5f, 0x9a}, {16, 8, 0xff, 0xff, 0xff}, {10, 16, 0xff, 0xff, 0xff}, {22, 23, 0xff, 0xff, 0xff}, {16, 12, 0x1f, 0x5f, 0x9a}} {
		r, g, b, _ := img.At(c.x, c.y).RGBA()
		if r>>8 != c.r || g>>8 != c.g || b>>8 != c.b {
			t.Errorf("pixel (%d,%d) = %02x%02x%02x, want %02x%02x%02x", c.x, c.y, r>>8, g>>8, b>>8, c.r, c.g, c.b)
		}
	}
}
