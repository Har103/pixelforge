package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Har103/pixelforge/internal/auth"
	"github.com/Har103/pixelforge/internal/httpapi"
	"github.com/Har103/pixelforge/internal/pg"
	"github.com/Har103/pixelforge/internal/room"
	"github.com/Har103/pixelforge/internal/store"
	"github.com/Har103/pixelforge/web"
)

// These tests need two things the machine may not have: a database, because a
// room is a database concept, and a browser, because that is the entire point.
// Both are skips rather than failures, so a checkout with neither still goes
// green - the suite is meant to be run, not to be a tax on everyone else.
//
//	PIXELFORGE_TEST_DSN=postgres://user:pass@127.0.0.1:5432/db?sslmode=disable \
//	  go test ./internal/e2e/ -v
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("PIXELFORGE_TEST_DSN"))
	if dsn == "" {
		t.Skip("set PIXELFORGE_TEST_DSN to run the browser end-to-end suite")
	}
	return dsn
}

// newServer builds the whole application - pool, store, registry, handlers and
// the embedded front end - and listens on a loopback port a browser can reach.
// It mirrors the harness in internal/httpapi, which is unexported there.
func newServer(t *testing.T, dsn string) *httptest.Server {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	cfg, err := pg.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("parsing PIXELFORGE_TEST_DSN: %v", err)
	}
	pool := pg.NewPool(cfg, 4, log)
	t.Cleanup(pool.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := pool.WaitReady(ctx, 3); err != nil {
		t.Fatalf("test database not reachable: %v", err)
	}
	st := store.New(pool, log)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrating: %v", err)
	}

	registry := room.NewRegistry(st, log)
	runCtx, stop := context.WithCancel(context.Background())
	go registry.Run(runCtx)
	t.Cleanup(stop)

	secret := []byte("e2e-secret-please-ignore")
	api := &httpapi.Server{
		Rooms: registry, Store: st,
		Signer: auth.NewSigner(secret), Secret: secret,
		Log: log, Static: web.FS(), Version: "e2e",
		// The limiter has its own unit test; a browser clicking flat out must
		// not trip over it here.
		RateLimitPerMin: 100000,
	}

	srv := httptest.NewUnstartedServer(api.Routes())
	// The pages advertise absolute links, and the browser follows some of them,
	// so the origin has to be the address this server actually listens on. The
	// listener exists before Start, which is what makes that knowable in time.
	api.BaseURL = "http://" + srv.Listener.Addr().String()
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

// ------------------------------------------------------------------ session -

// session is one server, one room in it, and one browser tab looking at it.
type session struct {
	dsn        string
	srv        *httptest.Server
	chrome     *chrome
	api        *http.Client
	page       *page
	slug       string
	name       string
	width      int
	height     int
	cooldownMs int
	palette    []string
}

// roomInfo is the part of a room's description this suite uses. The palette in
// particular has to come from the server rather than from a constant here, or
// the tests would agree with themselves about a colour the room does not have.
type roomInfo struct {
	Name       string   `json:"name"`
	Width      int      `json:"width"`
	Height     int      `json:"height"`
	Palette    []string `json:"palette"`
	CooldownMs int64    `json:"cooldownMs"`
}

// roomSpec is what a test wants of its throwaway room. Everything but the
// cooldown is the same for every test here, and the cooldown is only ever
// non-zero for the one test that is about the cooldown.
type roomSpec struct {
	name       string
	viewport   viewport
	cooldownMs int
	// wide asks for a banner-shaped canvas. Fitted to a landscape viewport it
	// spans nearly the whole stage, which is what puts the controls drawn over
	// the canvas actually over the canvas.
	wide bool
}

// openRoom creates a throwaway room with no cooldown and points a fresh browser
// tab at it, returning once the client says it has finished booting.
func openRoom(t *testing.T, name string, v viewport) *session {
	t.Helper()
	return openRoomSpec(t, roomSpec{name: name, viewport: v})
}

func openRoomSpec(t *testing.T, spec roomSpec) *session {
	t.Helper()
	// Check for the database before paying for a browser launch.
	dsn := testDSN(t)
	browser := launchChrome(t)
	srv := newServer(t, dsn)

	s := &session{dsn: dsn, srv: srv, chrome: browser, api: newClient(), name: spec.name}
	s.createRoom(t, spec)
	s.page = s.openPage(t, spec.viewport)
	return s
}

