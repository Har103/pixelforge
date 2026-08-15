package render

// PNG rendering: the still picture of a canvas, and the Open Graph card that
// wraps it for link previews. Both go through the same nearest-neighbour
// scaler, so a shared image looks exactly like the canvas does in the browser.
// The card draws its own text from the bitmap font at the bottom of this file,
// because the standard library has no font package and a third-party one is not
// on the table.

import (
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"strings"
	"unicode/utf8"
)

const (
	// autoTargetLongEdge is what EncodePNG aims the long edge at when the caller
	// does not pick a scale: big enough to look at, small enough to embed.
	autoTargetLongEdge = 1024

	// autoMaxOutputPx caps the automatic scale at roughly four megapixels. It is
	// a backstop rather than a limit that bites often, since aiming the long edge
	// at 1024 already keeps the area near a megapixel. A canvas larger than the
	// cap at 1:1 is still rendered 1:1: dropping cells is worse than a big file.
	autoMaxOutputPx = 4 << 20

	// maxRenderPx is the largest image CanvasImage will allocate, about 16.7
	// million pixels or 64 MiB of RGBA. An explicit scale is the caller's
	// business right up to the point where it becomes a request to allocate the
	// whole machine, and a scale that arrived in a query string eventually will
	// be. An error is recoverable; the out-of-memory kill it replaces is not.
	maxRenderPx = 1 << 24
)

// fallbackColour is what a palette entry that will not parse renders as.
// Magenta is deliberately loud: a broken palette should be obvious in the
// picture rather than quietly wrong, and rendering a share link must never be
// the thing that panics the server.
var fallbackColour = color.RGBA{R: 0xff, G: 0x00, B: 0xff, A: 0xff}

// CanvasImage renders a canvas grid to an image at an integer scale.
// pixels is one palette index per cell, row-major, len == width*height.
// palette holds "#rrggbb" strings; index 0 is the background.
//
// Scaling is nearest neighbour, which at an integer factor is an exact block
// copy: no interpolation, so the pixel edges stay hard. A scale below 1 is
// treated as 1, since anything lower would mean discarding cells, and a scale
// that would allocate an absurdly large image is refused rather than attempted.
func CanvasImage(pixels []byte, width, height int, palette []string, scale int) (*image.RGBA, error) {
	if err := checkGrid(pixels, width, height); err != nil {
		return nil, err
	}
	if scale < 1 {
		scale = 1
	}
	if got := outputPixels(width, height, scale); got > maxRenderPx {
		return nil, fmt.Errorf("render: a %dx%d canvas at scale %d is %d pixels, over the %d limit", width, height, scale, got, maxRenderPx)
	}
	table := newColourTable(palette)

	img := image.NewRGBA(image.Rect(0, 0, width*scale, height*scale))
	for cy := 0; cy < height; cy++ {
		top := img.PixOffset(0, cy*scale)
		row := img.Pix[top : top+img.Stride]
		for cx, index := range pixels[cy*width : (cy+1)*width] {
			c := table.at(index)
			for i := 0; i < scale; i++ {
				o := (cx*scale + i) * 4
				row[o], row[o+1], row[o+2], row[o+3] = c.R, c.G, c.B, c.A
			}
		}
		// The rest of the block is the row that was just painted, so copy it
		// rather than looking every cell up again.
		for dy := 1; dy < scale; dy++ {
			off := top + dy*img.Stride
			copy(img.Pix[off:off+img.Stride], row)
		}
	}
	return img, nil
}

// EncodePNG renders the canvas and writes it as a PNG.
// scale <= 0 picks a scale that puts the long edge near 1024px, capped so the
// output never exceeds roughly 4 megapixels.
func EncodePNG(w io.Writer, pixels []byte, width, height int, palette []string, scale int) error {
	if w == nil {
		return errors.New("render: nil writer")
	}
	if scale <= 0 {
		scale = autoScale(width, height)
	}
	img, err := CanvasImage(pixels, width, height, palette, scale)
	if err != nil {
		return err
	}
	if err := png.Encode(w, img); err != nil {
		return fmt.Errorf("render: encoding png: %w", err)
	}
	return nil
}

