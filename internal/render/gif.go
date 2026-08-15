// Package render turns a canvas and its history into something shareable: a PNG
// of the current state, an Open Graph card for link previews, and an animated
// GIF time-lapse. All of it comes out of image/png and image/gif in the
// standard library, like everything else in Pixelforge.
package render

import (
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"io"
	"strconv"
	"strings"
)

// Defaults and limits for TimelapseOptions. The upper bounds are not arbitrary:
// a GIF with a thousand frames or a sixteen-times zoom on a large canvas is a
// file nobody wants to download, and the point of the export is to be shared.
const (
	defaultFrames = 60
	minFrames     = 2
	maxFrames     = 300

	defaultDelayMs = 80
	minDelayMs     = 20
	maxDelayMs     = 1000

	defaultHoldMs = 1500
	maxHoldMs     = 30000

	// minDelayCs is the floor in hundredths of a second. Zero means "as fast as
	// the viewer can manage", which reads as a flicker rather than an animation.
	minDelayCs = 2

	// targetLongEdge is what the default Scale aims for, in output pixels.
	targetLongEdge = 480
	maxScale       = 16

	defaultMaxOutputPx = 1 << 21

	// maxPaletteEntries is a hard limit of the GIF format: one colour table
	// index per pixel, eight bits wide.
	maxPaletteEntries = 256

	// maxCanvasCells guards the allocations below against a nonsense request and
	// keeps Width*Height clear of overflowing an int on a 32-bit build.
	maxCanvasCells = 1 << 24
)

// Placement is one pixel write in history order.
type Placement struct {
	X, Y  int
	Color uint8 // index into the palette
}

// TimelapseOptions tunes the encoder. Only Width, Height and Palette must be
// set; every other field takes a default and is clamped to a range that
// produces a GIF worth looking at.
type TimelapseOptions struct {
	// Width and Height are the canvas dimensions in cells.
	Width, Height int
	// Palette holds "#rrggbb" strings, index 0 being the background. GIF allows
	// at most 256 entries.
	Palette []string
	// Frames is the target number of frames. Default 60, clamped to 2..300.
	Frames int
	// DelayMs is the per-frame delay in milliseconds. Default 80, clamped to
	// 20..1000.
	DelayMs int
	// HoldMs is extra delay on the final frame so the finished picture can be
	// read before the loop restarts. Default 1500; pass a negative value to ask
	// for no hold at all.
	HoldMs int
	// Scale upscales every cell by an integer factor, nearest neighbour, so the
	// pixels stay crisp. The default is the largest factor keeping the long edge
	// at or under 480 output pixels. Clamped to 1..16.
	Scale int
	// MaxOutputPx caps width*height of the encoded image. Default 1<<21. Scale is
	// reduced until a frame fits.
	MaxOutputPx int
}

// withDefaults fills in the zero fields and clamps the rest. It must be called
// after the dimensions have been validated, since the default Scale divides by
// the long edge.
func (o TimelapseOptions) withDefaults() TimelapseOptions {
	out := o

	if out.Frames <= 0 {
		out.Frames = defaultFrames
	}
	out.Frames = min(max(out.Frames, minFrames), maxFrames)

	if out.DelayMs <= 0 {
		out.DelayMs = defaultDelayMs
	}
	out.DelayMs = min(max(out.DelayMs, minDelayMs), maxDelayMs)

	// Zero means "unset" and gets the default; a negative value is how a caller
	// asks for no hold at all, which is otherwise unsayable.
	switch {
	case out.HoldMs < 0:
		out.HoldMs = 0
	case out.HoldMs == 0:
		out.HoldMs = defaultHoldMs
	default:
		out.HoldMs = min(out.HoldMs, maxHoldMs)
	}

	if out.MaxOutputPx <= 0 {
		out.MaxOutputPx = defaultMaxOutputPx
	}

	out.Scale = resolveScale(out)
	return out
}

// resolveScale picks the zoom factor and then shrinks it until the frame fits
// the pixel budget.
func resolveScale(o TimelapseOptions) int {
	scale := o.Scale
	if scale <= 0 {
		// Integer division on purpose: the largest whole-number zoom that stays
		// at or under the target. A canvas already bigger than the target lands
		// on zero here and is clamped up to 1:1 below.
		scale = targetLongEdge / max(o.Width, o.Height)
	}
	scale = min(max(scale, 1), maxScale)

	// One is the floor. Going below it would mean discarding cells, and a
	// time-lapse that cannot show every pixel is worse than a large file.
	for scale > 1 && outputPixels(o.Width, o.Height, scale) > int64(o.MaxOutputPx) {
		scale--
	}
	return scale
}

// outputPixels counts the pixels in one encoded frame. It works in int64 so a
// large canvas and a large scale cannot silently wrap.
func outputPixels(w, h, scale int) int64 {
	return int64(w) * int64(scale) * int64(h) * int64(scale)
}