func (s *session) createRoom(t *testing.T, spec roomSpec) {
	t.Helper()
	width, height := 32, 32
	if spec.wide {
		width, height = 64, 16
	}
	body, _ := json.Marshal(map[string]any{
		"name": s.name, "width": width, "height": height,
		// Usually no cooldown: this suite paints as fast as it can click, and a
		// cooldown would turn most of these tests into a stopwatch.
		"cooldownMs": spec.cooldownMs,
		"unlisted":   true,
	})
	res, err := s.api.Post(s.srv.URL+"/api/rooms", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("creating a room: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(res.Body)
		t.Fatalf("creating a room: status %d: %s", res.StatusCode, raw)
	}
	var out struct {
		Slug string   `json:"slug"`
		Room roomInfo `json:"room"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatalf("decoding the created room: %v", err)
	}
	if out.Slug == "" || len(out.Room.Palette) == 0 {
		t.Fatalf("the server created a room without a slug or a palette: %+v", out)
	}
	s.slug = out.Slug
	s.name = out.Room.Name
	s.width, s.height = out.Room.Width, out.Room.Height
	s.palette = out.Room.Palette
	s.cooldownMs = int(out.Room.CooldownMs)
	if s.cooldownMs != spec.cooldownMs {
		t.Fatalf("asked for a %dms cooldown and got %dms", spec.cooldownMs, s.cooldownMs)
	}
	if s.width != width || s.height != height {
		t.Fatalf("asked for a %dx%d room and got %dx%d", width, height, s.width, s.height)
	}
}

// openPage opens another tab on this room, which is how the fan-out test gets a
// second pair of eyes. It shares the browser profile, so it is the same painter
// looking twice.
func (s *session) openPage(t *testing.T, v viewport) *page {
	t.Helper()
	return s.show(t, s.chrome.newPage(t), v)
}

// openStranger opens a tab that is somebody else: its own browser context means
// its own cookie jar, and every identity in this application is a cookie.
func (s *session) openStranger(t *testing.T, v viewport) *page {
	t.Helper()
	return s.show(t, s.chrome.newIsolatedPage(t), v)
}

func (s *session) show(t *testing.T, p *page, v viewport) *page {
	t.Helper()
	p.setViewport(t, v)
	p.navigate(t, s.srv.URL+"/r/"+s.slug)
	waitBooted(t, p)
	return p
}

// waitBooted waits for the client's own statement that it is ready: app.js
// hides the overlay only once the config, the snapshot and the first render
// have all landed, and writes its reason into the overlay when they do not.
func waitBooted(t *testing.T, p *page) {
	t.Helper()
	p.waitFor(t, `(() => {
		const boot = document.getElementById('boot');
		if (!boot) return false;
		const msg = document.getElementById('bootMsg');
		if (msg && msg.className === 'error') return 'the page gave up: ' + msg.textContent;
		return boot.hidden === true;
	})()`, 25*time.Second)
}

// newClient talks to the server as the test rather than as the visitor: it
// needs no cookie jar, because the browser keeps its own identity and this
// client only creates rooms and reads back what the browser did.
func newClient() *http.Client {
	return &http.Client{
		Timeout: 20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// ------------------------------------------------------ reading the server --

// snapshot fetches the authoritative grid: "PXF1", width, height, sequence and
// then one palette index per cell.
func (s *session) snapshot(t *testing.T) []byte {
	t.Helper()
	res, err := s.api.Get(s.srv.URL + "/api/r/" + s.slug + "/snapshot")
	if err != nil {
		t.Fatalf("fetching the snapshot: %v", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("reading the snapshot: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("snapshot answered %d: %s", res.StatusCode, body)
	}
	if len(body) < 16 || string(body[:4]) != "PXF1" {
		t.Fatalf("snapshot is not a PXF1 blob (%d bytes)", len(body))
	}
	w := int(binary.BigEndian.Uint16(body[4:6]))
	h := int(binary.BigEndian.Uint16(body[6:8]))
	if w != s.width || h != s.height {
		t.Fatalf("snapshot is %dx%d, want %dx%d", w, h, s.width, s.height)
	}
	if len(body) != 16+w*h {
		t.Fatalf("snapshot of a %dx%d grid is %d bytes", w, h, len(body))
	}
	return body[16:]
}

// waitForPixel polls the server until a cell holds the colour it should.
func (s *session) waitForPixel(t *testing.T, x, y int, want byte, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var got byte
	for {
		got = s.snapshot(t)[y*s.width+x]
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(pollInterval)
	}
	t.Fatalf("cell (%d, %d) is colour %d after %s, want %d", x, y, got, timeout, want)
}

// waitForHistory polls until the placement has reached PostgreSQL. The room
// writes behind a 250ms flush, and the history endpoint is the only reader that
// goes to the database rather than to the resident grid - which makes it the
// only honest way to know a pixel would survive the process.
func (s *session) waitForHistory(t *testing.T, x, y int, colour byte, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var entries []store.HistoryEntry
	for {
		entries = s.history(t)
		for _, e := range entries {
			if e.X == x && e.Y == y && e.Color == colour {
				return
			}
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(pollInterval)
	}
	t.Fatalf("cell (%d, %d) colour %d never reached the database; history holds %d entries: %+v",
		x, y, colour, len(entries), entries)
}

func (s *session) history(t *testing.T) []store.HistoryEntry {
	t.Helper()
	res, err := s.api.Get(s.srv.URL + "/api/r/" + s.slug + "/history?after=0&limit=100")
	if err != nil {
		t.Fatalf("fetching history: %v", err)
	}
	defer res.Body.Close()
	var out struct {
		Entries []store.HistoryEntry `json:"entries"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatalf("decoding history: %v", err)
	}
	return out.Entries
}

// ------------------------------------------------- reading the browser -----

// stagePoint returns a viewport point a given fraction across the stage. The
// canvas is fitted with padding and centred, so anything within a third of the
// middle is comfortably on it whatever the viewport.
func (s *session) stagePoint(t *testing.T, fx, fy float64) (float64, float64) {
	t.Helper()
	return s.page.rectOf(t, "#stage").at(fx, fy)
}

// cellUnder asks the page which cell a viewport point is over, rather than
// recomputing the mapping here. The client owns that arithmetic; a test that
// reimplemented it would agree with itself while both were wrong, and the
// coordinate readout is worth pinning anyway.
func (s *session) cellUnder(t *testing.T, x, y float64) (int, int) {
	t.Helper()
	s.page.moveMouse(t, x, y)
	s.page.waitFor(t, `/^\d+, \d+$/.test(document.getElementById('coords').textContent)`, 5*time.Second)

	text := s.page.evalString(t, `document.getElementById('coords').textContent`)
	var cx, cy int
	if _, err := fmt.Sscanf(text, "%d, %d", &cx, &cy); err != nil {
		t.Fatalf("the coordinate readout says %q, which is not a cell: %v", text, err)
	}
	if cx < 0 || cy < 0 || cx >= s.width || cy >= s.height {
		t.Fatalf("the point (%.0f, %.0f) is over cell (%d, %d), outside a %dx%d canvas",
			x, y, cx, cy, s.width, s.height)
	}
	return cx, cy
}

// unhover moves the pointer off the stage, so the hover outline the client
// draws around the cell under the cursor is not in the way of a colour reading.
func (s *session) unhover(t *testing.T, p *page) {
	t.Helper()
	p.moveMouse(t, 4, 4)
	p.waitFor(t, `document.getElementById('coords').textContent === '–, –'`, 5*time.Second)
	p.settle(t)
}

// selectedColour is the palette index the client says is selected.
func selectedColour(t *testing.T, p *page) int {
	t.Helper()
	return p.evalInt(t, `[...document.querySelectorAll('#palette .swatch')]
		.findIndex(b => b.classList.contains('sel'))`)
}

// requireCanvasShows asserts that the browser is displaying a colour at a
// point, and that the point still names the cell it did before.
func (s *session) requireCanvasShows(t *testing.T, p *page, x, y float64, cellX, cellY int, hex string) {
	t.Helper()
	gotX, gotY := s.cellUnder(t, x, y)
	if gotX != cellX || gotY != cellY {
		t.Fatalf("the point (%.0f, %.0f) now names cell (%d, %d), want (%d, %d)", x, y, gotX, gotY, cellX, cellY)
	}
	s.unhover(t, p)

	got := p.colourOnScreen(t, x, y)
	want := parseHex(t, hex)
	if !sameColour(got, want) {
		t.Errorf("the canvas shows rgb(%d, %d, %d) at cell (%d, %d), want %s = rgb(%d, %d, %d)",
			got[0], got[1], got[2], cellX, cellY, hex, want[0], want[1], want[2])
	}
}

// sameColour compares what the canvas showed with what was expected.
//
// Nearest-neighbour scaling copies palette entries verbatim and canvas
// compositing is plain arithmetic, so these should be exact; three levels of
// slack costs nothing, covers the rounding in a blend, and is still an order of
// magnitude tighter than the distance between any two palette entries.
func sameColour(got, want [3]int) bool {
	for i := range got {
		if d := got[i] - want[i]; d > 3 || d < -3 {
			return false
		}
	}
	return true
}

// blendOver is what the canvas does when it draws a colour at partial alpha
// over another: the cursor and the template ghost are both this, and predicting
// the exact pixel is what makes those assertions about the feature rather than
// about something having changed.
func blendOver(fg, bg [3]int, alpha float64) [3]int {
	var out [3]int
	for i := range out {
		out[i] = int(alpha*float64(fg[i]) + (1-alpha)*float64(bg[i]) + 0.5)
	}
	return out
}

// waitForCanvas polls what the browser is showing at a point until it is the
// colour expected. Cursors and ghosts arrive over a socket on a tick, so the
// only alternative is a sleep long enough to be wrong on a loaded machine.
func waitForCanvas(t *testing.T, p *page, x, y float64, want [3]int, what string) {
	t.Helper()
	const timeout = 5 * time.Second
	deadline := time.Now().Add(timeout)
	var got [3]int
	for {
		p.settle(t)
		got = p.colourOnScreen(t, x, y)
		if sameColour(got, want) {
			return
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(pollInterval)
	}
	t.Fatalf("waiting for %s: the canvas at (%.0f, %.0f) shows rgb(%d, %d, %d) after %s, want rgb(%d, %d, %d)",
		what, x, y, got[0], got[1], got[2], timeout, want[0], want[1], want[2])
}

// countPainted reports how many cells hold something other than the background.
func countPainted(pixels []byte) int {
	n := 0
	for _, c := range pixels {
		if c != 0 {
			n++
		}
	}
	return n
}

func parseHex(t *testing.T, hex string) [3]int {
	t.Helper()
	if len(hex) != 7 || hex[0] != '#' {
		t.Fatalf("%q is not a #rrggbb colour", hex)
	}
	var out [3]int
	for i := 0; i < 3; i++ {
		v, err := strconv.ParseInt(hex[1+i*2:3+i*2], 16, 32)
		if err != nil {
			t.Fatalf("parsing %q: %v", hex, err)
		}
		out[i] = int(v)
	}
	return out
}

// ================================================================== tests ===

// TestRoomPageBoots covers the failure everybody sees first: the page loads,
// says nothing, and shows an overlay forever. It also pins the two things that
// prove the client really ran - a subtitle only JavaScript writes, and a live
// WebSocket, which is the hand-written server in internal/ws satisfying a real
// browser's handshake rather than this project's own test client.
func TestRoomPageBoots(t *testing.T) {
	s := openRoom(t, "Boot check", desktopViewport)
	p := s.page

	if got := p.evalString(t, `document.getElementById('roomName').textContent`); got != s.name {
		t.Errorf("#roomName = %q, want %q", got, s.name)
	}
	if got := p.evalString(t, `document.title`); !strings.Contains(got, s.name) {
		t.Errorf("document.title = %q, want it to name the room %q", got, s.name)
	}

	// The subtitle is empty in the served HTML and filled from /config, so it
	// is only right if the script ran, fetched, and understood the answer.
	want := fmt.Sprintf("%d×%d", s.width, s.height)
	if got := p.evalString(t, `document.getElementById('roomSub').textContent`); !strings.Contains(got, want) {
		t.Errorf("#roomSub = %q, want it to mention %s", got, want)
	}

	p.waitFor(t, `document.getElementById('connLabel').textContent === 'websocket' &&
		document.getElementById('connDot').classList.contains('live')`, 15*time.Second)

	// Every failure this file exists for shows up here too: a script the policy
	// refused, a fetch that was blocked, an exception during boot.
	p.requireQuietConsole(t)
}

// TestNothingHiddenSwallowsClicks pins a bug that made the canvas completely
// unusable while looking perfectly fine: `.sheet { display: grid }` outranked
// the user agent's `[hidden] { display: none }`, so the modal stayed spread
// over the page, invisible and eating every click.
func TestNothingHiddenSwallowsClicks(t *testing.T) {
	s := openRoom(t, "Click target", desktopViewport)
	p := s.page

	// What the browser would actually deliver a click to, at the middle of the
	// stage and at the point the painting tests aim for.
	for _, tc := range []struct {
		name   string
		fx, fy float64
	}{
		{"the middle of the stage", 0.5, 0.5},
		{"where the painting tests click", 0.44, 0.47},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x, y := s.stagePoint(t, tc.fx, tc.fy)
			var hit struct {
				Tag string `json:"tag"`
				ID  string `json:"id"`
			}
			p.evalInto(t, fmt.Sprintf(`(() => {
				const el = document.elementFromPoint(%f, %f);
				return {tag: el ? el.tagName : '(nothing)', id: el ? el.id : ''};
			})()`, x, y), &hit)
			if hit.ID != "board" {
				t.Errorf("a click at (%.0f, %.0f) would land on <%s id=%q>, want the canvas #board",
					x, y, strings.ToLower(hit.Tag), hit.ID)
			}
		})
	}

	// And the rule that fixed it: everything marked hidden must compute to
	// display:none, whatever its own stylesheet says it is.
	for _, id := range []string{"sheet", "boot", "timelapse", "pausedBadge"} {
		t.Run("#"+id+" is really gone", func(t *testing.T) {
			if !p.evalBool(t, `document.getElementById(`+jsString(id)+`).hidden`) {
				t.Fatalf("#%s should be hidden on a freshly loaded room page", id)
			}
			got := p.evalString(t, `getComputedStyle(document.getElementById(`+jsString(id)+`)).display`)
			if got != "none" {
				t.Errorf("#%s is marked hidden but computes display:%s, so it is still over the page", id, got)
			}
		})
	}

	p.requireQuietConsole(t)
}

