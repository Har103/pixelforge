package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Har103/pixelforge/internal/pg"
)

// These tests need a real PostgreSQL, because the queries are the thing being
// tested and a fake store would only prove the fake works. Point
// PIXELFORGE_TEST_DSN at a throwaway database to run them.
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("PIXELFORGE_TEST_DSN"))
	if dsn == "" {
		t.Skip("set PIXELFORGE_TEST_DSN to run the database-backed store tests")
	}
	return dsn
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dsn := testDSN(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	cfg, err := pg.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("parsing test DSN: %v", err)
	}
	pool := pg.NewPool(cfg, 4, log)
	t.Cleanup(pool.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := pool.WaitReady(ctx, 3); err != nil {
		t.Fatalf("test database not reachable: %v", err)
	}
	s := New(pool, log)
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	return s
}

// newTestRoom creates a throwaway room and removes it, with everything that
// hangs off it, when the test ends. Every test gets its own so one test's
// placements can never become another's answers.
func newTestRoom(t *testing.T, s *Store) Room {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	r, err := s.CreateRoom(ctx, Room{
		Slug:       fmt.Sprintf("storetest-%d", time.Now().UnixNano()),
		Name:       "store test",
		Width:      32,
		Height:     32,
		Palette:    "classic",
		Visibility: "unlisted",
	})
	if err != nil {
		t.Fatalf("creating test room: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		for _, sql := range []string{
			`delete from room_placements where room_id = $1`,
			`delete from room_snapshots where room_id = $1`,
			`delete from bans where room_id = $1`,
			`delete from rooms where id = $1`,
		} {
			if _, err := s.pool.Query(cleanupCtx, sql, r.ID); err != nil {
				t.Errorf("cleaning up room %d: %v", r.ID, err)
			}
		}
	})
	return r
}

// paint appends one placement to the log, the way the write-behind loop does.
func paint(t *testing.T, s *Store, roomID, seq int64, x, y int, colour uint8, uid string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err := s.AppendPlacements(ctx, roomID, []Placement{
		{Seq: seq, X: x, Y: y, Color: colour, UID: uid, At: time.Now()},
	})
	if err != nil {
		t.Fatalf("appending placement %d: %v", seq, err)
	}
}

// testCtx bounds every query so a wedged database fails the test rather than
// hanging the run.
func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// ------------------------------------------------------------ cell history --

// TestCellHistoryIsNewestFirstAndScopedToOneCell pins the three ways this query
// can quietly go wrong: ordering the wrong way round, so the panel claims the
// oldest placement is the current one; matching the wrong cell, because x and y
// are two interchangeable integers and a swap type-checks perfectly; and
// forgetting the room, which would show one canvas's history on another.
func TestCellHistoryIsNewestFirstAndScopedToOneCell(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx(t)
	a := newTestRoom(t, s)
	b := newTestRoom(t, s)

	// The cell under test, painted three times by two people.
	paint(t, s, a.ID, 1, 2, 3, 3, "bess")
	paint(t, s, a.ID, 4, 2, 3, 5, "alec")
	paint(t, s, a.ID, 7, 2, 3, 7, "bess")

	// Decoys: the transposed cell, the same column, the same row, and the same
	// cell in another room. Each one appears in the answer if a predicate is
	// missing or the coordinates are swapped.
	paint(t, s, a.ID, 2, 3, 2, 11, "decoy")
	paint(t, s, a.ID, 3, 2, 4, 12, "decoy")
	paint(t, s, a.ID, 5, 5, 3, 13, "decoy")
	paint(t, s, b.ID, 1, 2, 3, 14, "decoy")

	got, err := s.CellHistory(ctx, a.ID, 2, 3, 10)
	if err != nil {
		t.Fatalf("cell history: %v", err)
	}
	want := []CellPlacement{
		{Seq: 7, Color: 7, UID: "bess"},
		{Seq: 4, Color: 5, UID: "alec"},
		{Seq: 1, Color: 3, UID: "bess"},
	}
	if len(got) != len(want) {
		t.Fatalf("cell history has %d entries, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].Seq != w.Seq || got[i].Color != w.Color || got[i].UID != w.UID {
			t.Errorf("entry %d = seq %d colour %d by %q, want seq %d colour %d by %q",
				i, got[i].Seq, got[i].Color, got[i].UID, w.Seq, w.Color, w.UID)
		}
		if got[i].Undone {
			t.Errorf("entry %d is marked undone but nothing has been retired", i)
		}
	}

	// The timestamp is milliseconds, which is what the client formats. Seconds
	// would put every placement in 1970 on screen.
	newest := got[0].AtMs
	if drift := time.Since(time.UnixMilli(newest)); drift < 0 || drift > time.Minute {
		t.Errorf("newest entry timestamp = %d ms (%v ago), which is not the millisecond it was written",
			newest, drift)
	}
}

