package pg

import (
	"bufio"
	"context"
	"crypto/md5"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config describes how to reach a server. Build one with ParseDSN or
// ConfigFromEnv rather than filling it in by hand.
type Config struct {
	Host            string
	Port            string
	User            string
	Password        string
	Database        string
	SSLMode         string // disable | prefer | require | verify-ca | verify-full
	ConnectTimeout  time.Duration
	ApplicationName string
	RuntimeParams   map[string]string
}

// ParseDSN accepts either a URL ("postgres://user:pass@host:5432/db?sslmode=require")
// or libpq keyword/value form ("host=... user=... password=... dbname=...").
func ParseDSN(dsn string) (*Config, error) {
	c := &Config{
		Port:            "5432",
		SSLMode:         "prefer",
		ConnectTimeout:  10 * time.Second,
		ApplicationName: "pixelforge",
		RuntimeParams:   map[string]string{},
	}
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return nil, errors.New("pg: empty DSN")
	}

	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return nil, fmt.Errorf("pg: parsing DSN: %w", err)
		}
		if u.User != nil {
			c.User = u.User.Username()
			if pw, ok := u.User.Password(); ok {
				c.Password = pw
			}
		}
		host := u.Hostname()
		if host != "" {
			c.Host = host
		}
		if p := u.Port(); p != "" {
			c.Port = p
		}
		c.Database = strings.TrimPrefix(u.Path, "/")
		for k, vs := range u.Query() {
			if len(vs) == 0 {
				continue
			}
			applyKeyword(c, k, vs[0])
		}
	} else {
		for _, field := range splitKeywordDSN(dsn) {
			k, v, ok := strings.Cut(field, "=")
			if !ok {
				return nil, fmt.Errorf("pg: malformed DSN field %q", field)
			}
			applyKeyword(c, strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), "'"))
		}
	}

	if c.Host == "" {
		c.Host = "127.0.0.1"
	}
	if c.User == "" {
		return nil, errors.New("pg: DSN has no user")
	}
	if c.Database == "" {
		c.Database = c.User
	}
	return c, nil
}

func applyKeyword(c *Config, k, v string) {
	switch strings.ToLower(k) {
	case "host":
		c.Host = v
	case "port":
		c.Port = v
	case "user":
		c.User = v
	case "password":
		c.Password = v
	case "dbname", "database":
		c.Database = v
	case "sslmode":
		c.SSLMode = v
	case "application_name":
		c.ApplicationName = v
	case "connect_timeout":
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.ConnectTimeout = time.Duration(n) * time.Second
		}
	default:
		c.RuntimeParams[k] = v
	}
}

// splitKeywordDSN splits on whitespace but keeps single-quoted values together.
func splitKeywordDSN(s string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch == '\'':
			inQuote = !inQuote
			cur.WriteByte(ch)
		case (ch == ' ' || ch == '\t') && !inQuote:
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteByte(ch)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// ConfigFromEnv reads DATABASE_URL if set, otherwise the standard PG* variables.
// Managed providers hand out one or the other, so accept both.
func ConfigFromEnv() (*Config, error) {
	for _, key := range []string{"DATABASE_URL", "POSTGRES_URL", "PG_DSN"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return ParseDSN(v)
		}
	}
	host := envOr("PGHOST", "")
	if host == "" {
		return nil, errors.New("pg: no DATABASE_URL and no PGHOST in the environment")
	}
	c := &Config{
		Host:            host,
		Port:            envOr("PGPORT", "5432"),
		User:            envOr("PGUSER", "postgres"),
		Password:        os.Getenv("PGPASSWORD"),
		Database:        envOr("PGDATABASE", ""),
		SSLMode:         envOr("PGSSLMODE", "prefer"),
		ConnectTimeout:  10 * time.Second,
		ApplicationName: envOr("PGAPPNAME", "pixelforge"),
		RuntimeParams:   map[string]string{},
	}
	if c.Database == "" {
		c.Database = c.User
	}
	return c, nil
}

