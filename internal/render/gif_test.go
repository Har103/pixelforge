package render

import (
	"bytes"
	"image/gif"
	"strings"
	"testing"
)

// testPalette mirrors the shape of the real canvas palette: index 0 is the
// background, the rest are paintable.
func testPalette() []string {
	return []string{"#12141c", "#ffffff", "#ff4d6d", "#3a86ff", "#ffd60a"}
}

// linePlacements fills the canvas in reading order, one cell per placement, so
// the number of painted cells in a frame equals the number of placements it has
// consumed.
func linePlacements(n, width, palette int) []Placement {
	out := make([]Placement, n)
	for i := range out {
		out[i] = Placement{
			X:     i % width,
			Y:     i / width,
			Color: uint8(1 + i%(palette-1)),
		}
	}
	return out
}

func encode(t *testing.T, placements []Placement, opts TimelapseOptions) *gif.GIF {
	t.Helper()
	var buf bytes.Buffer
	if err := EncodeTimelapse(&buf, placements, opts); err != nil {
		t.Fatalf("EncodeTimelapse: %v", err)
	}
	g, err := gif.DecodeAll(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("DecodeAll on %d bytes: %v", buf.Len(), err)
	}
	return g
}

// finalCanvas is the state the last frame must agree with, computed the slow
// obvious way so it is independent of the encoder's incremental buffer.
func finalCanvas(placements []Placement, width, height, palette int) []uint8 {
	cells := make([]uint8, width*height)
	for _, p := range placements {
		if p.X < 0 || p.Y < 0 || p.X >= width || p.Y >= height || int(p.Color) >= palette {
			continue
		}
		cells[p.Y*width+p.X] = p.Color
	}
	return cells
}

func TestEncodeTimelapseDecodes(t *testing.T) {
	pal := testPalette()
	g := encode(t, linePlacements(40, 8, len(pal)), TimelapseOptions{
		Width:   8,
		Height:  8,
		Palette: pal,
		Frames:  5,
		DelayMs: 100,
		HoldMs:  1000,
		Scale:   2,
	})

	if len(g.Image) != 5 {
		t.Fatalf("frames = %d, want 5", len(g.Image))
	}
	if len(g.Delay) != len(g.Image) {
		t.Fatalf("%d delays for %d frames", len(g.Delay), len(g.Image))
	}
	if g.LoopCount != 0 {
		t.Errorf("loop count = %d, want 0 (forever)", g.LoopCount)
	}
	if g.Config.Width != 16 || g.Config.Height != 16 {
		t.Errorf("config = %dx%d, want 16x16", g.Config.Width, g.Config.Height)
	}

	for i, img := range g.Image {
		if b := img.Bounds(); b.Dx() != 16 || b.Dy() != 16 {
			t.Errorf("frame %d bounds = %v, want 16x16", i, b)
		}
		// A single global palette is the whole point: no frame may re-quantise.
		if len(img.Palette) != len(g.Image[0].Palette) {
			t.Errorf("frame %d has %d palette entries, frame 0 has %d",
				i, len(img.Palette), len(g.Image[0].Palette))
		}
		for j := range g.Image[0].Palette {
			if img.Palette[j] != g.Image[0].Palette[j] {
				t.Errorf("frame %d palette entry %d differs from frame 0", i, j)
				break
			}
		}
	}

	// 100ms is 10 hundredths, and the final frame carries the extra second.
	for i, d := range g.Delay[:len(g.Delay)-1] {
		if d != 10 {
			t.Errorf("delay[%d] = %d, want 10", i, d)
		}
	}
	if last := g.Delay[len(g.Delay)-1]; last != 10+100 {
		t.Errorf("final delay = %d, want %d", last, 10+100)
	}
}

