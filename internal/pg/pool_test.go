package pg

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestPool builds a pool pointed at a fake server that answers every query.
// Nothing here needs a real database, so these run everywhere - which matters,
// because concurrency bugs are found by running the test a thousand times on
// every machine that ever builds the project, not by running it once on mine.
func newTestPool(t *testing.T, size int) (*Pool, *fakeServer) {
	t.Helper()
	srv := newFakeServer(t, serveIdle)
	p := NewPool(srv.config(), size, quietLogger())
	t.Cleanup(p.Close)
	return p, srv
}

// waitFor polls until cond holds or the deadline passes. Concurrency tests need
// to wait for something to become true without a sleep standing in for a
// happens-before edge; this at least fails loudly instead of flaking quietly.
func waitFor(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", within, what)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestPoolConcurrentAcquireRelease is the blunt instrument: many goroutines
// hammering the pool under the race detector, which is what turned up the
// send-on-closed-channel crash in the hub. Every acquire must yield a usable
// connection and every release must return the slot.
func TestPoolConcurrentAcquireRelease(t *testing.T) {
	const (
		size    = 4
		workers = 24
		rounds  = 15
	)
	p, srv := newTestPool(t, size)

	var wg sync.WaitGroup
	errs := make(chan error, workers*rounds)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < rounds; j++ {
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				err := p.Do(ctx, func(c *Conn) error {
					_, qerr := c.Query(ctx, "select 1")
					return qerr
				})
				cancel()
				if err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("a pooled query failed under contention: %v", err)
	}
	if peak := srv.peakLive.Load(); peak > size {
		t.Errorf("the server saw %d simultaneous connections for a pool of %d; the pool "+
			"is not bounding itself and would blow through the database's connection "+
			"limit under load", peak, size)
	}

	// Every slot must still be there. If a release lost one, the pool silently
	// degrades to a smaller pool and eventually to a deadlock.
	assertFullCapacity(t, p, size)
}

// assertFullCapacity acquires every slot the pool should have, proving none
// leaked, and then puts them all back.
func assertFullCapacity(t *testing.T, p *Pool, size int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	held := make([]*Conn, 0, size)
	for i := 0; i < size; i++ {
		c, err := p.Acquire(ctx)
		if err != nil {
			t.Fatalf("only %d of %d slots were still available: %v; the pool leaked "+
				"capacity", i, size, err)
		}
		held = append(held, c)
	}
	for _, c := range held {
		p.Release(c)
	}
}

// TestPoolExhaustionBlocksRatherThanOverdialling checks the whole point of a
// fixed-size pool. A pool that quietly dials past its size is the classic way an
// application takes a database down under a traffic spike.
func TestPoolExhaustionBlocksRatherThanOverdialling(t *testing.T) {
	const size = 2
	p, srv := newTestPool(t, size)
	ctx := bg(t)

	held := make([]*Conn, 0, size)
	for i := 0; i < size; i++ {
		c, err := p.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquiring slot %d: %v", i, err)
		}
		held = append(held, c)
	}

	// With every slot out, an acquire has nothing to give and must wait for the
	// context rather than dial a third connection.
	short, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if c, err := p.Acquire(short); err == nil {
		p.Release(c)
		t.Fatalf("the pool handed out a %drd connection for a size of %d", size+1, size)
	} else if err != context.DeadlineExceeded {
		t.Errorf("Acquire returned %v, want context.DeadlineExceeded", err)
	}
	if n := srv.accepted.Load(); n > int64(size) {
		t.Errorf("the server accepted %d connections for a pool of %d", n, size)
	}

	for _, c := range held {
		p.Release(c)
	}
	assertFullCapacity(t, p, size)
}

// TestPoolAcquireWithADeadContextLeaksNothing is the "cancelled mid-acquire"
// case. Which branch of the select wins is genuinely random when both are ready,
// so the outcome of any single call cannot be asserted - what can be asserted is
// that neither branch loses a slot, which is the failure that matters and the
// one that only shows up hours later as a stuck application.
func TestPoolAcquireWithADeadContextLeaksNothing(t *testing.T) {
	const size = 3
	p, _ := newTestPool(t, size)

	// Warm the pool so the idle channel holds live connections rather than
	// tokens, which exercises the validate-then-discard path too.
	assertFullCapacity(t, p, size)

	for i := 0; i < 40; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		c, err := p.Acquire(ctx)
		if err == nil {
			p.Release(c)
		}
	}
	assertFullCapacity(t, p, size)
}