func envOr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

// Redacted renders the target without the password, for logs.
func (c *Config) Redacted() string {
	return fmt.Sprintf("postgres://%s@%s:%s/%s?sslmode=%s", c.User, c.Host, c.Port, c.Database, c.SSLMode)
}

// Conn is a single connection. It is not safe for concurrent use; take one from
// a Pool instead of sharing.
type Conn struct {
	cfg    *Config
	raw    net.Conn
	r      *bufio.Reader
	w      *bufio.Writer
	wb     writeBuf
	params map[string]string
	pid    int32
	secret int32
	broken bool

	// txStatus is the byte from the last ReadyForQuery: 'I' idle, 'T' in a
	// transaction, 'E' in a failed transaction.
	txStatus byte
}

// Connect dials the server and completes startup and authentication.
func Connect(ctx context.Context, cfg *Config) (*Conn, error) {
	d := net.Dialer{Timeout: cfg.ConnectTimeout}
	addr := net.JoinHostPort(cfg.Host, cfg.Port)
	raw, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("pg: dialing %s: %w", addr, err)
	}
	if tc, ok := raw.(*net.TCPConn); ok {
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(30 * time.Second)
	}

	c := &Conn{
		cfg:    cfg,
		raw:    raw,
		params: map[string]string{},
	}

	if deadline, ok := ctx.Deadline(); ok {
		_ = raw.SetDeadline(deadline)
	} else {
		_ = raw.SetDeadline(time.Now().Add(cfg.ConnectTimeout + 20*time.Second))
	}

	if err := c.negotiateTLS(); err != nil {
		raw.Close()
		return nil, err
	}
	c.r = bufio.NewReaderSize(c.raw, 32<<10)
	c.w = bufio.NewWriterSize(c.raw, 32<<10)

	if err := c.startup(); err != nil {
		c.raw.Close()
		return nil, err
	}
	_ = c.raw.SetDeadline(time.Time{})
	return c, nil
}

// negotiateTLS sends an SSLRequest when the mode calls for it and upgrades the
// socket in place if the server agrees.
func (c *Conn) negotiateTLS() error {
	mode := strings.ToLower(c.cfg.SSLMode)
	if mode == "" {
		mode = "prefer"
	}
	if mode == "disable" {
		return nil
	}

	var pkt [8]byte
	binary.BigEndian.PutUint32(pkt[0:4], 8)
	binary.BigEndian.PutUint32(pkt[4:8], sslRequestCode)
	if _, err := c.raw.Write(pkt[:]); err != nil {
		return fmt.Errorf("pg: sending SSLRequest: %w", err)
	}
	var resp [1]byte
	if _, err := c.raw.Read(resp[:]); err != nil {
		return fmt.Errorf("pg: reading SSLRequest reply: %w", err)
	}
	switch resp[0] {
	case 'S':
		tcfg := &tls.Config{ServerName: c.cfg.Host, MinVersion: tls.VersionTLS12}
		// require only guarantees encryption, not identity - that is what libpq
		// does too. verify-ca and verify-full both check the chain here; the
		// hostname check is what separates them, and ServerName drives it.
		switch mode {
		case "require", "prefer":
			tcfg.InsecureSkipVerify = true
		case "verify-ca":
			tcfg.InsecureSkipVerify = true
			tcfg.VerifyPeerCertificate = verifyChainOnly(c.cfg.Host)
		case "verify-full":
			// default verification: chain plus hostname
		default:
			return fmt.Errorf("pg: unknown sslmode %q", c.cfg.SSLMode)
		}
		tconn := tls.Client(c.raw, tcfg)
		if err := tconn.Handshake(); err != nil {
			return fmt.Errorf("pg: TLS handshake: %w", err)
		}
		c.raw = tconn
		return nil
	case 'N':
		if mode == "prefer" {
			return nil
		}
		return fmt.Errorf("pg: server refused TLS but sslmode=%s", mode)
	case 'E':
		return fmt.Errorf("pg: server rejected SSLRequest")
	default:
		return fmt.Errorf("pg: unexpected SSLRequest reply %q", resp[0])
	}
}

