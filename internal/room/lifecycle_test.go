package room

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Har103/pixelforge/internal/pg"
	"github.com/Har103/pixelforge/internal/store"
)

// newRegistry stands up a registry on the real database and returns it with the
// pool, so a test can drive materialisation and eviction rather than being
// handed a room that already exists.
func newRegistry(t *testing.T) (*Registry, *store.Store, *pg.Pool) {
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
	return NewRegistry(st, log), st, pool
}

// makeRoom creates one room through the registry and arranges for its rows to
// be removed afterwards.
func makeRoom(t *testing.T, reg *Registry, pool *pg.Pool, name string) *Room {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rm, err := reg.Create(ctx, Spec{Name: name, Width: 16, Height: 16, CooldownMs: 0, Unlisted: true})
	if err != nil {
		t.Fatalf("creating room %q: %v", name, err)
	}
	t.Cleanup(func() { dropRoom(t, pool, rm.Meta.ID) })
	t.Cleanup(rm.stop)
	return rm
}

// ------------------------------------------------------------- eviction -----

// TestAnOccupiedRoomIsNeverEvicted pins the worst bug load testing found.
//
// evictIfCrowded used to pick the idlest resident room without asking whether
// anybody was in it, while sweep - the other eviction path - had always
// excluded occupied rooms. Evicting a room somebody is connected to stops the
// loop that persists its placements, but every open socket keeps painting into
// the object: pixels are accepted, broadcast to the people watching, and never
// written down. The next visitor resolves the same slug and gets a second,
// freshly loaded room that has never heard of any of it.
func TestAnOccupiedRoomIsNeverEvicted(t *testing.T) {
	reg, _, pool := newRegistry(t)
	reg.maxRooms = 2

	occupied := makeRoom(t, reg, pool, "occupied")
	sub := occupied.Hub.Subscribe("ws")
	defer occupied.Hub.Unsubscribe(sub)
	go func() {
		for range sub.C {
		}
	}()

	// Make the occupied room by far the idlest, so the old code would choose it
	// every time. Anything else would let this pass for the wrong reason.
	occupied.lastTouch.Store(time.Now().Add(-time.Hour).UnixNano())

	for i := 0; i < 4; i++ {
		rm := makeRoom(t, reg, pool, "filler")
		rm.Touch()
	}

	if occupied.Closed() {
		t.Fatal("the room somebody was connected to was evicted; their placements would be accepted and never written down")
	}
	reg.mu.Lock()
	_, resident := reg.rooms[occupied.Meta.Slug]
	reg.mu.Unlock()
	if !resident {
		t.Error("the occupied room was dropped from the registry, so the next visitor would load a second copy of the same canvas")
	}
}

// TestTheResidentCapYieldsToPeopleRatherThanDisconnectingThem covers the
// consequence of the rule above: when every resident room has somebody in it,
// the cap is exceeded rather than honoured. That is deliberate. The cap bounds
// memory, and no memory target is worth cutting people off mid-brushstroke.
func TestTheResidentCapYieldsToPeopleRatherThanDisconnectingThem(t *testing.T) {
	reg, _, pool := newRegistry(t)
	reg.maxRooms = 1

	var subs []*Subscriberish
	for i := 0; i < 3; i++ {
		rm := makeRoom(t, reg, pool, "busy")
		s := rm.Hub.Subscribe("ws")
		go func() {
			for range s.C {
			}
		}()
		subs = append(subs, &Subscriberish{room: rm, unsub: func() { rm.Hub.Unsubscribe(s) }})
	}
	defer func() {
		for _, s := range subs {
			s.unsub()
		}
	}()

	for _, s := range subs {
		if s.room.Closed() {
			t.Fatalf("room %q was evicted although somebody was connected to it", s.room.Meta.Slug)
		}
	}
	reg.mu.Lock()
	n := len(reg.rooms)
	reg.mu.Unlock()
	if n != 3 {
		t.Errorf("%d rooms resident, want all 3: the cap must give way to occupancy", n)
	}
}