// TestPoolReleaseOfNilDoesNotWedgeThePool is a regression test. Release(nil)
// used to push a slot token onto a channel that is already full, so the call
// never returned: one "defer p.Release(c)" written above the error check and
// the request goroutine is gone for good. Worse, on a partly-drained pool the
// extra token would have let the pool open more connections than its size.
func TestPoolReleaseOfNilDoesNotWedgeThePool(t *testing.T) {
	const size = 2
	p, srv := newTestPool(t, size)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 5; i++ {
			p.Release(nil)
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Release(nil) never returned; the caller's goroutine is wedged forever")
	}

	// The pool must be exactly as big as it was, not bigger.
	ctx := bg(t)
	held := make([]*Conn, 0, size)
	for i := 0; i < size; i++ {
		c, err := p.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquiring slot %d after Release(nil): %v", i, err)
		}
		held = append(held, c)
	}
	short, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if c, err := p.Acquire(short); err == nil {
		p.Release(c)
		t.Error("Release(nil) inflated the pool past its configured size, so it will " +
			"open more connections than the database allows")
	}
	for _, c := range held {
		p.Release(c)
	}
	if n := srv.accepted.Load(); n > int64(size) {
		t.Errorf("the server accepted %d connections for a pool of %d", n, size)
	}
}

// TestPoolNeverHandsOnABrokenConnection: the connection the pool is holding is
// the one that was open when the server restarted. Handing it to the next
// caller turns one dropped socket into a stream of failures.
func TestPoolNeverHandsOnABrokenConnection(t *testing.T) {
	// The first connection dies on the first query it is asked to run, the way a
	// backend does when the server is restarted underneath it. A freshly dialled
	// connection is handed out without validation, so this is the caller's own
	// query that fails, which is exactly the situation being tested.
	var seen atomic.Int64
	srv := newFakeServer(t, func(f *fakeConn) {
		if seen.Add(1) == 1 {
			f.acceptStartup()
			if _, _, err := f.readMsg(); err == nil {
				_ = f.Close()
			}
			return
		}
		serveIdle(f)
	})
	p := NewPool(srv.config(), 1, quietLogger())
	t.Cleanup(p.Close)
	ctx := bg(t)

	doomed, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if _, err := doomed.Query(ctx, "select 1"); err == nil {
		t.Fatal("the query against the dying connection reported success")
	}
	if !doomed.Broken() {
		t.Fatal("the connection is not marked broken, so Release will pool it again")
	}
	p.Release(doomed)

	fresh, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquiring after a broken connection was returned: %v", err)
	}
	defer p.Release(fresh)
	if fresh == doomed {
		t.Fatal("the pool handed back the same broken connection; every subsequent " +
			"request on it fails until the process restarts")
	}
	if _, err := fresh.Query(ctx, "select 1"); err != nil {
		t.Errorf("the replacement connection does not work: %v", err)
	}
}

// TestPoolClosesABrokenConnectionOnRelease isolates Release's own check. The
// broken flag is set here without the server going away, because a connection
// whose peer has already hung up would be closed by the next acquire's
// validation anyway - and then this test would pass whether Release did its job
// or not. What Release owes the pool is that the socket goes away now rather
// than being carried until somebody happens to ask for it.
func TestPoolClosesABrokenConnectionOnRelease(t *testing.T) {
	p, srv := newTestPool(t, 2)

	c, err := p.Acquire(bg(t))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "the connection to be established",
		func() bool { return srv.live.Load() == 1 })

	c.broken = true
	p.Release(c)

	waitFor(t, 10*time.Second, "Release to close the broken connection",
		func() bool { return srv.live.Load() == 0 })
}

