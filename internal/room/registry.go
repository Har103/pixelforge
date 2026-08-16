package room

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/Har103/pixelforge/internal/canvas"
	"github.com/Har103/pixelforge/internal/hub"
	"github.com/Har103/pixelforge/internal/store"
)

// Limits on what a room may be. They exist because every resident room costs a
// grid in memory, and because a canvas nobody can fill is not fun.
const (
	MinDim        = 16
	MaxDim        = 512
	MaxCooldownMs = 3_600_000
	MaxNameLen    = 60
	MaxRooms      = 64 // resident at once; beyond this the idlest is evicted
	IdleTTL       = 20 * time.Minute
)

// Spec describes a room somebody wants to create.
//
// CooldownMs uses -1 for "not specified", because zero is a perfectly good
// answer: "no cooldown" is a real choice in the creation form, and treating it
// as unset would silently hand the room a 750ms cooldown nobody asked for.
type Spec struct {
	Name       string
	Width      int
	Height     int
	Palette    string
	CooldownMs int
	Unlisted   bool
	OwnerHash  string
	OwnerUser  int64
}

// Normalise clamps a spec into the supported range and fills in defaults. It
// never rejects: a creation form should not be able to fail on a slider.
func (s *Spec) Normalise() {
	s.Name = strings.TrimSpace(s.Name)
	if s.Name == "" {
		s.Name = "Untitled canvas"
	}
	if len([]rune(s.Name)) > MaxNameLen {
		s.Name = string([]rune(s.Name)[:MaxNameLen])
	}
	s.Name = stripControl(s.Name)

	// Zero is never a valid dimension, so it can mean "unset" for these two.
	s.Width = clampInt(s.Width, MinDim, MaxDim, 128)
	s.Height = clampInt(s.Height, MinDim, MaxDim, 128)

	switch {
	case s.CooldownMs < 0:
		s.CooldownMs = 750
	case s.CooldownMs > MaxCooldownMs:
		s.CooldownMs = MaxCooldownMs
	}
	s.Palette = canvas.NormalisePaletteKey(s.Palette)
}

