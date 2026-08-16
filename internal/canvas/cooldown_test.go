package canvas

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestZeroCooldownMeansNoCooldown is a regression test with a story. "No
// cooldown" is an option in the room creation form, and a room that takes it
// still refused placements: now is read by the caller before it queues for the
// canvas lock, so two placements by one painter can arrive in the opposite
// order to the clock readings they carry, and an elapsed time of minus a
// microsecond is less than a cooldown of zero. The painter was told they were
// painting too fast in a room advertised as having no limit at all.
//
// The reordering is what a fast double-click or a drag does on a busy canvas,
// and it is reproduced here exactly - by handing Place a timestamp older than
// the one before it - rather than by racing and hoping.
func TestZeroCooldownMeansNoCooldown(t *testing.T) {
	c := New(8, 8, Palette, 0)
	t0 := time.Now()

	if _, err := c.Place(0, 0, 1, "bess", t0); err != nil {
		t.Fatalf("first placement: %v", err)
	}
	// A placement whose clock reading was taken before the one that has already
	// been applied. On a canvas with no cooldown this must simply land.
	if _, err := c.Place(1, 0, 2, "bess", t0.Add(-time.Microsecond)); err != nil {
		t.Fatalf("a placement carrying an earlier timestamp = %v, want it accepted: this canvas has no cooldown", err)
	}
	if got := c.CooldownRemaining("bess", t0.Add(-time.Microsecond)); got != 0 {
		t.Errorf("CooldownRemaining = %v on a canvas with no cooldown, want 0; the client turns this into a countdown", got)
	}

	// The ordinary case: the same painter, as fast as they like.
	for i := 2; i < 20; i++ {
		if _, err := c.Place(i%8, i/8, uint8(1+i%19), "bess", t0); err != nil {
			t.Fatalf("placement %d at the same instant as the last = %v, want it accepted", i, err)
		}
		if got := c.CooldownRemaining("bess", t0); got != 0 {
			t.Fatalf("CooldownRemaining = %v after placement %d, want 0", got, i)
		}
	}
	if got := c.Seq(); got != 20 {
		t.Errorf("the canvas is at sequence %d, want the 20 placements that were made", got)
	}
}

// TestZeroCooldownKeepsNoPerPainterState is the other half of the same
// decision. On a canvas with no cooldown the per-painter map can never be read,
// so filling it is a public canvas holding an id in memory for every stranger
// who has ever painted on it, to answer a question nobody will ask.
func TestZeroCooldownKeepsNoPerPainterState(t *testing.T) {
	c := New(64, 64, Palette, 0)
	now := time.Now()
	for i := 0; i < 4000; i++ {
		if _, err := c.Place(i%64, i/64, uint8(1+i%19), fmt.Sprintf("visitor-%d", i), now); err != nil {
			t.Fatalf("placement %d: %v", i, err)
		}
	}
	c.mu.RLock()
	held := len(c.lastPlace)
	c.mu.RUnlock()
	if held != 0 {
		t.Errorf("the canvas is holding cooldown state for %d painters on a canvas with no cooldown, want none",
			held)
	}
}