// TestPoolValidatesBeforeHandingOut covers the case the broken flag cannot see:
// the server closed an idle connection and the driver has not noticed, because
// nothing has been read from it since. Managed providers do this constantly.
func TestPoolValidatesBeforeHandingOut(t *testing.T) {
	var seen atomic.Int64
	srv := newFakeServer(t, func(f *fakeConn) {
		if seen.Add(1) == 1 {
			f.acceptStartup()
			// Vanish the way an idle-timeout reaper does: without warning, and
			// before the next validation.
			_ = f.Close()
			return
		}
		serveIdle(f)
	})
	p := NewPool(srv.config(), 1, quietLogger())
	t.Cleanup(p.Close)
	ctx := bg(t)

	dead, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	p.Release(dead)
	waitFor(t, 10*time.Second, "the server to drop the idle connection",
		func() bool { return srv.live.Load() == 0 })

	c, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquiring after the server dropped the idle connection: %v", err)
	}
	defer p.Release(c)
	if c == dead {
		t.Fatal("the pool handed back the connection the server had already closed; " +
			"the caller's first query fails for no reason it can see")
	}
	if _, err := c.Query(ctx, "select 1"); err != nil {
		t.Errorf("the revalidated connection does not work: %v", err)
	}
}

// TestPoolRetiresConnectionsPastMaxLife: long-lived connections are retired
// before they can fail in the middle of somebody's query.
func TestPoolRetiresConnectionsPastMaxLife(t *testing.T) {
	p, srv := newTestPool(t, 1)
	ctx := bg(t)

	first, err := p.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p.Release(first)

	// Nothing else is touching the pool here, so writing maxLife directly is the
	// honest way to age a connection without sleeping for half an hour.
	p.maxLife = time.Nanosecond

	second, err := p.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Release(second)
	if second == first {
		t.Error("a connection older than maxLife was handed out again, so it will keep " +
			"being reused until it dies mid-query")
	}
	if n := srv.accepted.Load(); n != 2 {
		t.Errorf("the server saw %d connections, want 2 (the original and its replacement)", n)
	}
}

// TestPoolAcquireAfterCloseFails: a request arriving during shutdown must be
// rejected rather than served over a connection nobody will ever close.
func TestPoolAcquireAfterCloseFails(t *testing.T) {
	p, _ := newTestPool(t, 2)
	p.Close()

	c, err := p.Acquire(bg(t))
	if err == nil {
		p.Release(c)
		t.Fatal("Acquire succeeded on a closed pool")
	}
	if !strings.Contains(err.Error(), "closed") {
		t.Errorf("error %q does not say the pool was closed", err)
	}
}

// TestPoolCloseIsIdempotent, because a deferred Close and an explicit one on the
// shutdown path both reach it.
func TestPoolCloseIsIdempotent(t *testing.T) {
	p, _ := newTestPool(t, 2)
	if _, err := p.Acquire(bg(t)); err != nil {
		t.Fatal(err)
	}
	p.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Close()
		p.Close()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a second Close blocked; shutdown would hang")
	}
}

// TestPoolCloseRacingAcquireAndRelease is the hub's bug class aimed at this
// package: a shutdown happening while requests are still in flight. Nothing here
// may panic, deadlock, or trip the race detector, and every goroutine must
// finish.
func TestPoolCloseRacingAcquireAndRelease(t *testing.T) {
	const workers = 12
	p, _ := newTestPool(t, 4)

	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 8; j++ {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				c, err := p.Acquire(ctx)
				if err == nil {
					_, _ = c.Query(ctx, "select 1")
					p.Release(c)
				}
				cancel()
			}
		}()
	}
	// Close from several goroutines at once, which is what a signal handler
	// racing a health check looks like.
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			p.Close()
		}()
	}

	close(start)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("goroutines were still blocked 60s after Close; the pool deadlocked " +
			"under a shutdown racing in-flight requests")
	}
}

// TestPoolReleaseAfterCloseClosesTheConnection: a connection checked out when
// shutdown began must be closed when it comes back, not quietly retained. The
// process usually exits straight afterwards, but a pool that is closed and
// recreated - which is what a reconnect-on-config-change does - would leak a
// socket every time.
func TestPoolReleaseAfterCloseClosesTheConnection(t *testing.T) {
	p, srv := newTestPool(t, 1)

	c, err := p.Acquire(bg(t))
	if err != nil {
		t.Fatal(err)
	}
	p.Close()
	p.Release(c)

	waitFor(t, 15*time.Second, "the released connection to be closed",
		func() bool { return srv.live.Load() == 0 })
}

