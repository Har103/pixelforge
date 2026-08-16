package pg

import (
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strconv"
	"strings"
	"testing"
)

// The backend half of SCRAM-SHA-256, complete enough to run a real exchange
// against the client.
//
// It computes with this package's own primitives, which would be circular if
// the point were to check the cryptography - the RFC 7677 vector in pg_test.go
// is what anchors that. The point here is the wiring: that doSASL sends the
// right messages in the right order, reads the reply from the right offsets,
// verifies the client's proof against an independently computed one, and
// refuses to finish the handshake when the server cannot prove it knows the
// stored key. Skipping that last check is a well-worn real-world bug, and it is
// invisible to every test that only ever talks to an honest server.
type scramServer struct {
	t          *testing.T
	password   string
	salt       []byte
	iterations int

	// Faults, each injected at the point a hostile or broken server would
	// introduce it. Zero values mean "behave correctly".
	serverNonce    string
	nonceOverride  func(clientNonce string) string
	rawServerFirst string
	corruptProof   bool
	skipFinal      bool
	finalError     string
	rawFinal       string
}

func newSCRAMServer(t *testing.T, password string) *scramServer {
	return &scramServer{
		t:           t,
		password:    password,
		salt:        []byte("0123456789abcdef"),
		iterations:  4096,
		serverNonce: "SERVERNONCE0123",
	}
}

func (s *scramServer) serve(f *fakeConn) {
	f.readStartup()
	f.send(msgAuth, func(w *writeBuf) {
		w.int32(authSASL)
		w.string(scramSHA256)
		w.byte(0)
	})

	typ, body, err := f.readMsg()
	if err != nil {
		s.t.Errorf("fake server: reading SASLInitialResponse: %v", err)
		return
	}
	if typ != 'p' {
		s.t.Errorf("fake server: expected a password message, got %q", typ)
		return
	}
	r := &readBuf{b: body}
	if mech := r.string(); mech != scramSHA256 {
		s.t.Errorf("fake server: client chose mechanism %q, want %s", mech, scramSHA256)
		return
	}
	clientFirst := string(r.next(int(r.int32())))
	if r.err != nil {
		s.t.Errorf("fake server: decoding SASLInitialResponse: %v", r.err)
		return
	}
	// "n,," is the gs2 header for "this client does not do channel binding".
	if !strings.HasPrefix(clientFirst, "n,,") {
		s.t.Errorf("fake server: client-first-message %q does not start with the gs2 "+
			"header we can handle", clientFirst)
		return
	}
	bare := strings.TrimPrefix(clientFirst, "n,,")
	attrs, err := parseSCRAMAttrs(bare)
	if err != nil {
		s.t.Errorf("fake server: parsing client-first-message: %v", err)
		return
	}
	clientNonce := attrs["r"]
	if clientNonce == "" {
		s.t.Error("fake server: the client sent no nonce")
		return
	}

	combined := clientNonce + s.serverNonce
	if s.nonceOverride != nil {
		combined = s.nonceOverride(clientNonce)
	}
	serverFirst := "r=" + combined +
		",s=" + base64.StdEncoding.EncodeToString(s.salt) +
		",i=" + strconv.Itoa(s.iterations)
	if s.rawServerFirst != "" {
		serverFirst = s.rawServerFirst
	}
	f.send(msgAuth, func(w *writeBuf) {
		w.int32(authSASLContinue)
		w.raw([]byte(serverFirst))
	})

	_, body, err = f.readMsg()
	if err != nil {
		// Expected whenever the client correctly refused to go on.
		return
	}
	clientFinal := string(body)
	withoutProof, proofB64, ok := strings.Cut(clientFinal, ",p=")
	if !ok {
		s.t.Errorf("fake server: client-final-message %q carries no proof", clientFinal)
		return
	}

	salted := pbkdf2SHA256([]byte(saslPrep(s.password)), s.salt, s.iterations, sha256.Size)
	clientKey := hmacSHA256(salted, []byte("Client Key"))
	storedKey := sha256.Sum256(clientKey)
	authMsg := bare + "," + serverFirst + "," + withoutProof
	clientSig := hmacSHA256(storedKey[:], []byte(authMsg))
	wantProof := make([]byte, len(clientKey))
	for i := range clientKey {
		wantProof[i] = clientKey[i] ^ clientSig[i]
	}
	gotProof, decErr := base64.StdEncoding.DecodeString(proofB64)
	if decErr != nil {
		s.t.Errorf("fake server: the client's proof is not base64: %v", decErr)
		return
	}
	// Only meaningful when the exchange was honest; a fault-injecting server has
	// already fed the client a different authMsg on purpose.
	if s.rawServerFirst == "" && s.nonceOverride == nil && !bytes.Equal(gotProof, wantProof) {
		s.t.Errorf("the client's SCRAM proof is wrong\n got %x\nwant %x\n"+
			"the server would reject this login", gotProof, wantProof)
	}
	if !strings.HasPrefix(withoutProof, "c=biws,") {
		s.t.Errorf("client-final-message channel binding is %q, want c=biws (base64 of "+
			"the \"n,,\" header we advertised)", withoutProof)
	}

	switch {
	case s.skipFinal:
		// The bug this models: a server that says "you are in" without ever
		// proving it knows the stored key.
		f.authOK()
	case s.finalError != "":
		f.send(msgAuth, func(w *writeBuf) {
			w.int32(authSASLFinal)
			w.raw([]byte("e=" + s.finalError))
		})
		return
	case s.rawFinal != "":
		f.send(msgAuth, func(w *writeBuf) {
			w.int32(authSASLFinal)
			w.raw([]byte(s.rawFinal))
		})
	default:
		serverKey := hmacSHA256(salted, []byte("Server Key"))
		sig := hmacSHA256(serverKey, []byte(authMsg))
		if s.corruptProof {
			sig[0] ^= 0xff
		}
		f.send(msgAuth, func(w *writeBuf) {
			w.int32(authSASLFinal)
			w.raw([]byte("v=" + base64.StdEncoding.EncodeToString(sig)))
		})
	}

	f.authOK()
	f.send(msgParameterStatus, func(w *writeBuf) {
		w.string("server_version")
		w.string("16.13 (fake)")
	})
	f.ready('I')
}

