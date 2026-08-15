package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Har103/pixelforge/internal/canvas"
	"github.com/Har103/pixelforge/internal/hub"
	"github.com/Har103/pixelforge/web"
)

// newTestServer builds the full stack with no database, which exercises every
// handler without needing PostgreSQL in CI.
func newTestServer(t *testing.T, cooldown time.Duration) (*httptest.Server, *canvas.Canvas, *hub.Hub) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	board := canvas.New(32, 32, cooldown)
	store := canvas.NewStore(nil, board, log)
	broker := hub.New(log)

	done := make(chan struct{})
	go broker.Run(done)
	t.Cleanup(func() { close(done) })

	api := &Server{
		Canvas: board, Store: store, Hub: broker, Log: log,
		Static: web.FS(), Version: "test", Secret: []byte("test-secret"),
	}
	srv := httptest.NewServer(api.Routes())
	t.Cleanup(srv.Close)
	return srv, board, broker
}

// client keeps cookies so the identity (and therefore the cooldown) is stable
// across requests, the way a browser behaves.
func newClient(t *testing.T) *http.Client {
	t.Helper()
	jar := &simpleJar{}
	return &http.Client{Jar: jar, Timeout: 10 * time.Second}
}

type simpleJar struct{ cookies []*http.Cookie }

func (j *simpleJar) SetCookies(_ *url.URL, cookies []*http.Cookie) { j.cookies = cookies }
func (j *simpleJar) Cookies(_ *url.URL) []*http.Cookie             { return j.cookies }

func TestConfigEndpoint(t *testing.T) {
	srv, _, _ := newTestServer(t, 0)
	c := newClient(t)

	res, err := c.Get(srv.URL + "/api/config")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	var cfg struct {
		Width, Height int
		Palette       []string
		CooldownMs    int64 `json:"cooldownMs"`
		UID           string
		Ephemeral     bool
	}
	if err := json.NewDecoder(res.Body).Decode(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Width != 32 || cfg.Height != 32 {
		t.Errorf("dimensions = %dx%d", cfg.Width, cfg.Height)
	}
	if len(cfg.Palette) != len(canvas.Palette) {
		t.Errorf("palette length = %d", len(cfg.Palette))
	}
	if !cfg.Ephemeral {
		t.Error("a store with no pool should report ephemeral")
	}
	if len(res.Cookies()) == 0 {
		t.Error("expected an identity cookie to be set")
	}
}

func TestSnapshotHeaderAndSize(t *testing.T) {
	srv, board, _ := newTestServer(t, 0)
	c := newClient(t)

	if _, err := board.Place(2, 3, 9, "seed", time.Now()); err != nil {
		t.Fatal(err)
	}

	res, err := c.Get(srv.URL + "/api/snapshot")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)

	if len(body) != 16+32*32 {
		t.Fatalf("snapshot length = %d, want %d", len(body), 16+32*32)
	}
	if string(body[:4]) != "PXF1" {
		t.Errorf("magic = %q", body[:4])
	}
	if w := binary.BigEndian.Uint16(body[4:6]); w != 32 {
		t.Errorf("width = %d", w)
	}
	if seq := binary.BigEndian.Uint64(body[8:16]); seq != 1 {
		t.Errorf("seq = %d, want 1", seq)
	}
	if body[16+3*32+2] != 9 {
		t.Errorf("pixel (2,3) = %d, want 9", body[16+3*32+2])
	}
}

func postPlace(t *testing.T, c *http.Client, base string, x, y, colour int) (*http.Response, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]int{"x": x, "y": y, "c": colour})
	res, err := c.Post(base+"/api/place", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	res.Body.Close()
	return res, out
}

