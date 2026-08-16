// Package e2e drives a real headless Chromium against a real server, because a
// class of Pixelforge bug exists only inside a browser: a Content Security
// Policy that silently refuses the script carrying the room slug, a "hidden"
// overlay that still swallows every click, a topbar that overflows a phone.
// None of those can fail a Go test of the handlers, and all three have shipped.
//
// This file is the driver - finding a browser, launching it, speaking WebSocket
// to it and the dozen DevTools calls the tests need. It is standard library
// like everything else here: the WebSocket client below is the client-side twin
// of internal/ws, masked frames and all.
//
// A session is driven entirely from the test's own goroutine, so nothing in
// here is synchronised beyond the pipe that collects the browser's stderr.
package e2e

import (
	"bufio"
	"bytes"
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
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// How long the driver is prepared to wait for various things. A DevTools round
// trip is normally sub-millisecond; these are the bounds at which we would
// rather see a named failure than a hung test.
const (
	callTimeout   = 30 * time.Second
	launchTimeout = 30 * time.Second
	readyTimeout  = 20 * time.Second
	pollInterval  = 50 * time.Millisecond
)

// ------------------------------------------------------------- the browser --

// chromeCandidates are tried in order when PIXELFORGE_E2E_CHROME is unset. The
// first is where this project's container keeps its browser; the rest are what
// a developer's machine is likely to call one.
var chromeCandidates = []string{
	"/opt/pw-browsers/chromium",
	"chromium",
	"chromium-browser",
	"google-chrome",
	"google-chrome-stable",
}

const noBrowserSkip = "no Chromium found, so the browser suite is skipped: " +
	"point PIXELFORGE_E2E_CHROME at a Chrome or Chromium binary, or put one on " +
	"PATH as chromium, chromium-browser, google-chrome or google-chrome-stable"

// findChrome resolves the browser to drive, or skips the test.
//
// An explicit PIXELFORGE_E2E_CHROME is treated as the only candidate rather
// than as a preference. Pinning a binary and quietly getting a different one is
// the opposite of what somebody sets that variable for, and a browser suite
// that tests the wrong browser is worse than one that does not run.
func findChrome(t *testing.T) string {
	t.Helper()
	if pinned := strings.TrimSpace(os.Getenv("PIXELFORGE_E2E_CHROME")); pinned != "" {
		path, err := exec.LookPath(pinned)
		if err != nil {
			t.Skipf("PIXELFORGE_E2E_CHROME=%s is not a runnable browser (%v), so the browser suite is skipped", pinned, err)
		}
		return path
	}
	for _, candidate := range chromeCandidates {
		if path, err := exec.LookPath(candidate); err == nil {
			return path
		}
	}
	t.Skip(noBrowserSkip)
	return ""
}

// chrome is a running headless browser and the DevTools endpoint it listens on.
type chrome struct {
	path   string
	port   int
	client *http.Client

	// browser is the session attached to the browser itself rather than to a
	// page, created on first use. See browserSession.
	browser *page
}

// launchChrome starts a browser and waits until it answers on its DevTools
// port. The process is always killed and reaped from a cleanup, so a test that
// fails halfway through does not leave a browser behind.
func launchChrome(t *testing.T) *chrome {
	t.Helper()
	path := findChrome(t)

	watch := &stderrWatch{ready: make(chan int, 1)}
	cmd := exec.Command(path,
		"--headless=new",
		"--disable-gpu",
		"--no-sandbox",
		// A profile per test keeps cookies, storage and Chrome's singleton lock
		// from leaking between runs, and t.TempDir removes it afterwards.
		"--user-data-dir="+t.TempDir(),
		// Port zero asks the kernel for a free one. A fixed 9222 would make two
		// runs on one machine fight, and CI runs packages in parallel.
		"--remote-debugging-port=0",
		"about:blank",
	)
	// Chrome announces its DevTools port on stderr and nowhere else, so stderr
	// is a value we own rather than a pipe: an io.Writer sidesteps the rule that
	// Wait must not run until every read from StderrPipe has finished.
	cmd.Stderr = watch

	if err := cmd.Start(); err != nil {
		t.Fatalf("starting %s: %v", path, err)
	}

	var waitErr error
	exited := make(chan struct{})
	go func() { waitErr = cmd.Wait(); close(exited) }()

	t.Cleanup(func() {
		// Interrupt first: a browser that is killed outright can leave its
		// renderer children orphaned, and this test binary would then wait on
		// pipes nobody is going to close.
		_ = cmd.Process.Signal(os.Interrupt)
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-exited
		}
	})

	c := &chrome{path: path, client: &http.Client{Timeout: 10 * time.Second}}

	select {
	case c.port = <-watch.ready:
	case <-exited:
		t.Fatalf("%s exited before announcing a DevTools port (%v)\n%s", path, waitErr, watch.tail())
	case <-time.After(launchTimeout):
		t.Fatalf("%s did not announce a DevTools port within %s\n%s", path, launchTimeout, watch.tail())
	}

	// The port is open before the browser is willing to serve on it, so the
	// announcement is a starting gun rather than a readiness signal.
	deadline := time.Now().Add(readyTimeout)
	for {
		if _, err := c.getJSON("/json/version"); err == nil {
			return c
		} else if time.Now().After(deadline) {
			t.Fatalf("%s never answered /json/version: %v\n%s", path, err, watch.tail())
		}
		select {
		case <-exited:
			t.Fatalf("%s exited while starting up (%v)\n%s", path, waitErr, watch.tail())
		case <-time.After(pollInterval):
		}
	}
}

