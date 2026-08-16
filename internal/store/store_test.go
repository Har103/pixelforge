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

// paintAll appends a batch the way the write-behind loop does, in one insert.
func paintAll(t *testing.T, s *Store, roomID int64, batch []Placement) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.AppendPlacements(ctx, roomID, batch); err != nil {
		t.Fatalf("appending %d placements: %v", len(batch), err)
	}
}

// replayed is what ReplayAfter hands its callback, collected so a test can
// compare the whole sequence rather than a count.
type replayedPixel struct {
	Seq    int64
	X, Y   int
	Colour uint8
}

func replayAll(t *testing.T, s *Store, roomID, after int64) []replayedPixel {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var out []replayedPixel
	n, err := s.ReplayAfter(ctx, roomID, after, func(seq int64, x, y int, c uint8) {
		out = append(out, replayedPixel{seq, x, y, c})
	})
	if err != nil {
		t.Fatalf("replaying room %d after %d: %v", roomID, after, err)
	}
	if n != len(out) {
		t.Errorf("ReplayAfter reported %d placements but called back %d times; a caller that trusts the count logs a lie",
			n, len(out))
	}
	return out
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

// TestMarkUndoneOnAnUnknownSequenceChangesNothing pins the quiet case. An undo
// racing a rebuild can name a sequence that is no longer there, and an update
// with no rows to hit is the right answer rather than an error a handler would
// have to translate into "something went wrong".
func TestMarkUndoneOnAnUnknownSequenceChangesNothing(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx(t)
	r := newTestRoom(t, s)

	paint(t, s, r.ID, 1, 2, 2, 3, "bess")
	if err := s.MarkUndone(ctx, r.ID, 999); err != nil {
		t.Fatalf("marking a sequence that does not exist: %v", err)
	}
	if got, err := s.CountPlacements(ctx, r.ID); err != nil || got != 1 {
		t.Errorf("%d live placements (err %v), want the untouched 1", got, err)
	}
}

// ---------------------------------------------------------------- replay ----

// TestReplayAfterIsOrderedLiveAndResumable covers the query that rebuilds a
// canvas at boot. All three properties are load-bearing: out of order and the
// wrong painter's colour ends up on top; including retired rows and every undo
// in the room's history comes back the moment somebody restarts the process;
// ignoring the cursor and a snapshot restore replays work it already has.
func TestReplayAfterIsOrderedLiveAndResumable(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx(t)
	r := newTestRoom(t, s)

	// Two painters interleaved, with one cell painted four times, because a
	// replay that loses the order only shows up on a cell somebody fought over.
	paintAll(t, s, r.ID, []Placement{
		{Seq: 1, X: 1, Y: 1, Color: 2, UID: "bess", At: time.Now()},
		{Seq: 2, X: 1, Y: 1, Color: 3, UID: "alec", At: time.Now()},
		{Seq: 3, X: 5, Y: 5, Color: 4, UID: "bess", At: time.Now()},
		{Seq: 4, X: 1, Y: 1, Color: 5, UID: "alec", At: time.Now()},
		{Seq: 5, X: 1, Y: 1, Color: 6, UID: "bess", At: time.Now()},
	})

	got := replayAll(t, s, r.ID, 0)
	want := []replayedPixel{
		{1, 1, 1, 2}, {2, 1, 1, 3}, {3, 5, 5, 4}, {4, 1, 1, 5}, {5, 1, 1, 6},
	}
	if len(got) != len(want) {
		t.Fatalf("replayed %d placements, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("replayed placement %d = %+v, want %+v — the canvas is rebuilt in this order, so the last one wins",
				i, got[i], want[i])
		}
	}

	// A retired placement is not part of the canvas. Replaying it would repaint
	// a pixel somebody has already taken back.
	if err := s.MarkUndone(ctx, r.ID, 4); err != nil {
		t.Fatalf("marking undone: %v", err)
	}
	got = replayAll(t, s, r.ID, 0)
	for _, p := range got {
		if p.Seq == 4 {
			t.Errorf("replay still includes retired placement %+v", p)
		}
	}
	if len(got) != 4 {
		t.Errorf("replayed %d placements after retiring one of five, want 4", len(got))
	}

	// The cursor is exclusive: restoring a snapshot taken at sequence N and then
	// replaying from N must not reapply N itself.
	got = replayAll(t, s, r.ID, 3)
	if len(got) != 1 || got[0].Seq != 5 {
		t.Errorf("replay after sequence 3 = %+v, want only sequence 5", got)
	}
	if got := replayAll(t, s, r.ID, 5); len(got) != 0 {
		t.Errorf("replay after the newest sequence = %+v, want nothing left to do", got)
	}

	// An empty log is the first boot of every room, and it has to be silent
	// rather than an error the registry turns into a failed page load.
	empty := newTestRoom(t, s)
	if got := replayAll(t, s, empty.ID, 0); len(got) != 0 {
		t.Errorf("replaying a room nobody has painted = %+v, want nothing", got)
	}
}