// TestPoolFailedDialReturnsItsSlot: a database that is down for a moment must
// not cost the pool a slot permanently. Losing one slot per failed dial means
// the pool shrinks to nothing over a single outage and never recovers.
func TestPoolFailedDialReturnsItsSlot(t *testing.T) {
	const size = 2
	var seen atomic.Int64
	srv := newFakeServer(t, func(f *fakeConn) {
		if seen.Add(1) <= int64(size) {
			// The first attempts fail the way a database mid-restart does.
			_ = f.Close()
			return
		}
		serveIdle(f)
	})
	p := NewPool(srv.config(), size, quietLogger())
	t.Cleanup(p.Close)

	for i := 0; i < size; i++ {
		if _, err := p.Acquire(bg(t)); err == nil {
			t.Fatalf("acquire %d succeeded although the server refuses to talk", i)
		}
	}
	assertFullCapacity(t, p, size)
}

// TestPoolDoReleasesWhenTheCallbackPanics, because a handler panic recovered
// higher up must not cost a slot. Losing one per panic is how an application
// that is merely erroring becomes an application that is hung.
func TestPoolDoReleasesWhenTheCallbackPanics(t *testing.T) {
	const size = 2
	p, _ := newTestPool(t, size)

	func() {
		defer func() {
			if recover() == nil {
				t.Error("the panic did not propagate out of Do")
			}
		}()
		_ = p.Do(bg(t), func(*Conn) error { panic("boom") })
	}()

	assertFullCapacity(t, p, size)
}

// TestPoolDoReturnsTheCallbackError checks the plumbing, since Query and Exec
// both ride on it and a swallowed error here would be invisible.
func TestPoolHelpersRoundTrip(t *testing.T) {
	p, _ := newTestPool(t, 2)
	ctx := bg(t)

	res, err := p.Query(ctx, "select 1")
	if err != nil {
		t.Fatalf("Pool.Query: %v", err)
	}
	if len(res.Rows) != 1 || string(res.Rows[0][0]) != "1" {
		t.Errorf("Pool.Query returned %v, want a single row holding \"1\"", res.Rows)
	}

	row, err := p.QueryRow(ctx, "select 1")
	if err != nil {
		t.Fatalf("Pool.QueryRow: %v", err)
	}
	if row == nil || string(row[0]) != "1" {
		t.Errorf("Pool.QueryRow returned %v, want a row holding \"1\"", row)
	}

	// An empty result must come back as a nil row rather than an error, because
	// that is how every caller in the store reports ErrNotFound.
	empty := newFakeServer(t, scripted(emptyResult))
	ep := NewPool(empty.config(), 1, quietLogger())
	t.Cleanup(ep.Close)
	row, err = ep.QueryRow(ctx, "select 1 where false")
	if err != nil {
		t.Fatalf("Pool.QueryRow over an empty result: %v", err)
	}
	if row != nil {
		t.Errorf("Pool.QueryRow returned %v for an empty result, want nil", row)
	}

	if err := p.Exec(ctx, "create table t (id int)"); err != nil {
		t.Fatalf("Pool.Exec: %v", err)
	}

	wantErr := errors.New("the callback said no")
	if err := p.Do(ctx, func(*Conn) error { return wantErr }); err != wantErr {
		t.Errorf("Do returned %v, want the callback's own error", err)
	}
}

// TestNewPoolClampsSizeToAtLeastOne, because a zero-capacity idle channel would
// make the first Acquire block forever.
func TestNewPoolClampsSizeToAtLeastOne(t *testing.T) {
	srv := newFakeServer(t, serveIdle)
	for _, size := range []int{-5, 0, 1} {
		p := NewPool(srv.config(), size, quietLogger())
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		c, err := p.Acquire(ctx)
		cancel()
		if err != nil {
			t.Fatalf("NewPool(size=%d) produced a pool that cannot hand out a single "+
				"connection (%v); a zero-capacity idle channel blocks the first Acquire "+
				"forever", size, err)
		}
		p.Release(c)
		p.Close()
	}
}

