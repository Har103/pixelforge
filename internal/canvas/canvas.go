// Package canvas holds the authoritative pixel grid in memory and applies the
// placement rules. Durability is delegated to Store, which persists
// asynchronously so a paint never waits on a database round trip.
package canvas

import (
	"errors"
	"sync"
	"time"
)

// Errors returned by Place.
var (
	ErrOutOfBounds = errors.New("canvas: coordinates outside the grid")
	ErrBadColour   = errors.New("canvas: colour index not in the palette")
	ErrCooldown    = errors.New("canvas: still cooling down")
	ErrSameColour  = errors.New("canvas: pixel is already that colour")
)

// Pixel is a single placement, as broadcast to clients and appended to history.
type Pixel struct {
	Seq   int64     `json:"s"`
	X     int       `json:"x"`
	Y     int       `json:"y"`
	Color uint8     `json:"c"`
	UID   string    `json:"u"`
	At    time.Time `json:"-"`
}

// Canvas is a fixed-size grid of palette indices, safe for concurrent use.
type Canvas struct {
	mu       sync.RWMutex
	width    int
	height   int
	pixels   []byte
	seq      int64
	cooldown time.Duration

	// palette is fixed for the life of the canvas. A cell is one byte because
	// of it, and a client cannot introduce a colour that is not in it.
	palette []string

	// lastPlace records the most recent placement per user id. It is pruned
	// lazily so an unbounded stream of one-shot visitors cannot grow it without
	// limit.
	lastPlace map[string]time.Time
	lastPrune time.Time
}

// New returns an empty canvas filled with palette index 0. A nil or empty
// palette falls back to Classic rather than producing a canvas nobody can paint.
func New(width, height int, palette []string, cooldown time.Duration) *Canvas {
	if width <= 0 || height <= 0 {
		panic("canvas: dimensions must be positive")
	}
	if len(palette) == 0 {
		palette = Palette
	}
	if len(palette) > 256 {
		// One byte per cell is the whole storage design; refusing here is
		// better than silently truncating a room's palette.
		panic("canvas: palette cannot exceed 256 colours")
	}
	return &Canvas{
		width:     width,
		height:    height,
		pixels:    make([]byte, width*height),
		cooldown:  cooldown,
		palette:   palette,
		lastPlace: make(map[string]time.Time),
		lastPrune: time.Now(),
	}
}

// Palette returns the colours this canvas accepts.
func (c *Canvas) Palette() []string { return c.palette }

// Width returns the grid width in pixels.
func (c *Canvas) Width() int { return c.width }

// Height returns the grid height in pixels.
func (c *Canvas) Height() int { return c.height }

// Cooldown returns the per-user delay between placements.
func (c *Canvas) Cooldown() time.Duration { return c.cooldown }

// Seq returns the sequence number of the most recent placement.
func (c *Canvas) Seq() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.seq
}

// Snapshot copies the grid. The copy is what gets served to new clients and
// written to the database, so callers may hold it as long as they like.
func (c *Canvas) Snapshot() (pixels []byte, seq int64) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]byte, len(c.pixels))
	copy(out, c.pixels)
	return out, c.seq
}

// Load replaces the grid wholesale. Used at boot when restoring from the
// database; dimensions must match.
func (c *Canvas) Load(pixels []byte, seq int64) error {
	if len(pixels) != c.width*c.height {
		return errors.New("canvas: snapshot size does not match configured dimensions")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	copy(c.pixels, pixels)
	c.seq = seq
	return nil
}

// Apply writes a pixel with no rule checks and without advancing the sequence
// past the supplied value. It exists for replaying history at boot.
func (c *Canvas) Apply(x, y int, colour uint8, seq int64) {
	if x < 0 || y < 0 || x >= c.width || y >= c.height || int(colour) >= len(c.palette) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pixels[y*c.width+x] = colour
	if seq > c.seq {
		c.seq = seq
	}
}

// CooldownRemaining reports how long uid must wait before its next placement.
func (c *Canvas) CooldownRemaining(uid string, now time.Time) time.Duration {
	c.mu.RLock()
	defer c.mu.RUnlock()
	last, ok := c.lastPlace[uid]
	if !ok {
		return 0
	}
	if elapsed := now.Sub(last); elapsed < c.cooldown {
		return c.cooldown - elapsed
	}
	return 0
}

// Place validates and applies a user placement, returning the resulting Pixel.
func (c *Canvas) Place(x, y int, colour uint8, uid string, now time.Time) (Pixel, error) {
	if x < 0 || y < 0 || x >= c.width || y >= c.height {
		return Pixel{}, ErrOutOfBounds
	}
	if int(colour) >= len(c.palette) {
		return Pixel{}, ErrBadColour
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if last, ok := c.lastPlace[uid]; ok {
		if elapsed := now.Sub(last); elapsed < c.cooldown {
			return Pixel{}, ErrCooldown
		}
	}
	idx := y*c.width + x
	if c.pixels[idx] == colour {
		// Not an error worth spending a cooldown on, but the client should know
		// nothing changed.
		return Pixel{}, ErrSameColour
	}

	c.pixels[idx] = colour
	c.seq++
	c.lastPlace[uid] = now
	c.pruneLocked(now)

	return Pixel{Seq: c.seq, X: x, Y: y, Color: colour, UID: uid, At: now}, nil
}

// pruneLocked drops cooldown entries that can no longer block anyone. Called
// with the write lock held.
func (c *Canvas) pruneLocked(now time.Time) {
	if now.Sub(c.lastPrune) < 5*time.Minute || len(c.lastPlace) < 1024 {
		return
	}
	for uid, t := range c.lastPlace {
		if now.Sub(t) > c.cooldown+time.Minute {
			delete(c.lastPlace, uid)
		}
	}
	c.lastPrune = now
}

// Clear resets every pixel to the background and bumps the sequence.
func (c *Canvas) Clear() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.pixels {
		c.pixels[i] = 0
	}
	c.seq++
	return c.seq
}

// Stats summarises the grid for the info panel.
type Stats struct {
	Width       int            `json:"width"`
	Height      int            `json:"height"`
	Painted     int            `json:"painted"`
	Total       int            `json:"total"`
	Seq         int64          `json:"seq"`
	ColorCounts map[string]int `json:"colorCounts"`
}

// Stats computes how much of the canvas is painted and the colour histogram.
func (c *Canvas) Stats() Stats {
	c.mu.RLock()
	defer c.mu.RUnlock()
	counts := make(map[string]int, len(c.palette))
	painted := 0
	var hist [256]int
	for _, p := range c.pixels {
		hist[p]++
		if p != 0 {
			painted++
		}
	}
	for i, n := range hist {
		if n > 0 && i < len(c.palette) {
			counts[c.palette[i]] = n
		}
	}
	return Stats{
		Width:       c.width,
		Height:      c.height,
		Painted:     painted,
		Total:       c.width * c.height,
		Seq:         c.seq,
		ColorCounts: counts,
	}
}