// TestALongHistoryPagesInReplayAndIsCappedInTheFeed drives both readers of the
// log past the limits that only appear on a room with a real history behind it.
// They share one room because building it is the expensive part of the test:
// sixty thousand placements is a busy canvas after a weekend, and it is the
// only way to reach either limit.
//
// The pager carries its cursor by hand from one query to the next, and getting
// that wrong either loses everything past the first page - a canvas that
// restores three-quarters painted - or never advances and hangs the boot in a
// loop. The feed's cap is the other side of the same log: without it a client
// can ask the server to build a gigabyte of JSON in one request.
func TestALongHistoryPagesInReplayAndIsCappedInTheFeed(t *testing.T) {
	s := newTestStore(t)
	r := newTestRoom(t, s)

	// Three full pages of 20000 and one row over, so the boundary is crossed
	// more than once and the tail is a single placement: an off-by-one in the
	// cursor loses it, and a pager that stops early leaves it behind.
	const total = 60001
	batch := make([]Placement, 0, 4000)
	for i := 1; i <= total; i++ {
		batch = append(batch, Placement{
			Seq: int64(i), X: i % 32, Y: (i / 32) % 32,
			Color: uint8(i % 20), UID: "bulk", At: time.Now(),
		})
		if len(batch) == 4000 || i == total {
			paintAll(t, s, r.ID, batch)
			batch = batch[:0]
		}
	}

	seen := 0
	var last int64
	ctx := testCtx(t)
	n, err := s.ReplayAfter(ctx, r.ID, 0, func(seq int64, x, y int, c uint8) {
		seen++
		if seq <= last {
			t.Fatalf("replay went backwards at %d after %d; the pager is re-reading rows it has already handed out",
				seq, last)
		}
		last = seq
	})
	if err != nil {
		t.Fatalf("replaying a long history: %v", err)
	}
	if n != total || seen != total {
		t.Errorf("replayed %d placements (%d callbacks) of %d; the pages after the first are missing",
			n, seen, total)
	}
	if last != total {
		t.Errorf("the last placement replayed was %d, want %d", last, total)
	}

	// The feed caps what one request can pull out of the same log.
	for _, tc := range []struct {
		name  string
		limit int
		want  int
	}{
		{name: "an absurd limit is capped", limit: 1_000_000, want: 50000},
		{name: "no limit is the same cap", limit: 0, want: 50000},
		{name: "a negative limit is the same cap", limit: -1, want: 50000},
		{name: "the largest limit that is honoured", limit: 200000, want: 60001},
		{name: "a modest limit is honoured", limit: 250, want: 250},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.History(ctx, r.ID, 0, tc.limit)
			if err != nil {
				t.Fatalf("history with limit %d: %v", tc.limit, err)
			}
			if len(got) != tc.want {
				t.Errorf("history with limit %d returned %d entries, want %d", tc.limit, len(got), tc.want)
			}
			if len(got) > 0 && got[0].Seq != 1 {
				t.Errorf("history with limit %d starts at sequence %d, want the oldest placement", tc.limit, got[0].Seq)
			}
		})
	}
}

