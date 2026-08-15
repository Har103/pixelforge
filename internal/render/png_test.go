package render

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
)

// pngPalette is a stand-in for canvas.Palette: index 0 is the background.
var pngPalette = []string{"#12141c", "#ffffff", "#ff4d6d", "#3a86ff"}

func pngColour(t *testing.T, hex string) color.RGBA {
	t.Helper()
	c, err := parseHexColour(hex)
	if err != nil {
		t.Fatalf("test palette entry %q does not parse: %v", hex, err)
	}
	return c
}

// pngDecode runs the bytes back through the decoder, which is the only way to
// be sure the encoder produced a real PNG rather than something that merely
// looked like one in memory.
func pngDecode(t *testing.T, b []byte) image.Image {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("decoding the encoded png: %v", err)
	}
	return img
}

// pngAt reads one pixel as 8-bit RGBA regardless of the concrete image type the
// decoder chose.
func pngAt(img image.Image, x, y int) color.RGBA {
	return color.RGBAModel.Convert(img.At(x, y)).(color.RGBA)
}

func TestCanvasImageRejectsAMismatchedPixelSlice(t *testing.T) {
	for _, tc := range []struct {
		name          string
		cells         int
		width, height int
	}{
		{"too short", 11, 4, 3},
		{"too long", 13, 4, 3},
		{"empty", 0, 4, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := CanvasImage(make([]byte, tc.cells), tc.width, tc.height, pngPalette, 1)
			if err == nil {
				t.Fatalf("%d cells for a %dx%d canvas should be an error", tc.cells, tc.width, tc.height)
			}
			// The message has to name the numbers or it is useless in a log.
			for _, want := range []string{"4x3", "12"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q should mention %q", err, want)
				}
			}
		})
	}
}

func TestCanvasImageRejectsNonPositiveDimensions(t *testing.T) {
	for _, tc := range [][2]int{{0, 4}, {4, 0}, {-1, 4}, {4, -1}} {
		if _, err := CanvasImage(nil, tc[0], tc[1], pngPalette, 1); err == nil {
			t.Errorf("CanvasImage(%d, %d) should be an error", tc[0], tc[1])
		}
	}
}

func TestCanvasImageScalesByExactNearestNeighbour(t *testing.T) {
	// 3x2 grid, every cell a different palette entry so a misplaced pixel shows.
	cells := []byte{
		0, 1, 2,
		3, 0, 1,
	}
	const scale = 4
	img, err := CanvasImage(cells, 3, 2, pngPalette, scale)
	if err != nil {
		t.Fatalf("CanvasImage: %v", err)
	}
	if got, want := img.Bounds(), image.Rect(0, 0, 12, 8); got != want {
		t.Fatalf("bounds = %v, want %v", got, want)
	}

	for cy := 0; cy < 2; cy++ {
		for cx := 0; cx < 3; cx++ {
			want := pngColour(t, pngPalette[cells[cy*3+cx]])
			for dy := 0; dy < scale; dy++ {
				for dx := 0; dx < scale; dx++ {
					x, y := cx*scale+dx, cy*scale+dy
					if got := img.RGBAAt(x, y); got != want {
						t.Fatalf("pixel (%d,%d) in cell (%d,%d) = %v, want %v", x, y, cx, cy, got, want)
					}
				}
			}
		}
	}

	// Spot check the block boundary from the other direction: (3,3) is the last
	// pixel of cell (0,0) and (4,4) the first of cell (1,1).
	if got, want := img.RGBAAt(3, 3), pngColour(t, pngPalette[0]); got != want {
		t.Errorf("pixel (3,3) = %v, want the colour of cell (0,0) %v", got, want)
	}
	if got, want := img.RGBAAt(4, 4), pngColour(t, pngPalette[0]); got != want {
		t.Errorf("pixel (4,4) = %v, want the colour of cell (1,1) %v", got, want)
	}

	// Nothing may be interpolated: the output can only contain colours that were
	// in the palette to begin with.
	seen := map[color.RGBA]bool{}
	for y := 0; y < 8; y++ {
		for x := 0; x < 12; x++ {
			seen[img.RGBAAt(x, y)] = true
		}
	}
	if len(seen) != 4 {
		t.Errorf("output holds %d distinct colours, want the 4 palette entries used: %v", len(seen), seen)
	}
	for _, hex := range pngPalette {
		if !seen[pngColour(t, hex)] {
			t.Errorf("palette entry %s is missing from the output", hex)
		}
	}
}

