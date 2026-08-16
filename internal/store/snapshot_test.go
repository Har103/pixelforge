package store

import (
	"errors"
	"testing"
	"time"
)

// A snapshot is the shortcut that stops every boot replaying the whole log, so
// what it round-trips has to be exact. It travels as a bytea, which is the one
// column in this schema where an encoding mistake is invisible until somebody's
// canvas comes back as noise.

// TestSnapshotRoundTripsEveryByteValue paints one cell of every palette index
// there can be. A grid is one byte per pixel, so the blob contains 0x00, the
// backslash and the quote, which are exactly the bytes a hand-rolled escape
// gets wrong.
func TestSnapshotRoundTripsEveryByteValue(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx(t)
	r := newTestRoom(t, s)

	pixels := make([]byte, 256)
	for i := range pixels {
		pixels[i] = byte(i)
	}
	if err := s.SaveSnapshot(ctx, r.ID, Snapshot{Width: 16, Height: 16, Pixels: pixels, Seq: 42}); err != nil {
		t.Fatalf("saving a snapshot: %v", err)
	}

	got, err := s.LoadSnapshot(ctx, r.ID)
	if err != nil {
		t.Fatalf("loading a snapshot: %v", err)
	}
	if got.Width != 16 || got.Height != 16 || got.Seq != 42 {
		t.Errorf("snapshot came back %dx%d at sequence %d, want 16x16 at 42", got.Width, got.Height, got.Seq)
	}
	if len(got.Pixels) != len(pixels) {
		t.Fatalf("snapshot came back %d bytes, want %d", len(got.Pixels), len(pixels))
	}
	for i := range pixels {
		if got.Pixels[i] != pixels[i] {
			t.Fatalf("byte %d came back as %d, want %d — one wrong byte is one wrong pixel on somebody's canvas",
				i, got.Pixels[i], pixels[i])
		}
	}
}

// TestSnapshotReplacesTheOneBefore pins the upsert. A room writes a snapshot
// every twenty seconds for as long as it is resident; if each one were a new
// row, the table would grow without bound and a restore would have to guess
// which row was current.
func TestSnapshotReplacesTheOneBefore(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx(t)
	r := newTestRoom(t, s)

	for i := 1; i <= 3; i++ {
		if err := s.SaveSnapshot(ctx, r.ID, Snapshot{
			Width: 2, Height: 2, Pixels: []byte{byte(i), byte(i), byte(i), byte(i)}, Seq: int64(i * 10),
		}); err != nil {
			t.Fatalf("saving snapshot %d: %v", i, err)
		}
	}

	got, err := s.LoadSnapshot(ctx, r.ID)
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	if got.Seq != 30 || got.Pixels[0] != 3 {
		t.Errorf("snapshot = sequence %d starting %d, want the newest: sequence 30 starting 3", got.Seq, got.Pixels[0])
	}
	row, err := s.pool.QueryRow(ctx, `select count(*) from room_snapshots where room_id = $1`, r.ID)
	if err != nil {
		t.Fatalf("counting snapshots: %v", err)
	}
	if n := string(row[0]); n != "1" {
		t.Errorf("the room has %s snapshot rows, want exactly 1", n)
	}
}

// TestLoadSnapshotForARoomWithoutOneIsNotAnError is the first boot of every
// room: there is no snapshot yet, and the registry has to be able to tell that
// apart from a broken database, because one means "start empty" and the other
// means "refuse to serve".
func TestLoadSnapshotForARoomWithoutOneIsNotAnError(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx(t)
	r := newTestRoom(t, s)

	if _, err := s.LoadSnapshot(ctx, r.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("loading a snapshot that was never saved = %v, want ErrNotFound", err)
	}
}

// TestSnapshotKeepsWhateverDimensionsItWasGiven documents a deliberate absence.
// The store does not check a snapshot against its room, because it does not
// know the room's dimensions and inventing an opinion here would be the wrong
// place for it. What it must do is report the dimensions honestly, since that
// is the only way the registry can notice the mismatch and replay in full
// instead of loading a grid of the wrong shape.
func TestSnapshotKeepsWhateverDimensionsItWasGiven(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx(t)
	r := newTestRoom(t, s) // 32x32

	cases := []struct {
		name          string
		width, height int
		pixels        []byte
	}{
		{name: "narrower than the room", width: 8, height: 8, pixels: make([]byte, 64)},
		{name: "wider than the room", width: 64, height: 64, pixels: make([]byte, 64*64)},
		{
			// The pair can also disagree with itself. Nothing stops a caller
			// saving this, and a reader that trusted width times height rather
			// than the blob would run off the end of it.
			name: "dimensions that do not match the blob", width: 32, height: 32, pixels: make([]byte, 3),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.SaveSnapshot(ctx, r.ID, Snapshot{
				Width: tc.width, Height: tc.height, Pixels: tc.pixels, Seq: 1,
			}); err != nil {
				t.Fatalf("saving: %v", err)
			}
			got, err := s.LoadSnapshot(ctx, r.ID)
			if err != nil {
				t.Fatalf("loading: %v", err)
			}
			if got.Width != tc.width || got.Height != tc.height || len(got.Pixels) != len(tc.pixels) {
				t.Errorf("snapshot came back %dx%d with %d bytes, want %dx%d with %d — the caller cannot detect a mismatch it is not told about",
					got.Width, got.Height, len(got.Pixels), tc.width, tc.height, len(tc.pixels))
			}
		})
	}
}