// TestCellHistoryShowsRetractedPlacements is deliberate rather than incidental:
// provenance has to include the pixel somebody took back, or "who painted over
// me and then undid it" becomes unanswerable.
func TestCellHistoryShowsRetractedPlacements(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx(t)
	r := newTestRoom(t, s)

	paint(t, s, r.ID, 1, 4, 4, 3, "bess")
	paint(t, s, r.ID, 2, 4, 4, 9, "alec")
	if err := s.MarkUndone(ctx, r.ID, 2); err != nil {
		t.Fatalf("marking undone: %v", err)
	}

	got, err := s.CellHistory(ctx, r.ID, 4, 4, 10)
	if err != nil {
		t.Fatalf("cell history: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("cell history has %d entries, want both the live and the retired one: %+v", len(got), got)
	}
	if !got[0].Undone {
		t.Errorf("alec's retired placement (seq %d) is not flagged undone", got[0].Seq)
	}
	if got[1].Undone {
		t.Errorf("bess's placement (seq %d) was flagged undone by somebody else's undo", got[1].Seq)
	}
}

func TestCellHistoryRespectsItsLimit(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx(t)
	r := newTestRoom(t, s)

	const total = 14
	for i := 1; i <= total; i++ {
		paint(t, s, r.ID, int64(i), 6, 6, uint8(i), "bess")
	}

	cases := []struct{ limit, want int }{
		{limit: 3, want: 3},
		{limit: 20, want: total},
		{limit: 0, want: 12},   // unset falls back to the default page
		{limit: 500, want: 12}, // and so does an absurd one
	}
	for _, tc := range cases {
		got, err := s.CellHistory(ctx, r.ID, 6, 6, tc.limit)
		if err != nil {
			t.Fatalf("cell history (limit %d): %v", tc.limit, err)
		}
		if len(got) != tc.want {
			t.Errorf("limit %d returned %d entries, want %d", tc.limit, len(got), tc.want)
		}
		// A limit applied to the wrong end of the ordering would hand back the
		// oldest placements, which is exactly the opposite of what a panel
		// showing "recent history" needs.
		if len(got) > 0 && got[0].Seq != total {
			t.Errorf("limit %d starts at seq %d, want the newest placement %d", tc.limit, got[0].Seq, total)
		}
	}
}

// ------------------------------------------------------- one cell, one user --

func TestLatestOwnPlacementFindsTheNewestLiveOne(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx(t)
	r := newTestRoom(t, s)
	other := newTestRoom(t, s)

	paint(t, s, r.ID, 1, 1, 1, 2, "alec")
	paint(t, s, r.ID, 2, 9, 9, 8, "bess")
	paint(t, s, r.ID, 3, 5, 6, 4, "alec")
	// A newer placement by the same painter in a different room must not be
	// mistaken for theirs here.
	paint(t, s, other.ID, 99, 7, 7, 1, "alec")

	got, err := s.LatestOwnPlacement(ctx, r.ID, "alec")
	if err != nil {
		t.Fatalf("latest own placement: %v", err)
	}
	if got.Seq != 3 || got.X != 5 || got.Y != 6 || got.Color != 4 {
		t.Errorf("latest = %+v, want seq 3 at (5,6) colour 4", got)
	}
	if got.UID != "alec" {
		t.Errorf("latest placement uid = %q, want alec", got.UID)
	}

	// Retiring it steps back to the painter's previous placement rather than
	// handing out a placement that is no longer on the canvas.
	if err := s.MarkUndone(ctx, r.ID, 3); err != nil {
		t.Fatalf("marking undone: %v", err)
	}
	got, err = s.LatestOwnPlacement(ctx, r.ID, "alec")
	if err != nil {
		t.Fatalf("latest own placement after an undo: %v", err)
	}
	if got.Seq != 1 || got.X != 1 || got.Y != 1 || got.Color != 2 {
		t.Errorf("after retiring seq 3, latest = %+v, want seq 1 at (1,1) colour 2", got)
	}

	// And once everything of theirs is retired there is nothing to find.
	if err := s.MarkUndone(ctx, r.ID, 1); err != nil {
		t.Fatalf("marking undone: %v", err)
	}
	if _, err := s.LatestOwnPlacement(ctx, r.ID, "alec"); !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound once every placement is retired", err)
	}
	if _, err := s.LatestOwnPlacement(ctx, r.ID, "never-painted"); !errors.Is(err, ErrNotFound) {
		t.Errorf("error for a painter who has never painted = %v, want ErrNotFound", err)
	}
}

