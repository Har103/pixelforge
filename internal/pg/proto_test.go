package pg

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"runtime"
	"strings"
	"testing"
)

func bufReader(p []byte) *bufio.Reader { return bufio.NewReader(bytes.NewReader(p)) }

// header builds the five bytes that precede every backend message body, so a
// test can claim a length the stream does not actually carry.
func header(typ byte, length uint32) []byte {
	head := []byte{typ, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(head[1:5], length)
	return head
}

// TestReadMessageRejectsBogusLengths covers the header arithmetic. A length
// below 4 would make the body length negative, and make([]byte, negative)
// panics, so every one of these must be turned away before the allocation.
func TestReadMessageRejectsBogusLengths(t *testing.T) {
	for _, length := range []uint32{0, 1, 2, 3} {
		_, err := readMessage(bufReader(header('D', length)))
		if err == nil {
			t.Errorf("length %d was accepted; a body length of %d would panic in make()", length, int64(length)-4)
			continue
		}
		if !strings.Contains(err.Error(), "bogus message length") {
			t.Errorf("length %d: error %q does not name the problem", length, err)
		}
	}
}

// TestReadMessageRefusesToAllocateWhatTheServerAsksFor is the memory-exhaustion
// case: a single forged length prefix must not be able to make the client
// allocate half a gigabyte, because the body is allocated before a single byte
// of it has been read and a server that lies costs nothing.
func TestReadMessageRefusesToAllocateWhatTheServerAsksFor(t *testing.T) {
	const claimed = 512 << 20 // twice maxMessageSize
	head := header('D', claimed)

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := readMessage(bufReader(head))
	runtime.ReadMemStats(&after)

	if err == nil {
		t.Fatal("a message claiming 512 MiB was accepted")
	}
	if !strings.Contains(err.Error(), "exceeds limit") {
		t.Errorf("error %q should say the message exceeded the size limit", err)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 8<<20 {
		t.Errorf("reading the header allocated %d bytes; a forged length prefix must not "+
			"be able to drive allocation, or a hostile server can OOM the process", grew)
	}
}

// TestReadMessageAtTheSizeLimit pins the boundary so a future change to
// maxMessageSize cannot quietly become off-by-one in the rejecting direction.
func TestReadMessageAtTheSizeLimit(t *testing.T) {
	tooBig := uint32(maxMessageSize) + 1
	if _, err := readMessage(bufReader(header('D', tooBig))); err == nil {
		t.Errorf("maxMessageSize+1 (%d bytes) should be rejected", tooBig)
	}
}

func TestReadMessageTruncatedStream(t *testing.T) {
	cases := []struct {
		name string
		wire []byte
		want error
	}{
		{"nothing at all", nil, io.EOF},
		{"half a header", []byte{'D', 0, 0}, io.ErrUnexpectedEOF},
		// io.ReadFull reports a plain EOF when it managed no bytes at all, so a
		// body promised and not begun looks like a clean hangup. Both are errors
		// and both mark the connection broken, which is all the caller needs.
		{"header only, body promised", []byte{'D', 0, 0, 0, 10}, io.EOF},
		{"body cut in half", append([]byte{'D', 0, 0, 0, 10}, 1, 2, 3), io.ErrUnexpectedEOF},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := readMessage(bufReader(tc.wire))
			if !errors.Is(err, tc.want) {
				t.Errorf("got %v, want %v; a half-delivered message must be an error, "+
					"not a message with a short body", err, tc.want)
			}
		})
	}
}

// TestReadMessageEmptyBody is the legal minimum: ParseComplete and friends carry
// a length of exactly 4 and no body.
func TestReadMessageEmptyBody(t *testing.T) {
	m, err := readMessage(bufReader([]byte{'1', 0, 0, 0, 4}))
	if err != nil {
		t.Fatalf("a body-less message must be accepted: %v", err)
	}
	if m.typ != '1' || len(m.body) != 0 {
		t.Errorf("got type %q body %v, want '1' and an empty body", m.typ, m.body)
	}
}

// TestReadMessageBodyIsNotAliased matters because callers keep the body: rows
// are appended to a Result that outlives the read loop. If two messages shared
// a buffer the second would silently rewrite the first.
func TestReadMessageBodyIsNotAliased(t *testing.T) {
	wire := []byte{'D', 0, 0, 0, 6, 0xAA, 0xBB, 'D', 0, 0, 0, 6, 0xCC, 0xDD}
	r := bufReader(wire)
	first, err := readMessage(r)
	if err != nil {
		t.Fatal(err)
	}
	kept := first.body
	if _, err := readMessage(r); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(kept, []byte{0xAA, 0xBB}) {
		t.Errorf("the first body became %x after reading the second; message bodies "+
			"must not share storage or retained rows will be corrupted", kept)
	}
}