// checkGrid rejects a pixel buffer that does not describe the stated grid. The
// arithmetic is in int64 so absurd dimensions cannot wrap into a product that
// happens to match the slice length.
func checkGrid(pixels []byte, width, height int) error {
	if width <= 0 || height <= 0 {
		return fmt.Errorf("render: canvas dimensions must be positive, got %dx%d", width, height)
	}
	if want := int64(width) * int64(height); want != int64(len(pixels)) {
		return fmt.Errorf("render: pixel buffer holds %d cells, but a %dx%d canvas needs %d", len(pixels), width, height, want)
	}
	return nil
}

// autoScale picks the zoom for a caller that did not ask for one.
func autoScale(width, height int) int {
	long := max(width, height)
	if long <= 0 {
		// Nonsense dimensions are the caller's problem; CanvasImage will say so.
		// Returning early only keeps the division below safe.
		return 1
	}
	// Rounding to nearest rather than down, because for a 600 cell canvas an
	// output of 1200 is much closer to the target than 600 is.
	scale := (autoTargetLongEdge + long/2) / long
	if scale < 1 {
		scale = 1
	}
	for scale > 1 && outputPixels(width, height, scale) > autoMaxOutputPx {
		scale--
	}
	return scale
}

// colourTable is a palette resolved to colours once, so the per-pixel loops
// never touch a string.
type colourTable struct {
	colours    []color.RGBA
	background color.RGBA
}

func newColourTable(entries []string) colourTable {
	t := colourTable{colours: make([]color.RGBA, len(entries)), background: fallbackColour}
	for i, s := range entries {
		t.colours[i] = paletteColour(s)
	}
	if len(t.colours) > 0 {
		t.background = t.colours[0]
	}
	return t
}

// at resolves one cell. An index past the end of the palette is drawn as the
// background rather than being an error: a snapshot can outlive the palette it
// was painted with, and an unknown colour reading as unpainted is a far better
// outcome than failing, or panicking, on a link preview.
func (t colourTable) at(index byte) color.RGBA {
	if int(index) < len(t.colours) {
		return t.colours[index]
	}
	return t.background
}

// paletteColour is parseHexColour with a visible failure mode instead of an
// error. EncodeTimelapse can refuse a bad palette because it is an explicit
// export the caller is waiting on; a PNG is served on a share link, where one
// unparseable entry should cost that colour and nothing else.
func paletteColour(s string) color.RGBA {
	c, err := parseHexColour(s)
	if err != nil {
		return fallbackColour
	}
	return c
}

// Card geometry. These are the numbers that made the layout look deliberate at
// 1200x630: margins wide enough to breathe, a name readable when the card is
// shown as a thumbnail in a timeline, and a subtitle that is clearly secondary.
const (
	cardWidth  = 1200
	cardHeight = 630

	cardMargin        = 56
	cardTextGap       = 32 // between the canvas and the name
	cardLineGap       = 16 // between the name and the subtitle
	cardNameScale     = 5
	cardSubtitleScale = 3
)

var (
	cardBackground = color.RGBA{R: 0x0a, G: 0x0b, B: 0x10, A: 0xff}
	cardBorder     = color.RGBA{R: 0x23, G: 0x27, B: 0x3a, A: 0xff}
	cardNameInk    = color.RGBA{R: 0xee, G: 0xf1, B: 0xf7, A: 0xff}
	cardSubtleInk  = color.RGBA{R: 0x78, G: 0x82, B: 0x9c, A: 0xff}
)