// --------------------------------------------------------------- history ----

// TestHistoryIsOldestFirst covers the time-lapse feed, which is the same log
// read the other way round from the cell panel: forwards, because a time-lapse
// that played backwards would be a different film. The caps on how much of it
// one request can pull are only reachable on a room with tens of thousands of
// placements, so they are tested where such a room already exists.
func TestHistoryIsOldestFirst(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx(t)
	r := newTestRoom(t, s)

	const total = 6
	batch := make([]Placement, 0, total)
	for i := 1; i <= total; i++ {
		batch = append(batch, Placement{
			Seq: int64(i), X: i, Y: 1, Color: uint8(i), UID: "bess", At: time.Now(),
		})
	}
	paintAll(t, s, r.ID, batch)
	if err := s.MarkUndone(ctx, r.ID, 3); err != nil {
		t.Fatalf("marking undone: %v", err)
	}

	got, err := s.History(ctx, r.ID, 0, 0)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(got) != total-1 {
		t.Fatalf("history has %d entries, want %d live ones: %+v", len(got), total-1, got)
	}
	for i := 1; i < len(got); i++ {
		if got[i].Seq <= got[i-1].Seq {
			t.Fatalf("history is not oldest-first at entry %d: %d follows %d", i, got[i].Seq, got[i-1].Seq)
		}
	}
	for _, e := range got {
		if e.Seq == 3 {
			t.Errorf("history includes retired placement %+v, so the time-lapse paints a pixel that was taken back", e)
		}
	}

	// The cursor is how a client resumes a long time-lapse without refetching.
	if got, err := s.History(ctx, r.ID, 4, 10); err != nil || len(got) != 2 || got[0].Seq != 5 {
		t.Errorf("history after sequence 4 = %+v (err %v), want sequences 5 and 6", got, err)
	}
	if got, err := s.History(ctx, r.ID, 0, 2); err != nil || len(got) != 2 {
		t.Errorf("history limited to 2 returned %d entries (err %v)", len(got), err)
	}
}

// TestHistoryTimestampsSurviveTheRoundTrip pins the wire format of a time. The
// client renders these as clock times, so a lost timezone or a seconds/millis
// mix-up puts every placement in the wrong hour, or in 1970.
func TestHistoryTimestampsSurviveTheRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx(t)
	r := newTestRoom(t, s)

	// Deliberately not UTC and not a round number of seconds: an encoder that
	// drops the zone or truncates to the second fails on exactly this.
	zone := time.FixedZone("UTC+7", 7*3600)
	at := time.Date(2021, time.March, 4, 5, 6, 7, 89_000_000, zone)
	paintAll(t, s, r.ID, []Placement{{Seq: 1, X: 1, Y: 1, Color: 2, UID: "bess", At: at}})

	got, err := s.History(ctx, r.ID, 0, 10)
	if err != nil || len(got) != 1 {
		t.Fatalf("history = %+v (err %v), want one entry", got, err)
	}
	if got[0].At != at.UnixMilli() {
		t.Errorf("stored %v (%d ms) and read back %d ms, a drift of %v",
			at, at.UnixMilli(), got[0].At,
			time.Duration(got[0].At-at.UnixMilli())*time.Millisecond)
	}
}

// ------------------------------------------------------------------ undo ----

