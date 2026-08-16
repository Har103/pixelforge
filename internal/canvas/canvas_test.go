package canvas

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestPlaceWritesAndAdvancesSequence(t *testing.T) {
	c := New(16, 16, Palette, 0)
	now := time.Now()

	p, err := c.Place(3, 4, 6, "alice", now)
	if err != nil {
		t.Fatalf("place: %v", err)
	}
	if p.Seq != 1 || p.X != 3 || p.Y != 4 || p.Color != 6 || p.UID != "alice" {
		t.Errorf("unexpected pixel: %+v", p)
	}
	pixels, seq := c.Snapshot()
	if seq != 1 {
		t.Errorf("seq = %d, want 1", seq)
	}
	if pixels[4*16+3] != 6 {
		t.Errorf("pixel not written: %d", pixels[4*16+3])
	}
}

func TestPlaceRejectsOutOfBounds(t *testing.T) {
	c := New(8, 8, Palette, 0)
	now := time.Now()
	for _, tc := range [][2]int{{-1, 0}, {0, -1}, {8, 0}, {0, 8}, {99, 99}} {
		if _, err := c.Place(tc[0], tc[1], 1, "u", now); !errors.Is(err, ErrOutOfBounds) {
			t.Errorf("Place(%d,%d) error = %v, want ErrOutOfBounds", tc[0], tc[1], err)
		}
	}
}

func TestPlaceRejectsColourOutsidePalette(t *testing.T) {
	c := New(8, 8, Palette, 0)
	if _, err := c.Place(0, 0, uint8(len(Palette)), "u", time.Now()); !errors.Is(err, ErrBadColour) {
		t.Errorf("error = %v, want ErrBadColour", err)
	}
}

func TestPlaceRejectsRepaintingTheSameColour(t *testing.T) {
	c := New(8, 8, Palette, 0)
	now := time.Now()
	if _, err := c.Place(1, 1, 5, "u", now); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Place(1, 1, 5, "u", now); !errors.Is(err, ErrSameColour) {
		t.Errorf("error = %v, want ErrSameColour", err)
	}
}

func TestCooldownBlocksThenExpires(t *testing.T) {
	c := New(8, 8, Palette, time.Second)
	t0 := time.Now()

	if _, err := c.Place(0, 0, 1, "bob", t0); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Place(1, 0, 2, "bob", t0.Add(400*time.Millisecond)); !errors.Is(err, ErrCooldown) {
		t.Errorf("error = %v, want ErrCooldown", err)
	}
	// A different user is unaffected.
	if _, err := c.Place(2, 0, 3, "carol", t0.Add(400*time.Millisecond)); err != nil {
		t.Errorf("carol should not share bob's cooldown: %v", err)
	}
	if _, err := c.Place(1, 0, 2, "bob", t0.Add(1100*time.Millisecond)); err != nil {
		t.Errorf("cooldown should have expired: %v", err)
	}
}

func TestCooldownRemaining(t *testing.T) {
	c := New(8, 8, Palette, 2*time.Second)
	t0 := time.Now()
	if _, err := c.Place(0, 0, 1, "dave", t0); err != nil {
		t.Fatal(err)
	}
	got := c.CooldownRemaining("dave", t0.Add(500*time.Millisecond))
	if got < 1400*time.Millisecond || got > 1500*time.Millisecond {
		t.Errorf("remaining = %v, want about 1.5s", got)
	}
	if got := c.CooldownRemaining("nobody", t0); got != 0 {
		t.Errorf("unknown user should have no cooldown, got %v", got)
	}
	if got := c.CooldownRemaining("dave", t0.Add(5*time.Second)); got != 0 {
		t.Errorf("expired cooldown should be zero, got %v", got)
	}
}

func TestSnapshotIsACopy(t *testing.T) {
	c := New(4, 4, Palette, 0)
	pixels, _ := c.Snapshot()
	pixels[0] = 99
	fresh, _ := c.Snapshot()
	if fresh[0] == 99 {
		t.Error("mutating a snapshot changed the canvas")
	}
}