// TestClickPaintsTheCellUnderThePointer is the whole product in one test: a
// real mouse press on the canvas, out over the WebSocket, into the room, and
// back again as a broadcast the page draws.
func TestClickPaintsTheCellUnderThePointer(t *testing.T) {
	s := openRoom(t, "Pointer paint", desktopViewport)
	p := s.page

	x, y := s.stagePoint(t, 0.44, 0.47)
	cellX, cellY := s.cellUnder(t, x, y)

	colour := selectedColour(t, p)
	if colour <= 0 {
		t.Fatalf("the client has palette index %d selected, which is the background or nothing: "+
			"this test could not tell a painted cell from an empty one", colour)
	}

	if before := s.snapshot(t)[cellY*s.width+cellX]; before != 0 {
		t.Fatalf("cell (%d, %d) is already colour %d before anything was painted", cellX, cellY, before)
	}

	p.clickAt(t, x, y)
	s.waitForPixel(t, cellX, cellY, byte(colour), 10*time.Second)

	// Exactly one cell, so a click that painted a neighbour as well - or a
	// screen-to-cell rounding error that painted somewhere else entirely - is a
	// failure rather than a pass.
	if painted := countPainted(s.snapshot(t)); painted != 1 {
		t.Errorf("%d cells are painted, want exactly the 1 that was clicked", painted)
	}

	// The live feed is drawn from the server's broadcast, so an entry naming
	// the cell proves the round trip finished in the browser and not merely in
	// the database.
	p.waitFor(t, fmt.Sprintf(
		`[...document.querySelectorAll('#feedList li')].some(li => li.textContent.trim() === '%d, %d')`,
		cellX, cellY), 10*time.Second)

	// And the browser is showing it.
	s.requireCanvasShows(t, p, x, y, cellX, cellY, s.palette[colour])

	p.requireQuietConsole(t)
}

// TestPaintSurvivesReloadAndAColdServer follows one pixel all the way down.
// A reload proves the canvas is rebuilt from the server rather than kept in the
// tab; a second server, whose registry has never heard of this room, proves the
// pixel is in PostgreSQL rather than in the first server's memory.
func TestPaintSurvivesReloadAndAColdServer(t *testing.T) {
	s := openRoom(t, "Persistence", desktopViewport)
	p := s.page

	x, y := s.stagePoint(t, 0.44, 0.47)
	cellX, cellY := s.cellUnder(t, x, y)
	colour := selectedColour(t, p)
	hex := s.palette[colour]

	p.clickAt(t, x, y)
	s.waitForPixel(t, cellX, cellY, byte(colour), 10*time.Second)

	p.reload(t)
	waitBooted(t, p)
	s.requireCanvasShows(t, p, x, y, cellX, cellY, hex)

	s.waitForHistory(t, cellX, cellY, byte(colour), 15*time.Second)

	cold := newServer(t, s.dsn)
	p.navigate(t, cold.URL+"/r/"+s.slug)
	waitBooted(t, p)
	s.requireCanvasShows(t, p, x, y, cellX, cellY, hex)

	p.requireQuietConsole(t)
}

// TestPaletteRendersAndSelectsColours checks the dock against the room's own
// palette and then proves the selection is load-bearing: pick a different
// swatch, paint, and the server has to record that colour.
func TestPaletteRendersAndSelectsColours(t *testing.T) {
	s := openRoom(t, "Palette", desktopViewport)
	p := s.page

	if got := p.evalInt(t, `document.querySelectorAll('#palette .swatch').length`); got != len(s.palette) {
		t.Fatalf("the dock shows %d swatches for a %d colour palette", got, len(s.palette))
	}

	// A swatch that does not carry its own colour is decoration, and a palette
	// rendered off by one is the kind of thing only a browser notices.
	for i, hex := range s.palette {
		got := p.evalString(t, fmt.Sprintf(
			`getComputedStyle(document.querySelectorAll('#palette .swatch')[%d]).backgroundColor`, i))
		if want := cssRGB(t, hex); got != want {
			t.Errorf("swatch %d is %s, want %s (%s)", i, got, want, hex)
		}
	}

	// Pick something the client did not start on, so painting the right colour
	// means the selection travelled rather than the default happening to match.
	const pick = 11
	if pick >= len(s.palette) {
		t.Fatalf("this room's palette only has %d colours", len(s.palette))
	}
	if before := selectedColour(t, p); before == pick {
		t.Fatalf("colour %d was already selected, so this test would pass without clicking anything", pick)
	}

	p.clickSelector(t, fmt.Sprintf(`#palette .swatch:nth-child(%d)`, pick+1))
	p.waitFor(t, fmt.Sprintf(
		`document.querySelectorAll('#palette .swatch')[%d].getAttribute('aria-checked') === 'true'`,
		pick), 5*time.Second)
	if got := selectedColour(t, p); got != pick {
		t.Fatalf("clicking swatch %d selected %d", pick, got)
	}

	x, y := s.stagePoint(t, 0.44, 0.47)
	cellX, cellY := s.cellUnder(t, x, y)
	p.clickAt(t, x, y)
	s.waitForPixel(t, cellX, cellY, pick, 10*time.Second)
	s.requireCanvasShows(t, p, x, y, cellX, cellY, s.palette[pick])

	p.requireQuietConsole(t)
}

