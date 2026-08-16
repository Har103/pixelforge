package store

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Har103/pixelforge/internal/pg"
)

// A migration test has to be able to start from nothing, and "nothing" cannot
// mean the database the rest of the suite is using: `go test ./...` runs
// packages in parallel against a single DSN, so a test that dropped the world
// would fail whichever package happened to be mid-query. A schema costs nothing
// to create, and pointing search_path at one through the startup packet sends
// every unqualified statement in the migration there instead - which is also,
// conveniently, the deployment shape that broke the v1 fold-forward.
type scratch struct {
	name  string
	pool  *pg.Pool
	store *Store
}

// scratchSeq keeps two schemas in one test binary from colliding; the pid keeps
// two binaries apart.
var scratchSeq atomic.Int64

// newScratch creates an empty schema and a store whose every statement lands in
// it. The schema and everything in it goes away when the test ends.
func newScratch(t *testing.T, conns int) *scratch {
	t.Helper()
	dsn := testDSN(t)

	cfg, err := pg.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("parsing test DSN: %v", err)
	}
	// The name is a constant and two integers, so there is nothing in it for an
	// injection to ride in on - which matters, because a schema name cannot be
	// a bind parameter.
	name := fmt.Sprintf("pf_scratch_%d_%d", os.Getpid(), scratchSeq.Add(1))
	cfg.RuntimeParams["search_path"] = name

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	pool := pg.NewPool(cfg, conns, log)
	t.Cleanup(pool.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := pool.WaitReady(ctx, 3); err != nil {
		t.Fatalf("test database not reachable: %v", err)
	}
	if err := pool.Exec(ctx, "create schema "+name); err != nil {
		// A role that cannot create a schema cannot run these tests, but it can
		// still run every other one, so say why and move on rather than failing.
		t.Skipf("cannot create a scratch schema (%v); grant CREATE on the test database to run the migration tests", err)
	}
	t.Cleanup(func() {
		dropCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := pool.Exec(dropCtx, "drop schema if exists "+name+" cascade"); err != nil {
			t.Errorf("dropping scratch schema %s: %v", name, err)
		}
	})

	return &scratch{name: name, pool: pool, store: New(pool, log)}
}

// reset takes the schema back to empty, for a test that needs to migrate into
// virgin ground more than once.
func (s *scratch) reset(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.pool.Exec(ctx, "drop schema "+s.name+" cascade; create schema "+s.name); err != nil {
		t.Fatalf("resetting scratch schema %s: %v", s.name, err)
	}
}

// exec runs a statement in the scratch schema and fails the test if it errors.
func (s *scratch) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := s.pool.Query(ctx, sql, args...); err != nil {
		t.Fatalf("running %.60q: %v", sql, err)
	}
}

// scalar reads a single value as text, which is enough for the counts and
// fingerprints these tests compare.
func (s *scratch) scalar(t *testing.T, sql string, args ...any) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	row, err := s.pool.QueryRow(ctx, sql, args...)
	if err != nil {
		t.Fatalf("running %.60q: %v", sql, err)
	}
	if row == nil || len(row) == 0 {
		t.Fatalf("running %.60q returned no row", sql)
	}
	return string(row[0])
}

// fingerprint renders every table, column and index in the scratch schema as
// one comparable string. Comparing this before and after a repeat migration is
// what turns "it did not crash" into "it did not change anything".
func (s *scratch) fingerprint(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var b strings.Builder
	cols, err := s.pool.Query(ctx, `
		select table_name, ordinal_position, column_name, data_type,
		       is_nullable, coalesce(column_default, '')
		  from information_schema.columns
		 where table_schema = current_schema()
		 order by table_name, ordinal_position`)
	if err != nil {
		t.Fatalf("reading columns: %v", err)
	}
	for _, r := range cols.Rows {
		for i, v := range r {
			if i > 0 {
				b.WriteByte('|')
			}
			b.Write(v)
		}
		b.WriteByte('\n')
	}
	idx, err := s.pool.Query(ctx, `
		select indexname, indexdef from pg_indexes
		 where schemaname = current_schema()
		 order by indexname`)
	if err != nil {
		t.Fatalf("reading indexes: %v", err)
	}
	for _, r := range idx.Rows {
		b.Write(r[0])
		b.WriteByte('|')
		b.Write(r[1])
		b.WriteByte('\n')
	}
	if b.Len() == 0 {
		t.Fatal("the schema fingerprint is empty, so it cannot detect a change in anything")
	}
	return b.String()
}