func TestCanvasImageTreatsScaleBelowOneAsOneToOne(t *testing.T) {
	for _, scale := range []int{0, -1, -100} {
		img, err := CanvasImage(make([]byte, 6), 3, 2, pngPalette, scale)
		if err != nil {
			t.Fatalf("scale %d: %v", scale, err)
		}
		if got, want := img.Bounds(), image.Rect(0, 0, 3, 2); got != want {
			t.Errorf("scale %d: bounds = %v, want %v", scale, got, want)
		}
	}
}

func TestCanvasImageRefusesAScaleThatWouldExhaustMemory(t *testing.T) {
	// A scale arriving from a query string one day is the whole reason for the
	// ceiling: this must be an error the handler can report, not an OOM kill.
	if _, err := CanvasImage(make([]byte, 64*64), 64, 64, pngPalette, 1<<20); err == nil {
		t.Fatal("a scale that would allocate terabytes should be an error")
	}
	// A large but sane scale is still allowed.
	img, err := CanvasImage(make([]byte, 64*64), 64, 64, pngPalette, 32)
	if err != nil {
		t.Fatalf("%d pixels is well inside the limit: %v", outputPixels(64, 64, 32), err)
	}
	if got, want := img.Bounds(), image.Rect(0, 0, 2048, 2048); got != want {
		t.Errorf("bounds = %v, want %v", got, want)
	}
}

func TestCanvasImageDrawsAnOutOfRangeIndexAsTheBackground(t *testing.T) {
	// Index 9 and 255 are past the end of a four entry palette, which is what a
	// snapshot painted before the palette shrank looks like.
	cells := []byte{0, 9, 255, 1}
	img, err := CanvasImage(cells, 4, 1, pngPalette, 1)
	if err != nil {
		t.Fatalf("an out of range index must not be an error: %v", err)
	}
	background := pngColour(t, pngPalette[0])
	for _, x := range []int{0, 1, 2} {
		if got := img.RGBAAt(x, 0); got != background {
			t.Errorf("pixel %d = %v, want the background %v", x, got, background)
		}
	}
	if got, want := img.RGBAAt(3, 0), pngColour(t, pngPalette[1]); got != want {
		t.Errorf("a valid index next to an invalid one = %v, want %v", got, want)
	}
}

func TestCanvasImageDrawsAMalformedPaletteEntryAsMagenta(t *testing.T) {
	// Every one of these is a plausible typo, and none of them may panic.
	broken := []string{"#12141c", "", "not a colour", "#ff", "#zzzzzz", "ffffff", "#1234567"}
	img, err := CanvasImage([]byte{0, 1, 2, 3, 4, 5, 6}, 7, 1, broken, 1)
	if err != nil {
		t.Fatalf("a malformed palette must not fail the render: %v", err)
	}
	if got, want := img.RGBAAt(0, 0), pngColour(t, "#12141c"); got != want {
		t.Errorf("the parseable entry = %v, want %v", got, want)
	}
	// "ffffff" without the hash is accepted; the rest fall back to magenta.
	if got, want := img.RGBAAt(5, 0), pngColour(t, "#ffffff"); got != want {
		t.Errorf("a hash-less entry = %v, want %v", got, want)
	}
	for _, x := range []int{1, 2, 3, 4, 6} {
		if got := img.RGBAAt(x, 0); got != fallbackColour {
			t.Errorf("entry %d (%q) = %v, want the magenta fallback %v", x, broken[x], got, fallbackColour)
		}
	}
}

func TestCanvasImageWithAnEmptyPaletteDoesNotPanic(t *testing.T) {
	for _, palette := range [][]string{nil, {}} {
		img, err := CanvasImage([]byte{0, 3}, 2, 1, palette, 2)
		if err != nil {
			t.Fatalf("an empty palette must not fail the render: %v", err)
		}
		// With no entries at all there is no background to fall back to, so every
		// cell is the loud fallback rather than something arbitrary.
		if got := img.RGBAAt(0, 0); got != fallbackColour {
			t.Errorf("empty palette pixel = %v, want %v", got, fallbackColour)
		}
	}
}

