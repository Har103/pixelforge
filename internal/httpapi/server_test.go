package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Har103/pixelforge/internal/auth"
	"github.com/Har103/pixelforge/internal/pg"
	"github.com/Har103/pixelforge/internal/room"
	"github.com/Har103/pixelforge/internal/store"
)

// These tests need a real PostgreSQL, because rooms are a database concept and
// mocking the store would only test the mock. Point PIXELFORGE_TEST_DSN at a
// throwaway database to run them; CI does exactly that.
//
// The handful of checks that do not need a database live in the ephemeral tests
// further down and always run.
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("PIXELFORGE_TEST_DSN"))
	if dsn == "" {
		t.Skip("set PIXELFORGE_TEST_DSN to run the database-backed API tests")
	}
	return dsn
}

func newServer(t *testing.T, dsn string) *httptest.Server {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	var st *store.Store
	if dsn == "" {
		st = store.New(nil, log)
	} else {
		cfg, err := pg.ParseDSN(dsn)
		if err != nil {
			t.Fatalf("parsing test DSN: %v", err)
		}
		pool := pg.NewPool(cfg, 4, log)
		t.Cleanup(pool.Close)

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := pool.WaitReady(ctx, 3); err != nil {
			t.Fatalf("test database not reachable: %v", err)
		}
		st = store.New(pool, log)
		if err := st.Migrate(ctx); err != nil {
			t.Fatalf("migrating: %v", err)
		}
	}

	registry := room.NewRegistry(st, log)
	ctx, cancel := context.WithCancel(context.Background())
	go registry.Run(ctx)
	t.Cleanup(cancel)

	secret := []byte("test-secret-please-ignore")
	api := &Server{
		Rooms: registry, Store: st,
		Signer: auth.NewSigner(secret), Secret: secret,
		Log: log, Static: testFS(), Version: "test",
		BaseURL:         "http://example.test",
		RateLimitPerMin: 100000, // the limiter has its own test
	}
	srv := httptest.NewServer(api.Routes())
	t.Cleanup(srv.Close)
	return srv
}

type jar struct{ cookies []*http.Cookie }

func (j *jar) SetCookies(_ *url.URL, c []*http.Cookie) {
	// Keep the union rather than the last response's set, or the moderator
	// cookie is lost the moment any other handler sets only pf_uid.
	byName := map[string]*http.Cookie{}
	for _, existing := range j.cookies {
		byName[existing.Name] = existing
	}
	for _, fresh := range c {
		byName[fresh.Name] = fresh
	}
	j.cookies = j.cookies[:0]
	for _, v := range byName {
		j.cookies = append(j.cookies, v)
	}
}
func (j *jar) Cookies(_ *url.URL) []*http.Cookie { return j.cookies }

