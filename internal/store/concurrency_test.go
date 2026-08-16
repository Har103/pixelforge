package store

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// A room's writes arrive from a write-behind goroutine while its reads are
// served to whoever is looking at the page, and both go through one pool with
// fewer connections than there are callers. These run under -race because the
// interesting failures there are not SQL at all: a shared buffer in the driver,
// or a Result handed to two callers at once, would show up as a canvas with
// somebody else's pixels in it and nothing in the log to explain why.

// TestConcurrentAppendsToOneRoomAllArrive is the write path under contention.
// Nothing may be lost, duplicated, or reordered, because the log is the only
// record of what happened once the process restarts.
func TestConcurrentAppendsToOneRoomAllArrive(t *testing.T) {
	s := newTestStore(t)
	r := newTestRoom(t, s)
	ctx := testCtx(t)

	const writers, each = 8, 25
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				seq := int64(w*each + i + 1)
				err := s.AppendPlacements(ctx, r.ID, []Placement{{
					Seq: seq, X: int(seq) % 32, Y: w, Color: uint8(1 + i%19),
					UID: fmt.Sprintf("painter-%d", w), At: time.Now(),
				}})
				if err != nil {
					t.Errorf("writer %d appending sequence %d: %v", w, seq, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	got := replayAll(t, s, r.ID, 0)
	if len(got) != writers*each {
		t.Fatalf("%d placements survived %d concurrent writers, want %d", len(got), writers, writers*each)
	}
	seen := make(map[int64]bool, len(got))
	var last int64
	for _, p := range got {
		if seen[p.Seq] {
			t.Errorf("sequence %d came back twice", p.Seq)
		}
		seen[p.Seq] = true
		if p.Seq <= last {
			t.Fatalf("replay is out of order: %d follows %d", p.Seq, last)
		}
		last = p.Seq
	}
	for seq := int64(1); seq <= writers*each; seq++ {
		if !seen[seq] {
			t.Errorf("sequence %d is missing, so a pixel somebody painted is gone", seq)
		}
	}

	// The per-painter query has to be right under contention too, because undo
	// asks it and would otherwise offer to retire somebody else's pixel.
	for w := 0; w < writers; w++ {
		uid := fmt.Sprintf("painter-%d", w)
		mine, err := s.LatestOwnPlacement(ctx, r.ID, uid)
		if err != nil {
			t.Errorf("latest placement for %s: %v", uid, err)
			continue
		}
		if want := int64(w*each + each); mine.Seq != want {
			t.Errorf("%s's latest placement is sequence %d, want %d", uid, mine.Seq, want)
		}
		if mine.Y != w {
			t.Errorf("%s's latest placement is on row %d, want %d — that is another painter's row", uid, mine.Y, w)
		}
	}
}

// TestReadsDuringWritesStaySane runs the page's queries against a log that is
// growing underneath them. Each read is its own statement, so it may see more
// than the last one did, but it must never see less, never see a gap, and never
// fail.
func TestReadsDuringWritesStaySane(t *testing.T) {
	s := newTestStore(t)
	r := newTestRoom(t, s)
	ctx := testCtx(t)

	const total = 120
	done := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(done)
		for i := 1; i <= total; i++ {
			err := s.AppendPlacements(ctx, r.ID, []Placement{{
				Seq: int64(i), X: i % 8, Y: (i / 8) % 8, Color: uint8(1 + i%19),
				UID: "writer", At: time.Now(),
			}})
			if err != nil {
				t.Errorf("appending sequence %d: %v", i, err)
				return
			}
		}
	}()

	for reader := 0; reader < 3; reader++ {
		wg.Add(1)
		go func(reader int) {
			defer wg.Done()
			var highest int64
			for {
				select {
				case <-done:
					return
				default:
				}
				n, err := s.CountPlacements(ctx, r.ID)
				if err != nil {
					t.Errorf("reader %d counting: %v", reader, err)
					return
				}
				if n < highest {
					t.Errorf("reader %d saw the count fall from %d to %d; nothing in this test deletes anything",
						reader, highest, n)
					return
				}
				highest = n

				var last int64
				if _, err := s.ReplayAfter(ctx, r.ID, 0, func(seq int64, x, y int, c uint8) {
					if seq <= last {
						t.Errorf("reader %d replayed %d after %d", reader, seq, last)
					}
					last = seq
				}); err != nil {
					t.Errorf("reader %d replaying: %v", reader, err)
					return
				}
				if _, err := s.CellHistory(ctx, r.ID, 1, 0, 10); err != nil {
					t.Errorf("reader %d reading cell history: %v", reader, err)
					return
				}
			}
		}(reader)
	}
	wg.Wait()

	if got, err := s.CountPlacements(ctx, r.ID); err != nil || got != total {
		t.Errorf("%d placements landed while three readers watched (err %v), want %d", got, err, total)
	}
}

// TestWriteBehindStyleBatchesUnderContention is the shape the room actually
// writes in: one large multi-row insert per flush, several rooms flushing at
// once, and a pool with fewer connections than there are flushers. The
// statement is built by hand with numbered placeholders, so a batch that
// mismatched its arguments would either error or, much worse, write one row's
// values into another row's columns.
func TestWriteBehindStyleBatchesUnderContention(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx(t)

	const flushers, batchSize = 6, 400
	rooms := make([]Room, flushers)
	for i := range rooms {
		rooms[i] = newTestRoom(t, s)
	}

	var wg sync.WaitGroup
	for f := 0; f < flushers; f++ {
		wg.Add(1)
		go func(f int) {
			defer wg.Done()
			batch := make([]Placement, 0, batchSize)
			for i := 0; i < batchSize; i++ {
				batch = append(batch, Placement{
					Seq: int64(i + 1), X: i % 32, Y: (i / 32) % 32, Color: uint8(1 + i%19),
					UID: fmt.Sprintf("room-%d-painter", f), At: time.Now(),
				})
			}
			if err := s.AppendPlacements(ctx, rooms[f].ID, batch); err != nil {
				t.Errorf("flusher %d: %v", f, err)
			}
		}(f)
	}
	wg.Wait()

	for f := range rooms {
		got := replayAll(t, s, rooms[f].ID, 0)
		if len(got) != batchSize {
			t.Fatalf("room %d has %d placements, want the %d in its batch", f, len(got), batchSize)
		}
		for i, p := range got {
			want := replayedPixel{int64(i + 1), i % 32, (i / 32) % 32, uint8(1 + i%19)}
			if p != want {
				t.Fatalf("room %d placement %d = %+v, want %+v — the batch's columns are crossed",
					f, i, p, want)
			}
		}
	}
}