func TestEncodePNGDecodesAtTheRequestedScale(t *testing.T) {
	cells := []byte{
		1, 2, 0, 3, 1,
		0, 0, 2, 2, 3,
		3, 1, 1, 0, 2,
		2, 3, 0, 1, 0,
	}
	var buf bytes.Buffer
	if err := EncodePNG(&buf, cells, 5, 4, pngPalette, 3); err != nil {
		t.Fatalf("EncodePNG: %v", err)
	}
	img := pngDecode(t, buf.Bytes())
	if got, want := img.Bounds(), image.Rect(0, 0, 15, 12); got != want {
		t.Fatalf("decoded bounds = %v, want %v", got, want)
	}
	// The colours have to survive the round trip too, and land on the same
	// blocks they did before encoding.
	for cy := 0; cy < 4; cy++ {
		for cx := 0; cx < 5; cx++ {
			want := pngColour(t, pngPalette[cells[cy*5+cx]])
			for _, p := range [][2]int{{0, 0}, {2, 2}, {1, 2}} {
				x, y := cx*3+p[0], cy*3+p[1]
				if got := pngAt(img, x, y); got != want {
					t.Fatalf("decoded pixel (%d,%d) = %v, want %v", x, y, got, want)
				}
			}
		}
	}
}

func TestEncodePNGAutoScaleAimsAtTheLongEdge(t *testing.T) {
	var buf bytes.Buffer
	if err := EncodePNG(&buf, make([]byte, 100*60), 100, 60, pngPalette, 0); err != nil {
		t.Fatalf("EncodePNG: %v", err)
	}
	// 1024/100 rounds to 10, so the long edge lands on 1000.
	if got, want := pngDecode(t, buf.Bytes()).Bounds(), image.Rect(0, 0, 1000, 600); got != want {
		t.Fatalf("auto scaled bounds = %v, want %v", got, want)
	}
}

func TestEncodePNGAutoScaleStaysNearTheTargetAndUnderTheCap(t *testing.T) {
	for _, tc := range [][2]int{
		{1, 1}, {3, 3}, {16, 16}, {64, 64}, {100, 60}, {128, 128}, {256, 256},
		{300, 300}, {512, 512}, {600, 600}, {683, 683}, {1000, 1000},
		{1, 2000}, {2000, 1}, {4096, 4096}, {5, 4000},
	} {
		width, height := tc[0], tc[1]
		scale := autoScale(width, height)
		if scale < 1 {
			t.Fatalf("autoScale(%d, %d) = %d, must never go below 1:1", width, height, scale)
		}
		if got := outputPixels(width, height, scale); got > autoMaxOutputPx && scale > 1 {
			t.Errorf("autoScale(%d, %d) = %d produces %d pixels, over the %d cap", width, height, scale, got, autoMaxOutputPx)
		}

		// Nearest, not merely close: no neighbouring scale may land the long edge
		// closer to the target, unless the cap is what stopped us.
		long := max(width, height)
		distance := func(s int) int {
			if d := long*s - autoTargetLongEdge; d >= 0 {
				return d
			}
			return autoTargetLongEdge - long*s
		}
		if outputPixels(width, height, scale+1) <= autoMaxOutputPx && distance(scale+1) < distance(scale) {
			t.Errorf("autoScale(%d, %d) = %d, but %d puts the long edge closer to %d", width, height, scale, scale+1, autoTargetLongEdge)
		}
		if scale > 1 && distance(scale-1) < distance(scale) {
			t.Errorf("autoScale(%d, %d) = %d, but %d puts the long edge closer to %d", width, height, scale, scale-1, autoTargetLongEdge)
		}
	}
}

func TestEncodePNGReportsBadInputInsteadOfWriting(t *testing.T) {
	var buf bytes.Buffer
	if err := EncodePNG(&buf, make([]byte, 5), 4, 3, pngPalette, 2); err == nil {
		t.Error("a mismatched pixel slice should be an error")
	}
	if buf.Len() != 0 {
		t.Errorf("nothing should have been written, got %d bytes", buf.Len())
	}
	if err := EncodePNG(nil, make([]byte, 12), 4, 3, pngPalette, 1); err == nil {
		t.Error("a nil writer should be an error")
	}
}

