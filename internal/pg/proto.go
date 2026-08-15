// Package pg implements a minimal PostgreSQL client speaking the v3 frontend/
// backend wire protocol directly over a net.Conn.
//
// It exists because Pixelforge ships with zero third-party modules: everything
// below is built on Go's standard library alone. The protocol is documented at
// https://www.postgresql.org/docs/current/protocol.html
//
// Scope: startup + SSL negotiation, cleartext/MD5/SCRAM-SHA-256 authentication,
// the simple query protocol, and the extended query protocol with parameter
// binding. All values are exchanged in text format, which keeps the type
// handling small and version-independent.
package pg

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
)

// Backend message types we care about. See "Message Formats" in the protocol
// docs; the byte is the first octet of every message after the startup phase.
const (
	msgAuth              = 'R'
	msgBackendKeyData    = 'K'
	msgBindComplete      = '2'
	msgCloseComplete     = '3'
	msgCommandComplete   = 'C'
	msgDataRow           = 'D'
	msgEmptyQuery        = 'I'
	msgErrorResponse     = 'E'
	msgNoData            = 'n'
	msgNoticeResponse    = 'N'
	msgNotification      = 'A'
	msgParameterDesc     = 't'
	msgParameterStatus   = 'S'
	msgParseComplete     = '1'
	msgPortalSuspended   = 's'
	msgReadyForQuery     = 'Z'
	msgRowDescription    = 'T'
	msgAuthSASLContinue  = 11
	msgAuthSASLFinal     = 12
	msgCopyInResponse    = 'G'
	msgCopyOutResponse   = 'H'
	msgNegotiateProtocol = 'v'
)

// Authentication sub-types carried in the body of an 'R' message.
const (
	authOK                = 0
	authCleartextPassword = 3
	authMD5Password       = 5
	authSASL              = 10
	authSASLContinue      = 11
	authSASLFinal         = 12
)

// protocolVersion is 3.0 encoded as (major<<16)|minor.
const protocolVersion = 196608

// sslRequestCode is the magic "version" that asks the server to start TLS.
const sslRequestCode = 80877103

// message is a single decoded backend message. body excludes the type byte and
// the 4-byte length prefix.
type message struct {
	typ  byte
	body []byte
}

// readMessage reads one backend message. It allocates a fresh body slice per
// message so callers may retain it.
func readMessage(r *bufio.Reader) (message, error) {
	var head [5]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return message{}, err
	}
	length := binary.BigEndian.Uint32(head[1:5])
	if length < 4 {
		return message{}, fmt.Errorf("pg: bogus message length %d for type %q", length, head[0])
	}
	if length > maxMessageSize {
		return message{}, fmt.Errorf("pg: message of %d bytes exceeds limit", length)
	}
	body := make([]byte, length-4)
	if _, err := io.ReadFull(r, body); err != nil {
		return message{}, err
	}
	return message{typ: head[0], body: body}, nil
}

// maxMessageSize guards against a hostile or corrupt server driving us to
// allocate unbounded memory. 256 MiB is far above any row we ever request.
const maxMessageSize = 256 << 20

// writeBuf incrementally builds a frontend message. Call start (or startUntyped
// for the startup/SSL packets), append fields, then done to patch in the
// length prefix.
type writeBuf struct {
	b      []byte
	lenPos int
	typed  bool
}

func (w *writeBuf) start(typ byte) {
	w.b = w.b[:0]
	w.b = append(w.b, typ)
	w.lenPos = len(w.b)
	w.b = append(w.b, 0, 0, 0, 0)
	w.typed = true
}

func (w *writeBuf) startUntyped() {
	w.b = w.b[:0]
	w.lenPos = 0
	w.b = append(w.b, 0, 0, 0, 0)
	w.typed = false
}

func (w *writeBuf) int32(v int32) {
	w.b = binary.BigEndian.AppendUint32(w.b, uint32(v))
}

func (w *writeBuf) int16(v int16) {
	w.b = binary.BigEndian.AppendUint16(w.b, uint16(v))
}

func (w *writeBuf) byte(v byte) {
	w.b = append(w.b, v)
}