// TestSCRAMFullExchange is the happy path end to end: the client's proof is
// checked against one the server computed independently from the password, and
// the connection has to come up.
func TestSCRAMFullExchange(t *testing.T) {
	s := newSCRAMServer(t, "correct horse battery staple")
	srv := newFakeServer(t, s.serve)
	cfg := srv.config()
	cfg.Password = s.password

	c, err := connectTo(t, cfg)
	if err != nil {
		t.Fatalf("a correct SCRAM exchange failed: %v", err)
	}
	if c.ServerParam("server_version") == "" {
		t.Error("startup did not continue past SASL to the parameter reports")
	}
}

// TestSCRAMRejectsAServerThatCannotProveItself is the whole reason the exchange
// is mutual. Verifying the server's proof is the step that stops a machine in
// the middle from collecting a login: it can relay the client's proof, but it
// cannot produce the server signature without the stored key. A client that
// skips the check connects happily to an impostor, and the failure is invisible
// - everything works, to the wrong server.
func TestSCRAMRejectsAServerThatCannotProveItself(t *testing.T) {
	cases := []struct {
		name      string
		why       string
		wantInErr string
		setup     func(*scramServer)
	}{
		{
			name:      "the server signature is wrong",
			why:       "an impostor relayed the proof but cannot sign as the server",
			wantInErr: "verifier mismatch",
			setup:     func(s *scramServer) { s.corruptProof = true },
		},
		{
			name:      "the server skips SASLFinal and just says yes",
			why:       "the server declared success without proving anything",
			wantInErr: "SASL final",
			setup:     func(s *scramServer) { s.skipFinal = true },
		},
		{
			name:      "SASLFinal carries no verifier",
			why:       "the final message has attributes but no v=",
			wantInErr: "no verifier",
			setup:     func(s *scramServer) { s.rawFinal = "x=nothing" },
		},
		{
			name:      "the verifier is not base64",
			why:       "v= holds bytes that cannot be decoded",
			wantInErr: "decoding SCRAM verifier",
			setup:     func(s *scramServer) { s.rawFinal = "v=!!!not base64!!!" },
		},
		{
			name:      "the verifier is the right shape but too short",
			why:       "a truncated signature must not compare equal",
			wantInErr: "verifier mismatch",
			setup: func(s *scramServer) {
				s.rawFinal = "v=" + base64.StdEncoding.EncodeToString([]byte{1, 2, 3})
			},
		},
		{
			name:      "the server reports an authentication error",
			why:       "the server said the proof was invalid",
			wantInErr: "invalid-proof",
			setup:     func(s *scramServer) { s.finalError = "invalid-proof" },
		},
		{
			name:      "the combined nonce does not extend ours",
			why:       "the server invented its own nonce instead of extending the client's",
			wantInErr: "does not extend",
			setup: func(s *scramServer) {
				s.nonceOverride = func(string) string { return "entirelyDifferentNonce" }
			},
		},
		{
			name:      "the server sends no nonce at all",
			why:       "server-first-message is missing r=",
			wantInErr: "malformed SCRAM server-first-message",
			setup: func(s *scramServer) {
				s.rawServerFirst = "s=" + base64.StdEncoding.EncodeToString([]byte("0123456789abcdef")) + ",i=4096"
			},
		},
		{
			name:      "the server sends no salt",
			why:       "server-first-message is missing s=",
			wantInErr: "malformed SCRAM server-first-message",
			setup:     func(s *scramServer) { s.rawServerFirst = "r=nonce,i=4096" },
		},
		{
			name:      "the server sends no iteration count",
			why:       "server-first-message is missing i=",
			wantInErr: "malformed SCRAM server-first-message",
			setup: func(s *scramServer) {
				s.rawServerFirst = "r=nonce,s=" + base64.StdEncoding.EncodeToString([]byte("salt"))
			},
		},
		{
			name:      "server-first-message is empty",
			why:       "the server sent nothing but the SASLContinue header",
			wantInErr: "malformed SCRAM server-first-message",
			setup:     func(s *scramServer) { s.rawServerFirst = "," },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newSCRAMServer(t, "hunter2")
			tc.setup(s)
			srv := newFakeServer(t, s.serve)
			cfg := srv.config()
			cfg.Password = s.password

			err := mustFailToConnect(t, cfg, tc.why)
			if !strings.Contains(err.Error(), tc.wantInErr) {
				t.Errorf("error %q does not mention %q", err, tc.wantInErr)
			}
		})
	}
}

