package loadtest

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestLoadThroughput answers "where does throughput top out, and what gives
// first". It ramps concurrency on both paint paths - POST /api/r/{slug}/place,
// which waits for an acknowledgement, and the "place" message over the socket,
// which does not - and records what the server itself reports at each step.
//
// The room has no cooldown. That is the only honest setting for this question:
// with the default 750ms, 200 painters could not exceed 266 placements a second
// no matter how fast the server was, and the number measured would be the
// product's rate limiter rather than its capacity.
func TestLoadThroughput(t *testing.T) {
	requireLoadtest(t)
	dsn := requireDSN(t)

	cond := measureConditions()
	h := newHarness(t, harnessOpts{dsn: dsn})

	// ------------------------------------------- HTTP, closed loop, ramping --
	//
	// A fresh room per step. Reusing one room lets the colours the previous
	// step left behind turn the next step's first pass into a wall of 409s,
	// which is a property of the experiment rather than of the server.
	httpTable := newTable("painters", "accepted", "rate/s", "cpu-cores",
		"hist-drops", "latency", "statuses")
	for _, workers := range []int{8, 32, 64, 128, 200} {
		slug, _ := createRoom(t, h.base, "tp-http", 256, 256, 0)
		r := runHTTPStorm(t, h, slug, workers, scaled(6*time.Second))
		httpTable.add(fmt.Sprint(workers), fmt.Sprint(r.accepted),
			fmt.Sprintf("%.0f", rate(r.accepted, r.elapsed)),
			fmt.Sprintf("%.2f", cpuBusy(r.cpu, r.elapsed, cond.CPUs)),
			fmt.Sprint(r.histDrops), r.lat.String(), r.statuses)
	}

	// ------------------------------------ WebSocket, paced, ramping offered --
	//
	// 64 sockets throughout, so the fan-out cost is held constant and the only
	// thing changing is how hard they paint.
	wsTable := newTable("offered/s", "offered", "accepted", "achieved/s", "cpu-cores",
		"hist-drops", "hist-drop%", "slow-drops", "behind", "broadcast latency")
	for _, target := range []int{1_000, 5_000, 10_000, 20_000, 30_000, 40_000, 60_000, 0} {
		slug, _ := createRoom(t, h.base, "tp-ws", 256, 256, 0)
		r := runPaced(t, h, pacedOpts{
			slug: slug, clients: 64, targetPS: target,
			dur: scaled(8 * time.Second), timed: 4,
		})
		label := fmt.Sprint(target)
		if target == 0 {
			label = "unbounded"
		}
		dropPct := 0.0
		if r.accepted > 0 {
			dropPct = 100 * float64(r.histDrops) / float64(r.accepted)
		}
		wsTable.add(label, fmt.Sprint(r.offered), fmt.Sprint(r.accepted),
			fmt.Sprintf("%.0f", r.acceptedRate()),
			fmt.Sprintf("%.2f", cpuBusy(r.cpu, r.elapsed, cond.CPUs)),
			fmt.Sprint(r.histDrops), fmt.Sprintf("%.1f%%", dropPct),
			fmt.Sprint(r.slowDrops), fmt.Sprint(r.behind), r.lat.String())
	}

	// ------------------------------------------------ where the limit lives --
	//
	// Same total painters, spread over one room or over eight. The canvas lock,
	// the hub and the write-behind queue are per room; the address limiter, the
	// HTTP stack and the CPU are not. If eight rooms go faster, the limit is
	// inside a room; if they do not, it is outside one.
	spreadTable := newTable("rooms", "painters total", "accepted", "rate/s", "cpu-cores", "latency")
	for _, rooms := range []int{1, 8} {
		slugs := make([]string, rooms)
		for i := range slugs {
			slugs[i], _ = createRoom(t, h.base, "tp-spread", 256, 256, 0)
		}
		r := runHTTPStormSpread(t, h, slugs, 64, scaled(6*time.Second))
		spreadTable.add(fmt.Sprint(rooms), "64", fmt.Sprint(r.accepted),
			fmt.Sprintf("%.0f", rate(r.accepted, r.elapsed)),
			fmt.Sprintf("%.2f", cpuBusy(r.cpu, r.elapsed, cond.CPUs)), r.lat.String())
	}

	t.Logf("\nTHROUGHPUT\nconditions: %s\nrooms are 256x256, cooldown 0ms, pool size 4, "+
		"address rate limit disabled, one fresh room per step\n\n"+
		"A. POST /api/r/{slug}/place, closed loop (client waits for the 200)\n%s\n"+
		"B. \"place\" over WebSocket, 64 sockets, offered rate ramped.\n"+
		"   latency = placement sent to that pixel arriving in a broadcast,\n"+
		"   which includes the hub's 50ms coalescing tick.\n%s\n"+
		"C. 64 HTTP painters spread over one room or eight.\n%s",
		cond, httpTable, wsTable, spreadTable)
	t.Logf("server log lines: %v", h.logs.summary())
	if tail := h.logs.errorTail(5); len(tail) > 0 {
		t.Logf("last server errors: %v", tail)
	}
}

