package pg

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// connectTo dials cfg with a bounded context and returns whatever Connect did,
// so a test can assert on the failure instead of hanging when there is none.
func connectTo(t *testing.T, cfg *Config) (*Conn, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := Connect(ctx, cfg)
	if c != nil {
		t.Cleanup(func() { _ = c.Close() })
	}
	return c, err
}

// mustFailToConnect asserts that Connect refused, which is the only safe
// outcome for every scripted failure below: a Conn handed back after a broken
// handshake would be used for real queries.
func mustFailToConnect(t *testing.T, cfg *Config, why string) error {
	t.Helper()
	c, err := connectTo(t, cfg)
	if err == nil {
		t.Fatalf("Connect succeeded although %s; the caller would go on to run "+
			"queries over a connection that was never established", why)
	}
	if c != nil {
		t.Errorf("Connect returned both a connection and the error %v", err)
	}
	return err
}

// TestConnectSurvivesTheServerVanishing walks the handshake and cuts the socket
// at each step. Managed databases restart, load balancers reap idle sockets and
// a deploy can drop a backend mid-handshake; every one of these has to come back
// as an error rather than a half-built Conn.
func TestConnectSurvivesTheServerVanishing(t *testing.T) {
	cases := []struct {
		name    string
		why     string
		handler func(*fakeConn)
	}{
		{
			name: "hangs up before reading anything",
			why:  "the server closed the socket without reading the startup packet",
			handler: func(f *fakeConn) {
				_ = f.Close()
			},
		},
		{
			name: "hangs up after the startup packet",
			why:  "the server read the startup packet and then vanished",
			handler: func(f *fakeConn) {
				f.readStartup()
				_ = f.Close()
			},
		},
		{
			name: "hangs up inside the authentication message",
			why:  "the authentication message arrived only half-written",
			handler: func(f *fakeConn) {
				f.readStartup()
				full := msg(msgAuth, func(w *writeBuf) { w.int32(authOK) })
				f.write(full[:3])
				_ = f.Close()
			},
		},
		{
			name: "hangs up after AuthenticationOk",
			why:  "the server authenticated us and then died before ReadyForQuery",
			handler: func(f *fakeConn) {
				f.readStartup()
				f.authOK()
				_ = f.Close()
			},
		},
		{
			name: "hangs up after BackendKeyData",
			why:  "the server died between the key data and ReadyForQuery",
			handler: func(f *fakeConn) {
				f.readStartup()
				f.authOK()
				f.send(msgBackendKeyData, func(w *writeBuf) { w.int32(1); w.int32(2) })
				_ = f.Close()
			},
		},
		{
			name: "sends a ReadyForQuery header and stops",
			why:  "ReadyForQuery was cut off before its status byte",
			handler: func(f *fakeConn) {
				f.readStartup()
				f.authOK()
				f.write(header(msgReadyForQuery, 5))
				_ = f.Close()
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newFakeServer(t, tc.handler)
			mustFailToConnect(t, srv.config(), tc.why)
		})
	}
}

