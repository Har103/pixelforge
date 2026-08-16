package room

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Har103/pixelforge/internal/canvas"
	"github.com/Har103/pixelforge/internal/hub"
	"github.com/Har103/pixelforge/internal/pg"
	"github.com/Har103/pixelforge/internal/store"
)

// UndoOwn answers "what was underneath this pixel" from the placement log, so
// there is nothing here to test without a real log. Point PIXELFORGE_TEST_DSN
// at a throwaway database to run these, the way the API tests do.
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("PIXELFORGE_TEST_DSN"))
	if dsn == "" {
		t.Skip("set PIXELFORGE_TEST_DSN to run the database-backed undo tests")
	}
	return dsn
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// newLiveRoom stands a real room up on a real database - registry, grid, hub
// and write-behind loop - and gives every test its own, so no test can undo a
// placement another one made. The room and its history are deleted afterwards.
func newLiveRoom(t *testing.T, cooldown time.Duration) (*Room, *store.Store, *pg.Pool) {
	t.Helper()
	dsn := testDSN(t)
	log := quietLog()

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
	st := store.New(pool, log)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrating: %v", err)
	}

	registry := NewRegistry(st, log)
	rm, err := registry.Create(ctx, Spec{
		Name:       "undo test",
		Width:      32,
		Height:     32,
		CooldownMs: int(cooldown.Milliseconds()),
		Unlisted:   true,
	})
	if err != nil {
		t.Fatalf("creating test room: %v", err)
	}

	// Cleanups run last-registered-first, so the deletion is registered before
	// the shutdown: the room has to finish writing its final snapshot before
	// the rows it belongs to go away.
	t.Cleanup(func() { dropRoom(t, pool, rm.Meta.ID) })
	t.Cleanup(rm.stop)
	return rm, st, pool
}

func dropRoom(t *testing.T, pool *pg.Pool, roomID int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, sql := range []string{
		`delete from room_placements where room_id = $1`,
		`delete from room_snapshots where room_id = $1`,
		`delete from bans where room_id = $1`,
		`delete from rooms where id = $1`,
	} {
		if _, err := pool.Query(ctx, sql, roomID); err != nil {
			t.Errorf("cleaning up room %d: %v", roomID, err)
		}
	}
}

// paint places a pixel and waits for the write-behind loop to land it in the
// database. UndoOwn reads the log rather than the grid, so undoing before the
// row is written would be testing a race instead of the feature.
func paint(t *testing.T, rm *Room, pool *pg.Pool, x, y int, colour uint8, uid string) canvas.Pixel {
	t.Helper()
	px, err := rm.Place(x, y, colour, uid)
	if err != nil {
		t.Fatalf("%s painting (%d,%d) colour %d: %v", uid, x, y, colour, err)
	}
	waitForSeq(t, pool, rm.Meta.ID, px.Seq)
	return px
}

func waitForSeq(t *testing.T, pool *pg.Pool, roomID, seq int64) {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		row, err := pool.QueryRow(ctx,
			`select 1 from room_placements where room_id = $1 and room_seq = $2`, roomID, seq)
		cancel()
		if err != nil {
			t.Fatalf("looking for placement %d: %v", seq, err)
		}
		if row != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("placement %d never reached the database", seq)
}

// settle waits out a couple of write-behind flushes, so a test that reads the
// log next sees everything the room meant to write - including anything it
// should not have written.
func settle() { time.Sleep(600 * time.Millisecond) }

func pixelAt(t *testing.T, rm *Room, x, y int) uint8 {
	t.Helper()
	pixels, _ := rm.Canvas.Snapshot()
	return pixels[y*rm.Canvas.Width()+x]
}