// SocialCard renders a 1200x630 Open Graph card: the canvas centred and
// scaled to fit with a margin, on a dark background, with the room name and a
// subtitle drawn beneath it. Both strings are drawn with a built-in 5x7 bitmap
// font; characters outside the font fall back to a blank box, so pass ASCII.
//
// An empty name or subtitle is simply not drawn, and the canvas grows into the
// space it would have taken.
func SocialCard(w io.Writer, pixels []byte, width, height int, palette []string, name, subtitle string) error {
	if w == nil {
		return errors.New("render: nil writer")
	}
	if err := checkGrid(pixels, width, height); err != nil {
		return err
	}

	// Reserve only the vertical space the text actually needs, then give the
	// rest to the canvas.
	textHeight := 0
	if name != "" {
		textHeight += glyphHeight * cardNameScale
	}
	if subtitle != "" {
		if textHeight > 0 {
			textHeight += cardLineGap
		}
		textHeight += glyphHeight * cardSubtitleScale
	}

	areaWidth := cardWidth - 2*cardMargin
	areaTop := cardMargin
	areaBottom := cardHeight - cardMargin
	if textHeight > 0 {
		areaBottom -= textHeight + cardTextGap
	}
	areaHeight := areaBottom - areaTop

	art, err := fitCanvas(pixels, width, height, palette, areaWidth, areaHeight)
	if err != nil {
		return err
	}

	card := image.NewRGBA(image.Rect(0, 0, cardWidth, cardHeight))
	fillRect(card, card.Bounds(), cardBackground)

	artWidth, artHeight := art.Bounds().Dx(), art.Bounds().Dy()
	at := image.Rect(0, 0, artWidth, artHeight).Add(image.Pt(
		(cardWidth-artWidth)/2,
		areaTop+(areaHeight-artHeight)/2,
	))
	draw.Draw(card, at, art, art.Bounds().Min, draw.Src)
	// The border sits just outside the canvas so it never covers a painted cell.
	strokeRect(card, at.Inset(-1), cardBorder)

	y := cardHeight - cardMargin - textHeight
	if name != "" {
		s := fitText(name, cardNameScale, areaWidth)
		drawText(card, (cardWidth-textWidth(s, cardNameScale))/2, y, s, cardNameScale, cardNameInk)
		y += glyphHeight*cardNameScale + cardLineGap
	}
	if subtitle != "" {
		s := fitText(subtitle, cardSubtitleScale, areaWidth)
		drawText(card, (cardWidth-textWidth(s, cardSubtitleScale))/2, y, s, cardSubtitleScale, cardSubtleInk)
	}

	if err := png.Encode(w, card); err != nil {
		return fmt.Errorf("render: encoding png: %w", err)
	}
	return nil
}

// fitCanvas renders the grid as large as it will go inside a maxWidth by
// maxHeight box. The expected path is the largest integer upscale that fits,
// which is what keeps the pixels crisp. A canvas too big to fit even at 1:1 is
// point sampled down by an integer step instead: still nearest neighbour and
// still no blending, but the whole grid stays visible rather than being cropped
// to whatever happened to be in the middle.
func fitCanvas(pixels []byte, width, height int, palette []string, maxWidth, maxHeight int) (*image.RGBA, error) {
	if err := checkGrid(pixels, width, height); err != nil {
		return nil, err
	}
	maxWidth = max(maxWidth, 1)
	maxHeight = max(maxHeight, 1)

	if scale := min(maxWidth/width, maxHeight/height); scale >= 1 {
		return CanvasImage(pixels, width, height, palette, scale)
	}

	step := max(ceilDiv(width, maxWidth), ceilDiv(height, maxHeight))
	table := newColourTable(palette)
	outWidth, outHeight := ceilDiv(width, step), ceilDiv(height, step)

	img := image.NewRGBA(image.Rect(0, 0, outWidth, outHeight))
	for y := 0; y < outHeight; y++ {
		src := y * step * width
		off := img.PixOffset(0, y)
		for x := 0; x < outWidth; x++ {
			c := table.at(pixels[src+x*step])
			o := off + x*4
			img.Pix[o], img.Pix[o+1], img.Pix[o+2], img.Pix[o+3] = c.R, c.G, c.B, c.A
		}
	}
	return img, nil
}

func ceilDiv(a, b int) int { return (a + b - 1) / b }

// fillRect paints a solid rectangle. draw.Draw clips to the destination, so a
// rectangle that falls off the image is simply not drawn, which is what lets
// the text and border drawing below stay total.
func fillRect(dst *image.RGBA, r image.Rectangle, c color.Color) {
	draw.Draw(dst, r, image.NewUniform(c), image.Point{}, draw.Src)
}

// strokeRect paints a one pixel outline around r.
func strokeRect(dst *image.RGBA, r image.Rectangle, c color.Color) {
	fillRect(dst, image.Rect(r.Min.X, r.Min.Y, r.Max.X, r.Min.Y+1), c)
	fillRect(dst, image.Rect(r.Min.X, r.Max.Y-1, r.Max.X, r.Max.Y), c)
	fillRect(dst, image.Rect(r.Min.X, r.Min.Y, r.Min.X+1, r.Max.Y), c)
	fillRect(dst, image.Rect(r.Max.X-1, r.Min.Y, r.Max.X, r.Max.Y), c)
}