// TestConnectRejectsHostileStartupMessages covers the bytes a compromised or
// buggy server could put on the wire during the one exchange that happens
// before any authentication has been proved.
func TestConnectRejectsHostileStartupMessages(t *testing.T) {
	cases := []struct {
		name      string
		why       string
		wantInErr string
		handler   func(*fakeConn)
	}{
		{
			name:      "an authentication method we do not implement",
			why:       "the server demanded GSSAPI",
			wantInErr: "unsupported authentication method",
			handler: func(f *fakeConn) {
				f.readStartup()
				f.send(msgAuth, func(w *writeBuf) { w.int32(7) })
			},
		},
		{
			name:      "an Authentication message with no body",
			why:       "the authentication code was truncated away entirely",
			wantInErr: "pg:",
			handler: func(f *fakeConn) {
				f.readStartup()
				f.send(msgAuth, func(*writeBuf) {})
				f.ready('I')
			},
		},
		{
			name:      "an Authentication message with a short code",
			why:       "the authentication code was cut to two bytes",
			wantInErr: "pg:",
			handler: func(f *fakeConn) {
				f.readStartup()
				f.send(msgAuth, func(w *writeBuf) { w.int16(0) })
				f.ready('I')
			},
		},
		{
			name:      "an MD5 challenge with no salt",
			why:       "the four salt bytes were missing",
			wantInErr: "truncated",
			handler: func(f *fakeConn) {
				f.readStartup()
				f.send(msgAuth, func(w *writeBuf) { w.int32(authMD5Password) })
			},
		},
		{
			name:      "a SASL offer with no mechanisms",
			why:       "the server offered SASL but listed nothing we speak",
			wantInErr: "SCRAM-SHA-256",
			handler: func(f *fakeConn) {
				f.readStartup()
				f.send(msgAuth, func(w *writeBuf) { w.int32(authSASL); w.byte(0) })
			},
		},
		{
			name:      "a SASL offer of a mechanism we do not speak",
			why:       "the server only offered SCRAM-SHA-256-PLUS",
			wantInErr: "SCRAM-SHA-256",
			handler: func(f *fakeConn) {
				f.readStartup()
				f.send(msgAuth, func(w *writeBuf) {
					w.int32(authSASL)
					w.string("SCRAM-SHA-256-PLUS")
					w.byte(0)
				})
			},
		},
		{
			name:      "a SASL continue arriving before the exchange started",
			why:       "the server jumped straight to SASLContinue",
			wantInErr: "outside the SASL exchange",
			handler: func(f *fakeConn) {
				f.readStartup()
				f.send(msgAuth, func(w *writeBuf) { w.int32(authSASLContinue) })
			},
		},
		{
			name:      "a message type that has no meaning during startup",
			why:       "the server sent a DataRow before ReadyForQuery",
			wantInErr: "unexpected message",
			handler: func(f *fakeConn) {
				f.readStartup()
				f.dataRow([]byte("surprise"))
			},
		},
		{
			name:      "a demand for a protocol version we do not speak",
			why:       "the server answered with NegotiateProtocolVersion",
			wantInErr: "protocol version",
			handler: func(f *fakeConn) {
				f.readStartup()
				f.send(msgNegotiateProtocol, func(w *writeBuf) { w.int32(0); w.int32(0) })
			},
		},
		{
			name:      "a message claiming more bytes than any sane server sends",
			why:       "the server claimed a 512 MiB startup message",
			wantInErr: "exceeds limit",
			handler: func(f *fakeConn) {
				f.readStartup()
				f.write(header(msgAuth, 512<<20))
				_ = f.Close()
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newFakeServer(t, tc.handler)
			err := mustFailToConnect(t, srv.config(), tc.why)
			if !strings.Contains(err.Error(), tc.wantInErr) {
				t.Errorf("error %q does not mention %q, so an operator reading the log "+
					"cannot tell what the server did", err, tc.wantInErr)
			}
		})
	}
}

// TestConnectSurfacesServerErrorResponse is the ordinary "wrong password" path:
// the failure has to arrive as a *pg.Error carrying its SQLSTATE, because
// callers branch on 28P01 to tell bad credentials from an unreachable host.
func TestConnectSurfacesServerErrorResponse(t *testing.T) {
	srv := newFakeServer(t, func(f *fakeConn) {
		f.readStartup()
		f.errorResponse("FATAL", "28P01", `password authentication failed for user "tester"`)
	})
	err := mustFailToConnect(t, srv.config(), "the server rejected the password")

	var pgErr *Error
	if !errors.As(err, &pgErr) {
		t.Fatalf("error %T (%v) is not a *pg.Error, so SQLSTATE is unreachable", err, err)
	}
	if pgErr.SQLState() != "28P01" {
		t.Errorf("SQLSTATE = %q, want 28P01", pgErr.SQLState())
	}
	if !strings.Contains(pgErr.Error(), "password authentication failed") {
		t.Errorf("error text lost the server's message: %s", pgErr.Error())
	}
}

// TestConnectPinsTheTextFormatsWeDecode guards a coupling that is invisible from
// either side on its own: values.go parses timestamps with a fixed set of ISO
// layouts and bytea as hex, and both only hold because the startup packet pins
// DateStyle and client_encoding. Drop either and every timestamp in the product
// starts failing to parse on a server with a different locale.
func TestConnectPinsTheTextFormatsWeDecode(t *testing.T) {
	got := make(chan map[string]string, 1)
	srv := newFakeServer(t, func(f *fakeConn) {
		got <- f.readStartup()
		f.completeStartup()
	})

	c := srv.dial(t)
	params := <-got

	for k, want := range map[string]string{
		"client_encoding":  "UTF8",
		"DateStyle":        "ISO, MDY",
		"user":             "tester",
		"database":         "testdb",
		"application_name": "pixelforge-test",
	} {
		if params[k] != want {
			t.Errorf("startup parameter %s = %q, want %q", k, params[k], want)
		}
	}
	if v := c.ServerParam("server_version"); v != "16.13 (fake)" {
		t.Errorf("ServerParam(server_version) = %q; ParameterStatus was not recorded", v)
	}
}

