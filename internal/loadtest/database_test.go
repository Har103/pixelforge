package loadtest

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// mustRoomID looks up a resident room's database id.
func mustRoomID(t *testing.T, h *harness, slug string) int64 {
	t.Helper()
	rm, ok := h.registry.Lookup(slug)
	if !ok {
		t.Fatalf("room %s is not resident", slug)
	}
	return rm.Meta.ID
}

// TestLoadSlowDatabase puts a TCP proxy between the server and PostgreSQL and
// then makes life difficult: first latency, then a complete cut, then recovery.
//
// The questions are the ones an operator asks at three in the morning. Does it
// stay up? Does it keep serving reads? Does it come back on its own? And does
// it lose anything it had already told a painter was theirs?
func TestLoadSlowDatabase(t *testing.T) {
	requireLoadtest(t)
	dsn := requireDSN(t)
	cond := measureConditions()

	t.Run("Latency", func(t *testing.T) { dbLatency(t, dsn, cond) })
	t.Run("Outage", func(t *testing.T) { dbOutage(t, dsn, cond) })
	t.Run("EphemeralMode", func(t *testing.T) { dbEphemeral(t, cond) })
}

// dbLatency walks the database further and further away and watches what the
// paint path and the read paths do about it.
func dbLatency(t *testing.T, dsn string, cond conditions) {
	pgAddr, database, user, pass := splitDSN(t, dsn)
	proxy, err := newDBProxy(pgAddr)
	if err != nil {
		t.Fatalf("starting the database proxy: %v", err)
	}
	defer proxy.Close()

	h := newHarness(t, harnessOpts{dsn: proxy.dsn(user, pass, database)})

	results := newTable("db latency", "paint rate/s", "hist-drops", "paint round trip",
		"GET snapshot", "GET stats (hits db)", "readyz")

	for _, lat := range []time.Duration{0, 5 * time.Millisecond, 25 * time.Millisecond, 100 * time.Millisecond} {
		// A fresh room per step. Reusing one means the second step offers the
		// same colours to the same cells the first step already set, and the
		// canvas correctly refuses every one of them - which reads as the
		// database latency having destroyed the paint rate when in fact the
		// experiment destroyed its own workload.
		proxy.SetLatency(0)
		slug, _ := createRoom(t, h.base, "db-latency", 128, 128, 0)
		proxy.SetLatency(lat)

		r := runPaced(t, h, pacedOpts{
			slug: slug, clients: 16, targetPS: 3_000, dur: scaled(6 * time.Second), timed: 4,
		})

		// Reads that never touch the database, against reads that always do.
		// The whole write-behind design is a bet that the first kind stays fast
		// while the second suffers, and that is testable.
		snapLat := timeRequests(h.base+"/api/r/"+slug+"/snapshot", 20)
		statsLat := timeRequests(h.base+"/api/r/"+slug+"/stats", 10)
		readyLat, readyCode := timeOne(h.base + "/readyz")

		results.add(lat.String(), fmt.Sprintf("%.0f", r.acceptedRate()),
			fmt.Sprint(r.histDrops), r.lat.String(),
			round(snapLat).String(), round(statsLat).String(),
			fmt.Sprintf("%s in %s", readyCode, round(readyLat)))
	}
	proxy.SetLatency(0)

	t.Logf("\nDATABASE LATENCY\nconditions: %s\n"+
		"128x128 room, cooldown 0, 16 sockets offering 3,000 placements/s, pool size 4,\n"+
		"PostgreSQL reached through a loopback proxy adding the stated latency each way.\n"+
		"GET snapshot serves the in-memory grid; GET stats runs three queries.\n%s\n%s",
		cond, results, proxy.stats())
}