// TestUndoUserRetiresOnlyThatPainter is the moderator's mass undo: it has to
// take back everything one person drew, count it honestly for the report in the
// UI, and leave everybody else's work alone.
func TestUndoUserRetiresOnlyThatPainter(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx(t)
	r := newTestRoom(t, s)

	paintAll(t, s, r.ID, []Placement{
		{Seq: 1, X: 1, Y: 1, Color: 2, UID: "vandal", At: time.Now()},
		{Seq: 2, X: 2, Y: 2, Color: 3, UID: "bess", At: time.Now()},
		{Seq: 3, X: 3, Y: 3, Color: 4, UID: "vandal", At: time.Now()},
		{Seq: 4, X: 4, Y: 4, Color: 5, UID: "vandal", At: time.Now()},
	})
	// One of the vandal's placements is already retired, so it must not be
	// counted again: the number goes on screen as "undid N placements".
	if err := s.MarkUndone(ctx, r.ID, 4); err != nil {
		t.Fatalf("marking undone: %v", err)
	}

	n, err := s.UndoUser(ctx, r.ID, "vandal")
	if err != nil {
		t.Fatalf("undoing a painter: %v", err)
	}
	if n != 2 {
		t.Errorf("UndoUser reported %d placements, want the 2 that were still live", n)
	}
	live := replayAll(t, s, r.ID, 0)
	if len(live) != 1 || live[0].Seq != 2 {
		t.Errorf("the canvas replays as %+v, want only bess's placement 2", live)
	}

	// Running it again finds nothing, because everything of theirs is retired.
	if n, err := s.UndoUser(ctx, r.ID, "vandal"); err != nil || n != 0 {
		t.Errorf("a second undo reported %d (err %v), want 0", n, err)
	}
	if n, err := s.UndoUser(ctx, r.ID, "never-painted"); err != nil || n != 0 {
		t.Errorf("undoing a painter who never painted reported %d (err %v), want 0", n, err)
	}
	// The rows survive the undo, because provenance is the reason they are kept
	// rather than deleted.
	hist, err := s.CellHistory(ctx, r.ID, 1, 1, 10)
	if err != nil || len(hist) != 1 || !hist[0].Undone {
		t.Errorf("the retired placement at (1,1) reads back as %+v (err %v), want one entry flagged undone",
			hist, err)
	}
}

// TestClearRoomRetiresEverythingWithoutLosingIt covers "clear the canvas": the
// grid empties, the log does not, and the room next door keeps its pixels.
func TestClearRoomRetiresEverythingWithoutLosingIt(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx(t)
	r := newTestRoom(t, s)
	other := newTestRoom(t, s)

	paintAll(t, s, r.ID, []Placement{
		{Seq: 1, X: 1, Y: 1, Color: 2, UID: "bess", At: time.Now()},
		{Seq: 2, X: 2, Y: 2, Color: 3, UID: "alec", At: time.Now()},
	})
	paintAll(t, s, other.ID, []Placement{
		{Seq: 1, X: 1, Y: 1, Color: 9, UID: "carl", At: time.Now()},
	})

	if err := s.ClearRoom(ctx, r.ID); err != nil {
		t.Fatalf("clearing: %v", err)
	}
	if got := replayAll(t, s, r.ID, 0); len(got) != 0 {
		t.Errorf("the cleared room replays as %+v, want an empty canvas", got)
	}
	if got, err := s.CountPlacements(ctx, other.ID); err != nil || got != 1 {
		t.Errorf("the room next door has %d live placements (err %v), want its own untouched 1", got, err)
	}
	// Cleared is not deleted: the history panel still answers "who painted here".
	hist, err := s.CellHistory(ctx, r.ID, 1, 1, 10)
	if err != nil || len(hist) != 1 || !hist[0].Undone {
		t.Errorf("the cleared cell's history is %+v (err %v), want the placement kept and flagged undone", hist, err)
	}
	// Clearing an already-empty room is what a double-click on the button does.
	if err := s.ClearRoom(ctx, r.ID); err != nil {
		t.Errorf("clearing an already-cleared room: %v", err)
	}
}