func TestLoadRejectsWrongSize(t *testing.T) {
	c := New(4, 4, Palette, 0)
	if err := c.Load(make([]byte, 15), 0); err == nil {
		t.Error("expected an error loading a mis-sized snapshot")
	}
	if err := c.Load(make([]byte, 16), 7); err != nil {
		t.Errorf("correct size should load: %v", err)
	}
	if c.Seq() != 7 {
		t.Errorf("seq = %d, want 7", c.Seq())
	}
}

func TestApplyIgnoresInvalidInput(t *testing.T) {
	c := New(4, 4, Palette, 0)
	c.Apply(-1, 0, 1, 5)
	c.Apply(0, 0, 200, 5) // colour outside the palette
	pixels, seq := c.Snapshot()
	for _, p := range pixels {
		if p != 0 {
			t.Fatal("invalid Apply calls should not write anything")
		}
	}
	if seq != 0 {
		t.Errorf("seq = %d, want 0", seq)
	}
}

func TestStatsCountsPaintedCells(t *testing.T) {
	c := New(10, 10, Palette, 0)
	now := time.Now()
	for i := 0; i < 5; i++ {
		if _, err := c.Place(i, 0, 7, "u"+string(rune('a'+i)), now); err != nil {
			t.Fatal(err)
		}
	}
	s := c.Stats()
	if s.Painted != 5 {
		t.Errorf("painted = %d, want 5", s.Painted)
	}
	if s.Total != 100 {
		t.Errorf("total = %d, want 100", s.Total)
	}
	if s.ColorCounts[Palette[7]] != 5 {
		t.Errorf("colour histogram = %v", s.ColorCounts)
	}
	if s.ColorCounts[Palette[0]] != 95 {
		t.Errorf("background count = %d, want 95", s.ColorCounts[Palette[0]])
	}
}

func TestClearResetsEveryCell(t *testing.T) {
	c := New(6, 6, Palette, 0)
	now := time.Now()
	_, _ = c.Place(1, 1, 3, "u", now)
	before := c.Seq()
	if got := c.Clear(); got != before+1 {
		t.Errorf("Clear returned %d, want %d", got, before+1)
	}
	pixels, _ := c.Snapshot()
	for i, p := range pixels {
		if p != 0 {
			t.Fatalf("cell %d was not cleared", i)
		}
	}
}

// TestConcurrentPlacesAreSerialised is the one that matters under -race: many
// goroutines painting at once must produce exactly one sequence number per
// successful placement, with no lost updates.
func TestConcurrentPlacesAreSerialised(t *testing.T) {
	c := New(64, 64, Palette, 0)
	const workers = 16
	const each = 100

	var wg sync.WaitGroup
	var mu sync.Mutex
	seqs := make(map[int64]bool, workers*each)

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				x := (id*each + i) % 64
				y := (id*each + i) / 64 % 64
				colour := uint8(1 + (id+i)%(len(Palette)-1))
				p, err := c.Place(x, y, colour, "u", time.Now())
				if err != nil {
					continue // ErrSameColour is expected on collisions
				}
				mu.Lock()
				if seqs[p.Seq] {
					t.Errorf("sequence %d was handed out twice", p.Seq)
				}
				seqs[p.Seq] = true
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()

	if int64(len(seqs)) != c.Seq() {
		t.Errorf("handed out %d sequences but the canvas is at %d", len(seqs), c.Seq())
	}
}

func TestConcurrentSnapshotDuringPlacement(t *testing.T) {
	c := New(32, 32, Palette, 0)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 500; i++ {
			_, _ = c.Place(i%32, (i/32)%32, uint8(1+i%5), "writer", time.Now())
		}
	}()
	for i := 0; i < 500; i++ {
		pixels, _ := c.Snapshot()
		if len(pixels) != 32*32 {
			t.Fatalf("snapshot length = %d", len(pixels))
		}
	}
	<-done
}