// dbOutage cuts the database out from under a running server.
func dbOutage(t *testing.T, dsn string, cond conditions) {
	pgAddr, database, user, pass := splitDSN(t, dsn)
	proxy, err := newDBProxy(pgAddr)
	if err != nil {
		t.Fatalf("starting the database proxy: %v", err)
	}
	defer proxy.Close()

	h := newHarness(t, harnessOpts{dsn: proxy.dsn(user, pass, database)})
	resident, _ := createRoom(t, h.base, "db-outage-resident", 128, 128, 0)

	// A slug that is valid but has never existed. Answering "is there such a
	// room" is the one read that unavoidably needs the database, so it is the
	// honest probe for "can a visitor reach a room that is not already in
	// memory" - a genuinely cold room would have to be evicted first, which
	// would need sixty-four others.
	notInMemory := "absent-room-9999"

	// Paint something before the outage so there is a known good state.
	pre := runPaced(t, h, pacedOpts{
		slug: resident, clients: 8, targetPS: 1_000, dur: scaled(4 * time.Second), timed: 2,
	})
	_, seqBeforeOutage := quiesce(t, h, resident)
	time.Sleep(1500 * time.Millisecond) // let the write-behind loop flush

	// ---------------------------------------------------------- the outage --
	errsBefore := h.logs.count(msgWritePlace)
	proxy.Cut()
	time.Sleep(500 * time.Millisecond)

	during := probe(t, h, resident, notInMemory)

	// Paint through the outage. Every one of these is acknowledged to its
	// painter and echoed to every other tab in the room.
	out := runPaced(t, h, pacedOpts{
		slug: resident, clients: 8, targetPS: 1_000, dur: scaled(6 * time.Second), timed: 2,
	})
	writeErrors := h.logs.count(msgWritePlace) - errsBefore

	// ------------------------------------------------------- the recovery ---
	proxy.Restore()
	recovered := time.Duration(-1)
	t0 := time.Now()
	for time.Since(t0) < scaled(60*time.Second) {
		if _, code := timeOne(h.base + "/readyz"); code == "200" {
			recovered = time.Since(t0)
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	after := probe(t, h, resident, notInMemory)

	// Ground truth is taken after the last thing that writes to this room -
	// including the probes above, which paint a pixel of their own. Capturing
	// it earlier and then probing would report the probe as data loss.
	paintedDuringOutage, seqAfterOutage := quiesce(t, h, resident)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	rowsAfter, _ := h.store.CountPlacements(ctx, mustRoomID(t, h, resident))
	cancel()

	// Does the state that was acknowledged during the outage survive a restart
	// once the database is back? The write-behind loop discards a batch it
	// could not write, so the only thing that can save those placements is a
	// snapshot - and snapshots were failing too, until now.
	rm, ok := h.registry.Lookup(resident)
	if !ok {
		t.Fatalf("room %s is not resident after recovery", resident)
	}
	snapSeq := waitForSnapshotSeq(t, h.store, rm.Meta.ID, seqAfterOutage, scaled(45*time.Second))

	h.Close()
	h2 := newHarness(t, harnessOpts{dsn: proxy.dsn(user, pass, database)})
	restored, restoredSeq, err := snapshotGrid(h2.base, resident)
	if err != nil {
		t.Fatalf("reading the restored grid: %v", err)
	}
	diff := gridDiff(paintedDuringOutage, restored, 128)
	_ = h2

	t.Logf("\nDATABASE OUTAGE AND RECOVERY\nconditions: %s\n"+
		"128x128 room already resident, plus a second room deliberately left cold.\n"+
		"The proxy closes every connection and refuses new ones for the duration.\n\n"+
		"before the outage: %d placements accepted, canvas seq %d\n\n"+
		"%-38s %-22s %-22s\n%-38s %-22s %-22s\n"+
		"%-38s %-22v %-22v\n"+
		"%-38s %-22v %-22v\n"+
		"%-38s %-22v %-22v\n"+
		"%-38s %-22v %-22v\n"+
		"%-38s %-22v %-22v\n"+
		"%-38s %-22v %-22v\n\n"+
		"during the outage:\n"+
		"  placements accepted ................. %d (%.0f/s), canvas seq %d -> %d\n"+
		"  \"writing placements\" errors logged ... %d\n"+
		"  history entries dropped by the queue  %d\n"+
		"  rows in room_placements afterwards .. %d against canvas seq %d\n\n"+
		"recovery:\n"+
		"  /readyz went green again after ...... %s\n"+
		"  snapshot reached seq ................ %d (wanted %d)\n"+
		"  restart: restored canvas seq ........ %d\n"+
		"  cells differing from what was painted %d of %d\n",
		cond, pre.accepted, seqBeforeOutage,
		"", "during outage", "after recovery",
		"", "-------------", "--------------",
		"GET /healthz", during.health, after.health,
		"GET /readyz", during.ready, after.ready,
		"GET snapshot of a RESIDENT room", during.residentSnapshot, after.residentSnapshot,
		"POST place in a RESIDENT room", during.residentPlace, after.residentPlace,
		"GET config of a room NOT in memory", during.coldConfig, after.coldConfig,
		"POST /api/rooms (create)", during.create, after.create,
		out.accepted, out.acceptedRate(), seqBeforeOutage, seqAfterOutage,
		writeErrors, out.histDrops, rowsAfter, seqAfterOutage,
		recovered, snapSeq, seqAfterOutage, restoredSeq, diff.cells, len(paintedDuringOutage))

	if recovered < 0 {
		t.Errorf("the server never reported ready again after the database came back")
	}
	if during.residentSnapshot != "200" {
		t.Errorf("a resident room stopped serving its grid during a database outage (got %s); "+
			"the in-memory canvas needs no database to be read", during.residentSnapshot)
	}
	if lost := seqAfterOutage - rowsAfter; lost > 0 {
		t.Logf("HISTORY LOST ACROSS THE OUTAGE: %d of %d placements have no row in "+
			"room_placements. room.writeBehind's write() resets pending to empty whether "+
			"or not AppendPlacements succeeded, so a batch that fails is discarded rather "+
			"than retried. The grid is rescued by the next snapshot; the log is not, and "+
			"the leaderboard, the time-lapse, per-cell provenance and undo all read the log.",
			lost, seqAfterOutage)
	}
	if diff.cells != 0 {
		t.Errorf("DATA LOSS ACROSS AN OUTAGE: %d of %d cells painted while the database was "+
			"unreachable did not survive a restart afterwards, even though the database came "+
			"back and a snapshot was written at seq %d. First divergence at (%d,%d): painted "+
			"colour %d, restored colour %d",
			diff.cells, len(paintedDuringOutage), snapSeq,
			diff.firstX, diff.firstY, diff.firstWant, diff.firstGot)
	}
}

// probeResult is what every interesting endpoint answers at one moment.
type probeResult struct {
	health           string
	ready            string
	residentSnapshot string
	residentPlace    string
	coldConfig       string
	create           string
}

func probe(t *testing.T, h *harness, resident, cold string) probeResult {
	t.Helper()
	var r probeResult
	_, r.health = timeOne(h.base + "/healthz")
	_, r.ready = timeOne(h.base + "/readyz")
	_, r.residentSnapshot = timeOne(h.base + "/api/r/" + resident + "/snapshot")

	p := newPainter(h.base)
	defer p.close()
	if status, err := p.place(resident, 3, 3, 5); err != nil {
		r.residentPlace = "error: " + err.Error()
	} else {
		r.residentPlace = fmt.Sprint(status)
	}
	_, r.coldConfig = timeOne(h.base + "/api/r/" + cold + "/config")

	status, _, err := p.postJSON("/api/rooms", map[string]any{
		"name": "probe", "width": 32, "height": 32, "cooldownMs": 0,
	})
	if err != nil {
		r.create = "error: " + err.Error()
	} else {
		r.create = fmt.Sprint(status)
	}
	return r
}

// dbEphemeral checks the no-database mode, which the project offers as the
// answer to "the database is not there".
func dbEphemeral(t *testing.T, cond conditions) {
	h := newHarness(t, harnessOpts{ephemeral: true})

	health := h.health()
	if !health.Ephemeral {
		t.Fatalf("a store built with a nil pool did not report ephemeral")
	}
	_, ready := timeOne(h.base + "/readyz")
	_, home := timeOne(h.base + "/")

	p := newPainter(h.base)
	defer p.close()
	create, body, err := p.postJSON("/api/rooms", map[string]any{
		"name": "ephemeral", "width": 32, "height": 32, "cooldownMs": 0,
	})
	if err != nil {
		t.Fatalf("creating a room in ephemeral mode: %v", err)
	}

	t.Logf("\nEPHEMERAL MODE (no database at all)\nconditions: %s\n"+
		"  GET /healthz reports ephemeral ... %v\n"+
		"  GET /readyz ...................... %s\n"+
		"  GET / ............................ %s\n"+
		"  POST /api/rooms .................. %d %v\n\n"+
		"Note on the transition. store.Ephemeral() is `s.pool == nil`, fixed when the\n"+
		"store is built, so there is no path from \"database configured\" into ephemeral\n"+
		"mode at runtime: a server whose database dies keeps trying, and a server whose\n"+
		"database is unreachable at boot exits instead - cmd/pixelforge returns\n"+
		"\"database never came up\" from run() after pool.WaitReady fails, and main calls\n"+
		"os.Exit(1). Ephemeral mode is reached only by starting with no DATABASE_URL.\n",
		cond, health.Ephemeral, ready, home, create, body["error"])

	if ready != "200" {
		t.Errorf("ephemeral mode should still report ready (it is a supported mode); got %s", ready)
	}
	if create != http.StatusServiceUnavailable {
		t.Errorf("creating a room without a database returned %d, want 503", create)
	}
}

// ------------------------------------------------------------------ helpers --

func timeOne(url string) (time.Duration, string) {
	t0 := time.Now()
	res, err := http.Get(url)
	if err != nil {
		return time.Since(t0), "error: " + err.Error()
	}
	defer res.Body.Close()
	drain := make([]byte, 4096)
	for {
		if _, err := res.Body.Read(drain); err != nil {
			break
		}
	}
	return time.Since(t0), fmt.Sprint(res.StatusCode)
}

// timeRequests returns the median of n sequential requests, which is enough to
// separate "the database is in this path" from "it is not".
func timeRequests(url string, n int) time.Duration {
	lat := newLatencies(n)
	local := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		d, _ := timeOne(url)
		local = append(local, d)
	}
	lat.addLocal(local)
	return lat.stats().P50
}
