package pg

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// ErrPoolClosed is what a caller gets from Acquire once the pool has been shut
// down, whether it asked before the shutdown or was already waiting for a slot
// when it happened.
var ErrPoolClosed = errors.New("pg: pool is closed")

// Pool is a small fixed-size connection pool with lazy dialling and automatic
// replacement of connections that die underneath us. Managed database providers
// idle-close aggressively, so every checkout re-validates.
//
// The idle channel holds one entry per slot, and a slot is either a pooled
// connection or a nil token meaning "you may dial one". That is the pool's whole
// accounting: entries in the channel plus connections currently checked out
// always come to size. Everything that puts a slot back does so under mu and
// without blocking, which is what lets Close hand over with an in-flight Release
// exactly rather than approximately.
type Pool struct {
	cfg    *Config
	size   int
	log    *slog.Logger
	idle   chan *Conn
	mu     sync.Mutex
	closed bool
	// done is closed at the same moment, and under the same lock, as closed is
	// set. The flag is what the code under mu reads; the channel is what a
	// goroutine parked in a select can be woken by.
	done    chan struct{}
	maxLife time.Duration
	born    map[*Conn]time.Time

	// midRelease, when it is set, is called by Release from inside the critical
	// section in which it decides where a connection goes, and nothing sets it
	// outside this package's tests.
	//
	// It is here because the interleaving it exposes cannot be asked for. What
	// used to leak a connection was a Close completing in the few nanoseconds
	// between Release reading the closed flag and Release sending down the
	// channel, and no amount of hammering reliably lands a goroutine there - a
	// stress test that fails one run in fifty is not a test, it is a rumour.
	// Holding Release inside that section on demand turns "Close must not be able
	// to get past me here" into something a test can state and watch fail.
	midRelease func()
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
		done:    make(chan struct{}),
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
	select {
	case c := <-p.idle:
		return p.checkout(ctx, c)
	case <-p.done:
		// Waiting on done rather than testing a flag before the select is the
		// difference between a caller that is told the pool is gone and one that
		// is never told anything: Close drains every slot, so an Acquire already
		// parked here is waiting on a channel nothing will ever fill again. Only
		// the fact that request contexts carry deadlines kept that from being a
		// permanent goroutine leak in production.
		//
		// Once Close has returned, nothing can put a slot back - Release and
		// returnSlot both check under mu - so this is also the branch a late
		// caller takes, and it takes it deterministically.
		return nil, ErrPoolClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// checkout turns a slot into a connection to hand out: either the pooled
// connection that came with it, if it survives validation, or a fresh dial into
// the capacity the slot represents.
func (p *Pool) checkout(ctx context.Context, c *Conn) (*Conn, error) {
	if c != nil {
		if p.stillGood(ctx, c) {
			return c, nil
		}
		p.discard(c)
	}
	fresh, err := Connect(ctx, p.cfg)
	if err != nil {
		// Hand the slot back so we do not leak capacity on a failed dial.
		p.returnSlot()
		return nil, err
	}

	p.mu.Lock()
	if p.closed {
		// The pool shut down while this was dialling, so Close never saw this
		// connection and never will. Handing it to the caller would leave the
		// socket's fate to a Release the caller may not reach.
		p.mu.Unlock()
		_ = fresh.Close()
		return nil, ErrPoolClosed
	}
	p.born[fresh] = time.Now()
	p.mu.Unlock()
	return fresh, nil
}

// returnSlot gives back capacity that no connection is attached to any more, so
// the next caller can dial into it. The send is made under mu and cannot block,
// for the reasons set out in Release.
func (p *Pool) returnSlot() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	select {
	case p.idle <- nil:
	default:
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
		// Acquire never returns a nil connection with a nil error, and it hands
		// the slot back itself when a dial fails, so a nil here is a connection
		// the caller never held - most often a "defer p.Release(c)" written
		// above the error check. Returning a token for it would let the pool
		// open more connections than its size, and on a pool that is not
		// exhausted the channel is already full, so the send blocks forever.
		return
	}
	// Broken only ever goes from false to true, so reading it out here cannot
	// pool a connection already known to be dead. One that breaks a moment after
	// this read is pooled and then caught by the next Acquire's validation, which
	// is what has always happened to a connection that dies while it is idle.
	broken := c.Broken()

	p.mu.Lock()
	pooled := false
	if !p.closed {
		// Whether the pool is closed and where this connection goes are decided
		// under one hold of the same lock Close takes. Reading the flag and then
		// dropping the lock before the send - which is what this used to do -
		// leaves a window for Close to complete in: the connection then lands in
		// a channel nobody will ever read again and the socket stays open until
		// the process exits. Anything that closes and recreates a pool, which is
		// what a configuration reload does, leaks one every time.
		//
		// A broken connection's slot goes back empty, so the next caller dials
		// into that capacity instead of being handed a socket that is gone.
		slot := c
		if broken {
			slot = nil
		}
		if p.midRelease != nil {
			p.midRelease()
		}
		select {
		case p.idle <- slot:
			pooled = !broken
		default:
			// Unreachable while every acquired connection is released once:
			// Acquire takes a slot out before Release can put one back, so there
			// is always room. It is reachable when a caller releases twice, and
			// that is why the send is non-blocking rather than the lock simply
			// being held across a blocking one. A blocking send would park here
			// holding mu, and every goroutine that so much as asks the pool for a
			// connection would be stuck behind it; this costs the caller its
			// connection and a log line instead.
			p.log.Warn("pg: a connection came back to a pool with no slot free for it, " +
				"which means it was released twice; closing it")
		}
	}
	p.mu.Unlock()

	if !pooled {
		p.discard(c)
	}
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

// Close shuts every pooled connection. Connections that are checked out when it
// runs are closed by the Release that brings them back, so none is left open and
// none is closed twice.
//
// It deliberately waits for none of that. It used to collect the slots one at a
// time with a two-second fallback each, which for a pool of four with requests in
// flight is eight seconds added to a SIGTERM - most of the ten a container
// runtime typically allows before SIGKILL, spent waiting for connections that
// only come back when their request finishes anyway.
func (p *Pool) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	close(p.done)

	// Draining under mu is what makes the handover with Release exact: Release
	// decides where a connection goes under this same lock, so every connection
	// is either already in here to be collected or belongs to a Release that has
	// yet to look at the flag - and will then close it itself. The receives
	// cannot block, so nothing is held up by holding the lock across them.
	var open []*Conn
drain:
	for {
		select {
		case c := <-p.idle:
			if c != nil {
				delete(p.born, c)
				open = append(open, c)
			}
		default:
			break drain
		}
	}
	p.mu.Unlock()

	// Shutting the sockets down is I/O and is nobody's business but this
	// goroutine's, so it happens with the lock back.
	for _, c := range open {
		_ = c.Close()
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