// Subscriberish pairs a room with the closure that detaches its watcher, only
// so the test above can clean up in a loop.
type Subscriberish struct {
	room  *Room
	unsub func()
}

// TestAReleasedRoomRefusesToPaint is the second half of the same defect. Even
// with eviction fixed, shutdown still stops rooms that have people in them, and
// a request already holding the pointer would otherwise paint into a canvas
// whose writer has gone. Refusing sends the caller back to the registry.
func TestAReleasedRoomRefusesToPaint(t *testing.T) {
	reg, _, pool := newRegistry(t)
	rm := makeRoom(t, reg, pool, "released")

	if _, err := rm.Place(1, 1, 3, "alec"); err != nil {
		t.Fatalf("painting in a live room: %v", err)
	}
	if rm.Closed() {
		t.Fatal("a running room reports itself closed")
	}

	rm.stop()

	if !rm.Closed() {
		t.Error("a stopped room does not report itself closed, so nothing downstream can tell")
	}
	_, err := rm.Place(2, 2, 4, "alec")
	if !errors.Is(err, ErrRoomClosed) {
		t.Errorf("painting in a released room returned %v, want ErrRoomClosed - otherwise the pixel is accepted, broadcast, and lost", err)
	}
}

// ---------------------------------------------------------------- locks -----

// TestLocksSurviveTheRoomBeingReleased is the bug that made the moderation tool
// a lie. The locks table has existed since the first schema and nothing ever
// read or wrote it, so freezing a region was an in-memory decision: twenty
// minutes after the last person left, the room was released and came back
// without it, and the area a moderator had deliberately protected was quietly
// paintable again.
func TestLocksSurviveTheRoomBeingReleased(t *testing.T) {
	reg, _, pool := newRegistry(t)
	rm := makeRoom(t, reg, pool, "locked")
	ctx := testCtx(t)

	frozen := []Lock{{X1: 2, Y1: 2, X2: 5, Y2: 5}, {X1: 9, Y1: 1, X2: 10, Y2: 12}}
	if err := rm.SetLocks(ctx, frozen); err != nil {
		t.Fatalf("setting locks: %v", err)
	}
	if _, err := rm.Place(3, 3, 4, "alec"); !errors.Is(err, ErrLocked) {
		t.Fatalf("painting inside a fresh lock returned %v, want ErrLocked", err)
	}

	// Release it the way the registry does, then load the same canvas again.
	slug := rm.Meta.Slug
	rm.stop()
	reg.mu.Lock()
	delete(reg.rooms, slug)
	reg.mu.Unlock()

	reloaded, err := reg.Get(ctx, slug)
	if err != nil {
		t.Fatalf("reloading room %q: %v", slug, err)
	}
	t.Cleanup(reloaded.stop)

	got := reloaded.Locks()
	if len(got) != len(frozen) {
		t.Fatalf("the reloaded room has %d locked regions, want %d - the moderator's decision did not survive", len(got), len(frozen))
	}
	for i, want := range frozen {
		if got[i] != want {
			t.Errorf("lock %d is %+v, want %+v", i, got[i], want)
		}
	}
	if _, err := reloaded.Place(3, 3, 4, "alec"); !errors.Is(err, ErrLocked) {
		t.Errorf("painting inside a lock after a reload returned %v, want ErrLocked - the protected area is paintable again", err)
	}
	// And a cell outside every rectangle is still free, so this is not passing
	// because the whole canvas became unpaintable.
	if _, err := reloaded.Place(14, 14, 4, "bess"); err != nil {
		t.Errorf("painting outside every lock returned %v, want success", err)
	}
}

