// Command genfavicon writes web/static/favicon.svg and web/static/favicon.png
// from one piece of geometry, so the two cannot disagree (SIN-98).
//
// The mark is a squared, Tektur-style "S" in white on the steel-blue accent
// (--accent, light value), in a square with 2px corners. The S is a path, not
// text: a favicon cannot load a web font. Run it from the repository root:
//
//	go run ./web/genfavicon
//
// The test in this package regenerates both files in memory and fails when the
// committed ones differ, so editing the geometry without running the command
// is caught.
package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log"
	"os"
	"path/filepath"
	"strings"
)

const (
	// size is the grid both files are drawn on, and the PNG's side in pixels.
	size = 32
	// corner is the square's corner radius on that grid.
	corner = 2
	// background is --accent in sindri.css, light theme; foreground is the S.
	background = "#1f5f9a"
	foreground = "#ffffff"
	// samples is the supersampling per axis used to anti-alias the PNG.
	samples = 8
)

type point struct{ x, y float64 }

// glyph is the S, clockwise from the top-left. Three bars 4 wide, joined by
// two verticals, with the outer corners at the top right and bottom left
// chamfered by 2.
var glyph = []point{
	{8, 7}, {22, 7}, {24, 9}, {24, 11}, {12, 11}, {12, 14}, {24, 14}, {24, 25},
	{10, 25}, {8, 23}, {8, 21}, {20, 21}, {20, 18}, {8, 18},
}

// svg renders the mark. The path is the glyph and nothing else is computed.
func svg() []byte {
	var d strings.Builder
	for i, p := range glyph {
		cmd := "L"
		if i == 0 {
			cmd = "M"
		}
		fmt.Fprintf(&d, "%s%g %g", cmd, p.x, p.y)
	}
	d.WriteString("Z")
	return fmt.Appendf(nil, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %[1]d %[1]d" width="%[1]d" height="%[1]d">
<rect width="%[1]d" height="%[1]d" rx="%[2]d" fill="%[3]s"/>
<path d="%[4]s" fill="%[5]s"/>
</svg>
`, size, corner, background, d.String(), foreground)
}

// inGlyph reports whether (x, y) is inside the S, by the even-odd rule.
func inGlyph(x, y float64) bool {
	in := false
	for i, j := 0, len(glyph)-1; i < len(glyph); j, i = i, i+1 {
		a, b := glyph[i], glyph[j]
		if (a.y > y) != (b.y > y) && x < (b.x-a.x)*(y-a.y)/(b.y-a.y)+a.x {
			in = !in
		}
	}
	return in
}

// inSquare reports whether (x, y) is inside the rounded square.
func inSquare(x, y float64) bool {
	if x < 0 || y < 0 || x > size || y > size {
		return false
	}
	// Distance from the nearest corner's centre, when in a corner zone.
	cx, cy := x, y
	switch {
	case x < corner:
		cx = corner
	case x > size-corner:
		cx = size - corner
	}
	switch {
	case y < corner:
		cy = corner
	case y > size-corner:
		cy = size - corner
	}
	dx, dy := x-cx, y-cy
	return dx*dx+dy*dy <= corner*corner
}

// pngImage rasterises the mark at size by size, supersampled, with alpha
// outside the rounded corners.
func pngImage() ([]byte, error) {
	bg, fg := hex(background), hex(foreground)
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	for py := range size {
		for px := range size {
			var r, g, b, a float64
			for sy := range samples {
				for sx := range samples {
					x := float64(px) + (float64(sx)+0.5)/samples
					y := float64(py) + (float64(sy)+0.5)/samples
					switch {
					case !inSquare(x, y):
					case inGlyph(x, y):
						r, g, b, a = r+float64(fg.R), g+float64(fg.G), b+float64(fg.B), a+1
					default:
						r, g, b, a = r+float64(bg.R), g+float64(bg.G), b+float64(bg.B), a+1
					}
				}
			}
			if a == 0 {
				continue
			}
			img.SetNRGBA(px, py, color.NRGBA{
				R: uint8(r/a + 0.5), G: uint8(g/a + 0.5), B: uint8(b/a + 0.5),
				A: uint8(a/(samples*samples)*255 + 0.5),
			})
		}
	}
	var buf bytes.Buffer
	if err := (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("encoding the favicon: %w", err)
	}
	return buf.Bytes(), nil
}

func hex(s string) color.NRGBA {
	var r, g, b uint8
	if _, err := fmt.Sscanf(s, "#%02x%02x%02x", &r, &g, &b); err != nil {
		panic("genfavicon: bad colour " + s) // impossible: the constants above
	}
	return color.NRGBA{R: r, G: g, B: b, A: 255}
}

func main() {
	dir := filepath.Join("web", "static")
	if _, err := os.Stat(dir); err != nil {
		log.Fatalf("genfavicon: %s not found; run from the repository root", dir)
	}
	p, err := pngImage()
	if err != nil {
		log.Fatal(err)
	}
	for name, b := range map[string][]byte{"favicon.svg": svg(), "favicon.png": p} {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			log.Fatalf("genfavicon: %v", err)
		}
	}
}
