// Package ws is a minimal RFC 6455 WebSocket server built on net/http's
// Hijacker. No third-party modules: the handshake, the frame codec, masking,
// fragmentation and the control-frame rules are all implemented here.
//
// It deliberately supports only what Pixelforge needs - server side, no
// extensions, no compression, no subprotocol negotiation - which keeps the
// whole thing auditable in one sitting.
package ws

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Opcodes from RFC 6455 section 5.2.
const (
	OpContinuation = 0x0
	OpText         = 0x1
	OpBinary       = 0x2
	OpClose        = 0x8
	OpPing         = 0x9
	OpPong         = 0xA
)

// Close status codes from RFC 6455 section 7.4.1.
const (
	CloseNormal            = 1000
	CloseGoingAway         = 1001
	CloseProtocolError     = 1002
	CloseUnsupportedData   = 1003
	CloseInvalidPayload    = 1007
	ClosePolicyViolation   = 1008
	CloseMessageTooBig     = 1009
	CloseInternalServerErr = 1011
)

// magicGUID is the fixed string concatenated with the client key before hashing,
// defined in RFC 6455 section 1.3.
const magicGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// ErrClosed is returned once the peer has closed the connection cleanly.
var ErrClosed = errors.New("ws: connection closed")

// Conn is an established WebSocket. Reads must come from a single goroutine;
// writes are serialised internally so any goroutine may call WriteMessage.
type Conn struct {
	conn net.Conn
	br   *bufio.Reader

	wmu sync.Mutex
	cmu sync.Mutex

	maxMessage   int64
	writeTimeout time.Duration

	closed    bool
	closeOnce sync.Once

	// fragment reassembly state
	fragOp   byte
	fragBuf  []byte
	fragging bool
}

// Options tunes an upgraded connection.
type Options struct {
	// MaxMessageSize caps a single (possibly fragmented) message. Default 1 MiB.
	MaxMessageSize int64
	// WriteTimeout bounds a single frame write. Default 10s.
	WriteTimeout time.Duration
	// ReadBufferSize sizes the bufio.Reader. Default 4096.
	ReadBufferSize int
}

func (o *Options) withDefaults() Options {
	out := Options{MaxMessageSize: 1 << 20, WriteTimeout: 10 * time.Second, ReadBufferSize: 4096}
	if o != nil {
		if o.MaxMessageSize > 0 {
			out.MaxMessageSize = o.MaxMessageSize
		}
		if o.WriteTimeout > 0 {
			out.WriteTimeout = o.WriteTimeout
		}
		if o.ReadBufferSize > 0 {
			out.ReadBufferSize = o.ReadBufferSize
		}
	}
	return out
}

// Upgrade completes the opening handshake and takes ownership of the socket.
// On failure it has already written an HTTP error response.
func Upgrade(w http.ResponseWriter, r *http.Request, opts *Options) (*Conn, error) {
	o := opts.withDefaults()

	if r.Method != http.MethodGet {
		http.Error(w, "websocket: GET required", http.StatusMethodNotAllowed)
		return nil, fmt.Errorf("ws: method %s", r.Method)
	}
	if !headerContainsToken(r.Header, "Connection", "upgrade") {
		http.Error(w, "websocket: Connection header must contain 'upgrade'", http.StatusBadRequest)
		return nil, errors.New("ws: missing Connection: upgrade")
	}
	if !strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket") {
		http.Error(w, "websocket: Upgrade header must be 'websocket'", http.StatusBadRequest)
		return nil, errors.New("ws: missing Upgrade: websocket")
	}
	if r.Header.Get("Sec-Websocket-Version") != "13" {
		w.Header().Set("Sec-WebSocket-Version", "13")
		http.Error(w, "websocket: only version 13 is supported", http.StatusUpgradeRequired)
		return nil, errors.New("ws: unsupported version")
	}
	key := r.Header.Get("Sec-Websocket-Key")
	if key == "" {
		http.Error(w, "websocket: missing Sec-WebSocket-Key", http.StatusBadRequest)
		return nil, errors.New("ws: missing key")
	}
	// The key must be 16 random bytes, base64 encoded.
	if raw, err := base64.StdEncoding.DecodeString(key); err != nil || len(raw) != 16 {
		http.Error(w, "websocket: malformed Sec-WebSocket-Key", http.StatusBadRequest)
		return nil, errors.New("ws: malformed key")
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "websocket: connection cannot be hijacked", http.StatusInternalServerError)
		return nil, errors.New("ws: ResponseWriter is not a Hijacker (HTTP/2?)")
	}
	netConn, brw, err := hj.Hijack()
	if err != nil {
		http.Error(w, "websocket: hijack failed", http.StatusInternalServerError)
		return nil, fmt.Errorf("ws: hijack: %w", err)
	}
	// Anything the client pipelined before the handshake completed is a
	// protocol violation for us; refuse rather than silently dropping it.
	if brw.Reader.Buffered() > 0 {
		netConn.Close()
		return nil, errors.New("ws: client sent data before handshake completed")
	}

	accept := acceptKey(key)
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"

	_ = netConn.SetWriteDeadline(time.Now().Add(o.WriteTimeout))
	if _, err := io.WriteString(netConn, resp); err != nil {
		netConn.Close()
		return nil, fmt.Errorf("ws: writing handshake response: %w", err)
	}
	_ = netConn.SetWriteDeadline(time.Time{})

	return &Conn{
		conn:         netConn,
		br:           bufio.NewReaderSize(netConn, o.ReadBufferSize),
		maxMessage:   o.MaxMessageSize,
		writeTimeout: o.WriteTimeout,
	}, nil
}

