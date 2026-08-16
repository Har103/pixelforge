package pg

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A scripted backend on a real loopback listener.
//
// The driver takes a net.Conn only from its own dialler, and adding a seam to
// inject one would end up testing the seam. Speaking the v3 protocol back at it
// over TCP costs nothing here - the package already owns an encoder - and it is
// the only way to reach the paths that matter: a socket that dies between two
// bytes of a length prefix, a length prefix that lies, a field count that
// disagrees with the row description. None of those can be produced by a real
// PostgreSQL, which is exactly why they are untested elsewhere.

// fakeServer accepts connections and hands each to a handler goroutine. Every
// handler is joined before the test returns, so handlers may call t.Errorf.
type fakeServer struct {
	t  *testing.T
	ln net.Listener
	wg sync.WaitGroup

	// accepted counts every connection ever made and live tracks how many are
	// open right now, which is how the pool tests observe that the pool really
	// does bound itself and really does reuse rather than redial.
	accepted atomic.Int64
	live     atomic.Int64
	peakLive atomic.Int64

	mu     sync.Mutex
	closed bool
	conns  []net.Conn
}

// newFakeServer starts a listener and serves each accepted connection with
// handler. The listener is closed and every handler joined at test cleanup.
func newFakeServer(t *testing.T, handler func(*fakeConn)) *fakeServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening on loopback: %v", err)
	}
	s := &fakeServer{t: t, ln: ln}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// A handler that waits for a message the driver will never send
			// would otherwise hang the whole package until the test binary
			// times out, hiding which test is at fault.
			_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
			s.mu.Lock()
			s.conns = append(s.conns, conn)
			s.mu.Unlock()
			s.accepted.Add(1)
			if n := s.live.Add(1); n > s.peakLive.Load() {
				s.peakLive.Store(n)
			}
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				defer s.live.Add(-1)
				defer conn.Close()
				handler(&fakeConn{t: t, Conn: conn})
			}()
		}
	}()

	t.Cleanup(s.stop)
	return s
}

func (s *fakeServer) stop() {
	s.mu.Lock()
	already := s.closed
	s.closed = true
	conns := s.conns
	s.conns = nil
	s.mu.Unlock()
	if already {
		s.wg.Wait()
		return
	}
	_ = s.ln.Close()
	// A handler blocked reading from a client that never hangs up would
	// otherwise hold the test open until its own 30 second deadline, which is
	// exactly what a test about a connection nobody released does.
	for _, conn := range conns {
		_ = conn.Close()
	}
	s.wg.Wait()
}

// config returns a Config pointed at this server with TLS switched off, which
// is what every test that is not about TLS wants.
func (s *fakeServer) config() *Config {
	host, port, err := net.SplitHostPort(s.ln.Addr().String())
	if err != nil {
		s.t.Fatalf("splitting listener address %q: %v", s.ln.Addr(), err)
	}
	return &Config{
		Host:            host,
		Port:            port,
		User:            "tester",
		Database:        "testdb",
		SSLMode:         "disable",
		ConnectTimeout:  5 * time.Second,
		ApplicationName: "pixelforge-test",
		RuntimeParams:   map[string]string{},
	}
}

// dial completes a Connect against this server, failing the test if it does not
// succeed. Tests that expect the connect itself to fail call Connect directly.
func (s *fakeServer) dial(t *testing.T) *Conn {
	t.Helper()
	return s.dialCfg(t, s.config())
}