// TestLeaderboardCountsLivePlacementsPerPainter pins the ordering and the
// exclusion. A board that counted retired placements would rank the person
// whose work a moderator has just undone at the top of the room.
func TestLeaderboardCountsLivePlacementsPerPainter(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx(t)
	r := newTestRoom(t, s)

	batch := []Placement{}
	add := func(seq int64, uid string) {
		batch = append(batch, Placement{
			Seq: seq, X: int(seq % 32), Y: int(seq / 32), Color: 3, UID: uid, At: time.Now(),
		})
	}
	for i := int64(1); i <= 5; i++ {
		add(i, "bess")
	}
	for i := int64(6); i <= 8; i++ {
		add(i, "alec")
	}
	add(9, "carl")
	paintAll(t, s, r.ID, batch)
	for _, seq := range []int64{1, 2, 3, 4} { // most of bess's work is retired
		if err := s.MarkUndone(ctx, r.ID, seq); err != nil {
			t.Fatalf("marking undone: %v", err)
		}
	}

	got, err := s.Leaderboard(ctx, r.ID, 10)
	if err != nil {
		t.Fatalf("leaderboard: %v", err)
	}
	want := []LeaderRow{{UID: "alec", Count: 3}, {UID: "bess", Count: 1}, {UID: "carl", Count: 1}}
	if len(got) != len(want) {
		t.Fatalf("leaderboard has %d rows, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			// The tie between bess and carl is broken by uid, so the board does
			// not shuffle itself every time somebody refreshes.
			t.Errorf("leaderboard row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if got, err := s.Leaderboard(ctx, r.ID, 1); err != nil || len(got) != 1 || got[0].UID != "alec" {
		t.Errorf("leaderboard limited to 1 = %+v (err %v), want alec alone", got, err)
	}
}

// ------------------------------------------------------- room isolation -----

// TestEveryReadPathIsScopedToItsRoom is the test for the failure that would be
// hardest to notice and worst to explain: a room showing another room's pixels.
// Every query in this package takes a room id, and a missing predicate in any
// one of them leaks - so this builds two rooms whose placements collide on
// every axis at once (same coordinates, same sequence numbers, same painters,
// different colours) and then checks every read path individually.
func TestEveryReadPathIsScopedToItsRoom(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx(t)
	mine := newTestRoom(t, s)
	theirs := newTestRoom(t, s)

	paintAll(t, s, mine.ID, []Placement{
		{Seq: 1, X: 2, Y: 3, Color: 3, UID: "bess", At: time.Now()},
		{Seq: 2, X: 2, Y: 3, Color: 5, UID: "alec", At: time.Now()},
		{Seq: 3, X: 7, Y: 7, Color: 9, UID: "bess", At: time.Now()},
	})
	paintAll(t, s, theirs.ID, []Placement{
		{Seq: 1, X: 2, Y: 3, Color: 11, UID: "bess", At: time.Now()},
		{Seq: 2, X: 2, Y: 3, Color: 12, UID: "alec", At: time.Now()},
		{Seq: 3, X: 7, Y: 7, Color: 13, UID: "carl", At: time.Now()},
		{Seq: 4, X: 2, Y: 3, Color: 14, UID: "carl", At: time.Now()},
	})
	if err := s.Ban(ctx, theirs.ID, "carl"); err != nil {
		t.Fatalf("banning next door: %v", err)
	}
	if err := s.SaveSnapshot(ctx, theirs.ID, Snapshot{Width: 2, Height: 1, Pixels: []byte{13, 14}, Seq: 4}); err != nil {
		t.Fatalf("snapshotting next door: %v", err)
	}
	if err := s.SaveSnapshot(ctx, mine.ID, Snapshot{Width: 2, Height: 1, Pixels: []byte{3, 5}, Seq: 3}); err != nil {
		t.Fatalf("snapshotting: %v", err)
	}

	// The colours 11 to 14 exist only next door, so any of them turning up in
	// an answer here is a leak, and each check names the query that leaked.
	t.Run("ReplayAfter", func(t *testing.T) {
		got := replayAll(t, s, mine.ID, 0)
		if len(got) != 3 {
			t.Fatalf("replayed %d placements, want this room's 3: %+v", len(got), got)
		}
		for _, p := range got {
			if p.Colour > 9 {
				t.Errorf("replay returned %+v, which is the room next door's paint", p)
			}
		}
	})
	t.Run("History", func(t *testing.T) {
		got, err := s.History(ctx, mine.ID, 0, 100)
		if err != nil || len(got) != 3 {
			t.Fatalf("history has %d entries (err %v), want this room's 3", len(got), err)
		}
		for _, e := range got {
			if e.Color > 9 {
				t.Errorf("history returned %+v, which is the room next door's paint", e)
			}
		}
	})
	t.Run("CountPlacements", func(t *testing.T) {
		if got, err := s.CountPlacements(ctx, mine.ID); err != nil || got != 3 {
			t.Errorf("count = %d (err %v), want 3; 7 would be both rooms added together", got, err)
		}
	})
	t.Run("Leaderboard", func(t *testing.T) {
		got, err := s.Leaderboard(ctx, mine.ID, 10)
		if err != nil {
			t.Fatalf("leaderboard: %v", err)
		}
		for _, row := range got {
			if row.UID == "carl" {
				t.Errorf("carl is on this room's leaderboard with %d placements, and he has only ever painted next door",
					row.Count)
			}
		}
		if len(got) != 2 {
			t.Errorf("leaderboard has %d painters, want this room's 2: %+v", len(got), got)
		}
	})
	t.Run("CellHistory", func(t *testing.T) {
		got, err := s.CellHistory(ctx, mine.ID, 2, 3, 10)
		if err != nil || len(got) != 2 {
			t.Fatalf("cell history has %d entries (err %v), want this room's 2: %+v", len(got), err, got)
		}
		if got[0].Seq != 2 || got[0].Color != 5 {
			t.Errorf("newest entry = %+v, want this room's sequence 2 colour 5", got[0])
		}
	})
	t.Run("TopPlacementAt", func(t *testing.T) {
		got, err := s.TopPlacementAt(ctx, mine.ID, 2, 3)
		if err != nil {
			t.Fatalf("top placement: %v", err)
		}
		if got.Seq != 2 || got.Color != 5 {
			t.Errorf("top = %+v, want this room's sequence 2 colour 5; sequence 4 colour 14 is next door's", got)
		}
	})
	t.Run("ColourBeneath", func(t *testing.T) {
		got, err := s.ColourBeneath(ctx, mine.ID, 2, 3, 2)
		if err != nil {
			t.Fatalf("colour beneath: %v", err)
		}
		if got != 3 {
			t.Errorf("colour beneath = %d, want this room's 3; 11 is what is under the same cell next door", got)
		}
	})
	t.Run("LatestOwnPlacement", func(t *testing.T) {
		got, err := s.LatestOwnPlacement(ctx, mine.ID, "bess")
		if err != nil {
			t.Fatalf("latest own placement: %v", err)
		}
		if got.Seq != 3 || got.Color != 9 {
			t.Errorf("bess's latest = %+v, want this room's sequence 3 colour 9", got)
		}
		// Carl has painted next door and nowhere else. Undo asks this question
		// first, so a leak here lets him retire a stranger's pixel.
		if _, err := s.LatestOwnPlacement(ctx, mine.ID, "carl"); !errors.Is(err, ErrNotFound) {
			t.Errorf("carl has a latest placement here (err %v), and he has only ever painted next door", err)
		}
	})
	t.Run("Bans", func(t *testing.T) {
		got, err := s.Bans(ctx, mine.ID)
		if err != nil {
			t.Fatalf("bans: %v", err)
		}
		if got["carl"] {
			t.Error("carl is banned here, and he was only ever banned next door")
		}
		if len(got) != 0 {
			t.Errorf("this room has bans %v, want none of its own", got)
		}
		if next, err := s.Bans(ctx, theirs.ID); err != nil || !next["carl"] {
			t.Errorf("the ban next door reads back as %v (err %v), want carl banned there", next, err)
		}
	})
	t.Run("LoadSnapshot", func(t *testing.T) {
		got, err := s.LoadSnapshot(ctx, mine.ID)
		if err != nil {
			t.Fatalf("load snapshot: %v", err)
		}
		if string(got.Pixels) != string([]byte{3, 5}) || got.Seq != 3 {
			t.Errorf("snapshot = %v seq %d, want this room's [3 5] at sequence 3", got.Pixels, got.Seq)
		}
	})

	// The writes are scoped too, and getting one of these wrong is a moderator
	// action landing on a canvas they have never seen.
	t.Run("UndoUser", func(t *testing.T) {
		n, err := s.UndoUser(ctx, mine.ID, "bess")
		if err != nil || n != 2 {
			t.Fatalf("undoing bess retired %d placements (err %v), want this room's 2", n, err)
		}
		if got, err := s.CountPlacements(ctx, theirs.ID); err != nil || got != 4 {
			t.Errorf("the room next door has %d live placements (err %v), want its untouched 4", got, err)
		}
	})
	t.Run("ClearRoom", func(t *testing.T) {
		if err := s.ClearRoom(ctx, mine.ID); err != nil {
			t.Fatalf("clearing: %v", err)
		}
		if got, err := s.CountPlacements(ctx, theirs.ID); err != nil || got != 4 {
			t.Errorf("clearing this room left %d live placements next door (err %v), want its untouched 4", got, err)
		}
	})
}

// TestUndoQueriesOnUntouchedGround covers the answers undo has to give when
// there is nothing to answer with. Each of these is reached by a real click -
// undo on a fresh canvas, the history panel on a cell nobody has painted - and
// each has to be an empty answer rather than an error, because a handler cannot
// tell the difference between "nothing here" and "the database is down" if they
// arrive the same way.
func TestUndoQueriesOnUntouchedGround(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx(t)
	r := newTestRoom(t, s)

	// A cell in a room where nothing has ever been painted.
	if got, err := s.CellHistory(ctx, r.ID, 9, 9, 10); err != nil || len(got) != 0 {
		t.Errorf("the history of an unpainted cell = %+v (err %v), want an empty list", got, err)
	}
	if colour, err := s.ColourBeneath(ctx, r.ID, 9, 9, 1); err != nil || colour != 0 {
		t.Errorf("the colour beneath an unpainted cell = %d (err %v), want the background", colour, err)
	}
	if got, err := s.Leaderboard(ctx, r.ID, 10); err != nil || len(got) != 0 {
		t.Errorf("the leaderboard of an empty room = %+v (err %v), want an empty list", got, err)
	}
	if got, err := s.CountPlacements(ctx, r.ID); err != nil || got != 0 {
		t.Errorf("the placement count of an empty room = %d (err %v), want 0", got, err)
	}

	// A cell whose every placement has been retired is the same question with a
	// different history: it is unpainted now, and what is underneath is the
	// background rather than the last thing somebody took back.
	paintAll(t, s, r.ID, []Placement{
		{Seq: 1, X: 3, Y: 3, Color: 5, UID: "bess", At: time.Now()},
		{Seq: 2, X: 3, Y: 3, Color: 7, UID: "alec", At: time.Now()},
		{Seq: 3, X: 3, Y: 3, Color: 9, UID: "carl", At: time.Now()},
	})
	for _, seq := range []int64{1, 2} {
		if err := s.MarkUndone(ctx, r.ID, seq); err != nil {
			t.Fatalf("marking undone: %v", err)
		}
	}
	if colour, err := s.ColourBeneath(ctx, r.ID, 3, 3, 3); err != nil || colour != 0 {
		t.Errorf("the colour beneath carl's placement = %d (err %v), want the background: both placements under it were taken back",
			colour, err)
	}
	if err := s.MarkUndone(ctx, r.ID, 3); err != nil {
		t.Fatalf("marking undone: %v", err)
	}
	if _, err := s.TopPlacementAt(ctx, r.ID, 3, 3); !errors.Is(err, ErrNotFound) {
		t.Errorf("the top placement of a cell whose history is entirely retired = %v, want ErrNotFound", err)
	}
	// The history panel still has all three, because they happened.
	if got, err := s.CellHistory(ctx, r.ID, 3, 3, 10); err != nil || len(got) != 3 {
		t.Errorf("the history of the retired cell has %d entries (err %v), want all 3", len(got), err)
	}
}