// TestSCRAMStepRejectsMalformedServerFirst works on the state machine directly,
// where a case can be expressed in one line and the whole surface can be walked.
func TestSCRAMStepRejectsMalformedServerFirst(t *testing.T) {
	goodSalt := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))

	cases := []struct {
		name        string
		serverFirst func(nonce string) string
		wantInErr   string
	}{
		{
			name:        "an attribute with no equals sign",
			serverFirst: func(n string) string { return "r=" + n + "x,bare,i=4096,s=" + goodSalt },
			wantInErr:   "malformed SCRAM attribute",
		},
		{
			name:        "a multi-character attribute key",
			serverFirst: func(n string) string { return "rr=" + n + ",s=" + goodSalt + ",i=4096" },
			wantInErr:   "malformed SCRAM attribute",
		},
		{
			name:        "a salt that is not base64",
			serverFirst: func(n string) string { return "r=" + n + "x,s=@@@@,i=4096" },
			wantInErr:   "decoding SCRAM salt",
		},
		{
			name:        "an iteration count that is not a number",
			serverFirst: func(n string) string { return "r=" + n + "x,s=" + goodSalt + ",i=lots" },
			wantInErr:   "bad SCRAM iteration count",
		},
		{
			name:        "an iteration count of zero",
			serverFirst: func(n string) string { return "r=" + n + "x,s=" + goodSalt + ",i=0" },
			wantInErr:   "bad SCRAM iteration count",
		},
		{
			name:        "a negative iteration count",
			serverFirst: func(n string) string { return "r=" + n + "x,s=" + goodSalt + ",i=-1" },
			wantInErr:   "bad SCRAM iteration count",
		},
		{
			// Without a ceiling a single server-first-message pins a core for
			// minutes, which is a denial of service the server gets for free.
			name:        "an iteration count chosen to burn CPU",
			serverFirst: func(n string) string { return "r=" + n + "x,s=" + goodSalt + ",i=2000000000" },
			wantInErr:   "refusing SCRAM iteration count",
		},
		{
			name:        "an empty message",
			serverFirst: func(string) string { return "" },
			wantInErr:   "malformed SCRAM server-first-message",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := newSCRAM("pencil")
			if err != nil {
				t.Fatal(err)
			}
			out, err := c.step([]byte(tc.serverFirst(c.nonce)))
			if err == nil {
				t.Fatalf("step accepted the message and produced %q; the client would go "+
					"on to send a proof derived from data it could not validate", out)
			}
			if !strings.Contains(err.Error(), tc.wantInErr) {
				t.Errorf("error %q does not mention %q", err, tc.wantInErr)
			}
		})
	}
}

