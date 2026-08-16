package loadtest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Har103/pixelforge/internal/store"
)

// TestLoadWriteBehind interrogates the comment in room.record:
//
//	// The writer has fallen far enough behind that the buffer is full.
//	// Losing a line of history beats stalling everyone's paint; the next
//	// snapshot captures the pixel regardless.
//
// Three questions, in order:
//
//  1. At what placement rate does it actually start dropping, and how deep is
//     the buffer in seconds rather than in entries?
//  2. After a burst that provokes drops, does a snapshot genuinely capture the
//     dropped pixels, so a restart restores the right grid?
//  3. What is lost that a snapshot cannot restore?
//
// The restart in (2) is a real one: the server is shut down, a second server is
// built against the same database, and the grid it comes up with is compared
// byte for byte with the grid that was painted.
func TestLoadWriteBehind(t *testing.T) {
	requireLoadtest(t)
	dsn := requireDSN(t)
	cond := measureConditions()

	t.Run("DropOnset", func(t *testing.T) { writeBehindOnset(t, dsn, cond) })
	t.Run("SnapshotRescuesDroppedPixels", func(t *testing.T) { writeBehindSnapshotClaim(t, dsn, cond) })
	t.Run("CrashBeforeSnapshot", func(t *testing.T) { writeBehindCrash(t, dsn, cond) })
}

// ------------------------------------------------------- 1. the drop onset --

// writeBehindOnset finds the rate at which history starts being thrown away,
// with the database at its natural speed and then deliberately slowed, because
// the buffer's real unit is not entries but seconds-of-headroom and that
// depends entirely on how fast the writer drains.
func writeBehindOnset(t *testing.T, dsn string, cond conditions) {
	pgAddr, database, user, pass := splitDSN(t, dsn)

	proxy, err := newDBProxy(pgAddr)
	if err != nil {
		t.Fatalf("starting the database proxy: %v", err)
	}
	defer proxy.Close()

	h := newHarness(t, harnessOpts{dsn: proxy.dsn(user, pass, database)})

	results := newTable("db latency", "offered/s", "accepted", "achieved/s",
		"hist-drops", "drop%", "buffer depth", "broadcast latency")

	// queueCap is room.materialise's `make(chan store.Placement, 4096)`. It is
	// the number that decides everything here, so it is named rather than
	// buried in the arithmetic.
	const queueCap = 4096

	for _, dbLatency := range []time.Duration{0, 2 * time.Millisecond, 10 * time.Millisecond} {
		proxy.SetLatency(dbLatency)
		for _, target := range []int{2_000, 10_000, 30_000} {
			slug, _ := createRoom(t, h.base, "wb-onset", 256, 256, 0)
			r := runPaced(t, h, pacedOpts{
				slug: slug, clients: 32, targetPS: target,
				dur: scaled(8 * time.Second), timed: 2,
			})
			dropPct := 0.0
			if r.accepted > 0 {
				dropPct = 100 * float64(r.histDrops) / float64(r.accepted)
			}
			depth := time.Duration(float64(queueCap) / r.acceptedRate() * float64(time.Second))
			results.add(dbLatency.String(), fmt.Sprint(target), fmt.Sprint(r.accepted),
				fmt.Sprintf("%.0f", r.acceptedRate()), fmt.Sprint(r.histDrops),
				fmt.Sprintf("%.1f%%", dropPct), depth.Round(time.Millisecond).String(),
				r.lat.String())
		}
	}
	proxy.SetLatency(0)

	t.Logf("\nWRITE-BEHIND: WHEN DOES IT START DROPPING\nconditions: %s\n"+
		"room 256x256 cooldown=0, 32 sockets, pool size 4, PostgreSQL reached through a "+
		"loopback TCP proxy that adds the stated one-way latency.\n"+
		"buffer depth = 4096 queued entries divided by the achieved rate: how long the\n"+
		"write-behind loop may stall before history starts being thrown away.\n%s\n%s",
		cond, results, proxy.stats())
}

// ------------------------------------------- 2. does the snapshot rescue it --