func TestPlaceHappyPath(t *testing.T) {
	srv, board, _ := newTestServer(t, 0)
	c := newClient(t)

	res, out := postPlace(t, c, srv.URL, 5, 6, 11)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %v", res.StatusCode, out)
	}
	if out["ok"] != true {
		t.Errorf("body = %v", out)
	}
	pixels, _ := board.Snapshot()
	if pixels[6*32+5] != 11 {
		t.Errorf("pixel not applied: %d", pixels[6*32+5])
	}
}

func TestPlaceValidation(t *testing.T) {
	srv, _, _ := newTestServer(t, 0)

	cases := []struct {
		name       string
		x, y, c    int
		wantStatus int
	}{
		{"negative x", -1, 0, 1, http.StatusBadRequest},
		{"x past edge", 32, 0, 1, http.StatusBadRequest},
		{"y past edge", 0, 32, 1, http.StatusBadRequest},
		{"colour past palette", 0, 0, 250, http.StatusBadRequest},
		{"negative colour", 0, 0, -3, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newClient(t)
			res, out := postPlace(t, c, srv.URL, tc.x, tc.y, tc.c)
			if res.StatusCode != tc.wantStatus {
				t.Errorf("status = %d, want %d (body %v)", res.StatusCode, tc.wantStatus, out)
			}
		})
	}
}

func TestPlaceRejectsMalformedBody(t *testing.T) {
	srv, _, _ := newTestServer(t, 0)
	c := newClient(t)
	res, err := c.Post(srv.URL+"/api/place", "application/json", strings.NewReader("{not json"))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", res.StatusCode)
	}
}

func TestPlaceRejectsUnknownFields(t *testing.T) {
	srv, _, _ := newTestServer(t, 0)
	c := newClient(t)
	res, err := c.Post(srv.URL+"/api/place", "application/json",
		strings.NewReader(`{"x":1,"y":1,"c":1,"admin":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an unknown field", res.StatusCode)
	}
}

func TestCooldownIsEnforcedPerIdentity(t *testing.T) {
	srv, _, _ := newTestServer(t, 5*time.Second)

	alice := newClient(t)
	if res, out := postPlace(t, alice, srv.URL, 1, 1, 3); res.StatusCode != http.StatusOK {
		t.Fatalf("first placement failed: %d %v", res.StatusCode, out)
	}
	res, out := postPlace(t, alice, srv.URL, 2, 2, 4)
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second placement status = %d, want 429", res.StatusCode)
	}
	if _, ok := out["retryInMs"]; !ok {
		t.Errorf("429 response should tell the client when to retry: %v", out)
	}

	// A separate identity is not affected.
	bob := newClient(t)
	if res, _ := postPlace(t, bob, srv.URL, 3, 3, 5); res.StatusCode != http.StatusOK {
		t.Errorf("a different client should not inherit the cooldown: %d", res.StatusCode)
	}
}

func TestForgedIdentityCookieIsIgnored(t *testing.T) {
	srv, _, _ := newTestServer(t, 5*time.Second)

	// Place once with a legitimate identity.
	c := newClient(t)
	postPlace(t, c, srv.URL, 1, 1, 3)

	// Now hand-craft a cookie with a bogus signature. The server should mint a
	// fresh identity rather than trust it - and critically, must not accept the
	// attacker's chosen uid.
	req, _ := http.NewRequest("POST", srv.URL+"/api/place",
		strings.NewReader(`{"x":9,"y":9,"c":2}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: uidCookie, Value: "deadbeef.0000000000000000000"})
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; a forged cookie should be replaced, not rejected outright", res.StatusCode)
	}
	var reissued bool
	for _, ck := range res.Cookies() {
		if ck.Name == uidCookie && ck.Value != "deadbeef.0000000000000000000" {
			reissued = true
		}
	}
	if !reissued {
		t.Error("expected the server to reissue a signed identity cookie")
	}
}

