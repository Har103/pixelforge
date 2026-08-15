package ws

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAcceptKeyMatchesRFC6455Example(t *testing.T) {
	// RFC 6455 section 1.3 worked example.
	if got, want := acceptKey("dGhlIHNhbXBsZSBub25jZQ=="), "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="; got != want {
		t.Errorf("acceptKey = %q, want %q", got, want)
	}
}

func TestHeaderContainsToken(t *testing.T) {
	h := http.Header{}
	h.Set("Connection", "keep-alive, Upgrade")
	if !headerContainsToken(h, "Connection", "upgrade") {
		t.Error("should match a token case-insensitively inside a comma list")
	}
	if headerContainsToken(h, "Connection", "close") {
		t.Error("matched a token that is not present")
	}
}

// --- a tiny client, so the tests exercise real frames on a real socket -------

type testClient struct {
	conn net.Conn
	br   *bufio.Reader
}

func dial(t *testing.T, srv *httptest.Server) *testClient {
	t.Helper()
	u := strings.TrimPrefix(srv.URL, "http://")
	conn, err := net.Dial("tcp", u)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	key := make([]byte, 16)
	_, _ = rand.Read(key)
	k := base64.StdEncoding.EncodeToString(key)

	req := "GET /ws HTTP/1.1\r\nHost: " + u + "\r\nUpgrade: websocket\r\n" +
		"Connection: Upgrade\r\nSec-WebSocket-Key: " + k + "\r\nSec-WebSocket-Version: 13\r\n\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
		t.Fatalf("handshake write: %v", err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("handshake read: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101", resp.StatusCode)
	}
	if got, want := resp.Header.Get("Sec-WebSocket-Accept"), acceptKey(k); got != want {
		t.Fatalf("accept = %q, want %q", got, want)
	}
	t.Cleanup(func() { conn.Close() })
	return &testClient{conn: conn, br: br}
}

// write sends a properly masked client frame.
func (c *testClient) write(t *testing.T, fin bool, opcode byte, payload []byte) {
	t.Helper()
	var head []byte
	b0 := opcode
	if fin {
		b0 |= 0x80
	}
	head = append(head, b0)
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
	_, _ = rand.Read(mask[:])
	head = append(head, mask[:]...)
	masked := make([]byte, n)
	for i := range payload {
		masked[i] = payload[i] ^ mask[i&3]
	}
	if _, err := c.conn.Write(append(head, masked...)); err != nil {
		t.Fatalf("client write: %v", err)
	}
}

// writeRaw sends bytes verbatim, for protocol-violation tests.
func (c *testClient) writeRaw(t *testing.T, b []byte) {
	t.Helper()
	if _, err := c.conn.Write(b); err != nil {
		t.Fatalf("client raw write: %v", err)
	}
}