func TestLastFrameMatchesFinalCanvasState(t *testing.T) {
	const w, h, scale = 7, 5, 3
	pal := testPalette()
	placements := linePlacements(w*h, w, len(pal))
	// Repaint a few cells so the last write wins rather than the first.
	placements = append(placements,
		Placement{X: 0, Y: 0, Color: 4},
		Placement{X: 6, Y: 4, Color: 2},
		Placement{X: 3, Y: 2, Color: 0},
	)

	g := encode(t, placements, TimelapseOptions{
		Width: w, Height: h, Palette: pal, Frames: 6, Scale: scale,
	})

	want := finalCanvas(placements, w, h, len(pal))
	last := g.Image[len(g.Image)-1]
	if b := last.Bounds(); b.Dx() != w*scale || b.Dy() != h*scale {
		t.Fatalf("bounds = %v, want %dx%d", b, w*scale, h*scale)
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			// Every pixel of the scale*scale block must carry the cell's index,
			// which is what nearest-neighbour upscaling means here.
			for dy := 0; dy < scale; dy++ {
				for dx := 0; dx < scale; dx++ {
					got := last.ColorIndexAt(x*scale+dx, y*scale+dy)
					if got != want[y*w+x] {
						t.Fatalf("cell (%d,%d) offset (%d,%d) = %d, want %d",
							x, y, dx, dy, got, want[y*w+x])
					}
				}
			}
		}
	}
}

func TestPlacementsAreSpreadEvenlyAcrossFrames(t *testing.T) {
	pal := testPalette()
	// Ten placements over four frames cannot divide evenly; the remainder should
	// be shared out rather than landing on the final frame.
	g := encode(t, linePlacements(10, 8, len(pal)), TimelapseOptions{
		Width: 8, Height: 8, Palette: pal, Frames: 4, Scale: 1,
	})

	if len(g.Image) != 4 {
		t.Fatalf("frames = %d, want 4", len(g.Image))
	}
	want := []int{2, 5, 7, 10}
	for i, img := range g.Image {
		painted := 0
		for _, px := range img.Pix {
			if px != 0 {
				painted++
			}
		}
		if painted != want[i] {
			t.Errorf("frame %d has %d painted cells, want %d", i, painted, want[i])
		}
	}
}

func TestFewerPlacementsThanFramesGivesOneFrameEach(t *testing.T) {
	pal := testPalette()
	g := encode(t, linePlacements(3, 8, len(pal)), TimelapseOptions{
		Width: 8, Height: 8, Palette: pal, Frames: 60, Scale: 1,
	})
	if len(g.Image) != 3 {
		t.Fatalf("frames = %d, want 3 (one per placement)", len(g.Image))
	}
	for i, img := range g.Image {
		painted := 0
		for _, px := range img.Pix {
			if px != 0 {
				painted++
			}
		}
		if painted != i+1 {
			t.Errorf("frame %d has %d painted cells, want %d", i, painted, i+1)
		}
	}
}

func TestOutOfRangePlacementsAreSkipped(t *testing.T) {
	const w, h = 4, 4
	pal := testPalette()
	placements := []Placement{
		{X: -1, Y: 0, Color: 1},
		{X: 0, Y: -1, Color: 1},
		{X: w, Y: 0, Color: 1},
		{X: 0, Y: h, Color: 1},
		{X: 999, Y: 999, Color: 1},
		{X: 2, Y: 2, Color: uint8(len(pal))}, // colour past the end of the palette
		{X: 1, Y: 1, Color: 200},
		{X: 3, Y: 0, Color: 2}, // the only valid one
	}

	g := encode(t, placements, TimelapseOptions{
		Width: w, Height: h, Palette: pal, Frames: 4, Scale: 1,
	})

	last := g.Image[len(g.Image)-1]
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			want := uint8(0)
			if x == 3 && y == 0 {
				want = 2
			}
			if got := last.ColorIndexAt(x, y); got != want {
				t.Errorf("cell (%d,%d) = %d, want %d", x, y, got, want)
			}
		}
	}
}

func TestEmptyHistoryProducesOneBackgroundFrame(t *testing.T) {
	pal := testPalette()
	for _, placements := range [][]Placement{nil, {}} {
		g := encode(t, placements, TimelapseOptions{
			Width: 6, Height: 4, Palette: pal, Scale: 2,
		})
		if len(g.Image) != 1 {
			t.Fatalf("frames = %d, want 1", len(g.Image))
		}
		if b := g.Image[0].Bounds(); b.Dx() != 12 || b.Dy() != 8 {
			t.Errorf("bounds = %v, want 12x8", b)
		}
		for i, px := range g.Image[0].Pix {
			if px != 0 {
				t.Fatalf("pixel %d = %d, want the background index", i, px)
			}
		}
		// Even a still frame must not claim a zero delay.
		if g.Delay[0] < minDelayCs {
			t.Errorf("delay = %d, want at least %d", g.Delay[0], minDelayCs)
		}
	}
}

