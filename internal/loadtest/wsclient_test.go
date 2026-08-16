package loadtest

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// A hand-written RFC 6455 client, because internal/ws is a server and the
// project takes no third-party modules. The handshake, masking and framing are
// the same approach as the DevTools client in internal/e2e/cdp_test.go; this
// one differs where a canvas client has to: it carries cookies (so the server
// knows which painter it is), it keeps binary frames instead of rejecting them
// (pixel batches are binary), and it is built to have several hundred of it
// alive at once.

const (
	wsOpContinuation = 0x0
	wsOpText         = 0x1
	wsOpBinary       = 0x2
	wsOpClose        = 0x8
	wsOpPing         = 0x9
	wsOpPong         = 0xA

	wsMagicGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

	// wsMaxFrame bounds one frame. A pixel batch is 3 + 5*n bytes and the hub
	// caps a batch at 65535 pixels, so a megabyte is generous; a corrupt length
	// must not become an enormous allocation.
	wsMaxFrame = 8 << 20
)

type wsClient struct {
	conn net.Conn
	br   *bufio.Reader

	// writeMu serialises frames. A client that pings from one goroutine while
	// painting from another would otherwise interleave two frames on the wire.
	writeMu sync.Mutex
}

// wsDial performs the opening handshake and verifies Sec-WebSocket-Accept, so a
// server that answers 101 without understanding the protocol is caught here
// rather than as garbled frames later.
func wsDial(rawURL, cookie string) (*wsClient, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parsing %q: %w", rawURL, err)
	}
	if u.Scheme != "ws" {
		return nil, fmt.Errorf("%q is not a ws:// URL", rawURL)
	}

	conn, err := net.DialTimeout("tcp", u.Host, 15*time.Second)
	if err != nil {
		return nil, err
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		// Pixel frames are small and latency is the thing being measured.
		_ = tc.SetNoDelay(true)
	}

	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		conn.Close()
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(nonce[:])

	req := "GET " + u.RequestURI() + " HTTP/1.1\r\n" +
		"Host: " + u.Host + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n"
	if cookie != "" {
		req += "Cookie: " + cookie + "\r\n"
	}
	req += "\r\n"

	_ = conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
	if _, err := io.WriteString(conn, req); err != nil {
		conn.Close()
		return nil, fmt.Errorf("sending the handshake: %w", err)
	}
	_ = conn.SetWriteDeadline(time.Time{})

	br := bufio.NewReaderSize(conn, 16<<10)
	_ = conn.SetReadDeadline(time.Now().Add(20 * time.Second))
	res, err := http.ReadResponse(br, nil)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("reading the handshake response: %w", err)
	}
	res.Body.Close()
	_ = conn.SetReadDeadline(time.Time{})

	if res.StatusCode != http.StatusSwitchingProtocols {
		conn.Close()
		return nil, fmt.Errorf("handshake answered %s, want 101", res.Status)
	}
	if got, want := res.Header.Get("Sec-WebSocket-Accept"), wsAcceptKey(key); got != want {
		conn.Close()
		return nil, fmt.Errorf("Sec-WebSocket-Accept = %q, want %q", got, want)
	}
	return &wsClient{conn: conn, br: br}, nil
}

