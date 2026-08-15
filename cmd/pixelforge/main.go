// Command pixelforge serves a shared, live pixel canvas.
//
// Everything here is standard library only - the PostgreSQL driver and the
// WebSocket server included. See internal/pg and internal/ws.
package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Har103/pixelforge/internal/canvas"
	"github.com/Har103/pixelforge/internal/httpapi"
	"github.com/Har103/pixelforge/internal/hub"
	"github.com/Har103/pixelforge/internal/pg"
	"github.com/Har103/pixelforge/web"
)

// version is stamped at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	// The container image is built FROM scratch, so there is no shell and no
	// curl for Docker's HEALTHCHECK to call. The binary probes itself instead.
	if len(os.Args) > 1 && (os.Args[1] == "-healthcheck" || os.Args[1] == "--healthcheck") {
		os.Exit(healthcheck())
	}
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// healthcheck returns 0 when the local server answers /healthz.
func healthcheck() int {
	port := envStr("PORT", "8080")
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthcheck: status", resp.StatusCode)
		return 1
	}
	return 0
}

func run() error {
	log := newLogger()
	slog.SetDefault(log)

	cfg := loadConfig()
	log.Info("starting pixelforge",
		"version", version,
		"port", cfg.Port,
		"canvas", fmt.Sprintf("%dx%d", cfg.Width, cfg.Height),
		"cooldown", cfg.Cooldown,
	)

	board := canvas.New(cfg.Width, cfg.Height, cfg.Cooldown)

	// The database is optional. Without one the canvas still works, it just
	// does not survive a restart - which beats crash-looping in front of
	// whoever is looking at the deployment.
	var pool *pg.Pool
	if dbCfg, err := pg.ConfigFromEnv(); err != nil {
		log.Warn("running without persistence", "reason", err)
	} else {
		log.Info("connecting to postgres", "target", dbCfg.Redacted())
		pool = pg.NewPool(dbCfg, cfg.PoolSize, log)
		defer pool.Close()

		readyCtx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		err := pool.WaitReady(readyCtx, 8)
		cancel()
		if err != nil {
			return fmt.Errorf("database never came up: %w", err)
		}
		log.Info("postgres connected")
	}

	store := canvas.NewStore(pool, board, log)

	bootCtx, cancelBoot := context.WithTimeout(context.Background(), 60*time.Second)
	if err := store.Migrate(bootCtx); err != nil {
		cancelBoot()
		return err
	}
	if err := store.Restore(bootCtx); err != nil {
		cancelBoot()
		return err
	}
	cancelBoot()

	broker := hub.New(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	hubDone := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); broker.Run(hubDone) }()
	go func() { defer wg.Done(); store.Run(ctx) }()

	srv := &httpapi.Server{
		Canvas:  board,
		Store:   store,
		Hub:     broker,
		Log:     log,
		Static:  web.FS(),
		Version: version,
		Secret:  cfg.Secret,
	}

	httpServer := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: srv.Routes(),
		// No WriteTimeout: SSE and WebSocket connections are long-lived by
		// design and a write deadline would cut them off mid-stream.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelDebug),
	}

	serveErr := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", httpServer.Addr, "url", "http://localhost:"+cfg.Port)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
		close(serveErr)
	}()

	select {
	case err := <-serveErr:
		if err != nil {
			return err
		}
	case <-ctx.Done():
		log.Info("shutdown signal received, draining")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Warn("http shutdown", "err", err)
	}
	close(hubDone)
	stop() // cancels ctx so store.Run drains and writes its final snapshot

	waited := make(chan struct{})
	go func() { wg.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(25 * time.Second):
		log.Warn("background workers did not finish in time")
	}

	log.Info("stopped cleanly")
	return nil
}

type config struct {
	Port     string
	Width    int
	Height   int
	Cooldown time.Duration
	PoolSize int
	Secret   []byte
}

func loadConfig() config {
	c := config{
		Port:     envStr("PORT", "8080"),
		Width:    envInt("CANVAS_WIDTH", 256, 16, 1024),
		Height:   envInt("CANVAS_HEIGHT", 256, 16, 1024),
		Cooldown: time.Duration(envInt("COOLDOWN_MS", 750, 0, 3_600_000)) * time.Millisecond,
		PoolSize: envInt("DB_POOL_SIZE", 4, 1, 32),
	}

	if s := strings.TrimSpace(os.Getenv("APP_SECRET")); s != "" {
		c.Secret = []byte(s)
	} else {
		// A per-boot secret means identity cookies are invalidated on restart,
		// which resets everyone's cooldown. Fine for a demo, worth a warning.
		c.Secret = make([]byte, 32)
		if _, err := rand.Read(c.Secret); err != nil {
			c.Secret = []byte("pixelforge-insecure-fallback-secret")
		}
		slog.Warn("APP_SECRET is not set, generating a random one - identity cookies will not survive a restart")
	}
	return c
}

func envStr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def, minV, maxV int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		slog.Warn("ignoring unparseable value", "key", key, "value", raw)
		return def
	}
	if n < minV || n > maxV {
		slog.Warn("value out of range, clamping", "key", key, "value", n, "min", minV, "max", maxV)
		return max(minV, min(maxV, n))
	}
	return n
}

// newLogger emits JSON when LOG_FORMAT=json, which is what a platform's log
// viewer wants, and human-readable text otherwise.
func newLogger() *slog.Logger {
	level := slog.LevelInfo
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LOG_LEVEL"))) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	opts := &slog.HandlerOptions{Level: level}
	if strings.EqualFold(os.Getenv("LOG_FORMAT"), "json") {
		return slog.New(slog.NewJSONHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stdout, opts))
}