func TestIdentityCookieSignatureRoundTrip(t *testing.T) {
	s := &Server{Secret: []byte("k")}
	signed := s.signUID("abc123")
	got, ok := s.verifyUID(signed)
	if !ok || got != "abc123" {
		t.Errorf("round trip failed: %q %v", got, ok)
	}
	if _, ok := s.verifyUID("abc123.deadbeef"); ok {
		t.Error("a bad signature was accepted")
	}
	if _, ok := s.verifyUID("nosignature"); ok {
		t.Error("a cookie with no signature was accepted")
	}
	other := &Server{Secret: []byte("different")}
	if _, ok := other.verifyUID(signed); ok {
		t.Error("a cookie signed with another key was accepted")
	}
}

func TestHealthAndReady(t *testing.T) {
	srv, _, _ := newTestServer(t, 0)
	c := newClient(t)

	for _, path := range []string{"/healthz", "/readyz"} {
		res, err := c.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != http.StatusOK {
			t.Errorf("%s status = %d", path, res.StatusCode)
		}
		res.Body.Close()
	}
}

func TestIndexIsServedAndUnknownPathsAre404(t *testing.T) {
	srv, _, _ := newTestServer(t, 0)
	c := newClient(t)

	res, err := c.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("index status = %d", res.StatusCode)
	}
	if !bytes.Contains(body, []byte("Pixelforge")) {
		t.Error("index does not look like the app shell")
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content type = %q", ct)
	}
	if res.Header.Get("Content-Security-Policy") == "" {
		t.Error("expected a CSP header")
	}

	res, err = c.Get(srv.URL + "/nope")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("unknown path status = %d, want 404", res.StatusCode)
	}
}

func TestStaticAssetsAreServed(t *testing.T) {
	srv, _, _ := newTestServer(t, 0)
	c := newClient(t)
	for _, p := range []string{"/assets/app.js", "/assets/style.css"} {
		res, err := c.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Errorf("%s status = %d", p, res.StatusCode)
		}
	}
}

// TestSSEDeliversPlacements is the end-to-end realtime check: subscribe, paint
// from a second client, and confirm the event arrives on the stream.
func TestSSEDeliversPlacements(t *testing.T) {
	srv, _, _ := newTestServer(t, 0)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/sse", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type = %q", ct)
	}

	reader := bufio.NewReader(res.Body)
	// Drain the preamble: "retry:", then the hello event.
	deadline := time.Now().Add(5 * time.Second)
	sawHello := false
	for !sawHello && time.Now().Before(deadline) {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("reading stream: %v", err)
		}
		if strings.Contains(line, `"hello"`) {
			sawHello = true
		}
	}
	if !sawHello {
		t.Fatal("never received the hello event")
	}

	painter := newClient(t)
	if res, out := postPlace(t, painter, srv.URL, 7, 8, 12); res.StatusCode != http.StatusOK {
		t.Fatalf("place failed: %d %v", res.StatusCode, out)
	}

	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("reading stream: %v", err)
		}
		if !strings.HasPrefix(line, "data: ") || !strings.Contains(line, `"px"`) {
			continue
		}
		var msg struct {
			T string `json:"t"`
			P []struct {
				X, Y int
				C    int
			} `json:"p"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(strings.TrimSpace(line), "data: ")), &msg); err != nil {
			t.Fatalf("decoding event %q: %v", line, err)
		}
		if len(msg.P) != 1 || msg.P[0].X != 7 || msg.P[0].Y != 8 || msg.P[0].C != 12 {
			t.Fatalf("unexpected event payload: %+v", msg)
		}
		return
	}
	t.Fatal("placement never arrived on the SSE stream")
}

func TestHistoryAndStatsWithoutDatabase(t *testing.T) {
	srv, _, _ := newTestServer(t, 0)
	c := newClient(t)

	res, err := c.Get(srv.URL + "/api/history?after=0&limit=10")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("history status = %d; without a database it should return an empty list, not an error", res.StatusCode)
	}

	res2, err := c.Get(srv.URL + "/api/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusOK {
		t.Errorf("stats status = %d", res2.StatusCode)
	}
}