// TestSecondTabSeesThePixel is the claim the product is built on: paint here
// and everybody watching sees it. Two tabs on one canvas is the smallest honest
// version of that, and it exercises presence and the binary broadcast frame.
func TestSecondTabSeesThePixel(t *testing.T) {
	s := openRoom(t, "Fan out", desktopViewport)
	painter := s.page
	watcher := s.openPage(t, desktopViewport)

	// Both sockets have to be up before anything is painted, and each tab has to
	// have been told there are two of them - which is the presence broadcast
	// working, and is also what makes the assertions below about the wire true.
	for _, p := range []*page{painter, watcher} {
		p.waitFor(t, `document.getElementById('connDot').classList.contains('live')`, 15*time.Second)
		p.waitFor(t, `Number(document.getElementById('statClients').textContent) >= 2`, 15*time.Second)
	}

	x, y := s.stagePoint(t, 0.44, 0.47)
	cellX, cellY := s.cellUnder(t, x, y)
	colour := selectedColour(t, painter)

	painter.clickAt(t, x, y)
	s.waitForPixel(t, cellX, cellY, byte(colour), 10*time.Second)

	// Nothing but a broadcast writes the feed, so an entry in the tab that did
	// not paint is proof the pixel travelled from one browser to the other.
	watcher.waitFor(t, fmt.Sprintf(
		`[...document.querySelectorAll('#feedList li')].some(li => li.textContent.trim() === '%d, %d')`,
		cellX, cellY), 10*time.Second)

	// And the second viewer is showing it. This one deliberately does not claim
	// to prove where the pixel came from: activating a tab makes the client
	// refetch the snapshot, so what is on screen may have arrived either way.
	// The feed above is the assertion about the wire; this is the assertion that
	// somebody looking at the page would actually see the pixel.
	s.requireCanvasShows(t, watcher, x, y, cellX, cellY, s.palette[colour])

	painter.requireQuietConsole(t)
	watcher.requireQuietConsole(t)
}

// TestPhoneViewportKeepsEverythingOnScreen pins the regression that made the
// room unusable on a phone: the topbar overflowed, which on a page that cannot
// scroll sideways means the controls at the end of it simply do not exist.
func TestPhoneViewportKeepsEverythingOnScreen(t *testing.T) {
	s := openRoom(t, "On a phone", phoneViewport)
	p := s.page

	type overflow struct {
		Desc  string  `json:"desc"`
		Right float64 `json:"right"`
	}
	var over []overflow
	p.evalInto(t, `(() => {
		const limit = document.documentElement.clientWidth;
		const out = [];
		for (const el of document.querySelectorAll('body *')) {
			const style = getComputedStyle(el);
			if (style.display === 'none' || style.visibility === 'hidden') continue;
			const r = el.getBoundingClientRect();
			if (r.width === 0 || r.height === 0) continue;
			if (r.right <= limit + 1) continue;
			const cls = typeof el.className === 'string' && el.className.trim()
				? '.' + el.className.trim().split(/\s+/).join('.') : '';
			out.push({desc: el.tagName.toLowerCase() + (el.id ? '#' + el.id : '') + cls, right: r.right});
		}
		return out;
	})()`, &over)
	if len(over) > 0 {
		var b strings.Builder
		for _, o := range over {
			fmt.Fprintf(&b, "\n  %s reaches %.0fpx", o.Desc, o.Right)
		}
		t.Errorf("%d element(s) reach past the %dpx viewport:%s", len(over), phoneViewport.width, b.String())
	}
	if got := p.evalFloat(t, `document.documentElement.scrollWidth`); got > float64(phoneViewport.width)+1 {
		t.Errorf("the document is %.0fpx wide in a %dpx viewport, so something is hiding off to the right",
			got, phoneViewport.width)
	}

	// The two things the app is unusable without. The palette in particular
	// lives in a dock whose height is set per breakpoint, so it is exactly what
	// a wrong breakpoint pushes off the bottom.
	for _, sel := range []string{"#board", "#palette"} {
		t.Run(sel, func(t *testing.T) {
			r := p.rectOf(t, sel)
			if r.W <= 0 || r.H <= 0 {
				t.Fatalf("%s is %.0fx%.0f on a phone, so it is not there at all", sel, r.W, r.H)
			}
			if r.Y+r.H > float64(phoneViewport.height)+1 {
				t.Errorf("%s ends at %.0fpx, below the %dpx viewport", sel, r.Y+r.H, phoneViewport.height)
			}
			if r.X < -1 || r.X+r.W > float64(phoneViewport.width)+1 {
				t.Errorf("%s spans %.0f to %.0fpx, outside the %dpx viewport", sel, r.X, r.X+r.W, phoneViewport.width)
			}
		})
	}

	if got := p.evalInt(t, `document.querySelectorAll('#palette .swatch').length`); got != len(s.palette) {
		t.Errorf("the phone dock shows %d of the palette's %d colours", got, len(s.palette))
	}

	// A canvas nobody can reach is no better than a missing one.
	x, y := s.stagePoint(t, 0.5, 0.5)
	if id := p.evalString(t, fmt.Sprintf(
		`(document.elementFromPoint(%f, %f) || {}).id || '(nothing)'`, x, y)); id != "board" {
		t.Errorf("a tap in the middle of the stage would land on %q, want the canvas", id)
	}

	p.requireQuietConsole(t)
}

// TestExportsSucceedOrFailHonestly is the browser's view of a bug found by
// attacking this product the way one would attack somebody else's: an absurd
// ?scale= used to answer 200 with an empty body, which a CDN or a link unfurler
// caches as a permanently broken image. Fetching from inside the page also puts
// the Content Security Policy in the path, which a Go client never does.
func TestExportsSucceedOrFailHonestly(t *testing.T) {
	s := openRoom(t, "Exports", desktopViewport)
	p := s.page

	// Paint something first: an export of an empty grid would still be a valid
	// PNG, so it would not notice a renderer that quietly drew nothing.
	x, y := s.stagePoint(t, 0.44, 0.47)
	cellX, cellY := s.cellUnder(t, x, y)
	colour := selectedColour(t, p)
	p.clickAt(t, x, y)
	s.waitForPixel(t, cellX, cellY, byte(colour), 10*time.Second)

	// Checked here rather than at the end: the last case below asks for a
	// failure on purpose, and the browser rightly logs the 4xx it gets. A quiet
	// console is only meaningful up to the point where the test starts
	// misbehaving deliberately.
	p.requireQuietConsole(t)

	base := "/r/" + url.PathEscape(s.slug)
	cases := []struct {
		name     string
		path     string
		wantOK   bool
		wantMIME string
		magic    string
	}{
		{"a normal export", base + "/canvas.png?scale=4", true, "image/png", "PNG"},
		{"the link preview card", base + "/card.png", true, "image/png", "PNG"},
		{"the time-lapse", base + "/timelapse.gif", true, "image/gif", "GIF"},
		{"an impossible scale", base + "/canvas.png?scale=99999", false, "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var res struct {
				Status int    `json:"status"`
				Bytes  int    `json:"bytes"`
				Type   string `json:"type"`
				Magic  string `json:"magic"`
			}
			p.evalInto(t, `(async () => {
				const r = await fetch(`+jsString(tc.path)+`, {cache: 'no-store'});
				const body = new Uint8Array(await r.arrayBuffer());
				return {
					status: r.status,
					bytes: body.length,
					type: (r.headers.get('content-type') || '').split(';')[0],
					magic: Array.from(body.slice(0, 4)).map(c => String.fromCharCode(c)).join(''),
				};
			})()`, &res)

			if !tc.wantOK {
				if res.Status == http.StatusOK {
					t.Errorf("%s answered 200 with %d bytes; a success that is not one gets cached as a broken image",
						tc.path, res.Bytes)
				}
				if res.Status < 400 || res.Status > 499 {
					t.Errorf("%s answered %d, want a 4xx", tc.path, res.Status)
				}
				return
			}
			if res.Status != http.StatusOK {
				t.Fatalf("%s answered %d", tc.path, res.Status)
			}
			if res.Bytes < 100 {
				t.Errorf("%s answered 200 with %d bytes, which is not an image", tc.path, res.Bytes)
			}
			if !strings.Contains(res.Magic, tc.magic) {
				t.Errorf("%s starts with %q, want %s", tc.path, res.Magic, tc.magic)
			}
			if res.Type != tc.wantMIME {
				t.Errorf("%s is served as %q, want %q", tc.path, res.Type, tc.wantMIME)
			}
		})
	}
}