// TestCooldownBoundaryIsInclusiveAtTheExpiryInstant pins the comparison at the
// exact instant it changes. One nanosecond either way is the difference between
// a painter who is refused at the moment their timer hits zero - and complains,
// because the countdown on their screen said they could go - and one who is
// allowed a fraction early.
func TestCooldownBoundaryIsInclusiveAtTheExpiryInstant(t *testing.T) {
	const cooldown = 750 * time.Millisecond
	cases := []struct {
		name        string
		elapsed     time.Duration
		wantErr     error
		wantWaiting time.Duration
	}{
		{name: "the same instant", elapsed: 0, wantErr: ErrCooldown, wantWaiting: cooldown},
		{name: "halfway through", elapsed: cooldown / 2, wantErr: ErrCooldown, wantWaiting: cooldown / 2},
		{name: "one nanosecond early", elapsed: cooldown - 1, wantErr: ErrCooldown, wantWaiting: 1},
		{name: "exactly at expiry", elapsed: cooldown, wantErr: nil, wantWaiting: 0},
		{name: "one nanosecond late", elapsed: cooldown + 1, wantErr: nil, wantWaiting: 0},
		{name: "long after", elapsed: time.Hour, wantErr: nil, wantWaiting: 0},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := New(8, 8, Palette, cooldown)
			t0 := time.Now()
			if _, err := c.Place(0, 0, 1, "bess", t0); err != nil {
				t.Fatalf("first placement: %v", err)
			}
			at := t0.Add(tc.elapsed)

			waiting := c.CooldownRemaining("bess", at)
			if waiting != tc.wantWaiting {
				t.Errorf("CooldownRemaining %v after painting = %v, want %v", tc.elapsed, waiting, tc.wantWaiting)
			}
			// The second placement is on a different cell and colour, so the
			// only thing that can refuse it is the cooldown.
			_, err := c.Place(1+i, 1, 2, "bess", at)
			if tc.wantErr == nil {
				if err != nil {
					t.Errorf("placing %v after the last one = %v, want it accepted", tc.elapsed, err)
				}
			} else if !errors.Is(err, tc.wantErr) {
				t.Errorf("placing %v after the last one = %v, want %v", tc.elapsed, err, tc.wantErr)
			}
			// What the canvas refuses and what it reported a moment earlier have
			// to agree, or the countdown on screen expires at a different
			// instant to the rule it is counting down to.
			if (err == nil) != (waiting == 0) {
				t.Errorf("Place returned %v while CooldownRemaining had just said %v to wait: the rule and the countdown disagree",
					err, waiting)
			}
		})
	}
}

// TestClearCooldownIsSafeForAPainterWhoHasNeverPainted covers the call as undo
// makes it: undo clears the painter's cooldown so taking back a misclick does
// not also cost them their turn, and it does that without checking whether
// there was one.
func TestClearCooldownIsSafeForAPainterWhoHasNeverPainted(t *testing.T) {
	c := New(8, 8, Palette, time.Hour)
	now := time.Now()

	c.ClearCooldown("a-stranger")
	c.ClearCooldown("")
	if got := c.CooldownRemaining("a-stranger", now); got != 0 {
		t.Errorf("a painter who has never painted has a %v cooldown", got)
	}

	if _, err := c.Place(0, 0, 1, "bess", now); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Place(1, 0, 2, "bess", now); !errors.Is(err, ErrCooldown) {
		t.Fatalf("second placement = %v, want ErrCooldown", err)
	}
	// Clearing one painter's cooldown must not clear anybody else's.
	if _, err := c.Place(2, 0, 3, "alec", now); err != nil {
		t.Fatal(err)
	}
	c.ClearCooldown("bess")
	if got := c.CooldownRemaining("bess", now); got != 0 {
		t.Errorf("bess still has %v to wait after her cooldown was cleared", got)
	}
	if _, err := c.Place(1, 0, 2, "bess", now); err != nil {
		t.Errorf("bess was refused after her cooldown was cleared: %v", err)
	}
	if got := c.CooldownRemaining("alec", now); got == 0 {
		t.Error("clearing bess's cooldown cleared alec's, so anybody's undo is everybody's free turn")
	}
}

// TestManyPaintersCoolDownIndependently is the room full of people. Each one is
// held to their own cooldown and nobody else's, which is the difference between
// a busy canvas and a queue.
func TestManyPaintersCoolDownIndependently(t *testing.T) {
	const painters = 500
	c := New(64, 64, Palette, time.Second)
	t0 := time.Now()

	for i := 0; i < painters; i++ {
		uid := fmt.Sprintf("painter-%d", i)
		// Each painter starts a fifth of a second after the last, so they are
		// all inside their own cooldown and outside most of the others'.
		at := t0.Add(time.Duration(i) * 200 * time.Millisecond)
		if _, err := c.Place(i%64, i/64, uint8(1+i%19), uid, at); err != nil {
			t.Fatalf("painter %d: %v", i, err)
		}
		if _, err := c.Place((i+1)%64, i/64, uint8(2+i%18), uid, at); !errors.Is(err, ErrCooldown) {
			t.Fatalf("painter %d was not held to their own cooldown: %v", i, err)
		}
		if got := c.CooldownRemaining(uid, at); got != time.Second {
			t.Fatalf("painter %d has %v to wait, want the full second", i, got)
		}
		if i > 0 {
			// The painter from five turns ago is a full second past theirs.
			old := fmt.Sprintf("painter-%d", i-1)
			if got := c.CooldownRemaining(old, at.Add(time.Second)); got != 0 {
				t.Fatalf("painter %d still has %v to wait a second after their placement", i-1, got)
			}
		}
	}
}