func clampInt(v, lo, hi, def int) int {
	if v == 0 {
		return def
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// stripControl removes control characters, which have no business in a room
// name and make a mess of logs and link previews.
func stripControl(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if unicode.IsControl(r) {
			continue
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

// Registry holds the rooms that are currently in memory.
type Registry struct {
	store *store.Store
	log   *slog.Logger

	mu      sync.Mutex
	rooms   map[string]*Room
	loading map[string]chan struct{}

	maxRooms int
	idleTTL  time.Duration

	// ephemeral rooms live only in this process, for the no-database mode.
	ephemeral bool
}

// NewRegistry creates an empty registry. Call Run to start idle eviction.
func NewRegistry(st *store.Store, log *slog.Logger) *Registry {
	if log == nil {
		log = slog.Default()
	}
	return &Registry{
		store:     st,
		log:       log,
		rooms:     make(map[string]*Room),
		loading:   make(map[string]chan struct{}),
		maxRooms:  MaxRooms,
		idleTTL:   IdleTTL,
		ephemeral: st.Ephemeral(),
	}
}

// Get returns a resident room, loading it from the database if needed.
//
// Concurrent callers for the same slug collapse onto one load: without that,
// a burst of requests to a cold room would each build a grid and start a pair
// of goroutines, and all but one would be thrown away.
func (r *Registry) Get(ctx context.Context, slug string) (*Room, error) {
	slug = strings.ToLower(strings.TrimSpace(slug))
	if slug == "" {
		return nil, ErrNotFound
	}

	for {
		r.mu.Lock()
		if rm, ok := r.rooms[slug]; ok {
			r.mu.Unlock()
			rm.Touch()
			return rm, nil
		}
		if wait, ok := r.loading[slug]; ok {
			r.mu.Unlock()
			select {
			case <-wait:
				continue // whoever was loading has finished; look again
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		done := make(chan struct{})
		r.loading[slug] = done
		r.mu.Unlock()

		rm, err := r.load(ctx, slug)

		r.mu.Lock()
		delete(r.loading, slug)
		if err == nil {
			r.rooms[slug] = rm
		}
		r.mu.Unlock()
		close(done)

		if err != nil {
			return nil, err
		}
		r.evictIfCrowded()
		return rm, nil
	}
}

// Lookup returns a room only if it is already resident, without loading it.
func (r *Registry) Lookup(slug string) (*Room, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rm, ok := r.rooms[strings.ToLower(slug)]
	return rm, ok
}

// load builds a live room from its stored configuration and history.
func (r *Registry) load(ctx context.Context, slug string) (*Room, error) {
	meta, err := r.store.RoomBySlug(ctx, slug)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return r.materialise(ctx, meta)
}

// materialise builds the in-memory half of a room: grid, hub, goroutines.
func (r *Registry) materialise(ctx context.Context, meta store.Room) (*Room, error) {
	board := canvas.New(meta.Width, meta.Height,
		canvas.PaletteFor(meta.Palette),
		time.Duration(meta.CooldownMs)*time.Millisecond)

	rm := &Room{
		Meta:    meta,
		Canvas:  board,
		Hub:     hub.New(r.log.With("room", meta.Slug)),
		store:   r.store,
		log:     r.log,
		queue:   make(chan store.Placement, 4096),
		hubDone: make(chan struct{}),
		stopped: make(chan struct{}),
		bans:    map[string]bool{},
		cursors: map[string]Cursor{},
		paused:  meta.Paused,
	}
	rm.Touch()

	// Restore: newest snapshot, then replay anything the snapshot predates.
	from := int64(0)
	if snap, err := r.store.LoadSnapshot(ctx, meta.ID); err == nil {
		if snap.Width == meta.Width && snap.Height == meta.Height {
			if err := board.Load(snap.Pixels, snap.Seq); err == nil {
				from = snap.Seq
			}
		} else {
			r.log.Warn("snapshot dimensions differ from the room, replaying in full",
				"room", meta.Slug,
				"snapshot", fmt.Sprintf("%dx%d", snap.Width, snap.Height),
				"room_size", fmt.Sprintf("%dx%d", meta.Width, meta.Height))
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}

	replayed, err := r.store.ReplayAfter(ctx, meta.ID, from, func(seq int64, x, y int, c uint8) {
		board.Apply(x, y, c, seq)
	})
	if err != nil {
		return nil, err
	}

	if bans, err := r.store.Bans(ctx, meta.ID); err == nil {
		rm.bans = bans
	}

	rm.run()
	r.log.Info("room resident", "room", meta.Slug,
		"size", fmt.Sprintf("%dx%d", meta.Width, meta.Height),
		"seq", board.Seq(), "replayed", replayed)
	return rm, nil
}

// Create makes a new room and returns it along with the plaintext moderator
// key, which is shown to the creator once and never stored.
func (r *Registry) Create(ctx context.Context, spec Spec) (*Room, error) {
	spec.Normalise()

	visibility := "public"
	if spec.Unlisted {
		visibility = "unlisted"
	}

	// Try a few slugs before giving up: collisions are rare but a creation
	// form that fails on one is a bad experience.
	var meta store.Room
	var err error
	for attempt := 0; attempt < 6; attempt++ {
		slug := NewSlug(spec.Name, attempt)
		meta, err = r.store.CreateRoom(ctx, store.Room{
			Slug:       slug,
			Name:       spec.Name,
			Width:      spec.Width,
			Height:     spec.Height,
			Palette:    spec.Palette,
			CooldownMs: spec.CooldownMs,
			Visibility: visibility,
			OwnerHash:  spec.OwnerHash,
			OwnerUser:  spec.OwnerUser,
		})
		if err == nil {
			break
		}
		if !errors.Is(err, store.ErrSlugTaken) {
			return nil, err
		}
	}
	if err != nil {
		return nil, fmt.Errorf("room: could not find a free slug: %w", err)
	}

	rm, err := r.materialise(ctx, meta)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.rooms[meta.Slug] = rm
	r.mu.Unlock()
	r.evictIfCrowded()
	return rm, nil
}

// evictIfCrowded drops the idlest rooms when too many are resident.
func (r *Registry) evictIfCrowded() {
	r.mu.Lock()
	if len(r.rooms) <= r.maxRooms {
		r.mu.Unlock()
		return
	}
	var idlest *Room
	for _, rm := range r.rooms {
		if idlest == nil || rm.Idle() > idlest.Idle() {
			idlest = rm
		}
	}
	if idlest != nil {
		delete(r.rooms, idlest.Meta.Slug)
	}
	r.mu.Unlock()

	if idlest != nil {
		r.log.Info("evicting room to stay under the resident cap", "room", idlest.Meta.Slug)
		idlest.stop()
	}
}

// Run sweeps idle rooms out of memory until ctx is cancelled, then shuts every
// remaining room down so each writes a final snapshot.
func (r *Registry) Run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			r.shutdown()
			return
		case <-t.C:
			r.sweep()
		}
	}
}

func (r *Registry) sweep() {
	r.mu.Lock()
	var stale []*Room
	for slug, rm := range r.rooms {
		// A room with people in it is never idle, however quiet they are.
		if rm.Hub.Count() == 0 && rm.Idle() > r.idleTTL {
			stale = append(stale, rm)
			delete(r.rooms, slug)
		}
	}
	r.mu.Unlock()

	for _, rm := range stale {
		r.log.Info("room idle, releasing", "room", rm.Meta.Slug, "idle", rm.Idle().Round(time.Second))
		rm.stop()
	}
}

func (r *Registry) shutdown() {
	r.mu.Lock()
	rooms := make([]*Room, 0, len(r.rooms))
	for _, rm := range r.rooms {
		rooms = append(rooms, rm)
	}
	r.rooms = map[string]*Room{}
	r.mu.Unlock()

	var wg sync.WaitGroup
	for _, rm := range rooms {
		wg.Add(1)
		go func(rm *Room) { defer wg.Done(); rm.stop() }(rm)
	}
	wg.Wait()
	r.log.Info("all rooms released", "count", len(rooms))
}

// Resident reports how many rooms are in memory and how many clients they hold.
func (r *Registry) Resident() (rooms, clients int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rm := range r.rooms {
		rooms++
		clients += rm.Hub.Count()
	}
	return
}

// ------------------------------------------------------------------ slugs ---

// Slugs are readable rather than random so a link is repeatable out loud — the
// kind of thing you can say across a room or put on a slide.
var slugAdjectives = []string{
	"amber", "brisk", "calm", "cobalt", "coral", "crisp", "dawn", "dusk",
	"ember", "fern", "frost", "gilded", "hazel", "indigo", "ivory", "jade",
	"lunar", "mellow", "misty", "noble", "opal", "plum", "quiet", "rapid",
	"rustic", "saffron", "scarlet", "silver", "solar", "sunset", "teal",
	"umber", "velvet", "vivid", "warm", "zesty",
}

var slugNouns = []string{
	"anchor", "arbour", "badger", "beacon", "canvas", "cedar", "cinder",
	"comet", "delta", "eagle", "falcon", "fjord", "harbour", "heron", "kite",
	"lantern", "lagoon", "meadow", "mosaic", "otter", "pixel", "prism",
	"quarry", "ridge", "river", "sable", "signal", "summit", "thicket",
	"tundra", "vessel", "willow", "yonder", "zephyr",
}

// NewSlug builds a URL segment. The first attempt tries to derive it from the
// room's name, which makes shared links self-describing; later attempts fall
// back to the word list so a collision resolves without asking the user.
func NewSlug(name string, attempt int) string {
	if attempt == 0 {
		if s := slugify(name); s != "" {
			return s + "-" + randomDigits(4)
		}
	}
	return pick(slugAdjectives) + "-" + pick(slugNouns) + "-" + randomDigits(4)
}

func slugify(name string) string {
	var b strings.Builder
	lastDash := true
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
		if b.Len() >= 32 {
			break
		}
	}
	return strings.Trim(b.String(), "-")
}

func pick(list []string) string {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(list))))
	if err != nil {
		return list[0]
	}
	return list[n.Int64()]
}

func randomDigits(n int) string {
	const digits = "0123456789"
	out := make([]byte, n)
	for i := range out {
		v, err := rand.Int(rand.Reader, big.NewInt(int64(len(digits))))
		if err != nil {
			out[i] = '0'
			continue
		}
		out[i] = digits[v.Int64()]
	}
	return string(out)
}

// ValidSlug reports whether a string could be a slug at all, so a nonsense URL
// never reaches the database.
func ValidSlug(s string) bool {
	if len(s) < 3 || len(s) > 48 {
		return false
	}
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			continue
		}
		return false
	}
	return true
}