func TestPaletteIsWellFormed(t *testing.T) {
	if len(Palette) < 2 {
		t.Fatal("palette needs at least a background and one colour")
	}
	if len(Palette) > 256 {
		t.Fatal("palette must fit in one byte per pixel")
	}
	seen := map[string]bool{}
	for i, hex := range Palette {
		if len(hex) != 7 || hex[0] != '#' {
			t.Errorf("palette[%d] = %q is not a #rrggbb colour", i, hex)
		}
		if seen[hex] {
			t.Errorf("palette[%d] = %q is a duplicate", i, hex)
		}
		seen[hex] = true
	}
}

// ---------------------------------------------------------------- bounds ----

// TestPlaceRejectsEveryWayOutOfBounds walks off the grid in all four
// directions, on both axes, and checks that nothing was written and no sequence
// was spent. x and y are two interchangeable integers, and a check that
// compared x against the height would pass every square-canvas test ever
// written and corrupt memory on the first rectangular room.
func TestPlaceRejectsEveryWayOutOfBounds(t *testing.T) {
	// Deliberately not square: on a 6x3 grid, (4,4) is inside a square canvas's
	// bounds and outside this one's.
	const w, h = 6, 3
	cases := []struct {
		name string
		x, y int
	}{
		{"left of the grid", -1, 1},
		{"above the grid", 1, -1},
		{"right of the grid", w, 1},
		{"below the grid", 1, h},
		{"one past the far corner", w, h},
		{"inside a square canvas but below this one", 4, 4},
		{"far outside", 9999, 9999},
		{"minimum int", -1 << 62, -1 << 62},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := New(w, h, Palette, 0)
			if _, err := c.Place(tc.x, tc.y, 3, "u", time.Now()); !errors.Is(err, ErrOutOfBounds) {
				t.Fatalf("Place(%d,%d) on a %dx%d grid = %v, want ErrOutOfBounds", tc.x, tc.y, w, h, err)
			}
			if seq := c.Seq(); seq != 0 {
				t.Errorf("a rejected placement advanced the sequence to %d; the client would wait for a pixel that never arrives", seq)
			}
			pixels, _ := c.Snapshot()
			for i, p := range pixels {
				if p != 0 {
					t.Fatalf("a rejected placement wrote colour %d at cell %d", p, i)
				}
			}
			// The cooldown is not spent either, so a misclick outside the grid
			// does not cost a turn.
			if got := c.CooldownRemaining("u", time.Now()); got != 0 {
				t.Errorf("a rejected placement started a cooldown of %v", got)
			}
		})
	}

	// The corners are the other half of the same check: they must be inside.
	c := New(w, h, Palette, 0)
	for _, corner := range [][2]int{{0, 0}, {w - 1, 0}, {0, h - 1}, {w - 1, h - 1}} {
		if _, err := c.Place(corner[0], corner[1], 3, "u", time.Now()); err != nil {
			t.Errorf("Place%v on a %dx%d grid = %v, want the corner to be paintable", corner, w, h, err)
		}
	}
}

// TestPlaceRejectsColoursOutsideThisCanvasPalette uses a four-colour palette,
// because a colour check written against the classic twenty would let index 5
// through on a Game Boy room and store a byte no client can render.
func TestPlaceRejectsColoursOutsideThisCanvasPalette(t *testing.T) {
	small := PaletteFor("gameboy")
	if len(small) != 4 {
		t.Fatalf("the gameboy palette has %d colours, and this test is written around 4", len(small))
	}
	c := New(4, 4, small, 0)

	for _, colour := range []uint8{4, 5, 19, 200, 255} {
		if _, err := c.Place(0, 0, colour, "u", time.Now()); !errors.Is(err, ErrBadColour) {
			t.Errorf("Place with colour %d on a %d-colour palette = %v, want ErrBadColour",
				colour, len(small), err)
		}
	}
	if _, err := c.Place(0, 0, 3, "u", time.Now()); err != nil {
		t.Errorf("the last colour in the palette was refused: %v", err)
	}

	// A full byte of palette is the documented maximum, and the last index of it
	// has to be reachable or the last colour is decorative.
	full := make([]string, 256)
	for i := range full {
		full[i] = "#000000"
	}
	big := New(2, 2, full, 0)
	if _, err := big.Place(0, 0, 255, "u", time.Now()); err != nil {
		t.Errorf("colour 255 on a 256-colour palette = %v, want it accepted", err)
	}
}