// string appends a NUL-terminated C string.
func (w *writeBuf) string(s string) {
	w.b = append(w.b, s...)
	w.b = append(w.b, 0)
}

func (w *writeBuf) raw(p []byte) {
	w.b = append(w.b, p...)
}

// done patches the length field (which counts itself but not the type byte)
// and returns the complete packet.
func (w *writeBuf) done() []byte {
	n := len(w.b) - w.lenPos
	binary.BigEndian.PutUint32(w.b[w.lenPos:w.lenPos+4], uint32(n))
	return w.b
}

// readBuf consumes a message body field by field.
type readBuf struct {
	b   []byte
	err error
}

func (r *readBuf) int32() int32 {
	if len(r.b) < 4 {
		r.fail("int32")
		return 0
	}
	v := int32(binary.BigEndian.Uint32(r.b[:4]))
	r.b = r.b[4:]
	return v
}

func (r *readBuf) int16() int16 {
	if len(r.b) < 2 {
		r.fail("int16")
		return 0
	}
	v := int16(binary.BigEndian.Uint16(r.b[:2]))
	r.b = r.b[2:]
	return v
}

func (r *readBuf) byte() byte {
	if len(r.b) < 1 {
		r.fail("byte")
		return 0
	}
	v := r.b[0]
	r.b = r.b[1:]
	return v
}

// string reads a NUL-terminated C string.
func (r *readBuf) string() string {
	i := 0
	for i < len(r.b) && r.b[i] != 0 {
		i++
	}
	if i == len(r.b) {
		r.fail("cstring")
		return ""
	}
	s := string(r.b[:i])
	r.b = r.b[i+1:]
	return s
}

// next returns the next n bytes without copying.
func (r *readBuf) next(n int) []byte {
	if n < 0 || len(r.b) < n {
		r.fail("bytes")
		return nil
	}
	v := r.b[:n]
	r.b = r.b[n:]
	return v
}

func (r *readBuf) rest() []byte { v := r.b; r.b = nil; return v }

func (r *readBuf) fail(what string) {
	if r.err == nil {
		r.err = fmt.Errorf("pg: truncated message reading %s", what)
	}
	r.b = nil
}

// Error is a decoded ErrorResponse or NoticeResponse. The field codes are
// listed under "Error and Notice Message Fields" in the protocol docs.
type Error struct {
	Severity string
	Code     string
	Message  string
	Detail   string
	Hint     string
	Position string
	Where    string
	Schema   string
	Table    string
	Column   string
	Constrai string
}

func (e *Error) Error() string {
	var sb strings.Builder
	sb.WriteString("pg: ")
	if e.Severity != "" {
		sb.WriteString(e.Severity)
		sb.WriteString(" ")
	}
	if e.Code != "" {
		sb.WriteString(e.Code)
		sb.WriteString(": ")
	}
	sb.WriteString(e.Message)
	if e.Detail != "" {
		sb.WriteString(" (detail: ")
		sb.WriteString(e.Detail)
		sb.WriteString(")")
	}
	if e.Hint != "" {
		sb.WriteString(" (hint: ")
		sb.WriteString(e.Hint)
		sb.WriteString(")")
	}
	return sb.String()
}

// SQLState returns the five-character SQLSTATE code, or "" if absent.
func (e *Error) SQLState() string { return e.Code }

func parseError(body []byte) *Error {
	e := &Error{}
	r := &readBuf{b: body}
	for {
		code := r.byte()
		if code == 0 || r.err != nil {
			break
		}
		val := r.string()
		switch code {
		case 'S':
			e.Severity = val
		case 'C':
			e.Code = val
		case 'M':
			e.Message = val
		case 'D':
			e.Detail = val
		case 'H':
			e.Hint = val
		case 'P':
			e.Position = val
		case 'W':
			e.Where = val
		case 's':
			e.Schema = val
		case 't':
			e.Table = val
		case 'c':
			e.Column = val
		case 'n':
			e.Constrai = val
		}
	}
	if e.Message == "" {
		e.Message = "unknown server error"
	}
	return e
}