// acceptKey computes the Sec-WebSocket-Accept value: base64(SHA-1(key + GUID)).
func acceptKey(key string) string {
	h := sha1.New()
	io.WriteString(h, key)
	io.WriteString(h, magicGUID)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// headerContainsToken reports whether a comma-separated header lists a token,
// case-insensitively. "keep-alive, Upgrade" must match "upgrade".
func headerContainsToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, part := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

// RemoteAddr reports the peer address.
func (c *Conn) RemoteAddr() net.Addr { return c.conn.RemoteAddr() }

// SetReadDeadline bounds how long the next ReadMessage may block.
func (c *Conn) SetReadDeadline(t time.Time) error { return c.conn.SetReadDeadline(t) }

// ReadMessage returns the next complete application message, transparently
// reassembling fragments and answering ping and close frames. Control frames
// never surface to the caller.
func (c *Conn) ReadMessage() (opcode byte, payload []byte, err error) {
	for {
		f, err := c.readFrame()
		if err != nil {
			return 0, nil, err
		}

		switch f.opcode {
		case OpPing:
			if err := c.WriteControl(OpPong, f.payload); err != nil {
				return 0, nil, err
			}
			continue

		case OpPong:
			continue

		case OpClose:
			code := CloseNormal
			var reason string
			if len(f.payload) >= 2 {
				code = int(binary.BigEndian.Uint16(f.payload[:2]))
				reason = string(f.payload[2:])
				if !utf8.ValidString(reason) {
					_ = c.closeWith(CloseInvalidPayload, "")
					return 0, nil, errors.New("ws: close reason is not valid UTF-8")
				}
			} else if len(f.payload) == 1 {
				_ = c.closeWith(CloseProtocolError, "")
				return 0, nil, errors.New("ws: close frame with 1-byte payload")
			}
			_ = c.closeWith(CloseNormal, "")
			if reason != "" {
				return 0, nil, fmt.Errorf("%w: peer sent %d (%s)", ErrClosed, code, reason)
			}
			return 0, nil, fmt.Errorf("%w: peer sent %d", ErrClosed, code)

		case OpText, OpBinary:
			if c.fragging {
				_ = c.closeWith(CloseProtocolError, "interleaved data frame")
				return 0, nil, errors.New("ws: data frame received mid-fragment")
			}
			if f.fin {
				if f.opcode == OpText && !utf8.Valid(f.payload) {
					_ = c.closeWith(CloseInvalidPayload, "")
					return 0, nil, errors.New("ws: text frame is not valid UTF-8")
				}
				return f.opcode, f.payload, nil
			}
			c.fragging = true
			c.fragOp = f.opcode
			c.fragBuf = append(c.fragBuf[:0], f.payload...)

		case OpContinuation:
			if !c.fragging {
				_ = c.closeWith(CloseProtocolError, "unexpected continuation")
				return 0, nil, errors.New("ws: continuation frame with nothing to continue")
			}
			if int64(len(c.fragBuf)+len(f.payload)) > c.maxMessage {
				_ = c.closeWith(CloseMessageTooBig, "")
				return 0, nil, errors.New("ws: fragmented message exceeds limit")
			}
			c.fragBuf = append(c.fragBuf, f.payload...)
			if f.fin {
				op := c.fragOp
				msg := append([]byte(nil), c.fragBuf...)
				c.fragging = false
				c.fragBuf = c.fragBuf[:0]
				if op == OpText && !utf8.Valid(msg) {
					_ = c.closeWith(CloseInvalidPayload, "")
					return 0, nil, errors.New("ws: text message is not valid UTF-8")
				}
				return op, msg, nil
			}

		default:
			_ = c.closeWith(CloseProtocolError, "unknown opcode")
			return 0, nil, fmt.Errorf("ws: unknown opcode 0x%x", f.opcode)
		}
	}
}

type frame struct {
	fin     bool
	opcode  byte
	payload []byte
}

func (c *Conn) readFrame() (frame, error) {
	var head [2]byte
	if _, err := io.ReadFull(c.br, head[:]); err != nil {
		return frame{}, err
	}

	fin := head[0]&0x80 != 0
	rsv := head[0] & 0x70
	opcode := head[0] & 0x0F
	masked := head[1]&0x80 != 0
	length := int64(head[1] & 0x7F)

	// We negotiate no extensions, so any reserved bit set is a protocol error.
	if rsv != 0 {
		_ = c.closeWith(CloseProtocolError, "reserved bits set")
		return frame{}, errors.New("ws: reserved bits set without a negotiated extension")
	}
	// RFC 6455 5.1: a client MUST mask every frame it sends.
	if !masked {
		_ = c.closeWith(CloseProtocolError, "unmasked client frame")
		return frame{}, errors.New("ws: client frame was not masked")
	}

	isControl := opcode&0x08 != 0
	if isControl {
		// Control frames must be short and must not be fragmented.
		if length > 125 {
			_ = c.closeWith(CloseProtocolError, "oversized control frame")
			return frame{}, errors.New("ws: control frame longer than 125 bytes")
		}
		if !fin {
			_ = c.closeWith(CloseProtocolError, "fragmented control frame")
			return frame{}, errors.New("ws: fragmented control frame")
		}
	}

	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return frame{}, err
		}
		length = int64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return frame{}, err
		}
		v := binary.BigEndian.Uint64(ext[:])
		if v > 1<<62 {
			return frame{}, errors.New("ws: absurd frame length")
		}
		length = int64(v)
	}

	if length > c.maxMessage {
		_ = c.closeWith(CloseMessageTooBig, "")
		return frame{}, fmt.Errorf("ws: frame of %d bytes exceeds the %d byte limit", length, c.maxMessage)
	}

	var mask [4]byte
	if _, err := io.ReadFull(c.br, mask[:]); err != nil {
		return frame{}, err
	}

	payload := make([]byte, length)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return frame{}, err
	}
	for i := range payload {
		payload[i] ^= mask[i&3]
	}

	return frame{fin: fin, opcode: opcode, payload: payload}, nil
}

