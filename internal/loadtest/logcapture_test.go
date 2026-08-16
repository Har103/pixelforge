package loadtest

import (
	"context"
	"log/slog"
	"strings"
	"sync"
)

// logCapture is the server's logger. It exists because the two failures this
// suite most wants to catch are only ever reported as log lines:
//
//   - room.record dropping a history entry when the write-behind buffer is
//     full ("placement buffer full, dropping history entry", with the seq)
//   - hub.broadcast disconnecting a client that cannot keep up
//     ("client too slow, disconnecting")
//
// Counting them from outside the room and hub packages means the experiments
// can assert on real product behaviour without editing a single line of it.
type logCapture struct {
	mu      sync.Mutex
	counts  map[string]int
	dropped []int64 // seq numbers room.record threw away, in order
	errors  []string
	// keepErrors bounds the error tail: a database outage produces one line per
	// failed batch and there is no reason to hold megabytes of them.
	keepErrors int
}

// maxDroppedKept bounds the sequence numbers retained for inspection. The
// count is always exact; only the list of which ones is truncated.
const maxDroppedKept = 4096

func newLogCapture() *logCapture {
	return &logCapture{counts: map[string]int{}, keepErrors: 200}
}

func (c *logCapture) Enabled(_ context.Context, l slog.Level) bool {
	// Debug is where hub logs every subscribe and unsubscribe; at a thousand
	// connections a second that is the load test measuring its own logger.
	return l >= slog.LevelInfo
}

func (c *logCapture) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counts[r.Message]++

	// Keep the dropped sequence numbers, but bounded: a saturated room drops
	// tens of thousands a second, and a memory experiment must not be measuring
	// its own instrumentation.
	if r.Message == msgBufferFull && len(c.dropped) < maxDroppedKept {
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "seq" {
				c.dropped = append(c.dropped, a.Value.Int64())
				return false
			}
			return true
		})
	}
	if r.Level >= slog.LevelError && len(c.errors) < c.keepErrors {
		var sb strings.Builder
		sb.WriteString(r.Message)
		r.Attrs(func(a slog.Attr) bool {
			sb.WriteString(" ")
			sb.WriteString(a.Key)
			sb.WriteString("=")
			sb.WriteString(a.Value.String())
			return true
		})
		c.errors = append(c.errors, sb.String())
	}
	return nil
}

func (c *logCapture) WithAttrs(attrs []slog.Attr) slog.Handler {
	// Share the counters: room.materialise gives every hub a logger with the
	// room name attached, and those lines must still be counted centrally.
	return &derivedCapture{parent: c, attrs: attrs}
}

func (c *logCapture) WithGroup(string) slog.Handler { return c }

// derivedCapture is what WithAttrs returns: a thin shell that funnels every
// record back into the one set of counters.
type derivedCapture struct {
	parent *logCapture
	attrs  []slog.Attr
}

func (d *derivedCapture) Enabled(ctx context.Context, l slog.Level) bool {
	return d.parent.Enabled(ctx, l)
}

func (d *derivedCapture) Handle(ctx context.Context, r slog.Record) error {
	return d.parent.Handle(ctx, r)
}

func (d *derivedCapture) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &derivedCapture{parent: d.parent, attrs: append(append([]slog.Attr{}, d.attrs...), attrs...)}
}

func (d *derivedCapture) WithGroup(string) slog.Handler { return d }

// The exact log messages this suite keys on. If any of them is reworded in the
// product, the assertions here go quiet rather than wrong - so they are named
// once, here, where the mismatch is obvious.
const (
	msgBufferFull   = "placement buffer full, dropping history entry" // room.record
	msgSlowClient   = "client too slow, disconnecting"                // hub.broadcast
	msgWritePlace   = "writing placements"                            // room.writeBehind, on failure
	msgEvicting     = "evicting room to stay under the resident cap"  // registry.evictIfCrowded
	msgRoomResident = "room resident"                                 // registry.materialise
)

func (c *logCapture) count(msg string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[msg]
}

func (c *logCapture) errorTail(n int) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.errors) < n {
		n = len(c.errors)
	}
	out := make([]string, n)
	copy(out, c.errors[len(c.errors)-n:])
	return out
}

// summary lists every message the server logged and how often, which is how an
// experiment reports "what else was happening" without guessing.
func (c *logCapture) summary() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int, len(c.counts))
	for k, v := range c.counts {
		out[k] = v
	}
	return out
}