// EncodeTimelapse writes an animated GIF replaying the placements from an empty
// canvas to the final state. Placements must be in history order.
//
// The animation has min(opts.Frames, len(placements)) frames, each showing the
// cumulative canvas at that point, or a single background frame when there is
// nothing to replay. A placement that falls outside the canvas or names a
// colour the palette does not have is skipped rather than failing the encode:
// history read back from an older or differently sized canvas should still
// produce something watchable.
func EncodeTimelapse(w io.Writer, placements []Placement, opts TimelapseOptions) error {
	if w == nil {
		return errors.New("render: nil writer")
	}
	if opts.Width <= 0 || opts.Height <= 0 {
		return fmt.Errorf("render: canvas dimensions must be positive, got %dx%d", opts.Width, opts.Height)
	}
	if int64(opts.Width)*int64(opts.Height) > maxCanvasCells {
		return fmt.Errorf("render: canvas of %dx%d cells is too large to encode", opts.Width, opts.Height)
	}
	if len(opts.Palette) == 0 {
		return errors.New("render: palette is empty")
	}
	if len(opts.Palette) > maxPaletteEntries {
		return fmt.Errorf("render: palette has %d entries, GIF allows at most %d", len(opts.Palette), maxPaletteEntries)
	}
	pal, err := buildPalette(opts.Palette)
	if err != nil {
		return err
	}

	o := opts.withDefaults()

	frames := o.Frames
	if len(placements) < frames {
		// Fewer placements than frames would mean frames that change nothing, so
		// give each placement its own instead.
		frames = len(placements)
	}
	if frames < 1 {
		// Nothing to replay. A single background frame is friendlier than an
		// error: an untouched canvas is a legitimate thing to export.
		frames = 1
	}

	rect := image.Rect(0, 0, o.Width*o.Scale, o.Height*o.Scale)

	// cells is the live canvas, advanced in place as the history is consumed.
	// Replaying from the start for every frame would make the encode quadratic
	// in the length of the history, which for a busy canvas is millions of
	// placements.
	cells := make([]uint8, o.Width*o.Height)

	out := gif.GIF{
		Image: make([]*image.Paletted, 0, frames),
		Delay: make([]int, 0, frames),
		// Zero means loop forever.
		LoopCount: 0,
		// Naming the palette here puts it in the global colour table, so the
		// frames share one copy of it. Every frame is already in palette indices
		// and no quantiser is involved anywhere in this file.
		Config: image.Config{
			ColorModel: pal,
			Width:      rect.Dx(),
			Height:     rect.Dy(),
		},
	}

	delay := max(csFromMs(o.DelayMs), minDelayCs)
	cursor := 0
	for i := 0; i < frames; i++ {
		// Frame i ends at n*(i+1)/frames, which shares the remainder out across
		// the animation rather than dumping it on the last frame.
		end := len(placements) * (i + 1) / frames
		for ; cursor < end; cursor++ {
			p := placements[cursor]
			if p.X < 0 || p.Y < 0 || p.X >= o.Width || p.Y >= o.Height || int(p.Color) >= len(pal) {
				continue
			}
			cells[p.Y*o.Width+p.X] = p.Color
		}

		frame := image.NewPaletted(rect, pal)
		scaleInto(frame, cells, o.Width, o.Height, o.Scale)
		out.Image = append(out.Image, frame)
		out.Delay = append(out.Delay, delay)
	}

	// Hold the finished picture. The clamps above keep this well inside the
	// uint16 the GIF delay field actually is.
	out.Delay[len(out.Delay)-1] = delay + csFromMs(o.HoldMs)

	return gif.EncodeAll(w, &out)
}

// scaleInto expands the cell grid into a frame by nearest-neighbour repetition,
// which for an integer factor is an exact block copy: no interpolation, so the
// pixel edges stay hard.
func scaleInto(dst *image.Paletted, cells []uint8, w, h, scale int) {
	if scale == 1 {
		// The stride equals the width, so the grid maps onto Pix one to one.
		copy(dst.Pix, cells)
		return
	}
	rowLen := w * scale
	for y := 0; y < h; y++ {
		top := y * scale * dst.Stride
		row := dst.Pix[top : top+rowLen]
		for x, c := range cells[y*w : (y+1)*w] {
			block := row[x*scale : (x+1)*scale]
			for i := range block {
				block[i] = c
			}
		}
		// The rest of the block is the row again, so copy it rather than walking
		// the cells a second time.
		for dy := 1; dy < scale; dy++ {
			off := top + dy*dst.Stride
			copy(dst.Pix[off:off+rowLen], row)
		}
	}
}

// csFromMs converts milliseconds to the hundredths of a second GIF stores,
// rounding to nearest.
func csFromMs(ms int) int {
	if ms <= 0 {
		return 0
	}
	return (ms + 5) / 10
}

// buildPalette converts the hex strings into a colour table. The canvas holds
// palette indices rather than colours, so this table is the whole mapping and
// it is used unchanged for every frame.
func buildPalette(hexes []string) (color.Palette, error) {
	pal := make(color.Palette, len(hexes))
	for i, h := range hexes {
		c, err := parseHexColour(h)
		if err != nil {
			return nil, fmt.Errorf("render: palette entry %d: %w", i, err)
		}
		pal[i] = c
	}
	return pal, nil
}

// parseHexColour accepts "#rrggbb", and the same without the hash for the
// benefit of callers reading colours out of a config file.
func parseHexColour(s string) (color.RGBA, error) {
	digits := strings.TrimPrefix(s, "#")
	if len(digits) != 6 {
		return color.RGBA{}, fmt.Errorf("%q is not a #rrggbb colour", s)
	}
	v, err := strconv.ParseUint(digits, 16, 32)
	if err != nil {
		return color.RGBA{}, fmt.Errorf("%q is not a #rrggbb colour", s)
	}
	return color.RGBA{R: uint8(v >> 16), G: uint8(v >> 8), B: uint8(v), A: 0xff}, nil
}