// TestClearingLocksIsAlsoRemembered covers the other direction. Unfreezing has
// to persist too, or a moderator who releases a region finds it frozen again
// after a restart and no way to explain why.
func TestClearingLocksIsAlsoRemembered(t *testing.T) {
	reg, _, pool := newRegistry(t)
	rm := makeRoom(t, reg, pool, "unlocked")
	ctx := testCtx(t)

	if err := rm.SetLocks(ctx, []Lock{{X1: 1, Y1: 1, X2: 4, Y2: 4}}); err != nil {
		t.Fatalf("setting locks: %v", err)
	}
	if err := rm.SetLocks(ctx, nil); err != nil {
		t.Fatalf("clearing locks: %v", err)
	}

	slug := rm.Meta.Slug
	rm.stop()
	reg.mu.Lock()
	delete(reg.rooms, slug)
	reg.mu.Unlock()

	reloaded, err := reg.Get(ctx, slug)
	if err != nil {
		t.Fatalf("reloading room: %v", err)
	}
	t.Cleanup(reloaded.stop)

	if got := reloaded.Locks(); len(got) != 0 {
		t.Errorf("the reloaded room has %d locked regions, want none: unfreezing did not stick", len(got))
	}
	if _, err := reloaded.Place(2, 2, 4, "alec"); err != nil {
		t.Errorf("painting in an unfrozen region returned %v, want success", err)
	}
}

// ---------------------------------------------------- write-behind retry -----