// retired reports whether the log has marked one placement undone.
func retired(t *testing.T, st *store.Store, roomID int64, x, y int, seq int64) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	entries, err := st.CellHistory(ctx, roomID, x, y, 100)
	if err != nil {
		t.Fatalf("reading cell history: %v", err)
	}
	for _, e := range entries {
		if e.Seq == seq {
			return e.Undone
		}
	}
	t.Fatalf("placement %d is not in the log for (%d,%d) at all", seq, x, y)
	return false
}

// pixelFrame is the shape the hub puts on the wire for SSE clients.
type pixelFrame struct {
	T string         `json:"t"`
	P []canvas.Pixel `json:"p"`
}

func nextPixelFrame(sub *hub.Subscriber, wait time.Duration) (canvas.Pixel, bool) {
	deadline := time.After(wait)
	for {
		select {
		case f, ok := <-sub.C:
			if !ok {
				return canvas.Pixel{}, false
			}
			var msg pixelFrame
			if err := json.Unmarshal(f.Data, &msg); err != nil || msg.T != "px" || len(msg.P) == 0 {
				continue
			}
			return msg.P[0], true
		case <-deadline:
			return canvas.Pixel{}, false
		}
	}
}

// undoneFrame is how a retraction reaches clients. It is deliberately not a
// "px" frame: an undo is not a placement, and a client that treated it as one
// would count it in the room's placement total and drift one ahead of the
// server on every undo.
type undoneFrame struct {
	T   string `json:"t"`
	X   int    `json:"x"`
	Y   int    `json:"y"`
	C   uint8  `json:"c"`
	UID string `json:"uid"`
}

func nextUndoneFrame(t *testing.T, sub *hub.Subscriber, wait time.Duration) (undoneFrame, bool) {
	t.Helper()
	deadline := time.After(wait)
	for {
		select {
		case f, ok := <-sub.C:
			if !ok {
				return undoneFrame{}, false
			}
			var msg undoneFrame
			if err := json.Unmarshal(f.Data, &msg); err != nil {
				continue
			}
			if msg.T == "px" {
				t.Error("the undo went out as a px frame, which every client will count as a placement")
			}
			if msg.T != "undone" {
				continue
			}
			return msg, true
		case <-deadline:
			return undoneFrame{}, false
		}
	}
}

// ------------------------------------------------------------- undoing one --

// TestUndoOwnRestoresTheColourUnderneath is the point of the whole feature. The
// grid alone cannot answer it: once alec paints over bess, the only record that
// bess's colour was ever there is the log.
func TestUndoOwnRestoresTheColourUnderneath(t *testing.T) {
	rm, st, pool := newLiveRoom(t, 0)
	ctx := testCtx(t)

	bess := paint(t, rm, pool, 4, 5, 3, "bess")
	// A pixel on the transposed cell, because x and y are two integers of the
	// same type and a swap anywhere in this path compiles perfectly.
	paint(t, rm, pool, 5, 4, 11, "carl")
	alec := paint(t, rm, pool, 4, 5, 7, "alec")

	px, err := rm.UndoOwn(ctx, "alec")
	if err != nil {
		t.Fatalf("undo: %v", err)
	}
	if px.X != 4 || px.Y != 5 {
		t.Errorf("undo reported (%d,%d), want the cell alec painted, (4,5)", px.X, px.Y)
	}
	if px.Color != 3 {
		t.Errorf("undo restored colour %d, want bess's 3 — reverting to the background erases a pixel alec never placed", px.Color)
	}
	if got := pixelAt(t, rm, 4, 5); got != 3 {
		t.Errorf("canvas (4,5) = %d, want bess's 3 back on the grid", got)
	}
	if got := pixelAt(t, rm, 5, 4); got != 11 {
		t.Errorf("canvas (5,4) = %d, want carl's 11: the undo landed on the transposed cell", got)
	}
	if !retired(t, st, rm.Meta.ID, 4, 5, alec.Seq) {
		t.Error("alec's placement is still live in the log, so a rebuild would paint it straight back")
	}
	if retired(t, st, rm.Meta.ID, 4, 5, bess.Seq) {
		t.Error("undoing alec's pixel retired bess's as well")
	}

	// An undo retires a placement rather than adding one. Everything that
	// counts a room's history - the leaderboard, the time-lapse, the browse
	// page - counts live rows, so a repair booked as a placement would credit
	// alec with a pixel he spent the day taking back.
	live, err := st.CountPlacements(ctx, rm.Meta.ID)
	if err != nil {
		t.Fatalf("counting placements: %v", err)
	}
	if live != 2 {
		t.Errorf("%d live placements after the undo, want bess's and carl's 2", live)
	}
}

