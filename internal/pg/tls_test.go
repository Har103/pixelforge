package pg

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"
)

// TLS negotiation with a real handshake.
//
// The mode decisions in negotiateTLS are the difference between an encrypted
// session and one an intermediary can read, and between checking who the server
// is and taking its word for it. None of that can be exercised without a
// certificate, so these tests generate one - stdlib only, which is the whole
// point of the exercise.

// testCert mints a self-signed certificate for the given name. It is not signed
// by anything in the system trust store, which is what makes it useful: it is
// exactly the certificate an intermediary would present.
func testCert(t *testing.T, name string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating a key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	if ip := net.ParseIP(name); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{name}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating a certificate: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// newTLSServer answers the SSLRequest with 'S', upgrades the socket and then
// runs an ordinary startup over the encrypted connection. Reaching the startup
// packet at all proves the client really did wrap the socket.
func newTLSServer(t *testing.T, cert tls.Certificate, handshook chan<- struct{}) *fakeServer {
	t.Helper()
	return newFakeServer(t, func(f *fakeConn) {
		var pkt [8]byte
		if _, err := io.ReadFull(f.Conn, pkt[:]); err != nil {
			return
		}
		if _, err := f.Conn.Write([]byte{'S'}); err != nil {
			return
		}
		tconn := tls.Server(f.Conn, &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		})
		if err := tconn.Handshake(); err != nil {
			// Expected whenever the client was right to refuse this certificate.
			return
		}
		select {
		case handshook <- struct{}{}:
		default:
		}
		inner := &fakeConn{t: f.t, Conn: tconn}
		inner.acceptStartup()
		for {
			typ, _, err := inner.readMsg()
			if err != nil || typ == 'X' {
				return
			}
		}
	})
}

// TestTLSModes is the table of what each sslmode promises. require and prefer
// promise encryption only - libpq behaves the same way, and pretending
// otherwise would be worse than being clear about it - while verify-ca and
// verify-full promise the certificate chains to something we trust.
func TestTLSModes(t *testing.T) {
	cases := []struct {
		name      string
		mode      string
		certName  string
		wantErr   bool
		wantInErr string
	}{
		{
			name:     "require encrypts without checking who answered",
			mode:     "require",
			certName: "127.0.0.1",
		},
		{
			name:     "prefer upgrades when the server offers it",
			mode:     "prefer",
			certName: "127.0.0.1",
		},
		{
			name:     "require does not care that the name is wrong either",
			mode:     "require",
			certName: "somebody-else.example",
		},
		{
			// The security-critical case: a certificate signed by nobody we trust
			// is exactly what an intermediary presents, and verify-full must
			// refuse it even though the name matches.
			name:      "verify-full refuses a certificate signed by nobody we trust",
			mode:      "verify-full",
			certName:  "127.0.0.1",
			wantErr:   true,
			wantInErr: "TLS handshake",
		},
		{
			name:      "verify-full refuses a certificate for the wrong name",
			mode:      "verify-full",
			certName:  "somebody-else.example",
			wantErr:   true,
			wantInErr: "TLS handshake",
		},
		{
			// verify-ca deliberately skips the hostname check, so this proves the
			// chain check it does keep is real rather than a no-op.
			name:      "verify-ca refuses an untrusted chain",
			mode:      "verify-ca",
			certName:  "127.0.0.1",
			wantErr:   true,
			wantInErr: "verifying server certificate chain",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handshook := make(chan struct{}, 1)
			srv := newTLSServer(t, testCert(t, tc.certName), handshook)
			cfg := srv.config()
			cfg.SSLMode = tc.mode

			c, err := connectTo(t, cfg)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("sslmode=%s accepted a certificate it should have refused; "+
						"the caller believes it verified the server's identity and it did "+
						"not", tc.mode)
				}
				if !strings.Contains(err.Error(), tc.wantInErr) {
					t.Errorf("error %q does not mention %q", err, tc.wantInErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("sslmode=%s: %v", tc.mode, err)
			}
			select {
			case <-handshook:
			default:
				t.Fatal("the server never completed a TLS handshake, so the startup " +
					"packet went over the wire in the clear")
			}
			if _, ok := c.raw.(*tls.Conn); !ok {
				t.Errorf("the connection is a %T, not a *tls.Conn; nothing after startup "+
					"would be encrypted", c.raw)
			}
		})
	}
}

