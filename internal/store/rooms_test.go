package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// newPublicRoom creates a listed room, which is the only kind the browse page
// can see, and removes it afterwards.
func newPublicRoom(t *testing.T, s *Store, name string, ownerUser int64) Room {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	r, err := s.CreateRoom(ctx, Room{
		Slug:       fmt.Sprintf("storetest-pub-%s-%d", name, time.Now().UnixNano()),
		Name:       name,
		Width:      32,
		Height:     32,
		Palette:    "classic",
		CooldownMs: 750,
		Visibility: "public",
		OwnerHash:  "hash-" + name,
		OwnerUser:  ownerUser,
	})
	if err != nil {
		t.Fatalf("creating public room %q: %v", name, err)
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

// ----------------------------------------------------------------- rooms ----

// TestCreateRoomRoundTripsEveryField pins the insert against its own column
// list. The two are written out by hand, so a column added in the middle of one
// and the end of the other silently shifts every value one place to the left -
// which is a room that comes back with its height as its width.
func TestCreateRoomRoundTripsEveryField(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx(t)

	r := newPublicRoom(t, s, "roundtrip", 0)
	got, err := s.RoomBySlug(ctx, r.Slug)
	if err != nil {
		t.Fatalf("reading the room back: %v", err)
	}
	if got != r {
		t.Errorf("the room read back as %+v, want the %+v that was returned by the insert", got, r)
	}
	if got.Width != 32 || got.Height != 32 || got.Palette != "classic" || got.CooldownMs != 750 {
		t.Errorf("room = %dx%d palette %q cooldown %d, want 32x32 classic 750",
			got.Width, got.Height, got.Palette, got.CooldownMs)
	}
	if got.OwnerHash != "hash-roundtrip" || got.OwnerUser != 0 || got.Paused {
		t.Errorf("room ownership = hash %q user %d paused %v, want the hash it was given, no account and not paused",
			got.OwnerHash, got.OwnerUser, got.Paused)
	}
	// owner_user is NULL for a room nobody has claimed, and the query folds that
	// to zero so callers never have to think about NULL.
	if got.CreatedAt.IsZero() || got.LastActive.IsZero() {
		t.Errorf("room timestamps = %v, %v, want the database's now()", got.CreatedAt, got.LastActive)
	}
}

// TestCreateRoomRejectsADuplicateSlug covers the one collision users can cause
// by hand, because the slug is the URL.
func TestCreateRoomRejectsADuplicateSlug(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx(t)

	first := newPublicRoom(t, s, "dup", 0)
	_, err := s.CreateRoom(ctx, Room{
		Slug: first.Slug, Name: "another", Width: 16, Height: 16,
		Palette: "classic", Visibility: "public",
	})
	if !errors.Is(err, ErrSlugTaken) {
		t.Errorf("creating a second room on slug %q = %v, want ErrSlugTaken so the caller can pick another",
			first.Slug, err)
	}
}

func TestRoomBySlugMissesCleanly(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx(t)
	if _, err := s.RoomBySlug(ctx, "storetest-no-such-room"); !errors.Is(err, ErrNotFound) {
		t.Errorf("looking up a room that does not exist = %v, want ErrNotFound", err)
	}
}

// TestUpdateRoomChangesOnlyTheMutableSettings is the settings dialog. Width,
// height and palette are fixed for the life of a room because every stored
// pixel is an index into them, so an update statement that touched them would
// reinterpret the whole canvas.
func TestUpdateRoomChangesOnlyTheMutableSettings(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx(t)
	r := newPublicRoom(t, s, "settings", 0)

	changed := r
	changed.Name = "renamed"
	changed.Visibility = "unlisted"
	changed.CooldownMs = 2500
	changed.Paused = true
	// These four are not in the update statement, and this proves it rather
	// than trusting it.
	changed.Width = 999
	changed.Height = 999
	changed.Palette = "neon"
	changed.Slug = "storetest-hijacked"

	if err := s.UpdateRoom(ctx, changed); err != nil {
		t.Fatalf("updating: %v", err)
	}
	got, err := s.RoomBySlug(ctx, r.Slug)
	if err != nil {
		t.Fatalf("reading the room back on its original slug: %v", err)
	}
	if got.Name != "renamed" || got.Visibility != "unlisted" || got.CooldownMs != 2500 || !got.Paused {
		t.Errorf("room = %+v, want the name, visibility, cooldown and pause it was given", got)
	}
	if got.Width != r.Width || got.Height != r.Height || got.Palette != r.Palette || got.Slug != r.Slug {
		t.Errorf("room = %dx%d palette %q slug %q, want the immutable %dx%d %q %q — every stored pixel is an index into these",
			got.Width, got.Height, got.Palette, got.Slug, r.Width, r.Height, r.Palette, r.Slug)
	}
}

// TestTouchRoomMovesActivityForward covers the ordering key for the browse
// page. It is fire-and-forget by design: a paint must never fail because the
// liveliness column could not be written.
func TestTouchRoomMovesActivityForward(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx(t)
	r := newPublicRoom(t, s, "touch", 0)

	s.TouchRoom(ctx, r.ID)
	got, err := s.RoomBySlug(ctx, r.Slug)
	if err != nil {
		t.Fatalf("reading the room back: %v", err)
	}
	if !got.LastActive.After(r.LastActive) {
		t.Errorf("last_active is %v after a touch, want something later than the %v it was created with",
			got.LastActive, r.LastActive)
	}
	// A room id that does not exist is not an error anybody should hear about.
	s.TouchRoom(ctx, -1)
}

// TestListRoomsShowsPublicRoomsMostRecentFirst covers the browse page. The
// database is shared with whatever else is running, so this asserts about the
// rooms it created rather than the whole list - which is also how the query
// behaves in production, where the list is never only yours.
func TestListRoomsShowsPublicRoomsMostRecentFirst(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx(t)

	older := newPublicRoom(t, s, "older", 0)
	newer := newPublicRoom(t, s, "newer", 0)
	hidden := newTestRoom(t, s) // unlisted

	paintAll(t, s, newer.ID, []Placement{
		{Seq: 1, X: 1, Y: 1, Color: 2, UID: "bess", At: time.Now()},
		{Seq: 2, X: 2, Y: 2, Color: 3, UID: "bess", At: time.Now()},
		{Seq: 3, X: 3, Y: 3, Color: 4, UID: "alec", At: time.Now()},
	})
	if err := s.MarkUndone(ctx, newer.ID, 3); err != nil {
		t.Fatalf("marking undone: %v", err)
	}
	// Make the ordering deliberate rather than incidental.
	s.TouchRoom(ctx, older.ID)
	s.TouchRoom(ctx, newer.ID)

	rooms, err := s.ListRooms(ctx, 200)
	if err != nil {
		t.Fatalf("listing rooms: %v", err)
	}
	posOlder, posNewer := -1, -1
	for i, r := range rooms {
		switch r.ID {
		case older.ID:
			posOlder = i
		case newer.ID:
			posNewer = i
			if r.Placements != 2 {
				t.Errorf("the room lists %d placements, want the 2 that are still live; the third was undone",
					r.Placements)
			}
		case hidden.ID:
			t.Error("an unlisted room is on the browse page, which is the one thing unlisted means")
		}
	}
	if posOlder < 0 || posNewer < 0 {
		t.Fatalf("the browse page has the rooms at positions %d and %d, want both listed", posOlder, posNewer)
	}
	if posNewer > posOlder {
		t.Errorf("the more recently active room is at position %d, behind the older one at %d",
			posNewer, posOlder)
	}
	if got := len(rooms); got == 0 {
		t.Error("the browse page is empty")
	}

	// A limit of zero is a caller who did not say, not a caller who wants
	// nothing, and an absurd one is clamped rather than served.
	for _, limit := range []int{0, -5, 100000} {
		got, err := s.ListRooms(ctx, limit)
		if err != nil {
			t.Fatalf("listing rooms with limit %d: %v", limit, err)
		}
		if len(got) == 0 {
			t.Errorf("limit %d returned no rooms at all, want the default page", limit)
		}
		if len(got) > 200 {
			t.Errorf("limit %d returned %d rooms, want it clamped", limit, len(got))
		}
	}
}

// TestRoomsForUserListsOnlyThatAccountsRooms covers "my canvases".
func TestRoomsForUserListsOnlyThatAccountsRooms(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx(t)

	me := newTestUser(t, s, "owner")
	someoneElse := newTestUser(t, s, "stranger")

	mine := newPublicRoom(t, s, "mine", me.ID)
	theirs := newPublicRoom(t, s, "theirs", someoneElse.ID)
	unowned := newPublicRoom(t, s, "unowned", 0)

	got, err := s.RoomsForUser(ctx, me.ID)
	if err != nil {
		t.Fatalf("listing owned rooms: %v", err)
	}
	if len(got) != 1 || got[0].ID != mine.ID {
		t.Fatalf("the account owns %+v, want only room %d", got, mine.ID)
	}
	if got[0].OwnerUser != me.ID {
		t.Errorf("the room's owner reads back as %d, want %d", got[0].OwnerUser, me.ID)
	}
	if got, err := s.RoomsForUser(ctx, someoneElse.ID); err != nil || len(got) != 1 || got[0].ID != theirs.ID {
		t.Errorf("the other account owns %+v (err %v), want only room %d", got, err, theirs.ID)
	}
	// Nobody is account zero, and asking for its rooms must not return every
	// unclaimed room in the installation.
	if got, err := s.RoomsForUser(ctx, 0); err != nil || len(got) != 0 {
		t.Errorf("RoomsForUser(0) = %+v (err %v), want nothing; room %d is unowned, not owned by nobody",
			got, err, unowned.ID)
	}
}

// TestClaimRoomAttachesARoomToAnAccount covers upgrading a moderator key into
// something that survives losing the cookie.
func TestClaimRoomAttachesARoomToAnAccount(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx(t)

	user := newTestUser(t, s, "claimer")
	r := newPublicRoom(t, s, "claimable", 0)
	other := newPublicRoom(t, s, "not-claimed", 0)

	if err := s.ClaimRoom(ctx, r.ID, user.ID); err != nil {
		t.Fatalf("claiming: %v", err)
	}
	got, err := s.RoomBySlug(ctx, r.Slug)
	if err != nil {
		t.Fatalf("reading the room back: %v", err)
	}
	if got.OwnerUser != user.ID {
		t.Errorf("the claimed room's owner is %d, want %d", got.OwnerUser, user.ID)
	}
	if got, err := s.RoomBySlug(ctx, other.Slug); err != nil || got.OwnerUser != 0 {
		t.Errorf("the room next door is owned by %d (err %v), want it left unowned", got.OwnerUser, err)
	}
}

// ------------------------------------------------------------------ bans ----

// TestBansAreIdempotentAndScopedToARoom covers the moderator's block list. A
// second ban is a double-click, not an error, and lifting one must not lift the
// others.
func TestBansAreIdempotentAndScopedToARoom(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx(t)
	r := newTestRoom(t, s)

	for i := 0; i < 2; i++ {
		if err := s.Ban(ctx, r.ID, "vandal"); err != nil {
			t.Fatalf("ban %d: %v", i+1, err)
		}
	}
	if err := s.Ban(ctx, r.ID, "other"); err != nil {
		t.Fatalf("banning a second painter: %v", err)
	}

	bans, err := s.Bans(ctx, r.ID)
	if err != nil {
		t.Fatalf("loading bans: %v", err)
	}
	if len(bans) != 2 || !bans["vandal"] || !bans["other"] {
		t.Fatalf("bans = %v, want vandal and other exactly once each", bans)
	}

	if err := s.Unban(ctx, r.ID, "vandal"); err != nil {
		t.Fatalf("unbanning: %v", err)
	}
	bans, err = s.Bans(ctx, r.ID)
	if err != nil {
		t.Fatalf("loading bans: %v", err)
	}
	if bans["vandal"] || !bans["other"] {
		t.Errorf("bans = %v, want vandal lifted and other still blocked", bans)
	}
	// Lifting a ban nobody placed is what a moderator clicking twice does.
	if err := s.Unban(ctx, r.ID, "never-banned"); err != nil {
		t.Errorf("unbanning somebody who was not banned: %v", err)
	}
}

// ----------------------------------------------------------------- users ----

// newTestUser registers an account with a name nothing else will collide with,
// and removes it afterwards. usernames are globally unique, so a fixed name
// would make the test fail the second time it ran.
func newTestUser(t *testing.T, s *Store, name string) User {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	u, err := s.CreateUser(ctx, fmt.Sprintf("storetest_%s_%d", name, time.Now().UnixNano()), "hash-"+name)
	if err != nil {
		t.Fatalf("creating user %q: %v", name, err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if _, err := s.pool.Query(cleanupCtx, `delete from users where id = $1`, u.ID); err != nil {
			t.Errorf("cleaning up user %d: %v", u.ID, err)
		}
	})
	return u
}

// TestUsersAreUniqueAndFoundCaseInsensitively covers sign-up and sign-in. The
// display name keeps the case somebody typed while the lookup ignores it, so
// "Bess" and "bess" cannot become two accounts that both look like the same
// person on screen.
func TestUsersAreUniqueAndFoundCaseInsensitively(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx(t)

	u := newTestUser(t, s, "Case")
	if u.ID == 0 || u.CreatedAt.IsZero() {
		t.Errorf("the new account is %+v, want an id and a creation time", u)
	}
	if u.PwHash != "hash-Case" {
		t.Errorf("the stored hash is %q, want the one that was handed in; this package does no cryptography", u.PwHash)
	}

	for _, spelling := range []string{u.Username, strings.ToUpper(u.Username), strings.ToLower(u.Username)} {
		got, err := s.UserByName(ctx, spelling)
		if err != nil {
			t.Fatalf("looking up %q: %v", spelling, err)
		}
		if got.ID != u.ID {
			t.Errorf("looking up %q found account %d, want %d", spelling, got.ID, u.ID)
		}
		if got.Username != u.Username {
			t.Errorf("account %d displays as %q, want the case it was registered with, %q",
				got.ID, got.Username, u.Username)
		}
	}

	// Registering the same name differently cased has to collide, or two people
	// end up with accounts nobody can tell apart.
	if _, err := s.CreateUser(ctx, strings.ToUpper(u.Username), "another"); !errors.Is(err, ErrUsernameTaken) {
		t.Errorf("registering %q a second time = %v, want ErrUsernameTaken", strings.ToUpper(u.Username), err)
	}

	byID, err := s.UserByID(ctx, u.ID)
	if err != nil || byID.Username != u.Username {
		t.Errorf("account %d by id = %+v (err %v), want %+v", u.ID, byID, err, u)
	}
	if _, err := s.UserByID(ctx, 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("account 0 = %v, want ErrNotFound; zero is the id callers use for nobody", err)
	}
	if _, err := s.UserByName(ctx, "storetest_nobody_at_all"); !errors.Is(err, ErrNotFound) {
		t.Errorf("an unregistered name = %v, want ErrNotFound", err)
	}
}

// ----------------------------------------------------------------- ready ----

// TestReadyAnswersWhenTheDatabaseDoes covers the readiness probe, which is what
// an orchestrator uses to decide whether to send this instance traffic.
func TestReadyAnswersWhenTheDatabaseDoes(t *testing.T) {
	s := newTestStore(t)
	if err := s.Ready(testCtx(t)); err != nil {
		t.Errorf("Ready = %v against a database that is plainly answering", err)
	}
}
