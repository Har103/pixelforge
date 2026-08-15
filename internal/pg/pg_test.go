package pg

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

// TestSCRAMRFC7677Vector checks the client against the worked example in
// RFC 7677 section 3. If this passes, the PBKDF2, HMAC, proof and server
// verifier are all correct.
func TestSCRAMRFC7677Vector(t *testing.T) {
	const (
		clientNonce = "rOprNGfwEbeRWgbNEkqO"
		serverFirst = "r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0," +
			"s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096"
		wantFinal = "c=biws,r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0," +
			"p=dHzbZapWIk4jUhN+Ute9ytag9zjfMHgsqmmiz7AndVQ="
		serverFinal = "v=6rriTRBi23WpRR/wtup+mMhUZUn/dB5nLTJRsjl95G4="
	)

	c := &scramClient{password: "pencil", nonce: clientNonce}
	// The RFC vector includes the username; PostgreSQL sends it in the startup
	// packet instead and leaves n= empty. Everything else is identical.
	c.clientFirst = "n=user,r=" + clientNonce

	got, err := c.step([]byte(serverFirst))
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	if string(got) != wantFinal {
		t.Errorf("client-final-message mismatch\n got: %s\nwant: %s", got, wantFinal)
	}
	if err := c.verify([]byte(serverFinal)); err != nil {
		t.Errorf("verify: %v", err)
	}
}

func TestSCRAMRejectsTamperedVerifier(t *testing.T) {
	c := &scramClient{password: "pencil", nonce: "rOprNGfwEbeRWgbNEkqO"}
	c.clientFirst = "n=user,r=rOprNGfwEbeRWgbNEkqO"
	if _, err := c.step([]byte("r=rOprNGfwEbeRWgbNEkqOxyz,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096")); err != nil {
		t.Fatalf("step: %v", err)
	}
	if err := c.verify([]byte("v=" + base64.StdEncoding.EncodeToString(make([]byte, 32)))); err == nil {
		t.Error("expected verification to fail on a wrong server signature")
	}
}

func TestSCRAMRejectsNonceThatDoesNotExtendOurs(t *testing.T) {
	c, err := newSCRAM("pencil")
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.step([]byte("r=someoneElsesNonce,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096"))
	if err == nil || !strings.Contains(err.Error(), "does not extend") {
		t.Errorf("expected a nonce-extension error, got %v", err)
	}
}

func TestSCRAMRejectsAbsurdIterationCount(t *testing.T) {
	c, err := newSCRAM("pencil")
	if err != nil {
		t.Fatal(err)
	}
	msg := "r=" + c.nonce + "extra,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=99999999"
	if _, err := c.step([]byte(msg)); err == nil {
		t.Error("expected the client to refuse an enormous iteration count")
	}
}

func TestPBKDF2KnownAnswer(t *testing.T) {
	// RFC 7677's derived salted password for ("pencil", W22ZaJ0SNY7soEsUEjb6gQ==, 4096).
	salt, _ := base64.StdEncoding.DecodeString("W22ZaJ0SNY7soEsUEjb6gQ==")
	got := pbkdf2SHA256([]byte("pencil"), salt, 4096, 32)
	want, _ := base64.StdEncoding.DecodeString("xKSVEDI6tPlSysH6mUQZOeeOp01r6B3fcJbodRPcYV0=")
	if !bytes.Equal(got, want) {
		t.Errorf("salted password mismatch\n got %x\nwant %x", got, want)
	}
}

func TestPBKDF2MultiBlockOutput(t *testing.T) {
	// A key longer than one SHA-256 block exercises the block-concatenation path.
	out := pbkdf2SHA256([]byte("password"), []byte("salt"), 2, 100)
	if len(out) != 100 {
		t.Fatalf("expected 100 bytes, got %d", len(out))
	}
	if bytes.Equal(out[:32], out[32:64]) {
		t.Error("blocks are identical, the block counter is not being applied")
	}
}

func TestParseDSNURL(t *testing.T) {
	c, err := ParseDSN("postgres://alice:s3cr%2Ft@db.internal:6543/shop?sslmode=require&application_name=x")
	if err != nil {
		t.Fatal(err)
	}
	if c.User != "alice" {
		t.Errorf("user = %q", c.User)
	}
	if c.Password != "s3cr/t" {
		t.Errorf("password was not URL-decoded: %q", c.Password)
	}
	if c.Host != "db.internal" || c.Port != "6543" {
		t.Errorf("host:port = %s:%s", c.Host, c.Port)
	}
	if c.Database != "shop" {
		t.Errorf("database = %q", c.Database)
	}
	if c.SSLMode != "require" {
		t.Errorf("sslmode = %q", c.SSLMode)
	}
	if c.ApplicationName != "x" {
		t.Errorf("application_name = %q", c.ApplicationName)
	}
}

func TestParseDSNKeywordForm(t *testing.T) {
	c, err := ParseDSN("host=10.0.0.5 port=5433 user=bob password='has space' dbname=app sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	if c.Host != "10.0.0.5" || c.Port != "5433" || c.User != "bob" || c.Database != "app" {
		t.Errorf("unexpected config: %+v", c)
	}
	if c.Password != "has space" {
		t.Errorf("quoted password mishandled: %q", c.Password)
	}
}