// TestSCRAMIterationCeilingBoundary pins both sides of the limit, so the guard
// cannot drift into rejecting counts a real server uses. PostgreSQL defaults to
// 4096 and lets an administrator raise it.
func TestSCRAMIterationCeilingBoundary(t *testing.T) {
	goodSalt := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))
	for _, tc := range []struct {
		iters   int
		wantErr bool
	}{
		{1, false},
		{4096, false},
		{1_000_000, false},
		{1_000_001, true},
	} {
		c, err := newSCRAM("pencil")
		if err != nil {
			t.Fatal(err)
		}
		msg := "r=" + c.nonce + "x,s=" + goodSalt + ",i=" + strconv.Itoa(tc.iters)
		_, err = c.step([]byte(msg))
		if tc.wantErr && err == nil {
			t.Errorf("i=%d was accepted, above the ceiling", tc.iters)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("i=%d was refused (%v), but a server may legitimately use it", tc.iters, err)
		}
	}
}

// TestSCRAMVerifyRejectsEverythingButTheRightSignature covers the final check on
// its own, including the empty and absent cases that a lenient implementation
// would let through.
func TestSCRAMVerifyRejectsEverythingButTheRightSignature(t *testing.T) {
	goodSalt := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))

	fresh := func(t *testing.T) *scramClient {
		t.Helper()
		c, err := newSCRAM("pencil")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.step([]byte("r=" + c.nonce + "x,s=" + goodSalt + ",i=4096")); err != nil {
			t.Fatal(err)
		}
		return c
	}

	for _, tc := range []struct{ name, serverFinal string }{
		{"empty", ""},
		{"no attributes at all", ","},
		{"an empty verifier", "v="},
		{"the wrong signature", "v=" + base64.StdEncoding.EncodeToString(make([]byte, 32))},
		{"a signature of the wrong length", "v=" + base64.StdEncoding.EncodeToString(make([]byte, 16))},
		{"an error attribute", "e=other-error"},
		{"an unrelated attribute", "x=hello"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := fresh(t).verify([]byte(tc.serverFinal)); err == nil {
				t.Errorf("verify accepted %q; the client would treat an unauthenticated "+
					"server as authenticated", tc.serverFinal)
			}
		})
	}
}

// TestSCRAMChannelBindingIsAdvertisedConsistently: the client says "n,," in the
// gs2 header and must repeat the same claim as c=biws in the final message,
// because the server hashes both. Sending a channel-binding value we never
// negotiated is how a downgrade slips through.
func TestSCRAMChannelBindingIsAdvertisedConsistently(t *testing.T) {
	c, err := newSCRAM("pencil")
	if err != nil {
		t.Fatal(err)
	}
	if got := string(c.first()); !strings.HasPrefix(got, "n,,") {
		t.Errorf("client-first-message = %q, want the \"n,,\" gs2 header", got)
	}
	goodSalt := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))
	final, err := c.step([]byte("r=" + c.nonce + "x,s=" + goodSalt + ",i=4096"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(final), "c=biws,") {
		t.Errorf("client-final-message = %q, want it to open with c=biws, the base64 of "+
			"the header we advertised", final)
	}
	// A server-supplied channel-binding attribute is not something we asked for
	// and must not change what we send.
	c2, _ := newSCRAM("pencil")
	final2, err := c2.step([]byte("r=" + c2.nonce + "x,s=" + goodSalt + ",i=4096,c=tls-server-end-point"))
	if err != nil {
		t.Fatalf("a stray channel-binding attribute broke the exchange: %v", err)
	}
	if !strings.HasPrefix(string(final2), "c=biws,") {
		t.Errorf("a server-sent channel-binding attribute changed our reply to %q", final2)
	}
}

// TestSCRAMNoncesAreNotReused: two clients must not derive the same nonce, or
// two sessions share an authentication message and a captured proof replays.
func TestSCRAMNoncesAreFresh(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		c, err := newSCRAM("pencil")
		if err != nil {
			t.Fatal(err)
		}
		if c.nonce == "" {
			t.Fatal("the generated nonce is empty")
		}
		if seen[c.nonce] {
			t.Fatalf("nonce %q was generated twice in 64 tries; proofs from one session "+
				"can be replayed into another", c.nonce)
		}
		seen[c.nonce] = true
		if strings.ContainsAny(c.nonce, ",") {
			t.Errorf("nonce %q contains a comma, which is the SCRAM attribute separator "+
				"and would split the message", c.nonce)
		}
	}
}