// dialCfg is dial with the config adjusted, for the tests that vary sslmode or
// add runtime parameters.
func (s *fakeServer) dialCfg(t *testing.T, cfg *Config) *Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := Connect(ctx, cfg)
	if err != nil {
		t.Fatalf("connecting to the fake server: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// fakeConn is one accepted connection with helpers for reading frontend
// messages and writing backend ones.
type fakeConn struct {
	t *testing.T
	net.Conn
}

// readStartup consumes the untyped startup packet and returns the parameters it
// carried. An SSLRequest is answered with 'N' and the real startup packet is
// then read, so a handler works whether or not the client tried TLS.
func (f *fakeConn) readStartup() map[string]string {
	f.t.Helper()
	for {
		var head [4]byte
		if _, err := io.ReadFull(f.Conn, head[:]); err != nil {
			f.t.Errorf("fake server: reading startup length: %v", err)
			return nil
		}
		length := binary.BigEndian.Uint32(head[:])
		if length < 8 || length > 1<<20 {
			f.t.Errorf("fake server: implausible startup length %d", length)
			return nil
		}
		body := make([]byte, length-4)
		if _, err := io.ReadFull(f.Conn, body); err != nil {
			f.t.Errorf("fake server: reading startup body: %v", err)
			return nil
		}
		code := binary.BigEndian.Uint32(body[:4])
		if code == sslRequestCode {
			if _, err := f.Conn.Write([]byte{'N'}); err != nil {
				f.t.Errorf("fake server: refusing SSL: %v", err)
				return nil
			}
			continue
		}
		params := map[string]string{}
		r := &readBuf{b: body[4:]}
		for {
			k := r.string()
			if k == "" || r.err != nil {
				break
			}
			params[k] = r.string()
		}
		return params
	}
}

// readMsg reads one typed frontend message. A closed connection comes back as
// an error rather than a test failure, because the driver hanging up is the
// normal end of most handlers.
func (f *fakeConn) readMsg() (byte, []byte, error) {
	var head [5]byte
	if _, err := io.ReadFull(f.Conn, head[:]); err != nil {
		return 0, nil, err
	}
	length := binary.BigEndian.Uint32(head[1:5])
	if length < 4 || length > 1<<24 {
		return 0, nil, errors.New("fake server: implausible frontend message length")
	}
	body := make([]byte, length-4)
	if _, err := io.ReadFull(f.Conn, body); err != nil {
		return 0, nil, err
	}
	return head[0], body, nil
}

// drainUntil reads frontend messages until one of the wanted type arrives, so a
// handler does not have to care whether the driver sent Describe.
func (f *fakeConn) drainUntil(want byte) bool {
	for {
		typ, _, err := f.readMsg()
		if err != nil {
			return false
		}
		if typ == want {
			return true
		}
	}
}

func (f *fakeConn) write(p []byte) {
	f.t.Helper()
	if _, err := f.Conn.Write(p); err != nil {
		// The driver hanging up first is expected in the cut-the-socket tests.
		f.t.Logf("fake server: write: %v", err)
	}
}

// msg frames body as a backend message of the given type.
func msg(typ byte, build func(*writeBuf)) []byte {
	var w writeBuf
	w.start(typ)
	build(&w)
	return append([]byte(nil), w.done()...)
}

func (f *fakeConn) send(typ byte, build func(*writeBuf)) {
	f.t.Helper()
	f.write(msg(typ, build))
}

func (f *fakeConn) authOK() {
	f.t.Helper()
	f.send(msgAuth, func(w *writeBuf) { w.int32(authOK) })
}

func (f *fakeConn) ready(status byte) {
	f.t.Helper()
	f.send(msgReadyForQuery, func(w *writeBuf) { w.byte(status) })
}

// acceptStartup performs the shortest legal successful startup: read the
// packet, say authentication succeeded, report one parameter and a key, then
// go idle.
func (f *fakeConn) acceptStartup() {
	f.t.Helper()
	f.readStartup()
	f.completeStartup()
}

// completeStartup is the reply half of acceptStartup, for handlers that have
// already consumed the startup packet themselves.
func (f *fakeConn) completeStartup() {
	f.t.Helper()
	f.authOK()
	f.send(msgParameterStatus, func(w *writeBuf) {
		w.string("server_version")
		w.string("16.13 (fake)")
	})
	f.send(msgBackendKeyData, func(w *writeBuf) {
		w.int32(4242)
		w.int32(999)
	})
	f.ready('I')
}

func (f *fakeConn) rowDescription(cols ...string) {
	f.t.Helper()
	f.send(msgRowDescription, func(w *writeBuf) {
		w.int16(int16(len(cols)))
		for _, name := range cols {
			w.string(name)
			w.int32(0)  // table OID
			w.int16(0)  // column attribute number
			w.int32(25) // text
			w.int16(-1) // variable width
			w.int32(-1) // no type modifier
			w.int16(0)  // text format
		}
	})
}

// dataRow writes a DataRow. A nil value is sent as the -1 length that means
// SQL NULL.
func (f *fakeConn) dataRow(vals ...[]byte) {
	f.t.Helper()
	f.send(msgDataRow, func(w *writeBuf) {
		w.int16(int16(len(vals)))
		for _, v := range vals {
			if v == nil {
				w.int32(-1)
				continue
			}
			w.int32(int32(len(v)))
			w.raw(v)
		}
	})
}

func (f *fakeConn) commandComplete(tag string) {
	f.t.Helper()
	f.send(msgCommandComplete, func(w *writeBuf) { w.string(tag) })
}

func (f *fakeConn) errorResponse(severity, code, message string) {
	f.t.Helper()
	f.send(msgErrorResponse, func(w *writeBuf) {
		w.byte('S')
		w.string(severity)
		w.byte('C')
		w.string(code)
		w.byte('M')
		w.string(message)
		w.byte(0)
	})
}

// scripted builds a handler that completes startup and then answers query
// rounds with the given replies in order, so a test can say exactly what the
// server puts on the wire - including nothing at all. Rounds past the end of
// the list get an empty result, which keeps "and now use the connection again"
// tests short.
//
// A reply is responsible for everything after the frontend's Sync, ReadyForQuery
// included, because half the interesting cases are the ones that never send it.
func scripted(replies ...func(*fakeConn)) func(*fakeConn) {
	return func(f *fakeConn) {
		f.acceptStartup()
		for round := 0; ; round++ {
			typ, _, err := f.readMsg()
			if err != nil {
				return
			}
			switch typ {
			case 'X':
				return
			case 'P':
				// Bind, Describe, Execute and Sync arrive in the same write.
				if !f.drainUntil('S') {
					return
				}
			case 'Q':
				// The simple query protocol has no Sync.
			default:
				round--
				continue
			}
			if round < len(replies) {
				replies[round](f)
				continue
			}
			f.send(msgParseComplete, func(*writeBuf) {})
			f.send(msgBindComplete, func(*writeBuf) {})
			f.commandComplete("SELECT 0")
			f.ready('I')
		}
	}
}

// emptyResult is the commonest reply: one column, no rows.
func emptyResult(f *fakeConn) {
	f.send(msgParseComplete, func(*writeBuf) {})
	f.send(msgBindComplete, func(*writeBuf) {})
	f.rowDescription("col")
	f.commandComplete("SELECT 0")
	f.ready('I')
}

// serveIdle is the workhorse handler: it completes startup and then answers
// every extended-protocol round with a single one-column row, which is what
// Ping needs, until the client hangs up.
func serveIdle(f *fakeConn) {
	f.acceptStartup()
	for {
		typ, _, err := f.readMsg()
		if err != nil {
			return
		}
		switch typ {
		case 'X':
			return
		case 'P':
			if !f.drainUntil('S') {
				return
			}
			f.send(msgParseComplete, func(*writeBuf) {})
			f.send(msgBindComplete, func(*writeBuf) {})
			f.rowDescription("?column?")
			f.dataRow([]byte("1"))
			f.commandComplete("SELECT 1")
			f.ready('I')
		case 'Q':
			f.commandComplete("SELECT 0")
			f.ready('I')
		}
	}
}
