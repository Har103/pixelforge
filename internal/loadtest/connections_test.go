package loadtest

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestLoadManyConnections opens hundreds of sockets on one room while
// placements fly, and asks four things:
//
//   - what a connection costs in memory and goroutines
//   - whether they all come back when everybody disconnects
//   - whether the hub really does disconnect a client that stops reading
//   - whether that slow client degrades the fast ones while it is being tolerated
func TestLoadManyConnections(t *testing.T) {
	requireLoadtest(t)
	dsn := requireDSN(t)
	cond := measureConditions()

	t.Run("Scaling", func(t *testing.T) { connectionsScaling(t, dsn, cond) })
	t.Run("SlowClient", func(t *testing.T) { connectionsSlowClient(t, dsn, cond) })
}

// connectionsScaling ramps the number of watchers on one busy room.
func connectionsScaling(t *testing.T, dsn string, cond conditions) {
	h := newHarness(t, harnessOpts{dsn: dsn})

	results := newTable("watchers", "connect time", "goroutines", "heap", "per-client",
		"paint rate/s", "frames seen", "slow-drops", "goroutines after", "leaked")

	for _, watchers := range []int{50, 200, 500} {
		slug, _ := createRoom(t, h.base, fmt.Sprintf("conn-%d", watchers), 128, 128, 0)

		settle()
		gorBefore := runtime.NumGoroutine()
		memBefore := sampleMem(time.Now())

		// Connect. Every Subscribe broadcasts a presence update to everyone
		// already in the room, so this phase is quadratic in the number of
		// watchers - which is itself worth timing.
		t0 := time.Now()
		conns := make([]*wsClient, 0, watchers)
		painters := make([]*painter, 0, watchers)
		var frames atomic.Int64
		stop := make(chan struct{})
		var readers sync.WaitGroup

		for i := 0; i < watchers; i++ {
			p := newPainter(h.base)
			if _, err := p.bootstrap(slug); err != nil {
				t.Fatalf("watcher %d bootstrap: %v", i, err)
			}
			c, err := wsDial(wsURL(h.hostPort(), slug), p.cookieHeader())
			if err != nil {
				t.Fatalf("watcher %d dial (%d already open): %v", i, i, err)
			}
			conns = append(conns, c)
			painters = append(painters, p)

			readers.Add(1)
			go func(c *wsClient) {
				defer readers.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					if _, err := c.read(time.Now().Add(2 * time.Second)); err != nil {
						if isTimeout(err) {
							continue
						}
						return
					}
					frames.Add(1)
				}
			}(c)
		}
		connectTime := time.Since(t0)

		settle()
		gorConnected := runtime.NumGoroutine()
		memConnected := sampleMem(time.Now())
		perClient := float64(memConnected.HeapAlloc-memBefore.HeapAlloc) / float64(watchers)

		// Paint into the room from a separate, modest set of painters, so the
		// fan-out is what is being loaded rather than the paint path.
		slowBefore := h.logs.count(msgSlowClient)
		r := runPaced(t, h, pacedOpts{
			slug: slug, clients: 8, targetPS: 2_000, dur: scaled(6 * time.Second), timed: 2,
		})

		// Disconnect everybody and see whether the server lets go.
		close(stop)
		for _, c := range conns {
			c.close()
		}
		readers.Wait()
		for _, p := range painters {
			p.close()
		}

		gorAfter := waitForGoroutines(gorBefore+8, 30*time.Second)
		leaked := gorAfter - gorBefore

		results.add(fmt.Sprint(watchers), connectTime.Round(time.Millisecond).String(),
			fmt.Sprintf("%d (+%d)", gorConnected, gorConnected-gorBefore),
			mib(memConnected.HeapAlloc), fmt.Sprintf("%.1fKiB", perClient/1024),
			fmt.Sprintf("%.0f", r.acceptedRate()), fmt.Sprint(frames.Load()),
			fmt.Sprint(h.logs.count(msgSlowClient)-slowBefore),
			fmt.Sprint(gorAfter), fmt.Sprint(leaked))

		if leaked > 8 {
			t.Errorf("GOROUTINE LEAK: %d goroutines before %d watchers connected, %d after "+
				"they all disconnected (+%d)", gorBefore, watchers, gorAfter, leaked)
		}
	}

	t.Logf("\nMANY CONNECTIONS: SCALING\nconditions: %s\n"+
		"one 128x128 room per step, cooldown 0. Watchers only read; a separate set of\n"+
		"8 sockets offers 2,000 placements/s throughout. \"per-client\" is heap growth\n"+
		"divided by watchers and includes both ends of every connection, because the\n"+
		"generator shares this process - treat it as an upper bound on the server's share.\n"+
		"\"leaked\" is goroutines still running 30s after every client disconnected.\n%s",
		cond, results)
	t.Logf("server log lines: %v", h.logs.summary())
}