// read returns the next server frame. Server frames are never masked.
func (c *testClient) read(t *testing.T) (byte, []byte) {
	t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var head [2]byte
	if _, err := io.ReadFull(c.br, head[:]); err != nil {
		t.Fatalf("client read header: %v", err)
	}
	if head[1]&0x80 != 0 {
		t.Fatal("server frame was masked, which RFC 6455 forbids")
	}
	length := int64(head[1] & 0x7F)
	switch length {
	case 126:
		var e [2]byte
		io.ReadFull(c.br, e[:])
		length = int64(binary.BigEndian.Uint16(e[:]))
	case 127:
		var e [8]byte
		io.ReadFull(c.br, e[:])
		length = int64(binary.BigEndian.Uint64(e[:]))
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(c.br, buf); err != nil {
		t.Fatalf("client read payload: %v", err)
	}
	return head[0] & 0x0F, buf
}

// echoServer upgrades and echoes every application message back.
func echoServer(t *testing.T, opts *Options) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := Upgrade(w, r, opts)
		if err != nil {
			return
		}
		defer c.Close()
		for {
			op, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			if err := c.WriteMessage(op, data); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestTextRoundTrip(t *testing.T) {
	c := dial(t, echoServer(t, nil))
	c.write(t, true, OpText, []byte("héllo wörld"))
	op, data := c.read(t)
	if op != OpText || string(data) != "héllo wörld" {
		t.Errorf("got op=%d %q", op, data)
	}
}

func TestBinaryRoundTripLargePayload(t *testing.T) {
	c := dial(t, echoServer(t, &Options{MaxMessageSize: 1 << 20}))
	// 70 KiB forces the 64-bit extended length path in both directions.
	payload := make([]byte, 70*1024)
	for i := range payload {
		payload[i] = byte(i * 7)
	}
	c.write(t, true, OpBinary, payload)
	op, got := c.read(t)
	if op != OpBinary {
		t.Fatalf("opcode = %d", op)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("payload mismatch: got %d bytes, want %d", len(got), len(payload))
	}
}

func TestFragmentedMessageIsReassembled(t *testing.T) {
	c := dial(t, echoServer(t, nil))
	c.write(t, false, OpText, []byte("frag"))
	c.write(t, false, OpContinuation, []byte("ment"))
	c.write(t, true, OpContinuation, []byte("ed"))
	op, data := c.read(t)
	if op != OpText || string(data) != "fragmented" {
		t.Errorf("got op=%d %q, want text %q", op, data, "fragmented")
	}
}

func TestPingIsAnsweredWithPong(t *testing.T) {
	c := dial(t, echoServer(t, nil))
	c.write(t, true, OpPing, []byte("beat"))
	op, data := c.read(t)
	if op != OpPong || string(data) != "beat" {
		t.Errorf("got op=%d %q, want pong %q", op, data, "beat")
	}
}

func TestUnmaskedClientFrameIsRejected(t *testing.T) {
	c := dial(t, echoServer(t, nil))
	// FIN + text, length 1, mask bit clear - a protocol violation.
	c.writeRaw(t, []byte{0x81, 0x01, 'x'})
	op, payload := c.read(t)
	if op != OpClose {
		t.Fatalf("expected a close frame, got opcode %d", op)
	}
	if code := binary.BigEndian.Uint16(payload[:2]); code != CloseProtocolError {
		t.Errorf("close code = %d, want %d", code, CloseProtocolError)
	}
}

func TestReservedBitsAreRejected(t *testing.T) {
	c := dial(t, echoServer(t, nil))
	// RSV1 set with no extension negotiated.
	c.writeRaw(t, []byte{0xC1, 0x81, 0, 0, 0, 0, 'x'})
	op, payload := c.read(t)
	if op != OpClose {
		t.Fatalf("expected a close frame, got opcode %d", op)
	}
	if code := binary.BigEndian.Uint16(payload[:2]); code != CloseProtocolError {
		t.Errorf("close code = %d, want %d", code, CloseProtocolError)
	}
}

func TestOversizedControlFrameIsRejected(t *testing.T) {
	c := dial(t, echoServer(t, nil))
	c.write(t, true, OpPing, bytes.Repeat([]byte("x"), 200))
	op, payload := c.read(t)
	if op != OpClose {
		t.Fatalf("expected a close frame, got opcode %d", op)
	}
	if code := binary.BigEndian.Uint16(payload[:2]); code != CloseProtocolError {
		t.Errorf("close code = %d, want %d", code, CloseProtocolError)
	}
}

func TestMessageOverLimitIsRejected(t *testing.T) {
	c := dial(t, echoServer(t, &Options{MaxMessageSize: 128}))
	c.write(t, true, OpBinary, bytes.Repeat([]byte("x"), 512))
	op, payload := c.read(t)
	if op != OpClose {
		t.Fatalf("expected a close frame, got opcode %d", op)
	}
	if code := binary.BigEndian.Uint16(payload[:2]); code != CloseMessageTooBig {
		t.Errorf("close code = %d, want %d", code, CloseMessageTooBig)
	}
}

func TestInvalidUTF8TextIsRejected(t *testing.T) {
	c := dial(t, echoServer(t, nil))
	c.write(t, true, OpText, []byte{0xff, 0xfe, 0xfd})
	op, payload := c.read(t)
	if op != OpClose {
		t.Fatalf("expected a close frame, got opcode %d", op)
	}
	if code := binary.BigEndian.Uint16(payload[:2]); code != CloseInvalidPayload {
		t.Errorf("close code = %d, want %d", code, CloseInvalidPayload)
	}
}

func TestCloseHandshake(t *testing.T) {
	c := dial(t, echoServer(t, nil))
	payload := make([]byte, 2)
	binary.BigEndian.PutUint16(payload, CloseNormal)
	c.write(t, true, OpClose, payload)
	op, got := c.read(t)
	if op != OpClose {
		t.Fatalf("expected a close frame back, got opcode %d", op)
	}
	if code := binary.BigEndian.Uint16(got[:2]); code != CloseNormal {
		t.Errorf("close code = %d, want %d", code, CloseNormal)
	}
}

func TestHandshakeRejectsBadRequests(t *testing.T) {
	srv := echoServer(t, nil)
	cases := []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"no upgrade header", map[string]string{"Connection": "Upgrade"}, http.StatusBadRequest},
		{"wrong version", map[string]string{
			"Connection": "Upgrade", "Upgrade": "websocket",
			"Sec-WebSocket-Key": "dGhlIHNhbXBsZSBub25jZQ==", "Sec-WebSocket-Version": "8",
		}, http.StatusUpgradeRequired},
		{"missing key", map[string]string{
			"Connection": "Upgrade", "Upgrade": "websocket", "Sec-WebSocket-Version": "13",
		}, http.StatusBadRequest},
		{"malformed key", map[string]string{
			"Connection": "Upgrade", "Upgrade": "websocket", "Sec-WebSocket-Version": "13",
			"Sec-WebSocket-Key": "not-base64!!",
		}, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest("GET", srv.URL+"/ws", nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

func TestConcurrentWritesDoNotInterleave(t *testing.T) {
	// Every frame must arrive intact even when many goroutines write at once.
	const writers = 8
	const each = 25

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		done := make(chan struct{})
		for i := 0; i < writers; i++ {
			go func(id int) {
				for j := 0; j < each; j++ {
					_ = c.WriteText([]byte(fmt.Sprintf("%d:%d", id, j)))
				}
				done <- struct{}{}
			}(i)
		}
		for i := 0; i < writers; i++ {
			<-done
		}
		time.Sleep(150 * time.Millisecond)
	}))
	t.Cleanup(srv.Close)

	c := dial(t, srv)
	seen := map[string]bool{}
	for i := 0; i < writers*each; i++ {
		op, data := c.read(t)
		if op != OpText {
			t.Fatalf("unexpected opcode %d", op)
		}
		s := string(data)
		if !strings.Contains(s, ":") {
			t.Fatalf("frame %q looks corrupted", s)
		}
		if seen[s] {
			t.Fatalf("duplicate frame %q", s)
		}
		seen[s] = true
	}
	if len(seen) != writers*each {
		t.Errorf("received %d distinct frames, want %d", len(seen), writers*each)
	}
}