// stderrWatch scans Chrome's stderr for the line announcing the DevTools port
// and keeps a bounded tail of everything else, so a browser that dies on
// startup can say why instead of timing out silently.
type stderrWatch struct {
	mu      sync.Mutex
	partial []byte
	lines   []string
	found   bool
	ready   chan int
}

const stderrTailLines = 25

func (w *stderrWatch) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.partial = append(w.partial, p...)
	for {
		i := bytes.IndexByte(w.partial, '\n')
		if i < 0 {
			break
		}
		w.line(string(bytes.TrimRight(w.partial[:i], "\r")))
		w.partial = w.partial[i+1:]
	}
	// A process that writes megabytes without a newline must not be able to
	// grow this buffer without bound.
	if len(w.partial) > 64<<10 {
		w.partial = w.partial[:0]
	}
	return len(p), nil
}

func (w *stderrWatch) line(s string) {
	w.lines = append(w.lines, s)
	if len(w.lines) > stderrTailLines {
		w.lines = w.lines[len(w.lines)-stderrTailLines:]
	}
	if w.found {
		return
	}
	if port, ok := devToolsPort(s); ok {
		w.found = true
		w.ready <- port
	}
}

func (w *stderrWatch) tail() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.lines) == 0 {
		return "(the browser printed nothing on stderr)"
	}
	return "browser stderr:\n  " + strings.Join(w.lines, "\n  ")
}

// devToolsPort pulls the port out of Chrome's announcement, which reads
//
//	DevTools listening on ws://127.0.0.1:41235/devtools/browser/<uuid>
func devToolsPort(line string) (int, bool) {
	const marker = "DevTools listening on ws://"
	i := strings.Index(line, marker)
	if i < 0 {
		return 0, false
	}
	host, _, ok := strings.Cut(line[i+len(marker):], "/")
	if !ok {
		return 0, false
	}
	_, portText, err := net.SplitHostPort(host)
	if err != nil {
		return 0, false
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port <= 0 {
		return 0, false
	}
	return port, true
}

func (c *chrome) endpoint(path string) string {
	return "http://127.0.0.1:" + strconv.Itoa(c.port) + path
}

func (c *chrome) getJSON(path string) ([]byte, error) {
	return c.request(http.MethodGet, path)
}

func (c *chrome) request(method, path string) ([]byte, error) {
	req, err := http.NewRequest(method, c.endpoint(path), nil)
	if err != nil {
		return nil, err
	}
	res, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s %s: %s: %s", method, path, res.Status, strings.TrimSpace(string(body)))
	}
	return body, nil
}

// target is one page in the browser.
type target struct {
	ID  string `json:"id"`
	URL string `json:"webSocketDebuggerUrl"`
}

// newPage opens a tab and attaches to it, returning a session that speaks CDP.
func (c *chrome) newPage(t *testing.T) *page {
	t.Helper()

	// Chrome 111 and later insist on PUT for /json/new; older builds only
	// answer GET. Trying both keeps the suite working either side of that.
	var problems []string
	var tg target
	for _, method := range []string{http.MethodPut, http.MethodGet} {
		body, err := c.request(method, "/json/new?about:blank")
		if err != nil {
			problems = append(problems, method+": "+err.Error())
			continue
		}
		if err := json.Unmarshal(body, &tg); err != nil {
			problems = append(problems, method+": "+err.Error())
			continue
		}
		if tg.URL == "" {
			problems = append(problems, method+": the reply carried no webSocketDebuggerUrl")
			continue
		}
		break
	}
	if tg.URL == "" {
		t.Fatalf("could not open a page in %s: %s", c.path, strings.Join(problems, "; "))
	}

	conn, err := wsDial(tg.URL)
	if err != nil {
		t.Fatalf("connecting to the page's DevTools socket: %v", err)
	}
	p := &page{ws: conn}
	t.Cleanup(func() {
		conn.close()
		// Closing the tab drops the page's own WebSocket to the test server,
		// which has to happen before the server is asked to shut down.
		_, _ = c.getJSON("/json/close/" + tg.ID)
	})

	p.listen(t)
	return p
}