func TestUndoOwnRestoresTheBackgroundWhenNothingIsUnderneath(t *testing.T) {
	rm, st, pool := newLiveRoom(t, 0)
	ctx := testCtx(t)

	alec := paint(t, rm, pool, 7, 8, 6, "alec")

	px, err := rm.UndoOwn(ctx, "alec")
	if err != nil {
		t.Fatalf("undo: %v", err)
	}
	if px.Color != 0 {
		t.Errorf("undo restored colour %d on a cell nobody had painted before, want the background 0", px.Color)
	}
	if got := pixelAt(t, rm, 7, 8); got != 0 {
		t.Errorf("canvas (7,8) = %d, want the background 0", got)
	}
	if !retired(t, st, rm.Meta.ID, 7, 8, alec.Seq) {
		t.Error("alec's placement is still live in the log")
	}
}

// TestUndoOwnRefusesWhenSomebodyElseHasPaintedOver is the interesting half of
// the feature. Reverting to what was underneath would take away the pixel bess
// placed after alec - a stranger's work vanishing because somebody else pressed
// undo - so the refusal is the correct answer, and it has to leave the log
// exactly as it found it.
func TestUndoOwnRefusesWhenSomebodyElseHasPaintedOver(t *testing.T) {
	rm, st, pool := newLiveRoom(t, 0)
	ctx := testCtx(t)

	alec := paint(t, rm, pool, 2, 2, 5, "alec")
	bess := paint(t, rm, pool, 2, 2, 9, "bess")

	if _, err := rm.UndoOwn(ctx, "alec"); !errors.Is(err, ErrPaintedOver) {
		t.Fatalf("undo error = %v, want ErrPaintedOver", err)
	}
	if got := pixelAt(t, rm, 2, 2); got != 9 {
		t.Errorf("canvas (2,2) = %d, want bess's 9 left where she put it", got)
	}
	if retired(t, st, rm.Meta.ID, 2, 2, alec.Seq) {
		t.Error("the refusal retired alec's placement anyway, so the log no longer matches what is on the grid")
	}
	if retired(t, st, rm.Meta.ID, 2, 2, bess.Seq) {
		t.Error("the refusal retired bess's placement")
	}

	// The refusal is about this cell, not about alec: an earlier pixel of his
	// that nobody has touched is still his to take back once bess's is gone.
	if err := st.MarkUndone(ctx, rm.Meta.ID, bess.Seq); err != nil {
		t.Fatalf("retiring bess's placement: %v", err)
	}
	if _, err := rm.UndoOwn(ctx, "alec"); err != nil {
		t.Errorf("undo once nothing is on top of alec's pixel: %v", err)
	}
}