func newClient() *http.Client {
	return &http.Client{
		Jar:     &jar{},
		Timeout: 20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func postJSON(t *testing.T, c *http.Client, url string, body any) (*http.Response, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	res, err := c.Post(url, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	res.Body.Close()
	return res, out
}

func createRoom(t *testing.T, c *http.Client, base string, body map[string]any) (slug string, out map[string]any) {
	t.Helper()
	res, out := postJSON(t, c, base+"/api/rooms", body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("creating room: status %d body %v", res.StatusCode, out)
	}
	s, _ := out["slug"].(string)
	if s == "" {
		t.Fatalf("no slug in response %v", out)
	}
	return s, out
}

// ------------------------------------------------------------ room basics --

func TestCreateRoomAppliesTheChosenSettings(t *testing.T) {
	srv := newServer(t, testDSN(t))
	c := newClient()

	slug, out := createRoom(t, c, srv.URL, map[string]any{
		"name": "Chosen settings", "width": 48, "height": 32,
		"palette": "neon", "cooldownMs": 0,
	})

	room, _ := out["room"].(map[string]any)
	if room["width"].(float64) != 48 || room["height"].(float64) != 32 {
		t.Errorf("dimensions = %vx%v", room["width"], room["height"])
	}
	if room["paletteKey"] != "neon" {
		t.Errorf("palette = %v", room["paletteKey"])
	}
	// The regression that started all this: an explicit zero must not be
	// mistaken for "unset" and replaced by the default.
	if room["cooldownMs"].(float64) != 0 {
		t.Errorf("cooldownMs = %v, want 0", room["cooldownMs"])
	}
	if out["moderatorKey"] == "" || out["moderatorUrl"] == "" {
		t.Error("creation should hand back a moderator key and link")
	}
	if !strings.Contains(slug, "chosen-settings") {
		t.Errorf("slug %q should be derived from the name", slug)
	}
}

func TestRoomsAreIsolated(t *testing.T) {
	srv := newServer(t, testDSN(t))
	a, b := newClient(), newClient()

	slugA, _ := createRoom(t, a, srv.URL, map[string]any{"name": "Room A", "width": 32, "height": 32, "cooldownMs": 0})
	slugB, _ := createRoom(t, b, srv.URL, map[string]any{"name": "Room B", "width": 32, "height": 32, "cooldownMs": 0})

	if res, out := postJSON(t, a, srv.URL+"/api/r/"+slugA+"/place", map[string]int{"x": 3, "y": 3, "c": 5}); res.StatusCode != http.StatusOK {
		t.Fatalf("painting in A: %d %v", res.StatusCode, out)
	}
	time.Sleep(400 * time.Millisecond)

	// The pixel must exist in A and not in B.
	for _, tc := range []struct {
		slug string
		want byte
	}{{slugA, 5}, {slugB, 0}} {
		res, err := a.Get(srv.URL + "/api/r/" + tc.slug + "/snapshot")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if len(body) != 16+32*32 {
			t.Fatalf("snapshot of %s is %d bytes", tc.slug, len(body))
		}
		if got := body[16+3*32+3]; got != tc.want {
			t.Errorf("room %s pixel (3,3) = %d, want %d", tc.slug, got, tc.want)
		}
	}
}

func TestUnknownRoomIs404(t *testing.T) {
	srv := newServer(t, testDSN(t))
	c := newClient()

	for _, path := range []string{"/r/no-such-room-1234", "/api/r/no-such-room-1234/config", "/embed/no-such-room-1234"} {
		res, err := c.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", path, res.StatusCode)
		}
	}
	// A slug that could not exist must not even reach the database.
	res, err := c.Get(srv.URL + "/api/r/NOT..VALID/config")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("malformed slug = %d, want 404", res.StatusCode)
	}
}

// ------------------------------------------------------------- moderation --

func TestModerationNeedsTheModeratorKey(t *testing.T) {
	srv := newServer(t, testDSN(t))
	owner := newClient()
	slug, out := createRoom(t, owner, srv.URL, map[string]any{"name": "Guarded", "width": 32, "height": 32, "cooldownMs": 0})

	stranger := newClient()
	for _, path := range []string{"pause", "clear", "ban", "undo", "locks", "settings"} {
		res, _ := postJSON(t, stranger, srv.URL+"/api/r/"+slug+"/mod/"+path, map[string]any{"uid": "x"})
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("mod/%s without a key = %d, want 403", path, res.StatusCode)
		}
	}

	// The creator's cookie works.
	if res, body := postJSON(t, owner, srv.URL+"/api/r/"+slug+"/mod/pause", map[string]any{"paused": true}); res.StatusCode != http.StatusOK {
		t.Fatalf("owner pause = %d %v", res.StatusCode, body)
	}

	// And so does the recovery link's key, from a browser that has never seen
	// this room before.
	key, _ := out["moderatorKey"].(string)
	fresh := newClient()
	res, body := postJSON(t, fresh, srv.URL+"/api/r/"+slug+"/mod/pause?key="+url.QueryEscape(key), map[string]any{"paused": false})
	if res.StatusCode != http.StatusOK {
		t.Errorf("pause via the moderator key = %d %v", res.StatusCode, body)
	}

	// A wrong key does not.
	res, _ = postJSON(t, fresh, srv.URL+"/api/r/"+slug+"/mod/pause?key=not-the-key", map[string]any{"paused": true})
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("pause with a wrong key = %d, want 403", res.StatusCode)
	}
}

