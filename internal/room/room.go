// Package room turns the single-canvas engine into a multi-tenant one. A Room
// is a live canvas plus the goroutines that fan its updates out and write them
// down; a Registry owns the set of rooms that are currently in memory.
//
// Rooms are loaded lazily on first request and evicted when nobody has touched
// them for a while, so a thousand dormant rooms cost a row in Postgres rather
// than a resident grid and two goroutines.
package room

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Har103/pixelforge/internal/canvas"
	"github.com/Har103/pixelforge/internal/hub"
	"github.com/Har103/pixelforge/internal/store"
)

// Errors a caller is expected to handle.
var (
	ErrNotFound = errors.New("room: no such room")
	ErrPaused   = errors.New("room: the owner has paused this canvas")
	ErrBanned   = errors.New("room: you are blocked from this canvas")
	ErrLocked   = errors.New("room: that area is locked by the owner")

	// ErrRoomClosed is returned by a room that has been released from memory.
	//
	// It exists because a released room is still a perfectly usable Go object:
	// its grid answers, its hub accepts, and a request holding a pointer to it
	// will happily paint into a canvas nobody will ever read again. The write-
	// behind loop that would have persisted the pixel has returned, so the
	// placement is accepted, broadcast, and lost - and the visitor is told
	// nothing. Refusing is the only honest answer; the caller re-resolves the
	// slug and gets the live room.
	ErrRoomClosed = errors.New("room: this canvas was released from memory, try again")
)

// Room is a canvas that is currently resident in memory.
type Room struct {
	Meta   store.Room
	Canvas *canvas.Canvas
	Hub    *hub.Hub

	store *store.Store
	log   *slog.Logger

	queue    chan store.Placement
	hubDone  chan struct{}
	stopOnce sync.Once
	stopped  chan struct{}
	wg       sync.WaitGroup

	// lastTouch drives idle eviction. Reading it needs no lock because it is
	// only ever a monotonic timestamp.
	lastTouch atomic.Int64

	// dropped counts history entries the write-behind buffer had no room for.
	// It is the trigger for an early snapshot: the moment the log stops being a
	// complete account of the canvas, the snapshot is the only thing standing
	// between a crash and a wrong grid.
	dropped atomic.Int64

	mu     sync.RWMutex
	bans   map[string]bool
	locks  []Lock
	paused bool

	// cursors is deliberately ephemeral: where somebody's pointer is right now
	// is interesting for a second and worthless afterwards, so it never touches
	// the database and never survives a restart. Seeing other people move is
	// what makes a shared canvas feel like a room rather than a page that
	// updates, and it is the cheapest possible way to convey that.
	curMu    sync.Mutex
	cursors  map[string]Cursor
	curDirty bool
}

// Cursor is one painter's pointer position and the colour they have selected,
// so their cursor shows what they are about to paint.
type Cursor struct {
	UID    string `json:"u"`
	X      int    `json:"x"`
	Y      int    `json:"y"`
	Colour uint8  `json:"k"`
	at     time.Time
}

// cursorTTL is how long a pointer stays on screen after its owner stops moving.
// Long enough to survive a pause for thought, short enough that a closed tab
// does not leave a ghost.
const cursorTTL = 6 * time.Second

// Lock is a rectangle the owner has frozen. Coordinates are inclusive.
type Lock struct {
	X1, Y1, X2, Y2 int
}

// Contains reports whether a cell falls inside the locked rectangle.
func (l Lock) Contains(x, y int) bool {
	return x >= l.X1 && x <= l.X2 && y >= l.Y1 && y <= l.Y2
}

// Slug is the room's URL segment.
func (r *Room) Slug() string { return r.Meta.Slug }

// Paused reports whether painting is currently disabled.
func (r *Room) Paused() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.paused
}

// SetPaused turns painting on or off and tells every connected client.
func (r *Room) SetPaused(ctx context.Context, paused bool) error {
	r.mu.Lock()
	r.paused = paused
	r.Meta.Paused = paused
	meta := r.Meta
	r.mu.Unlock()

	r.Hub.BroadcastJSON(map[string]any{"t": "room", "paused": paused})
	return r.store.UpdateRoom(ctx, meta)
}

// Banned reports whether a painter is blocked here.
func (r *Room) Banned(uid string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.bans[uid]
}