func (c *Conn) startup() error {
	c.wb.startUntyped()
	c.wb.int32(protocolVersion)
	c.wb.string("user")
	c.wb.string(c.cfg.User)
	if c.cfg.Database != "" {
		c.wb.string("database")
		c.wb.string(c.cfg.Database)
	}
	if c.cfg.ApplicationName != "" {
		c.wb.string("application_name")
		c.wb.string(c.cfg.ApplicationName)
	}
	// Pin the wire text formats we decode so a server-side locale cannot change
	// how timestamps or bytea come back.
	c.wb.string("client_encoding")
	c.wb.string("UTF8")
	c.wb.string("DateStyle")
	c.wb.string("ISO, MDY")
	for k, v := range c.cfg.RuntimeParams {
		c.wb.string(k)
		c.wb.string(v)
	}
	c.wb.byte(0)
	if err := c.send(c.wb.done()); err != nil {
		return err
	}
	return c.authenticate()
}

func (c *Conn) authenticate() error {
	for {
		m, err := readMessage(c.r)
		if err != nil {
			return fmt.Errorf("pg: reading auth response: %w", err)
		}
		switch m.typ {
		case msgAuth:
			done, err := c.handleAuth(m.body)
			if err != nil {
				return err
			}
			if done {
				continue
			}
		case msgErrorResponse:
			return parseError(m.body)
		case msgParameterStatus:
			r := &readBuf{b: m.body}
			c.params[r.string()] = r.string()
		case msgBackendKeyData:
			r := &readBuf{b: m.body}
			c.pid = r.int32()
			c.secret = r.int32()
		case msgNoticeResponse:
			// ignore
		case msgReadyForQuery:
			r := &readBuf{b: m.body}
			c.txStatus = r.byte()
			return nil
		case msgNegotiateProtocol:
			return errors.New("pg: server requires a protocol version we do not speak")
		default:
			return fmt.Errorf("pg: unexpected message %q during startup", m.typ)
		}
	}
}

// handleAuth processes one 'R' message. It returns true when the exchange
// should continue reading further messages.
func (c *Conn) handleAuth(body []byte) (bool, error) {
	r := &readBuf{b: body}
	code := r.int32()
	// A short read yields zero, and zero is AuthenticationOk. Without this check
	// an 'R' message the server never finished sending - a truncated frame, a
	// proxy that cut the stream, anything at all in that shape - is indis-
	// tinguishable from "you are authenticated", and the driver goes on to hand
	// the caller a session it never proved it was entitled to.
	if r.err != nil {
		return false, fmt.Errorf("pg: truncated authentication message: %w", r.err)
	}
	switch code {
	case authOK:
		return true, nil

	case authCleartextPassword:
		c.wb.start('p')
		c.wb.string(c.cfg.Password)
		return true, c.send(c.wb.done())

	case authMD5Password:
		salt := r.next(4)
		if r.err != nil {
			return false, r.err
		}
		inner := md5.Sum([]byte(c.cfg.Password + c.cfg.User))
		outer := md5.Sum(append([]byte(hex.EncodeToString(inner[:])), salt...))
		c.wb.start('p')
		c.wb.string("md5" + hex.EncodeToString(outer[:]))
		return true, c.send(c.wb.done())

	case authSASL:
		return true, c.doSASL(r)

	case authSASLContinue, authSASLFinal:
		// Driven inside doSASL; reaching here means the server sent them out of
		// order.
		return false, errors.New("pg: unexpected SASL message outside the SASL exchange")

	default:
		return false, fmt.Errorf("pg: unsupported authentication method %d "+
			"(this driver speaks trust, cleartext, md5 and SCRAM-SHA-256)", code)
	}
}