// TestPoolWaitReadyGivesUp: an unreachable database must eventually be reported
// rather than retried forever, and the error has to carry the last real failure
// or the operator sees only "never became ready".
func TestPoolWaitReadyGivesUp(t *testing.T) {
	srv := newFakeServer(t, func(f *fakeConn) { _ = f.Close() })
	p := NewPool(srv.config(), 1, quietLogger())
	t.Cleanup(p.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err := p.WaitReady(ctx, 2)
	if err == nil {
		t.Fatal("WaitReady reported a database that never answers as ready")
	}
	if !strings.Contains(err.Error(), "never became ready") {
		t.Errorf("error %q does not explain what happened", err)
	}
	if !strings.Contains(err.Error(), "pg:") {
		t.Errorf("error %q lost the underlying failure", err)
	}
}

func TestPoolWaitReadySucceedsImmediately(t *testing.T) {
	p, srv := newTestPool(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := p.WaitReady(ctx, 3); err != nil {
		t.Fatalf("WaitReady against a healthy server: %v", err)
	}
	if n := srv.accepted.Load(); n != 1 {
		t.Errorf("WaitReady opened %d connections, want 1", n)
	}
}

// TestPoolWaitReadyHonoursItsContext, so a container that is being shut down
// while it waits for a database does not sit through the whole backoff.
func TestPoolWaitReadyHonoursItsContext(t *testing.T) {
	srv := newFakeServer(t, func(f *fakeConn) { _ = f.Close() })
	p := NewPool(srv.config(), 1, quietLogger())
	t.Cleanup(p.Close)

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- p.WaitReady(ctx, 1000) }()

	cancel()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("WaitReady returned success after its context was cancelled")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("WaitReady ignored a cancelled context")
	}
}

// TestPoolCloseIsPromptWithConnectionsStillOut is a timing test on the shutdown
// path. Close used to collect the pool's slots one at a time with a two-second
// fallback on each, and the slots that are not there to collect are precisely the
// ones a request is still using: the busier the process is when SIGTERM arrives,
// the longer it takes to die. A pool of four made that eight seconds, out of the
// ten a container runtime typically allows before SIGKILL, and it was waiting for
// connections that only come back when their request finishes anyway.
func TestPoolCloseIsPromptWithConnectionsStillOut(t *testing.T) {
	const size = 4
	srv := newFakeServer(t, serveIdle)
	p := NewPool(srv.config(), size, quietLogger())
	ctx := bg(t)

	all := make([]*Conn, 0, size)
	for i := 0; i < size; i++ {
		c, err := p.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquiring slot %d: %v", i, err)
		}
		all = append(all, c)
	}
	// Half of them back in the pool and half still out, which is what a shutdown
	// during ordinary traffic looks like.
	pooled, held := all[:2], all[2:]
	for _, c := range pooled {
		p.Release(c)
	}
	waitFor(t, 15*time.Second, "all four connections to be established",
		func() bool { return srv.live.Load() == size })

	start := time.Now()
	p.Close()
	took := time.Since(start)

	// A second is enormously generous for what this does - closing two sockets -
	// and still far under what one missing slot used to cost, let alone two.
	if took > time.Second {
		t.Errorf("Close took %v for a pool of %d with %d connections checked out; it is "+
			"waiting for connections that come back only when their request finishes, "+
			"so a SIGTERM arriving under load has to wait it out", took, size, len(held))
	}

	// Prompt is only half of the bargain: the connections the pool was holding
	// have to be gone, and the ones that were out have to go when they come back.
	waitFor(t, 15*time.Second, "the pooled connections to be closed by Close",
		func() bool { return srv.live.Load() == int64(len(held)) })
	for _, c := range held {
		p.Release(c)
	}
	waitFor(t, 15*time.Second, "the checked-out connections to be closed on release",
		func() bool { return srv.live.Load() == 0 })
}