func TestMaxOutputPxReducesScale(t *testing.T) {
	pal := testPalette()
	placements := linePlacements(50, 100, len(pal))

	// 100x100 cells at the requested scale of 8 would be 640000 pixels. The cap
	// allows 100000, so the largest scale that fits is 3 (90000).
	g := encode(t, placements, TimelapseOptions{
		Width: 100, Height: 100, Palette: pal, Frames: 3,
		Scale: 8, MaxOutputPx: 100000,
	})
	if b := g.Image[0].Bounds(); b.Dx() != 300 || b.Dy() != 300 {
		t.Errorf("bounds = %v, want 300x300 after the cap reduced the scale", b)
	}

	// Without the cap the requested scale stands.
	g = encode(t, placements, TimelapseOptions{
		Width: 100, Height: 100, Palette: pal, Frames: 3, Scale: 8,
	})
	if b := g.Image[0].Bounds(); b.Dx() != 800 || b.Dy() != 800 {
		t.Errorf("bounds = %v, want 800x800", b)
	}

	// The cap can never push the scale below 1:1, because that would mean
	// dropping cells from the replay.
	g = encode(t, placements, TimelapseOptions{
		Width: 100, Height: 100, Palette: pal, Frames: 3, Scale: 8, MaxOutputPx: 1,
	})
	if b := g.Image[0].Bounds(); b.Dx() != 100 || b.Dy() != 100 {
		t.Errorf("bounds = %v, want 100x100", b)
	}
}

func TestDefaultScaleTargetsTheLongEdge(t *testing.T) {
	pal := testPalette()
	cases := []struct {
		w, h           int
		wantDx, wantDy int
	}{
		{32, 32, 480, 480},   // 480/32 = 15
		{60, 30, 480, 240},   // driven by the long edge, aspect preserved
		{100, 100, 400, 400}, // 480/100 truncates to 4
		{7, 7, 112, 112},     // 480/7 = 68, clamped to the maximum scale of 16
		{600, 600, 600, 600}, // already past the target, so 1:1
	}
	for _, tc := range cases {
		g := encode(t, linePlacements(4, tc.w, len(pal)), TimelapseOptions{
			Width: tc.w, Height: tc.h, Palette: pal,
		})
		b := g.Image[0].Bounds()
		if b.Dx() != tc.wantDx || b.Dy() != tc.wantDy {
			t.Errorf("%dx%d canvas rendered %v, want %dx%d",
				tc.w, tc.h, b, tc.wantDx, tc.wantDy)
		}
	}
}

func TestDelaysAreClampedAndNeverZero(t *testing.T) {
	pal := testPalette()
	placements := linePlacements(8, 8, len(pal))
	base := TimelapseOptions{Width: 8, Height: 8, Palette: pal, Frames: 4, Scale: 1}

	// A silly-fast request is clamped to 20ms, which is the 2 hundredths floor.
	fast := base
	fast.DelayMs = 1
	g := encode(t, placements, fast)
	if g.Delay[0] != minDelayCs {
		t.Errorf("delay = %d, want %d", g.Delay[0], minDelayCs)
	}

	// The default is 80ms, and the default hold is 1500ms on top of it.
	g = encode(t, placements, base)
	if g.Delay[0] != 8 {
		t.Errorf("default delay = %d, want 8", g.Delay[0])
	}
	if last := g.Delay[len(g.Delay)-1]; last != 8+150 {
		t.Errorf("final delay = %d, want %d", last, 8+150)
	}

	// A negative hold is how a caller opts out of holding at all.
	noHold := base
	noHold.HoldMs = -1
	g = encode(t, placements, noHold)
	if last := g.Delay[len(g.Delay)-1]; last != g.Delay[0] {
		t.Errorf("final delay = %d, want the ordinary %d", last, g.Delay[0])
	}

	// An absurd hold is capped rather than overflowing the GIF's uint16 field.
	long := base
	long.HoldMs = 1 << 20
	g = encode(t, placements, long)
	if last := g.Delay[len(g.Delay)-1]; last != 8+maxHoldMs/10 {
		t.Errorf("final delay = %d, want %d", last, 8+maxHoldMs/10)
	}
}