// ================================================== live cursors ============

// TestLiveCursorsShowSomebodyElsesPointer covers the feature that makes a
// shared canvas feel like a room rather than a page that updates: you can see
// where the other person is about to paint, in the colour they are holding.
//
// The two tabs are two browser contexts on purpose. Every identity here is a
// cookie, so two tabs of one profile are one painter twice - which would make
// the filter below look like it worked when it had simply never been asked.
func TestLiveCursorsShowSomebodyElsesPointer(t *testing.T) {
	s := openRoom(t, "Cursors", desktopViewport)
	mine, theirs := s.page, s.openStranger(t, desktopViewport)

	// Both sockets up and each aware of the other, or a cursor has nowhere to
	// travel and the assertions below would be about nothing.
	for _, p := range []*page{mine, theirs} {
		p.waitFor(t, `document.getElementById('connDot').classList.contains('live')`, 15*time.Second)
		p.waitFor(t, `Number(document.getElementById('statClients').textContent) >= 2`, 15*time.Second)
	}

	x, y := s.stagePoint(t, 0.44, 0.47)
	cellX, cellY := s.cellUnder(t, x, y) // this is what puts my pointer there
	colour := selectedColour(t, mine)

	background := parseHex(t, s.palette[0])
	// The client fills the cell with the painter's selected colour at 55%, so
	// the expected pixel is arithmetic rather than "something changed".
	pointer := blendOver(parseHex(t, s.palette[colour]), background, 0.55)

	waitForCanvas(t, theirs, x, y, pointer,
		fmt.Sprintf("the other painter's pointer over cell (%d, %d)", cellX, cellY))

	// Nothing was painted: a pointer is not a placement.
	if painted := countPainted(s.snapshot(t)); painted != 0 {
		t.Errorf("%d cells are painted after nothing but pointer movement", painted)
	}

	// And I must not see a second, laggier copy of my own pointer. The room
	// broadcasts every cursor including mine, so this only holds because the
	// client drops its own by id - and it is invisible unless something asks.
	mine.settle(t)
	if got := mine.colourOnScreen(t, x, y); !sameColour(got, background) {
		t.Errorf("this tab draws its own cursor at cell (%d, %d): it shows rgb(%d, %d, %d), want the background rgb(%d, %d, %d)",
			cellX, cellY, got[0], got[1], got[2], background[0], background[1], background[2])
	}

	// Leaving the canvas takes the pointer away at once rather than leaving it
	// parked for the length of the six second timeout.
	mine.moveMouse(t, 4, 4)
	waitForCanvas(t, theirs, x, y, background, "the pointer to be taken away when its owner leaves")

	mine.requireQuietConsole(t)
	theirs.requireQuietConsole(t)
}

// ================================================== template overlay ========