// TestPlacingTheColourAlreadyThereCostsNothing pins the deliberate cheapness of
// a no-op. It is checked after the cooldown, so the case that matters is the
// painter's first action: clicking a cell that is already that colour must not
// start their cooldown, or a misclick on the background costs them a turn.
func TestPlacingTheColourAlreadyThereCostsNothing(t *testing.T) {
	c := New(4, 4, Palette, time.Second)
	t0 := time.Now()

	// The whole grid starts on palette index 0, so this is a no-op.
	if _, err := c.Place(1, 1, 0, "bess", t0); !errors.Is(err, ErrSameColour) {
		t.Fatalf("painting the background over the background = %v, want ErrSameColour", err)
	}
	if seq := c.Seq(); seq != 0 {
		t.Errorf("a no-op advanced the sequence to %d", seq)
	}
	if got := c.CooldownRemaining("bess", t0); got != 0 {
		t.Fatalf("a no-op started a %v cooldown, so a misclick costs a turn", got)
	}
	// And the painter can immediately do something real, at the same instant.
	if _, err := c.Place(1, 1, 5, "bess", t0); err != nil {
		t.Fatalf("the painter's first real placement was refused: %v", err)
	}
	// Now the cooldown is theirs, and repainting the same colour during it is
	// refused for the cooldown rather than for being a no-op - the order of the
	// two checks is what makes that true.
	if _, err := c.Place(1, 1, 5, "bess", t0.Add(time.Millisecond)); !errors.Is(err, ErrCooldown) {
		t.Errorf("error = %v, want ErrCooldown while the painter is cooling down", err)
	}
}

// -------------------------------------------------------------- sequence ----

// TestConcurrentPlacementsLeaveNoGapsInTheSequence is the stronger half of
// serialisation. Uniqueness alone allows 1, 2, 4: the sequence is what clients
// use to tell whether they have missed an update, so a hole in it makes every
// client that is watching decide it has fallen behind and refetch the canvas.
func TestConcurrentPlacementsLeaveNoGapsInTheSequence(t *testing.T) {
	c := New(64, 64, Palette, 0)
	const workers, each = 8, 200

	var wg sync.WaitGroup
	var mu sync.Mutex
	seqs := make(map[int64]bool, workers*each)

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				// Every worker paints its own row, and cycles the colour so no
				// placement is ever a no-op.
				x := i % 64
				y := id
				colour := uint8(1 + (i % (len(Palette) - 1)))
				p, err := c.Place(x, y, colour, "u", time.Now())
				if err != nil {
					t.Errorf("Place(%d,%d,%d) = %v, want every one of these to land", x, y, colour, err)
					return
				}
				mu.Lock()
				if seqs[p.Seq] {
					t.Errorf("sequence %d was handed out twice", p.Seq)
				}
				seqs[p.Seq] = true
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()

	if int64(len(seqs)) != c.Seq() {
		t.Fatalf("handed out %d sequences but the canvas is at %d", len(seqs), c.Seq())
	}
	for seq := int64(1); seq <= int64(workers*each); seq++ {
		if !seqs[seq] {
			t.Fatalf("sequence %d was never handed out, so the run is %d..%d with a hole in it",
				seq, 1, workers*each)
		}
	}
}

// TestSnapshotSequenceMatchesItsContent is the consistency the write-behind
// loop depends on: it snapshots the grid, stores the sequence that came with
// it, and replays the log from there at the next boot. If the two disagree by
// even one placement, the replay either skips a pixel or paints one twice.
//
// The writer paints cells in index order, so a snapshot at sequence N must have
// exactly the first N cells painted and nothing else - which is a property a
// torn read cannot satisfy.
func TestSnapshotSequenceMatchesItsContent(t *testing.T) {
	const w, h = 16, 16
	c := New(w, h, Palette, 0)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < w*h; i++ {
			if _, err := c.Place(i%w, i/w, uint8(1+i%19), "writer", time.Now()); err != nil {
				t.Errorf("placing cell %d: %v", i, err)
				return
			}
		}
	}()

	checked := 0
	for {
		pixels, seq := c.Snapshot()
		if len(pixels) != w*h {
			t.Fatalf("snapshot is %d bytes, want %d", len(pixels), w*h)
		}
		for i, p := range pixels {
			if int64(i) < seq && p == 0 {
				t.Fatalf("snapshot at sequence %d has cell %d unpainted, so it is missing a placement it claims to contain",
					seq, i)
			}
			if int64(i) >= seq && p != 0 {
				t.Fatalf("snapshot at sequence %d has cell %d painted colour %d, so it contains a placement newer than the sequence it will be stored with",
					seq, i, p)
			}
		}
		checked++
		select {
		case <-done:
			if seq == w*h {
				if checked < 2 {
					t.Error("the writer finished before a single snapshot was taken mid-run, so this proved nothing")
				}
				return
			}
		default:
		}
	}
}

