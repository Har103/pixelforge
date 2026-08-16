package loadtest

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Har103/pixelforge/internal/ws"
)

// TestLoadBaselineGenerator measures the ceiling this container and this load
// generator impose, with no Pixelforge in the picture at all: a handler that
// writes a constant, and a WebSocket endpoint that reads and discards.
//
// Every other number in this suite has to be read against these. A placement
// rate of N is only a statement about the product if the generator can do
// several times N against a handler that does nothing - otherwise the
// experiment measured itself.
func TestLoadBaselineGenerator(t *testing.T) {
	requireLoadtest(t)

	cond := measureConditions()
	t.Logf("conditions: %s", cond)

	results := newTable("path", "concurrency", "requests", "duration", "rate/s", "cpu-cores", "µs-cpu/req", "latency")

	// ---- HTTP: the same shape as POST /place, against a handler that does
	// nothing but reply. This is net/http client + loopback + net/http server.
	null := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer null.Close()

	for _, workers := range []int{8, 32, 64, 128, 200} {
		cpu0 := cpuTime()
		n, d, lat := hammerHTTP(null.URL, workers, scaled(3*time.Second))
		cpu := cpuTime() - cpu0
		results.add("http null handler", fmt.Sprint(workers), fmt.Sprint(n),
			d.Round(time.Millisecond).String(), fmt.Sprintf("%.0f", rate(n, d)),
			fmt.Sprintf("%.2f", cpuBusy(cpu, d, cond.CPUs)),
			fmt.Sprintf("%.1f", float64(cpu.Microseconds())/float64(max(n, 1))), lat.String())
	}

	// ---- WebSocket: the project's own upgrader, a reader that discards. This
	// is the frame cost, the mask cost and the scheduler, with no hub, no
	// canvas and no database.
	var received sync.WaitGroup
	nullWS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := ws.Upgrade(w, r, &ws.Options{MaxMessageSize: 8 << 10})
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer nullWS.Close()
	received.Wait()

	for _, clients := range []int{8, 32, 64, 128, 200} {
		cpu0 := cpuTime()
		n, d := hammerWSWrite(t, nullWS.Listener.Addr().String(), clients, scaled(3*time.Second))
		cpu := cpuTime() - cpu0
		results.add("ws null reader", fmt.Sprint(clients), fmt.Sprint(n),
			d.Round(time.Millisecond).String(), fmt.Sprintf("%.0f", rate(n, d)),
			fmt.Sprintf("%.2f", cpuBusy(cpu, d, cond.CPUs)),
			fmt.Sprintf("%.1f", float64(cpu.Microseconds())/float64(max(n, 1))),
			"n/a (fire and forget)")
	}

	t.Logf("\nGENERATOR / CONTAINER CEILING (no pixelforge)\n%s\n%s", cond, results)
}

// hammerHTTP runs a closed loop: every worker sends, waits for the reply, and
// sends again. Closed loop rather than open loop on purpose - it cannot outrun
// the thing it is measuring and pile up an unbounded queue in the client, which
// would turn "the server is slow" into "the generator ran out of memory".
func hammerHTTP(url string, workers int, dur time.Duration) (int, time.Duration, latencyStats) {
	lat := newLatencies(workers * 4096)
	deadline := time.Now().Add(dur)

	var wg sync.WaitGroup
	counts := make([]int, workers)
	start := time.Now()

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			c := &http.Client{
				Transport: &http.Transport{MaxConnsPerHost: 1, MaxIdleConnsPerHost: 1},
				Timeout:   30 * time.Second,
			}
			local := make([]time.Duration, 0, 4096)
			for time.Now().Before(deadline) {
				t0 := time.Now()
				res, err := c.Post(url, "application/json", nil)
				if err != nil {
					continue
				}
				_, _ = io.Copy(io.Discard, res.Body)
				res.Body.Close()
				local = append(local, time.Since(t0))
				counts[id]++
			}
			lat.addLocal(local)
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)

	total := 0
	for _, c := range counts {
		total += c
	}
	return total, elapsed, lat.stats()
}

// hammerWSWrite streams place-shaped frames as fast as the sockets accept them.
// There is no reply to wait for, which mirrors how the product's own client
// sends placements over the socket, so this is an open loop bounded by TCP
// backpressure rather than by an acknowledgement.
func hammerWSWrite(t *testing.T, hostPort string, clients int, dur time.Duration) (int, time.Duration) {
	t.Helper()

	conns := make([]*wsClient, 0, clients)
	for i := 0; i < clients; i++ {
		c, err := wsDial("ws://"+hostPort+"/", "")
		if err != nil {
			t.Fatalf("dialling baseline socket %d: %v", i, err)
		}
		conns = append(conns, c)
	}
	defer func() {
		for _, c := range conns {
			c.close()
		}
	}()

	deadline := time.Now().Add(dur)
	counts := make([]int, clients)
	var wg sync.WaitGroup
	start := time.Now()

	for i, c := range conns {
		wg.Add(1)
		go func(id int, c *wsClient) {
			defer wg.Done()
			for time.Now().Before(deadline) {
				if err := c.sendPlace(id%128, id/128, 1+(counts[id]%7)); err != nil {
					return
				}
				counts[id]++
			}
		}(i, c)
	}
	wg.Wait()
	elapsed := time.Since(start)

	total := 0
	for _, c := range counts {
		total += c
	}
	return total, elapsed
}

// ------------------------------------------------------------------ CPU -----

// cpuTime reports the process's user+system CPU consumption. The load generator
// and the server share this process, so the generator's own share - established
// by the baseline above - has to be subtracted before calling anything CPU
// bound. Getrusage is in syscall, which is standard library.
func cpuTime() time.Duration {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	user := time.Duration(ru.Utime.Sec)*time.Second + time.Duration(ru.Utime.Usec)*time.Microsecond
	sys := time.Duration(ru.Stime.Sec)*time.Second + time.Duration(ru.Stime.Usec)*time.Microsecond
	return user + sys
}

// cpuBusy is the fraction of the machine's cores this process kept busy over a
// window: 1.0 means one core saturated, and on a two-core container 2.0 is the
// wall.
func cpuBusy(cpuDelta, wall time.Duration, cores int) float64 {
	if wall <= 0 || cores <= 0 {
		return 0
	}
	return cpuDelta.Seconds() / wall.Seconds()
}