// TestTemplateOverlayTracesAnImageWithoutUploadingIt covers the feature end to
// end and the promise it is sold on. The image is built in the page and handed
// to the drop zone as a real drop, because a file chooser is not something a
// test can open and a synthetic call to loadTemplateFile would skip the wiring
// that actually breaks.
func TestTemplateOverlayTracesAnImageWithoutUploadingIt(t *testing.T) {
	s := openRoom(t, "Template", desktopViewport)
	p := s.page
	p.watchNetwork(t)
	mark := len(p.sent)

	p.clickSelector(t, "#btnTemplate")
	p.waitFor(t, `document.getElementById('tplPanel').hidden === false`, 5*time.Second)

	// A photograph-sized image first, to prove it is fitted rather than
	// stretched - and it is noise, so it compresses to something far too big to
	// go anywhere unnoticed.
	big := dropImage(t, p, imageSpec{width: 200, height: 150, noise: true})
	p.waitFor(t, `document.getElementById('tplLoaded').hidden === false`, 15*time.Second)
	if !p.evalBool(t, `document.getElementById('tplDrop').hidden`) {
		t.Error("the drop zone is still on screen after an image was dropped on it")
	}

	w, h := templateSize(t, p)
	if w > s.width || h > s.height {
		t.Errorf("a %dx%d image became a %dx%d template, which does not fit a %dx%d room",
			200, 150, w, h, s.width, s.height)
	}
	if w != s.width && h != s.height {
		t.Errorf("template is %dx%d: an image larger than the room should have been fitted to one of its sides", w, h)
	}

	// Now a small one in a single palette colour, so every number below is
	// exact rather than "it moved".
	const want = 11
	p.clickSelector(t, "#btnTplClear")
	if !p.evalBool(t, `document.getElementById('tplLoaded').hidden`) ||
		p.evalBool(t, `document.getElementById('tplDrop').hidden`) {
		t.Fatal("removing the template did not put the drop zone back")
	}
	dropImage(t, p, imageSpec{width: 8, height: 5, fill: s.palette[want]})
	p.waitFor(t, `document.getElementById('tplCount').textContent === '0 of 40 cells match'`, 15*time.Second)
	if got := p.evalString(t, `document.getElementById('tplSize').textContent`); got != "8×5" {
		t.Errorf("#tplSize = %q, want 8×5", got)
	}

	// "next cell" has to do both halves of its job: go somewhere useful and
	// pick up the colour that belongs there.
	if before := selectedColour(t, p); before == want {
		t.Fatalf("colour %d was already selected, so jumping to a cell that needs it would prove nothing", want)
	}
	p.clickSelector(t, "#btnTplNext")
	if got := selectedColour(t, p); got != want {
		t.Errorf("jumping to the next cell selected colour %d, want the %d the template needs there", got, want)
	}

	// The jump centres the target cell, so the middle of the stage is now the
	// cell the template wants painted.
	mx, my := s.stagePoint(t, 0.5, 0.5)
	cellX, cellY := s.cellUnder(t, mx, my)
	if got := s.snapshot(t)[cellY*s.width+cellX]; got != 0 {
		t.Fatalf("the next cell to paint (%d, %d) is already colour %d", cellX, cellY, got)
	}

	// The ghost is drawn under the art at 45%, and hiding it has to actually
	// remove it rather than dim it.
	background := parseHex(t, s.palette[0])
	ghost := blendOver(parseHex(t, s.palette[want]), background, 0.45)
	s.unhover(t, p)
	if got := p.colourOnScreen(t, mx, my); !sameColour(got, ghost) {
		t.Errorf("the template ghost shows rgb(%d, %d, %d) at (%d, %d), want rgb(%d, %d, %d)",
			got[0], got[1], got[2], cellX, cellY, ghost[0], ghost[1], ghost[2])
	}
	p.clickSelector(t, "#btnTplToggle")
	if got := p.evalString(t, `document.getElementById('btnTplToggle').textContent`); got != "show" {
		t.Errorf("the toggle still says %q after hiding the template", got)
	}
	s.unhover(t, p)
	if got := p.colourOnScreen(t, mx, my); !sameColour(got, background) {
		t.Errorf("the hidden template still shows rgb(%d, %d, %d) at (%d, %d), want the background rgb(%d, %d, %d)",
			got[0], got[1], got[2], cellX, cellY, background[0], background[1], background[2])
	}
	p.clickSelector(t, "#btnTplToggle")
	s.unhover(t, p)
	if got := p.colourOnScreen(t, mx, my); !sameColour(got, ghost) {
		t.Errorf("showing the template again left rgb(%d, %d, %d) at (%d, %d), want the ghost rgb(%d, %d, %d)",
			got[0], got[1], got[2], cellX, cellY, ghost[0], ghost[1], ghost[2])
	}

	// Painting the cell the template asked for has to move the progress it
	// reports, in all three places it reports it.
	p.clickAt(t, mx, my)
	s.waitForPixel(t, cellX, cellY, want, 10*time.Second)
	p.waitFor(t, `document.getElementById('tplCount').textContent === '1 of 40 cells match'`, 10*time.Second)
	if got := p.evalString(t, `document.getElementById('tplPercent').textContent`); got != "2%" {
		t.Errorf("#tplPercent = %q after painting 1 of 40 cells, want 2%%", got)
	}
	if got := p.evalString(t, `document.getElementById('tplBar').style.width`); got == "0%" || got == "" {
		t.Errorf("#tplBar is still %q after a cell was painted", got)
	}

	// The whole point of the feature: nothing went anywhere.
	requireNothingUploaded(t, p, mark, s.srv.URL, big.size)

	// And the harness this feature was developed against is not part of the
	// site: it used to live under /assets, where it was embedded and served.
	res, err := s.api.Get(s.srv.URL + "/assets/template.test.html")
	if err != nil {
		t.Fatalf("asking for the old test page: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("/assets/template.test.html answers %d: the template harness is being served to the public", res.StatusCode)
	}

	p.requireQuietConsole(t)
}

// imageSpec describes a picture to build in the page. A fill makes every
// prediction exact; noise makes the file too large to leave the browser
// unnoticed.
type imageSpec struct {
	width  int
	height int
	fill   string
	noise  bool
}

type droppedImage struct {
	size int
}

// dropImage builds an image inside the page and hands it to the drop zone as a
// real drop event carrying a real File. Nothing here reaches outside the tab,
// which is the only way to test a feature whose input is a file chooser.
func dropImage(t *testing.T, p *page, spec imageSpec) droppedImage {
	t.Helper()
	paint := fmt.Sprintf(`g.fillStyle = %s; g.fillRect(0, 0, c.width, c.height);`, jsString(spec.fill))
	if spec.noise {
		// A fixed seed, because a test that is different every run is a test
		// that fails differently every run.
		paint = `let seed = 12345;
			const img = g.createImageData(c.width, c.height);
			for (let i = 0; i < img.data.length; i += 4) {
				seed = (seed * 1103515245 + 12345) & 0x7fffffff;
				img.data[i] = seed & 255;
				img.data[i + 1] = (seed >> 8) & 255;
				img.data[i + 2] = (seed >> 16) & 255;
				img.data[i + 3] = 255;
			}
			g.putImageData(img, 0, 0);`
	}

	var out struct {
		Size int `json:"size"`
	}
	p.evalInto(t, fmt.Sprintf(`(async () => {
		const c = document.createElement('canvas');
		c.width = %d; c.height = %d;
		const g = c.getContext('2d');
		%s
		const blob = await new Promise(done => c.toBlob(done, 'image/png'));
		const file = new File([blob], 'reference.png', { type: 'image/png' });
		const dt = new DataTransfer();
		dt.items.add(file);
		document.getElementById('tplDrop').dispatchEvent(
			new DragEvent('drop', { dataTransfer: dt, bubbles: true }));
		return { size: file.size };
	})()`, spec.width, spec.height, paint), &out)

	if out.Size <= 0 {
		t.Fatalf("the image built in the page is %d bytes", out.Size)
	}
	return droppedImage{size: out.Size}
}

func templateSize(t *testing.T, p *page) (int, int) {
	t.Helper()
	text := p.evalString(t, `document.getElementById('tplSize').textContent`)
	var w, h int
	if _, err := fmt.Sscanf(text, "%d×%d", &w, &h); err != nil {
		t.Fatalf("#tplSize says %q, which is not a size: %v", text, err)
	}
	return w, h
}

// requireNothingUploaded checks every request the page made against what the
// app is supposed to ask for. It is the only way "the image never leaves the
// browser" is a fact rather than a comment in a source file.
func requireNothingUploaded(t *testing.T, p *page, mark int, origin string, imageBytes int) {
	t.Helper()
	// Paths the room page legitimately fetches. Anything else - and anything at
	// all off this origin - is the failure being watched for.
	allowed := []string{"/r/", "/assets/", "/api/r/", "/api/palettes", "/favicon.ico"}
	// Every request this app makes is a tiny JSON body or none at all. The
	// smallest useful upload of the image would be orders of magnitude past it.
	const maxBody = 1024

	if imageBytes <= maxBody*4 {
		t.Fatalf("the test image is only %d bytes, which is too small for the size check below to mean anything", imageBytes)
	}

	for _, req := range p.requestsSince(t, mark) {
		if !strings.HasPrefix(req.url, origin+"/") {
			t.Errorf("the page asked for something off its own origin: %s", req)
			continue
		}
		path := strings.TrimPrefix(req.url, origin)
		ok := false
		for _, prefix := range allowed {
			if strings.HasPrefix(path, prefix) {
				ok = true
				break
			}
		}
		if !ok {
			t.Errorf("the page made a request nothing in the app should make: %s", req)
		}
		if req.body < 0 {
			t.Errorf("a request carried a body too large for DevTools to capture, which is exactly what an upload looks like: %s", req)
		} else if req.body > maxBody {
			t.Errorf("a request carried a %d byte body (the image is %d bytes): %s", req.body, imageBytes, req)
		}
	}
}

// ================================================== the inspector ===========

// TestShiftClickAsksWhoPaintedACell covers provenance, and the modifier that
// makes it safe to ask. A shift-click that painted would overwrite the very
// pixel somebody was asking about, and it is one wrong condition away.
func TestShiftClickAsksWhoPaintedACell(t *testing.T) {
	s := openRoom(t, "Inspector", desktopViewport)
	p := s.page

	x, y := s.stagePoint(t, 0.44, 0.47)
	cellX, cellY := s.cellUnder(t, x, y)
	colour := selectedColour(t, p)
	p.clickAt(t, x, y)
	s.waitForPixel(t, cellX, cellY, byte(colour), 10*time.Second)
	// The inspector reads the placement log rather than the grid, so the pixel
	// has to have reached the database before there is anything to answer with.
	s.waitForHistory(t, cellX, cellY, byte(colour), 15*time.Second)

	p.modClickAt(t, x, y, modShift)
	p.waitFor(t, `document.getElementById('inspectPanel').hidden === false`, 5*time.Second)
	if got, want := p.evalString(t, `document.getElementById('inspectTitle').textContent`),
		fmt.Sprintf("cell %d, %d", cellX, cellY); got != want {
		t.Errorf("#inspectTitle = %q, want %q", got, want)
	}

	p.waitFor(t, `document.querySelectorAll('#inspectBody .history li').length > 0`, 10*time.Second)
	if n := p.evalInt(t, `document.querySelectorAll('#inspectBody .history li').length`); n != 1 {
		t.Errorf("the history of a cell painted once has %d entries", n)
	}
	// The server hands back the caller's own id so the client can say "you"
	// rather than a hex string nobody recognises.
	if got := p.evalString(t, `document.querySelector('#inspectBody .history li b').textContent`); got != "you" {
		t.Errorf("the history names the painter %q, want %q", got, "you")
	}
	if got := p.evalString(t, `document.querySelector('#inspectBody .history li b').className`); got != "mine" {
		t.Errorf("the painter entry is styled %q, want it marked as mine", got)
	}

	// A cell nobody has touched says so, rather than showing an empty panel.
	x2, y2 := s.stagePoint(t, 0.62, 0.62)
	cell2X, cell2Y := s.cellUnder(t, x2, y2)
	p.modClickAt(t, x2, y2, modShift)
	p.waitFor(t, fmt.Sprintf(`document.getElementById('inspectTitle').textContent === 'cell %d, %d'`,
		cell2X, cell2Y), 5*time.Second)
	p.waitFor(t, `/nobody has painted here/i.test(document.getElementById('inspectBody').innerText)`, 10*time.Second)

	// Neither question may have answered itself by painting.
	pixels := s.snapshot(t)
	if got := pixels[cell2Y*s.width+cell2X]; got != 0 {
		t.Errorf("shift-clicking the empty cell (%d, %d) painted it colour %d", cell2X, cell2Y, got)
	}
	if got := pixels[cellY*s.width+cellX]; got != byte(colour) {
		t.Errorf("shift-clicking (%d, %d) changed it from colour %d to %d", cellX, cellY, colour, got)
	}
	if painted := countPainted(pixels); painted != 1 {
		t.Errorf("%d cells are painted after one placement and two shift-clicks, want 1", painted)
	}

	p.requireQuietConsole(t)
}

// ================================================== undo ====================

// TestUndoPutsBackWhatWasUnderneath covers the three things that make undo a
// feature rather than a delete: it restores the colour beneath rather than the
// background, it does not cost the painter their turn, and it refuses when
// taking your pixel back would erase somebody else's.
func TestUndoPutsBackWhatWasUnderneath(t *testing.T) {
	// A cooldown far longer than this test takes, so that a second placement
	// landing can only mean the undo cleared it.
	s := openRoomSpec(t, roomSpec{name: "Undo", viewport: desktopViewport, cooldownMs: 20000})
	p := s.page

	x, y := s.stagePoint(t, 0.44, 0.47)
	cellX, cellY := s.cellUnder(t, x, y)

	mine := selectedColour(t, p)
	const underneath = 3
	if mine == underneath {
		t.Fatalf("this test needs two different colours and the client picked %d", mine)
	}

	// Somebody else paints first. Their colour is the one that has to come
	// back, which is the difference between reading the log and assuming.
	s.paintAsSomebodyElse(t, cellX, cellY, underneath)
	s.waitForPixel(t, cellX, cellY, underneath, 10*time.Second)
	s.waitForHistory(t, cellX, cellY, underneath, 15*time.Second)

	p.clickAt(t, x, y)
	s.waitForPixel(t, cellX, cellY, byte(mine), 10*time.Second)
	s.waitForHistory(t, cellX, cellY, byte(mine), 15*time.Second)

	// The headline counter before the undo, so the assertion below can be about
	// what the undo did to it rather than about an absolute number.
	before := p.evalString(t, `document.getElementById('statPlacements').textContent`)

	// Ctrl+Z, because the shortcut is the way anybody actually undoes anything.
	p.pressKey(t, "z", "KeyZ", 90, modCtrl)
	s.waitForPixel(t, cellX, cellY, underneath, 10*time.Second)
	s.requireCanvasShows(t, p, x, y, cellX, cellY, s.palette[underneath])

	// An undo is a retraction, not a placement. The server stopped recording it
	// as one, so a client that counts the broadcast drifts one ahead of every
	// stats page and snaps back on the next reload - which is exactly what
	// production did until the retraction got its own message.
	p.settle(t)
	if after := p.evalString(t, `document.getElementById('statPlacements').textContent`); after != before {
		t.Errorf("the placement counter went from %s to %s across an undo; an undo is not a placement", before, after)
	}

	// The panel showing that cell's history has to notice, too. Leaving a
	// retracted pixel on screen looking current is worse than not offering the
	// history at all.
	p.modClickAt(t, x, y, modShift)
	p.waitFor(t, `!document.getElementById('inspectPanel').hidden &&
		document.querySelectorAll('#inspectBody .history li').length > 0`, 10*time.Second)
	if got := p.evalInt(t, `document.querySelectorAll('#inspectBody .history li.undone').length`); got < 1 {
		t.Errorf("the cell's history shows %d retracted entries, want at least the pixel just undone", got)
	}

	// The cooldown is gone: taking back a misclick must not cost a turn. With a
	// twenty second cooldown still running, this placement could not land.
	x2, y2 := s.stagePoint(t, 0.56, 0.53)
	cell2X, cell2Y := s.cellUnder(t, x2, y2)
	p.clickAt(t, x2, y2)
	s.waitForPixel(t, cell2X, cell2Y, byte(mine), 10*time.Second)
	s.waitForHistory(t, cell2X, cell2Y, byte(mine), 15*time.Second)

	// Checked here rather than at the end: the refusal below is provoked on
	// purpose and the browser rightly logs the 409 it gets. A quiet console is
	// only meaningful up to the point where the test starts misbehaving.
	p.requireQuietConsole(t)

	// Now somebody paints over the pixel I would take back next.
	const theirs = 7
	if theirs == mine {
		t.Fatal("the two painters need different colours for the refusal below to be visible")
	}
	s.paintAsSomebodyElse(t, cell2X, cell2Y, theirs)
	s.waitForPixel(t, cell2X, cell2Y, theirs, 10*time.Second)
	s.waitForHistory(t, cell2X, cell2Y, theirs, 15*time.Second)

	// The button this time, so both routes into undo are covered.
	p.clickSelector(t, "#btnUndo")
	p.waitFor(t, `document.getElementById('toast').className.includes('warn')`, 10*time.Second)
	if got := p.evalString(t, `document.getElementById('toast').textContent`); !strings.Contains(got, "painted over") {
		t.Errorf("the refusal reads %q, which does not explain that somebody painted over it", got)
	}
	// And the refusal is real: their pixel is still theirs.
	if got := s.snapshot(t)[cell2Y*s.width+cell2X]; got != theirs {
		t.Errorf("cell (%d, %d) is colour %d after a refused undo, want %d - somebody else's pixel was erased",
			cell2X, cell2Y, got, theirs)
	}
}

// paintAsSomebodyElse places a pixel over plain HTTP with no cookie jar, so the
// server mints a fresh painter id for it.
//
// That is the honest cheap way to be a second person here: undo has to refuse
// when somebody else has painted over you, and it cannot refuse if the somebody
// else turns out to be you.
func (s *session) paintAsSomebodyElse(t *testing.T, x, y, colour int) {
	t.Helper()
	body, _ := json.Marshal(map[string]int{"x": x, "y": y, "c": colour})
	res, err := newClient().Post(s.srv.URL+"/api/r/"+s.slug+"/place", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("painting as somebody else: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(res.Body)
		t.Fatalf("painting as somebody else at (%d, %d): status %d: %s", x, y, res.StatusCode, raw)
	}
}

// ================================================== stage controls ==========

// TestControlsInsideTheStageReceiveTheirClicks pins a bug that killed every
// control drawn over the canvas at once: the stage captured the pointer on
// pointerdown, which retargets the pointerup, so the click event was dispatched
// at the stage and the button underneath the cursor never heard about it. The
// zoom box, both panels, the time-lapse bar and the minimap all live inside the
// stage, and all of them were dead.
//
// The room is deliberately wide, so the controls genuinely overlap the canvas
// and the second half of the bug - the click being treated as a placement on
// the cell underneath - has somewhere to happen.
func TestControlsInsideTheStageReceiveTheirClicks(t *testing.T) {
	s := openRoomSpec(t, roomSpec{name: "Stage controls", viewport: desktopViewport, wide: true})
	p := s.page

	zoom := p.rectOf(t, "#btnZoomIn")
	board := p.rectOf(t, "#board")
	if zoom.X+zoom.W < board.X || zoom.X > board.X+board.W {
		t.Fatalf("the zoom control at %.0f..%.0f does not overlap the canvas at %.0f..%.0f, so this test would prove nothing",
			zoom.X, zoom.X+zoom.W, board.X, board.X+board.W)
	}

	before := p.evalString(t, `document.getElementById('zoomLabel').textContent`)
	p.clickSelector(t, "#btnZoomIn")
	p.waitFor(t, fmt.Sprintf(`document.getElementById('zoomLabel').textContent !== %s`, jsString(before)),
		5*time.Second)

	// ...and pressing a button over the canvas is not a placement.
	if painted := countPainted(s.snapshot(t)); painted != 0 {
		t.Errorf("%d cells were painted by clicking a zoom button that sits over the canvas", painted)
	}

	p.requireQuietConsole(t)
}

// ------------------------------------------- the driver's own moving parts --
//
// The two tests below need neither a browser nor a database, so they are what
// keeps the driver honest on a machine that skips everything above.

// TestDevToolsPortParsesChromesAnnouncement covers the one piece of the driver
// that guesses at somebody else's output format.
func TestDevToolsPortParsesChromesAnnouncement(t *testing.T) {
	cases := []struct {
		name string
		line string
		want int
		ok   bool
	}{
		{
			name: "what Chrome actually prints",
			line: "DevTools listening on ws://127.0.0.1:41235/devtools/browser/6a2f-4c1e",
			want: 41235, ok: true,
		},
		{
			name: "with a log prefix in front of it",
			line: "[73:73:0815/232946.473342:INFO:x.cc(42)] DevTools listening on ws://127.0.0.1:9222/devtools/browser/abc",
			want: 9222, ok: true,
		},
		{
			name: "an IPv6 loopback",
			line: "DevTools listening on ws://[::1]:5555/devtools/browser/abc",
			want: 5555, ok: true,
		},
		{name: "an unrelated line", line: "[ERROR:bus.cc(408)] Failed to connect to the bus"},
		{name: "the marker with nothing after it", line: "DevTools listening on ws://"},
		{name: "a host with no port", line: "DevTools listening on ws://127.0.0.1/devtools/browser/abc"},
		{name: "a port that is not a number", line: "DevTools listening on ws://127.0.0.1:http/devtools/browser/abc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := devToolsPort(tc.line)
			if ok != tc.ok {
				t.Fatalf("devToolsPort(%q) ok = %v, want %v", tc.line, ok, tc.ok)
			}
			if got != tc.want {
				t.Errorf("devToolsPort(%q) = %d, want %d", tc.line, got, tc.want)
			}
		})
	}
}

// cssRGB renders a hex colour the way getComputedStyle reports one, which is
// how a stylesheet value and a palette entry can be compared at all.
func cssRGB(t *testing.T, hex string) string {
	t.Helper()
	c := parseHex(t, hex)
	return fmt.Sprintf("rgb(%d, %d, %d)", c[0], c[1], c[2])
}

// TestWebSocketClientHandlesRealFrames exercises the parts of the frame codec
// that a well-behaved browser may never show it: both extended length
// encodings, a control frame arriving in the middle of a conversation, and a
// message split across frames. The peer is hand-rolled rather than internal/ws
// because nothing in this project sends a fragmented message, and a codec whose
// only exercise is the traffic it happens to see has untested branches in it.
func TestWebSocketClientHandlesRealFrames(t *testing.T) {
	medium := strings.Repeat("y", 300)   // opcode length 126: the 16-bit form
	large := strings.Repeat("z", 70<<10) // opcode length 127: the 64-bit form

	answered := make(chan []byte, 1)
	endpoint := rawWSServer(t, func(conn net.Conn, br *bufio.Reader) {
		for _, f := range []struct {
			fin     bool
			opcode  byte
			payload string
		}{
			{true, opText, "small"},
			{true, opText, medium},
			{true, opText, large},
			{true, opPing, "beat"},
			{false, opText, "frag"},
			{true, opContinuation, "mented"},
		} {
			if err := writeUnmaskedFrame(conn, f.fin, f.opcode, []byte(f.payload)); err != nil {
				answered <- nil
				return
			}
		}

		// The client owes us a pong, and RFC 6455 says it must be masked.
		opcode, payload, masked, err := readClientFrame(br)
		if err != nil || opcode != opPong || !masked {
			answered <- nil
			return
		}
		answered <- payload
	})

	c, err := wsDial(endpoint)
	if err != nil {
		t.Fatalf("dialling the test server: %v", err)
	}
	t.Cleanup(c.close)

	for _, want := range []string{"small", medium, large, "fragmented"} {
		got, err := c.readText(time.Now().Add(10 * time.Second))
		if err != nil {
			t.Fatalf("reading the %d byte message: %v", len(want), err)
		}
		if string(got) != want {
			t.Fatalf("read %d bytes (%s), want %d bytes (%s)",
				len(got), oneLine(string(got)), len(want), oneLine(want))
		}
	}

	select {
	case payload := <-answered:
		if string(payload) != "beat" {
			t.Errorf("the client answered the ping with a masked pong carrying %q, want %q", payload, "beat")
		}
	case <-time.After(10 * time.Second):
		t.Error("the client never answered the ping")
	}
}

// rawWSServer accepts one connection, checks the opening handshake the way a
// server has to, and hands the raw socket to the caller. It reports the ws://
// address to dial.
func rawWSServer(t *testing.T, serve func(net.Conn, *bufio.Reader)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		br := bufio.NewReader(conn)
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		// A client that gets this wrong deserves a refusal rather than a
		// connection that half works, and refusing here is what makes this test
		// cover the handshake the driver sends as well as the frames it reads.
		key := req.Header.Get("Sec-WebSocket-Key")
		nonce, err := base64.StdEncoding.DecodeString(key)
		if err != nil || len(nonce) != 16 || req.Header.Get("Sec-WebSocket-Version") != "13" {
			_, _ = io.WriteString(conn, "HTTP/1.1 400 Bad Request\r\n\r\n")
			return
		}
		_, _ = io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\n"+
			"Upgrade: websocket\r\nConnection: Upgrade\r\n"+
			"Sec-WebSocket-Accept: "+acceptKey(key)+"\r\n\r\n")

		serve(conn, br)
	}()

	return "ws://" + ln.Addr().String() + "/devtools/page/test"
}

