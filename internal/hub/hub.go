// Package hub fans placements out to every connected client, over WebSocket or
// Server-Sent Events, without either transport knowing about the other.
//
// Updates are coalesced on a short tick rather than sent per placement. Under a
// paint storm that turns hundreds of tiny frames per second into a handful of
// batched ones, which is the difference between a canvas that stays smooth and
// one that melts a free-tier container.
package hub

import (
	"encoding/binary"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Har103/pixelforge/internal/canvas"
)

// Binary message kinds. The first byte of every binary frame says what follows.
const (
	KindPixelBatch = 0x01
)

// Frame is one outbound message. Binary frames carry pixel batches; text frames
// carry JSON control messages.
type Frame struct {
	Binary bool
	Data   []byte
}

// Subscriber is one connected client. Read from C until it is closed.
type Subscriber struct {
	ID   uint64
	C    chan Frame
	Name string

	// mu makes sending on C and closing it mutually exclusive.
	//
	// broadcast deliberately copies the subscriber set and then releases the
	// hub lock before delivering, so a slow client cannot stall everyone else.
	// That leaves a window in which another goroutine can close this channel
	// between the copy and the send - and a send on a closed channel is not a
	// dropped message, it is a panic that takes the whole process with it.
	// Every broadcast reaches this from somewhere: the coalescing loop flushing
	// pixels and announcing presence, a request goroutine sending a control
	// message, the cursor tick, and shutdown closing everything at once.
	mu     sync.Mutex
	closed bool
}

// send offers one frame without blocking. ok reports whether it was delivered,
// alive whether the client is still connected at all.
func (s *Subscriber) send(f Frame) (ok, alive bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false, false
	}
	select {
	case s.C <- f:
		return true, true
	default:
		return false, true
	}
}

// close shuts the channel exactly once, reporting whether this call was the one
// that did it, so the caller knows whether the bookkeeping is still theirs to do.
func (s *Subscriber) close() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.closed = true
	close(s.C)
	return true
}

// Hub owns the subscriber set and the broadcast loop.
type Hub struct {
	mu     sync.RWMutex
	subs   map[uint64]*Subscriber
	nextID atomic.Uint64

	log *slog.Logger

	pendMu  sync.Mutex
	pending []canvas.Pixel

	tick time.Duration

	// Presence is announced by Run rather than by whoever joined or left, and
	// what is recorded here is only that the set changed - never what it changed
	// to. A client wants the number that is true now, not a replay of every
	// number it passed through on the way, so a burst of arrivals collapses into
	// a single frame carrying the figure at the end of it. See presenceStep.
	//
	// This state deliberately does not live under mu, even though mu is already
	// the lock that guards who is connected. Announcing means broadcasting, and
	// broadcast takes mu to copy the subscriber set: mu is an RWMutex and Go's
	// RWMutex is not reentrant, so an announcement made while holding mu would
	// deadlock the room outright the first time anybody joined. A separate lock
	// makes that mistake impossible to make by accident rather than merely
	// avoided by the order the statements happen to be in today.
	presenceMu    sync.Mutex
	presenceDirty bool
	presenceAt    time.Time
	presenceEvery time.Duration

	// bufferSize is how many frames a slow client may fall behind before it is
	// disconnected. Small on purpose: a client that cannot keep up with the
	// canvas is better off reconnecting and refetching the snapshot.
	bufferSize int

	stats struct {
		broadcast atomic.Uint64
		dropped   atomic.Uint64
	}
}

// New creates a hub. Call Run to start the broadcast loop: it is what turns
// queued placements and subscriber-set changes into frames, so a hub that is
// never run delivers neither pixels nor presence.
func New(log *slog.Logger) *Hub {
	if log == nil {
		log = slog.Default()
	}
	return &Hub{
		subs:          make(map[uint64]*Subscriber),
		log:           log,
		tick:          50 * time.Millisecond,
		presenceEvery: time.Second,
		bufferSize:    32,
	}
}

// Subscribe registers a new client.
func (h *Hub) Subscribe(name string) *Subscriber {
	s := &Subscriber{
		ID:   h.nextID.Add(1),
		C:    make(chan Frame, h.bufferSize),
		Name: name,
	}
	h.mu.Lock()
	h.subs[s.ID] = s
	n := len(h.subs)
	h.mu.Unlock()

	h.log.Debug("client subscribed", "id", s.ID, "transport", name, "total", n)
	h.markPresence()
	return s
}

// Unsubscribe removes a client and closes its channel exactly once.
func (h *Hub) Unsubscribe(s *Subscriber) {
	if s == nil || !s.close() {
		return
	}
	h.mu.Lock()
	delete(h.subs, s.ID)
	n := len(h.subs)
	h.mu.Unlock()

	h.log.Debug("client unsubscribed", "id", s.ID, "transport", s.Name, "total", n)
	h.markPresence()
}

// Count reports how many clients are connected.
func (h *Hub) Count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subs)
}

// Publish queues a placement for the next broadcast tick.
func (h *Hub) Publish(p canvas.Pixel) {
	h.pendMu.Lock()
	h.pending = append(h.pending, p)
	h.pendMu.Unlock()
}