// ------------------------------------------------------------ load, apply ---

// TestLoadRejectsAWrongSizedSnapshotWithoutDamage covers the boot path's guard.
// A snapshot from a room that has since been recreated at another size must be
// refused whole rather than copied as far as it fits, because copy() would
// happily fill the grid with the top half of somebody else's canvas.
func TestLoadRejectsAWrongSizedSnapshotWithoutDamage(t *testing.T) {
	c := New(4, 4, Palette, 0)
	if _, err := c.Place(0, 0, 5, "u", time.Now()); err != nil {
		t.Fatal(err)
	}

	for _, size := range []int{0, 15, 17, 64} {
		blob := make([]byte, size)
		for i := range blob {
			blob[i] = 9
		}
		if err := c.Load(blob, 100); err == nil {
			t.Errorf("loading a %d-byte snapshot into a 4x4 grid succeeded", size)
		}
	}
	pixels, seq := c.Snapshot()
	if pixels[0] != 5 || seq != 1 {
		t.Errorf("after the refused loads the canvas is at sequence %d with %d in the first cell, want 1 and 5 — a rejected load damaged it",
			seq, pixels[0])
	}
}

// TestApplyIsForReplayAndOnlyMovesTheSequenceForward pins the two rules replay
// needs. Apply writes without any of the placement checks, because the log is
// already the record of what was allowed; but a snapshot restored at sequence
// 900 followed by a replay of an older row must not drag the canvas back to
// where the next placement reuses a sequence number.
func TestApplyIsForReplayAndOnlyMovesTheSequenceForward(t *testing.T) {
	c := New(4, 4, Palette, time.Hour)
	if err := c.Load(make([]byte, 16), 900); err != nil {
		t.Fatal(err)
	}

	c.Apply(1, 1, 7, 500) // an older row than the snapshot
	if got := c.Seq(); got != 900 {
		t.Errorf("sequence = %d after applying an older placement, want it left at 900", got)
	}
	pixels, _ := c.Snapshot()
	if pixels[1*4+1] != 7 {
		t.Error("Apply did not write the pixel; a replayed placement was lost")
	}

	c.Apply(2, 2, 8, 1200)
	if got := c.Seq(); got != 1200 {
		t.Errorf("sequence = %d after applying a newer placement, want 1200", got)
	}

	// Apply ignores what it cannot represent rather than panicking, because it
	// is fed straight from the database and a corrupt row must not take the
	// process down at boot.
	for _, bad := range [][4]int{{-1, 0, 1, 1300}, {0, -1, 1, 1300}, {4, 0, 1, 1300}, {0, 4, 1, 1300}, {0, 0, 250, 1300}} {
		c.Apply(bad[0], bad[1], uint8(bad[2]), int64(bad[3]))
	}
	if got := c.Seq(); got != 1200 {
		t.Errorf("sequence = %d after a batch of unusable rows, want it unmoved at 1200", got)
	}

	// A cooldown is not a thing replay has an opinion about: Apply must not
	// arm one, or the first painter after a boot is locked out for an hour.
	if got := c.CooldownRemaining("u", time.Now()); got != 0 {
		t.Errorf("replay left a %v cooldown on a painter who has not painted since the restart", got)
	}
}