// strandedIn empties a pool's idle channel and reports how many connections were
// in it. On a closed pool that number must be zero: nothing will ever read from
// that channel again, so a connection left in it is a socket held open until the
// process exits.
func strandedIn(p *Pool) int {
	n := 0
	for {
		select {
		case c := <-p.idle:
			if c != nil {
				n++
			}
		default:
			return n
		}
	}
}

// TestPoolReleaseRacingCloseStrandsNothing pins the window this used to have.
// Release read the closed flag, dropped the mutex, and only then sent the
// connection down the channel; a Close that completed in between left that
// connection in a channel with nobody at the other end. The process usually exits
// straight afterwards and nobody notices - but anything that closes a pool and
// builds another, which is what a configuration reload does, leaks a socket every
// time.
//
// Release now decides where the connection goes and puts it there under one hold
// of the lock Close also takes, which leaves exactly two orders: the connection is
// in the channel before Close drains it, or Release finds the pool already closed
// and shuts the connection itself.
//
// midRelease is what makes that statable. Parking a Release inside its critical
// section and then asking Close to get past it is a question with a definite
// answer; hammering both from goroutines and hoping to land in a window a few
// nanoseconds wide is not - it does not fail on the broken code either, which is
// how this test was first written and why it is not written that way now.
func TestPoolReleaseRacingCloseStrandsNothing(t *testing.T) {
	srv := newFakeServer(t, serveIdle)
	p := NewPool(srv.config(), 2, quietLogger())
	t.Cleanup(p.Close)

	c, err := p.Acquire(bg(t))
	if err != nil {
		t.Fatal(err)
	}

	inRelease := make(chan struct{})
	letGo := make(chan struct{})
	p.midRelease = func() {
		close(inRelease)
		<-letGo
	}

	released := make(chan struct{})
	go func() {
		defer close(released)
		p.Release(c)
	}()
	<-inRelease

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		p.Close()
	}()

	// Close must not be able to finish while a Release is mid-decision, because
	// finishing means having drained a channel this connection is about to go
	// into. Half a second is not proof of a negative, but on the code that had the
	// window it takes microseconds.
	select {
	case <-closed:
		t.Error("Close ran to completion while a Release was deciding where its connection " +
			"goes, so the two are not deciding under the same lock and the connection is " +
			"about to be pooled into a closed pool")
	case <-time.After(500 * time.Millisecond):
	}

	close(letGo)
	<-released
	<-closed

	if n := strandedIn(p); n > 0 {
		t.Errorf("%d connection(s) were pooled into a closed pool, so nothing will ever "+
			"close them; that is a socket held open for the life of the process every time "+
			"a pool is closed and recreated", n)
	}
	waitFor(t, 20*time.Second, "the connection to be closed by whichever of the two got it",
		func() bool { return srv.live.Load() == 0 })
}

// TestPoolReleasingTwiceDoesNotWedgeThePool is why Release makes a non-blocking
// send while holding the mutex rather than simply holding the mutex across a
// blocking one. Widening the lock is what closes the race with Close, but a
// blocking send under it turns a caller's bug - releasing in a defer and again on
// the happy path - from one wedged goroutine into a pool that no goroutine can
// use again, because they all queue behind the mutex the wedged one is holding.
func TestPoolReleasingTwiceDoesNotWedgeThePool(t *testing.T) {
	p, _ := newTestPool(t, 1)
	ctx := bg(t)

	c, err := p.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p.Release(c)

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Release(c)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the second Release never returned; it is parked on a channel that has no " +
			"room while holding the pool's mutex, so every goroutine that touches the " +
			"pool from here on is stuck behind it")
	}

	// The pool is still the size it was and still usable: the extra connection is
	// closed rather than kept, and the slot it came back into revalidates.
	if _, err := p.Query(ctx, "select 1"); err != nil {
		t.Errorf("the pool cannot run a query after a connection was released twice: %v", err)
	}
	held, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquiring the pool's only slot after a double release: %v", err)
	}
	short, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if extra, err := p.Acquire(short); err == nil {
		p.Release(extra)
		t.Error("a double release inflated the pool past its configured size, so it will " +
			"open more connections than the database allows")
	}
	p.Release(held)
}

