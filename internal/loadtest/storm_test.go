package loadtest

import (
	"errors"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The WebSocket paint path takes no acknowledgement, so a client can write
// faster than the server reads and the excess simply sits in socket buffers.
// That makes an unbounded loop a poor instrument: it reports a placement rate
// that includes bytes the server has not looked at yet, and a latency that is
// mostly queueing in the kernel.
//
// pacedRun fixes the offered rate instead and measures what comes back, which
// is the shape that can answer "at what rate does this start to degrade". An
// unbounded run is still worth doing - it is what a runaway script looks like -
// and pacedOpts.targetPS = 0 asks for one.

type pacedOpts struct {
	slug string
	// clients is how many sockets are open and painting.
	clients int
	// targetPS is the aggregate offered rate in placements per second. Zero
	// means "as fast as the sockets accept", which is a saturation test.
	targetPS int
	dur      time.Duration
	// timed is how many of the clients measure the round trip from sending a
	// placement to seeing it echoed in a broadcast. Kept small and constant so
	// the measurement costs the same at every point on a ramp.
	timed int
}

type pacedResult struct {
	offered   int   // placements the generator wrote to a socket
	accepted  int64 // placements the room's canvas actually applied (its seq delta)
	elapsed   time.Duration
	cpu       time.Duration
	lat       latencyStats
	histDrops int
	slowDrops int
	denied    int
	// behind counts how often a sender fell behind its own schedule, which is
	// how this run declares "the generator could not offer the rate asked for"
	// rather than blaming the server for it.
	behind int
}

func (r pacedResult) acceptedRate() float64 { return rate(int(r.accepted), r.elapsed) }

// runPaced opens the sockets, paints for the duration, and closes everything
// before reading the final counters, so one step of a ramp cannot bleed into
// the next.
func runPaced(t *testing.T, h *harness, o pacedOpts) pacedResult {
	t.Helper()

	type client struct {
		p    *painter
		ws   *wsClient
		out  *outstanding
		stop chan struct{}
	}
	clients := make([]client, 0, o.clients)
	var readers sync.WaitGroup

	lat := newLatencies(16384)
	var offered, denied, behind atomic.Int64

	for i := 0; i < o.clients; i++ {
		p := newPainter(h.base)
		if _, err := p.bootstrap(o.slug); err != nil {
			t.Fatalf("painter %d bootstrap: %v", i, err)
		}
		ws, err := wsDial(wsURL(h.hostPort(), o.slug), p.cookieHeader())
		if err != nil {
			t.Fatalf("painter %d dial: %v", i, err)
		}
		c := client{p: p, ws: ws, out: newOutstanding(i < o.timed), stop: make(chan struct{})}
		clients = append(clients, c)

		// Drain from the instant the socket exists, not when painting starts.
		//
		// Every Subscribe broadcasts a presence update to everyone already in
		// the room, so opening N sockets costs N^2/2 frames. A client that is
		// not reading yet fills its 32-frame buffer during that storm and the
		// hub disconnects it as slow - which would be the generator producing
		// the failure, before the experiment has even begun.
		readers.Add(1)
		go func(c client) {
			defer readers.Done()
			drain(c.ws, c.stop, c.out, lat, &denied)
		}(c)
	}

	cfg, err := clients[0].p.bootstrap(o.slug)
	if err != nil {
		t.Fatalf("reading room config: %v", err)
	}
	w, hgt, colours := cfg.Room.Width, cfg.Room.Height, len(cfg.Room.Palette)
	seqBefore := cfg.Room.Seq

	dropsBefore := h.logs.count(msgBufferFull)
	slowBefore := h.logs.count(msgSlowClient)

	// interval spreads the aggregate target evenly across the sockets. At very
	// high targets it falls below what a sleep can resolve, and the "behind"
	// counter is how a run admits that the generator, not the server, set the
	// rate.
	var interval time.Duration
	if o.targetPS > 0 {
		interval = time.Duration(float64(time.Second) / (float64(o.targetPS) / float64(o.clients)))
	}

	cpu0 := cpuTime()
	start := time.Now()
	deadline := start.Add(o.dur)

	var wg sync.WaitGroup
	for i, c := range clients {
		wg.Add(1)
		go func(id int, c client) {
			defer wg.Done()
			plan := newPaintPlan(w, hgt, o.clients, id, colours)
			n := int64(0)
			for time.Now().Before(deadline) {
				if interval > 0 {
					// Schedule against a fixed origin rather than sleeping a
					// fixed amount each time: sleeping "interval" per iteration
					// accumulates every scheduling delay and silently offers a
					// lower rate than the one on the label.
					due := start.Add(time.Duration(n) * interval)
					wait := time.Until(due)
					switch {
					case wait > 0:
						time.Sleep(wait)
					case wait < -50*time.Millisecond:
						behind.Add(1)
					}
				}
				x, y, colour := plan.next()
				c.out.mark(x, y, byte(colour))
				if err := c.ws.sendPlace(x, y, colour); err != nil {
					break
				}
				offered.Add(1)
				n++
			}
		}(i, c)
	}
	wg.Wait()
	elapsed := time.Since(start)
	cpu := cpuTime() - cpu0

	// Let the last placements be echoed before the readers stop, or the tail of
	// every run looks like a loss.
	time.Sleep(600 * time.Millisecond)
	for _, c := range clients {
		close(c.stop)
	}

	after, err := clients[0].p.bootstrap(o.slug)
	if err != nil {
		t.Fatalf("reading room config after the run: %v", err)
	}

	// Close every socket only after the final read, so the room is quiet but
	// still resident and the seq it reports is this run's.
	for _, c := range clients {
		c.ws.close()
	}
	readers.Wait()
	for _, c := range clients {
		c.p.close()
	}
	time.Sleep(200 * time.Millisecond)

	return pacedResult{
		offered:   int(offered.Load()),
		accepted:  after.Room.Seq - seqBefore,
		elapsed:   elapsed,
		cpu:       cpu,
		lat:       lat.stats(),
		histDrops: h.logs.count(msgBufferFull) - dropsBefore,
		slowDrops: h.logs.count(msgSlowClient) - slowBefore,
		denied:    int(denied.Load()),
		behind:    int(behind.Load()),
	}
}

// drain reads a socket continuously. Every client must do this whether or not
// it is timing anything: the hub gives each subscriber a 32-frame buffer and
// disconnects anyone who fills it, so a client that stops reading manufactures
// the very "slow client" failure this suite is trying to observe.
func drain(c *wsClient, stop <-chan struct{}, out *outstanding, lat *latencies, denied *atomic.Int64) {
	local := make([]time.Duration, 0, 4096)
	defer func() { lat.addLocal(local) }()

	for {
		select {
		case <-stop:
			return
		default:
		}
		msg, err := c.read(time.Now().Add(2 * time.Second))
		if err != nil {
			// A read deadline in a quiet room is not an error; anything else
			// means the socket is gone.
			select {
			case <-stop:
				return
			default:
			}
			if isTimeout(err) {
				continue
			}
			return
		}
		if !out.timing {
			if !msg.binary && isDenial(msg.payload) {
				denied.Add(1)
			}
			continue
		}
		if !msg.binary {
			if isDenial(msg.payload) {
				denied.Add(1)
			}
			continue
		}
		batch, ok := pixelBatch(msg.payload)
		if !ok {
			continue
		}
		now := time.Now()
		for _, p := range batch {
			if sent, ok := out.claim(p.X, p.Y, p.C); ok {
				local = append(local, now.Sub(sent))
			}
		}
	}
}

// isTimeout distinguishes "nothing was broadcast for two seconds", which is
// normal in a quiet room, from "this socket is gone".
func isTimeout(err error) bool {
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// outstanding tracks placements a timed client is waiting to see come back.
//
// Keying on the cell and the colour rather than on a sequence number is forced
// by the wire format: a pixel batch carries x, y and colour and nothing else,
// so there is no id to match on. It is unambiguous here only because each
// painter owns a disjoint run of cells and changes colour once per pass, which
// is the other reason paintPlan is shaped the way it is.
type outstanding struct {
	timing bool
	mu     sync.Mutex
	at     map[uint32]time.Time
}

func newOutstanding(timing bool) *outstanding {
	return &outstanding{timing: timing, at: map[uint32]time.Time{}}
}

func key(x, y int, c byte) uint32 {
	return uint32(x&0xFFF)<<20 | uint32(y&0xFFF)<<8 | uint32(c)
}

func (o *outstanding) mark(x, y int, c byte) {
	if !o.timing {
		return
	}
	o.mu.Lock()
	// Bound the map: if echoes stop coming back, this must not become the
	// experiment's memory leak.
	if len(o.at) > 4096 {
		o.at = map[uint32]time.Time{}
	}
	o.at[key(x, y, c)] = time.Now()
	o.mu.Unlock()
}

func (o *outstanding) claim(x, y int, c byte) (time.Time, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	k := key(x, y, c)
	at, ok := o.at[k]
	if ok {
		delete(o.at, k)
	}
	return at, ok
}
