package loadtest

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestLoadSoak looks for things that only ever grow. There are four candidates
// in this codebase, and they are not equally worrying:
//
//   - canvas.lastPlace, one entry per painter who has placed a pixel in a room
//     with a cooldown. pruneLocked will not run at all until five minutes have
//     passed AND the map has 1024 entries, and it only runs from inside Place -
//     so a room that goes quiet keeps whatever it accumulated.
//   - room.cursors, one entry per painter who has moved a pointer. Entries
//     expire after six seconds, but only inside liveCursors, which
//     broadcastCursors skips entirely when Hub.Count() is zero.
//   - hub.subs, which is bounded by live connections, and hub.pending, which
//     the coalescing loop empties every 50ms - as long as that loop is running.
//   - httpapi.ipLimiter.counts, which is explicitly bounded at 20000.
//
// The distinguishing feature of the first two is that they are keyed by painter
// id, and a client that throws its cookie away gets a fresh signed id on every
// single request. So the question is not "does it grow", it is "what is it
// bounded by, and is that bound reachable".
func TestLoadSoak(t *testing.T) {
	requireLoadtest(t)
	dsn := requireDSN(t)
	cond := measureConditions()

	t.Run("DistinctPainters", func(t *testing.T) { soakDistinctPainters(t, dsn, cond) })
	t.Run("SteadyState", func(t *testing.T) { soakSteadyState(t, dsn, cond) })
}

// soakDistinctPainters pours a stream of one-shot visitors into a room with a
// cooldown, which is what fills canvas.lastPlace, and into the cursor map.
//
// Every painter here is a fresh cookie jar, so the server mints it a new signed
// id - exactly what a script that discards cookies does, and exactly what the
// per-address limiter exists to bound. The limiter is off in this harness, so
// this measures the shape of the growth rather than how fast a real attacker
// could produce it.
func soakDistinctPainters(t *testing.T, dsn string, cond conditions) {
	h := newHarness(t, harnessOpts{dsn: dsn})

	// The same soak twice, differing only in the cooldown.
	//
	// canvas.Place skips the lastPlace bookkeeping entirely when the cooldown is
	// zero - a deliberate choice, documented in the code - so the room with no
	// cooldown is the control. What one retains and the other does not is
	// lastPlace; what both retain is room.cursors and whatever else is keyed by
	// painter id.
	withCooldown := runPainterSoak(t, h, cond, 750)
	withoutCooldown := runPainterSoak(t, h, cond, 0)

	attributable := float64(withCooldown.retainedPer) - float64(withoutCooldown.retainedPer)

	t.Logf("\nSOAK: A STREAM OF ONE-SHOT PAINTERS\nconditions: %s\n"+
		"128x128 rooms, 24 workers each minting a brand new painter identity, moving one\n"+
		"cursor, placing one pixel and disconnecting. The per-address rate limiter is\n"+
		"disabled in this harness, so this is the shape of the growth, not a claim about\n"+
		"how quickly it can be provoked in production.\n\n"+
		"A. room with a 750ms cooldown (canvas.lastPlace is maintained)\n%s\n"+
		"   painters minted %d; retained after everybody left: %s (%.0f bytes each)\n\n"+
		"B. room with no cooldown (canvas.Place skips lastPlace entirely)\n%s\n"+
		"   painters minted %d; retained after everybody left: %s (%.0f bytes each)\n\n"+
		"difference attributable to canvas.lastPlace: %.0f bytes per distinct painter.\n\n"+
		"Neither map is bounded by anything except the number of distinct painter ids the\n"+
		"room has seen, and httpapi.painterID mints a fresh signed id for any request that\n"+
		"arrives without a valid cookie. canvas.pruneLocked cannot run until five minutes\n"+
		"have passed AND the map holds 1024 entries, and it only runs from inside Place -\n"+
		"so a room that fills up and then goes quiet keeps every entry until it is evicted.\n"+
		"room.cursors expires entries after six seconds, but only inside liveCursors, which\n"+
		"broadcastCursors skips whenever Hub.Count() is zero - so an empty room never\n"+
		"expires a cursor either.\n",
		cond, withCooldown.curve, withCooldown.minted, mib(withCooldown.retained),
		withCooldown.retainedPer,
		withoutCooldown.curve, withoutCooldown.minted, mib(withoutCooldown.retained),
		withoutCooldown.retainedPer, attributable)
}

type painterSoak struct {
	curve       *table
	minted      int64
	retained    uint64
	retainedPer float64
}