// TestUndoOwnTwiceWalksBackTwoPlacements presses undo twice on one cell. The
// second press has to find the placement the first one uncovered, which only
// works if an undo is a retraction rather than a placement of its own: a fresh
// row in the log would make itself the painter's most recent placement and the
// second undo would take back the repair instead of the mistake.
func TestUndoOwnTwiceWalksBackTwoPlacements(t *testing.T) {
	rm, st, pool := newLiveRoom(t, 0)
	ctx := testCtx(t)

	first := paint(t, rm, pool, 1, 1, 2, "alec")
	second := paint(t, rm, pool, 1, 1, 4, "alec")

	px, err := rm.UndoOwn(ctx, "alec")
	if err != nil {
		t.Fatalf("first undo: %v", err)
	}
	if px.Color != 2 || pixelAt(t, rm, 1, 1) != 2 {
		t.Fatalf("first undo left colour %d on the grid, want alec's earlier 2", pixelAt(t, rm, 1, 1))
	}
	settle()

	px, err = rm.UndoOwn(ctx, "alec")
	if err != nil {
		t.Fatalf("second undo: %v", err)
	}
	if px.X != 1 || px.Y != 1 {
		t.Errorf("second undo reported (%d,%d), want it to step back to (1,1)", px.X, px.Y)
	}
	if px.Color != 0 {
		t.Errorf("second undo restored colour %d, want the background 0: both of alec's placements are now taken back", px.Color)
	}
	if got := pixelAt(t, rm, 1, 1); got != 0 {
		t.Errorf("canvas (1,1) = %d after undoing both placements, want the background 0", got)
	}
	if !retired(t, st, rm.Meta.ID, 1, 1, second.Seq) || !retired(t, st, rm.Meta.ID, 1, 1, first.Seq) {
		t.Error("one of alec's two placements is still live in the log after he undid both")
	}
}

// TestUndoOwnWalksBackAcrossCells is the same walk, spread over the canvas: the
// second undo must find the painter's previous pixel wherever it is, not linger
// on the cell the first undo touched.
func TestUndoOwnWalksBackAcrossCells(t *testing.T) {
	rm, _, pool := newLiveRoom(t, 0)
	ctx := testCtx(t)

	paint(t, rm, pool, 1, 1, 2, "alec")
	paint(t, rm, pool, 6, 7, 4, "alec")

	if px, err := rm.UndoOwn(ctx, "alec"); err != nil || px.X != 6 || px.Y != 7 {
		t.Fatalf("first undo = (%d,%d) err %v, want the newest placement at (6,7)", px.X, px.Y, err)
	}
	if got := pixelAt(t, rm, 6, 7); got != 0 {
		t.Fatalf("canvas (6,7) = %d after undoing it, want the background 0", got)
	}
	settle()

	px, err := rm.UndoOwn(ctx, "alec")
	if err != nil {
		t.Fatalf("second undo: %v", err)
	}
	if px.X != 1 || px.Y != 1 {
		t.Errorf("second undo reported (%d,%d), want alec's earlier pixel at (1,1)", px.X, px.Y)
	}
	if got := pixelAt(t, rm, 1, 1); got != 0 {
		t.Errorf("canvas (1,1) = %d, want the background 0 now that alec has undone both of his pixels", got)
	}
}

// TestUndoOwnClearsTheCooldown covers the promise in the feature's own comment:
// undoing a misclick must not also cost the painter their turn. The cooldown
// here is a minute, so nothing but clearing it can let the next placement land.
func TestUndoOwnClearsTheCooldown(t *testing.T) {
	rm, _, pool := newLiveRoom(t, time.Minute)
	ctx := testCtx(t)

	paint(t, rm, pool, 3, 3, 5, "alec")

	// The cooldown is real, or the rest of this test proves nothing.
	if _, err := rm.Place(4, 4, 6, "alec"); !errors.Is(err, canvas.ErrCooldown) {
		t.Fatalf("second placement error = %v, want ErrCooldown", err)
	}

	if _, err := rm.UndoOwn(ctx, "alec"); err != nil {
		t.Fatalf("undo: %v", err)
	}
	if got := pixelAt(t, rm, 3, 3); got != 0 {
		t.Errorf("canvas (3,3) = %d, want the undone pixel gone", got)
	}
	if left := rm.Canvas.CooldownRemaining("alec", time.Now()); left != 0 {
		t.Errorf("cooldown after an undo = %v, want none", left)
	}
	if _, err := rm.Place(4, 4, 6, "alec"); err != nil {
		t.Errorf("painting straight after an undo: %v — the misclick has cost alec his turn after all", err)
	}
}