func TestPauseAndLocksBlockPainting(t *testing.T) {
	srv := newServer(t, testDSN(t))
	owner := newClient()
	slug, _ := createRoom(t, owner, srv.URL, map[string]any{"name": "Controls", "width": 32, "height": 32, "cooldownMs": 0})

	painter := newClient()
	if res, _ := postJSON(t, painter, srv.URL+"/api/r/"+slug+"/place", map[string]int{"x": 1, "y": 1, "c": 2}); res.StatusCode != http.StatusOK {
		t.Fatal("painting should work before anything is locked")
	}

	postJSON(t, owner, srv.URL+"/api/r/"+slug+"/mod/pause", map[string]any{"paused": true})
	res, body := postJSON(t, painter, srv.URL+"/api/r/"+slug+"/place", map[string]int{"x": 2, "y": 2, "c": 3})
	if res.StatusCode != http.StatusForbidden || !strings.Contains(body["error"].(string), "paused") {
		t.Errorf("painting while paused = %d %v", res.StatusCode, body)
	}
	postJSON(t, owner, srv.URL+"/api/r/"+slug+"/mod/pause", map[string]any{"paused": false})

	postJSON(t, owner, srv.URL+"/api/r/"+slug+"/mod/locks", map[string]any{
		"locks": []map[string]int{{"X1": 5, "Y1": 5, "X2": 9, "Y2": 9}},
	})
	res, body = postJSON(t, painter, srv.URL+"/api/r/"+slug+"/place", map[string]int{"x": 7, "y": 7, "c": 4})
	if res.StatusCode != http.StatusForbidden || !strings.Contains(body["error"].(string), "locked") {
		t.Errorf("painting inside a lock = %d %v", res.StatusCode, body)
	}
	if res, _ := postJSON(t, painter, srv.URL+"/api/r/"+slug+"/place", map[string]int{"x": 11, "y": 7, "c": 4}); res.StatusCode != http.StatusOK {
		t.Error("painting outside the lock should still work")
	}
}

func TestBanAndUndo(t *testing.T) {
	srv := newServer(t, testDSN(t))
	owner := newClient()
	slug, _ := createRoom(t, owner, srv.URL, map[string]any{"name": "Cleanup", "width": 32, "height": 32, "cooldownMs": 0})

	vandal := newClient()
	res, err := vandal.Get(srv.URL + "/api/r/" + slug + "/config")
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		UID string `json:"uid"`
	}
	_ = json.NewDecoder(res.Body).Decode(&cfg)
	res.Body.Close()

	for i := 0; i < 5; i++ {
		postJSON(t, vandal, srv.URL+"/api/r/"+slug+"/place", map[string]int{"x": i, "y": 0, "c": 6})
	}
	time.Sleep(600 * time.Millisecond)

	_, body := postJSON(t, owner, srv.URL+"/api/r/"+slug+"/mod/undo", map[string]any{"uid": cfg.UID})
	if n, _ := body["undone"].(float64); n != 5 {
		t.Errorf("undone = %v, want 5", body["undone"])
	}

	snap, _ := owner.Get(srv.URL + "/api/r/" + slug + "/snapshot")
	pixels, _ := io.ReadAll(snap.Body)
	snap.Body.Close()
	for i := 0; i < 5; i++ {
		if pixels[16+i] != 0 {
			t.Errorf("pixel %d survived the undo: %d", i, pixels[16+i])
		}
	}

	postJSON(t, owner, srv.URL+"/api/r/"+slug+"/mod/ban", map[string]any{"uid": cfg.UID})
	res2, body2 := postJSON(t, vandal, srv.URL+"/api/r/"+slug+"/place", map[string]int{"x": 20, "y": 20, "c": 7})
	if res2.StatusCode != http.StatusForbidden {
		t.Errorf("a banned painter placed a pixel: %d %v", res2.StatusCode, body2)
	}
}

// --------------------------------------------------------------- exports ---