// TestSnapshotWithNoPixelsFailsLoudly covers the degenerate call. The column is
// NOT NULL on purpose: a snapshot of nothing would restore as a blank canvas
// and quietly discard the log position that came with it.
func TestSnapshotWithNoPixelsFailsLoudly(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx(t)
	r := newTestRoom(t, s)

	if err := s.SaveSnapshot(ctx, r.ID, Snapshot{Width: 4, Height: 4, Pixels: nil, Seq: 1}); err == nil {
		t.Error("saving a snapshot with no pixels succeeded, so a room can be persisted as nothing at all")
	}
	// An empty-but-present blob is a different thing and is allowed through: it
	// is what a zero-sized grid would legitimately produce.
	if err := s.SaveSnapshot(ctx, r.ID, Snapshot{Width: 0, Height: 0, Pixels: []byte{}, Seq: 1}); err != nil {
		t.Errorf("saving an empty snapshot: %v", err)
	}
	got, err := s.LoadSnapshot(ctx, r.ID)
	if err != nil || len(got.Pixels) != 0 {
		t.Errorf("the empty snapshot came back as %v (err %v), want no pixels", got.Pixels, err)
	}
}

// TestRestoreThenReplayLandsWhereTheLogSays is the boot path end to end, and
// the one place the snapshot and the log have to agree. The room saves a
// snapshot at sequence N, keeps painting, and then restarts: restoring the
// snapshot and replaying everything after N must reproduce the grid exactly.
// An inclusive cursor repaints N, an exclusive one that starts from zero
// repaints the whole log, and both are only visible as the wrong colour on the
// cells that were painted twice.
func TestRestoreThenReplayLandsWhereTheLogSays(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx(t)
	r := newTestRoom(t, s)

	const w, h = 4, 4
	grid := make([]byte, w*h)
	apply := func(x, y int, colour uint8) { grid[y*w+x] = colour }

	// Before the snapshot: two cells, one of them painted twice.
	early := []Placement{
		{Seq: 1, X: 0, Y: 0, Color: 3, UID: "bess", At: time.Now()},
		{Seq: 2, X: 1, Y: 1, Color: 5, UID: "alec", At: time.Now()},
		{Seq: 3, X: 0, Y: 0, Color: 7, UID: "carl", At: time.Now()},
	}
	paintAll(t, s, r.ID, early)
	for _, p := range early {
		apply(p.X, p.Y, p.Color)
	}
	if err := s.SaveSnapshot(ctx, r.ID, Snapshot{Width: w, Height: h, Pixels: grid, Seq: 3}); err != nil {
		t.Fatalf("snapshotting: %v", err)
	}

	// After the snapshot: the same cells again, plus a new one, so a replay that
	// starts from the wrong place is visible rather than merely inefficient.
	late := []Placement{
		{Seq: 4, X: 0, Y: 0, Color: 9, UID: "bess", At: time.Now()},
		{Seq: 5, X: 3, Y: 3, Color: 11, UID: "alec", At: time.Now()},
		{Seq: 6, X: 1, Y: 1, Color: 13, UID: "carl", At: time.Now()},
	}
	paintAll(t, s, r.ID, late)
	for _, p := range late {
		apply(p.X, p.Y, p.Color)
	}
	// And one of the late placements is retired before the restart, so the
	// restore must not paint it back on.
	if err := s.MarkUndone(ctx, r.ID, 6); err != nil {
		t.Fatalf("marking undone: %v", err)
	}

	// The restart. Everything below this line knows only what is in the
	// database.
	snap, err := s.LoadSnapshot(ctx, r.ID)
	if err != nil {
		t.Fatalf("restoring the snapshot: %v", err)
	}
	restored := make([]byte, len(snap.Pixels))
	copy(restored, snap.Pixels)
	var replayedSeqs []int64
	if _, err := s.ReplayAfter(ctx, r.ID, snap.Seq, func(seq int64, x, y int, c uint8) {
		replayedSeqs = append(replayedSeqs, seq)
		restored[y*w+x] = c
	}); err != nil {
		t.Fatalf("replaying after the snapshot: %v", err)
	}

	if len(replayedSeqs) != 2 || replayedSeqs[0] != 4 || replayedSeqs[1] != 5 {
		t.Errorf("replayed sequences %v after a snapshot at 3, want 4 and 5: 3 would be applied twice and 6 was taken back",
			replayedSeqs)
	}

	// What the log says the canvas is: the snapshot, plus the live placements
	// after it.
	want := make([]byte, w*h)
	copy(want, grid)
	want[1*w+1] = 5 // sequence 6 was retired, so alec's 5 is what remains at (1,1)
	for i := range want {
		if restored[i] != want[i] {
			t.Fatalf("after restore and replay, cell (%d,%d) is colour %d, want %d — the grid on screen is not the grid in the log",
				i%w, i/w, restored[i], want[i])
		}
	}
}
