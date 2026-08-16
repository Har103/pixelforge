package store

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"testing"
)

// Ephemeral mode is the promise that the service boots and serves without a
// database instead of crash-looping in front of whoever is looking at it. The
// promise is only as good as the weakest method: one missing nil-pool guard is
// a nil dereference in a request handler, which is a 500 for that user and a
// panic in the log for everyone else.
//
// These tests need no database at all, so they run everywhere - which is the
// point, because the mode they cover is the one nobody remembers to try.

func ephemeralStore() *Store {
	return New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// ephemeralCases is keyed by method name so a completeness check can prove
// nothing has been left out. Each case calls one method and asserts what it
// documents, because "did not panic" is a low bar for a mode the service is
// expected to serve traffic in.
func ephemeralCases() map[string]func(*testing.T, *Store) {
	ctx := context.Background()
	return map[string]func(*testing.T, *Store){
		"Ephemeral": func(t *testing.T, s *Store) {
			if !s.Ephemeral() {
				t.Error("a store built on a nil pool does not consider itself ephemeral, so every caller will assume it has a database")
			}
		},
		"Migrate": func(t *testing.T, s *Store) {
			if err := s.Migrate(ctx); err != nil {
				t.Errorf("Migrate = %v, want nil: there is nothing to migrate and boot must not fail on it", err)
			}
		},
		"Ready": func(t *testing.T, s *Store) {
			if err := s.Ready(ctx); err != nil {
				t.Errorf("Ready = %v, want nil: with no database configured the service is as ready as it will ever be", err)
			}
		},
		"CreateRoom": func(t *testing.T, s *Store) {
			r, err := s.CreateRoom(ctx, Room{Slug: "e", Name: "e", Width: 8, Height: 8})
			if err != nil {
				t.Fatalf("CreateRoom = %v, want a usable in-memory room", err)
			}
			if r.ID == 0 {
				t.Error("the room came back with id 0, which cannot key a registry entry")
			}
			if r.CreatedAt.IsZero() || r.LastActive.IsZero() {
				t.Errorf("the room came back with zero timestamps (%v, %v), which the browse page renders as 1970",
					r.CreatedAt, r.LastActive)
			}
		},
		"RoomBySlug": func(t *testing.T, s *Store) {
			if _, err := s.RoomBySlug(ctx, "anything"); !errors.Is(err, ErrNotFound) {
				t.Errorf("RoomBySlug = %v, want ErrNotFound so the caller creates one instead of retrying", err)
			}
		},
		"UpdateRoom": func(t *testing.T, s *Store) {
			if err := s.UpdateRoom(ctx, Room{ID: 1, Name: "e"}); err != nil {
				t.Errorf("UpdateRoom = %v, want nil", err)
			}
		},
		"TouchRoom": func(t *testing.T, s *Store) { s.TouchRoom(ctx, 1) },
		"ListRooms": func(t *testing.T, s *Store) {
			got, err := s.ListRooms(ctx, 10)
			if err != nil || len(got) != 0 {
				t.Errorf("ListRooms = %v, %v, want an empty list and no error", got, err)
			}
		},
		"RoomsForUser": func(t *testing.T, s *Store) {
			got, err := s.RoomsForUser(ctx, 7)
			if err != nil || len(got) != 0 {
				t.Errorf("RoomsForUser = %v, %v, want an empty list and no error", got, err)
			}
		},
		"ClaimRoom": func(t *testing.T, s *Store) {
			if err := s.ClaimRoom(ctx, 1, 2); err != nil {
				t.Errorf("ClaimRoom = %v, want nil", err)
			}
		},
		"LoadSnapshot": func(t *testing.T, s *Store) {
			if _, err := s.LoadSnapshot(ctx, 1); !errors.Is(err, ErrNotFound) {
				t.Errorf("LoadSnapshot = %v, want ErrNotFound so the room starts from an empty grid", err)
			}
		},
		"SaveSnapshot": func(t *testing.T, s *Store) {
			if err := s.SaveSnapshot(ctx, 1, Snapshot{Width: 2, Height: 2, Pixels: []byte{0, 0, 0, 0}}); err != nil {
				t.Errorf("SaveSnapshot = %v, want nil: the periodic snapshot must not become a periodic error", err)
			}
		},
		"AppendPlacements": func(t *testing.T, s *Store) {
			if err := s.AppendPlacements(ctx, 1, []Placement{{Seq: 1, X: 1, Y: 1, Color: 2, UID: "u"}}); err != nil {
				t.Errorf("AppendPlacements = %v, want nil", err)
			}
		},
		"ReplayAfter": func(t *testing.T, s *Store) {
			called := 0
			n, err := s.ReplayAfter(ctx, 1, 0, func(int64, int, int, uint8) { called++ })
			if n != 0 || err != nil || called != 0 {
				t.Errorf("ReplayAfter = %d, %v with %d callbacks, want 0, nil and no callbacks", n, err, called)
			}
		},
		"History": func(t *testing.T, s *Store) {
			got, err := s.History(ctx, 1, 0, 10)
			if err != nil || len(got) != 0 {
				t.Errorf("History = %v, %v, want an empty feed and no error", got, err)
			}
		},
		"CountPlacements": func(t *testing.T, s *Store) {
			got, err := s.CountPlacements(ctx, 1)
			if got != 0 || err != nil {
				t.Errorf("CountPlacements = %d, %v, want 0 and no error", got, err)
			}
		},
		"Leaderboard": func(t *testing.T, s *Store) {
			got, err := s.Leaderboard(ctx, 1, 10)
			if err != nil || len(got) != 0 {
				t.Errorf("Leaderboard = %v, %v, want an empty board and no error", got, err)
			}
		},
		"UndoUser": func(t *testing.T, s *Store) {
			n, err := s.UndoUser(ctx, 1, "u")
			if n != 0 || err != nil {
				t.Errorf("UndoUser = %d, %v, want 0 retired and no error", n, err)
			}
		},
		"ClearRoom": func(t *testing.T, s *Store) {
			if err := s.ClearRoom(ctx, 1); err != nil {
				t.Errorf("ClearRoom = %v, want nil", err)
			}
		},
		"DeleteRoom": func(t *testing.T, s *Store) {
			// Succeeds rather than reporting ErrNotFound. There is nothing to
			// delete, but the caller has already taken the room out of memory and
			// told everybody watching, so the only thing an error could achieve
			// here is a red message about a canvas that really has gone.
			if err := s.DeleteRoom(ctx, 1); err != nil {
				t.Errorf("DeleteRoom = %v, want nil: with no database the room is already as gone as it can be", err)
			}
		},
		"Locks": func(t *testing.T, s *Store) {
			got, err := s.Locks(ctx, 1)
			if err != nil {
				t.Fatalf("Locks = %v, want no error: a room with no database still has to boot", err)
			}
			// Nil rather than empty is deliberate here and the opposite of Bans:
			// the caller ranges over this and copies it into the room's own
			// slice, never writes into what it is handed.
			if len(got) != 0 {
				t.Errorf("Locks returned %d rectangles from a store with no database", len(got))
			}
		},
		"SetLocks": func(t *testing.T, s *Store) {
			// Accepting the write is the honest behaviour: the room has already
			// applied the locks in memory and told everybody, and in ephemeral
			// mode there is by definition nothing to outlive the process.
			if err := s.SetLocks(ctx, 1, []LockRect{{X1: 1, Y1: 2, X2: 3, Y2: 4}}); err != nil {
				t.Errorf("SetLocks = %v, want nil: freezing a region must not fail because there is no database", err)
			}
			if err := s.SetLocks(ctx, 1, nil); err != nil {
				t.Errorf("SetLocks(nil) = %v, want nil", err)
			}
		},
		"Bans": func(t *testing.T, s *Store) {
			got, err := s.Bans(ctx, 1)
			if err != nil {
				t.Fatalf("Bans = %v, want no error", err)
			}
			// The map is handed straight to a room, which writes into it on the
			// next ban. A nil map reads fine and panics on the first write, so
			// an empty one is not the same thing as none at all.
			if got == nil {
				t.Error("Bans returned a nil map, which panics the moment a moderator bans somebody")
			}
			if len(got) != 0 {
				t.Errorf("Bans = %v, want it empty", got)
			}
		},
		"Ban": func(t *testing.T, s *Store) {
			if err := s.Ban(ctx, 1, "u"); err != nil {
				t.Errorf("Ban = %v, want nil", err)
			}
		},
		"Unban": func(t *testing.T, s *Store) {
			if err := s.Unban(ctx, 1, "u"); err != nil {
				t.Errorf("Unban = %v, want nil", err)
			}
		},
		"CreateUser": func(t *testing.T, s *Store) {
			// The one method that refuses: an account nobody can sign back into
			// is worse than being told to come back when there is a database.
			if _, err := s.CreateUser(ctx, "someone", "hash"); err == nil {
				t.Error("CreateUser succeeded with no database, so the account it promised does not exist")
			}
		},
		"UserByName": func(t *testing.T, s *Store) {
			if _, err := s.UserByName(ctx, "someone"); !errors.Is(err, ErrNotFound) {
				t.Errorf("UserByName = %v, want ErrNotFound", err)
			}
		},
		"UserByID": func(t *testing.T, s *Store) {
			if _, err := s.UserByID(ctx, 1); !errors.Is(err, ErrNotFound) {
				t.Errorf("UserByID = %v, want ErrNotFound", err)
			}
		},
		"CellHistory": func(t *testing.T, s *Store) {
			got, err := s.CellHistory(ctx, 1, 1, 1, 10)
			if err != nil || len(got) != 0 {
				t.Errorf("CellHistory = %v, %v, want an empty history and no error", got, err)
			}
		},
		"LatestOwnPlacement": func(t *testing.T, s *Store) {
			if _, err := s.LatestOwnPlacement(ctx, 1, "u"); !errors.Is(err, ErrNotFound) {
				t.Errorf("LatestOwnPlacement = %v, want ErrNotFound so undo says there is nothing to undo", err)
			}
		},
		"TopPlacementAt": func(t *testing.T, s *Store) {
			if _, err := s.TopPlacementAt(ctx, 1, 1, 1); !errors.Is(err, ErrNotFound) {
				t.Errorf("TopPlacementAt = %v, want ErrNotFound", err)
			}
		},
		"ColourBeneath": func(t *testing.T, s *Store) {
			got, err := s.ColourBeneath(ctx, 1, 1, 1, 5)
			if got != 0 || err != nil {
				t.Errorf("ColourBeneath = %d, %v, want the background and no error", got, err)
			}
		},
		"MarkUndone": func(t *testing.T, s *Store) {
			if err := s.MarkUndone(ctx, 1, 1); err != nil {
				t.Errorf("MarkUndone = %v, want nil", err)
			}
		},
	}
}

func TestEphemeralStoreServesWithoutADatabase(t *testing.T) {
	for name, check := range ephemeralCases() {
		t.Run(name, func(t *testing.T) {
			check(t, ephemeralStore())
		})
	}
}

// TestNilStoreIsAlsoEphemeral covers the other way to end up without a pool.
// Ephemeral() is written to tolerate a nil receiver, so a caller that never got
// a store at all gets the same degraded service rather than a panic.
func TestNilStoreIsAlsoEphemeral(t *testing.T) {
	var s *Store
	for name, check := range ephemeralCases() {
		t.Run(name, func(t *testing.T) {
			check(t, s)
		})
	}
}

// TestEveryStoreMethodIsCoveredInEphemeralMode is the guard on the guard. A
// method added later without a nil-pool check will not fail any test in this
// file, because no test will call it - so make the omission itself the failure.
func TestEveryStoreMethodIsCoveredInEphemeralMode(t *testing.T) {
	covered := ephemeralCases()
	typ := reflect.TypeOf(&Store{})
	for i := 0; i < typ.NumMethod(); i++ {
		name := typ.Method(i).Name
		if _, ok := covered[name]; !ok {
			t.Errorf("Store.%s is never exercised without a database; add it to ephemeralCases and check it does something sane on a nil pool",
				name)
		}
	}
	for name := range covered {
		if _, ok := typ.MethodByName(name); !ok {
			t.Errorf("ephemeralCases covers %q, which is not a method of Store any more", name)
		}
	}
}

// TestNewWithoutALoggerStillLogs covers the other nil a caller can pass. The
// store logs on the paths that swallow errors, so a nil logger would turn
// TouchRoom's debug line into a panic on the first paint.
func TestNewWithoutALogger(t *testing.T) {
	s := New(nil, nil)
	if s.log == nil {
		t.Fatal("the store has no logger, so the first swallowed error panics instead of being logged")
	}
	s.TouchRoom(context.Background(), 1)
}

// TestScanRowsRefuseRowsOfTheWrongShape covers the guards that sit between a
// column list and a struct. The two are written out by hand in this package, so
// the failure they exist for is somebody editing one of them: without the
// check, a row one column short is an index out of range in the middle of a
// request rather than an error the handler can report.
func TestScanRowsRefuseRowsOfTheWrongShape(t *testing.T) {
	short := make([][]byte, 12) // rooms has thirteen columns
	if _, err := scanRoom(short); err == nil {
		t.Error("scanRoom accepted a row of 12 columns, so a column list that drifts out of step reads garbage")
	}
	if _, err := scanUser(make([][]byte, 3)); err == nil {
		t.Error("scanUser accepted a row of 3 columns")
	}
	// The right shape with nothing in it is a different thing: NULLs are
	// legitimate, and every column decoder folds them to a zero value.
	r, err := scanRoom(make([][]byte, 13))
	if err != nil {
		t.Errorf("scanRoom on a row of NULLs = %v, want the zero room", err)
	}
	if r.ID != 0 || r.Slug != "" || r.OwnerUser != 0 {
		t.Errorf("scanRoom on a row of NULLs = %+v, want zeroes", r)
	}
	if _, err := scanUser(make([][]byte, 4)); err != nil {
		t.Errorf("scanUser on a row of NULLs = %v, want the zero user", err)
	}
}

// ------------------------------------------------- a database that is gone --

// TestEveryQueryFailsCleanlyWhenTheDatabaseIsGone is the difference between a
// degraded service and a corrupted one. Ephemeral mode is a deliberate absence
// of a database; this is a database that was there and is not answering now,
// and the two must not look the same to a caller.
//
// The distinction that matters most is ErrNotFound. A room boots by loading its
// snapshot, treating ErrNotFound as "this room has never been saved, start
// empty" - so a failing query that came back as ErrNotFound would restore an
// empty grid over a full one and then overwrite the real snapshot with it at
// the next tick. Every lookup below has to fail as an error instead.
func TestEveryQueryFailsCleanlyWhenTheDatabaseIsGone(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx(t)
	// Closing the pool is the cheapest honest way to make every query fail: it
	// is the same path a query takes when the database has gone away, minus the
	// waiting. Close is idempotent, so the test helper's cleanup is unbothered.
	s.pool.Close()

	if s.Ephemeral() {
		t.Fatal("a store with a closed pool reports itself ephemeral, so callers will treat lost data as an intentional absence")
	}

	mustFail := []struct {
		name string
		call func() error
	}{
		{"Migrate", func() error { return s.Migrate(ctx) }},
		{"Ready", func() error { return s.Ready(ctx) }},
		{"CreateRoom", func() error { _, err := s.CreateRoom(ctx, Room{Slug: "x", Name: "x", Width: 8, Height: 8}); return err }},
		{"RoomBySlug", func() error { _, err := s.RoomBySlug(ctx, "x"); return err }},
		{"UpdateRoom", func() error { return s.UpdateRoom(ctx, Room{ID: 1}) }},
		{"ListRooms", func() error { _, err := s.ListRooms(ctx, 10); return err }},
		{"RoomsForUser", func() error { _, err := s.RoomsForUser(ctx, 1); return err }},
		{"ClaimRoom", func() error { return s.ClaimRoom(ctx, 1, 1) }},
		{"LoadSnapshot", func() error { _, err := s.LoadSnapshot(ctx, 1); return err }},
		{"SaveSnapshot", func() error { return s.SaveSnapshot(ctx, 1, Snapshot{Pixels: []byte{0}}) }},
		{"AppendPlacements", func() error { return s.AppendPlacements(ctx, 1, []Placement{{Seq: 1}}) }},
		{"ReplayAfter", func() error { _, err := s.ReplayAfter(ctx, 1, 0, func(int64, int, int, uint8) {}); return err }},
		{"History", func() error { _, err := s.History(ctx, 1, 0, 10); return err }},
		{"CountPlacements", func() error { _, err := s.CountPlacements(ctx, 1); return err }},
		{"Leaderboard", func() error { _, err := s.Leaderboard(ctx, 1, 10); return err }},
		{"UndoUser", func() error { _, err := s.UndoUser(ctx, 1, "u"); return err }},
		{"ClearRoom", func() error { return s.ClearRoom(ctx, 1) }},
		{"Bans", func() error { _, err := s.Bans(ctx, 1); return err }},
		{"Ban", func() error { return s.Ban(ctx, 1, "u") }},
		{"Unban", func() error { return s.Unban(ctx, 1, "u") }},
		{"CreateUser", func() error { _, err := s.CreateUser(ctx, "u", "h"); return err }},
		{"UserByName", func() error { _, err := s.UserByName(ctx, "u"); return err }},
		{"UserByID", func() error { _, err := s.UserByID(ctx, 1); return err }},
		{"CellHistory", func() error { _, err := s.CellHistory(ctx, 1, 1, 1, 10); return err }},
		{"LatestOwnPlacement", func() error { _, err := s.LatestOwnPlacement(ctx, 1, "u"); return err }},
		{"TopPlacementAt", func() error { _, err := s.TopPlacementAt(ctx, 1, 1, 1); return err }},
		{"ColourBeneath", func() error { _, err := s.ColourBeneath(ctx, 1, 1, 1, 5); return err }},
		{"MarkUndone", func() error { return s.MarkUndone(ctx, 1, 1) }},
	}
	for _, tc := range mustFail {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatalf("%s succeeded against a database that is not there", tc.name)
			}
			if errors.Is(err, ErrNotFound) {
				t.Errorf("%s = %v, which callers read as 'there is nothing here' rather than 'ask again later'",
					tc.name, err)
			}
		})
	}

	// TouchRoom is the exception on purpose: it is fire-and-forget so that a
	// database problem can never fail somebody's paint.
	s.TouchRoom(ctx, 1)
}