func runPainterSoak(t *testing.T, h *harness, cond conditions, cooldownMs int) painterSoak {
	t.Helper()
	slug, _ := createRoom(t, h.base, fmt.Sprintf("soak-painters-%d", cooldownMs), 128, 128, cooldownMs)

	const workers = 24
	dur := scaled(30 * time.Second)

	settle()
	start := time.Now()
	base := sampleMem(start)

	curve := newTable("elapsed", "distinct painters", "heap", "heap inuse", "sys", "goroutines")

	var minted atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				// A brand new identity every time round, which is what a client
				// that discards its cookie gets.
				p := newPainter(h.base)
				if _, err := p.bootstrap(slug); err != nil {
					p.close()
					return
				}
				n := minted.Add(1)
				if c, err := wsDial(wsURL(h.hostPort(), slug), p.cookieHeader()); err == nil {
					_ = c.sendCursor(int(n%128), int((n/128)%128), 1+int(n%7))
					_ = c.sendPlace(int(n%128), int((n/128)%128), 1+int(n%7))
					_, _ = c.read(time.Now().Add(200 * time.Millisecond))
					c.close()
				}
				p.close()
			}
		}()
	}

	tick := time.NewTicker(scaled(5 * time.Second))
	defer tick.Stop()
	for time.Since(start) < dur {
		<-tick.C
		settle()
		m := sampleMem(start)
		curve.add(m.At.Round(time.Second).String(), fmt.Sprint(minted.Load()),
			mib(m.HeapAlloc), mib(m.HeapInuse), mib(m.Sys), fmt.Sprint(m.Goroutines))
	}
	close(stop)
	wg.Wait()

	// Everybody has gone and nothing is connected. Whatever is still held after
	// two collections and a wait is held for as long as the room is resident.
	settle()
	time.Sleep(scaled(5 * time.Second))
	settle()
	idle := sampleMem(start)

	retained := uint64(0)
	if idle.HeapAlloc > base.HeapAlloc {
		retained = idle.HeapAlloc - base.HeapAlloc
	}
	per := 0.0
	if n := minted.Load(); n > 0 {
		per = float64(retained) / float64(n)
	}
	curve.add("idle", fmt.Sprint(minted.Load()), mib(idle.HeapAlloc),
		mib(idle.HeapInuse), mib(idle.Sys), fmt.Sprint(idle.Goroutines))

	return painterSoak{curve: curve, minted: minted.Load(), retained: retained, retainedPer: per}
}

// soakSteadyState is the ordinary case: a fixed set of painters, a fixed set of
// watchers, painting for a good while. Nothing here should grow at all, and the
// point of running it is to have a curve that says so.
func soakSteadyState(t *testing.T, dsn string, cond conditions) {
	h := newHarness(t, harnessOpts{dsn: dsn})
	slug, _ := createRoom(t, h.base, "soak steady", 128, 128, 0)

	const watchers = 64
	conns := make([]*wsClient, 0, watchers)
	stop := make(chan struct{})
	var readers sync.WaitGroup
	for i := 0; i < watchers; i++ {
		p := newPainter(h.base)
		if _, err := p.bootstrap(slug); err != nil {
			t.Fatalf("watcher %d bootstrap: %v", i, err)
		}
		c, err := wsDial(wsURL(h.hostPort(), slug), p.cookieHeader())
		if err != nil {
			t.Fatalf("watcher %d dial: %v", i, err)
		}
		conns = append(conns, c)
		readers.Add(1)
		go func(c *wsClient) {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := c.read(time.Now().Add(time.Second)); err != nil && !isTimeout(err) {
					return
				}
			}
		}(c)
		p.close()
	}
	defer func() {
		close(stop)
		for _, c := range conns {
			c.close()
		}
		readers.Wait()
	}()

	curve := newTable("elapsed", "heap", "heap inuse", "sys", "goroutines", "gc runs")
	start := time.Now()
	done := make(chan struct{})
	var sampling sync.WaitGroup
	sampling.Add(1)
	go func() {
		defer sampling.Done()
		tick := time.NewTicker(scaled(6 * time.Second))
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				m := sampleMem(start)
				curve.add(m.At.Round(time.Second).String(), mib(m.HeapAlloc),
					mib(m.HeapInuse), mib(m.Sys), fmt.Sprint(m.Goroutines), fmt.Sprint(m.NumGC))
			}
		}
	}()

	r := runPaced(t, h, pacedOpts{
		slug: slug, clients: 16, targetPS: 5_000, dur: scaled(60 * time.Second), timed: 4,
	})
	close(done)
	sampling.Wait()

	settle()
	final := sampleMem(start)

	t.Logf("\nSOAK: STEADY STATE\nconditions: %s\n"+
		"128x128 room, cooldown 0, %d watchers, 16 sockets offering 5,000 placements/s\n"+
		"for %s. A canvas with no cooldown keeps no per-painter bookkeeping at all, which\n"+
		"is what makes this the flat case.\n%s\n"+
		"after a collection: %s\n"+
		"placements accepted: %d at %.0f/s, history dropped: %d\n"+
		"paint round trip: %s\n",
		cond, watchers, scaled(60*time.Second), curve, final,
		r.accepted, r.acceptedRate(), r.histDrops, r.lat)
}