// wsAcceptKey is base64(SHA-1(key + GUID)) from RFC 6455 section 1.3.
func wsAcceptKey(key string) string {
	h := sha1.New()
	io.WriteString(h, key)
	io.WriteString(h, wsMagicGUID)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func (c *wsClient) close() { _ = c.conn.Close() }

// shrinkReceiveBuffer makes this client genuinely slow rather than merely
// inattentive.
//
// A client that simply stops calling read is not slow from the server's point
// of view: the kernel keeps acknowledging data into a receive buffer that is
// megabytes wide on loopback, so the server's writes never block, the
// subscriber's 32-frame channel never fills, and the hub never notices. Closing
// the receive window down to a few kilobytes is what makes the backpressure
// reach the server, which is the condition hub.broadcast's "client too slow"
// path is actually written for.
func (c *wsClient) shrinkReceiveBuffer(bytes int) error {
	tc, ok := c.conn.(*net.TCPConn)
	if !ok {
		return errors.New("not a TCP connection")
	}
	return tc.SetReadBuffer(bytes)
}

// abort cuts the socket without a close frame, which is what a browser tab
// being killed looks like to the server. The churn experiment wants exactly
// this rather than a polite goodbye.
func (c *wsClient) abort() {
	if tc, ok := c.conn.(*net.TCPConn); ok {
		_ = tc.SetLinger(0)
	}
	_ = c.conn.Close()
}

// writeText sends one masked text message. RFC 6455 requires a client to mask
// every frame it sends, and internal/ws enforces it.
func (c *wsClient) writeText(payload []byte) error {
	return c.writeFrame(wsOpText, payload)
}

func (c *wsClient) writeFrame(opcode byte, payload []byte) error {
	head := make([]byte, 0, 14)
	head = append(head, 0x80|opcode)

	n := len(payload)
	switch {
	case n <= 125:
		head = append(head, byte(n)|0x80)
	case n <= 0xFFFF:
		head = append(head, 126|0x80)
		head = binary.BigEndian.AppendUint16(head, uint16(n))
	default:
		head = append(head, 127|0x80)
		head = binary.BigEndian.AppendUint64(head, uint64(n))
	}

	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	head = append(head, mask[:]...)

	frame := make([]byte, 0, len(head)+n)
	frame = append(frame, head...)
	for i := range payload {
		frame = append(frame, payload[i]^mask[i&3])
	}

	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.conn.SetWriteDeadline(time.Now().Add(20 * time.Second)); err != nil {
		return err
	}
	_, err := c.conn.Write(frame)
	return err
}

// wsMessage is one complete application message.
type wsMessage struct {
	binary  bool
	payload []byte
}

// read returns the next complete message, answering pings and reassembling
// fragments on the way.
func (c *wsClient) read(deadline time.Time) (wsMessage, error) {
	var assembled []byte
	fragging := false
	fragBinary := false

	for {
		if err := c.conn.SetReadDeadline(deadline); err != nil {
			return wsMessage{}, err
		}
		f, err := c.readFrame()
		if err != nil {
			return wsMessage{}, err
		}

		switch f.opcode {
		case wsOpPing:
			if err := c.writeFrame(wsOpPong, f.payload); err != nil {
				return wsMessage{}, err
			}

		case wsOpPong:
			// Nothing asked for it; ignore it.

		case wsOpClose:
			code := 0
			if len(f.payload) >= 2 {
				code = int(binary.BigEndian.Uint16(f.payload[:2]))
			}
			return wsMessage{}, fmt.Errorf("server closed the socket (code %d)", code)

		case wsOpText, wsOpBinary:
			if fragging {
				return wsMessage{}, errors.New("a data frame arrived mid-fragment")
			}
			if f.fin {
				return wsMessage{binary: f.opcode == wsOpBinary, payload: f.payload}, nil
			}
			fragging = true
			fragBinary = f.opcode == wsOpBinary
			assembled = append(assembled[:0], f.payload...)

		case wsOpContinuation:
			if !fragging {
				return wsMessage{}, errors.New("a continuation frame arrived with nothing to continue")
			}
			if len(assembled)+len(f.payload) > wsMaxFrame {
				return wsMessage{}, fmt.Errorf("a fragmented message passed the %d byte limit", wsMaxFrame)
			}
			assembled = append(assembled, f.payload...)
			if f.fin {
				return wsMessage{binary: fragBinary, payload: assembled}, nil
			}

		default:
			return wsMessage{}, fmt.Errorf("unknown opcode 0x%x", f.opcode)
		}
	}
}

type wsFrame struct {
	fin     bool
	opcode  byte
	payload []byte
}

func (c *wsClient) readFrame() (wsFrame, error) {
	var head [2]byte
	if _, err := io.ReadFull(c.br, head[:]); err != nil {
		return wsFrame{}, err
	}

	fin := head[0]&0x80 != 0
	opcode := head[0] & 0x0F
	masked := head[1]&0x80 != 0
	length := int64(head[1] & 0x7F)

	// A server must never mask (RFC 6455 section 5.1). Unmasking anyway would
	// paper over a peer that has the protocol wrong.
	if masked {
		return wsFrame{}, errors.New("the server masked a frame, which RFC 6455 forbids")
	}

	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return wsFrame{}, err
		}
		length = int64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return wsFrame{}, err
		}
		length = int64(binary.BigEndian.Uint64(ext[:]))
	}
	if length < 0 || length > wsMaxFrame {
		return wsFrame{}, fmt.Errorf("frame of %d bytes passes the %d byte limit", length, wsMaxFrame)
	}

	payload := make([]byte, length)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return wsFrame{}, err
	}
	return wsFrame{fin: fin, opcode: opcode, payload: payload}, nil
}

// ---------------------------------------------------- canvas-level helpers --

// sendPlace asks the server to paint a cell over the socket, which is the path
// the real front end uses and the one that saves a round trip per pixel.
func (c *wsClient) sendPlace(x, y, colour int) error {
	msg, _ := json.Marshal(map[string]any{"t": "place", "x": x, "y": y, "c": colour})
	return c.writeText(msg)
}

func (c *wsClient) sendCursor(x, y, colour int) error {
	msg, _ := json.Marshal(map[string]any{"t": "cur", "x": x, "y": y, "c": colour})
	return c.writeText(msg)
}

// pixelBatch decodes hub.KindPixelBatch: 0x01 | count u16 | (x u16, y u16,
// colour u8) repeated. The count field saturates at 0xFFFF, so the payload
// length is the honest source of how many pixels arrived.
func pixelBatch(payload []byte) ([]batchPixel, bool) {
	if len(payload) < 3 || payload[0] != 0x01 {
		return nil, false
	}
	body := payload[3:]
	n := len(body) / 5
	out := make([]batchPixel, 0, n)
	for i := 0; i < n; i++ {
		off := i * 5
		out = append(out, batchPixel{
			X: int(binary.BigEndian.Uint16(body[off : off+2])),
			Y: int(binary.BigEndian.Uint16(body[off+2 : off+4])),
			C: body[off+4],
		})
	}
	return out, true
}

type batchPixel struct {
	X, Y int
	C    byte
}

// wsURL builds the socket address for a room on a listener.
func wsURL(hostPort, slug string) string {
	return "ws://" + hostPort + "/api/r/" + slug + "/ws"
}