func TestExportsRender(t *testing.T) {
	srv := newServer(t, testDSN(t))
	c := newClient()
	slug, _ := createRoom(t, c, srv.URL, map[string]any{"name": "Exports", "width": 32, "height": 32, "cooldownMs": 0})
	postJSON(t, c, srv.URL+"/api/r/"+slug+"/place", map[string]int{"x": 4, "y": 4, "c": 9})
	time.Sleep(500 * time.Millisecond)

	cases := []struct{ path, contentType, magic string }{
		{"/r/" + slug + "/canvas.png?scale=4", "image/png", "\x89PNG"},
		{"/r/" + slug + "/card.png", "image/png", "\x89PNG"},
		{"/r/" + slug + "/timelapse.gif", "image/gif", "GIF8"},
	}
	for _, tc := range cases {
		res, err := c.Get(srv.URL + tc.path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Errorf("%s = %d", tc.path, res.StatusCode)
			continue
		}
		if ct := res.Header.Get("Content-Type"); ct != tc.contentType {
			t.Errorf("%s content type = %q, want %q", tc.path, ct, tc.contentType)
		}
		if len(body) < 100 || !strings.HasPrefix(string(body), tc.magic) {
			t.Errorf("%s does not look like %s (%d bytes)", tc.path, tc.contentType, len(body))
		}
	}
}