// TestConnectSendsExtraRuntimeParameters checks that unknown DSN options reach
// the server, which is how options like search_path are meant to be passed.
func TestConnectSendsExtraRuntimeParameters(t *testing.T) {
	got := make(chan map[string]string, 1)
	srv := newFakeServer(t, func(f *fakeConn) {
		got <- f.readStartup()
		f.completeStartup()
	})
	cfg := srv.config()
	cfg.RuntimeParams["search_path"] = "pixelforge,public"

	srv.dialCfg(t, cfg)
	if params := <-got; params["search_path"] != "pixelforge,public" {
		t.Errorf("search_path = %q, want it forwarded to the server", params["search_path"])
	}
}

// TestNegotiateTLS covers the SSLRequest handshake without a certificate, which
// is where the mode decisions are made. The reply byte is the only thing the
// client has to go on, so each possible reply gets its own case.
func TestNegotiateTLS(t *testing.T) {
	cases := []struct {
		name      string
		mode      string
		reply     byte
		wantErr   bool
		wantInErr string
	}{
		{name: "prefer accepts a refusal", mode: "prefer", reply: 'N'},
		{name: "require will not go unencrypted", mode: "require", reply: 'N',
			wantErr: true, wantInErr: "refused TLS"},
		{name: "verify-full will not go unencrypted", mode: "verify-full", reply: 'N',
			wantErr: true, wantInErr: "refused TLS"},
		{name: "an error reply is fatal", mode: "prefer", reply: 'E',
			wantErr: true, wantInErr: "rejected SSLRequest"},
		{name: "a nonsense reply is fatal", mode: "prefer", reply: 'X',
			wantErr: true, wantInErr: "unexpected SSLRequest reply"},
		{name: "an unknown sslmode is refused rather than guessed", mode: "banana", reply: 'S',
			wantErr: true, wantInErr: "unknown sslmode"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newFakeServer(t, func(f *fakeConn) {
				var head [8]byte
				if _, err := io.ReadFull(f.Conn, head[:]); err != nil {
					return
				}
				f.write([]byte{tc.reply})
				if tc.wantErr {
					// The client is expected to give up here, so waiting for a
					// startup packet would only deadlock the handler.
					_ = f.Close()
					return
				}
				f.readStartup()
				f.completeStartup()
			})

			cfg := srv.config()
			cfg.SSLMode = tc.mode
			c, err := connectTo(t, cfg)
			switch {
			case tc.wantErr && err == nil:
				t.Fatalf("sslmode=%s with reply %q connected anyway; the caller believes "+
					"the session is protected when it is not", tc.mode, tc.reply)
			case tc.wantErr:
				if !strings.Contains(err.Error(), tc.wantInErr) {
					t.Errorf("error %q does not mention %q", err, tc.wantInErr)
				}
			case err != nil:
				t.Fatalf("sslmode=%s with reply %q should have connected: %v", tc.mode, tc.reply, err)
			default:
				_ = c.Close()
			}
		})
	}
}

// TestNegotiateTLSDisabledSendsNoSSLRequest makes sure sslmode=disable really
// skips the probe, rather than sending it and ignoring the answer.
func TestNegotiateTLSDisabledSendsNoSSLRequest(t *testing.T) {
	first := make(chan uint32, 1)
	srv := newFakeServer(t, func(f *fakeConn) {
		var head [8]byte
		if _, err := io.ReadFull(f.Conn, head[:]); err != nil {
			f.t.Errorf("fake server: reading the first packet: %v", err)
			return
		}
		first <- uint32(head[4])<<24 | uint32(head[5])<<16 | uint32(head[6])<<8 | uint32(head[7])
		// The rest of the startup packet is already on the way; drain and accept.
		length := uint32(head[0])<<24 | uint32(head[1])<<16 | uint32(head[2])<<8 | uint32(head[3])
		rest := make([]byte, length-8)
		_, _ = io.ReadFull(f.Conn, rest)
		f.completeStartup()
	})

	cfg := srv.config()
	cfg.SSLMode = "disable"
	srv.dialCfg(t, cfg)

	if code := <-first; code != protocolVersion {
		t.Errorf("the first packet carried code %d, want the protocol version %d; "+
			"sslmode=disable must not probe for TLS", code, protocolVersion)
	}
}