// Ban blocks a painter and drops nothing they have already drawn.
func (r *Room) Ban(ctx context.Context, uid string) error {
	r.mu.Lock()
	r.bans[uid] = true
	r.mu.Unlock()
	return r.store.Ban(ctx, r.Meta.ID, uid)
}

// Unban lifts a block.
func (r *Room) Unban(ctx context.Context, uid string) error {
	r.mu.Lock()
	delete(r.bans, uid)
	r.mu.Unlock()
	return r.store.Unban(ctx, r.Meta.ID, uid)
}

// Locks returns the frozen rectangles.
func (r *Room) Locks() []Lock {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Lock, len(r.locks))
	copy(out, r.locks)
	return out
}

// SetLocks replaces the frozen rectangles wholesale and writes them down.
//
// Writing them down is the whole point: a lock is a moderator saying "this part
// is finished, leave it alone", and until now that decision lived only in
// memory. Twenty minutes after the last person left, the room was released, and
// it came back without them - the protected area silently paintable again, with
// nothing on anyone's screen to say it had changed.
func (r *Room) SetLocks(ctx context.Context, locks []Lock) error {
	r.mu.Lock()
	r.locks = append(r.locks[:0], locks...)
	r.mu.Unlock()
	r.Hub.BroadcastJSON(map[string]any{"t": "locks", "locks": locks})

	rects := make([]store.LockRect, len(locks))
	for i, l := range locks {
		rects[i] = store.LockRect{X1: l.X1, Y1: l.Y1, X2: l.X2, Y2: l.Y2}
	}
	if err := r.store.SetLocks(ctx, r.Meta.ID, rects); err != nil {
		return err
	}
	r.Touch()
	return nil
}

func (r *Room) locked(x, y int) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, l := range r.locks {
		if l.Contains(x, y) {
			return true
		}
	}
	return false
}

// SetCursor records where a painter's pointer is. Out-of-range coordinates are
// dropped rather than clamped: a cursor at the edge because the client sent
// nonsense is more confusing than no cursor at all.
func (r *Room) SetCursor(uid string, x, y int, colour uint8) {
	if uid == "" || x < 0 || y < 0 || x >= r.Canvas.Width() || y >= r.Canvas.Height() {
		return
	}
	r.curMu.Lock()
	prev, existed := r.cursors[uid]
	if !existed || prev.X != x || prev.Y != y || prev.Colour != colour {
		r.curDirty = true
	}
	r.cursors[uid] = Cursor{UID: uid, X: x, Y: y, Colour: colour, at: time.Now()}
	r.curMu.Unlock()
}

// DropCursor removes a painter's pointer, called when their socket closes so
// the last position does not linger for the full TTL.
func (r *Room) DropCursor(uid string) {
	r.curMu.Lock()
	if _, ok := r.cursors[uid]; ok {
		delete(r.cursors, uid)
		r.curDirty = true
	}
	r.curMu.Unlock()
}

// liveCursors returns the cursors still within the TTL, expiring the rest.
func (r *Room) liveCursors() ([]Cursor, bool) {
	now := time.Now()
	r.curMu.Lock()
	defer r.curMu.Unlock()

	out := make([]Cursor, 0, len(r.cursors))
	for uid, c := range r.cursors {
		if now.Sub(c.at) > cursorTTL {
			delete(r.cursors, uid)
			r.curDirty = true
			continue
		}
		out = append(out, c)
	}
	dirty := r.curDirty
	r.curDirty = false
	return out, dirty
}

// Closed reports whether this room has been released from memory. A caller
// holding a pointer to a closed room should resolve the slug again rather than
// keep using it: the registry has already replaced it.
func (r *Room) Closed() bool {
	select {
	case <-r.stopped:
		return true
	default:
		return false
	}
}

// Touch records that somebody is using this room, which keeps it resident and
// orders it on the browse page.
func (r *Room) Touch() { r.lastTouch.Store(time.Now().UnixNano()) }

// Idle reports how long since anything happened here.
func (r *Room) Idle() time.Duration {
	return time.Since(time.Unix(0, r.lastTouch.Load()))
}