// TestCooldownBookkeepingIsPrunedRatherThanAccumulated is the memory question.
// The map is keyed by painter id, and on a public canvas the ids are strangers
// who paint once and never come back: if nothing ever removed them, a canvas
// that stayed up for a month would hold every visitor it had ever had.
//
// It also pins the half that matters more than the memory: a prune must never
// forget somebody who is still inside their cooldown, because that hands them a
// free placement.
func TestCooldownBookkeepingIsPrunedRatherThanAccumulated(t *testing.T) {
	const cooldown = 10 * time.Minute
	c := New(256, 256, Palette, cooldown)
	t0 := time.Now()

	const crowd = 2000
	for i := 0; i < crowd; i++ {
		uid := fmt.Sprintf("visitor-%d", i)
		if _, err := c.Place(i%256, i/256, uint8(1+i%19), uid, t0); err != nil {
			t.Fatalf("visitor %d: %v", i, err)
		}
	}
	if got := c.trackedPainters(); got != crowd {
		t.Fatalf("the canvas is tracking %d painters after %d placements, want all of them: they are all still cooling down",
			got, crowd)
	}

	// Six minutes later, past the prune interval but well inside a ten minute
	// cooldown. The sweep runs and must keep everybody.
	if _, err := c.Place(0, 200, 5, "latecomer", t0.Add(6*time.Minute)); err != nil {
		t.Fatalf("the latecomer: %v", err)
	}
	if got := c.trackedPainters(); got != crowd+1 {
		t.Fatalf("the canvas is tracking %d painters six minutes in, want %d: a prune dropped somebody who is still cooling down, which is a free placement",
			got, crowd+1)
	}
	if got := c.CooldownRemaining("visitor-0", t0.Add(6*time.Minute)); got != 4*time.Minute {
		t.Errorf("a visitor from six minutes ago has %v left of a ten minute cooldown, want 4m", got)
	}

	// Twelve minutes in, the crowd's cooldowns are long gone and their entries
	// can no longer block anybody, so they must not still be here.
	if _, err := c.Place(1, 200, 6, "later-still", t0.Add(12*time.Minute)); err != nil {
		t.Fatalf("the second latecomer: %v", err)
	}
	if got := c.trackedPainters(); got > 2 {
		t.Errorf("the canvas is still tracking %d painters twelve minutes after a crowd of %d painted once, want only the ones who could still be blocked",
			got, crowd)
	}
	// Pruning is not an amnesty for anybody it kept: this painter has just
	// painted, so they are still held to the whole cooldown.
	if got := c.CooldownRemaining("later-still", t0.Add(12*time.Minute)); got != cooldown {
		t.Errorf("a painter who has just painted has %v to wait, want the full %v", got, cooldown)
	}
	// And the one who painted six minutes in is inside their cooldown still, so
	// the sweep must have kept them.
	if got := c.CooldownRemaining("latecomer", t0.Add(12*time.Minute)); got != 4*time.Minute {
		t.Errorf("the painter from six minutes in has %v to wait, want 4m: the sweep forgot somebody it could still block",
			got)
	}
}

// trackedPainters reports how many painters the canvas is holding cooldown
// state for. It reaches inside on purpose: the memory this uses is not visible
// through the exported API, and "does it grow without bound" is not a question
// that can be asked from outside.
func (c *Canvas) trackedPainters() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.lastPlace)
}

// TestConcurrentCooldownAccountingIsRaceFree drives the read and write sides of
// the map from many goroutines at once, which is what -race is here to inspect.
func TestConcurrentCooldownAccountingIsRaceFree(t *testing.T) {
	c := New(64, 64, Palette, 50*time.Millisecond)
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			uid := fmt.Sprintf("painter-%d", w%3) // deliberate contention on the same ids
			for i := 0; i < 200; i++ {
				now := time.Now()
				_, _ = c.Place((w*200+i)%64, (w*200+i)/64%64, uint8(1+i%19), uid, now)
				_ = c.CooldownRemaining(uid, now)
				if i%50 == 0 {
					c.ClearCooldown(uid)
				}
			}
		}(w)
	}
	wg.Wait()
}