func TestSocialCardIsAlwaysTwelveHundredBySixThirty(t *testing.T) {
	longName := strings.Repeat("an unreasonably long room name ", 20)
	for _, tc := range []struct {
		name            string
		width, height   int
		title, subtitle string
		palette         []string
	}{
		{"single cell", 1, 1, "One", "1x1", pngPalette},
		{"tiny", 8, 8, "Tiny", "8x8 canvas", pngPalette},
		{"typical", 128, 128, "Pixelforge", "128x128 - 4,096 placements", pngPalette},
		{"wide", 300, 100, "Wide", "300x100", pngPalette},
		{"tall", 100, 300, "Tall", "100x300", pngPalette},
		{"larger than the card", 1600, 900, "Huge", "1600x900", pngPalette},
		{"no text at all", 64, 64, "", "", pngPalette},
		{"name only", 64, 64, "Name only", "", pngPalette},
		{"subtitle only", 64, 64, "", "subtitle only", pngPalette},
		{"name far too long", 64, 64, longName, longName, pngPalette},
		{"outside the font", 64, 64, "Café ✓ ★ 日本", "→ not ascii ←", pngPalette},
		{"every documented glyph", 64, 64, "ABCXYZ abcxyz 0189", `.,:;!?'"-_/()#&+×`, pngPalette},
		{"broken palette", 64, 64, "Broken", "palette", []string{"nope", "#zz1122"}},
		{"empty palette", 64, 64, "Empty", "palette", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			cells := make([]byte, tc.width*tc.height)
			for i := range cells {
				cells[i] = byte(i % 5) // includes index 4, past the end of the palette
			}
			if err := SocialCard(&buf, cells, tc.width, tc.height, tc.palette, tc.title, tc.subtitle); err != nil {
				t.Fatalf("SocialCard: %v", err)
			}
			if got, want := pngDecode(t, buf.Bytes()).Bounds(), image.Rect(0, 0, 1200, 630); got != want {
				t.Fatalf("card bounds = %v, want %v", got, want)
			}
		})
	}
}

func TestSocialCardCentresTheCanvasOnTheBackground(t *testing.T) {
	const side = 32
	cells := make([]byte, side*side)
	for i := range cells {
		cells[i] = 1 // a solid white square is easy to find again
	}
	var buf bytes.Buffer
	if err := SocialCard(&buf, cells, side, side, pngPalette, "Pixelforge", "a shared canvas"); err != nil {
		t.Fatalf("SocialCard: %v", err)
	}
	img := pngDecode(t, buf.Bytes())

	if got := pngAt(img, 0, 0); got != cardBackground {
		t.Errorf("corner = %v, want the card background %v", got, cardBackground)
	}

	white := pngColour(t, pngPalette[1])
	box := image.Rectangle{Min: image.Pt(1<<30, 1<<30)}
	counts := map[color.RGBA]int{}
	for y := 0; y < cardHeight; y++ {
		for x := 0; x < cardWidth; x++ {
			c := pngAt(img, x, y)
			counts[c]++
			if c == white {
				box.Min.X = min(box.Min.X, x)
				box.Min.Y = min(box.Min.Y, y)
				box.Max.X = max(box.Max.X, x+1)
				box.Max.Y = max(box.Max.Y, y+1)
			}
		}
	}

	if counts[white] == 0 {
		t.Fatal("the canvas was not drawn on the card")
	}
	if got := box.Dx() * box.Dy(); got != counts[white] {
		t.Errorf("the canvas is not a solid rectangle: %d white pixels in a %v box", counts[white], box)
	}
	if box.Dx() != box.Dy() {
		t.Errorf("a square canvas rendered as %dx%d", box.Dx(), box.Dy())
	}
	if box.Dx()%side != 0 {
		t.Errorf("canvas width %d is not a whole multiple of the %d cells, so the scale was not an integer", box.Dx(), side)
	}
	// Symmetric margins are what "centred horizontally" means in pixels.
	if left, right := box.Min.X, cardWidth-box.Max.X; left != right {
		t.Errorf("canvas is not centred: %d px to the left, %d to the right", left, right)
	}
	if box.Min.Y < cardMargin {
		t.Errorf("canvas top %d intrudes on the %d px margin", box.Min.Y, cardMargin)
	}

	if counts[cardBorder] < 2*(box.Dx()+box.Dy()) {
		t.Errorf("only %d border pixels for a %v canvas, expected an outline around it", counts[cardBorder], box)
	}
	if counts[cardNameInk] == 0 {
		t.Error("the name was not drawn")
	}
	if counts[cardSubtleInk] == 0 {
		t.Error("the subtitle was not drawn")
	}
	if counts[cardBackground] < cardWidth*cardHeight/2 {
		t.Errorf("only %d background pixels; the card should be mostly background", counts[cardBackground])
	}
}