// Place applies one painter's placement and pushes it to history and to every
// connected client. It is the only write path into a room.
func (r *Room) Place(x, y int, colour uint8, uid string) (canvas.Pixel, error) {
	// Checked before anything else, because everything after this point either
	// mutates state that will never be written down or publishes to a hub whose
	// loop has already returned - and publishing to a stopped hub appends to a
	// pending buffer that nothing will ever drain.
	if r.Closed() {
		return canvas.Pixel{}, ErrRoomClosed
	}
	if r.Paused() {
		return canvas.Pixel{}, ErrPaused
	}
	if r.Banned(uid) {
		return canvas.Pixel{}, ErrBanned
	}
	if r.locked(x, y) {
		return canvas.Pixel{}, ErrLocked
	}

	px, err := r.Canvas.Place(x, y, colour, uid, time.Now())
	if err != nil {
		return canvas.Pixel{}, err
	}

	r.Touch()
	r.record(store.Placement{Seq: px.Seq, X: px.X, Y: px.Y, Color: px.Color, UID: px.UID, At: px.At})
	r.Hub.Publish(px)
	return px, nil
}

// Clear wipes the grid and marks the room's history undone.
func (r *Room) Clear(ctx context.Context) error {
	r.Canvas.Clear()
	r.Hub.BroadcastJSON(map[string]any{"t": "cleared"})
	r.Touch()
	if err := r.store.ClearRoom(ctx, r.Meta.ID); err != nil {
		return err
	}
	return r.snapshot(ctx)
}

// UndoPainter removes everything one painter drew and rebuilds the grid from
// what is left. Rebuilding is the honest way to do it: a pixel they overwrote
// has to come back as whatever was underneath, which only the log knows.
func (r *Room) UndoPainter(ctx context.Context, uid string) (int64, error) {
	n, err := r.store.UndoUser(ctx, r.Meta.ID, uid)
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, nil
	}
	if err := r.rebuild(ctx); err != nil {
		return n, err
	}
	r.Hub.BroadcastJSON(map[string]any{"t": "rebuilt", "undone": n, "uid": uid})
	return n, nil
}

// Errors from UndoOwn that the caller turns into a message.
var (
	ErrNothingToUndo = errors.New("room: you have not painted anything here yet")
	ErrPaintedOver   = errors.New("room: somebody has painted over that since")
)

// UndoOwn reverts a painter's most recent placement and restores whatever was
// underneath it, from the log rather than from a guess.
//
// It refuses when somebody else has painted over the cell in the meantime.
// Silently reverting to a colour a third party chose after you would be a
// stranger's pixel disappearing because you clicked undo, which is worse than
// being told no.
func (r *Room) UndoOwn(ctx context.Context, uid string) (canvas.Pixel, error) {
	mine, err := r.store.LatestOwnPlacement(ctx, r.Meta.ID, uid)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return canvas.Pixel{}, ErrNothingToUndo
		}
		return canvas.Pixel{}, err
	}

	top, err := r.store.TopPlacementAt(ctx, r.Meta.ID, mine.X, mine.Y)
	if err != nil {
		return canvas.Pixel{}, err
	}
	if top.Seq != mine.Seq {
		return canvas.Pixel{}, ErrPaintedOver
	}

	beneath, err := r.store.ColourBeneath(ctx, r.Meta.ID, mine.X, mine.Y, mine.Seq)
	if err != nil {
		return canvas.Pixel{}, err
	}
	if err := r.store.MarkUndone(ctx, r.Meta.ID, mine.Seq); err != nil {
		return canvas.Pixel{}, err
	}

	// Clear the cooldown before applying, not only after. Whoever presses undo
	// is almost always still cooling down from the pixel they are taking back,
	// and a placement refused for that reason would leave the undo unbroadcast:
	// every open tab would keep showing a pixel the log says is gone.
	r.ClearCooldown(uid)

	px, err := r.Canvas.Place(mine.X, mine.Y, beneath, uid, time.Now())
	if err != nil {
		// The cell already holds that colour, which can happen if the canvas
		// was rebuilt underneath us. The log is right either way.
		r.Canvas.Apply(mine.X, mine.Y, beneath, r.Canvas.Seq())
		px = canvas.Pixel{X: mine.X, Y: mine.Y, Color: beneath, UID: uid}
	}

	// Announced as a retraction rather than published as a placement. It reaches
	// the same clients over the same transports either way, but a client that
	// counts it as a placement drifts one ahead of the server on every undo,
	// and one showing that cell's history has no way to know the entry on
	// screen has just been taken back.
	r.Hub.BroadcastJSON(map[string]any{
		"t": "undone", "x": px.X, "y": px.Y, "c": px.Color, "uid": uid, "s": px.Seq,
	})
	// Applying it re-armed the cooldown, so clear it once more: undoing a
	// misclick must not cost the painter their turn.
	r.ClearCooldown(uid)
	r.Touch()

	// The repair is deliberately not appended to the log. An undo is a
	// retraction of a placement, not a placement of its own, and a row for it
	// would become the painter's most recent placement - so their next undo
	// would take back the repair instead of the pixel before it. Durability
	// comes from a snapshot instead, the way Clear and UndoPainter get it:
	// without one, a restart could replay an older snapshot and paint the
	// undone pixel straight back.
	if err := r.snapshot(ctx); err != nil {
		r.log.Error("snapshotting after an undo", "room", r.Meta.Slug, "err", err)
	}
	return px, nil
}