// listen turns on the two domains every page in this suite wants.
//
// Runtime carries console output and uncaught exceptions. Log carries the
// browser's own complaints - a blocked script, a refused connection, a resource
// that never arrived - and a Content Security Policy violation appears there
// and nowhere else, which is precisely the failure that once shipped unnoticed.
func (p *page) listen(t *testing.T) {
	t.Helper()
	p.mustCall(t, "Runtime.enable", nil)
	p.mustCall(t, "Log.enable", nil)
}

// browserSession attaches to the browser's own DevTools endpoint, once.
//
// The Target domain lives on the browser rather than on a page, and a fresh
// browser context - a separate cookie jar - is the only way two tabs can be two
// different people. Every identity in this application is a cookie, so without
// one a second tab is the same painter twice: it would see its own cursor
// filtered out as if the filter worked, and it could never lose a race for its
// own pixel.
func (c *chrome) browserSession(t *testing.T) *page {
	t.Helper()
	if c.browser != nil {
		return c.browser
	}
	raw, err := c.getJSON("/json/version")
	if err != nil {
		t.Fatalf("asking %s where its browser endpoint is: %v", c.path, err)
	}
	var v struct {
		URL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.Unmarshal(raw, &v); err != nil || v.URL == "" {
		t.Fatalf("/json/version named no browser WebSocket: %s", raw)
	}
	conn, err := wsDial(v.URL)
	if err != nil {
		t.Fatalf("attaching to the browser: %v", err)
	}
	t.Cleanup(conn.close)
	c.browser = &page{ws: conn}
	return c.browser
}

// newIsolatedPage opens a tab in its own browser context: a separate cookie
// jar, and therefore a separate painter.
func (c *chrome) newIsolatedPage(t *testing.T) *page {
	t.Helper()
	b := c.browserSession(t)

	var ctx struct {
		ID string `json:"browserContextId"`
	}
	// disposeOnDetach so a run that dies mid-test cannot leave the context
	// behind in a browser somebody is still using.
	decode(t, b.mustCall(t, "Target.createBrowserContext", map[string]any{"disposeOnDetach": true}),
		&ctx, "the new browser context")
	if ctx.ID == "" {
		t.Fatal("Target.createBrowserContext returned no context id")
	}

	var tgt struct {
		ID string `json:"targetId"`
	}
	decode(t, b.mustCall(t, "Target.createTarget", map[string]any{
		"url": "about:blank", "browserContextId": ctx.ID,
	}), &tgt, "the new target")
	if tgt.ID == "" {
		t.Fatal("Target.createTarget returned no target id")
	}

	conn, err := wsDial(fmt.Sprintf("ws://127.0.0.1:%d/devtools/page/%s", c.port, tgt.ID))
	if err != nil {
		t.Fatalf("connecting to the isolated page's DevTools socket: %v", err)
	}
	t.Cleanup(func() {
		conn.close()
		_, _ = b.call("Target.closeTarget", map[string]any{"targetId": tgt.ID})
		_, _ = b.call("Target.disposeBrowserContext", map[string]any{"browserContextId": ctx.ID})
	})

	p := &page{ws: conn}
	p.listen(t)
	return p
}

func decode(t *testing.T, raw json.RawMessage, dst any, what string) {
	t.Helper()
	if err := json.Unmarshal(raw, dst); err != nil {
		t.Fatalf("decoding %s from %s: %v", what, raw, err)
	}
}

// ---------------------------------------------------- a WebSocket client ----

// Opcodes and the handshake constant from RFC 6455, repeated here rather than
// borrowed from internal/ws: that package is the server, and a client that
// shares the server's idea of the protocol cannot notice the two disagreeing.
const (
	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA

	magicGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

	// maxFrame bounds a single frame. DevTools replies are large - a snapshot
	// of the DOM or a base64 screenshot runs to megabytes - but not unbounded,
	// and a corrupt length must not turn into an enormous allocation.
	maxFrame = 32 << 20
)

type wsConn struct {
	conn net.Conn
	br   *bufio.Reader
}

// wsDial performs the opening handshake against a ws:// URL and verifies the
// server's Sec-WebSocket-Accept, so a proxy that answers 101 without
// understanding the protocol is caught here rather than as garbled frames.
func wsDial(rawURL string) (*wsConn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parsing %q: %w", rawURL, err)
	}
	if u.Scheme != "ws" {
		return nil, fmt.Errorf("%q is not a ws:// URL", rawURL)
	}

	conn, err := net.DialTimeout("tcp", u.Host, 10*time.Second)
	if err != nil {
		return nil, err
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
		"Sec-WebSocket-Version: 13\r\n\r\n"

	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(conn, req); err != nil {
		conn.Close()
		return nil, fmt.Errorf("sending the handshake: %w", err)
	}
	_ = conn.SetWriteDeadline(time.Time{})

	br := bufio.NewReaderSize(conn, 64<<10)
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
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
	if got, want := res.Header.Get("Sec-WebSocket-Accept"), acceptKey(key); got != want {
		conn.Close()
		return nil, fmt.Errorf("Sec-WebSocket-Accept = %q, want %q", got, want)
	}
	return &wsConn{conn: conn, br: br}, nil
}

// acceptKey is base64(SHA-1(key + GUID)) from RFC 6455 section 1.3.
func acceptKey(key string) string {
	h := sha1.New()
	io.WriteString(h, key)
	io.WriteString(h, magicGUID)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func (c *wsConn) close() { _ = c.conn.Close() }

// writeText sends one masked text message. RFC 6455 requires a client to mask
// every frame it sends, and Chrome enforces it.
func (c *wsConn) writeText(payload []byte) error {
	head := make([]byte, 0, 14)
	head = append(head, 0x80|opText)

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

	if err := c.conn.SetWriteDeadline(time.Now().Add(callTimeout)); err != nil {
		return err
	}
	_, err := c.conn.Write(frame)
	return err
}

// readText returns the next complete text message, answering pings and
// reassembling fragments on the way. CDP replies routinely pass 64 KiB, so both
// extended length encodings matter here rather than being theoretical.
func (c *wsConn) readText(deadline time.Time) ([]byte, error) {
	var assembled []byte
	fragging := false

	for {
		if err := c.conn.SetReadDeadline(deadline); err != nil {
			return nil, err
		}
		f, err := c.readFrame()
		if err != nil {
			return nil, err
		}

		switch f.opcode {
		case opPing:
			if err := c.writeControl(opPong, f.payload); err != nil {
				return nil, err
			}

		case opPong:
			// Nothing asked for it; ignore it.

		case opClose:
			code := 0
			if len(f.payload) >= 2 {
				code = int(binary.BigEndian.Uint16(f.payload[:2]))
			}
			return nil, fmt.Errorf("the browser closed the DevTools socket (code %d)", code)

		case opText:
			if fragging {
				return nil, errors.New("a data frame arrived in the middle of a fragmented message")
			}
			if f.fin {
				return f.payload, nil
			}
			fragging = true
			assembled = append(assembled[:0], f.payload...)

		case opBinary:
			return nil, errors.New("the browser sent a binary frame, which the DevTools protocol never does")

		case opContinuation:
			if !fragging {
				return nil, errors.New("a continuation frame arrived with nothing to continue")
			}
			if len(assembled)+len(f.payload) > maxFrame {
				return nil, fmt.Errorf("a fragmented message passed the %d byte limit", maxFrame)
			}
			assembled = append(assembled, f.payload...)
			if f.fin {
				return assembled, nil
			}

		default:
			return nil, fmt.Errorf("unknown opcode 0x%x", f.opcode)
		}
	}
}

func (c *wsConn) writeControl(opcode byte, payload []byte) error {
	if len(payload) > 125 {
		payload = payload[:125]
	}
	head := []byte{0x80 | opcode, byte(len(payload)) | 0x80}
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	head = append(head, mask[:]...)
	for i := range payload {
		head = append(head, payload[i]^mask[i&3])
	}
	if err := c.conn.SetWriteDeadline(time.Now().Add(callTimeout)); err != nil {
		return err
	}
	_, err := c.conn.Write(head)
	return err
}

type wsFrame struct {
	fin     bool
	opcode  byte
	payload []byte
}

func (c *wsConn) readFrame() (wsFrame, error) {
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
		return wsFrame{}, errors.New("the browser masked a frame, which RFC 6455 forbids of a server")
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
	if length < 0 || length > maxFrame {
		return wsFrame{}, fmt.Errorf("frame of %d bytes passes the %d byte limit", length, maxFrame)
	}

	payload := make([]byte, length)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return wsFrame{}, err
	}
	return wsFrame{fin: fin, opcode: opcode, payload: payload}, nil
}

// ------------------------------------------------------------ CDP sessions --

// page is one tab and the DevTools session bound to it.
type page struct {
	ws     *wsConn
	lastID int
	said   []logLine
	sent   []netRequest
}

// netRequest is one thing the page asked the network for. Recording these is
// how a promise like "the image never leaves the browser" becomes an assertion
// rather than a comment.
type netRequest struct {
	method string
	url    string
	body   int // bytes of request body Chrome saw, capped by maxPostDataSize
}

func (r netRequest) String() string {
	if r.body > 0 {
		return fmt.Sprintf("%s %s (%d byte body)", r.method, r.url, r.body)
	}
	return r.method + " " + r.url
}

// logLine is one thing the page said: a console call, an uncaught exception, or
// a complaint from the browser itself.
type logLine struct {
	source string
	level  string
	text   string
}

func (l logLine) String() string { return fmt.Sprintf("[%s/%s] %s", l.source, l.level, l.text) }

// call sends one command and returns its result, recording every event that
// arrives in the meantime.
//
// Replies and events share one ordered socket, so anything the page said before
// the browser handled this command has already been read by the time the reply
// arrives. That ordering is what lets a test assert on console output without a
// reader goroutine or a sleep.
func (p *page) call(method string, params map[string]any) (json.RawMessage, error) {
	if params == nil {
		params = map[string]any{}
	}
	p.lastID++
	id := p.lastID

	req, err := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	if err := p.ws.writeText(req); err != nil {
		return nil, fmt.Errorf("sending %s: %w", method, err)
	}

	deadline := time.Now().Add(callTimeout)
	for {
		raw, err := p.ws.readText(deadline)
		if err != nil {
			return nil, fmt.Errorf("awaiting the reply to %s: %w", method, err)
		}
		var msg struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
				Data    string `json:"data"`
			} `json:"error"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(raw, &msg); err != nil {
			return nil, fmt.Errorf("decoding a DevTools message: %w", err)
		}

		switch {
		case msg.Method != "":
			p.record(msg.Method, msg.Params)
		case msg.ID != id:
			// A reply to a command that has already timed out. Drop it rather
			// than mistake it for this one's.
		case msg.Error != nil:
			detail := msg.Error.Message
			if msg.Error.Data != "" {
				detail += ": " + msg.Error.Data
			}
			return nil, fmt.Errorf("%s: %s", method, detail)
		default:
			return msg.Result, nil
		}
	}
}

func (p *page) mustCall(t *testing.T, method string, params map[string]any) json.RawMessage {
	t.Helper()
	res, err := p.call(method, params)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return res
}

// record keeps whatever the page reports about itself.
func (p *page) record(method string, params json.RawMessage) {
	switch method {
	case "Runtime.consoleAPICalled":
		var ev struct {
			Type string `json:"type"`
			Args []struct {
				Type        string          `json:"type"`
				Value       json.RawMessage `json:"value"`
				Description string          `json:"description"`
			} `json:"args"`
		}
		if json.Unmarshal(params, &ev) != nil {
			return
		}
		parts := make([]string, 0, len(ev.Args))
		for _, arg := range ev.Args {
			switch {
			case len(arg.Value) > 0:
				var s string
				if json.Unmarshal(arg.Value, &s) == nil {
					parts = append(parts, s)
				} else {
					parts = append(parts, string(arg.Value))
				}
			case arg.Description != "":
				parts = append(parts, arg.Description)
			default:
				parts = append(parts, arg.Type)
			}
		}
		p.said = append(p.said, logLine{source: "console", level: ev.Type, text: strings.Join(parts, " ")})

	case "Runtime.exceptionThrown":
		var ev struct {
			Details struct {
				Text      string `json:"text"`
				URL       string `json:"url"`
				Line      int    `json:"lineNumber"`
				Exception *struct {
					Description string `json:"description"`
				} `json:"exception"`
			} `json:"exceptionDetails"`
		}
		if json.Unmarshal(params, &ev) != nil {
			return
		}
		text := ev.Details.Text
		if ev.Details.Exception != nil && ev.Details.Exception.Description != "" {
			text = ev.Details.Exception.Description
		}
		if ev.Details.URL != "" {
			text = fmt.Sprintf("%s (%s:%d)", text, ev.Details.URL, ev.Details.Line+1)
		}
		p.said = append(p.said, logLine{source: "exception", level: "error", text: text})

	case "Log.entryAdded":
		var ev struct {
			Entry struct {
				Source string `json:"source"`
				Level  string `json:"level"`
				Text   string `json:"text"`
				URL    string `json:"url"`
			} `json:"entry"`
		}
		if json.Unmarshal(params, &ev) != nil {
			return
		}
		text := ev.Entry.Text
		if ev.Entry.URL != "" {
			text += " (" + ev.Entry.URL + ")"
		}
		p.said = append(p.said, logLine{source: ev.Entry.Source, level: ev.Entry.Level, text: text})

	case "Network.requestWillBeSent":
		var ev struct {
			Request struct {
				Method      string `json:"method"`
				URL         string `json:"url"`
				PostData    string `json:"postData"`
				HasPostData bool   `json:"hasPostData"`
			} `json:"request"`
		}
		if json.Unmarshal(params, &ev) != nil {
			return
		}
		size := len(ev.Request.PostData)
		// A body too large for maxPostDataSize arrives truncated or absent, so
		// record the flag as a size of its own rather than reporting zero for
		// the very case this exists to catch.
		if size == 0 && ev.Request.HasPostData {
			size = -1
		}
		p.sent = append(p.sent, netRequest{method: ev.Request.Method, url: ev.Request.URL, body: size})
	}
}

// watchNetwork starts recording what the page asks the network for.
//
// maxPostDataSize is generous on purpose: the point is to see the size of any
// body that goes out, and a cap below the thing we are watching for would hide
// exactly the upload this is meant to catch.
func (p *page) watchNetwork(t *testing.T) {
	t.Helper()
	p.mustCall(t, "Network.enable", map[string]any{"maxPostDataSize": 1 << 20})
}

// requestsSince returns the requests recorded after a mark taken earlier with
// len(p.sent), having first made a round trip so that anything already sent has
// been read off the socket.
func (p *page) requestsSince(t *testing.T, mark int) []netRequest {
	t.Helper()
	if _, err := p.eval("1"); err != nil {
		t.Fatalf("draining the DevTools event stream: %v", err)
	}
	if mark > len(p.sent) {
		return nil
	}
	return p.sent[mark:]
}

// complaints returns everything the page reported at error level.
func (p *page) complaints() []logLine {
	var out []logLine
	for _, l := range p.said {
		if l.level == "error" || l.level == "assert" {
			out = append(out, l)
		}
	}
	return out
}

// requireQuietConsole is the cheapest high-value assertion in the suite: a page
// that boots but complains is a page that is half broken, and a refused inline
// script or a canvas that failed to load says so here first.
func (p *page) requireQuietConsole(t *testing.T) {
	t.Helper()
	// One round trip so that anything already said has been read: events queue
	// ahead of the reply on the same socket.
	if _, err := p.eval("1"); err != nil {
		t.Fatalf("draining the DevTools event stream: %v", err)
	}
	bad := p.complaints()
	if len(bad) == 0 {
		return
	}
	var b strings.Builder
	for _, l := range bad {
		b.WriteString("\n  ")
		b.WriteString(l.String())
	}
	t.Errorf("the page logged %d error(s) it should not have:%s", len(bad), b.String())
}

// ------------------------------------------------------------- evaluation ---

// eval runs an expression in the page and returns its value.
//
// A JavaScript exception comes back as a Go error rather than a zero value: a
// test that silently treats a thrown TypeError as false is a test that passes
// when the front end breaks.
func (p *page) eval(expr string) (json.RawMessage, error) {
	res, err := p.call("Runtime.evaluate", map[string]any{
		"expression":    expr,
		"returnByValue": true,
		"awaitPromise":  true,
	})
	if err != nil {
		return nil, err
	}
	var out struct {
		Result struct {
			Type  string          `json:"type"`
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		Details *struct {
			Text      string `json:"text"`
			Line      int    `json:"lineNumber"`
			Exception *struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return nil, fmt.Errorf("decoding the result of %s: %w", oneLine(expr), err)
	}
	if out.Details != nil {
		detail := out.Details.Text
		if out.Details.Exception != nil && out.Details.Exception.Description != "" {
			detail = out.Details.Exception.Description
		}
		return nil, fmt.Errorf("%s threw: %s", oneLine(expr), oneLine(detail))
	}
	return out.Result.Value, nil
}

func (p *page) mustEval(t *testing.T, expr string) json.RawMessage {
	t.Helper()
	v, err := p.eval(expr)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return v
}

// evalInto runs an expression and decodes its value into dst.
func (p *page) evalInto(t *testing.T, expr string, dst any) {
	t.Helper()
	v := p.mustEval(t, expr)
	if len(v) == 0 {
		t.Fatalf("%s returned undefined, want a value", oneLine(expr))
	}
	if err := json.Unmarshal(v, dst); err != nil {
		t.Fatalf("%s returned %s, which does not fit %T: %v", oneLine(expr), v, dst, err)
	}
}

func (p *page) evalString(t *testing.T, expr string) string {
	t.Helper()
	var s string
	p.evalInto(t, expr, &s)
	return s
}

func (p *page) evalInt(t *testing.T, expr string) int {
	t.Helper()
	var f float64
	p.evalInto(t, expr, &f)
	return int(f)
}

func (p *page) evalFloat(t *testing.T, expr string) float64 {
	t.Helper()
	var f float64
	p.evalInto(t, expr, &f)
	return f
}

func (p *page) evalBool(t *testing.T, expr string) bool {
	t.Helper()
	var b bool
	p.evalInto(t, expr, &b)
	return b
}

// waitFor polls an expression until it returns true, and fails with whatever it
// returned last if the deadline passes first.
//
// Two conventions make the failures readable. An expression that throws is
// treated as "not yet": a navigation in flight destroys the execution context
// and an element the page has not built yet is a null dereference, and both
// resolve on their own. A string result, on the other hand, is a verdict the
// page has reached and cannot come back from, so it fails immediately - the
// room page writes its boot failure into the overlay, and waiting thirty
// seconds to report a message that is already on screen helps nobody.
func (p *page) waitFor(t *testing.T, expr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := "undefined"
	var lastErr error

	for {
		v, err := p.eval(expr)
		if err != nil {
			lastErr = err
		} else {
			lastErr = nil
			last = "undefined"
			if len(v) > 0 {
				last = string(v)
			}
			if last == "true" {
				return
			}
			var verdict string
			if json.Unmarshal(v, &verdict) == nil {
				t.Fatalf("waiting for %s: the page reported %q", oneLine(expr), verdict)
			}
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(pollInterval)
	}

	if lastErr != nil {
		t.Fatalf("waiting for %s: still failing after %s: %v", oneLine(expr), timeout, lastErr)
	}
	t.Fatalf("waiting for %s: still %s after %s", oneLine(expr), last, timeout)
}

// ------------------------------------------------------------- navigation ---

type viewport struct {
	width  int
	height int
	mobile bool
}

var (
	desktopViewport = viewport{width: 1280, height: 800}
	phoneViewport   = viewport{width: 390, height: 844, mobile: true}
)

// setViewport pins the window size. Headless Chrome's default is not part of
// any contract, and every geometric assertion in this suite would otherwise be
// at the mercy of the build that happens to be installed.
func (p *page) setViewport(t *testing.T, v viewport) {
	t.Helper()
	p.mustCall(t, "Emulation.setDeviceMetricsOverride", map[string]any{
		"width":             v.width,
		"height":            v.height,
		"deviceScaleFactor": 1,
		"mobile":            v.mobile,
	})
}

// navigate loads a URL and waits for the new document to finish.
//
// The wait cannot be "readyState is complete" on its own, because the document
// being left behind is also complete. Stamping the outgoing window and waiting
// for the stamp to be gone is exact: a navigation replaces the window object,
// so the absence of the stamp is proof that this is the new document.
func (p *page) navigate(t *testing.T, target string) {
	t.Helper()
	p.stampDocument(t)

	res := p.mustCall(t, "Page.navigate", map[string]any{"url": target})
	var out struct {
		ErrorText string `json:"errorText"`
	}
	if err := json.Unmarshal(res, &out); err == nil && out.ErrorText != "" {
		t.Fatalf("navigating to %s: %s", target, out.ErrorText)
	}
	p.waitForNewDocument(t)
}

// reload re-fetches the current page, which is how the suite checks that state
// survives in the server rather than in the tab.
func (p *page) reload(t *testing.T) {
	t.Helper()
	p.stampDocument(t)
	p.mustCall(t, "Page.reload", map[string]any{"ignoreCache": false})
	p.waitForNewDocument(t)
}

func (p *page) stampDocument(t *testing.T) {
	t.Helper()
	if _, err := p.eval(`window.__pfLeaving = true`); err != nil {
		t.Fatalf("marking the outgoing document: %v", err)
	}
}

func (p *page) waitForNewDocument(t *testing.T) {
	t.Helper()
	p.waitFor(t, `document.readyState === "complete" && window.__pfLeaving === undefined`, 30*time.Second)
}

// focus makes this tab the active one.
//
// Chrome treats a background tab as something nobody is looking at: it suspends
// its animation frames, and it lets an input event aimed at it sit for five
// seconds waiting for an acknowledgement the hidden widget never sends. Both
// matter as soon as a test opens a second tab on the same canvas, and both are
// avoided by modelling the obvious thing - these are events from somebody who
// is looking at this page.
func (p *page) focus(t *testing.T) {
	t.Helper()
	p.mustCall(t, "Page.bringToFront", nil)
}

// settle waits for the page to paint. The renderer coalesces work into frames,
// so anything drawn in response to an event is on screen only after the next
// one; waiting two frames is the cheap way to know the first has landed.
func (p *page) settle(t *testing.T) {
	t.Helper()
	p.focus(t)
	p.mustEval(t, `new Promise(done => requestAnimationFrame(() => requestAnimationFrame(() => done(true))))`)
}

// ------------------------------------------------------------------ input ---

// rect is an element's box in CSS pixels, in viewport coordinates.
type rect struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

func (r rect) centre() (float64, float64) { return r.X + r.W/2, r.Y + r.H/2 }

// at returns the point a given fraction across and down the box.
func (r rect) at(fx, fy float64) (float64, float64) { return r.X + r.W*fx, r.Y + r.H*fy }

func (p *page) rectOf(t *testing.T, selector string) rect {
	t.Helper()
	var out struct {
		Found bool `json:"found"`
		rect
	}
	p.evalInto(t, `(() => {
		const el = document.querySelector(`+jsString(selector)+`);
		if (!el) return {found: false};
		const r = el.getBoundingClientRect();
		return {found: true, x: r.x, y: r.y, w: r.width, h: r.height};
	})()`, &out)
	if !out.Found {
		t.Fatalf("no element matches %s", selector)
	}
	return out.rect
}

// moveMouse sends a real pointer move. The canvas tracks pointer events, so
// this is also how the page is told which cell is under the cursor.
func (p *page) moveMouse(t *testing.T, x, y float64) {
	t.Helper()
	p.focus(t)
	p.mustCall(t, "Input.dispatchMouseEvent", map[string]any{
		"type": "mouseMoved", "x": x, "y": y,
		"button": "none", "buttons": 0, "pointerType": "mouse",
	})
}

// Modifier bits, as CDP numbers them.
const (
	modAlt   = 1
	modCtrl  = 2
	modMeta  = 4
	modShift = 8
)

// clickAt presses and releases the left button where it is told.
//
// Real input rather than element.click(): the stage listens for pointerdown and
// pointerup and never sees a synthetic click, so a scripted click would paint
// nothing while appearing to work.
func (p *page) clickAt(t *testing.T, x, y float64) {
	t.Helper()
	p.modClickAt(t, x, y, 0)
}

// modClickAt clicks with modifier keys held. Shift turns a click on the canvas
// into a question rather than a placement, so the modifier has to travel with
// the event for that to be testable at all.
func (p *page) modClickAt(t *testing.T, x, y float64, modifiers int) {
	t.Helper()
	p.moveMouse(t, x, y)
	p.mustCall(t, "Input.dispatchMouseEvent", map[string]any{
		"type": "mousePressed", "x": x, "y": y, "modifiers": modifiers,
		"button": "left", "buttons": 1, "clickCount": 1, "pointerType": "mouse",
	})
	p.mustCall(t, "Input.dispatchMouseEvent", map[string]any{
		"type": "mouseReleased", "x": x, "y": y, "modifiers": modifiers,
		"button": "left", "buttons": 0, "clickCount": 1, "pointerType": "mouse",
	})
}

// pressKey sends a real key press. The shortcut handlers listen on the window,
// so a synthesised KeyboardEvent would prove only that dispatchEvent works.
func (p *page) pressKey(t *testing.T, key, code string, virtualKey, modifiers int) {
	t.Helper()
	p.focus(t)
	for _, kind := range []string{"keyDown", "keyUp"} {
		p.mustCall(t, "Input.dispatchKeyEvent", map[string]any{
			"type": kind, "key": key, "code": code, "modifiers": modifiers,
			"windowsVirtualKeyCode": virtualKey, "nativeVirtualKeyCode": virtualKey,
		})
	}
}

// clickSelector clicks the middle of an element, refusing to pretend it worked
// when the element has no size on screen.
func (p *page) clickSelector(t *testing.T, selector string) {
	t.Helper()
	r := p.rectOf(t, selector)
	if r.W <= 0 || r.H <= 0 {
		t.Fatalf("%s has no size on screen, so no click can reach it", selector)
	}
	x, y := r.centre()
	p.clickAt(t, x, y)
}

// ----------------------------------------------------------------- pixels ---

// colourOnScreen reads back what the browser is actually showing at a point by
// sampling the canvas's own backing store, which is the only way to assert that
// a pixel is visible rather than merely stored.
func (p *page) colourOnScreen(t *testing.T, x, y float64) [3]int {
	t.Helper()
	var out struct {
		OK bool `json:"ok"`
		R  int  `json:"r"`
		G  int  `json:"g"`
		B  int  `json:"b"`
	}
	p.evalInto(t, fmt.Sprintf(`(() => {
		const board = document.getElementById('board');
		const r = board.getBoundingClientRect();
		const sx = Math.round((%f - r.left) * board.width / r.width);
		const sy = Math.round((%f - r.top) * board.height / r.height);
		if (sx < 0 || sy < 0 || sx >= board.width || sy >= board.height) return {ok: false};
		const d = board.getContext('2d').getImageData(sx, sy, 1, 1).data;
		return {ok: true, r: d[0], g: d[1], b: d[2]};
	})()`, x, y), &out)
	if !out.OK {
		t.Fatalf("the point (%.0f, %.0f) is outside the canvas", x, y)
	}
	return [3]int{out.R, out.G, out.B}
}

// ------------------------------------------------------------------ small ---

// jsString renders a Go string as a JavaScript literal. JSON's escaping is a
// subset of JavaScript's, so encoding/json is the whole implementation.
func jsString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

// oneLine squashes an expression onto a single line so a failure message stays
// readable when the expression is a small program.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 160 {
		s = s[:157] + "..."
	}
	return s
}
