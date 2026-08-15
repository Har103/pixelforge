package pg

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Pool is a small fixed-size connection pool with lazy dialling and automatic
// replacement of connections that die underneath us. Managed database providers
// idle-close aggressively, so every checkout re-validates.
type Pool struct {
	cfg     *Config
	size    int
	log     *slog.Logger
	idle    chan *Conn
	mu      sync.Mutex
	closed  bool
	opened  int
	maxLife time.Duration
	born    map[*Conn]time.Time
}

// NewPool creates a pool of at most size connections. Nothing is dialled until
// the first Acquire.
func NewPool(cfg *Config, size int, log *slog.Logger) *Pool {
	if size < 1 {
		size = 1
	}
	if log == nil {
		log = slog.Default()
	}
	p := &Pool{
		cfg:     cfg,
		size:    size,
		log:     log,
		idle:    make(chan *Conn, size),
		maxLife: 30 * time.Minute,
		born:    make(map[*Conn]time.Time, size),
	}
	// Seed the channel with slot tokens: a nil entry means "you may dial one".
	for i := 0; i < size; i++ {
		p.idle <- nil
	}
	return p
}

// Acquire takes a live connection from the pool, dialling or redialling as
// needed. Every acquired connection must be returned with Release.
func (p *Pool) Acquire(ctx context.Context) (*Conn, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, errors.New("pg: pool is closed")
	}
	p.mu.Unlock()

	select {
	case c := <-p.idle:
		if c != nil {
			if p.stillGood(ctx, c) {
				return c, nil
			}
			p.discard(c)
		}
		fresh, err := Connect(ctx, p.cfg)
		if err != nil {
			// Hand the slot back so we do not leak capacity on a failed dial.
			p.idle <- nil
			return nil, err
		}
		p.mu.Lock()
		p.born[fresh] = time.Now()
		p.mu.Unlock()
		return fresh, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// stillGood decides whether a pooled connection can be handed out. Connections
// past maxLife are retired proactively rather than failing mid-query.
func (p *Pool) stillGood(ctx context.Context, c *Conn) bool {
	if c.Broken() {
		return false
	}
	p.mu.Lock()
	born, ok := p.born[c]
	p.mu.Unlock()
	if ok && time.Since(born) > p.maxLife {
		return false
	}
	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := c.Ping(pingCtx); err != nil {
		p.log.Debug("pooled connection failed validation, redialling", "err", err)
		return false
	}
	return true
}

func (p *Pool) discard(c *Conn) {
	p.mu.Lock()
	delete(p.born, c)
	p.mu.Unlock()
	_ = c.Close()
}

// Release returns a connection to the pool. Broken connections are closed and
// the slot is freed for a fresh dial.
func (p *Pool) Release(c *Conn) {
	if c == nil {
		p.idle <- nil
		return
	}
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed || c.Broken() {
		p.discard(c)
		if !closed {
			p.idle <- nil
		}
		return
	}
	p.idle <- c
}

// Do runs fn with a pooled connection, always releasing it afterwards.
func (p *Pool) Do(ctx context.Context, fn func(*Conn) error) error {
	c, err := p.Acquire(ctx)
	if err != nil {
		return err
	}
	defer p.Release(c)
	return fn(c)
}

// Exec runs a statement on a pooled connection.
func (p *Pool) Exec(ctx context.Context, sql string) error {
	return p.Do(ctx, func(c *Conn) error { return c.Exec(ctx, sql) })
}

// Query runs a parameterised statement on a pooled connection.
func (p *Pool) Query(ctx context.Context, sql string, args ...any) (*Result, error) {
	var res *Result
	err := p.Do(ctx, func(c *Conn) error {
		var err error
		res, err = c.Query(ctx, sql, args...)
		return err
	})
	return res, err
}

// QueryRow returns the first row of a parameterised statement, or nil.
func (p *Pool) QueryRow(ctx context.Context, sql string, args ...any) ([][]byte, error) {
	res, err := p.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	if len(res.Rows) == 0 {
		return nil, nil
	}
	return res.Rows[0], nil
}

// Close drains and shuts every pooled connection.
func (p *Pool) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.mu.Unlock()

	for i := 0; i < p.size; i++ {
		select {
		case c := <-p.idle:
			if c != nil {
				p.discard(c)
			}
		case <-time.After(2 * time.Second):
		}
	}
}

// WaitReady blocks until the database answers or the context expires, retrying
// with backoff. A managed database provisioned alongside the app is routinely
// not listening yet when the app's first container starts.
func (p *Pool) WaitReady(ctx context.Context, attempts int) error {
	var last error
	delay := 500 * time.Millisecond
	for i := 0; i < attempts; i++ {
		attemptCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := p.Do(attemptCtx, func(c *Conn) error { return c.Ping(attemptCtx) })
		cancel()
		if err == nil {
			return nil
		}
		last = err
		p.log.Warn("database not ready yet", "attempt", i+1, "of", attempts, "err", err)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ctx.Err()
		}
		if delay < 5*time.Second {
			delay *= 2
		}
	}
	return fmt.Errorf("pg: database never became ready: %w", last)
}