// ClearCooldown lets a painter act again immediately. Used after an undo.
func (r *Room) ClearCooldown(uid string) { r.Canvas.ClearCooldown(uid) }

// rebuild replays the surviving log from an empty grid.
func (r *Room) rebuild(ctx context.Context) error {
	r.Canvas.Clear()
	if _, err := r.store.ReplayAfter(ctx, r.Meta.ID, 0, func(seq int64, x, y int, c uint8) {
		r.Canvas.Apply(x, y, c, seq)
	}); err != nil {
		return err
	}
	return r.snapshot(ctx)
}

func (r *Room) record(p store.Placement) {
	if r.store.Ephemeral() {
		return
	}
	select {
	case r.queue <- p:
	default:
		// The writer has fallen far enough behind that the buffer is full.
		// Losing a line of history still beats stalling everyone's paint, so the
		// entry goes - but the claim that "the next snapshot captures the pixel
		// regardless" is only true if a snapshot actually happens, and the
		// periodic one is up to twenty seconds away. Load testing put 38% of a
		// canvas on the wrong colour after a crash in that window. Recording the
		// drop lets the write-behind loop pull the snapshot forward, which is
		// the only thing that can save these pixels.
		r.dropped.Add(1)
		r.log.Warn("placement buffer full, dropping history entry",
			"room", r.Meta.Slug, "seq", p.Seq)
	}
}

func (r *Room) snapshot(ctx context.Context) error {
	pixels, seq := r.Canvas.Snapshot()
	return r.store.SaveSnapshot(ctx, r.Meta.ID, store.Snapshot{
		Width: r.Canvas.Width(), Height: r.Canvas.Height(), Pixels: pixels, Seq: seq,
	})
}

// run drives the room's background loops until stop is called.
func (r *Room) run() {
	r.wg.Add(3)
	go func() { defer r.wg.Done(); r.Hub.Run(r.hubDone) }()
	go func() { defer r.wg.Done(); r.writeBehind() }()
	go func() { defer r.wg.Done(); r.broadcastCursors() }()
}

// broadcastCursors ships pointer positions on a slower tick than placements.
// Cursors are cosmetic, so they get a coarser rate and are skipped entirely
// when nothing has moved - an idle room should cost nothing.
func (r *Room) broadcastCursors() {
	t := time.NewTicker(120 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-r.stopped:
			return
		case <-t.C:
			if r.Hub.Count() == 0 {
				continue
			}
			cursors, changed := r.liveCursors()
			if !changed {
				continue
			}
			r.Hub.BroadcastJSON(map[string]any{"t": "cursors", "c": cursors})
		}
	}
}