func TestFrameCountIsClamped(t *testing.T) {
	pal := testPalette()
	placements := linePlacements(1000, 100, len(pal))

	g := encode(t, placements, TimelapseOptions{
		Width: 100, Height: 100, Palette: pal, Frames: 100000, Scale: 1,
	})
	if len(g.Image) != maxFrames {
		t.Errorf("frames = %d, want the maximum of %d", len(g.Image), maxFrames)
	}

	g = encode(t, placements, TimelapseOptions{
		Width: 100, Height: 100, Palette: pal, Frames: 1, Scale: 1,
	})
	if len(g.Image) != minFrames {
		t.Errorf("frames = %d, want the minimum of %d", len(g.Image), minFrames)
	}
}

func TestEncodeTimelapseRejectsBadInput(t *testing.T) {
	pal := testPalette()
	ok := TimelapseOptions{Width: 4, Height: 4, Palette: pal}

	tooMany := make([]string, 257)
	for i := range tooMany {
		tooMany[i] = "#ffffff"
	}

	cases := []struct {
		name string
		w    *bytes.Buffer
		opts TimelapseOptions
		want string
	}{
		{"nil writer", nil, ok, "nil writer"},
		{"zero width", &bytes.Buffer{}, TimelapseOptions{Width: 0, Height: 4, Palette: pal}, "dimensions"},
		{"negative height", &bytes.Buffer{}, TimelapseOptions{Width: 4, Height: -1, Palette: pal}, "dimensions"},
		{"empty palette", &bytes.Buffer{}, TimelapseOptions{Width: 4, Height: 4}, "palette is empty"},
		{"oversized palette", &bytes.Buffer{}, TimelapseOptions{Width: 4, Height: 4, Palette: tooMany}, "at most 256"},
		{"absurd canvas", &bytes.Buffer{}, TimelapseOptions{Width: 1 << 20, Height: 1 << 20, Palette: pal}, "too large"},
		{"malformed colour", &bytes.Buffer{}, TimelapseOptions{Width: 4, Height: 4, Palette: []string{"#12141c", "wrong"}}, "palette entry 1"},
		{"short colour", &bytes.Buffer{}, TimelapseOptions{Width: 4, Height: 4, Palette: []string{"#fff"}}, "palette entry 0"},
		{"non-hex colour", &bytes.Buffer{}, TimelapseOptions{Width: 4, Height: 4, Palette: []string{"#gggggg"}}, "palette entry 0"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A nil *bytes.Buffer would be a non-nil interface, so the nil writer
			// case has to pass an untyped nil.
			var err error
			if tc.w == nil {
				err = EncodeTimelapse(nil, nil, tc.opts)
			} else {
				err = EncodeTimelapse(tc.w, nil, tc.opts)
			}
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
			if tc.w != nil && tc.w.Len() != 0 {
				t.Errorf("wrote %d bytes despite failing validation", tc.w.Len())
			}
		})
	}
}

func TestPaletteAcceptsColoursWithoutAHash(t *testing.T) {
	g := encode(t, []Placement{{X: 0, Y: 0, Color: 1}}, TimelapseOptions{
		Width: 2, Height: 2, Palette: []string{"12141c", "#ff4d6d"}, Scale: 1,
	})
	r, gr, b, _ := g.Image[0].At(0, 0).RGBA()
	if r>>8 != 0xff || gr>>8 != 0x4d || b>>8 != 0x6d {
		t.Errorf("painted colour = %02x%02x%02x, want ff4d6d", r>>8, gr>>8, b>>8)
	}
}

// TestLongHistoryEncodesInReasonableTime is a smoke test for the incremental
// buffer: half a million placements should encode without the quadratic replay
// that rebuilding the canvas for every frame would cause.
func TestLongHistoryEncodesInReasonableTime(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping the half-million-placement encode in short mode")
	}
	pal := testPalette()
	// Wrap around the canvas so every placement is a real write rather than an
	// out-of-bounds skip.
	placements := make([]Placement, 500_000)
	for i := range placements {
		placements[i] = Placement{
			X:     i % 200,
			Y:     (i / 200) % 200,
			Color: uint8(1 + i%(len(pal)-1)),
		}
	}
	var buf bytes.Buffer
	if err := EncodeTimelapse(&buf, placements, TimelapseOptions{
		Width: 200, Height: 200, Palette: pal, Frames: 120, Scale: 2,
	}); err != nil {
		t.Fatalf("EncodeTimelapse: %v", err)
	}
	if buf.Len() == 0 {
		t.Fatal("encoded nothing")
	}
}