// The bitmap font. Each glyph is five pixels wide and seven tall, one row per
// entry, and within a row bit 4 is the leftmost pixel. Lowercase letters are
// drawn with the uppercase glyphs: at card sizes a separate lowercase set with
// real descenders buys very little and doubles the table.
const (
	glyphWidth  = 5
	glyphHeight = 7
	// glyphAdvance leaves one blank column between characters.
	glyphAdvance = glyphWidth + 1
)

type glyph [glyphHeight]uint8

// missingGlyph is drawn for any rune the table does not cover, so an unexpected
// character shows up as an obvious empty box instead of a hole in the string.
var missingGlyph = glyph{0b11111, 0b10001, 0b10001, 0b10001, 0b10001, 0b10001, 0b11111}

var glyphs = map[rune]glyph{
	' ': {},

	'A': {0b01110, 0b10001, 0b10001, 0b11111, 0b10001, 0b10001, 0b10001},
	'B': {0b11110, 0b10001, 0b10001, 0b11110, 0b10001, 0b10001, 0b11110},
	'C': {0b01110, 0b10001, 0b10000, 0b10000, 0b10000, 0b10001, 0b01110},
	'D': {0b11110, 0b10001, 0b10001, 0b10001, 0b10001, 0b10001, 0b11110},
	'E': {0b11111, 0b10000, 0b10000, 0b11110, 0b10000, 0b10000, 0b11111},
	'F': {0b11111, 0b10000, 0b10000, 0b11110, 0b10000, 0b10000, 0b10000},
	'G': {0b01110, 0b10001, 0b10000, 0b10111, 0b10001, 0b10001, 0b01111},
	'H': {0b10001, 0b10001, 0b10001, 0b11111, 0b10001, 0b10001, 0b10001},
	'I': {0b11111, 0b00100, 0b00100, 0b00100, 0b00100, 0b00100, 0b11111},
	'J': {0b00111, 0b00010, 0b00010, 0b00010, 0b00010, 0b10010, 0b01100},
	'K': {0b10001, 0b10010, 0b10100, 0b11000, 0b10100, 0b10010, 0b10001},
	'L': {0b10000, 0b10000, 0b10000, 0b10000, 0b10000, 0b10000, 0b11111},
	'M': {0b10001, 0b11011, 0b10101, 0b10101, 0b10001, 0b10001, 0b10001},
	'N': {0b10001, 0b11001, 0b10101, 0b10011, 0b10001, 0b10001, 0b10001},
	'O': {0b01110, 0b10001, 0b10001, 0b10001, 0b10001, 0b10001, 0b01110},
	'P': {0b11110, 0b10001, 0b10001, 0b11110, 0b10000, 0b10000, 0b10000},
	'Q': {0b01110, 0b10001, 0b10001, 0b10001, 0b10101, 0b10010, 0b01101},
	'R': {0b11110, 0b10001, 0b10001, 0b11110, 0b10100, 0b10010, 0b10001},
	'S': {0b01111, 0b10000, 0b10000, 0b01110, 0b00001, 0b00001, 0b11110},
	'T': {0b11111, 0b00100, 0b00100, 0b00100, 0b00100, 0b00100, 0b00100},
	'U': {0b10001, 0b10001, 0b10001, 0b10001, 0b10001, 0b10001, 0b01110},
	'V': {0b10001, 0b10001, 0b10001, 0b10001, 0b10001, 0b01010, 0b00100},
	'W': {0b10001, 0b10001, 0b10001, 0b10101, 0b10101, 0b11011, 0b10001},
	'X': {0b10001, 0b10001, 0b01010, 0b00100, 0b01010, 0b10001, 0b10001},
	'Y': {0b10001, 0b10001, 0b01010, 0b00100, 0b00100, 0b00100, 0b00100},
	'Z': {0b11111, 0b00001, 0b00010, 0b00100, 0b01000, 0b10000, 0b11111},

	'0': {0b01110, 0b10001, 0b10011, 0b10101, 0b11001, 0b10001, 0b01110},
	'1': {0b00100, 0b01100, 0b00100, 0b00100, 0b00100, 0b00100, 0b01110},
	'2': {0b01110, 0b10001, 0b00001, 0b00010, 0b00100, 0b01000, 0b11111},
	'3': {0b11111, 0b00010, 0b00100, 0b00010, 0b00001, 0b10001, 0b01110},
	'4': {0b00010, 0b00110, 0b01010, 0b10010, 0b11111, 0b00010, 0b00010},
	'5': {0b11111, 0b10000, 0b11110, 0b00001, 0b00001, 0b10001, 0b01110},
	'6': {0b00110, 0b01000, 0b10000, 0b11110, 0b10001, 0b10001, 0b01110},
	'7': {0b11111, 0b00001, 0b00010, 0b00100, 0b01000, 0b01000, 0b01000},
	'8': {0b01110, 0b10001, 0b10001, 0b01110, 0b10001, 0b10001, 0b01110},
	'9': {0b01110, 0b10001, 0b10001, 0b01111, 0b00001, 0b00010, 0b01100},

	'.':  {0, 0, 0, 0, 0, 0b00110, 0b00110},
	',':  {0, 0, 0, 0, 0b00110, 0b00110, 0b01100},
	':':  {0, 0b00110, 0b00110, 0, 0b00110, 0b00110, 0},
	';':  {0, 0b00110, 0b00110, 0, 0b00110, 0b00110, 0b01100},
	'!':  {0b00100, 0b00100, 0b00100, 0b00100, 0b00100, 0, 0b00100},
	'?':  {0b01110, 0b10001, 0b00001, 0b00010, 0b00100, 0, 0b00100},
	'\'': {0b00100, 0b00100, 0, 0, 0, 0, 0},
	'"':  {0b01010, 0b01010, 0, 0, 0, 0, 0},
	'-':  {0, 0, 0, 0b01110, 0, 0, 0},
	'_':  {0, 0, 0, 0, 0, 0, 0b11111},
	'/':  {0b00001, 0b00010, 0b00010, 0b00100, 0b01000, 0b01000, 0b10000},
	'(':  {0b00010, 0b00100, 0b01000, 0b01000, 0b01000, 0b00100, 0b00010},
	')':  {0b01000, 0b00100, 0b00010, 0b00010, 0b00010, 0b00100, 0b01000},
	'#':  {0b01010, 0b01010, 0b11111, 0b01010, 0b11111, 0b01010, 0b01010},
	'&':  {0b01100, 0b10010, 0b10010, 0b01100, 0b10101, 0b10010, 0b01101},
	'+':  {0, 0b00100, 0b00100, 0b11111, 0b00100, 0b00100, 0},
	'×':  {0, 0b10001, 0b01010, 0b00100, 0b01010, 0b10001, 0},
}