// writeBehind batches placements into the database so painting never waits on
// it, and snapshots the grid periodically so recovery is cheap.
func (r *Room) writeBehind() {
	flush := time.NewTicker(250 * time.Millisecond)
	defer flush.Stop()
	snap := time.NewTicker(20 * time.Second)
	defer snap.Stop()

	pending := make([]store.Placement, 0, 400)
	failing := false

	// A batch the database refused is kept and retried on the next tick rather
	// than dropped on the floor. The original code cleared pending whether or
	// not the write succeeded, so a database outage silently destroyed every
	// placement made during it - a load test with a 6,000-placement burst across
	// an outage found 2,002 of them had no row afterwards. The grid survives
	// because a later snapshot rescues it; the log does not, and the log is what
	// the leaderboard, the time-lapse, per-cell provenance and undo all read.
	//
	// retainLimit bounds the retry so a long outage cannot turn a fixed-size
	// buffer into an unbounded one. Past it the oldest entries go, because if
	// something has to be lost it should be the history furthest from what
	// anyone is looking at.
	const retainLimit = 20000

	snapshotNow := func(why string) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		if err := r.snapshot(ctx); err != nil {
			r.log.Error("writing snapshot", "room", r.Meta.Slug, "why", why, "err", err)
		}
		cancel()
	}

	write := func() {
		if len(pending) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		err := r.store.AppendPlacements(ctx, r.Meta.ID, pending)
		cancel()

		if err == nil {
			if failing {
				r.log.Info("placement writes recovered",
					"room", r.Meta.Slug, "flushed", len(pending))
				failing = false
			}
			pending = pending[:0]
			return
		}

		failing = true
		r.log.Error("writing placements, will retry",
			"room", r.Meta.Slug, "count", len(pending), "err", err)
		if over := len(pending) - retainLimit; over > 0 {
			r.log.Warn("dropping the oldest unwritten history to bound memory",
				"room", r.Meta.Slug, "dropped", over)
			r.dropped.Add(int64(over))
			pending = append(pending[:0], pending[over:]...)
		}
	}

	for {
		select {
		case <-r.stopped:
			for {
				select {
				case p := <-r.queue:
					pending = append(pending, p)
					if len(pending) >= 400 {
						write()
					}
					continue
				default:
				}
				break
			}
			write()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			if err := r.snapshot(ctx); err != nil {
				r.log.Error("writing shutdown snapshot", "room", r.Meta.Slug, "err", err)
			}
			cancel()
			return

		case p := <-r.queue:
			pending = append(pending, p)
			if len(pending) >= 400 {
				write()
			}

		case <-flush.C:
			write()
			// Once history has been dropped the log can no longer rebuild this
			// canvas, and the snapshot is all that stands between a crash and a
			// grid full of wrong colours. Waiting out the rest of the twenty
			// second cycle to write one is exactly the wrong instinct.
			if r.dropped.Swap(0) > 0 {
				snapshotNow("catch-up snapshot after dropping history")
			}

		case <-snap.C:
			r.dropped.Store(0)
			snapshotNow("periodic snapshot")
		}
	}
}

// stop drains the room and releases its goroutines.
func (r *Room) stop() {
	r.stopOnce.Do(func() {
		close(r.stopped)
		close(r.hubDone)
		done := make(chan struct{})
		go func() { r.wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			r.log.Warn("room did not shut down in time", "room", r.Meta.Slug)
		}
	})
}

// Info is the room description handed to clients.
type Info struct {
	Slug       string   `json:"slug"`
	Name       string   `json:"name"`
	Width      int      `json:"width"`
	Height     int      `json:"height"`
	Palette    []string `json:"palette"`
	PaletteKey string   `json:"paletteKey"`
	CooldownMs int64    `json:"cooldownMs"`
	Visibility string   `json:"visibility"`
	Paused     bool     `json:"paused"`
	Seq        int64    `json:"seq"`
	Clients    int      `json:"clients"`
	Locks      []Lock   `json:"locks"`
	CreatedAt  string   `json:"createdAt"`
}

// Info describes the room for the client bootstrap.
func (r *Room) Info() Info {
	return Info{
		Slug:       r.Meta.Slug,
		Name:       r.Meta.Name,
		Width:      r.Canvas.Width(),
		Height:     r.Canvas.Height(),
		Palette:    r.Canvas.Palette(),
		PaletteKey: r.Meta.Palette,
		CooldownMs: r.Canvas.Cooldown().Milliseconds(),
		Visibility: r.Meta.Visibility,
		Paused:     r.Paused(),
		Seq:        r.Canvas.Seq(),
		Clients:    r.Hub.Count(),
		Locks:      r.Locks(),
		CreatedAt:  r.Meta.CreatedAt.UTC().Format(time.RFC3339),
	}
}

var _ = fmt.Sprintf
