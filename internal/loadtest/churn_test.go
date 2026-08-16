package loadtest

import (
	"fmt"
	"math/rand"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestLoadChurn is the shape that produced the "send on closed channel" panic
// this codebase already fixed: clients arriving and leaving continuously while
// placements fly, so that the window between hub.broadcast copying the
// subscriber set and delivering into it is being crossed constantly.
//
// Run it under -race. Half the disconnections are aborts rather than polite
// closes, because a browser tab being killed does not send a close frame and
// the server's read loop finds out from a reset rather than from a protocol
// message.
//
//	PIXELFORGE_LOADTEST=1 PIXELFORGE_TEST_DSN=... \
//	  go test ./internal/loadtest/ -run TestLoadChurn -race -v
func TestLoadChurn(t *testing.T) {
	requireLoadtest(t)
	dsn := requireDSN(t)
	cond := measureConditions()

	h := newHarness(t, harnessOpts{dsn: dsn})
	slug, _ := createRoom(t, h.base, "churn", 128, 128, 0)

	settle()
	gorBefore := runtime.NumGoroutine()
	memBefore := sampleMem(time.Now())

	// Steady painters, so there is always something to fan out while the
	// subscriber set is being rewritten underneath the broadcast loop.
	paintStop := make(chan struct{})
	var painting sync.WaitGroup
	const painters = 8
	// Held so they can be closed before the goroutine count is read: each
	// painter keeps an idle HTTP connection alive, and an idle connection is
	// two goroutines client side and one server side. Leaving eight of those
	// open reads as a twenty-four goroutine leak in the server, which is the
	// kind of mistake that turns a load test into a false alarm.
	steady := make([]*painter, 0, painters)
	for i := 0; i < painters; i++ {
		p := newPainter(h.base)
		if _, err := p.bootstrap(slug); err != nil {
			t.Fatalf("painter %d bootstrap: %v", i, err)
		}
		steady = append(steady, p)
		c, err := wsDial(wsURL(h.hostPort(), slug), p.cookieHeader())
		if err != nil {
			t.Fatalf("painter %d dial: %v", i, err)
		}
		painting.Add(2)
		go func(c *wsClient) {
			defer painting.Done()
			for {
				select {
				case <-paintStop:
					return
				default:
				}
				if _, err := c.read(time.Now().Add(time.Second)); err != nil && !isTimeout(err) {
					return
				}
			}
		}(c)
		go func(id int, p *painter, c *wsClient) {
			defer painting.Done()
			defer c.close()
			plan := newPaintPlan(128, 128, painters, id, 8)
			for {
				select {
				case <-paintStop:
					return
				default:
				}
				x, y, colour := plan.next()
				if err := c.sendPlace(x, y, colour); err != nil {
					return
				}
				time.Sleep(2 * time.Millisecond)
			}
		}(i, p, c)
	}

	// Churn: connect, live briefly, leave. Half abort, half close politely.
	const churners = 48
	churnStop := make(chan struct{})
	var churning sync.WaitGroup
	var cycles, dialFails, aborted atomic.Int64

	deadline := time.Now().Add(scaled(20 * time.Second))
	for i := 0; i < churners; i++ {
		churning.Add(1)
		go func(id int) {
			defer churning.Done()
			rng := rand.New(rand.NewSource(int64(id)*7919 + 13))
			p := newPainter(h.base)
			defer p.close()
			if _, err := p.bootstrap(slug); err != nil {
				return
			}
			cookie := p.cookieHeader()

			for time.Now().Before(deadline) {
				select {
				case <-churnStop:
					return
				default:
				}
				c, err := wsDial(wsURL(h.hostPort(), slug), cookie)
				if err != nil {
					dialFails.Add(1)
					time.Sleep(20 * time.Millisecond)
					continue
				}
				// Read for a short, random while, then vanish. The randomness
				// matters: identical lifetimes would synchronise every
				// disconnection and stop exercising the window.
				live := time.Duration(5+rng.Intn(60)) * time.Millisecond
				readUntil := time.Now().Add(live)
				for time.Now().Before(readUntil) {
					if _, err := c.read(readUntil); err != nil {
						break
					}
				}
				// Send a cursor and a placement on the way out, so a message
				// can be in flight through the handler at the moment the socket
				// dies.
				_ = c.sendCursor(rng.Intn(128), rng.Intn(128), 1+rng.Intn(7))
				_ = c.sendPlace(rng.Intn(128), rng.Intn(128), 1+rng.Intn(7))

				if id%2 == 0 {
					c.abort()
					aborted.Add(1)
				} else {
					c.close()
				}
				cycles.Add(1)
			}
		}(i)
	}

	churning.Wait()
	close(churnStop)
	close(paintStop)
	painting.Wait()
	for _, p := range steady {
		p.close()
	}

	// Everything is gone; the server should let go of all of it.
	gorAfter := waitForGoroutines(gorBefore+8, 45*time.Second)
	memAfter := sampleMem(time.Now())
	health := h.health()

	t.Logf("\nCHURN\nconditions: %s\n"+
		"128x128 room, cooldown 0, %d steady painters over WebSocket, %d clients\n"+
		"connecting and disconnecting continuously for %s (half by abort, half by close)\n"+
		"  connect/disconnect cycles ..... %d (%.0f/s)\n"+
		"  aborted without a close frame . %d\n"+
		"  handshakes that failed ........ %d\n"+
		"  placements accepted ........... %d\n"+
		"  refusals ...................... %d\n"+
		"  goroutines before / after ..... %d / %d (leaked %d)\n"+
		"  heap before / after ........... %s / %s\n"+
		"  hub subscribers at the end .... %d\n"+
		"  \"client too slow\" drops ........ %d\n"+
		"  server errors logged .......... %v\n",
		cond, painters, churners, scaled(20*time.Second),
		cycles.Load(), rate(int(cycles.Load()), scaled(20*time.Second)),
		aborted.Load(), dialFails.Load(), health.Places, health.Rejected,
		gorBefore, gorAfter, gorAfter-gorBefore,
		mib(memBefore.HeapAlloc), mib(memAfter.HeapAlloc),
		health.Clients, h.logs.count(msgSlowClient), h.logs.errorTail(3))

	if gorAfter-gorBefore > 8 {
		t.Errorf("GOROUTINE LEAK: %d goroutines remain after %d connect/disconnect cycles "+
			"(started at %d)", gorAfter, cycles.Load(), gorBefore)
	}
	if health.Clients != 0 {
		t.Errorf("the hub still reports %d subscribers after every client disconnected",
			health.Clients)
	}
	if dialFails.Load() > cycles.Load()/20 {
		t.Errorf("%d of %d handshakes failed, which is more than churn alone explains",
			dialFails.Load(), cycles.Load())
	}
}

// TestLoadPresenceStorm isolates something the churn and connection experiments
// both brush against: hub.Subscribe and hub.Unsubscribe each call
// announcePresence(true), and the "no more than once a second" guard only
// applies when the count has not changed - which, on a join or a leave, it
// always has. So a room that N people join costs N^2/2 broadcast frames.
func TestLoadPresenceStorm(t *testing.T) {
	requireLoadtest(t)
	dsn := requireDSN(t)
	cond := measureConditions()

	h := newHarness(t, harnessOpts{dsn: dsn})
	results := newTable("joiners", "join time", "frames delivered", "frames per joiner",
		"slow-drops", "cpu-cores")

	for _, joiners := range []int{25, 50, 100, 200} {
		slug, _ := createRoom(t, h.base, fmt.Sprintf("presence-%d", joiners), 32, 32, 0)

		conns := make([]*wsClient, 0, joiners)
		var frames atomic.Int64
		stop := make(chan struct{})
		var readers sync.WaitGroup

		slowBefore := h.logs.count(msgSlowClient)
		cpu0 := cpuTime()
		t0 := time.Now()
		for i := 0; i < joiners; i++ {
			p := newPainter(h.base)
			if _, err := p.bootstrap(slug); err != nil {
				t.Fatalf("joiner %d bootstrap: %v", i, err)
			}
			c, err := wsDial(wsURL(h.hostPort(), slug), p.cookieHeader())
			if err != nil {
				t.Fatalf("joiner %d dial: %v", i, err)
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
					if _, err := c.read(time.Now().Add(500 * time.Millisecond)); err != nil {
						if isTimeout(err) {
							continue
						}
						return
					}
					frames.Add(1)
				}
			}(c)
			p.close()
		}
		joinTime := time.Since(t0)
		time.Sleep(1500 * time.Millisecond)
		cpu := cpuTime() - cpu0

		close(stop)
		for _, c := range conns {
			c.close()
		}
		readers.Wait()

		results.add(fmt.Sprint(joiners), joinTime.Round(time.Millisecond).String(),
			fmt.Sprint(frames.Load()),
			fmt.Sprintf("%.0f", float64(frames.Load())/float64(joiners)),
			fmt.Sprint(h.logs.count(msgSlowClient)-slowBefore),
			fmt.Sprintf("%.2f", cpuBusy(cpu, joinTime+1500*time.Millisecond, cond.CPUs)))
	}

	t.Logf("\nPRESENCE STORM ON JOIN\nconditions: %s\n"+
		"an empty 32x32 room, no painting at all: every frame counted here is a presence\n"+
		"announcement caused by somebody else arriving. \"frames per joiner\" growing with\n"+
		"the number of joiners is the quadratic.\n%s", cond, results)
}
