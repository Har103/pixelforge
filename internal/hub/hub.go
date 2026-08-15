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

	dropped atomic.Bool
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

	lastPresence int
	presenceAt   time.Time

	// bufferSize is how many frames a slow client may fall behind before it is
	// disconnected. Small on purpose: a client that cannot keep up with the
	// canvas is better off reconnecting and refetching the snapshot.
	bufferSize int

	stats struct {
		broadcast atomic.Uint64
		dropped   atomic.Uint64
	}
}

// New creates a hub. Call Run to start the broadcast loop.
func New(log *slog.Logger) *Hub {
	if log == nil {
		log = slog.Default()
	}
	return &Hub{
		subs:       make(map[uint64]*Subscriber),
		log:        log,
		tick:       50 * time.Millisecond,
		bufferSize: 32,
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
	h.announcePresence(true)
	return s
}

// Unsubscribe removes a client and closes its channel exactly once.
func (h *Hub) Unsubscribe(s *Subscriber) {
	if s == nil || !s.dropped.CompareAndSwap(false, true) {
		return
	}
	h.mu.Lock()
	delete(h.subs, s.ID)
	n := len(h.subs)
	h.mu.Unlock()
	close(s.C)

	h.log.Debug("client unsubscribed", "id", s.ID, "transport", s.Name, "total", n)
	h.announcePresence(true)
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

// Run drives the coalescing loop until done is closed.
func (h *Hub) Run(done <-chan struct{}) {
	t := time.NewTicker(h.tick)
	defer t.Stop()
	presence := time.NewTicker(5 * time.Second)
	defer presence.Stop()

	for {
		select {
		case <-done:
			h.closeAll()
			return
		case <-t.C:
			h.flush()
		case <-presence.C:
			h.announcePresence(false)
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
		select {
		case s.C <- f:
			h.stats.broadcast.Add(1)
		default:
			h.stats.dropped.Add(1)
			h.log.Warn("client too slow, disconnecting", "id", s.ID, "transport", s.Name)
			go h.Unsubscribe(s)
		}
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

// announcePresence tells clients how many people are connected. force sends even
// if the count has not changed, but never more than once a second.
func (h *Hub) announcePresence(force bool) {
	n := h.Count()

	h.mu.Lock()
	changed := n != h.lastPresence
	tooSoon := time.Since(h.presenceAt) < time.Second
	if !changed && !force {
		h.mu.Unlock()
		return
	}
	if tooSoon && !changed {
		h.mu.Unlock()
		return
	}
	h.lastPresence = n
	h.presenceAt = time.Now()
	h.mu.Unlock()

	h.BroadcastJSON(map[string]any{"t": "presence", "n": n})
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
		if s.dropped.CompareAndSwap(false, true) {
			close(s.C)
		}
	}
}

// Stats reports cumulative counters for the health endpoint.
func (h *Hub) Stats() (clients int, delivered, dropped uint64) {
	return h.Count(), h.stats.broadcast.Load(), h.stats.dropped.Load()
}