// TestTLSUnknownModeIsRefusedBeforeTheHandshake: an sslmode nobody recognises
// must not fall through to whatever the last case happened to set up.
func TestTLSUnknownModeIsRefusedBeforeTheHandshake(t *testing.T) {
	handshook := make(chan struct{}, 1)
	srv := newTLSServer(t, testCert(t, "127.0.0.1"), handshook)
	for _, mode := range []string{"banana", "verify_full", "VERIFY-FULL-ISH", "true", "1"} {
		cfg := srv.config()
		cfg.SSLMode = mode
		err := mustFailToConnect(t, cfg, "sslmode "+mode+" means nothing")
		if !strings.Contains(err.Error(), "unknown sslmode") {
			t.Errorf("sslmode=%s failed with %q, which does not tell the operator the "+
				"mode was the problem", mode, err)
		}
	}
}

// TestTLSModeIsCaseInsensitive, because DSNs are written by hand and libpq
// accepts any casing.
func TestTLSModeIsCaseInsensitive(t *testing.T) {
	handshook := make(chan struct{}, 1)
	srv := newTLSServer(t, testCert(t, "127.0.0.1"), handshook)
	for _, mode := range []string{"REQUIRE", "Require", "reQuire"} {
		cfg := srv.config()
		cfg.SSLMode = mode
		if _, err := connectTo(t, cfg); err != nil {
			t.Errorf("sslmode=%s was rejected: %v", mode, err)
		}
	}

	// The same for disable, which must not accidentally probe for TLS.
	srv2 := newFakeServer(t, serveIdle)
	for _, mode := range []string{"DISABLE", "Disable"} {
		cfg := srv2.config()
		cfg.SSLMode = mode
		if _, err := connectTo(t, cfg); err != nil {
			t.Errorf("sslmode=%s was rejected: %v", mode, err)
		}
	}
}

// TestTLSEmptyModeDefaultsToPrefer covers a Config built by hand rather than by
// ParseDSN, which is what ConfigFromEnv can produce when PGSSLMODE is unset.
func TestTLSEmptyModeDefaultsToPrefer(t *testing.T) {
	srv := newFakeServer(t, serveIdle)
	cfg := srv.config()
	cfg.SSLMode = ""
	// serveIdle answers the SSLRequest with 'N' via readStartup, so an empty mode
	// behaving as prefer means the connection still comes up.
	if _, err := connectTo(t, cfg); err != nil {
		t.Fatalf("an empty sslmode should behave as prefer, got %v", err)
	}
}

// TestVerifyChainOnlyRejectsWhatItCannotParse exercises the callback directly
// for the inputs a TLS stack would only produce if something was very wrong.
func TestVerifyChainOnlyRejectsMalformedCertificates(t *testing.T) {
	verify := verifyChainOnly("db.example")

	if err := verify(nil, nil); err == nil {
		t.Error("a handshake presenting no certificate at all was accepted")
	}
	if err := verify([][]byte{{0x00, 0x01, 0x02}}, nil); err == nil {
		t.Error("bytes that are not a certificate were accepted")
	} else if !strings.Contains(err.Error(), "parsing server certificate") {
		t.Errorf("error %q does not say the certificate could not be parsed", err)
	}

	// A well-formed certificate that nothing in the system store signed still
	// has to fail, which is the check that carries the security property.
	cert := testCert(t, "db.example")
	if err := verify(cert.Certificate, nil); err == nil {
		t.Error("a self-signed certificate passed the chain check; verify-ca would " +
			"accept any certificate an intermediary cared to generate")
	}
}