// runHTTPStormSpread is runHTTPStorm with the painters divided between several
// rooms, which is the control for "is the ceiling inside a room or outside it".
func runHTTPStormSpread(t *testing.T, h *harness, slugs []string, workers int, dur time.Duration) stormResult {
	t.Helper()

	painters := make([]*painter, workers)
	assigned := make([]string, workers)
	for i := range painters {
		painters[i] = newPainter(h.base)
		assigned[i] = slugs[i%len(slugs)]
		if _, err := painters[i].bootstrap(assigned[i]); err != nil {
			t.Fatalf("painter %d bootstrap: %v", i, err)
		}
	}
	defer func() {
		for _, p := range painters {
			p.close()
		}
	}()

	cfg, err := painters[0].bootstrap(assigned[0])
	if err != nil {
		t.Fatalf("reading room config: %v", err)
	}
	w, hgt, colours := cfg.Room.Width, cfg.Room.Height, len(cfg.Room.Palette)

	lat := newLatencies(workers * 2048)
	var accepted atomic.Int64
	statuses := newStatusCounts()
	perRoom := workers / len(slugs)

	deadline := time.Now().Add(dur)
	cpu0 := cpuTime()
	start := time.Now()

	var wg sync.WaitGroup
	for i, p := range painters {
		wg.Add(1)
		go func(id int, p *painter) {
			defer wg.Done()
			local := make([]time.Duration, 0, 2048)
			plan := newPaintPlan(w, hgt, perRoom, id/len(slugs), colours)
			for time.Now().Before(deadline) {
				x, y, colour := plan.next()
				t0 := time.Now()
				status, err := p.place(assigned[id], x, y, colour)
				if err != nil {
					statuses.add(0)
					continue
				}
				statuses.add(status)
				if status == http.StatusOK {
					local = append(local, time.Since(t0))
					accepted.Add(1)
				}
			}
			lat.addLocal(local)
		}(i, p)
	}
	wg.Wait()

	return stormResult{
		accepted: int(accepted.Load()),
		elapsed:  time.Since(start),
		cpu:      cpuTime() - cpu0,
		lat:      lat.stats(),
		statuses: statuses.String(),
	}
}

// stormResult is one row of the ramp.
type stormResult struct {
	accepted int
	rejected int
	elapsed  time.Duration
	cpu      time.Duration
	lat      latencyStats
	// histDrops counts room.record throwing history away because the
	// write-behind buffer was full; slowDrops counts the hub disconnecting a
	// client that could not keep up. Both are the product reporting its own
	// degradation, which is why they are on every row.
	histDrops int
	slowDrops int
	// rejectedServerSide is the server's own refusal counter, which should
	// track the client's view; a divergence means placements were lost
	// somewhere other than where the client thought.
	rejectedServerSide int
	// statuses breaks the refusals down, so "rejected" is never a mystery.
	statuses string
}

