// Package loadtest drives the real Pixelforge server, in-process, over real
// HTTP and real WebSockets, hard enough to find where it stops coping.
//
// Nothing here is part of the shipped binary: every file in this package is a
// _test.go file, so the package contributes no code to cmd/pixelforge and
// imports nothing outside the standard library.
//
// # Running it
//
// The whole suite is skipped unless PIXELFORGE_LOADTEST=1 is set and -short is
// off, so `go test ./...` stays fast for everybody else:
//
//	PIXELFORGE_LOADTEST=1 \
//	PIXELFORGE_TEST_DSN='postgres://pf:pf@127.0.0.1:5432/pf_load?sslmode=disable' \
//	  go test ./internal/loadtest/ -run TestLoad -v -timeout 30m
//
// Individual experiments:
//
//	-run TestLoadBaselineGenerator   generator/container ceiling, no product
//	-run TestLoadThroughput          placements/sec and latency percentiles
//	-run TestLoadWriteBehind         when the history queue drops, and whether
//	                                 a snapshot really rescues the pixel
//	-run TestLoadManyConnections     hundreds of sockets, leaks, slow clients
//	-run TestLoadChurn               connect/disconnect storm (run with -race)
//	-run TestLoadManyRooms           past the 64-room resident cap, eviction
//	-run TestLoadSlowDatabase        latency and outage, via a stdlib TCP proxy
//	-run TestLoadSoak                growth curves for the unbounded-memory question
//
// Knobs, all optional:
//
//	PIXELFORGE_LOADTEST_SCALE   float, multiplies durations (default 1.0)
//	PIXELFORGE_LOADTEST_KEEP    1 keeps the rooms it creates, for poking at
package loadtest

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Har103/pixelforge/internal/auth"
	"github.com/Har103/pixelforge/internal/httpapi"
	"github.com/Har103/pixelforge/internal/pg"
	"github.com/Har103/pixelforge/internal/room"
	"github.com/Har103/pixelforge/internal/store"
	"github.com/Har103/pixelforge/web"
)

// ---------------------------------------------------------------- gating -----

// requireLoadtest skips unless the operator has asked for the heavy suite. Two
// gates rather than one: the env var is the deliberate opt-in, and -short is
// what CI and `go test ./...` reach for by reflex.
func requireLoadtest(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("load tests are skipped in -short mode")
	}
	if os.Getenv("PIXELFORGE_LOADTEST") != "1" {
		t.Skip("set PIXELFORGE_LOADTEST=1 to run the load suite (see package doc)")
	}
}

// requireDSN returns the database this suite may abuse.
func requireDSN(t *testing.T) string {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("PIXELFORGE_TEST_DSN"))
	if dsn == "" {
		dsn = strings.TrimSpace(os.Getenv("DATABASE_URL"))
	}
	if dsn == "" {
		t.Skip("set PIXELFORGE_TEST_DSN to a throwaway database to run the load suite")
	}
	return dsn
}