// ------------------------------------------------------------- idempotency --

// TestMigrateIsIdempotent covers the ordinary case that happens on every single
// boot: the schema is already there. Applying it again must change neither the
// shape of the database nor a byte of what is in it, because the alternative is
// a restart that quietly rewrites production.
func TestMigrateIsIdempotent(t *testing.T) {
	sc := newScratch(t, 2)
	ctx := testCtx(t)

	if err := sc.store.Migrate(ctx); err != nil {
		t.Fatalf("first migration: %v", err)
	}
	before := sc.fingerprint(t)

	// Data written between migrations has to survive them untouched. A schema
	// statement that dropped and recreated a table would pass a shape
	// comparison and still lose every pixel in the room.
	room, err := sc.store.CreateRoom(ctx, Room{
		Slug: "idempotent", Name: "idempotent", Width: 32, Height: 32,
		Palette: "classic", CooldownMs: 750, Visibility: "public",
	})
	if err != nil {
		t.Fatalf("creating a room between migrations: %v", err)
	}
	if err := sc.store.AppendPlacements(ctx, room.ID, []Placement{
		{Seq: 1, X: 1, Y: 2, Color: 3, UID: "bess", At: time.Now()},
		{Seq: 2, X: 4, Y: 5, Color: 6, UID: "alec", At: time.Now()},
	}); err != nil {
		t.Fatalf("appending placements between migrations: %v", err)
	}
	if err := sc.store.SaveSnapshot(ctx, room.ID, Snapshot{
		Width: 32, Height: 32, Pixels: make([]byte, 32*32), Seq: 2,
	}); err != nil {
		t.Fatalf("saving a snapshot between migrations: %v", err)
	}
	if err := sc.store.Ban(ctx, room.ID, "carl"); err != nil {
		t.Fatalf("banning between migrations: %v", err)
	}

	for i := 2; i <= 3; i++ {
		if err := sc.store.Migrate(ctx); err != nil {
			t.Fatalf("migration %d: %v", i, err)
		}
		if got := sc.fingerprint(t); got != before {
			t.Fatalf("migration %d changed the schema:\nbefore:\n%s\nafter:\n%s", i, before, got)
		}
		if got, err := sc.store.CountPlacements(ctx, room.ID); err != nil || got != 2 {
			t.Errorf("after migration %d the room has %d placements (err %v), want the 2 it started with",
				i, got, err)
		}
		if got, err := sc.store.RoomBySlug(ctx, "idempotent"); err != nil || got.ID != room.ID {
			t.Errorf("after migration %d the room is %+v (err %v), want the one created before it",
				i, got, err)
		}
		bans, err := sc.store.Bans(ctx, room.ID)
		if err != nil || !bans["carl"] {
			t.Errorf("after migration %d the bans are %v (err %v), want carl still banned", i, bans, err)
		}
		snap, err := sc.store.LoadSnapshot(ctx, room.ID)
		if err != nil || snap.Seq != 2 || len(snap.Pixels) != 32*32 {
			t.Errorf("after migration %d the snapshot is %dx%d seq %d with %d bytes (err %v), want the one saved before it",
				i, snap.Width, snap.Height, snap.Seq, len(snap.Pixels), err)
		}
	}
}

// ------------------------------------------------------------- concurrency --