func TestTopPlacementAtIsWhatIsShowing(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx(t)
	r := newTestRoom(t, s)
	other := newTestRoom(t, s)

	paint(t, s, r.ID, 1, 2, 2, 3, "bess")
	paint(t, s, r.ID, 2, 2, 2, 5, "alec")
	paint(t, s, r.ID, 3, 3, 3, 9, "bess") // a newer placement on a different cell
	paint(t, s, other.ID, 50, 2, 2, 12, "carl")

	got, err := s.TopPlacementAt(ctx, r.ID, 2, 2)
	if err != nil {
		t.Fatalf("top placement: %v", err)
	}
	if got.Seq != 2 || got.Color != 5 || got.UID != "alec" {
		t.Errorf("top = %+v, want alec's seq 2 colour 5", got)
	}

	// Undoing the top reveals what was under it, which is the whole reason the
	// log keeps retired rows instead of deleting them.
	if err := s.MarkUndone(ctx, r.ID, 2); err != nil {
		t.Fatalf("marking undone: %v", err)
	}
	got, err = s.TopPlacementAt(ctx, r.ID, 2, 2)
	if err != nil {
		t.Fatalf("top placement after an undo: %v", err)
	}
	if got.Seq != 1 || got.Color != 3 || got.UID != "bess" {
		t.Errorf("top after retiring alec = %+v, want bess's seq 1 colour 3", got)
	}

	if _, err := s.TopPlacementAt(ctx, r.ID, 30, 30); !errors.Is(err, ErrNotFound) {
		t.Errorf("error for a cell nobody has painted = %v, want ErrNotFound", err)
	}
}

func TestColourBeneathSkipsRetiredPlacements(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx(t)
	r := newTestRoom(t, s)

	paint(t, s, r.ID, 1, 2, 2, 3, "bess")
	paint(t, s, r.ID, 2, 2, 2, 5, "alec")
	paint(t, s, r.ID, 3, 2, 2, 7, "carl")
	paint(t, s, r.ID, 4, 8, 8, 13, "decoy") // a newer placement elsewhere

	cases := []struct {
		seq  int64
		want uint8
		why  string
	}{
		{seq: 3, want: 5, why: "the placement immediately under carl's"},
		{seq: 2, want: 3, why: "the placement immediately under alec's"},
		{seq: 1, want: 0, why: "the background, because nothing is under bess's"},
	}
	for _, tc := range cases {
		got, err := s.ColourBeneath(ctx, r.ID, 2, 2, tc.seq)
		if err != nil {
			t.Fatalf("colour beneath seq %d: %v", tc.seq, err)
		}
		if got != tc.want {
			t.Errorf("colour beneath seq %d = %d, want %d — %s", tc.seq, got, tc.want, tc.why)
		}
	}

	// With the middle placement retired, the one under it is what would come
	// back. Anything else would resurrect a pixel somebody already took away.
	if err := s.MarkUndone(ctx, r.ID, 2); err != nil {
		t.Fatalf("marking undone: %v", err)
	}
	got, err := s.ColourBeneath(ctx, r.ID, 2, 2, 3)
	if err != nil {
		t.Fatalf("colour beneath: %v", err)
	}
	if got != 3 {
		t.Errorf("colour beneath seq 3 = %d, want 3 now that seq 2 is retired", got)
	}
}

// TestMarkUndoneRetiresExactlyOnePlacement guards the blast radius: the query
// takes a room and a sequence, and both have to be in the WHERE clause or one
// painter's undo quietly retires a stranger's pixel.
func TestMarkUndoneRetiresExactlyOnePlacement(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx(t)
	r := newTestRoom(t, s)
	other := newTestRoom(t, s)

	paint(t, s, r.ID, 1, 2, 2, 3, "bess")
	paint(t, s, r.ID, 2, 2, 2, 5, "alec")
	paint(t, s, r.ID, 3, 6, 6, 7, "alec")
	paint(t, s, other.ID, 2, 2, 2, 9, "carl") // same sequence, different room

	if err := s.MarkUndone(ctx, r.ID, 2); err != nil {
		t.Fatalf("marking undone: %v", err)
	}

	live, err := s.CountPlacements(ctx, r.ID)
	if err != nil {
		t.Fatalf("counting placements: %v", err)
	}
	if live != 2 {
		t.Errorf("%d live placements after retiring one of three, want 2", live)
	}
	if got, err := s.CountPlacements(ctx, other.ID); err != nil || got != 1 {
		t.Errorf("the other room has %d live placements (err %v), want its own untouched 1", got, err)
	}

	// A sequence that belongs to another room must not be retired here either.
	if err := s.MarkUndone(ctx, other.ID, 3); err != nil {
		t.Fatalf("marking undone in the other room: %v", err)
	}
	if got, _ := s.CountPlacements(ctx, r.ID); got != 2 {
		t.Errorf("a MarkUndone aimed at another room changed this one: %d live placements, want 2", got)
	}
}