// TestNewRefusesACanvasNobodyCanUse covers the two constructor panics. Both are
// programmer errors at startup rather than anything a user can cause, and both
// are better as a failed boot than as a canvas that is silently wrong.
func TestNewRefusesACanvasNobodyCanUse(t *testing.T) {
	cases := []struct {
		name    string
		w, h    int
		palette []string
	}{
		{name: "zero width", w: 0, h: 8, palette: Palette},
		{name: "zero height", w: 8, h: 0, palette: Palette},
		{name: "negative width", w: -1, h: 8, palette: Palette},
		{name: "negative height", w: 8, h: -1, palette: Palette},
		{name: "a palette that does not fit in a byte", w: 8, h: 8, palette: make([]string, 257)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("New(%d, %d, %d colours) returned instead of panicking",
						tc.w, tc.h, len(tc.palette))
				}
			}()
			New(tc.w, tc.h, tc.palette, 0)
		})
	}

	// An absent palette is not a programmer error, it is a room that did not
	// name one, and it falls back rather than failing.
	for _, p := range [][]string{nil, {}} {
		c := New(4, 4, p, 0)
		if len(c.Palette()) != len(Palette) {
			t.Errorf("a canvas built with no palette has %d colours, want the %d of the default",
				len(c.Palette()), len(Palette))
		}
		if _, err := c.Place(0, 0, 5, "u", time.Now()); err != nil {
			t.Errorf("a canvas built with no palette cannot be painted: %v", err)
		}
	}
}

// TestStatsHistogramCountsEveryCell is the info panel's arithmetic: the colour
// counts have to add up to the whole grid, or the percentages next to them are
// nonsense.
func TestStatsHistogramCountsEveryCell(t *testing.T) {
	c := New(8, 4, Palette, 0)
	now := time.Now()
	for i := 0; i < 6; i++ {
		if _, err := c.Place(i, 0, 7, "u", now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.Place(0, 1, 3, "u", now); err != nil {
		t.Fatal(err)
	}

	s := c.Stats()
	if s.Width != 8 || s.Height != 4 || s.Total != 32 {
		t.Errorf("stats say %dx%d totalling %d, want 8x4 totalling 32", s.Width, s.Height, s.Total)
	}
	if s.Painted != 7 {
		t.Errorf("painted = %d, want the 7 cells that are not the background", s.Painted)
	}
	if s.Seq != 7 {
		t.Errorf("stats report sequence %d, want the canvas's 7", s.Seq)
	}
	sum := 0
	for _, n := range s.ColorCounts {
		sum += n
	}
	if sum != s.Total {
		t.Errorf("the colour histogram adds up to %d over a grid of %d, so the percentages beside it are wrong",
			sum, s.Total)
	}
	if s.ColorCounts[Palette[7]] != 6 || s.ColorCounts[Palette[3]] != 1 || s.ColorCounts[Palette[0]] != 25 {
		t.Errorf("histogram = %v, want 6 of colour 7, 1 of colour 3 and 25 background", s.ColorCounts)
	}
}

// TestClearEmptiesTheGridAndMovesOn covers the moderator's reset. The sequence
// has to advance, because clients treat it as "something changed" and a clear
// that left it alone would leave every client showing the old canvas.
func TestClearEmptiesTheGridAndMovesOn(t *testing.T) {
	c := New(4, 4, Palette, time.Hour)
	now := time.Now()
	if _, err := c.Place(1, 1, 3, "bess", now); err != nil {
		t.Fatal(err)
	}
	before := c.Seq()

	if got := c.Clear(); got != before+1 {
		t.Errorf("Clear returned sequence %d, want %d", got, before+1)
	}
	pixels, seq := c.Snapshot()
	for i, p := range pixels {
		if p != 0 {
			t.Fatalf("cell %d survived the clear as colour %d", i, p)
		}
	}
	if seq != before+1 {
		t.Errorf("the canvas is at sequence %d after a clear, want %d", seq, before+1)
	}
	// Clearing the canvas is not an amnesty: whoever was cooling down still is.
	if got := c.CooldownRemaining("bess", now); got == 0 {
		t.Error("clearing the canvas cleared the cooldowns with it, so a clear is a free turn for everybody")
	}
}