// scale stretches or shrinks every experiment's duration together, so a slower
// machine can still get through the suite without editing constants.
func scale() float64 {
	v := strings.TrimSpace(os.Getenv("PIXELFORGE_LOADTEST_SCALE"))
	if v == "" {
		return 1
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f <= 0 {
		return 1
	}
	return f
}

func scaled(d time.Duration) time.Duration { return time.Duration(float64(d) * scale()) }

// ------------------------------------------------------------- conditions ---

// conditions is the hardware and configuration a number was measured under. A
// throughput figure without one of these attached is not a result.
type conditions struct {
	CPUs       int
	GOMAXPROCS int
	CPUQuota   string
	MemLimit   string
	Go         string
}

func measureConditions() conditions {
	return conditions{
		CPUs:       runtime.NumCPU(),
		GOMAXPROCS: runtime.GOMAXPROCS(0),
		CPUQuota:   readCgroup("cpu.max", "cpu/cpu.cfs_quota_us"),
		MemLimit:   readCgroup("memory.max", "memory/memory.limit_in_bytes"),
		Go:         runtime.Version(),
	}
}

func (c conditions) String() string {
	return fmt.Sprintf("cpus=%d gomaxprocs=%d cpu.max=%s mem.max=%s go=%s",
		c.CPUs, c.GOMAXPROCS, c.CPUQuota, c.MemLimit, c.Go)
}

// readCgroup reports a cgroup v2 file, falling back to the v1 path, so the
// report says what the container was actually allowed rather than what the
// host happens to have.
func readCgroup(v2, v1 string) string {
	for _, p := range []string{"/sys/fs/cgroup/" + v2, "/sys/fs/cgroup/" + v1} {
		if b, err := os.ReadFile(p); err == nil {
			s := strings.TrimSpace(string(b))
			if s != "" {
				return s
			}
		}
	}
	return "unset"
}

// ------------------------------------------------------------- the server ---

// harness is one whole Pixelforge: pool, store, registry, handlers, listener.
// It is the same wiring cmd/pixelforge does, and the same wiring the httpapi
// and e2e suites use, so what it measures is the product rather than a mock.
type harness struct {
	t        *testing.T
	srv      *httptest.Server
	base     string
	pool     *pg.Pool
	store    *store.Store
	registry *room.Registry
	api      *httpapi.Server
	logs     *logCapture

	stopRegistry context.CancelFunc
	closed       bool
}

// harnessOpts are the few things an experiment needs to vary.
type harnessOpts struct {
	dsn string
	// poolSize mirrors cmd/pixelforge's PG_POOL_SIZE. 4 is what the other
	// suites use; the throughput experiment sweeps it.
	poolSize int
	// rateLimitPerMin is set absurdly high by default: every virtual painter
	// shares 127.0.0.1, so the per-address limiter would otherwise be the only
	// thing this suite ever measured. The limiter has its own unit test.
	rateLimitPerMin int
	// ephemeral runs the no-database mode.
	ephemeral bool
}

func newHarness(t *testing.T, opts harnessOpts) *harness {
	t.Helper()

	if opts.poolSize == 0 {
		opts.poolSize = 4
	}
	if opts.rateLimitPerMin == 0 {
		opts.rateLimitPerMin = 100_000_000
	}

	logs := newLogCapture()
	log := slog.New(logs)

	h := &harness{t: t, logs: logs}

	if !opts.ephemeral {
		cfg, err := pg.ParseDSN(opts.dsn)
		if err != nil {
			t.Fatalf("parsing DSN: %v", err)
		}
		h.pool = pg.NewPool(cfg, opts.poolSize, log)

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := h.pool.WaitReady(ctx, 3); err != nil {
			t.Fatalf("database not reachable: %v", err)
		}
		h.store = store.New(h.pool, log)
		if err := h.store.Migrate(ctx); err != nil {
			t.Fatalf("migrating: %v", err)
		}
	} else {
		h.store = store.New(nil, log)
	}

	h.registry = room.NewRegistry(h.store, log)
	runCtx, stop := context.WithCancel(context.Background())
	h.stopRegistry = stop
	go h.registry.Run(runCtx)

	secret := []byte("loadtest-secret-please-ignore")
	h.api = &httpapi.Server{
		Rooms: h.registry, Store: h.store,
		Signer: auth.NewSigner(secret), Secret: secret,
		Log: log, Static: web.FS(), Version: "loadtest",
		RateLimitPerMin: opts.rateLimitPerMin,
	}

	srv := httptest.NewUnstartedServer(h.api.Routes())
	srv.Config.ReadHeaderTimeout = 10 * time.Second
	srv.Config.IdleTimeout = 120 * time.Second
	h.api.BaseURL = "http://" + srv.Listener.Addr().String()
	srv.Start()
	h.srv = srv
	h.base = srv.URL

	t.Cleanup(h.Close)
	return h
}

// Close shuts the server down the way SIGTERM does: stop serving, then cancel
// the registry so every resident room drains its queue and writes a final
// snapshot. Experiments that want a *crash* instead use killWithoutFlush.
func (h *harness) Close() {
	if h.closed {
		return
	}
	h.closed = true
	h.srv.Close()
	h.stopRegistry()
	// registry.Run's shutdown is synchronous inside the goroutine; give it the
	// room's own 20s stop budget to finish writing before the pool goes away.
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		if rooms, _ := h.registry.Resident(); rooms == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if h.pool != nil {
		h.pool.Close()
	}
}

// killWithoutFlush ends the server without letting any room write its shutdown
// snapshot, which is what a SIGKILL, an OOM kill or a node loss looks like from
// the database's point of view. The caller must have already made writes fail
// (see dbProxy.Cut) so the drain the registry attempts cannot land.
func (h *harness) killWithoutFlush() {
	if h.closed {
		return
	}
	h.closed = true
	h.srv.Close()
	h.stopRegistry()
	// Do not wait for rooms to drain: the point is that they never get to.
	if h.pool != nil {
		go h.pool.Close()
	}
}

// hostPort is the listener address, for clients that speak raw TCP.
func (h *harness) hostPort() string { return h.srv.Listener.Addr().String() }

// health reads /healthz, which carries the server's own counters.
func (h *harness) health() healthz {
	h.t.Helper()
	var out healthz
	res, err := http.Get(h.base + "/healthz")
	if err != nil {
		h.t.Fatalf("GET /healthz: %v", err)
	}
	defer res.Body.Close()
	decodeJSON(h.t, res.Body, &out)
	return out
}

type healthz struct {
	Status    string `json:"status"`
	Ephemeral bool   `json:"ephemeral"`
	Rooms     int    `json:"rooms"`
	Clients   int    `json:"clients"`
	Requests  uint64 `json:"requests"`
	Places    uint64 `json:"places"`
	Rejected  uint64 `json:"rejected"`
}

func decodeJSON(t *testing.T, r io.Reader, dst any) {
	t.Helper()
	if err := jsonDecode(r, dst); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
}

// splitDSN pulls the pieces a TCP proxy needs out of a DSN: where PostgreSQL
// actually is, and the credentials to rebuild a DSN pointing at the proxy
// instead. It leans on the project's own parser rather than a second one.
func splitDSN(t *testing.T, dsn string) (hostPort, database, user, password string) {
	t.Helper()
	cfg, err := pg.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("parsing DSN: %v", err)
	}
	host := cfg.Host
	if host == "" || strings.HasPrefix(host, "/") {
		// A unix socket cannot be proxied over TCP, and every experiment that
		// needs the proxy needs a TCP database.
		t.Skipf("this experiment needs a TCP PostgreSQL; the DSN points at %q", cfg.Host)
	}
	return net.JoinHostPort(host, cfg.Port), cfg.Database, cfg.User, cfg.Password
}