// connectionsSlowClient checks the hub's promise: a subscriber whose 32-frame
// buffer fills is disconnected rather than allowed to stall the loop. The
// interesting half is whether the fast clients notice while that is happening.
func connectionsSlowClient(t *testing.T, dsn string, cond conditions) {
	h := newHarness(t, harnessOpts{dsn: dsn})

	// A big room and a high rate on purpose. The hub coalesces to twenty frames
	// a second, so the only way to push volume at a stuck client is to make each
	// frame large - and volume is what this needs, because the subscriber's
	// 32-frame channel is the last thing to fill, not the first. Ahead of it sit
	// the server's socket send buffer and the client's receive buffer, which on
	// Linux together run to megabytes.
	slug, _ := createRoom(t, h.base, "slow client", 512, 512, 0)

	const fast = 32
	const slow = 8
	const target = 40_000
	dur := scaled(30 * time.Second)

	// Fast watchers read continuously and time how long each broadcast takes to
	// arrive after the one before it. A hub that stalls on a slow client shows
	// up as a gap here.
	fastConns := make([]*wsClient, 0, fast)
	gaps := newLatencies(8192)
	stop := make(chan struct{})
	var readers sync.WaitGroup

	for i := 0; i < fast; i++ {
		p := newPainter(h.base)
		if _, err := p.bootstrap(slug); err != nil {
			t.Fatalf("fast watcher %d bootstrap: %v", i, err)
		}
		c, err := wsDial(wsURL(h.hostPort(), slug), p.cookieHeader())
		if err != nil {
			t.Fatalf("fast watcher %d dial: %v", i, err)
		}
		fastConns = append(fastConns, c)
		readers.Add(1)
		go func(c *wsClient) {
			defer readers.Done()
			local := make([]time.Duration, 0, 2048)
			last := time.Time{}
			for {
				select {
				case <-stop:
					gaps.addLocal(local)
					return
				default:
				}
				msg, err := c.read(time.Now().Add(2 * time.Second))
				if err != nil {
					if isTimeout(err) {
						last = time.Time{}
						continue
					}
					gaps.addLocal(local)
					return
				}
				if !msg.binary {
					continue
				}
				now := time.Now()
				if !last.IsZero() {
					local = append(local, now.Sub(last))
				}
				last = now
			}
		}(c)
	}

	// Slow clients: connected, subscribed, never reading a byte, and with their
	// receive window closed down so the server actually feels it. This is a
	// laptop that went to sleep, or a tab the browser froze.
	slowConns := make([]*wsClient, 0, slow)
	for i := 0; i < slow; i++ {
		p := newPainter(h.base)
		if _, err := p.bootstrap(slug); err != nil {
			t.Fatalf("slow watcher %d bootstrap: %v", i, err)
		}
		c, err := wsDial(wsURL(h.hostPort(), slug), p.cookieHeader())
		if err != nil {
			t.Fatalf("slow watcher %d dial: %v", i, err)
		}
		if err := c.shrinkReceiveBuffer(4096); err != nil {
			t.Fatalf("shrinking slow watcher %d receive buffer: %v", i, err)
		}
		slowConns = append(slowConns, c)
	}
	defer func() {
		for _, c := range slowConns {
			c.close()
		}
	}()

	clientsBefore := 0
	if rm, ok := h.registry.Lookup(slug); ok {
		clientsBefore = rm.Hub.Count()
	}
	slowBefore := h.logs.count(msgSlowClient)

	r := runPaced(t, h, pacedOpts{
		slug: slug, clients: 8, targetPS: target, dur: dur, timed: 4,
	})

	dropped := h.logs.count(msgSlowClient) - slowBefore
	clientsAfter := 0
	if rm, ok := h.registry.Lookup(slug); ok {
		clientsAfter = rm.Hub.Count()
	}

	close(stop)
	for _, c := range fastConns {
		c.close()
	}
	readers.Wait()

	gapStats := gaps.stats()

	// Every pixel is five bytes on the wire plus a three byte batch header, so
	// the volume pushed at each subscriber follows directly from what was
	// accepted. It is the number that says how much the server had to hold for
	// a client that was not reading before it gave up on it.
	perClientBytes := float64(r.accepted)*5 + float64(r.lat.N)*3

	t.Logf("\nMANY CONNECTIONS: A SLOW CLIENT\nconditions: %s\n"+
		"512x512 room, %d watchers reading continuously, %d watchers never reading a byte\n"+
		"and with a 4 KiB receive buffer, 8 sockets offering %d placements/s for %s.\n"+
		"  hub subscribers before / after ... %d / %d\n"+
		"  \"client too slow\" disconnections .. %d\n"+
		"  broadcast volume per subscriber ... %.1f MiB\n"+
		"  fast clients' inter-frame gap ..... %s\n"+
		"  paint round trip for the painters . %s\n"+
		"  achieved placement rate ........... %.0f/s\n",
		cond, fast, slow, target, dur,
		clientsBefore, clientsAfter, dropped, perClientBytes/(1<<20),
		gapStats, r.lat, r.acceptedRate())

	if dropped < slow {
		t.Errorf("SLOW-CLIENT PROTECTION IS LATE: the hub disconnected %d of %d subscribers "+
			"that never read a byte, after %.1f MiB had been broadcast to each. The "+
			"32-frame channel in hub.Subscriber is not the first thing to fill: the "+
			"server's socket send buffer and the client's receive buffer absorb megabytes "+
			"first, so the protection engages only once the kernel is already holding that "+
			"much per stuck client.", dropped, slow, perClientBytes/(1<<20))
	}
	// The hub's coalescing tick is 50ms, so a fast client should see frames at
	// roughly that cadence whenever the room is busy. A tail far past it would
	// mean a stuck subscriber held the broadcast loop up.
	if gapStats.N > 0 && gapStats.P99 > 500*time.Millisecond {
		t.Errorf("SLOW CLIENT HURT FAST ONES: p99 inter-frame gap for readers that were "+
			"keeping up was %s, against a 50ms broadcast tick", gapStats.P99)
	}
}