// TestAWriteTheDatabaseRefusesIsRetriedRatherThanDiscarded pins the second
// data-loss path load testing found. The flush used to clear its buffer whether
// or not the insert succeeded, so a database outage destroyed every placement
// made during it - a 6,000-placement burst across an outage left 2,002 of them
// with no row. The grid survives because a later snapshot rescues it; the log
// does not, and the leaderboard, the time-lapse, per-cell provenance and undo
// all read the log.
//
// The outage is a trigger that refuses inserts for this room and is then
// dropped. That is deliberately not a network fault: from the flush's point of
// view "the connection died" and "the server said no" are the same event -
// AppendPlacements returned an error - and a trigger makes it happen at an
// exact moment rather than when a proxy gets around to it.
//
// Note that removing the room's own row would not do: room_placements carries
// room_id but has no foreign key, so the inserts would quietly succeed and this
// test would pass without ever testing anything. It did, until it was checked.
func TestAWriteTheDatabaseRefusesIsRetriedRatherThanDiscarded(t *testing.T) {
	reg, st, pool := newRegistry(t)
	rm := makeRoom(t, reg, pool, "retry")
	ctx := testCtx(t)

	fn := fmt.Sprintf("pf_refuse_%d", rm.Meta.ID)
	trg := fmt.Sprintf("pf_refuse_trg_%d", rm.Meta.ID)
	exec := func(sql string) {
		t.Helper()
		if _, err := pool.Query(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	exec(fmt.Sprintf(`create or replace function %s() returns trigger as $fn$
		begin
			if new.room_id = %d then
				raise exception 'the database is refusing writes for this room';
			end if;
			return new;
		end $fn$ language plpgsql`, fn, rm.Meta.ID))
	exec(fmt.Sprintf(`create trigger %s before insert on room_placements
		for each row execute function %s()`, trg, fn))
	t.Cleanup(func() {
		if _, err := pool.Query(context.Background(),
			fmt.Sprintf(`drop trigger if exists %s on room_placements`, trg)); err != nil {
			t.Errorf("removing the test trigger: %v", err)
		}
		if _, err := pool.Query(context.Background(),
			fmt.Sprintf(`drop function if exists %s()`, fn)); err != nil {
			t.Errorf("removing the test function: %v", err)
		}
	})

	for i := 0; i < 5; i++ {
		if _, err := rm.Place(i, 0, 3, "alec"); err != nil {
			t.Fatalf("painting during the outage: %v", err)
		}
	}

	// Long enough for the 250ms flush to have tried and failed several times,
	// which is when a discard would have happened.
	time.Sleep(900 * time.Millisecond)
	if n := countPlacements(t, st, rm.Meta.ID); n != 0 {
		t.Fatalf("%d placements were written while the database was refusing them; the outage this test needs did not happen", n)
	}

	// Now let the database accept writes again and give the retry its chance.
	exec(fmt.Sprintf(`drop trigger %s on room_placements`, trg))

	deadline := time.Now().Add(15 * time.Second)
	var n int
	for time.Now().Before(deadline) {
		n = countPlacements(t, st, rm.Meta.ID)
		if n >= 5 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if n < 5 {
		t.Errorf("%d of 5 placements reached the log after the database recovered; the rest were discarded when the write failed", n)
	}
}

func countPlacements(t *testing.T, st *store.Store, roomID int64) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	n, err := st.CountPlacements(ctx, roomID)
	if err != nil {
		t.Fatalf("counting placements: %v", err)
	}
	return int(n)
}

// TestDroppingHistoryPullsTheSnapshotForward covers the third one. When the
// write-behind buffer overflows, the entry is dropped on purpose - stalling
// everyone's paint is worse - and the comment justifying it claimed the next
// snapshot would capture the pixel regardless. That is only true if a snapshot
// happens, and the periodic one was up to twenty seconds away: load testing put
// 38% of a canvas on the wrong colour after a crash inside that window.
func TestDroppingHistoryPullsTheSnapshotForward(t *testing.T) {
	reg, st, pool := newRegistry(t)
	rm := makeRoom(t, reg, pool, "dropped")
	ctx := testCtx(t)

	before, err := st.LoadSnapshot(ctx, rm.Meta.ID)
	beforeSeq := int64(-1)
	if err == nil {
		beforeSeq = before.Seq
	} else if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("reading the snapshot: %v", err)
	}

	if _, err := rm.Place(7, 7, 5, "alec"); err != nil {
		t.Fatalf("painting: %v", err)
	}
	// Report a drop the way a full buffer does. Going through the counter rather
	// than actually flooding keeps the test about the response to a drop rather
	// than about how hard this machine has to be pushed to cause one.
	rm.dropped.Add(1)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		snap, err := st.LoadSnapshot(ctx, rm.Meta.ID)
		if err == nil && snap.Seq > beforeSeq {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("no snapshot was written within ten seconds of history being dropped; the canvas is only as good as its log, and the log now has a hole in it")
}

// TestARoomWithNoDatabaseStillPaints keeps the ephemeral promise honest from
// this side: the closed check added to Place must not accidentally refuse a
// room that simply has nowhere to write.
func TestARoomWithNoDatabaseStillPaints(t *testing.T) {
	st := store.New(nil, quietLog())
	reg := NewRegistry(st, quietLog())
	ctx := context.Background()

	rm, err := reg.Create(ctx, Spec{Name: "no database", Width: 8, Height: 8, CooldownMs: 0})
	if err != nil {
		t.Fatalf("creating a room without a database: %v", err)
	}
	defer rm.stop()

	if _, err := rm.Place(1, 1, 2, "alec"); err != nil {
		t.Errorf("painting in an ephemeral room returned %v, want success", err)
	}
	if err := rm.SetLocks(ctx, []Lock{{X1: 0, Y1: 0, X2: 1, Y2: 1}}); err != nil {
		t.Errorf("freezing a region without a database returned %v; there is nothing to save but the lock still applies", err)
	}
	if _, err := rm.Place(0, 0, 2, "bess"); !errors.Is(err, ErrLocked) {
		t.Errorf("painting inside an ephemeral lock returned %v, want ErrLocked", err)
	}
}

// TestErrRoomClosedReadsAsSomethingToRetry is a small guard on the wording. The
// error reaches a painter through the API, and "try again" is the difference
// between a message that tells somebody what to do and one that just says no.
func TestErrRoomClosedReadsAsSomethingToRetry(t *testing.T) {
	if !strings.Contains(ErrRoomClosed.Error(), "try again") {
		t.Errorf("ErrRoomClosed = %q, which does not tell the painter their pixel is worth resending", ErrRoomClosed)
	}
}

// --------------------------------------------------------------- deleting ----

// TestDeletingARoomRemovesEverythingItOwned is the only destructive operation in
// the product, so it gets the most explicit test. A half-deleted room - rows
// without a room, or a room row whose pixels have gone - is worse than no delete
// at all: it shows up on the browse page as a canvas that cannot be opened.
func TestDeletingARoomRemovesEverythingItOwned(t *testing.T) {
	reg, st, pool := newRegistry(t)
	ctx := testCtx(t)

	rm, err := reg.Create(ctx, Spec{Name: "doomed", Width: 16, Height: 16, CooldownMs: 0, Unlisted: true})
	if err != nil {
		t.Fatalf("creating the room: %v", err)
	}
	roomID, slug := rm.Meta.ID, rm.Meta.Slug

	// Give it something in every table that hangs off a room, so "everything"
	// means something.
	if _, err := rm.Place(1, 1, 3, "alec"); err != nil {
		t.Fatalf("painting: %v", err)
	}
	waitForSeq(t, pool, roomID, 1)
	if err := rm.SetLocks(ctx, []Lock{{X1: 2, Y1: 2, X2: 4, Y2: 4}}); err != nil {
		t.Fatalf("locking: %v", err)
	}
	if err := st.Ban(ctx, roomID, "bess"); err != nil {
		t.Fatalf("banning: %v", err)
	}
	if err := rm.snapshot(ctx); err != nil {
		t.Fatalf("snapshotting: %v", err)
	}

	counts := func(when string) map[string]int64 {
		t.Helper()
		out := map[string]int64{}
		for table, sql := range map[string]string{
			"rooms":           `select count(*) from rooms where id = $1`,
			"room_placements": `select count(*) from room_placements where room_id = $1`,
			"room_snapshots":  `select count(*) from room_snapshots where room_id = $1`,
			"bans":            `select count(*) from bans where room_id = $1`,
			"locks":           `select count(*) from locks where room_id = $1`,
		} {
			res, err := pool.Query(ctx, sql, roomID)
			if err != nil {
				t.Fatalf("counting %s %s: %v", table, when, err)
			}
			n, _ := pg.Int64(res.Rows[0][0])
			out[table] = n
		}
		return out
	}

	before := counts("before the delete")
	for table, n := range before {
		if n == 0 {
			t.Fatalf("%s is already empty before the delete, so this test would pass without deleting anything", table)
		}
	}

	if err := reg.Delete(ctx, rm); err != nil {
		t.Fatalf("deleting the room: %v", err)
	}

	for table, n := range counts("after the delete") {
		if n != 0 {
			t.Errorf("%d row(s) left in %s after deleting the room", n, table)
		}
	}
	if !rm.Closed() {
		t.Error("the deleted room is still running, so its write-behind loop could put history back into a table that no longer has a room")
	}
	if _, resident := reg.Lookup(slug); resident {
		t.Error("the deleted room is still in the registry, so the slug still resolves to it")
	}
	if _, err := reg.Get(ctx, slug); !errors.Is(err, ErrNotFound) {
		t.Errorf("loading the deleted slug returned %v, want ErrNotFound", err)
	}
}

// TestDeletingARoomTellsThePeopleWatching. A canvas disappearing with no
// explanation is worse than one that says goodbye, and a client that is not told
// spends the next ten seconds reconnecting to something that will never answer.
func TestDeletingARoomTellsThePeopleWatching(t *testing.T) {
	reg, _, pool := newRegistry(t)
	ctx := testCtx(t)

	rm := makeRoom(t, reg, pool, "farewell")
	sub := rm.Hub.Subscribe("sse")

	frames := make(chan string, 32)
	go func() {
		for f := range sub.C {
			frames <- string(f.Data)
		}
		close(frames)
	}()

	if err := reg.Delete(ctx, rm); err != nil {
		t.Fatalf("deleting the room: %v", err)
	}

	deadline := time.After(5 * time.Second)
	for {
		select {
		case raw, ok := <-frames:
			if !ok {
				t.Fatal("the subscription closed without ever saying the canvas had been deleted")
			}
			var msg struct {
				T      string `json:"t"`
				Reason string `json:"reason"`
			}
			if err := json.Unmarshal([]byte(raw), &msg); err != nil || msg.T != "deleted" {
				continue
			}
			if msg.Reason == "" {
				t.Error("the deletion frame carries no reason, so the page can only say something vague")
			}
			return
		case <-deadline:
			t.Fatal("no deletion frame arrived within five seconds")
		}
	}
}