func TestRoomPageCarriesLinkPreviewTags(t *testing.T) {
	srv := newServer(t, testDSN(t))
	c := newClient()
	slug, _ := createRoom(t, c, srv.URL, map[string]any{"name": "Preview me", "width": 32, "height": 32})

	res, err := c.Get(srv.URL + "/r/" + slug)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	html := string(body)

	for _, want := range []string{
		`property="og:title" content="Preview me"`,
		`property="og:image" content="http://example.test/r/` + slug + `/card.png"`,
		`data-slug="` + slug + `"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("room page is missing %s", want)
		}
	}
}

// TestRoomPageHasNoInlineScript pins the bug that broke every room page: the
// slug used to travel in an inline <script>, which the Content Security Policy
// refused, leaving the client with no idea which canvas to load.
func TestRoomPageHasNoInlineScript(t *testing.T) {
	srv := newServer(t, testDSN(t))
	c := newClient()
	slug, _ := createRoom(t, c, srv.URL, map[string]any{"name": "No inline", "width": 32, "height": 32})

	for _, path := range []string{"/r/" + slug, "/embed/" + slug, "/"} {
		res, err := c.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		csp := res.Header.Get("Content-Security-Policy")
		res.Body.Close()

		html := string(body)
		for _, chunk := range strings.Split(html, "<script") {
			if strings.HasPrefix(chunk, "<!doctype") || !strings.Contains(chunk, ">") {
				continue
			}
			open := chunk[:strings.Index(chunk, ">")]
			if !strings.Contains(open, "src=") && strings.Contains(open, " ") {
				continue
			}
			if !strings.Contains(open, "src=") {
				t.Errorf("%s contains an inline <script%s>, which this CSP refuses", path, open)
			}
		}
		if !strings.Contains(csp, "script-src 'self'") {
			t.Errorf("%s CSP should name script-src explicitly, got %q", path, csp)
		}
		if strings.Contains(csp, "script-src 'self' 'unsafe-inline'") {
			t.Errorf("%s CSP has been weakened to allow inline scripts", path)
		}
	}
}

// ------------------------------------------------------------- no database -

// These run everywhere, because "the database is missing" is a state the
// service is supposed to survive.

func TestWithoutADatabaseTheSiteStillServes(t *testing.T) {
	srv := newServer(t, "")
	c := newClient()

	res, err := c.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	var health map[string]any
	_ = json.NewDecoder(res.Body).Decode(&health)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("healthz = %d", res.StatusCode)
	}
	if health["ephemeral"] != true {
		t.Error("healthz should admit it has no database")
	}

	if res, err := c.Get(srv.URL + "/"); err != nil {
		t.Fatal(err)
	} else {
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Errorf("home page = %d without a database, want 200", res.StatusCode)
		}
	}

	// Creating a room says so plainly rather than failing obscurely.
	res2, body := postJSON(t, c, srv.URL+"/api/rooms", map[string]any{"name": "nope"})
	if res2.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("create room = %d, want 503", res2.StatusCode)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "database") {
		t.Errorf("error should mention the missing database, got %q", msg)
	}
}

func TestPalettesEndpoint(t *testing.T) {
	srv := newServer(t, "")
	c := newClient()
	res, err := c.Get(srv.URL + "/api/palettes")
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Palettes []struct {
			Key    string   `json:"key"`
			Colors []string `json:"colors"`
		} `json:"palettes"`
		Limits map[string]int `json:"limits"`
	}
	_ = json.NewDecoder(res.Body).Decode(&out)
	res.Body.Close()

	if len(out.Palettes) < 3 {
		t.Errorf("expected several palettes, got %d", len(out.Palettes))
	}
	if out.Limits["maxDim"] != room.MaxDim {
		t.Errorf("limits should tell the client the real bounds, got %v", out.Limits)
	}
}

func TestIPLimiter(t *testing.T) {
	l := newIPLimiter(3, time.Minute)
	for i := 0; i < 3; i++ {
		if !l.allow("1.2.3.4") {
			t.Fatalf("request %d should be allowed", i+1)
		}
	}
	if l.allow("1.2.3.4") {
		t.Error("the fourth request should be refused")
	}
	if !l.allow("5.6.7.8") {
		t.Error("a different address should have its own budget")
	}
}

func TestClientIPPrefersForwardedHeader(t *testing.T) {
	cases := []struct{ xff, xreal, remote, want string }{
		{"203.0.113.7, 10.0.0.1", "", "10.0.0.1:5000", "203.0.113.7"},
		{"", "203.0.113.9", "10.0.0.1:5000", "203.0.113.9"},
		{"", "", "198.51.100.4:5000", "198.51.100.4"},
	}
	for _, tc := range cases {
		r := httptest.NewRequest("POST", "/", nil)
		r.RemoteAddr = tc.remote
		if tc.xff != "" {
			r.Header.Set("X-Forwarded-For", tc.xff)
		}
		if tc.xreal != "" {
			r.Header.Set("X-Real-Ip", tc.xreal)
		}
		if got := clientIP(r); got != tc.want {
			t.Errorf("clientIP = %q, want %q", got, tc.want)
		}
	}
}

func TestValidUsername(t *testing.T) {
	for _, ok := range []string{"abc", "har103", "a.b-c_d", strings.Repeat("a", 24)} {
		if !validUsername(ok) {
			t.Errorf("validUsername(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "ab", strings.Repeat("a", 25), "has space", "has/slash", "emoji😀"} {
		if validUsername(bad) {
			t.Errorf("validUsername(%q) = true, want false", bad)
		}
	}
}

// TestExportsFailLoudly pins a bug found by attacking my own product with the
// same probes I aimed at a competitor: the export handlers used to encode
// straight into the ResponseWriter, so an absurd ?scale= produced HTTP 200 with
// an empty body - a broken image that a CDN or a link unfurler would cache as
// if it were fine. Rendering into a buffer first means a failure is a failure.
func TestExportsFailLoudlyRatherThanServingAnEmptyBody(t *testing.T) {
	srv := newServer(t, testDSN(t))
	c := newClient()
	slug, _ := createRoom(t, c, srv.URL, map[string]any{"name": "Absurd scale", "width": 32, "height": 32})

	res, err := c.Get(srv.URL + "/r/" + slug + "/canvas.png?scale=99999")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()

	if res.StatusCode == http.StatusOK && len(body) == 0 {
		t.Error("an impossible scale returned 200 with an empty body")
	}
	if res.StatusCode == http.StatusOK {
		t.Errorf("an impossible scale should not be a success, got %d", res.StatusCode)
	}

	// And a sane scale must still work, with a length the client can trust.
	res2, err := c.Get(srv.URL + "/r/" + slug + "/canvas.png?scale=4")
	if err != nil {
		t.Fatal(err)
	}
	good, _ := io.ReadAll(res2.Body)
	res2.Body.Close()
	if res2.StatusCode != http.StatusOK || len(good) < 100 {
		t.Errorf("a normal export broke: %d, %d bytes", res2.StatusCode, len(good))
	}
	if cl := res2.Header.Get("Content-Length"); cl != strconv.Itoa(len(good)) {
		t.Errorf("Content-Length = %q but body is %d bytes", cl, len(good))
	}
}
