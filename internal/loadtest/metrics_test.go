package loadtest

import (
	"fmt"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// latencies collects raw samples rather than bucketing them, because the
// question is about the tail and a histogram's top bucket cannot answer "how
// bad was the worst one". Eight bytes a sample means a million placements costs
// 8 MB, which is affordable and bounded by the experiment's own length.
type latencies struct {
	mu sync.Mutex
	d  []time.Duration
}

func newLatencies(hint int) *latencies { return &latencies{d: make([]time.Duration, 0, hint)} }

// addLocal merges a worker's private slice in one lock acquisition. Workers
// record into their own slice and hand it over at the end, so the recorder is
// never the contended thing being measured.
func (l *latencies) addLocal(d []time.Duration) {
	l.mu.Lock()
	l.d = append(l.d, d...)
	l.mu.Unlock()
}

// stats sorts once and reads the percentiles off. Nearest-rank, so p99 of a
// hundred samples is the 99th slowest rather than an interpolation between two
// numbers that were never measured.
func (l *latencies) stats() latencyStats {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.d) == 0 {
		return latencyStats{}
	}
	sort.Slice(l.d, func(i, j int) bool { return l.d[i] < l.d[j] })

	at := func(p float64) time.Duration {
		idx := int(p * float64(len(l.d)))
		if idx >= len(l.d) {
			idx = len(l.d) - 1
		}
		return l.d[idx]
	}
	var sum time.Duration
	for _, v := range l.d {
		sum += v
	}
	return latencyStats{
		N:    len(l.d),
		Mean: sum / time.Duration(len(l.d)),
		P50:  at(0.50),
		P95:  at(0.95),
		P99:  at(0.99),
		P999: at(0.999),
		Max:  l.d[len(l.d)-1],
	}
}

type latencyStats struct {
	N                              int
	Mean, P50, P95, P99, P999, Max time.Duration
}

func (s latencyStats) String() string {
	if s.N == 0 {
		return "no samples"
	}
	return fmt.Sprintf("n=%d mean=%s p50=%s p95=%s p99=%s p99.9=%s max=%s",
		s.N, round(s.Mean), round(s.P50), round(s.P95), round(s.P99), round(s.P999), round(s.Max))
}

func round(d time.Duration) time.Duration {
	switch {
	case d > time.Second:
		return d.Round(10 * time.Millisecond)
	case d > time.Millisecond:
		return d.Round(100 * time.Microsecond)
	default:
		return d.Round(time.Microsecond)
	}
}

// ------------------------------------------------------------- memory ------

// memSample is one point on a growth curve. Sys is what the process has taken
// from the OS and never gives back readily, HeapInuse is what is live; the two
// together separate "leaking" from "the allocator is holding on".
type memSample struct {
	At         time.Duration
	HeapAlloc  uint64
	HeapInuse  uint64
	Sys        uint64
	Goroutines int
	NumGC      uint32
}

func sampleMem(since time.Time) memSample {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return memSample{
		At:         time.Since(since),
		HeapAlloc:  ms.HeapAlloc,
		HeapInuse:  ms.HeapInuse,
		Sys:        ms.Sys,
		Goroutines: runtime.NumGoroutine(),
		NumGC:      ms.NumGC,
	}
}

func (m memSample) String() string {
	return fmt.Sprintf("t=%-6s heap=%-8s inuse=%-8s sys=%-8s goroutines=%-5d gc=%d",
		m.At.Round(time.Second), mib(m.HeapAlloc), mib(m.HeapInuse), mib(m.Sys), m.Goroutines, m.NumGC)
}

func mib(b uint64) string { return fmt.Sprintf("%.1fMiB", float64(b)/(1<<20)) }

// settle forces the collector to run twice and gives finalisers a moment, so a
// "goroutines afterwards" reading is about leaks rather than about scheduling.
// Two collections because the first may only make things unreachable.
func settle() {
	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	runtime.GC()
	time.Sleep(50 * time.Millisecond)
}

// waitForGoroutines gives a count time to come down before declaring a leak.
// Server-side connection teardown is asynchronous - a WebSocket handler returns
// only after its writer goroutine has - so an immediate reading after the last
// client closes is measuring the shutdown, not a leak.
func waitForGoroutines(target int, within time.Duration) int {
	deadline := time.Now().Add(within)
	best := runtime.NumGoroutine()
	for time.Now().Before(deadline) {
		settle()
		n := runtime.NumGoroutine()
		if n < best {
			best = n
		}
		if n <= target {
			return n
		}
		time.Sleep(100 * time.Millisecond)
	}
	return best
}

// ------------------------------------------------------------- reporting ---

// table renders a fixed-width results table. Every experiment prints one, and
// every row carries the conditions it was measured under.
type table struct {
	header []string
	rows   [][]string
}

func newTable(header ...string) *table { return &table{header: header} }

func (t *table) add(cells ...string) {
	out := make([]string, len(cells))
	copy(out, cells)
	t.rows = append(t.rows, out)
}

func (t *table) String() string {
	widths := make([]int, len(t.header))
	for i, h := range t.header {
		widths[i] = len(h)
	}
	for _, r := range t.rows {
		for i, c := range r {
			if i < len(widths) && len(c) > widths[i] {
				widths[i] = len(c)
			}
		}
	}
	var sb strings.Builder
	line := func(cells []string) {
		for i, c := range cells {
			if i >= len(widths) {
				break
			}
			sb.WriteString(fmt.Sprintf("%-*s", widths[i], c))
			if i < len(cells)-1 {
				sb.WriteString("  ")
			}
		}
		sb.WriteString("\n")
	}
	line(t.header)
	sep := make([]string, len(t.header))
	for i := range sep {
		sep[i] = strings.Repeat("-", widths[i])
	}
	line(sep)
	for _, r := range t.rows {
		line(r)
	}
	return sb.String()
}

func rate(n int, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return float64(n) / d.Seconds()
}