// writeUnmaskedFrame writes one frame the way a server must: never masked, and
// with the shortest length encoding that fits.
func writeUnmaskedFrame(w io.Writer, fin bool, opcode byte, payload []byte) error {
	head := []byte{opcode}
	if fin {
		head[0] |= 0x80
	}
	n := len(payload)
	switch {
	case n <= 125:
		head = append(head, byte(n))
	case n <= 0xFFFF:
		head = append(head, 126)
		head = binary.BigEndian.AppendUint16(head, uint16(n))
	default:
		head = append(head, 127)
		head = binary.BigEndian.AppendUint64(head, uint64(n))
	}
	_, err := w.Write(append(head, payload...))
	return err
}

// readClientFrame reads one frame from a client, reporting whether it was
// masked as the specification requires.
func readClientFrame(br *bufio.Reader) (opcode byte, payload []byte, masked bool, err error) {
	var head [2]byte
	if _, err := io.ReadFull(br, head[:]); err != nil {
		return 0, nil, false, err
	}
	opcode = head[0] & 0x0F
	masked = head[1]&0x80 != 0
	length := int64(head[1] & 0x7F)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return 0, nil, masked, err
		}
		length = int64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return 0, nil, masked, err
		}
		length = int64(binary.BigEndian.Uint64(ext[:]))
	}

	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(br, mask[:]); err != nil {
			return 0, nil, masked, err
		}
	}
	payload = make([]byte, length)
	if _, err := io.ReadFull(br, payload); err != nil {
		return 0, nil, masked, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i&3]
		}
	}
	return opcode, payload, masked, nil
}