// TestConnectHonoursContextCancellation checks that a caller giving up during a
// connect is obeyed. A pool acquiring under a request context depends on it.
func TestConnectHonoursContextCancellation(t *testing.T) {
	// A listener with a full accept backlog is unreliable to arrange; a server
	// that accepts and then says nothing is the same situation from the client's
	// point of view and is deterministic.
	srv := newFakeServer(t, func(f *fakeConn) {
		f.readStartup()
		<-time.After(20 * time.Second)
	})

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		c, err := Connect(ctx, srv.config())
		if c != nil {
			_ = c.Close()
		}
		errc <- err
	}()

	cancel()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("Connect succeeded after its context was cancelled")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Connect ignored a cancelled context and was still blocked 15s later")
	}
}

// TestCloseSendsTerminate proves the polite shutdown happens, because a server
// that never sees Terminate keeps the backend around until it notices the FIN.
func TestCloseSendsTerminate(t *testing.T) {
	saw := make(chan byte, 4)
	srv := newFakeServer(t, func(f *fakeConn) {
		f.acceptStartup()
		for {
			typ, _, err := f.readMsg()
			if err != nil {
				close(saw)
				return
			}
			saw <- typ
		}
	})

	c := srv.dial(t)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	typ, ok := <-saw
	if !ok {
		t.Fatal("the connection was closed without sending Terminate")
	}
	if typ != 'X' {
		t.Errorf("the last message was %q, want 'X' (Terminate)", typ)
	}
}

// TestCloseIsIdempotent matters because Pool.discard and a caller's defer can
// both reach the same connection.
func TestCloseIsIdempotent(t *testing.T) {
	srv := newFakeServer(t, serveIdle)
	c := srv.dial(t)
	if err := c.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("second Close returned %v, want nil", err)
	}
}

// TestUseAfterCloseIsAnErrorNotAPanic is a regression test for a nil pointer
// dereference: Close sets raw to nil and applyDeadline dereferenced it without
// checking, so any query on a closed connection took the process down. A pool
// that hands back a connection it has already discarded, or a caller whose defer
// ordering is wrong, is a bug worth an error - not a crash in an unrelated
// goroutine.
func TestUseAfterCloseIsAnErrorNotAPanic(t *testing.T) {
	srv := newFakeServer(t, serveIdle)

	ops := map[string]func(*Conn) error{
		"Exec":  func(c *Conn) error { return c.Exec(context.Background(), "select 1") },
		"Query": func(c *Conn) error { _, err := c.Query(context.Background(), "select 1"); return err },
		"Ping":  func(c *Conn) error { return c.Ping(context.Background()) },
		"QueryRow": func(c *Conn) error {
			_, err := c.QueryRow(context.Background(), "select 1")
			return err
		},
	}
	for name, op := range ops {
		t.Run(name, func(t *testing.T) {
			c := srv.dial(t)
			if err := c.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("%s on a closed connection panicked with %v; a misuse bug "+
						"must not take the process down", name, r)
				}
			}()
			err := op(c)
			if err == nil {
				t.Fatalf("%s on a closed connection returned no error", name)
			}
			if !strings.Contains(err.Error(), "closed") {
				t.Errorf("%s returned %q, which does not tell the caller the connection "+
					"was already closed", name, err)
			}
		})
	}
}

// TestBrokenConnectionSkipsTerminate: writing Terminate to a socket we already
// know is dead just produces a second error to swallow.
func TestBrokenConnectionIsReported(t *testing.T) {
	srv := newFakeServer(t, func(f *fakeConn) {
		f.acceptStartup()
		_, _, _ = f.readMsg()
		_ = f.Close()
	})

	c := srv.dial(t)
	if c.Broken() {
		t.Fatal("a freshly connected Conn reports itself broken")
	}
	if _, err := c.Query(context.Background(), "select 1"); err == nil {
		t.Fatal("a query against a server that hung up returned no error")
	}
	if !c.Broken() {
		t.Error("Broken() is false after the socket died; the pool would hand this " +
			"connection to the next caller")
	}
}

// TestConnectRefusedIsAnError is the plainest failure there is, and the message
// has to name the address or an operator cannot tell which host is down.
func TestConnectRefusedIsAnError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	host, port, _ := net.SplitHostPort(addr)
	cfg := &Config{Host: host, Port: port, User: "u", SSLMode: "disable", ConnectTimeout: 2 * time.Second}
	err = mustFailToConnect(t, cfg, "nothing is listening on that port")
	if !strings.Contains(err.Error(), host) || !strings.Contains(err.Error(), port) {
		t.Errorf("error %q does not name the address it failed to reach", err)
	}
}