// WriteMessage sends a complete unfragmented message.
func (c *Conn) WriteMessage(opcode byte, payload []byte) error {
	return c.writeFrame(true, opcode, payload)
}

// WriteText is shorthand for a text message.
func (c *Conn) WriteText(s []byte) error { return c.WriteMessage(OpText, s) }

// WriteBinary is shorthand for a binary message.
func (c *Conn) WriteBinary(b []byte) error { return c.WriteMessage(OpBinary, b) }

// WriteControl sends a control frame. Payload must be 125 bytes or fewer.
func (c *Conn) WriteControl(opcode byte, payload []byte) error {
	if len(payload) > 125 {
		payload = payload[:125]
	}
	return c.writeFrame(true, opcode, payload)
}

// Ping sends a ping frame; the peer's pong is consumed by ReadMessage.
func (c *Conn) Ping() error { return c.WriteControl(OpPing, nil) }

func (c *Conn) writeFrame(fin bool, opcode byte, payload []byte) error {
	c.cmu.Lock()
	closed := c.closed
	c.cmu.Unlock()
	if closed {
		return ErrClosed
	}

	// Server-to-client frames are never masked (RFC 6455 5.1).
	var head [10]byte
	head[0] = opcode
	if fin {
		head[0] |= 0x80
	}
	n := len(payload)
	var headLen int
	switch {
	case n <= 125:
		head[1] = byte(n)
		headLen = 2
	case n <= 0xFFFF:
		head[1] = 126
		binary.BigEndian.PutUint16(head[2:4], uint16(n))
		headLen = 4
	default:
		head[1] = 127
		binary.BigEndian.PutUint64(head[2:10], uint64(n))
		headLen = 10
	}

	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := c.conn.SetWriteDeadline(time.Now().Add(c.writeTimeout)); err != nil {
		return err
	}
	// One Write for the whole frame: two syscalls per message would interleave
	// badly under concurrent writers even with the mutex held.
	buf := make([]byte, 0, headLen+n)
	buf = append(buf, head[:headLen]...)
	buf = append(buf, payload...)
	if _, err := c.conn.Write(buf); err != nil {
		c.markClosed()
		return err
	}
	return nil
}

// Close sends a normal close frame and shuts the socket.
func (c *Conn) Close() error { return c.closeWith(CloseNormal, "") }

// CloseWith sends a close frame carrying a specific status code and reason.
func (c *Conn) CloseWith(code int, reason string) error { return c.closeWith(code, reason) }

func (c *Conn) closeWith(code int, reason string) error {
	var err error
	c.closeOnce.Do(func() {
		if len(reason) > 123 {
			reason = reason[:123]
		}
		payload := make([]byte, 2+len(reason))
		binary.BigEndian.PutUint16(payload[:2], uint16(code))
		copy(payload[2:], reason)
		_ = c.writeFrame(true, OpClose, payload)
		c.markClosed()
		err = c.conn.Close()
	})
	return err
}

func (c *Conn) markClosed() {
	c.cmu.Lock()
	c.closed = true
	c.cmu.Unlock()
}

// IsClosed reports whether the connection has been shut down.
func (c *Conn) IsClosed() bool {
	c.cmu.Lock()
	defer c.cmu.Unlock()
	return c.closed
}