// TestPoolAcquireStopsWaitingWhenThePoolCloses: Close drains every slot, so an
// Acquire that is waiting for one is waiting for something that will never
// arrive. Both callers here pass a context with no deadline, which is the only
// interesting case - request contexts have one, and that is the sole reason this
// was a stuck goroutine rather than a hung server.
func TestPoolAcquireStopsWaitingWhenThePoolCloses(t *testing.T) {
	t.Run("closed while waiting", func(t *testing.T) {
		p, _ := newTestPool(t, 1)
		held, err := p.Acquire(bg(t))
		if err != nil {
			t.Fatal(err)
		}

		errc := make(chan error, 1)
		go func() {
			_, err := p.Acquire(context.Background())
			errc <- err
		}()

		p.Close()
		select {
		case err := <-errc:
			if !errors.Is(err, ErrPoolClosed) {
				t.Errorf("a waiting Acquire returned %v, want ErrPoolClosed", err)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("an Acquire that was waiting for a slot when the pool closed never " +
				"returned; Close drained every slot, so it is parked on a channel that " +
				"nothing will ever fill again")
		}
		p.Release(held)
	})

	t.Run("closed before asking", func(t *testing.T) {
		p, _ := newTestPool(t, 2)
		p.Close()

		errc := make(chan error, 1)
		go func() {
			_, err := p.Acquire(context.Background())
			errc <- err
		}()
		select {
		case err := <-errc:
			if !errors.Is(err, ErrPoolClosed) {
				t.Errorf("Acquire on a closed pool returned %v, want ErrPoolClosed", err)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("Acquire on an already-closed pool never returned")
		}
	})
}

// TestPoolShutdownDuringADialKeepsNothing covers the shape a SIGTERM takes when
// the database is being slow to answer: the pool closes while a connection is
// half way through its handshake. Whatever the dial then does, the pool must be
// left holding nothing - a connection that arrives after shutdown belongs to
// nobody, and the slot it was dialling into is not worth handing back to a pool
// that will never dial again.
func TestPoolShutdownDuringADialKeepsNothing(t *testing.T) {
	// dialling stages a dial that stops mid-handshake until the test lets it
	// through, so "the pool closed while this was in flight" is a fact rather
	// than a hoped-for interleaving.
	dialling := func(t *testing.T, finish func(*fakeConn)) (*Pool, *fakeServer, chan struct{}, chan error) {
		t.Helper()
		var reached atomic.Int64
		gate := make(chan struct{})
		srv := newFakeServer(t, func(f *fakeConn) {
			reached.Add(1)
			<-gate
			finish(f)
		})
		p := NewPool(srv.config(), 2, quietLogger())
		t.Cleanup(p.Close)

		errc := make(chan error, 1)
		go func() {
			_, err := p.Acquire(context.Background())
			errc <- err
		}()
		waitFor(t, 20*time.Second, "the dial to reach the server",
			func() bool { return reached.Load() == 1 })
		return p, srv, gate, errc
	}

	t.Run("the dial succeeds after the pool has closed", func(t *testing.T) {
		p, srv, gate, errc := dialling(t, func(f *fakeConn) { serveIdle(f) })

		p.Close()
		close(gate)

		select {
		case err := <-errc:
			if !errors.Is(err, ErrPoolClosed) {
				t.Errorf("Acquire returned %v, want ErrPoolClosed: a connection that finishes "+
					"dialling after shutdown must not be handed to a caller, because Close "+
					"has already decided it is not responsible for it", err)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("Acquire never returned after the pool closed under its dial")
		}
		waitFor(t, 20*time.Second, "the connection that arrived too late to be closed",
			func() bool { return srv.live.Load() == 0 })
	})

	t.Run("the dial fails after the pool has closed", func(t *testing.T) {
		p, _, gate, errc := dialling(t, func(f *fakeConn) { _ = f.Close() })

		p.Close()
		close(gate)

		select {
		case err := <-errc:
			if err == nil {
				t.Error("Acquire reported success although the dial failed")
			}
		case <-time.After(20 * time.Second):
			t.Fatal("Acquire never returned after its dial failed against a closed pool")
		}
		if n := strandedIn(p); n != 0 {
			t.Errorf("the pool is holding %d connection(s) after being closed", n)
		}
	})
}