// TestConcurrentMigrateAgainstAVirginSchema is the test the advisory lock
// exists for. "create table if not exists" looks in the catalogue and then
// inserts into it, and those two steps are not atomic against another connection
// doing the same thing: the loser gets "duplicate key value violates unique
// constraint pg_type_typname_nsp_index" and its whole migration fails. That is
// not a hypothetical - it is a rolling deploy, and it is a test suite starting
// several packages at once - so this runs the collision on purpose, many times,
// because a lock that only usually holds is not a lock.
func TestConcurrentMigrateAgainstAVirginSchema(t *testing.T) {
	const (
		rounds  = 5
		racers  = 8
		wantTbl = 6 // rooms, room_placements, room_snapshots, users, bans, locks
	)
	sc := newScratch(t, racers)
	ctx := testCtx(t)

	for round := 1; round <= rounds; round++ {
		sc.reset(t)

		start := make(chan struct{})
		errs := make(chan error, racers)
		var wg sync.WaitGroup
		for i := 0; i < racers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start // release them together, or they just queue politely
				errs <- sc.store.Migrate(ctx)
			}()
		}
		close(start)
		wg.Wait()
		close(errs)

		failed := 0
		for err := range errs {
			if err != nil {
				failed++
				t.Errorf("round %d: a concurrent migration failed: %v", round, err)
			}
		}
		if failed > 0 {
			t.Fatalf("round %d: %d of %d concurrent migrations failed, which is a boot that crash-loops whenever two replicas start together",
				round, failed, racers)
		}

		// Surviving the race is only half of it: the schema still has to be
		// complete afterwards, because a migration that lost a table but
		// returned nil would fail later and somewhere else.
		got := sc.scalar(t, `select count(*) from information_schema.tables
		                      where table_schema = current_schema() and table_type = 'BASE TABLE'`)
		if got != fmt.Sprint(wantTbl) {
			t.Fatalf("round %d: the schema has %s tables after %d concurrent migrations, want %d",
				round, got, racers, wantTbl)
		}
	}
}