// TestUndoOwnReachesConnectedClients pins the other half of that promise. The
// painter's own cooldown is still running when they press undo - it is their
// pixel they are taking back, moments after placing it - and if that stops the
// undo being published, everybody watching keeps the pixel on screen while the
// grid, the exports and the log all say it is gone.
func TestUndoOwnReachesConnectedClients(t *testing.T) {
	rm, _, pool := newLiveRoom(t, time.Minute)
	ctx := testCtx(t)

	paint(t, rm, pool, 5, 5, 3, "bess")
	paint(t, rm, pool, 5, 5, 7, "alec")

	sub := rm.Hub.Subscribe("sse")
	defer rm.Hub.Unsubscribe(sub)

	if _, err := rm.UndoOwn(ctx, "alec"); err != nil {
		t.Fatalf("undo: %v", err)
	}

	msg, ok := nextUndoneFrame(t, sub, 3*time.Second)
	if !ok {
		t.Fatal("the undo was never broadcast, so every open tab still shows the pixel alec took back")
	}
	if msg.X != 5 || msg.Y != 5 || msg.C != 3 {
		t.Errorf("broadcast = (%d,%d) colour %d, want bess's 3 restored at (5,5)", msg.X, msg.Y, msg.C)
	}
	if msg.UID != "alec" {
		t.Errorf("broadcast uid = %q, want alec: a client needs to know whose pixel went", msg.UID)
	}
}

// TestUndoOwnWhenTheGridAlreadyAgrees exercises the branch that exists because
// the grid can be rebuilt underneath an undo: the repair is already on screen,
// so there is nothing to place and nothing to broadcast, but the placement must
// still be retired or the next rebuild brings it back.
func TestUndoOwnWhenTheGridAlreadyAgrees(t *testing.T) {
	rm, st, pool := newLiveRoom(t, 0)
	ctx := testCtx(t)

	paint(t, rm, pool, 3, 2, 11, "carl") // the transposed cell again
	alec := paint(t, rm, pool, 2, 3, 5, "alec")

	// Stand in for a rebuild that has already dropped alec's pixel: the grid
	// shows the background while the log still says the placement is live.
	rm.Canvas.Apply(2, 3, 0, rm.Canvas.Seq())

	px, err := rm.UndoOwn(ctx, "alec")
	if err != nil {
		t.Fatalf("undo when the grid already shows the colour underneath: %v", err)
	}
	if px.X != 2 || px.Y != 3 || px.Color != 0 {
		t.Errorf("undo reported (%d,%d) colour %d, want (2,3) colour 0", px.X, px.Y, px.Color)
	}
	if got := pixelAt(t, rm, 2, 3); got != 0 {
		t.Errorf("canvas (2,3) = %d, want the background 0", got)
	}
	if got := pixelAt(t, rm, 3, 2); got != 11 {
		t.Errorf("canvas (3,2) = %d, want carl's 11 left alone", got)
	}
	if !retired(t, st, rm.Meta.ID, 2, 3, alec.Seq) {
		t.Error("the placement is still live in the log, so the next rebuild paints it back")
	}
}

func TestUndoOwnRefusesWhenYouHavePaintedNothing(t *testing.T) {
	rm, _, pool := newLiveRoom(t, 0)
	ctx := testCtx(t)

	if _, err := rm.UndoOwn(ctx, "never-painted-here"); !errors.Is(err, ErrNothingToUndo) {
		t.Errorf("undo by a painter with no pixels = %v, want ErrNothingToUndo", err)
	}

	// Somebody else's work is not yours to take back either.
	paint(t, rm, pool, 9, 9, 4, "bess")
	if _, err := rm.UndoOwn(ctx, "alec"); !errors.Is(err, ErrNothingToUndo) {
		t.Errorf("undo by a painter who has only watched = %v, want ErrNothingToUndo", err)
	}

	// And once alec has taken back the one pixel he placed, he is back to
	// having nothing to undo rather than holding a phantom placement.
	paint(t, rm, pool, 10, 10, 6, "alec")
	if _, err := rm.UndoOwn(ctx, "alec"); err != nil {
		t.Fatalf("undo: %v", err)
	}
	settle()
	if _, err := rm.UndoOwn(ctx, "alec"); !errors.Is(err, ErrNothingToUndo) {
		t.Errorf("undoing again = %v, want ErrNothingToUndo", err)
	}
}

