package loadtest

import (
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// dbProxy sits between the server and PostgreSQL so an experiment can make the
// database slow, or make it disappear, without touching PostgreSQL itself.
//
// It is a plain TCP relay in the same spirit as the rest of the project: no
// protocol awareness at all, which is what makes it trustworthy. It does not
// know a Parse from a Bind, so it cannot accidentally repair or corrupt
// anything; it moves bytes, optionally late, or not at all.
//
// Latency is applied per chunk in both directions. That models a distant
// database rather than a busy one - a busy database would also serialise - but
// it is enough to push the write-behind loop into its slow path, which is the
// thing under test.
type dbProxy struct {
	ln      net.Listener
	target  string
	stopped chan struct{}

	mu      sync.Mutex
	conns   map[net.Conn]struct{}
	latency time.Duration
	cut     bool

	accepted atomic.Int64
	refused  atomic.Int64
	bytesIn  atomic.Int64
	bytesOut atomic.Int64

	wg sync.WaitGroup
}

// newDBProxy listens on a loopback port and forwards to target ("host:port").
func newDBProxy(target string) (*dbProxy, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &dbProxy{
		ln:      ln,
		target:  target,
		stopped: make(chan struct{}),
		conns:   map[net.Conn]struct{}{},
	}
	p.wg.Add(1)
	go func() { defer p.wg.Done(); p.serve() }()
	return p, nil
}

// addr is what to put in a DSN so the server talks through the proxy.
func (p *dbProxy) addr() string { return p.ln.Addr().String() }

func (p *dbProxy) dsn(user, pass, db string) string {
	return fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable", user, pass, p.addr(), db)
}

// SetLatency adds a one-way delay to every chunk in both directions, so a round
// trip costs roughly twice this.
func (p *dbProxy) SetLatency(d time.Duration) {
	p.mu.Lock()
	p.latency = d
	p.mu.Unlock()
}

// Cut severs the database: existing connections are closed and new ones are
// refused. Closing rather than blackholing is deliberate - it is what a
// restarted or failed-over database does, and it makes the server's error path
// run immediately rather than after a fifteen second timeout.
func (p *dbProxy) Cut() {
	p.mu.Lock()
	p.cut = true
	conns := make([]net.Conn, 0, len(p.conns))
	for c := range p.conns {
		conns = append(conns, c)
	}
	p.conns = map[net.Conn]struct{}{}
	p.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

// Restore brings the database back.
func (p *dbProxy) Restore() {
	p.mu.Lock()
	p.cut = false
	p.mu.Unlock()
}

func (p *dbProxy) isCut() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cut
}

func (p *dbProxy) delay() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.latency
}

func (p *dbProxy) Close() {
	select {
	case <-p.stopped:
		return
	default:
	}
	close(p.stopped)
	_ = p.ln.Close()
	p.Cut()
	p.wg.Wait()
}

func (p *dbProxy) serve() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			select {
			case <-p.stopped:
				return
			default:
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return
		}
		if p.isCut() {
			p.refused.Add(1)
			_ = c.Close()
			continue
		}
		p.accepted.Add(1)
		p.wg.Add(1)
		go func() { defer p.wg.Done(); p.handle(c) }()
	}
}

func (p *dbProxy) handle(client net.Conn) {
	defer client.Close()

	upstream, err := net.DialTimeout("tcp", p.target, 10*time.Second)
	if err != nil {
		return
	}
	defer upstream.Close()

	p.track(client)
	p.track(upstream)
	defer p.untrack(client)
	defer p.untrack(upstream)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); p.pipe(upstream, client, &p.bytesIn); upstream.Close() }()
	go func() { defer wg.Done(); p.pipe(client, upstream, &p.bytesOut); client.Close() }()
	wg.Wait()
}

// pipe copies one direction, sleeping the configured latency before each chunk
// it forwards. Reading the delay per chunk rather than per connection is what
// lets an experiment change it while traffic is flowing.
func (p *dbProxy) pipe(dst, src net.Conn, counter *atomic.Int64) {
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if d := p.delay(); d > 0 {
				select {
				case <-time.After(d):
				case <-p.stopped:
					return
				}
			}
			if p.isCut() {
				return
			}
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
			counter.Add(int64(n))
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return
			}
			return
		}
	}
}

func (p *dbProxy) track(c net.Conn) {
	p.mu.Lock()
	if p.cut {
		p.mu.Unlock()
		_ = c.Close()
		return
	}
	p.conns[c] = struct{}{}
	p.mu.Unlock()
}

func (p *dbProxy) untrack(c net.Conn) {
	p.mu.Lock()
	delete(p.conns, c)
	p.mu.Unlock()
}

func (p *dbProxy) stats() string {
	return fmt.Sprintf("accepted=%d refused=%d bytes(server->db)=%d bytes(db->server)=%d",
		p.accepted.Load(), p.refused.Load(), p.bytesOut.Load(), p.bytesIn.Load())
}