// TestMD5AuthenticationMatchesTheServer runs the MD5 exchange against a server
// that checks the answer, which is the only way to know the salt is being
// applied in the right order.
func TestMD5AuthenticationMatchesTheServer(t *testing.T) {
	const (
		user     = "tester"
		password = "s3cret"
	)
	salt := []byte{0xde, 0xad, 0xbe, 0xef}

	srv := newFakeServer(t, func(f *fakeConn) {
		f.readStartup()
		f.send(msgAuth, func(w *writeBuf) {
			w.int32(authMD5Password)
			w.raw(salt)
		})
		typ, body, err := f.readMsg()
		if err != nil || typ != 'p' {
			f.t.Errorf("fake server: expected a password message, got %q (%v)", typ, err)
			return
		}
		got := string(bytes.TrimRight(body, "\x00"))

		inner := md5.Sum([]byte(password + user))
		outer := md5.Sum(append([]byte(hex.EncodeToString(inner[:])), salt...))
		want := "md5" + hex.EncodeToString(outer[:])
		if got != want {
			f.t.Errorf("the client sent MD5 response %q, want %q; the server would reject "+
				"this login", got, want)
		}
		f.completeStartup()
	})

	cfg := srv.config()
	cfg.User = user
	cfg.Password = password
	if _, err := connectTo(t, cfg); err != nil {
		t.Fatalf("MD5 authentication failed: %v", err)
	}
}

// TestCleartextAuthenticationSendsThePassword covers the simplest method, which
// is what a local trust-with-password setup and some proxies still use.
func TestCleartextAuthenticationSendsThePassword(t *testing.T) {
	const password = "pässword with spaces"
	srv := newFakeServer(t, func(f *fakeConn) {
		f.readStartup()
		f.send(msgAuth, func(w *writeBuf) { w.int32(authCleartextPassword) })
		typ, body, err := f.readMsg()
		if err != nil || typ != 'p' {
			f.t.Errorf("fake server: expected a password message, got %q (%v)", typ, err)
			return
		}
		if got := string(bytes.TrimRight(body, "\x00")); got != password {
			f.t.Errorf("the client sent %q, want %q", got, password)
		}
		f.completeStartup()
	})

	cfg := srv.config()
	cfg.Password = password
	if _, err := connectTo(t, cfg); err != nil {
		t.Fatalf("cleartext authentication failed: %v", err)
	}
}

// TestSASLPrepEdges pins the documented partial SASLprep. It is deliberately
// incomplete, so the tests have to say which parts are load-bearing: control
// characters are stripped because they would corrupt the attribute encoding,
// and everything else passes through so an existing password keeps working.
func TestSASLPrepEdges(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"plain ASCII is untouched", "hunter2", "hunter2"},
		{"empty stays empty", "", ""},
		{"a NUL is removed", "pass\x00word", "password"},
		{"a newline is removed", "pass\nword", "password"},
		{"a tab is removed", "pass\tword", "password"},
		{"DEL is removed", "pass\x7fword", "password"},
		{"a space is kept", "pass word", "pass word"},
		{"punctuation is kept", "p@ss,w=rd", "p@ss,w=rd"},
		{"non-ASCII passes through whole", "pässwörd", "pässwörd"},
		{"non-ASCII keeps its control characters", "pä\x01ss", "pä\x01ss"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := saslPrep(tc.in); got != tc.want {
				t.Errorf("saslPrep(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestPBKDF2AgainstRFC6070 uses the published HMAC-SHA-256 vectors rather than
// this package's own output, so the derivation is anchored to something outside
// the repository.
func TestPBKDF2KnownVectors(t *testing.T) {
	cases := []struct {
		password, salt string
		iter, length   int
		wantHex        string
	}{
		{"password", "salt", 1, 32,
			"120fb6cffcf8b32c43e7225256c4f837a86548c92ccc35480805987cb70be17b"},
		{"password", "salt", 2, 32,
			"ae4d0c95af6b46d32d0adff928f06dd02a303f8ef3c251dfd6e2d85a95474c43"},
		{"password", "salt", 4096, 32,
			"c5e478d59288c841aa530db6845c4c8d962893a001ce4e11a4963873aa98134a"},
		{"passwordPASSWORDpassword", "saltSALTsaltSALTsaltSALTsaltSALTsalt", 4096, 40,
			"348c89dbcbd32b2f32d814b8116e84cf2b17347ebc1800181c4e2a1fb8dd53e1c635518c7dac47e9"},
	}
	for _, tc := range cases {
		got := pbkdf2SHA256([]byte(tc.password), []byte(tc.salt), tc.iter, tc.length)
		if hex.EncodeToString(got) != tc.wantHex {
			t.Errorf("pbkdf2(%q, %q, %d, %d)\n got %x\nwant %s",
				tc.password, tc.salt, tc.iter, tc.length, got, tc.wantHex)
		}
	}
}