// lookupGlyph never fails, which is what keeps drawText total for any input.
func lookupGlyph(r rune) glyph {
	if r >= 'a' && r <= 'z' {
		r -= 'a' - 'A'
	}
	if g, ok := glyphs[r]; ok {
		return g
	}
	return missingGlyph
}

// textWidth reports how wide s renders, excluding the trailing gap after the
// last character.
func textWidth(s string, scale int) int {
	n := utf8.RuneCountInString(s)
	if n == 0 {
		return 0
	}
	return (n*glyphAdvance - 1) * max(scale, 1)
}

// fitText truncates s with an ellipsis so it never renders wider than maxWidth.
// Text running off the edge of a card looks like a bug; a clipped name that
// ends in an ellipsis looks intended.
func fitText(s string, scale, maxWidth int) string {
	if textWidth(s, scale) <= maxWidth {
		return s
	}
	const ellipsis = "..."
	runes := []rune(s)
	for n := len(runes) - 1; n >= 0; n-- {
		// Trailing spaces come off first, so a cut that lands on a word boundary
		// reads as "long name..." rather than leaving a gap before the dots.
		candidate := strings.TrimRight(string(runes[:n]), " ") + ellipsis
		if textWidth(candidate, scale) <= maxWidth {
			return candidate
		}
	}
	// Not even the ellipsis fits, so there is nothing honest left to draw.
	return ""
}

// drawText draws s with the top left corner of its first glyph at (x, y). Only
// the set bits are painted, so whatever is underneath shows through the gaps.
func drawText(dst *image.RGBA, x, y int, s string, scale int, c color.Color) {
	scale = max(scale, 1)
	for _, r := range s {
		g := lookupGlyph(r)
		for row := 0; row < glyphHeight; row++ {
			bits := g[row]
			if bits == 0 {
				continue
			}
			for col := 0; col < glyphWidth; col++ {
				if bits&(1<<(glyphWidth-1-col)) == 0 {
					continue
				}
				px, py := x+col*scale, y+row*scale
				fillRect(dst, image.Rect(px, py, px+scale, py+scale), c)
			}
		}
		x += glyphAdvance * scale
	}
}