// Run drives the coalescing loop until done is closed. Presence rides the same
// tick as placements, so both are the work of one goroutine on a known schedule
// rather than of whichever request happened to arrive.
func (h *Hub) Run(done <-chan struct{}) {
	t := time.NewTicker(h.tick)
	defer t.Stop()

	for {
		select {
		case <-done:
			h.closeAll()
			return
		case now := <-t.C:
			h.flush()
			// The tick already carries the time it fired, and taking a second
			// reading here would differ from it only by however long the flush
			// took - which is not a quantity the presence throttle should be
			// measuring.
			h.presenceStep(now)
		}
	}
}

func (h *Hub) flush() {
	h.pendMu.Lock()
	if len(h.pending) == 0 {
		h.pendMu.Unlock()
		return
	}
	batch := h.pending
	h.pending = make([]canvas.Pixel, 0, len(batch))
	h.pendMu.Unlock()

	// Binary form for WebSocket clients: 3-byte header then 5 bytes per pixel.
	bin := make([]byte, 3, 3+len(batch)*5)
	bin[0] = KindPixelBatch
	binary.BigEndian.PutUint16(bin[1:3], uint16(min(len(batch), 0xFFFF)))
	for _, p := range batch {
		bin = binary.BigEndian.AppendUint16(bin, uint16(p.X))
		bin = binary.BigEndian.AppendUint16(bin, uint16(p.Y))
		bin = append(bin, p.Color)
	}

	// JSON form for SSE clients, which cannot carry binary.
	type wire struct {
		T string         `json:"t"`
		P []canvas.Pixel `json:"p"`
	}
	txt, err := json.Marshal(wire{T: "px", P: batch})
	if err != nil {
		h.log.Error("encoding pixel batch", "err", err)
		return
	}

	h.broadcast(Frame{Binary: true, Data: bin}, Frame{Binary: false, Data: txt})
}

// broadcast delivers the binary frame to WebSocket subscribers and the text
// frame to everyone else. A subscriber whose buffer is full is disconnected
// rather than allowed to stall the loop.
func (h *Hub) broadcast(bin, txt Frame) {
	h.mu.RLock()
	targets := make([]*Subscriber, 0, len(h.subs))
	for _, s := range h.subs {
		targets = append(targets, s)
	}
	h.mu.RUnlock()

	for _, s := range targets {
		f := txt
		if s.Name == "ws" {
			f = bin
		}
		switch ok, alive := s.send(f); {
		case ok:
			h.stats.broadcast.Add(1)
		case alive:
			h.stats.dropped.Add(1)
			h.log.Warn("client too slow, disconnecting", "id", s.ID, "transport", s.Name)
			go h.Unsubscribe(s)
		}
		// A subscriber that is no longer alive was closed between the copy above
		// and this send. Nothing to do: whoever closed it has already removed it.
	}
}

// BroadcastJSON sends an arbitrary control message to every client, as text on
// both transports.
func (h *Hub) BroadcastJSON(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		h.log.Error("encoding control message", "err", err)
		return
	}
	f := Frame{Binary: false, Data: data}
	h.broadcast(f, f)
}

// markPresence records that somebody joined or left, for Run to act on.
//
// Announcing from here instead - which is what Subscribe and Unsubscribe used to
// do - makes one arrival cost a frame to every client already in the room, so N
// people filling a room cost N^2/2 frames between them. At a few hundred that is
// no longer a cost, it is an outage: the frames arrive faster than a client can
// be scheduled to read them, its 32-frame buffer fills, and broadcast drops it as
// too slow. People were being disconnected from the canvas by other people
// arriving, and every one of those disconnections was itself another presence
// change, which broadcast another N frames.
func (h *Hub) markPresence() {
	h.presenceMu.Lock()
	h.presenceDirty = true
	h.presenceMu.Unlock()
}

// presenceStep announces the headcount if the subscriber set has changed since
// the last announcement and that announcement is not too recent, reporting
// whether it sent anything.
//
// The throttle is leading-edge with a trailing announcement: a change to a room
// that has been quiet goes out on the very next tick, a burst costs one frame per
// presenceEvery however many people it involves, and the dirty flag survives the
// wait so the figure the room settles on is always delivered. A count that is a
// second stale is invisible to somebody watching a number tick over; a count that
// is permanently wrong is a bug report.
//
// The set is counted here rather than at the moment it changed, because after a
// burst of joins and leaves the only number worth sending is the one that is true
// now.
func (h *Hub) presenceStep(now time.Time) bool {
	h.presenceMu.Lock()
	if !h.presenceDirty || now.Sub(h.presenceAt) < h.presenceEvery {
		h.presenceMu.Unlock()
		return false
	}
	h.presenceDirty = false
	h.presenceAt = now
	h.presenceMu.Unlock()

	h.BroadcastJSON(map[string]any{"t": "presence", "n": h.Count()})
	return true
}

func (h *Hub) closeAll() {
	h.mu.Lock()
	subs := make([]*Subscriber, 0, len(h.subs))
	for _, s := range h.subs {
		subs = append(subs, s)
	}
	h.subs = make(map[uint64]*Subscriber)
	h.mu.Unlock()

	for _, s := range subs {
		s.close()
	}
}

// Stats reports cumulative counters for the health endpoint.
func (h *Hub) Stats() (clients int, delivered, dropped uint64) {
	return h.Count(), h.stats.broadcast.Load(), h.stats.dropped.Load()
}