func TestParseDSNDefaults(t *testing.T) {
	c, err := ParseDSN("postgres://solo@example.com/")
	if err != nil {
		t.Fatal(err)
	}
	if c.Port != "5432" {
		t.Errorf("port default = %q", c.Port)
	}
	if c.Database != "solo" {
		t.Errorf("database should default to the user, got %q", c.Database)
	}
	if c.SSLMode != "prefer" {
		t.Errorf("sslmode default = %q", c.SSLMode)
	}
}

func TestParseDSNRejectsGarbage(t *testing.T) {
	for _, dsn := range []string{"", "   ", "postgres://:pw@host/db"} {
		if _, err := ParseDSN(dsn); err == nil {
			t.Errorf("expected an error for %q", dsn)
		}
	}
}

func TestRedactedHidesPassword(t *testing.T) {
	c, err := ParseDSN("postgres://alice:hunter2@db:5432/shop")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(c.Redacted(), "hunter2") {
		t.Errorf("Redacted leaked the password: %s", c.Redacted())
	}
}

func TestEncodeParamRoundTrip(t *testing.T) {
	blob := []byte{0x00, 0x01, 0xff, 0x5c, 'x', 0x7f}
	enc, err := encodeParam(blob)
	if err != nil {
		t.Fatal(err)
	}
	if string(enc) != `\x0001ff5c787f` {
		t.Fatalf("bytea encoding = %s", enc)
	}
	back, err := Bytea(enc)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(back, blob) {
		t.Errorf("round trip mismatch: %x vs %x", back, blob)
	}
}

func TestEncodeParamNil(t *testing.T) {
	v, err := encodeParam(nil)
	if err != nil || v != nil {
		t.Errorf("nil should encode to SQL NULL, got %v %v", v, err)
	}
	var nilBytes []byte
	v, err = encodeParam(nilBytes)
	if err != nil || v != nil {
		t.Errorf("a nil []byte should encode to SQL NULL, got %v %v", v, err)
	}
}

func TestEncodeParamRejectsUnknownType(t *testing.T) {
	if _, err := encodeParam(struct{ A int }{1}); err == nil {
		t.Error("expected an error for an unsupported parameter type")
	}
}

func TestByteaEscapeFormat(t *testing.T) {
	got, err := Bytea([]byte(`ab\\cd\001e`))
	if err != nil {
		t.Fatal(err)
	}
	if want := []byte("ab\\cd\x01e"); !bytes.Equal(got, want) {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestTimeParsing(t *testing.T) {
	got, err := Time([]byte("2026-08-15 19:02:26.5+00"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Year() != 2026 || got.Month() != time.August || got.Day() != 15 {
		t.Errorf("parsed to %v", got)
	}
}

func TestAffectedFromTag(t *testing.T) {
	cases := map[string]int64{
		"INSERT 0 3": 3,
		"UPDATE 12":  12,
		"DELETE 0":   0,
		"SELECT 41":  41,
		"BEGIN":      0,
		"":           0,
	}
	for tag, want := range cases {
		if got := affectedFromTag(tag); got != want {
			t.Errorf("affectedFromTag(%q) = %d, want %d", tag, got, want)
		}
	}
}

func TestParseErrorFields(t *testing.T) {
	// A synthetic ErrorResponse body: field code, value, NUL, ... terminated by 0.
	var body []byte
	add := func(code byte, val string) {
		body = append(body, code)
		body = append(body, val...)
		body = append(body, 0)
	}
	add('S', "ERROR")
	add('C', "23505")
	add('M', "duplicate key value violates unique constraint")
	add('D', "Key (id)=(1) already exists.")
	body = append(body, 0)

	e := parseError(body)
	if e.Severity != "ERROR" || e.SQLState() != "23505" {
		t.Errorf("unexpected fields: %+v", e)
	}
	if !strings.Contains(e.Error(), "duplicate key") || !strings.Contains(e.Error(), "23505") {
		t.Errorf("Error() = %s", e.Error())
	}
}

func TestWriteBufLengthPrefix(t *testing.T) {
	var w writeBuf
	w.start('Q')
	w.string("select 1")
	out := w.done()
	if out[0] != 'Q' {
		t.Fatalf("type byte = %q", out[0])
	}
	// length counts itself plus the body, but not the type byte
	if got, want := int(out[1])<<24|int(out[2])<<16|int(out[3])<<8|int(out[4]), len(out)-1; got != want {
		t.Errorf("length prefix = %d, want %d", got, want)
	}
}

func TestReadBufTruncationIsAnError(t *testing.T) {
	r := &readBuf{b: []byte{0x01}}
	r.int32()
	if r.err == nil {
		t.Error("reading past the end should record an error rather than panic")
	}
}

func TestSASLPrepStripsControlCharacters(t *testing.T) {
	if got := saslPrep("pass\x01word\x7f"); got != "password" {
		t.Errorf("saslPrep = %q", got)
	}
	if got := saslPrep("pässword"); got != "pässword" {
		t.Errorf("non-ASCII should pass through unchanged, got %q", got)
	}
}