// writeBehindSnapshotClaim provokes drops, waits for the periodic snapshot the
// comment relies on, restarts the server for real, and compares the grid.
//
// The burst is paced rather than unbounded on purpose. A fire-and-forget socket
// lets a client write far more than the server has read, so an unbounded burst
// leaves hundreds of thousands of placements sitting in socket buffers that the
// server keeps applying after the clients have gone - and a "grid as painted"
// read in the middle of that is not ground truth, it is a photograph of a
// moving object. A paced burst plus quiesce gives a grid that has stopped
// changing, which is the only thing worth comparing a restart against.
func writeBehindSnapshotClaim(t *testing.T, dsn string, cond conditions) {
	pgAddr, database, user, pass := splitDSN(t, dsn)
	proxy, err := newDBProxy(pgAddr)
	if err != nil {
		t.Fatalf("starting the database proxy: %v", err)
	}
	defer proxy.Close()

	h := newHarness(t, harnessOpts{dsn: proxy.dsn(user, pass, database)})
	slug, _ := createRoom(t, h.base, "wb-snapshot", 128, 128, 0)

	// 10ms each way is an ordinary number for a managed database in the same
	// region. It is enough to make the write-behind loop drop entries at a
	// placement rate the product would otherwise handle comfortably.
	proxy.SetLatency(10 * time.Millisecond)
	r := runPaced(t, h, pacedOpts{
		slug: slug, clients: 32, targetPS: 10_000, dur: scaled(8 * time.Second), timed: 1,
	})
	if r.histDrops == 0 {
		t.Skipf("no history entries were dropped (accepted %d at %.0f/s); nothing to test",
			r.accepted, r.acceptedRate())
	}

	painted, paintedSeq := quiesce(t, h, slug)
	t.Logf("burst: %d placements accepted at %.0f/s, %d history entries dropped; "+
		"canvas settled at seq %d", r.accepted, r.acceptedRate(), r.histDrops, paintedSeq)

	rm, ok := h.registry.Lookup(slug)
	if !ok {
		t.Fatalf("room %s is not resident after the burst", slug)
	}
	roomID := rm.Meta.ID

	// Give the database its speed back so the snapshot the claim depends on can
	// actually be written.
	proxy.SetLatency(0)

	// The claim is about "the next snapshot", so wait for one. The snapshot
	// ticker in room.writeBehind is 20 seconds; polling the stored seq is exact
	// where sleeping would be a guess.
	got := waitForSnapshotSeq(t, h.store, roomID, paintedSeq, scaled(60*time.Second))
	if got < paintedSeq {
		t.Fatalf("no snapshot at or past seq %d arrived within the wait (stored seq %d)",
			paintedSeq, got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	rows, err := h.store.CountPlacements(ctx, roomID)
	cancel()
	if err != nil {
		t.Fatalf("counting placements: %v", err)
	}

	// A real restart: this server goes away, a new one is built on the same
	// database and materialises the room from snapshot plus replay.
	h.Close()

	h2 := newHarness(t, harnessOpts{dsn: proxy.dsn(user, pass, database)})
	restored, restoredSeq, err := snapshotGrid(h2.base, slug)
	if err != nil {
		t.Fatalf("reading the restored grid: %v", err)
	}

	diff := gridDiff(painted, restored, 128)
	t.Logf("\nWRITE-BEHIND: DOES A SNAPSHOT RESCUE THE DROPPED PIXELS\nconditions: %s\n"+
		"room %s 128x128 cooldown=0, 32 sockets, 10,000 placements/s offered for %s,\n"+
		"database reached through a proxy adding 10ms each way.\n"+
		"  placements accepted ......... %d (canvas seq %d after quiescing)\n"+
		"  history entries dropped ..... %d\n"+
		"  rows in room_placements ..... %d\n"+
		"  history never written ....... %d (%.1f%% of what was painted)\n"+
		"  snapshot written at seq ..... %d\n"+
		"  clean shutdown, then a second server on the same database\n"+
		"  restored canvas seq ......... %d\n"+
		"  cells differing from painted  %d of %d\n",
		cond, slug, scaled(8*time.Second), r.accepted, paintedSeq, r.histDrops, rows,
		paintedSeq-rows, 100*float64(paintedSeq-rows)/float64(max64(paintedSeq, 1)),
		got, restoredSeq, diff.cells, len(painted))

	if diff.cells != 0 {
		t.Errorf("DATA LOSS: %d of %d cells differ after a clean restart; first at (%d,%d): "+
			"painted colour %d, restored colour %d",
			diff.cells, len(painted), diff.firstX, diff.firstY, diff.firstWant, diff.firstGot)
		return
	}
	t.Logf("VERDICT: the grid survived exactly. Every dropped history entry was recovered "+
		"by the snapshot, so the comment in room.record is CORRECT for the grid, given a "+
		"snapshot and a clean shutdown. It is not correct for the history: %d placements "+
		"(%.1f%%) have no row in room_placements and are invisible to the leaderboard, the "+
		"time-lapse, the per-cell provenance panel and undo.",
		paintedSeq-rows, 100*float64(paintedSeq-rows)/float64(max64(paintedSeq, 1)))
	_ = h2
}

// -------------------------------------------- 3. crash before the snapshot --

// writeBehindCrash asks the harder half of the same question. The comment says
// the next snapshot captures the pixel; the next snapshot is up to twenty
// seconds away, and nothing acknowledges the painter any differently in the
// meantime. This measures what a crash inside that window costs.
//
// The crash is produced by cutting the database at the TCP level and only then
// tearing the server down, so no shutdown snapshot and no final flush can
// land - which is exactly what the database sees when a process is killed.
func writeBehindCrash(t *testing.T, dsn string, cond conditions) {
	pgAddr, database, user, pass := splitDSN(t, dsn)
	proxy, err := newDBProxy(pgAddr)
	if err != nil {
		t.Fatalf("starting the database proxy: %v", err)
	}
	defer proxy.Close()

	h := newHarness(t, harnessOpts{dsn: proxy.dsn(user, pass, database)})
	slug, owner := createRoom(t, h.base, "wb-crash", 128, 128, 0)

	// Establish a definite snapshot before the burst, so "restored to the last
	// snapshot plus surviving rows" is distinguishable from "restored nothing".
	// Clearing the room is the one moderator action that writes a snapshot
	// synchronously, which makes the baseline exact instead of a 20s wait.
	if status, body, err := owner.postJSON("/api/r/"+slug+"/mod/clear", map[string]any{}); err != nil || status != 200 {
		t.Fatalf("clearing the room to force a baseline snapshot: status %d body %v err %v",
			status, body, err)
	}

	rm, ok := h.registry.Lookup(slug)
	if !ok {
		t.Fatalf("room %s not resident", slug)
	}
	roomID := rm.Meta.ID

	proxy.SetLatency(10 * time.Millisecond)
	r := runPaced(t, h, pacedOpts{
		slug: slug, clients: 32, targetPS: 10_000, dur: scaled(8 * time.Second), timed: 1,
	})
	if r.histDrops == 0 {
		t.Skipf("no history entries were dropped, so there is nothing to lose")
	}

	painted, paintedSeq := quiesce(t, h, slug)

	// Let the write-behind loop flush everything it still holds, so what is
	// missing afterwards is what record() threw away rather than what simply
	// had not been written yet. A flush tick is 250ms.
	proxy.SetLatency(0)
	time.Sleep(scaled(3 * time.Second))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	rows, _ := h.store.CountPlacements(ctx, roomID)
	snapBefore, snapErr := h.store.LoadSnapshot(ctx, roomID)
	cancel()
	snapSeq := int64(-1)
	if snapErr == nil {
		snapSeq = snapBefore.Seq
	}

	// The crash. Nothing the server does from here can reach the database, so
	// no shutdown snapshot and no final flush can land - which is exactly what
	// the database sees when a process is killed.
	proxy.Cut()
	h.killWithoutFlush()
	time.Sleep(500 * time.Millisecond)
	proxy.Restore()

	h2 := newHarness(t, harnessOpts{dsn: proxy.dsn(user, pass, database)})
	restored, restoredSeq, err := snapshotGrid(h2.base, slug)
	if err != nil {
		t.Fatalf("reading the restored grid: %v", err)
	}
	diff := gridDiff(painted, restored, 128)

	t.Logf("\nWRITE-BEHIND: A CRASH BEFORE THE NEXT SNAPSHOT\nconditions: %s\n"+
		"room %s 128x128 cooldown=0, cleared first to force a baseline snapshot, then\n"+
		"32 sockets offering 10,000 placements/s for %s through a proxy adding 10ms each\n"+
		"way; the write-behind queue is allowed to flush, and only then is the database\n"+
		"cut at the TCP level and the server torn down without a shutdown snapshot.\n"+
		"  placements accepted ........... %d (canvas seq %d after quiescing)\n"+
		"  history entries dropped ....... %d\n"+
		"  rows in room_placements ....... %d\n"+
		"  newest snapshot on disk ....... seq %d\n"+
		"  restored canvas seq ........... %d\n"+
		"  cells differing from painted .. %d of %d\n",
		cond, slug, scaled(8*time.Second), r.accepted, paintedSeq, r.histDrops, rows,
		snapSeq, restoredSeq, diff.cells, len(painted))

	if diff.cells == 0 {
		t.Logf("VERDICT: nothing was lost even without a shutdown snapshot - a periodic "+
			"snapshot landed after the drops. This outcome depends on where the 20s "+
			"snapshot tick fell (snapshot seq %d, painted seq %d).", snapSeq, paintedSeq)
		return
	}
	t.Logf("VERDICT: %d of %d cells (%.2f%%) do not survive a crash that lands between a "+
		"dropped history entry and the next 20-second snapshot. First divergence at "+
		"(%d,%d): painted colour %d, restored colour %d. Every one of these placements "+
		"was acknowledged to its painter and echoed to every other tab in the room, and "+
		"the write-behind queue had already been given time to flush - so this is not the "+
		"ordinary write-behind window, it is the dropped entries specifically.",
		diff.cells, len(painted), 100*float64(diff.cells)/float64(len(painted)),
		diff.firstX, diff.firstY, diff.firstWant, diff.firstGot)
	_ = h2
}

// ------------------------------------------------------------------ helpers --

type gridDifference struct {
	cells               int
	firstX, firstY      int
	firstWant, firstGot byte
}

// gridDiff compares two grids cell by cell. Both come from the same endpoint on
// two different servers, so any difference is the restore losing or inventing a
// pixel.
func gridDiff(want, got []byte, w int) gridDifference {
	d := gridDifference{firstX: -1, firstY: -1}
	n := len(want)
	if len(got) < n {
		n = len(got)
	}
	if w < 1 {
		w = 1
	}
	for i := 0; i < n; i++ {
		if want[i] != got[i] {
			if d.cells == 0 {
				d.firstX, d.firstY = i%w, i/w
				d.firstWant, d.firstGot = want[i], got[i]
			}
			d.cells++
		}
	}
	d.cells += abs(len(want) - len(got))
	return d
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// waitForSnapshotSeq polls the stored snapshot until it is at least as new as
// the grid that was painted, which is the event the comment in room.record
// promises will happen.
func waitForSnapshotSeq(t *testing.T, st *store.Store, roomID, want int64, within time.Duration) int64 {
	t.Helper()
	deadline := time.Now().Add(within)
	var last int64 = -1
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		snap, err := st.LoadSnapshot(ctx, roomID)
		cancel()
		if err == nil {
			last = snap.Seq
			if snap.Seq >= want {
				return snap.Seq
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return last
}

// quiesce waits for a room to stop changing, then reads its grid.
//
// It exists because the WebSocket paint path takes no acknowledgement: a client
// can write far more than the server has read, so when the last client hangs up
// the server is still applying placements out of its receive buffers. Reading
// the grid at that moment yields a photograph of a moving object, and comparing
// a restart against it reports data loss that never happened. Ask twice, and
// only believe the answer when the sequence number has stopped moving.
func quiesce(t *testing.T, h *harness, slug string) ([]byte, int64) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	var lastSeq int64 = -1
	stable := 0
	for time.Now().Before(deadline) {
		_, seq, err := snapshotGrid(h.base, slug)
		if err != nil {
			t.Fatalf("reading the grid while quiescing: %v", err)
		}
		if seq == lastSeq {
			stable++
			// Three consecutive identical readings, half a second apart, is
			// enough: the flush tick is 250ms and the hub tick is 50ms.
			if stable >= 3 {
				pixels, seq, err := snapshotGrid(h.base, slug)
				if err != nil {
					t.Fatalf("reading the settled grid: %v", err)
				}
				return pixels, seq
			}
		} else {
			stable = 0
			lastSeq = seq
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("room %s never stopped changing", slug)
	return nil, 0
}