// TestReadBufNeverPanics walks every accessor over a buffer that is always one
// byte too short. Every one of these is reached with attacker-controlled data
// straight off the socket, so the contract is "record an error", never "panic".
func TestReadBufNeverPanics(t *testing.T) {
	cases := []struct {
		name string
		buf  []byte
		read func(*readBuf)
	}{
		{"int32 on 3 bytes", []byte{1, 2, 3}, func(r *readBuf) { r.int32() }},
		{"int32 on nothing", nil, func(r *readBuf) { r.int32() }},
		{"int16 on 1 byte", []byte{1}, func(r *readBuf) { r.int16() }},
		{"int16 on nothing", nil, func(r *readBuf) { r.int16() }},
		{"byte on nothing", nil, func(r *readBuf) { r.byte() }},
		{"cstring with no NUL", []byte("abc"), func(r *readBuf) { r.string() }},
		{"cstring on nothing", nil, func(r *readBuf) { r.string() }},
		{"next past the end", []byte{1, 2}, func(r *readBuf) { r.next(9) }},
		{"next with a negative length", []byte{1, 2}, func(r *readBuf) { r.next(-1) }},
		{"next of everything", []byte{1, 2}, func(r *readBuf) { r.next(2); r.next(1) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &readBuf{b: tc.buf}
			tc.read(r)
			if r.err == nil {
				t.Errorf("reading past the end of %v recorded no error, so the caller "+
					"will treat the zero value as real data", tc.buf)
			}
		})
	}
}

// TestReadBufStickyError checks that the first failure is the one reported. The
// message-body decoders keep reading after a short field, and a later, vaguer
// error would bury the real one.
func TestReadBufStickyError(t *testing.T) {
	r := &readBuf{b: []byte{1}}
	r.int32()
	first := r.err
	r.string()
	if r.err != first {
		t.Errorf("the recorded error changed from %v to %v; the first failure is the "+
			"one that explains the message", first, r.err)
	}
	if !strings.Contains(first.Error(), "int32") {
		t.Errorf("error %q does not say which field was short", first)
	}
}

// TestReadBufNextDoesNotAliasTheWholeBuffer guards the NULL-marker path in
// DataRow decoding, where a length of -1 must not hand back the rest of the
// message as if it were a value.
func TestReadBufNextZeroLength(t *testing.T) {
	r := &readBuf{b: []byte{1, 2, 3}}
	v := r.next(0)
	if len(v) != 0 {
		t.Errorf("next(0) returned %v, want an empty slice", v)
	}
	if r.err != nil {
		t.Errorf("next(0) is legal - an empty string column - but recorded %v", r.err)
	}
}

// TestParseErrorHostileBodies feeds ErrorResponse decoding the bodies a real
// server never sends. An error arriving mid-query is already the unhappy path;
// panicking there turns a reportable database error into a crash.
func TestParseErrorHostileBodies(t *testing.T) {
	cases := []struct {
		name string
		body []byte
	}{
		{"empty", nil},
		{"terminator only", []byte{0}},
		{"field code but no value", []byte{'M'}},
		{"value with no NUL terminator", []byte("Mboom")},
		{"no trailing terminator", []byte("SERROR\x00C23505\x00")},
		{"unknown field codes", []byte("ZZZ\x00Qqq\x00\x00")},
		{"invalid UTF-8 in the message", []byte("M\xff\xfe\xfd\x00\x00")},
		{"NUL-heavy", bytes.Repeat([]byte{0}, 64)},
		{"code with an empty value", []byte("C\x00M\x00\x00")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := parseError(tc.body)
			if e == nil {
				t.Fatal("parseError returned nil; callers dereference it unconditionally")
			}
			if e.Message == "" {
				t.Error("Message is empty, so Error() would render a bare \"pg: \" with no " +
					"indication of what went wrong")
			}
			if s := e.Error(); !strings.HasPrefix(s, "pg: ") {
				t.Errorf("Error() = %q, want the package prefix", s)
			}
		})
	}
}

