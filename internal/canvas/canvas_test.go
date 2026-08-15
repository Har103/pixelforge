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