func TestSocialCardRejectsAMismatchedPixelSlice(t *testing.T) {
	var buf bytes.Buffer
	if err := SocialCard(&buf, make([]byte, 7), 4, 4, pngPalette, "n", "s"); err == nil {
		t.Error("a mismatched pixel slice should be an error")
	}
	if buf.Len() != 0 {
		t.Errorf("nothing should have been written, got %d bytes", buf.Len())
	}
	if err := SocialCard(nil, make([]byte, 16), 4, 4, pngPalette, "n", "s"); err == nil {
		t.Error("a nil writer should be an error")
	}
}

func TestSocialCardTextIsTruncatedRatherThanOverflowed(t *testing.T) {
	const scale = 5
	short := "ROOM"
	if got := fitText(short, scale, 1000); got != short {
		t.Errorf("fitText(%q) = %q, want it left alone", short, got)
	}

	long := strings.Repeat("wide", 50)
	for _, maxWidth := range []int{1088, 500, 200, 60, 30} {
		got := fitText(long, scale, maxWidth)
		if w := textWidth(got, scale); w > maxWidth {
			t.Errorf("fitText(..., %d) = %q at %d px wide, which overflows", maxWidth, got, w)
		}
		if got != "" && !strings.HasSuffix(got, "...") {
			t.Errorf("fitText(..., %d) = %q, want it to end in an ellipsis", maxWidth, got)
		}
	}
	// Narrower than the ellipsis itself: draw nothing rather than a stray dot.
	if got := fitText(long, scale, 4); got != "" {
		t.Errorf("fitText with no room = %q, want the empty string", got)
	}
}

func TestSocialCardFontCoversTheDocumentedCharacters(t *testing.T) {
	const required = "ABCDEFGHIJKLMNOPQRSTUVWXYZ" +
		"abcdefghijklmnopqrstuvwxyz" +
		"0123456789 " + `.,:;!?'"-_/()#&+×`
	for _, r := range required {
		if g := lookupGlyph(r); g == missingGlyph {
			t.Errorf("no glyph for %q", r)
		}
	}
	// Lowercase is drawn with the uppercase glyphs, deliberately.
	if lookupGlyph('a') != lookupGlyph('A') {
		t.Error("lowercase should reuse the uppercase glyph")
	}
	// Anything else is a blank box rather than a gap or a panic.
	for _, r := range []rune{'✓', '★', 'é', '日', 0, 0x10FFFF} {
		if lookupGlyph(r) != missingGlyph {
			t.Errorf("%q should fall back to the blank box", r)
		}
	}
	if got, want := textWidth("AB", 2), (2*glyphAdvance-1)*2; got != want {
		t.Errorf("textWidth = %d, want %d", got, want)
	}
	if got := textWidth("", 3); got != 0 {
		t.Errorf("textWidth of the empty string = %d, want 0", got)
	}
}

func TestSocialCardDrawingStaysInsideTheImage(t *testing.T) {
	// drawText relies on draw.Draw clipping. Prove it: draw well off every edge
	// and check the image is untouched rather than the process being gone.
	img := image.NewRGBA(image.Rect(0, 0, 20, 20))
	fillRect(img, img.Bounds(), cardBackground)
	for _, at := range [][2]int{{-500, -500}, {-3, 5}, {5, -3}, {19, 19}, {500, 500}} {
		drawText(img, at[0], at[1], "OVERFLOW", 4, cardNameInk)
	}
	strokeRect(img, image.Rect(-10, -10, 30, 30), cardBorder)
	if got := img.RGBAAt(10, 10); got != cardBackground {
		t.Errorf("centre pixel = %v, want the untouched background %v", got, cardBackground)
	}
}