// TestUndoneWorkDoesNotComeBackWhenTheRoomIsRebuilt is the durability half. An
// undo that only edits the grid looks right until the room is next rebuilt from
// its history, at which point the pixel walks back onto the canvas.
func TestUndoneWorkDoesNotComeBackWhenTheRoomIsRebuilt(t *testing.T) {
	rm, st, pool := newLiveRoom(t, 0)
	ctx := testCtx(t)

	paint(t, rm, pool, 6, 6, 3, "bess")
	paint(t, rm, pool, 6, 6, 7, "alec")
	if _, err := rm.UndoOwn(ctx, "alec"); err != nil {
		t.Fatalf("undo: %v", err)
	}
	settle()

	// Replaying the surviving log over an empty grid is the strongest statement
	// the room can make about what really happened here.
	if err := rm.rebuild(ctx); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if got := pixelAt(t, rm, 6, 6); got != 3 {
		t.Errorf("after a rebuild (6,6) = %d, want bess's 3 (7 means alec's undone pixel came back, 0 means hers was lost)", got)
	}

	// And again through the path a restart takes: a registry that has never
	// seen this room loading it from scratch. The snapshot goes first so the
	// load has nothing but the log to work from.
	if _, err := pool.Query(ctx, `delete from room_snapshots where room_id = $1`, rm.Meta.ID); err != nil {
		t.Fatalf("dropping the snapshot: %v", err)
	}
	loaded, err := NewRegistry(st, quietLog()).Get(ctx, rm.Meta.Slug)
	if err != nil {
		t.Fatalf("loading the room afresh: %v", err)
	}
	t.Cleanup(loaded.stop)
	if got := pixelAt(t, loaded, 6, 6); got != 3 {
		t.Errorf("a room rebuilt from the log alone has (6,6) = %d, want bess's 3", got)
	}
}

// TestUndoSurvivesARestartFromAnOlderSnapshot covers the other restore path,
// the one a real restart usually takes. A room comes back from its newest
// snapshot and replays only the history after it, so an undo that leaves no
// trace anywhere - no snapshot, no row - is invisible to that replay and the
// pixel walks back onto the canvas the next time the process restarts.
func TestUndoSurvivesARestartFromAnOlderSnapshot(t *testing.T) {
	rm, st, pool := newLiveRoom(t, 0)
	ctx := testCtx(t)

	paint(t, rm, pool, 6, 6, 3, "bess")
	paint(t, rm, pool, 6, 6, 7, "alec")

	// A snapshot taken while alec's pixel is still on the grid, exactly as the
	// periodic one would have been.
	if err := rm.snapshot(ctx); err != nil {
		t.Fatalf("snapshotting: %v", err)
	}
	if _, err := rm.UndoOwn(ctx, "alec"); err != nil {
		t.Fatalf("undo: %v", err)
	}
	settle()

	loaded, err := NewRegistry(st, quietLog()).Get(ctx, rm.Meta.Slug)
	if err != nil {
		t.Fatalf("loading the room afresh: %v", err)
	}
	t.Cleanup(loaded.stop)
	if got := pixelAt(t, loaded, 6, 6); got != 3 {
		t.Errorf("a restarted room has (6,6) = %d, want bess's 3: alec's undone pixel is back from the snapshot", got)
	}
}