func (c *Conn) doSASL(r *readBuf) error {
	var offered []string
	for {
		m := r.string()
		if m == "" || r.err != nil {
			break
		}
		offered = append(offered, m)
	}
	found := false
	for _, m := range offered {
		if m == scramSHA256 {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("pg: server offered SASL mechanisms %v, none of which is %s", offered, scramSHA256)
	}

	sc, err := newSCRAM(c.cfg.Password)
	if err != nil {
		return err
	}
	initial := sc.first()
	c.wb.start('p')
	c.wb.string(scramSHA256)
	c.wb.int32(int32(len(initial)))
	c.wb.raw(initial)
	if err := c.send(c.wb.done()); err != nil {
		return err
	}

	// AuthenticationSASLContinue
	m, err := readMessage(c.r)
	if err != nil {
		return fmt.Errorf("pg: reading SASL continue: %w", err)
	}
	if m.typ == msgErrorResponse {
		return parseError(m.body)
	}
	if m.typ != msgAuth {
		return fmt.Errorf("pg: expected SASL continue, got %q", m.typ)
	}
	rb := &readBuf{b: m.body}
	if code := rb.int32(); code != authSASLContinue {
		return fmt.Errorf("pg: expected SASL continue (11), got auth code %d", code)
	}
	final, err := sc.step(rb.rest())
	if err != nil {
		return err
	}
	c.wb.start('p')
	c.wb.raw(final)
	if err := c.send(c.wb.done()); err != nil {
		return err
	}

	// AuthenticationSASLFinal
	m, err = readMessage(c.r)
	if err != nil {
		return fmt.Errorf("pg: reading SASL final: %w", err)
	}
	if m.typ == msgErrorResponse {
		return parseError(m.body)
	}
	if m.typ != msgAuth {
		return fmt.Errorf("pg: expected SASL final, got %q", m.typ)
	}
	rb = &readBuf{b: m.body}
	if code := rb.int32(); code != authSASLFinal {
		return fmt.Errorf("pg: expected SASL final (12), got auth code %d", code)
	}
	return sc.verify(rb.rest())
}

func (c *Conn) send(p []byte) error {
	if _, err := c.w.Write(p); err != nil {
		c.broken = true
		return fmt.Errorf("pg: write: %w", err)
	}
	if err := c.w.Flush(); err != nil {
		c.broken = true
		return fmt.Errorf("pg: flush: %w", err)
	}
	return nil
}

// Close sends Terminate and shuts the socket down.
func (c *Conn) Close() error {
	if c.raw == nil {
		return nil
	}
	if !c.broken {
		c.wb.start('X')
		_ = c.send(c.wb.done())
	}
	err := c.raw.Close()
	c.raw = nil
	return err
}

// Broken reports whether the connection hit an I/O or protocol error and must
// be discarded rather than reused.
func (c *Conn) Broken() bool { return c.broken }

// ServerParam returns a startup parameter reported by the server, such as
// "server_version".
func (c *Conn) ServerParam(k string) string { return c.params[k] }

// Result holds a materialised query result. Values are raw text-format bytes,
// nil for SQL NULL.
type Result struct {
	Columns  []string
	Rows     [][][]byte
	Tag      string // CommandComplete tag, e.g. "INSERT 0 1"
	Affected int64
}

// Exec runs one or more statements through the simple query protocol. Use it
// for DDL and migrations; it does not accept parameters.
func (c *Conn) Exec(ctx context.Context, sql string) error {
	if err := c.applyDeadline(ctx); err != nil {
		return err
	}
	defer c.clearDeadline()

	c.wb.start('Q')
	c.wb.string(sql)
	if err := c.send(c.wb.done()); err != nil {
		return err
	}
	_, err := c.readUntilReady(nil)
	return err
}

// Query runs a single statement through the extended protocol, binding args as
// text-format parameters ($1, $2, ...).
func (c *Conn) Query(ctx context.Context, sql string, args ...any) (*Result, error) {
	if err := c.applyDeadline(ctx); err != nil {
		return nil, err
	}
	defer c.clearDeadline()

	encoded := make([][]byte, len(args))
	for i, a := range args {
		v, err := encodeParam(a)
		if err != nil {
			return nil, fmt.Errorf("pg: encoding $%d: %w", i+1, err)
		}
		encoded[i] = v
	}

	// Parse (unnamed statement, let the server infer parameter types)
	c.wb.start('P')
	c.wb.string("")
	c.wb.string(sql)
	c.wb.int16(0)
	parse := append([]byte(nil), c.wb.done()...)

	// Bind (unnamed portal, all text in and out)
	c.wb.start('B')
	c.wb.string("")
	c.wb.string("")
	c.wb.int16(0)
	c.wb.int16(int16(len(encoded)))
	for _, v := range encoded {
		if v == nil {
			c.wb.int32(-1)
			continue
		}
		c.wb.int32(int32(len(v)))
		c.wb.raw(v)
	}
	c.wb.int16(0)
	bind := append([]byte(nil), c.wb.done()...)

	c.wb.start('D')
	c.wb.byte('P')
	c.wb.string("")
	describe := append([]byte(nil), c.wb.done()...)

	c.wb.start('E')
	c.wb.string("")
	c.wb.int32(0)
	execute := append([]byte(nil), c.wb.done()...)

	c.wb.start('S')
	sync := c.wb.done()

	out := make([]byte, 0, len(parse)+len(bind)+len(describe)+len(execute)+len(sync))
	out = append(out, parse...)
	out = append(out, bind...)
	out = append(out, describe...)
	out = append(out, execute...)
	out = append(out, sync...)
	if err := c.send(out); err != nil {
		return nil, err
	}

	res := &Result{}
	if _, err := c.readUntilReady(res); err != nil {
		return nil, err
	}
	return res, nil
}

// QueryRow is a convenience wrapper returning the first row, or nil when the
// result is empty.
func (c *Conn) QueryRow(ctx context.Context, sql string, args ...any) ([][]byte, error) {
	res, err := c.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	if len(res.Rows) == 0 {
		return nil, nil
	}
	return res.Rows[0], nil
}

// readUntilReady drains messages up to ReadyForQuery. The first server error is
// remembered and returned once the exchange is complete, so the connection is
// left in a reusable state.
func (c *Conn) readUntilReady(res *Result) (byte, error) {
	var firstErr error
	// described is the column count of the RowDescription in force, or -1 when
	// none has arrived. Callers index rows by the position of the column they
	// asked for, so a row that does not match the description they were also
	// given is a panic waiting to happen in their code rather than ours.
	described := -1
	for {
		m, err := readMessage(c.r)
		if err != nil {
			c.broken = true
			return 0, fmt.Errorf("pg: read: %w", err)
		}
		switch m.typ {
		case msgRowDescription:
			if res == nil {
				continue
			}
			r := &readBuf{b: m.body}
			n := int(r.int16())
			res.Columns = make([]string, 0, max(n, 0))
			for i := 0; i < n; i++ {
				name := r.string()
				r.int32() // table OID
				r.int16() // column attribute number
				r.int32() // type OID
				r.int16() // type size
				r.int32() // type modifier
				r.int16() // format code
				res.Columns = append(res.Columns, name)
			}
			if r.err != nil && firstErr == nil {
				firstErr = r.err
			}
			described = len(res.Columns)

		case msgDataRow:
			if res == nil {
				continue
			}
			r := &readBuf{b: m.body}
			n := int(r.int16())
			if described >= 0 && n != described {
				if firstErr == nil {
					firstErr = fmt.Errorf("pg: DataRow carries %d fields but the "+
						"RowDescription named %d columns", n, described)
				}
				continue
			}
			row := make([][]byte, 0, max(n, 0))
			for i := 0; i < n; i++ {
				size := int(r.int32())
				if size < 0 {
					row = append(row, nil)
					continue
				}
				// A zero-length field is an empty string, and an empty string is
				// not SQL NULL. Appending to a nil slice adds nothing and stays
				// nil, so copying into a slice that is already non-nil is what
				// keeps the two distinguishable to the caller.
				val := make([]byte, size)
				copy(val, r.next(size))
				row = append(row, val)
			}
			if r.err != nil {
				if firstErr == nil {
					firstErr = r.err
				}
				continue
			}
			res.Rows = append(res.Rows, row)

		case msgCommandComplete:
			if res != nil {
				r := &readBuf{b: m.body}
				res.Tag = r.string()
				res.Affected = affectedFromTag(res.Tag)
			}

		case msgErrorResponse:
			if e := parseError(m.body); firstErr == nil {
				firstErr = e
			}

		case msgReadyForQuery:
			r := &readBuf{b: m.body}
			c.txStatus = r.byte()
			return c.txStatus, firstErr

		case msgParseComplete, msgBindComplete, msgCloseComplete,
			msgNoData, msgParameterDesc, msgEmptyQuery, msgPortalSuspended,
			msgNoticeResponse, msgNotification:
			// nothing to do

		case msgParameterStatus:
			r := &readBuf{b: m.body}
			c.params[r.string()] = r.string()

		case msgBackendKeyData:
			r := &readBuf{b: m.body}
			c.pid = r.int32()
			c.secret = r.int32()

		case msgCopyInResponse, msgCopyOutResponse:
			c.broken = true
			return 0, errors.New("pg: COPY is not supported by this driver")

		default:
			c.broken = true
			return 0, fmt.Errorf("pg: unexpected message %q", m.typ)
		}
	}
}

// affectedFromTag pulls the row count out of tags like "INSERT 0 3",
// "UPDATE 2" or "DELETE 7".
func affectedFromTag(tag string) int64 {
	if tag == "" {
		return 0
	}
	fields := strings.Fields(tag)
	if len(fields) < 2 {
		return 0
	}
	n, err := strconv.ParseInt(fields[len(fields)-1], 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func (c *Conn) applyDeadline(ctx context.Context) error {
	// Close nils out raw, and calling a method on a nil net.Conn interface is a
	// nil dereference, not an error. Every query path starts here, so one check
	// turns "the pool discarded this connection while a caller still held it"
	// from a process-killing panic into an error the caller can report.
	if c.raw == nil {
		return errors.New("pg: connection is closed")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if dl, ok := ctx.Deadline(); ok {
		return c.raw.SetDeadline(dl)
	}
	return c.raw.SetDeadline(time.Now().Add(30 * time.Second))
}

func (c *Conn) clearDeadline() {
	if c.raw != nil {
		_ = c.raw.SetDeadline(time.Time{})
	}
}

// Ping issues a trivial round trip to confirm the connection is still usable.
func (c *Conn) Ping(ctx context.Context) error {
	_, err := c.Query(ctx, "select 1")
	return err
}

// verifyChainOnly implements sslmode=verify-ca: the certificate chain must be
// trusted, but the hostname is not required to match. Managed providers often
// front a database with a generated hostname that is absent from the cert.
func verifyChainOnly(host string) func([][]byte, [][]*x509.Certificate) error {
	_ = host
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return errors.New("pg: server presented no certificate")
		}
		certs := make([]*x509.Certificate, 0, len(rawCerts))
		for _, raw := range rawCerts {
			cert, err := x509.ParseCertificate(raw)
			if err != nil {
				return fmt.Errorf("pg: parsing server certificate: %w", err)
			}
			certs = append(certs, cert)
		}
		roots, err := x509.SystemCertPool()
		if err != nil {
			return fmt.Errorf("pg: loading system root certificates: %w", err)
		}
		intermediates := x509.NewCertPool()
		for _, cert := range certs[1:] {
			intermediates.AddCert(cert)
		}
		if _, err := certs[0].Verify(x509.VerifyOptions{
			Roots:         roots,
			Intermediates: intermediates,
		}); err != nil {
			return fmt.Errorf("pg: verifying server certificate chain: %w", err)
		}
		return nil
	}
}