// runHTTPStorm points N painters, each with its own identity and its own
// connection, at one room and lets them paint flat out for a while.
func runHTTPStorm(t *testing.T, h *harness, slug string, workers int, dur time.Duration) stormResult {
	t.Helper()

	painters := make([]*painter, workers)
	for i := range painters {
		painters[i] = newPainter(h.base)
		if _, err := painters[i].bootstrap(slug); err != nil {
			t.Fatalf("painter %d bootstrap: %v", i, err)
		}
	}
	defer func() {
		for _, p := range painters {
			p.close()
		}
	}()

	before := h.health()
	dropsBefore := h.logs.count(msgBufferFull)
	slowBefore := h.logs.count(msgSlowClient)

	cfg, err := painters[0].bootstrap(slug)
	if err != nil {
		t.Fatalf("reading room config: %v", err)
	}
	w, hgt, colours := cfg.Room.Width, cfg.Room.Height, len(cfg.Room.Palette)

	lat := newLatencies(workers * 2048)
	var accepted, rejected atomic.Int64
	statuses := newStatusCounts()

	deadline := time.Now().Add(dur)
	cpu0 := cpuTime()
	start := time.Now()

	var wg sync.WaitGroup
	for i, p := range painters {
		wg.Add(1)
		go func(id int, p *painter) {
			defer wg.Done()
			local := make([]time.Duration, 0, 2048)
			plan := newPaintPlan(w, hgt, workers, id, colours)
			for time.Now().Before(deadline) {
				x, y, colour := plan.next()
				t0 := time.Now()
				status, err := p.place(slug, x, y, colour)
				if err != nil {
					rejected.Add(1)
					statuses.add(0)
					continue
				}
				statuses.add(status)
				if status == http.StatusOK {
					local = append(local, time.Since(t0))
					accepted.Add(1)
				} else {
					rejected.Add(1)
				}
			}
			lat.addLocal(local)
		}(i, p)
	}
	wg.Wait()

	elapsed := time.Since(start)
	cpu := cpuTime() - cpu0
	after := h.health()

	return stormResult{
		accepted:  int(accepted.Load()),
		rejected:  int(rejected.Load()),
		elapsed:   elapsed,
		cpu:       cpu,
		lat:       lat.stats(),
		histDrops: h.logs.count(msgBufferFull) - dropsBefore,
		slowDrops: h.logs.count(msgSlowClient) - slowBefore,
		// The server's own refusal counter, so a client-side rejection that the
		// server never saw (a timeout, a dropped connection) is distinguishable
		// from one it issued.
		rejectedServerSide: int(after.Rejected - before.Rejected),
		statuses:           statuses.String(),
	}
}

// statusCounts explains the "rejected" column: a 409 is the canvas refusing a
// repaint, a 429 is a cooldown or the address limiter, and a 0 is the request
// never completing at all. Without the breakdown, "rejected" could mean the
// server is broken or could mean the workload is badly chosen - and in the
// first draft of this suite it meant the second.
type statusCounts struct {
	mu sync.Mutex
	n  map[int]int
}

func newStatusCounts() *statusCounts { return &statusCounts{n: map[int]int{}} }

func (s *statusCounts) add(code int) {
	s.mu.Lock()
	s.n[code]++
	s.mu.Unlock()
}

func (s *statusCounts) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]int, 0, len(s.n))
	for k := range s.n {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		label := fmt.Sprint(k)
		if k == 0 {
			label = "err"
		}
		parts = append(parts, fmt.Sprintf("%s:%d", label, s.n[k]))
	}
	return strings.Join(parts, " ")
}

func isDenial(payload []byte) bool {
	// Cheap substring test rather than a full unmarshal: this runs on every
	// control frame at full load.
	return len(payload) > 8 && containsBytes(payload, []byte(`"t":"denied"`))
}

func containsBytes(haystack, needle []byte) bool {
	if len(needle) > len(haystack) {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