// TestConcurrentMigrateWithLivePlacements pins the other half of the deploy
// story: the second replica migrates while the first is already serving, so the
// migration has to be harmless to data that is arriving underneath it.
func TestConcurrentMigrateWithLivePlacements(t *testing.T) {
	sc := newScratch(t, 6)
	ctx := testCtx(t)

	if err := sc.store.Migrate(ctx); err != nil {
		t.Fatalf("initial migration: %v", err)
	}
	room, err := sc.store.CreateRoom(ctx, Room{
		Slug: "live", Name: "live", Width: 32, Height: 32,
		Palette: "classic", CooldownMs: 0, Visibility: "public",
	})
	if err != nil {
		t.Fatalf("creating a room: %v", err)
	}

	const writers, each = 3, 40
	var wg sync.WaitGroup
	errs := make(chan error, writers+2)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				seq := int64(w*each + i + 1)
				if err := sc.store.AppendPlacements(ctx, room.ID, []Placement{
					{Seq: seq, X: int(seq % 32), Y: w, Color: uint8(1 + i%19), UID: fmt.Sprintf("w%d", w), At: time.Now()},
				}); err != nil {
					errs <- fmt.Errorf("appending placement %d: %w", seq, err)
					return
				}
			}
		}(w)
	}
	for m := 0; m < 2; m++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 3; i++ {
				if err := sc.store.Migrate(ctx); err != nil {
					errs <- fmt.Errorf("migrating alongside writers: %w", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("%v", err)
	}

	if got, err := sc.store.CountPlacements(ctx, room.ID); err != nil || got != writers*each {
		t.Errorf("%d placements survived a concurrent migration (err %v), want %d",
			got, err, writers*each)
	}
}

// ---------------------------------------------------------- the v1 canvas ---

// v1Canvas builds the pre-rooms tables by hand, because the only place their
// shape still exists is in installations that have not upgraded yet.
func v1Canvas(t *testing.T, sc *scratch, width, height int, pixels []byte, rows [][5]any) {
	t.Helper()
	sc.exec(t, `create table placements (
	                id         bigserial   primary key,
	                seq        bigint      not null,
	                x          integer     not null,
	                y          integer     not null,
	                color      smallint    not null,
	                uid        text        not null,
	                created_at timestamptz not null default now())`)
	sc.exec(t, `create table snapshots (
	                id         integer     primary key,
	                width      integer     not null,
	                height     integer     not null,
	                pixels     bytea       not null,
	                seq        bigint      not null,
	                updated_at timestamptz not null default now())`)
	for _, r := range rows {
		sc.exec(t, `insert into placements (seq, x, y, color, uid) values ($1,$2,$3,$4,$5)`,
			r[0], r[1], r[2], r[3], r[4])
	}
	if pixels != nil {
		sc.exec(t, `insert into snapshots (id, width, height, pixels, seq)
		            values (1, $1, $2, $3, $4)`,
			width, height, pixels, rows[len(rows)-1][0])
	}
}

// TestMigrateFoldsThePreRoomsCanvasForward is the upgrade path for everyone who
// installed Pixelforge before rooms existed. Their whole canvas is in the old
// tables, and if the fold does not happen they open the new build to an empty
// grid and a history that appears to have been deleted.
//
// It also pins the shape of the fold. The old canvas becomes a room called
// "main" sized from the v1 snapshot, every placement keeps its sequence so the
// log still replays in the order it happened, and the old tables are still
// there afterwards - a migration that destroys the only copy of the data is a
// migration nobody can walk back.
func TestMigrateFoldsThePreRoomsCanvasForward(t *testing.T) {
	sc := newScratch(t, 2)
	ctx := testCtx(t)

	// A grid with a hole in it, so a fold that reordered or dropped rows shows
	// up as the wrong colour rather than merely the wrong count.
	pixels := []byte{0, 3, 0, 7, 9, 0}
	rows := [][5]any{
		{int64(1), 0, 0, 3, "bess"},
		{int64(2), 1, 0, 5, "alec"},
		{int64(3), 0, 0, 7, "bess"}, // repaints the cell seq 1 painted
		{int64(4), 2, 1, 9, "carl"},
	}
	v1Canvas(t, sc, 6, 1, pixels, rows)

	if err := sc.store.Migrate(ctx); err != nil {
		t.Fatalf("migrating a v1 installation: %v", err)
	}

	main, err := sc.store.RoomBySlug(ctx, "main")
	if err != nil {
		t.Fatalf("the pre-rooms canvas did not become a room: %v", err)
	}
	if main.Width != 6 || main.Height != 1 {
		t.Errorf("the folded room is %dx%d, want the 6x1 the v1 snapshot recorded",
			main.Width, main.Height)
	}

	// Every placement, in the order it happened. Replay is what rebuilds a
	// canvas, so an out-of-order fold repaints the cell the wrong colour.
	type replayed struct {
		seq    int64
		x, y   int
		colour uint8
	}
	var got []replayed
	n, err := sc.store.ReplayAfter(ctx, main.ID, 0, func(seq int64, x, y int, c uint8) {
		got = append(got, replayed{seq, x, y, c})
	})
	if err != nil {
		t.Fatalf("replaying the folded log: %v", err)
	}
	if n != len(rows) {
		t.Fatalf("replayed %d placements, want the %d the old canvas had", n, len(rows))
	}
	for i, r := range rows {
		want := replayed{r[0].(int64), r[1].(int), r[2].(int), uint8(r[3].(int))}
		if got[i] != want {
			t.Errorf("replayed placement %d = %+v, want %+v", i, got[i], want)
		}
	}
	// The last write to (0,0) has to be the one that wins, which is the whole
	// point of keeping the sequence.
	top, err := sc.store.TopPlacementAt(ctx, main.ID, 0, 0)
	if err != nil || top.Seq != 3 || top.Color != 7 {
		t.Errorf("the top placement at (0,0) is %+v (err %v), want seq 3 colour 7", top, err)
	}

	snap, err := sc.store.LoadSnapshot(ctx, main.ID)
	if err != nil {
		t.Fatalf("the v1 snapshot did not come across: %v", err)
	}
	if snap.Width != 6 || snap.Height != 1 || snap.Seq != 4 || string(snap.Pixels) != string(pixels) {
		t.Errorf("the folded snapshot is %dx%d seq %d %v, want 6x1 seq 4 %v",
			snap.Width, snap.Height, snap.Seq, snap.Pixels, pixels)
	}

	// The old tables are the only copy of this data until the operator is
	// satisfied the fold worked, so the migration must not have touched them.
	if got := sc.scalar(t, `select count(*) from placements`); got != fmt.Sprint(len(rows)) {
		t.Errorf("the v1 placements table has %s rows after the fold, want its original %d",
			got, len(rows))
	}

	// Folding twice would double every pixel in the log and hand the room a
	// second identity. Run it until it is obvious the guard holds.
	for i := 2; i <= 3; i++ {
		if err := sc.store.Migrate(ctx); err != nil {
			t.Fatalf("migration %d after the fold: %v", i, err)
		}
		if got := sc.scalar(t, `select count(*) from rooms`); got != "1" {
			t.Fatalf("migration %d left %s rooms, want the single folded one", i, got)
		}
		if got, err := sc.store.CountPlacements(ctx, main.ID); err != nil || got != int64(len(rows)) {
			t.Fatalf("migration %d left %d placements (err %v), want %d - the fold ran twice",
				i, got, err, len(rows))
		}
		if got := sc.scalar(t, `select count(*) from room_snapshots`); got != "1" {
			t.Fatalf("migration %d left %s snapshots, want 1", i, got)
		}
	}
}

// TestMigrateSkipsTheFoldWhenRoomsAlreadyExist covers the other order of
// events: someone upgraded, created rooms, and only then restored the old
// tables from a backup. Folding at that point would invent a duplicate canvas
// nobody asked for.
func TestMigrateSkipsTheFoldWhenRoomsAlreadyExist(t *testing.T) {
	sc := newScratch(t, 2)
	ctx := testCtx(t)

	if err := sc.store.Migrate(ctx); err != nil {
		t.Fatalf("initial migration: %v", err)
	}
	if _, err := sc.store.CreateRoom(ctx, Room{
		Slug: "already-here", Name: "already here", Width: 16, Height: 16,
		Palette: "classic", Visibility: "public",
	}); err != nil {
		t.Fatalf("creating a room: %v", err)
	}

	v1Canvas(t, sc, 4, 4, make([]byte, 16), [][5]any{{int64(1), 0, 0, 3, "bess"}})
	if err := sc.store.Migrate(ctx); err != nil {
		t.Fatalf("migrating with rooms already present: %v", err)
	}

	if _, err := sc.store.RoomBySlug(ctx, "main"); err == nil {
		t.Error("the fold ran even though the installation already had rooms, so there is now a canvas nobody created")
	}
	if got := sc.scalar(t, `select count(*) from rooms`); got != "1" {
		t.Errorf("the installation has %s rooms, want only the one that was created", got)
	}
}

// TestMigrateIgnoresAPlacementsTableWithNoSnapshots is about not crash-looping.
// The fold reads two v1 tables, so finding only one of them is not a v1
// installation - and treating it as one used to end the migration in "relation
// snapshots does not exist", which fails Migrate, which fails the boot, on
// every replica, every time.
func TestMigrateIgnoresAPlacementsTableWithNoSnapshots(t *testing.T) {
	sc := newScratch(t, 2)
	ctx := testCtx(t)

	sc.exec(t, `create table placements (id bigserial primary key, note text)`)
	sc.exec(t, `insert into placements (note) values ('not a pixelforge table')`)

	if err := sc.store.Migrate(ctx); err != nil {
		t.Fatalf("a table called placements that is not a v1 canvas broke the migration: %v", err)
	}
	if got := sc.scalar(t, `select count(*) from rooms`); got != "0" {
		t.Errorf("the migration invented %s rooms out of a table it should have ignored", got)
	}
}

// TestMigrateOnVirginSchemaCreatesNoRooms is the ordinary first boot: there is
// nothing to fold, so nothing should appear.
func TestMigrateOnVirginSchemaCreatesNoRooms(t *testing.T) {
	sc := newScratch(t, 2)
	ctx := testCtx(t)

	if err := sc.store.Migrate(ctx); err != nil {
		t.Fatalf("migrating a virgin schema: %v", err)
	}
	if got := sc.scalar(t, `select count(*) from rooms`); got != "0" {
		t.Errorf("a first boot created %s rooms, want none", got)
	}
	for _, table := range []string{"rooms", "room_placements", "room_snapshots", "users", "bans", "locks"} {
		if got := sc.scalar(t, `select count(*) from information_schema.tables
		                         where table_schema = current_schema() and table_name = $1`, table); got != "1" {
			t.Errorf("table %q does not exist after a migration", table)
		}
	}
	// The indexes are not decoration. Every read of the log is "this room, in
	// order, after a point", and the per-cell one is what keeps "who painted
	// this, and what was here before" from being a sequential scan of the
	// room's entire history on a canvas with a million placements in it.
	for _, index := range []string{
		"rooms_activity_idx", "rooms_visibility_idx", "rooms_owner_idx",
		"room_placements_seq_idx", "room_placements_uid_idx", "room_placements_cell_idx",
		"locks_room_idx",
	} {
		if got := sc.scalar(t, `select count(*) from pg_indexes
		                         where schemaname = current_schema() and indexname = $1`, index); got != "1" {
			t.Errorf("index %q does not exist after a migration; the query it exists for now scans the whole table",
				index)
		}
	}
}