func TestParseErrorFullFieldSet(t *testing.T) {
	var body []byte
	for _, f := range []struct {
		code byte
		val  string
	}{
		{'S', "FATAL"}, {'C', "57P01"}, {'M', "terminating connection"},
		{'D', "detail"}, {'H', "hint"}, {'P', "12"}, {'W', "where"},
		{'s', "public"}, {'t', "rooms"}, {'c', "slug"}, {'n', "rooms_slug_key"},
	} {
		body = append(body, f.code)
		body = append(body, f.val...)
		body = append(body, 0)
	}
	body = append(body, 0)

	e := parseError(body)
	for _, tc := range []struct{ name, got, want string }{
		{"Severity", e.Severity, "FATAL"},
		{"Code", e.Code, "57P01"},
		{"Message", e.Message, "terminating connection"},
		{"Detail", e.Detail, "detail"},
		{"Hint", e.Hint, "hint"},
		{"Position", e.Position, "12"},
		{"Where", e.Where, "where"},
		{"Schema", e.Schema, "public"},
		{"Table", e.Table, "rooms"},
		{"Column", e.Column, "slug"},
		{"Constraint", e.Constrai, "rooms_slug_key"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
	if e.SQLState() != "57P01" {
		t.Errorf("SQLState() = %q; callers branch on this to tell a unique-violation "+
			"from a dropped connection", e.SQLState())
	}
}

// TestErrorRendering pins what an operator actually reads in a log line. The
// optional parts have to be optional: a NOTICE with no code must not render as
// ": " with nothing in front of it.
func TestErrorRendering(t *testing.T) {
	cases := []struct {
		name string
		err  *Error
		want string
	}{
		{
			name: "everything",
			err: &Error{Severity: "ERROR", Code: "23505", Message: "duplicate key",
				Detail: "Key (slug)=(x) already exists.", Hint: "pick another"},
			want: "pg: ERROR 23505: duplicate key (detail: Key (slug)=(x) already exists.) (hint: pick another)",
		},
		{
			name: "message only",
			err:  &Error{Message: "something happened"},
			want: "pg: something happened",
		},
		{
			name: "no code",
			err:  &Error{Severity: "NOTICE", Message: "table will be created"},
			want: "pg: NOTICE table will be created",
		},
		{
			name: "no severity",
			err:  &Error{Code: "08006", Message: "connection failure"},
			want: "pg: 08006: connection failure",
		},
		{
			name: "detail but no hint",
			err:  &Error{Message: "nope", Detail: "because"},
			want: "pg: nope (detail: because)",
		},
		{
			name: "hint but no detail",
			err:  &Error{Message: "nope", Hint: "try again"},
			want: "pg: nope (hint: try again)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Error(); got != tc.want {
				t.Errorf("Error() = %q\n           want %q", got, tc.want)
			}
		})
	}
}

// TestWriteBufStartUntyped covers the startup packet, whose length prefix counts
// itself and has no type byte in front of it.
func TestWriteBufStartUntyped(t *testing.T) {
	var w writeBuf
	w.startUntyped()
	w.int32(protocolVersion)
	w.string("user")
	w.string("bob")
	w.byte(0)
	out := w.done()

	if got, want := int(out[0])<<24|int(out[1])<<16|int(out[2])<<8|int(out[3]), len(out); got != want {
		t.Errorf("startup length prefix = %d, want %d (the whole packet, itself included)", got, want)
	}
	if got := int(out[4])<<24 | int(out[5])<<16 | int(out[6])<<8 | int(out[7]); got != protocolVersion {
		t.Errorf("protocol version = %d, want %d", got, protocolVersion)
	}
}

// TestWriteBufReuseResets makes sure a second message does not inherit the
// first one's bytes. Query builds five messages from one buffer, so a missed
// reset would put a Parse body inside a Bind.
func TestWriteBufReuseResets(t *testing.T) {
	var w writeBuf
	w.start('Q')
	w.string("select something rather long")
	first := append([]byte(nil), w.done()...)

	w.start('S')
	second := w.done()

	if len(second) != 5 {
		t.Errorf("Sync is %d bytes, want 5; the buffer kept %d bytes from the previous message",
			len(second), len(second)-5)
	}
	if second[0] != 'S' {
		t.Errorf("type byte = %q, want 'S'", second[0])
	}
	if bytes.Contains(second, []byte("select")) {
		t.Error("the second message still contains the first one's payload")
	}
	if first[0] != 'Q' {
		t.Errorf("the copy of the first message was corrupted: type byte %q", first[0])
	}
}

// TestWriteBufStringIsNulTerminated pins the C-string encoding, including the
// empty string that Query uses for the unnamed statement and portal.
func TestWriteBufStringIsNulTerminated(t *testing.T) {
	var w writeBuf
	w.start('P')
	w.string("")
	w.string("select 1")
	w.int16(0)
	out := w.done()[5:]

	want := append([]byte{0}, "select 1"...)
	want = append(want, 0, 0, 0)
	if !bytes.Equal(out, want) {
		t.Errorf("Parse body = %q, want %q", out, want)
	}
}
