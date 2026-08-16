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

// SetLocks replaces the frozen rectangles wholesale.
func (r *Room) SetLocks(locks []Lock) {
	r.mu.Lock()
	r.locks = append(r.locks[:0], locks...)
	r.mu.Unlock()
	r.Hub.BroadcastJSON(map[string]any{"t": "locks", "locks": locks})
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

	// Apply as a fresh placement so every connected client sees it.
	px, err := r.Canvas.Place(mine.X, mine.Y, beneath, uid, time.Now())
	if err != nil {
		// The cell already holds that colour, which can happen if the canvas
		// was rebuilt underneath us. Nothing to broadcast, but the log is right.
		r.Canvas.Apply(mine.X, mine.Y, beneath, r.Canvas.Seq())
		px = canvas.Pixel{X: mine.X, Y: mine.Y, Color: beneath, UID: uid}
	} else {
		r.Hub.Publish(px)
	}
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
		// Losing a line of history beats stalling everyone's paint; the next
		// snapshot captures the pixel regardless.
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

	write := func() {
		if len(pending) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		if err := r.store.AppendPlacements(ctx, r.Meta.ID, pending); err != nil {
			r.log.Error("writing placements", "room", r.Meta.Slug, "count", len(pending), "err", err)
		}
		cancel()
		pending = pending[:0]
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

		case <-snap.C:
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			if err := r.snapshot(ctx); err != nil {
				r.log.Error("writing periodic snapshot", "room", r.Meta.Slug, "err", err)
			}
			cancel()
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
