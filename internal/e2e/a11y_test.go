// Accessibility, driven the same way as everything else here: a real browser, a
// real keyboard, real computed styles. None of these can be asserted anywhere
// else. Whether the canvas can be painted without a mouse is a question about
// key handling, focus and a render loop; whether a label is legible is a
// question about what four stylesheets and a color-mix actually composited to;
// and whether a live region is polite or unbearable is a question about how
// many times it changed while three hundred pixels landed.
package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// ------------------------------------------------------------- keyboard ----

const (
	keyTab       = 9
	keyEnter     = 13
	keyEnd       = 35
	keyHome      = 36
	keyLeft      = 37
	keyUp        = 38
	keyRight     = 39
	keyDown      = 40
	keyPageUp    = 33
	keyPageDown  = 34
	keySpacebar  = 32
	keyLetterR   = 82
	keyLetterM   = 77
	keyLetterI   = 73
	keyLetterE   = 69
	keyEscape    = 27
	keyBracketRt = 221
)

func tab(t *testing.T, p *page)      { t.Helper(); p.pressKey(t, "Tab", "Tab", keyTab, 0) }
func shiftTab(t *testing.T, p *page) { t.Helper(); p.pressKey(t, "Tab", "Tab", keyTab, modShift) }

// activate presses Enter the way a keyboard user does.
//
// The driver's pressKey sends keyDown and keyUp and nothing between them, and
// Chrome synthesises the click that activates a <button> from the character
// event in the middle - so a bare Enter on a button does nothing at all, which
// is indistinguishable from a button that is broken. Links are different: they
// activate on the key down, which is why the skip link works either way.
func activate(t *testing.T, p *page) {
	t.Helper()
	p.focus(t)
	for _, ev := range []map[string]any{
		{"type": "rawKeyDown", "key": "Enter", "code": "Enter",
			"windowsVirtualKeyCode": keyEnter, "nativeVirtualKeyCode": keyEnter},
		{"type": "char", "key": "Enter", "text": "\r", "unmodifiedText": "\r"},
		{"type": "keyUp", "key": "Enter", "code": "Enter",
			"windowsVirtualKeyCode": keyEnter, "nativeVirtualKeyCode": keyEnter},
	} {
		p.mustCall(t, "Input.dispatchKeyEvent", ev)
	}
}

func arrow(t *testing.T, p *page, dir string, mods int) {
	t.Helper()
	switch dir {
	case "Right":
		p.pressKey(t, "ArrowRight", "ArrowRight", keyRight, mods)
	case "Left":
		p.pressKey(t, "ArrowLeft", "ArrowLeft", keyLeft, mods)
	case "Up":
		p.pressKey(t, "ArrowUp", "ArrowUp", keyUp, mods)
	case "Down":
		p.pressKey(t, "ArrowDown", "ArrowDown", keyDown, mods)
	default:
		t.Fatalf("no such arrow: %s", dir)
	}
}

// focusedName is a short, stable description of whatever has focus, in the same
// shorthand the failure messages use. "(nothing)" means <body>, which on this
// page always means focus was dropped rather than moved.
func focusedName(t *testing.T, p *page) string {
	t.Helper()
	return p.evalString(t, `(() => {
		const a = document.activeElement;
		if (!a || a === document.body) return '(nothing)';
		if (a.id) return a.tagName.toLowerCase() + '#' + a.id;
		const cls = typeof a.className === 'string' && a.className.trim()
			? '.' + a.className.trim().split(/\s+/).join('.') : '';
		return a.tagName.toLowerCase() + cls;
	})()`)
}

// focusBoard puts focus on the canvas the way a person would: take the skip
// link. That the link goes somewhere focusable is half of what is being tested,
// so it is a route rather than a board.focus().
//
// The link is focused directly rather than tabbed to, because Chrome resumes
// sequential navigation from wherever focus last was and a test that has
// already opened a panel is not at the top of the document. That the skip link
// is the *first* stop is asserted where it can be asserted honestly, on a page
// nothing has touched yet: see TestEveryControlIsReachableByTab.
func focusBoard(t *testing.T, p *page) {
	t.Helper()
	p.mustEval(t, `document.querySelector('.skip-link').focus()`)
	if got := focusedName(t, p); got != "a.skip-link" {
		t.Fatalf("the room page has no skip link to take; focus is on %s", got)
	}
	p.pressKey(t, "Enter", "Enter", keyEnter, 0)
	if got := focusedName(t, p); got != "canvas#board" {
		t.Fatalf("taking the skip link lands focus on %s, want the canvas", got)
	}
}

// cursorCell is where the client says its keyboard cursor is. The coordinate
// readout is the only place the client publishes it, which is also the reason
// it is worth pinning: a cursor a sighted keyboard user cannot locate is not
// much of a cursor.
func cursorCell(t *testing.T, p *page) (int, int) {
	t.Helper()
	text := p.evalString(t, `document.getElementById('coords').textContent`)
	var x, y int
	if _, err := fmt.Sscanf(text, "%d, %d", &x, &y); err != nil {
		t.Fatalf("the coordinate readout says %q, which is not a cell: %v", text, err)
	}
	return x, y
}

// said returns what a live region currently holds, with the zero-width space
// the client alternates to defeat duplicate suppression stripped back out.
func said(t *testing.T, p *page, id string) string {
	t.Helper()
	return strings.TrimSpace(strings.ReplaceAll(
		p.evalString(t, `document.getElementById(`+jsString(id)+`).textContent`), "​", ""))
}

// watchRegion starts counting writes to a live region. Counting is the whole
// point: a region that says the right thing five hundred times is worse than
// one that says nothing, and only the number of changes can tell them apart.
func watchRegion(t *testing.T, p *page, id string) {
	t.Helper()
	p.mustEval(t, `(() => {
		const node = document.getElementById(`+jsString(id)+`);
		if (window.__pfWatch && window.__pfWatch[`+jsString(id)+`]) {
			window.__pfWatch[`+jsString(id)+`].disconnect();
		}
		window.__pfWatch = window.__pfWatch || {};
		window.__pfSaid = window.__pfSaid || {};
		window.__pfSaid[`+jsString(id)+`] = [];
		// One entry per mutation record, not per callback. A MutationObserver
		// coalesces everything that happened in one task into a single call, so
		// counting callbacks would report a burst of ten announcements as one -
		// which is precisely the failure being counted.
		const obs = new MutationObserver((records) => {
			for (let i = 0; i < records.length; i++) {
				window.__pfSaid[`+jsString(id)+`].push(node.textContent);
			}
		});
		obs.observe(node, { childList: true, characterData: true, subtree: true });
		window.__pfWatch[`+jsString(id)+`] = obs;
		return true;
	})()`)
}

func heard(t *testing.T, p *page, id string) []string {
	t.Helper()
	var out []string
	p.evalInto(t, `(window.__pfSaid && window.__pfSaid[`+jsString(id)+`] || [])
		.map(s => s.replace(/​/g, '').trim())`, &out)
	return out
}

// ============================================================ painting =====

// TestKeyboardAlonePaintsTheCellUnderTheCursor is the headline claim: the
// product says "one canvas, everyone at once", and until this passed the
// "everyone" excluded anybody who does not use a pointing device. Nothing here
// touches the mouse — the cell that changes on the server is chosen entirely
// with arrows and painted with Enter.
func TestKeyboardAlonePaintsTheCellUnderTheCursor(t *testing.T) {
	s := openRoom(t, "Keyboard paint", desktopViewport)
	p := s.page

	focusBoard(t, p)

	// Landing on the canvas puts the cursor somewhere, and says where.
	startX, startY := cursorCell(t, p)
	if startX < 0 || startY < 0 || startX >= s.width || startY >= s.height {
		t.Fatalf("focusing the canvas put the cursor at (%d, %d), outside a %dx%d room",
			startX, startY, s.width, s.height)
	}

	// Two different step sizes, so a handler that ignored the modifier would
	// land somewhere else entirely rather than somewhere adjacent.
	arrow(t, p, "Right", 0)
	arrow(t, p, "Down", 0)
	arrow(t, p, "Right", modShift)
	arrow(t, p, "Down", modShift)

	wantX, wantY := startX+11, startY+11
	if gotX, gotY := cursorCell(t, p); gotX != wantX || gotY != wantY {
		t.Fatalf("after one step and one ten-cell jump on each axis the cursor is at (%d, %d), want (%d, %d)",
			gotX, gotY, wantX, wantY)
	}
	if wantX >= s.width || wantY >= s.height {
		t.Fatalf("this test walked the cursor to (%d, %d), off a %dx%d canvas", wantX, wantY, s.width, s.height)
	}

	colour := selectedColour(t, p)
	if colour <= 0 {
		t.Fatalf("the client has palette index %d selected, so a painted cell would be "+
			"indistinguishable from an empty one", colour)
	}
	if before := s.snapshot(t)[wantY*s.width+wantX]; before != 0 {
		t.Fatalf("cell (%d, %d) is already colour %d before anything was painted", wantX, wantY, before)
	}

	p.pressKey(t, "Enter", "Enter", keyEnter, 0)
	s.waitForPixel(t, wantX, wantY, byte(colour), 10*time.Second)

	// Exactly one cell: a key handler that fired twice, or one that painted
	// where the cursor used to be as well, is a failure rather than a pass.
	if painted := countPainted(s.snapshot(t)); painted != 1 {
		t.Errorf("%d cells are painted, want exactly the 1 the keyboard placed", painted)
	}

	// And Space, which is the other key everybody reaches for.
	arrow(t, p, "Right", 0)
	p.pressKey(t, " ", "Space", keySpacebar, 0)
	s.waitForPixel(t, wantX+1, wantY, byte(colour), 10*time.Second)

	p.requireQuietConsole(t)
}

// TestKeyboardReachesEveryCornerOfTheCanvas covers the part of the keyboard
// model that stops it being a toy. Nobody crosses five hundred cells one arrow
// press at a time, so if the long jumps are wrong the far side of a large
// canvas is unreachable in practice however correct the single steps are.
func TestKeyboardReachesEveryCornerOfTheCanvas(t *testing.T) {
	s := openRoom(t, "Long distance", desktopViewport)
	p := s.page
	focusBoard(t, p)

	cases := []struct {
		name      string
		key, code string
		vk, mods  int
		wantX     int
		wantY     int
	}{
		{name: "Ctrl+End is the far corner", key: "End", code: "End", vk: keyEnd, mods: modCtrl,
			wantX: s.width - 1, wantY: s.height - 1},
		{name: "Ctrl+Home is the origin", key: "Home", code: "Home", vk: keyHome, mods: modCtrl,
			wantX: 0, wantY: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p.pressKey(t, tc.key, tc.code, tc.vk, tc.mods)
			if gotX, gotY := cursorCell(t, p); gotX != tc.wantX || gotY != tc.wantY {
				t.Fatalf("cursor is at (%d, %d), want (%d, %d)", gotX, gotY, tc.wantX, tc.wantY)
			}
		})
	}

	// From the origin: End is the end of this row and nothing else, Page Down
	// is ten rows and nothing else.
	p.pressKey(t, "End", "End", keyEnd, 0)
	if x, y := cursorCell(t, p); x != s.width-1 || y != 0 {
		t.Errorf("End from the origin goes to (%d, %d), want the end of row 0 at (%d, 0)", x, y, s.width-1)
	}
	p.pressKey(t, "PageDown", "PageDown", keyPageDown, 0)
	if x, y := cursorCell(t, p); x != s.width-1 || y != 10 {
		t.Errorf("Page Down goes to (%d, %d), want ten rows down at (%d, 10)", x, y, s.width-1)
	}
	p.pressKey(t, "PageUp", "PageUp", keyPageUp, 0)
	if x, y := cursorCell(t, p); x != s.width-1 || y != 0 {
		t.Errorf("Page Up goes to (%d, %d), want back to (%d, 0)", x, y, s.width-1)
	}

	// The edge holds rather than wrapping, and says so: silence there is
	// indistinguishable from a key that never registered.
	p.pressKey(t, "Home", "Home", keyHome, modCtrl)
	arrow(t, p, "Left", 0)
	if x, y := cursorCell(t, p); x != 0 || y != 0 {
		t.Errorf("walking off the left edge moved the cursor to (%d, %d), want it held at the origin", x, y)
	}
	if got := said(t, p, "srSelf"); !strings.Contains(strings.ToLower(got), "edge") {
		t.Errorf("walking into the edge announced %q, which does not say the cursor stopped", got)
	}

	p.requireQuietConsole(t)
}

// TestTheCursorIsVisibleAndTheViewFollowsIt is the sighted half of the keyboard
// model. A cursor that is drawn nowhere, or that walks off the side of a view
// which does not follow it, is a cursor somebody has to guess the position of.
func TestTheCursorIsVisibleAndTheViewFollowsIt(t *testing.T) {
	s := openRoom(t, "Cursor visible", desktopViewport)
	p := s.page

	cx, cy := s.stagePoint(t, 0.5, 0.5)

	// Before any keyboard use there is no caret to draw, so the middle of the
	// stage is the canvas and nothing else. This is the control for the
	// measurement below.
	s.unhover(t, p)
	if n := brightPixelsAround(t, p, cx, cy, 40); n != 0 {
		t.Fatalf("%d bright pixels are already near the middle of an empty canvas, so the "+
			"cursor check below would pass without a cursor", n)
	}

	focusBoard(t, p)
	// Ctrl+Home centres the view on the origin, so after this the middle of the
	// stage is cell 0, 0 and the caret has to be drawn right there.
	p.pressKey(t, "Home", "Home", keyHome, modCtrl)
	p.settle(t)
	if n := brightPixelsAround(t, p, cx, cy, 40); n == 0 {
		t.Error("the keyboard cursor is drawn nowhere: a sighted keyboard user has no way to " +
			"tell which cell Enter would paint")
	}

	// The view followed the jump: the cell now under the middle of the stage is
	// the cell the cursor is on.
	gotX, gotY := s.cellUnder(t, cx, cy)
	if gotX != 0 || gotY != 0 {
		t.Errorf("after jumping the cursor to the origin the middle of the stage is cell (%d, %d), "+
			"want (0, 0): the view did not follow", gotX, gotY)
	}

	p.requireQuietConsole(t)
}

// brightPixelsAround counts near-white pixels in a box on the canvas. The
// keyboard caret is the only white thing this suite ever draws on an unpainted
// grid, so counting them is how "it is on screen" becomes an assertion.
func brightPixelsAround(t *testing.T, p *page, x, y float64, half int) int {
	t.Helper()
	p.settle(t)
	return p.evalInt(t, fmt.Sprintf(`(() => {
		const board = document.getElementById('board');
		const r = board.getBoundingClientRect();
		const sx = Math.round((%f - r.left) * board.width / r.width);
		const sy = Math.round((%f - r.top) * board.height / r.height);
		const half = %d;
		const x0 = Math.max(0, sx - half), y0 = Math.max(0, sy - half);
		const w = Math.min(board.width - x0, half * 2), h = Math.min(board.height - y0, half * 2);
		const d = board.getContext('2d').getImageData(x0, y0, w, h).data;
		let n = 0;
		for (let i = 0; i < d.length; i += 4) {
			if (d[i] > 230 && d[i + 1] > 230 && d[i + 2] > 230) n++;
		}
		return n;
	})()`, x, y, half))
}

// ============================================================ announcing ===

// TestTheLiveRegionIsUsefulAndNotSpam is the test this whole design turns on.
// Announcing every pixel is exactly as useless as announcing none, and the two
// failures look identical from the outside unless something counts.
func TestTheLiveRegionIsUsefulAndNotSpam(t *testing.T) {
	s := openRoom(t, "Announcements", desktopViewport)
	p := s.page

	// The feed must not be a live region. It used to be, which meant a screen
	// reader read out every placement anybody made anywhere - hundreds a minute
	// on a busy canvas, not one of them about you.
	if got := p.evalString(t,
		`document.getElementById('feed').getAttribute('aria-live') || '(none)'`); got != "(none)" {
		t.Errorf("the activity feed is aria-live=%q, so every remote pixel is read aloud", got)
	}

	focusBoard(t, p)

	// What the cursor says has to be worth hearing: where it is and what is
	// under it, in words rather than a hex code.
	p.pressKey(t, "Home", "Home", keyHome, modCtrl)
	arrow(t, p, "Right", 0)
	arrow(t, p, "Down", 0)
	p.waitFor(t, `/1, 1/.test(document.getElementById('srSelf').textContent)`, 5*time.Second)
	if got := said(t, p, "srSelf"); !strings.Contains(got, "background") {
		t.Errorf("moving onto an unpainted cell announced %q, which never says what colour is there", got)
	}

	// Holding an arrow key down must not name every cell crossed. Twenty
	// presses in a row is what a key repeat looks like to the page.
	watchRegion(t, p, "srSelf")
	for i := 0; i < 20; i++ {
		arrow(t, p, "Right", 0)
	}
	p.settle(t)
	time.Sleep(400 * time.Millisecond)
	spoken := heard(t, p, "srSelf")
	if len(spoken) == 0 {
		t.Fatal("crossing twenty cells announced nothing at all")
	}
	if len(spoken) > 6 {
		t.Errorf("crossing twenty cells wrote to the live region %d times; a screen reader reads "+
			"every one of those, so this is the announcement policy failing open:\n  %s",
			len(spoken), strings.Join(spoken, "\n  "))
	}
	// ...and what it settled on is where the cursor actually is.
	x, y := cursorCell(t, p)
	if last := spoken[len(spoken)-1]; !strings.Contains(last, fmt.Sprintf("%d, %d", x, y)) {
		t.Errorf("the cursor is at (%d, %d) but the last thing announced was %q", x, y, last)
	}

	// Choosing a colour is the one piece of state you cannot discover by
	// looking at the cell you are standing on.
	watchRegion(t, p, "srSelf")
	p.pressKey(t, "]", "BracketRight", keyBracketRt, 0)
	p.waitFor(t, `/selected/.test(document.getElementById('srSelf').textContent)`, 5*time.Second)
	if got := said(t, p, "srSelf"); strings.Contains(got, "#") {
		t.Errorf("picking a colour announced %q; a hex code read out as eight characters is not a colour", got)
	}

	// Painting is silent on screen - the pixel simply appears - so it is the
	// one outcome with nothing else to announce it.
	watchRegion(t, p, "srSelf")
	p.pressKey(t, "Enter", "Enter", keyEnter, 0)
	p.waitFor(t, `/[Pp]ainted/.test(document.getElementById('srSelf').textContent)`, 10*time.Second)

	p.requireQuietConsole(t)
}

// TestOtherPeoplePaintingIsSummarisedNotNarrated covers the other half of the
// policy. Somebody has to be able to feel that they are not alone on the canvas
// without being read a list of three hundred coordinates.
func TestOtherPeoplePaintingIsSummarisedNotNarrated(t *testing.T) {
	s := openRoom(t, "Peer noise", desktopViewport)
	p := s.page
	focusBoard(t, p)
	p.pressKey(t, "Home", "Home", keyHome, modCtrl)

	// A pixel landing next to yours is the one remote event worth interrupting
	// for, and it is the reason the canvas feels shared at all.
	watchRegion(t, p, "srPeers")
	s.paintAsSomebodyElse(t, 2, 1, 6)
	p.waitFor(t, `document.getElementById('srPeers').textContent.length > 0`, 15*time.Second)
	near := said(t, p, "srPeers")
	for _, want := range []string{"2, 1", "cursor"} {
		if !strings.Contains(near, want) {
			t.Errorf("a pixel landing beside the cursor announced %q, want it to mention %q", near, want)
		}
	}

	// Now a burst, some of it right beside the cursor, at the rate a busy canvas
	// actually runs at. Announcing each one is the failure this exists to
	// prevent.
	before := p.evalInt(t, `Number(document.getElementById('statPlacements').textContent) || 0`)
	watchRegion(t, p, "srPeers")
	const burst = 40
	for i := 0; i < burst; i++ {
		s.paintAsSomebodyElse(t, i%s.width, 3+(i/s.width), 1+i%9)
	}
	// Every one of them has to have reached this browser before the silence
	// below means anything: "it said nothing" and "nothing arrived" are the
	// same measurement otherwise, and only one of them is a pass.
	p.waitFor(t, fmt.Sprintf(
		`Number(document.getElementById('statPlacements').textContent) >= %d`,
		before+burst), 30*time.Second)
	p.settle(t)
	spoken := heard(t, p, "srPeers")
	if len(spoken) > 2 {
		t.Errorf("%d remote placements produced %d announcements; the digest is not throttling:\n  %s",
			burst, len(spoken), strings.Join(spoken, "\n  "))
	}

	// Silence is not the goal either. Within a digest period it has to say
	// something, and that something has to be a count rather than a coordinate.
	p.waitFor(t, `/pixels painted in the last/.test(document.getElementById('srPeers').textContent)`,
		30*time.Second)
	digest := said(t, p, "srPeers")
	if !strings.Contains(digest, "beside you") {
		t.Errorf("the digest reads %q, and never mentions that any of it happened next to the cursor", digest)
	}

	p.requireQuietConsole(t)
}

// TestReadingTheGridBeyondOneCell covers the readouts that make a canvas
// legible without eyes. One cell at a time is reading a picture through a
// keyhole; these are the three answers to "what is actually here".
func TestReadingTheGridBeyondOneCell(t *testing.T) {
	s := openRoom(t, "Reading", desktopViewport)
	p := s.page

	// A short horizontal run and one cell directly above the middle of it, so
	// every readout below has a known right answer. In the Classic palette these
	// are #ff4d6d and #8ac926, which the client names red and lime — from the
	// hex, because the palette belongs to the room rather than to the client.
	const runColour, aboveColour = 6, 11
	for x := 4; x <= 8; x++ {
		s.paintAsSomebodyElse(t, x, 5, runColour)
	}
	s.paintAsSomebodyElse(t, 6, 4, aboveColour)
	s.waitForPixel(t, 6, 4, aboveColour, 10*time.Second)
	// The readouts describe the grid this tab holds, and the broadcast that
	// fills it arrives on a tick. Reloading is the one way to be certain the
	// client's copy is the server's before asking it to describe itself.
	p.reload(t)
	waitBooted(t, p)

	focusBoard(t, p)
	p.pressKey(t, "Home", "Home", keyHome, modCtrl)
	for i := 0; i < 6; i++ {
		arrow(t, p, "Right", 0)
	}
	for i := 0; i < 5; i++ {
		arrow(t, p, "Down", 0)
	}
	if x, y := cursorCell(t, p); x != 6 || y != 5 {
		t.Fatalf("the cursor is at (%d, %d), want the middle of the painted run at (6, 5)", x, y)
	}

	t.Run("the eight cells around the cursor", func(t *testing.T) {
		p.pressKey(t, "r", "KeyR", keyLetterR, 0)
		p.waitFor(t, `/^Around 6, 5/.test(document.getElementById('srSelf').textContent)`, 5*time.Second)
		got := said(t, p, "srSelf")
		// The run continues east and west and there is a different colour due
		// north, so all three directions have to be in the answer.
		for _, want := range []string{"east", "west", "north"} {
			if !strings.Contains(got, want) {
				t.Errorf("the neighbourhood readout %q never mentions %q", got, want)
			}
		}
		if !strings.Contains(got, "lime") {
			t.Errorf("the neighbourhood readout %q never names the colour directly above the cursor", got)
		}
		if !strings.Contains(got, "red") {
			t.Errorf("the neighbourhood readout %q never names the colour of the run it is standing in", got)
		}
	})

	t.Run("the whole row", func(t *testing.T) {
		p.pressKey(t, "R", "KeyR", keyLetterR, modShift)
		p.waitFor(t, `/^Row 5/.test(document.getElementById('srSelf').textContent)`, 5*time.Second)
		got := said(t, p, "srSelf")
		if !strings.Contains(got, "columns 4 to 8") {
			t.Errorf("the row readout %q does not describe the painted run as columns 4 to 8", got)
		}
		if !strings.Contains(got, "5 of "+fmt.Sprint(s.width)) {
			t.Errorf("the row readout %q does not say how much of the row is painted", got)
		}
	})

	t.Run("where the painted areas are", func(t *testing.T) {
		p.pressKey(t, "m", "KeyM", keyLetterM, 0)
		p.waitFor(t, `/^Canvas /.test(document.getElementById('srSelf').textContent)`, 5*time.Second)
		got := said(t, p, "srSelf")
		if !strings.Contains(got, "top left") {
			t.Errorf("the overview %q does not say the paint is in the top left of the canvas", got)
		}
		if !strings.Contains(got, "cursor is in") {
			t.Errorf("the overview %q never says where the cursor is relative to it", got)
		}
	})

	t.Run("the eyedropper and the question", func(t *testing.T) {
		p.pressKey(t, "e", "KeyE", keyLetterE, 0)
		if got := selectedColour(t, p); got != runColour {
			t.Errorf("picking up the colour under the cursor selected %d, want %d", got, runColour)
		}
		before := countPainted(s.snapshot(t))
		p.pressKey(t, "i", "KeyI", keyLetterI, 0)
		p.waitFor(t, `document.getElementById('inspectPanel').hidden === false`, 5*time.Second)
		if got := focusedName(t, p); got != "aside#inspectPanel" {
			t.Errorf("asking who painted a cell from the keyboard left focus on %s, so the answer "+
				"is on screen and out of reach", got)
		}
		// Asking a question must never be a placement.
		if after := countPainted(s.snapshot(t)); after != before {
			t.Errorf("the eyedropper and the history question painted %d cells", after-before)
		}
	})

	p.requireQuietConsole(t)
}

// TestTracingATemplateNeedsNoMouse covers the loop the template feature exists
// for - go to the next cell that does not match, paint it, repeat - and the
// part of it that was drag-only. Positioning the overlay could not be done
// without a pointing device at all, which made a whole feature of the product
// unavailable rather than merely awkward.
func TestTracingATemplateNeedsNoMouse(t *testing.T) {
	s := openRoom(t, "Keyboard tracing", desktopViewport)
	p := s.page

	const want = 11
	p.clickSelector(t, "#btnTemplate")
	p.waitFor(t, `document.getElementById('tplPanel').hidden === false`, 5*time.Second)
	dropImage(t, p, imageSpec{width: 8, height: 5, fill: s.palette[want]})
	p.waitFor(t, `document.getElementById('tplCount').textContent === '0 of 40 cells match'`, 15*time.Second)

	// "Next cell" reports the unpainted cell nearest the middle of the template,
	// so where it lands is a reading of where the template is.
	focusBoard(t, p)
	p.pressKey(t, "n", "KeyN", 78, 0)
	beforeX, beforeY := cursorCell(t, p)

	// P turns positioning on from the canvas, which is where focus has to be for
	// the arrows to reach the template at all.
	p.pressKey(t, "p", "KeyP", 80, 0)
	p.waitFor(t, `document.getElementById('btnTplMove').getAttribute('aria-pressed') === 'true'`, 5*time.Second)
	cursorX, cursorY := cursorCell(t, p)

	for i := 0; i < 5; i++ {
		arrow(t, p, "Right", 0)
	}
	for i := 0; i < 3; i++ {
		arrow(t, p, "Down", 0)
	}
	p.waitFor(t, `/^Template at /.test(document.getElementById('srSelf').textContent)`, 5*time.Second)

	// The arrows moved the template and not the cursor. If both moved, the mode
	// is decoration.
	if x, y := cursorCell(t, p); x != cursorX || y != cursorY {
		t.Errorf("the cursor moved to (%d, %d) while the template was being positioned, want it "+
			"left at (%d, %d)", x, y, cursorX, cursorY)
	}

	p.pressKey(t, "p", "KeyP", 80, 0)
	p.pressKey(t, "n", "KeyN", 78, 0)
	afterX, afterY := cursorCell(t, p)
	if afterX != beforeX+5 || afterY != beforeY+3 {
		t.Fatalf("after moving the template five right and three down, its next cell is (%d, %d); "+
			"it was (%d, %d), so it should be (%d, %d)",
			afterX, afterY, beforeX, beforeY, beforeX+5, beforeY+3)
	}

	// And the loop closes: the jump picked up the colour the template needs
	// there, so Enter finishes the job.
	if got := selectedColour(t, p); got != want {
		t.Fatalf("jumping to the next cell selected colour %d, want the %d the template needs", got, want)
	}
	p.pressKey(t, "Enter", "Enter", keyEnter, 0)
	s.waitForPixel(t, afterX, afterY, want, 10*time.Second)
	p.waitFor(t, `document.getElementById('tplCount').textContent === '1 of 40 cells match'`, 10*time.Second)

	p.requireQuietConsole(t)
}

// ============================================================ focus ========

// TestEveryControlIsReachableByTab walks the tab order the way a keyboard user
// does and checks it against everything the page says is interactive. A control
// nobody can reach is a control that does not exist, and the failure is
// completely invisible to anybody holding a mouse.
func TestEveryControlIsReachableByTab(t *testing.T) {
	s := openRoom(t, "Tab order", desktopViewport)
	p := s.page

	// Everything visible that claims to be interactive, in document order.
	var want []string
	p.evalInto(t, `(() => {
		const sel = 'a[href], button:not([disabled]), input:not([disabled]), ' +
			'select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';
		// The same shorthand focusedName uses, so the two lists are comparable.
		const name = (a) => {
			if (a.id) return a.tagName.toLowerCase() + '#' + a.id;
			const cls = typeof a.className === 'string' && a.className.trim()
				? '.' + a.className.trim().split(/\s+/).join('.') : '';
			return a.tagName.toLowerCase() + cls;
		};
		return [...new Set([...document.querySelectorAll(sel)]
			// tabIndex < 0 is a deliberate removal from the order rather than an
			// oversight: the palette's unselected swatches are a roving
			// tabindex, and the panels are containers focus is moved to by hand.
			.filter(n => !n.hidden && n.tabIndex >= 0 && n.getClientRects().length > 0 &&
				!n.closest('[aria-hidden="true"]'))
			.map(name))];
	})()`, &want)
	if len(want) < 8 {
		t.Fatalf("the room page reports only %d interactive controls, which cannot be right: %v", len(want), want)
	}

	// And what Tab actually reaches, stopping when it comes back round.
	p.mustEval(t, `document.activeElement.blur()`)
	var got []string
	seen := map[string]bool{}
	for i := 0; i < len(want)+8; i++ {
		tab(t, p)
		name := focusedName(t, p)
		if name == "(nothing)" || seen[name] {
			break
		}
		seen[name] = true
		got = append(got, name)
	}

	// Nothing has been focused on this page yet, so this is the only place the
	// order can honestly be checked from the start of the document.
	if len(got) == 0 || got[0] != "a.skip-link" {
		t.Errorf("the first Tab lands on %v, want the skip link: without one a keyboard user "+
			"pays for the whole header before every single canvas", got)
	}
	for _, control := range want {
		if !seen[control] {
			t.Errorf("%s is on screen and interactive but Tab never reaches it", control)
		}
	}

	// The palette is one stop, not twenty. A radio group that is not roving its
	// tabindex buries the undo button twenty presses deep, which is the kind of
	// thing that is technically reachable and practically not.
	swatchStops := 0
	for _, name := range got {
		if strings.Contains(name, "swatch") {
			swatchStops++
		}
	}
	if swatchStops != 1 {
		t.Errorf("the palette is %d tab stops, want exactly 1: the twenty swatches are a radio "+
			"group and the arrows move within it", swatchStops)
	}
	if n := p.evalInt(t, `document.querySelectorAll('#palette .swatch').length`); n < 10 {
		t.Fatalf("this room's palette only has %d swatches, so the check above proves little", n)
	}

	// Arrowing inside the group has to actually move the selection, or one tab
	// stop is a cage rather than a group.
	p.clickSelector(t, `#palette .swatch:nth-child(3)`)
	before := selectedColour(t, p)
	arrow(t, p, "Right", 0)
	if after := selectedColour(t, p); after != before+1 {
		t.Errorf("arrowing inside the palette moved the selection from %d to %d, want %d",
			before, after, before+1)
	}

	p.requireQuietConsole(t)
}

// TestASheetTakesFocusAndGivesItBack covers the three things that make a modal
// a modal rather than a visual effect: focus goes in, Tab cannot leave, and
// closing puts focus back where it was. Without the last one a keyboard user is
// returned to the top of the document with no explanation.
func TestASheetTakesFocusAndGivesItBack(t *testing.T) {
	s := openRoom(t, "Sheets", desktopViewport)
	p := s.page

	for _, tc := range []struct{ name, opener string }{
		{"the share sheet", "#btnShare"},
		{"the help sheet", "#btnHelp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Focus the opener the way a keyboard user would arrive at it, so
			// "put it back" has somewhere real to put it back to.
			p.mustEval(t, `document.querySelector(`+jsString(tc.opener)+`).focus()`)
			opener := focusedName(t, p)
			activate(t, p)
			p.waitFor(t, `document.getElementById('sheet').hidden === false`, 5*time.Second)

			if !p.evalBool(t, `document.getElementById('sheetInner').contains(document.activeElement)`) {
				t.Fatalf("opening %s left focus on %s, outside the dialog", tc.name, focusedName(t, p))
			}
			if got := p.evalString(t,
				`document.getElementById('sheetInner').getAttribute('aria-modal') || '(none)'`); got != "true" {
				t.Errorf("the dialog is aria-modal=%q, so a screen reader reads straight past it "+
					"into the page behind", got)
			}
			// The dialog has to be named, or it announces as "dialog" and
			// nothing else.
			if got := p.evalString(t, `(() => {
				const by = document.getElementById('sheetInner').getAttribute('aria-labelledby');
				const node = by && document.getElementById(by);
				return node ? node.textContent.trim() : '(unnamed)';
			})()`); got == "" || got == "(unnamed)" {
				t.Errorf("the open dialog has no accessible name")
			}

			// The rest of the page is out of reach while it is open.
			if !p.evalBool(t, `document.querySelector('header.topbar').inert === true`) {
				t.Error("the page behind the dialog is not inert, so Tab and a screen reader both walk into it")
			}

			// Tab all the way round: it must never leave the dialog.
			for i := 0; i < 14; i++ {
				tab(t, p)
				if !p.evalBool(t, `document.getElementById('sheetInner').contains(document.activeElement)`) {
					t.Fatalf("Tab %d escaped the dialog onto %s", i+1, focusedName(t, p))
				}
			}
			shiftTab(t, p)
			if !p.evalBool(t, `document.getElementById('sheetInner').contains(document.activeElement)`) {
				t.Fatalf("Shift+Tab escaped the dialog onto %s", focusedName(t, p))
			}

			p.pressKey(t, "Escape", "Escape", keyEscape, 0)
			p.waitFor(t, `document.getElementById('sheet').hidden === true`, 5*time.Second)
			if got := focusedName(t, p); got != opener {
				t.Errorf("closing %s put focus on %s, want it back on %s", tc.name, got, opener)
			}
			if p.evalBool(t, `document.querySelector('header.topbar').inert === true`) {
				t.Error("the page is still inert after the dialog closed")
			}
		})
	}

	// The statistics sheet is a stack of coloured bars beside a stack of
	// numbers. Read aloud with nothing else, every row of it is a count of
	// something unnamed.
	t.Run("the statistics sheet names its colours", func(t *testing.T) {
		const painted = 11
		s.paintAsSomebodyElse(t, 5, 5, painted)
		s.waitForPixel(t, 5, 5, painted, 10*time.Second)
		p.clickSelector(t, "#btnStats")
		// textContent rather than innerText: the heading is uppercased by CSS,
		// and innerText reports what was rendered.
		p.waitFor(t, `/Most used colours/.test(document.getElementById('sheetBody').textContent)`, 15*time.Second)
		got := p.evalString(t, `document.getElementById('sheetBody').innerText.replace(/\s+/g, ' ')`)
		if !strings.Contains(got, "lime") {
			t.Errorf("the statistics sheet reads %q and never names the colour any of its bars is", got)
		}
		p.pressKey(t, "Escape", "Escape", keyEscape, 0)
		p.waitFor(t, `document.getElementById('sheet').hidden === true`, 5*time.Second)
	})

	p.requireQuietConsole(t)
}

// TestPanelsReturnFocusWhenTheyClose is the same contract for the two panels
// that are not modal. They are opened deliberately in order to be used, so
// focus goes in; and they are closed with a button that then stops existing,
// which is precisely when focus gets dropped on the floor.
func TestPanelsReturnFocusWhenTheyClose(t *testing.T) {
	s := openRoom(t, "Panels", desktopViewport)
	p := s.page

	p.mustEval(t, `document.getElementById('btnTemplate').focus()`)
	activate(t, p)
	p.waitFor(t, `document.getElementById('tplPanel').hidden === false`, 5*time.Second)
	if got := focusedName(t, p); got != "aside#tplPanel" {
		t.Errorf("opening the template panel left focus on %s, so its controls are somewhere "+
			"a keyboard user has to go and find", got)
	}

	// The file input is a real, focusable, named form control rather than a div
	// with a click handler - which is the only version of it a keyboard reaches
	// and a screen reader can describe.
	tab(t, p)
	if got := focusedName(t, p); got != "button#btnTplClose" {
		t.Errorf("the first Tab inside the template panel goes to %s, want its close button", got)
	}
	tab(t, p)
	if got := focusedName(t, p); got != "input#tplFile" {
		t.Errorf("Tab from the close button goes to %s, want the file input", got)
	}
	if got := p.evalString(t, `(() => {
		const n = document.getElementById('tplFile');
		return n.labels && n.labels.length ? n.labels[0].textContent.replace(/\s+/g, ' ').trim() : '(unlabelled)';
	})()`); !strings.Contains(got, "drop an image") {
		t.Errorf("the file input's label is %q, want the visible drop zone text", got)
	}

	p.pressKey(t, "Escape", "Escape", keyEscape, 0)
	p.waitFor(t, `document.getElementById('tplPanel').hidden === true`, 5*time.Second)
	if got := focusedName(t, p); got != "button#btnTemplate" {
		t.Errorf("closing the template panel put focus on %s, want it back on the button that opened it", got)
	}

	// The inspector, closed from its own close button: the element holding
	// focus is about to be removed, which is the case that drops focus.
	focusBoard(t, p)
	p.pressKey(t, "i", "KeyI", keyLetterI, 0)
	p.waitFor(t, `document.getElementById('inspectPanel').hidden === false`, 5*time.Second)
	p.mustEval(t, `document.getElementById('btnInspectClose').focus()`)
	activate(t, p)
	p.waitFor(t, `document.getElementById('inspectPanel').hidden === true`, 5*time.Second)
	if got := focusedName(t, p); got != "canvas#board" {
		t.Errorf("closing the cell history from its own close button left focus on %s, want the canvas", got)
	}

	p.requireQuietConsole(t)
}

// TestTheBootOverlayDoesNotHoldFocus covers the overlay nobody thinks about. It
// covers the entire page while it is up and it is the first thing on it, so if
// it holds focus or keeps the page inert after a failure there is no way out of
// it at all.
func TestTheBootOverlayDoesNotHoldFocus(t *testing.T) {
	s := openRoom(t, "Boot focus", desktopViewport)
	p := s.page

	if !p.evalBool(t, `document.getElementById('boot').hidden`) {
		t.Fatal("the boot overlay is still on screen after the client said it had booted")
	}
	// It is a live region, so the message - especially the failure message - is
	// spoken rather than sat there silently.
	if got := p.evalString(t, `document.getElementById('boot').getAttribute('role') || '(none)'`); got != "status" {
		t.Errorf("the boot overlay is role=%q, so what it says is never announced", got)
	}
	// Nothing focusable is left inside it to trap a Tab on.
	if n := p.evalInt(t, `document.querySelectorAll('#boot a[href], #boot button, #boot [tabindex]:not([tabindex="-1"])').length`); n != 0 {
		t.Errorf("the boot overlay contains %d focusable elements", n)
	}
	if p.evalBool(t, `document.querySelector('main.stage').inert === true`) {
		t.Error("the page is inert after boot, so nothing on it can be reached at all")
	}

	// The route in still works, which is the only thing that actually proves
	// the overlay is out of the way.
	focusBoard(t, p)
	p.requireQuietConsole(t)
}

// ============================================================ contrast =====

// contrastReport is what the page computes for one element: the colour it
// draws, the colour it draws on once every translucent layer above the base has
// been composited, and the ratio between them.
type contrastReport struct {
	Found  bool    `json:"found"`
	FG     string  `json:"fg"`
	BG     string  `json:"bg"`
	Ratio  float64 `json:"ratio"`
	Size   float64 `json:"size"`
	Weight float64 `json:"weight"`
}

// colourJS installs the measuring apparatus in the page.
//
// It measures what a browser renders rather than what a stylesheet says, and
// two things about this design make that necessary. Almost every surface here
// is a color-mix against `transparent`, so the real backdrop of a label is a
// composite of three or four layers - and Chrome computes color-mix to a
// color(srgb ...) value, which a parser that only knows rgb() silently treats
// as absent, quietly measuring every panel as if it were not there.
//
// The second is the interesting one. The HUD panels float over a canvas whose
// contents are whatever anybody painted, so their real backdrop is not a colour
// the stylesheet knows. Everything inside the stage is therefore measured
// against white: the worst case a palette can produce, and the only assumption
// under which a room created next year cannot make the feed unreadable.
const colourJS = `
(() => {
  const parse = (s) => {
    s = String(s);
    let m = s.match(/rgba?\(([^)]+)\)/);
    if (m) {
      const p = m[1].split(/[,\/\s]+/).filter(Boolean).map(Number);
      return { r: p[0], g: p[1], b: p[2], a: p.length > 3 ? p[3] : 1 };
    }
    m = s.match(/color\(srgb\s+([^)]+)\)/);
    if (m) {
      const p = m[1].split(/[\/\s]+/).filter(Boolean).map(Number);
      return { r: p[0] * 255, g: p[1] * 255, b: p[2] * 255, a: p.length > 3 ? p[3] : 1 };
    }
    return null;
  };
  const over = (f, b) => ({
    r: f.r * f.a + b.r * (1 - f.a),
    g: f.g * f.a + b.g * (1 - f.a),
    b: f.b * f.a + b.b * (1 - f.a),
    a: 1,
  });
  const lum = (c) => {
    const f = (v) => { v /= 255; return v <= 0.04045 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4); };
    return 0.2126 * f(c.r) + 0.7152 * f(c.g) + 0.0722 * f(c.b);
  };
  const hex = (c) => '#' + [c.r, c.g, c.b].map(v => Math.round(v).toString(16).padStart(2, '0')).join('');
  const ratio = (a, b) => {
    const x = lum(a), y = lum(b);
    return Math.round(((Math.max(x, y) + 0.05) / (Math.min(x, y) + 0.05)) * 100) / 100;
  };
  // Everything painted behind this element, composited from the page outwards.
  const backdrop = (node) => {
    const chain = [];
    for (let n = node; n; n = n.parentElement) chain.push(n);
    let bg = { r: 10, g: 11, b: 16, a: 1 };
    for (let i = chain.length - 1; i >= 0; i--) {
      // Below the stage is the canvas, and the canvas is whatever somebody
      // painted. Reset to the worst case rather than to today's grid.
      if (chain[i].matches('main.stage')) bg = { r: 255, g: 255, b: 255, a: 1 };
      const c = parse(getComputedStyle(chain[i]).backgroundColor);
      if (c && c.a > 0) bg = over(c, bg);
    }
    return bg;
  };
  window.__pfContrast = (selector) => {
    const node = document.querySelector(selector);
    if (!node || !node.getClientRects().length) return { found: false };
    const style = getComputedStyle(node);
    const bg = backdrop(node);
    const fg = over(parse(style.color) || { r: 0, g: 0, b: 0, a: 1 }, bg);
    return {
      found: true, fg: hex(fg), bg: hex(bg), ratio: ratio(fg, bg),
      size: parseFloat(style.fontSize), weight: parseFloat(style.fontWeight) || 400,
    };
  };
  // The edge of a control against the surface behind it, which is what somebody
  // has to be able to see in order to know the control is there at all.
  window.__pfEdge = (selector) => {
    const node = document.querySelector(selector);
    if (!node || !node.getClientRects().length) return { found: false };
    const style = getComputedStyle(node);
    const bg = node.parentElement ? backdrop(node.parentElement) : { r: 10, g: 11, b: 16, a: 1 };
    const ring = parse(style.borderTopColor);
    const edge = ring && ring.a > 0 && parseFloat(style.borderTopWidth) > 0
      ? over(ring, bg)
      : over(parse(style.backgroundColor) || { r: 0, g: 0, b: 0, a: 0 }, bg);
    return { found: true, fg: hex(edge), bg: hex(bg), ratio: ratio(edge, bg) };
  };
  // A palette swatch has no border and its fill is the colour it is offering,
  // so the ring around it is the only thing separating the control from the bar
  // it sits in. Two details decide whether that ring is real: a translucent one
  // is mostly whatever is underneath it, and an *inset* one is drawn over the
  // swatch rather than over the dock, which is how a 8% white ring on a #12141c
  // swatch came to be invisible against a #11131b dock.
  window.__pfSwatchRing = () => {
    const sw = document.querySelector('#palette .swatch:not(.sel)');
    if (!sw) return { found: false };
    const shadow = getComputedStyle(sw).boxShadow;
    const ring = parse(shadow);
    if (!ring) return { found: false };
    const dock = backdrop(sw.parentElement);
    const under = /\binset\b/.test(shadow)
      ? (parse(getComputedStyle(sw).backgroundColor) || dock)
      : dock;
    const edge = over(ring, under);
    return { found: true, fg: hex(edge), bg: hex(dock), ratio: ratio(edge, dock) };
  };
  return true;
})()`

func measureContrast(t *testing.T, p *page, selector string) contrastReport {
	t.Helper()
	var out contrastReport
	p.evalInto(t, `window.__pfContrast(`+jsString(selector)+`)`, &out)
	return out
}

// TestTextContrastMeetsAA pins every readable thing on the room page against
// WCAG 2.1 1.4.3. It exists so that a palette change cannot quietly walk the
// quiet greys back down to where they were: --text-faint used to be #5a6379,
// which is 3.1:1 on these backgrounds, and every single use of it is small
// text.
func TestTextContrastMeetsAA(t *testing.T) {
	s := openRoom(t, "Contrast", desktopViewport)
	p := s.page

	// Everything on screen at once, so the panels are measured where they
	// actually live rather than in isolation - and so that the labels which only
	// exist once a template is loaded are measured at all.
	s.paintAsSomebodyElse(t, 3, 3, 6)
	p.waitFor(t, `document.querySelectorAll('#feedList li').length > 0`, 15*time.Second)
	p.clickSelector(t, "#btnTemplate")
	p.waitFor(t, `document.getElementById('tplPanel').hidden === false`, 5*time.Second)
	dropImage(t, p, imageSpec{width: 6, height: 4, fill: s.palette[11]})
	p.waitFor(t, `document.getElementById('tplLoaded').hidden === false`, 15*time.Second)
	p.mustEval(t, colourJS)

	for _, sel := range []string{
		".stat-label",
		".conn-label",
		"#coords",
		"#zoomLabel",
		"#cooldownText",
		".feed-head",
		"#feedList li",
		".panel-head",
		".row-label",
		".row.check span",
		".tpl-count",
		"#tplPercent",
		"#btnShare",
		"#btnUndo",
		"#btnTplMove",
		"#roomName",
		"#roomSub",
	} {
		t.Run(sel, func(t *testing.T) {
			got := measureContrast(t, p, sel)
			if !got.Found {
				t.Fatalf("%s is not on screen, so nothing was measured", sel)
			}
			// The large-text allowance spelled out rather than assumed: 18.66px
			// bold or 24px plain. Nothing small here qualifies, which is rather
			// the point of checking.
			want := 4.5
			if got.Size >= 24 || (got.Size >= 18.66 && got.Weight >= 700) {
				want = 3.0
			}
			if got.Ratio < want {
				t.Errorf("%s draws %s on %s at %.2f:1, want at least %.1f:1 for %.1fpx text",
					sel, got.FG, got.BG, got.Ratio, want, got.Size)
			}
		})
	}

	p.requireQuietConsole(t)
}

// TestControlBoundariesMeetAA covers WCAG 1.4.11, which is the rule that is
// easy to forget because the text passes. A ghost button's fill is 1.1:1
// against the bar it sits in, so its border is the entire visual difference
// between a button and a gap - and that border used to be --line at 1.2:1,
// which is to say invisible.
func TestControlBoundariesMeetAA(t *testing.T) {
	s := openRoom(t, "Control edges", desktopViewport)
	p := s.page
	p.mustEval(t, colourJS)

	for _, sel := range []string{"#btnShare", "#btnHelp", "#btnUndo", "#btnZoomIn", "#btnTransport"} {
		t.Run(sel, func(t *testing.T) {
			var got contrastReport
			p.evalInto(t, `window.__pfEdge(`+jsString(sel)+`)`, &got)
			if !got.Found {
				t.Fatalf("%s is not on screen", sel)
			}
			if got.Ratio < 3.0 {
				t.Errorf("%s is bounded by %s on %s at %.2f:1, want 3:1 - the control has no "+
					"visible edge, so there is nothing to say it is a control", sel, got.FG, got.BG, got.Ratio)
			}
		})
	}

	// The palette is the case this rule was written for. The background swatch
	// of the Classic palette is #12141c and the dock behind it is #11131b, so
	// without a ring drawn outside the swatch that control is literally
	// invisible - and it is the one that erases things.
	t.Run("the palette swatch ring", func(t *testing.T) {
		var ring contrastReport
		p.evalInto(t, `window.__pfSwatchRing()`, &ring)
		if !ring.Found {
			t.Fatal("could not read the palette swatch's ring colour")
		}
		if ring.Ratio < 3.0 {
			t.Errorf("the swatch ring is %s on %s at %.2f:1, want 3:1 - a swatch the same colour "+
				"as the bar it sits in is a control nobody can see", ring.FG, ring.BG, ring.Ratio)
		}
	})

	p.requireQuietConsole(t)
}

// TestFocusIsVisibleOnEveryControl. A focus ring on the drop zone and nowhere
// else is the state this page shipped in: Tab moved, and nothing on screen
// changed, so a keyboard user had no idea where they were.
func TestFocusIsVisibleOnEveryControl(t *testing.T) {
	s := openRoom(t, "Focus rings", desktopViewport)
	p := s.page

	p.mustEval(t, `document.activeElement.blur()`)
	checked := 0
	for i := 0; i < 12; i++ {
		tab(t, p)
		name := focusedName(t, p)
		if name == "(nothing)" {
			break
		}
		var ring struct {
			Visible bool    `json:"visible"`
			Style   string  `json:"style"`
			Width   float64 `json:"width"`
		}
		p.evalInto(t, `(() => {
			const a = document.activeElement;
			// :focus-visible is what the stylesheet keys off, so it is what has
			// to match - a rule under :focus alone would never fire here.
			if (!a.matches(':focus-visible')) return { visible: false, style: 'not :focus-visible' };
			const s = getComputedStyle(a);
			const width = parseFloat(s.outlineWidth) || 0;
			const drawn = s.outlineStyle !== 'none' && width > 0;
			return { visible: drawn, style: s.outlineStyle + ' ' + s.outlineColor, width };
		})()`, &ring)
		if !ring.Visible {
			t.Errorf("%s takes focus with no visible outline (%s): Tab moves and nothing on screen changes",
				name, ring.Style)
		} else if ring.Width < 2 {
			t.Errorf("%s has a %.1fpx focus outline, which is too thin to notice", name, ring.Width)
		}
		checked++
	}
	if checked < 6 {
		t.Fatalf("only %d controls were reached, so this proves very little", checked)
	}

	p.requireQuietConsole(t)
}

// ============================================================ targets ======

// TestTouchTargetsAreBigEnough covers WCAG 2.2 2.5.8. The number is 24 by 24
// CSS pixels; a thumb is about nine millimetres. The time-lapse scrubber was a
// four pixel tall slider, which is a sixth of the minimum and completely
// impossible on a touchscreen.
func TestTouchTargetsAreBigEnough(t *testing.T) {
	for _, v := range []struct {
		name string
		vp   viewport
	}{
		{"desktop", desktopViewport},
		{"phone", phoneViewport},
	} {
		t.Run(v.name, func(t *testing.T) {
			s := openRoom(t, "Targets", v.vp)
			p := s.page
			// The controls that only exist once something is open, including the
			// time-lapse scrubber - which needs history to open at all, so the
			// pixel below has to have reached the database first. Without it the
			// bar never appears and the smallest target on the page is measured
			// by not being measured.
			p.clickSelector(t, "#btnTemplate")
			p.waitFor(t, `document.getElementById('tplPanel').hidden === false`, 5*time.Second)

			// Asking for a time-lapse of a canvas nobody has painted on used to
			// throw a TypeError while building the toast that says so, which
			// meant the button did nothing at all and said nothing about it.
			p.clickSelector(t, "#btnLapse")
			p.waitFor(t, `/no history/.test(document.getElementById('toast').textContent)`, 10*time.Second)

			s.paintAsSomebodyElse(t, 1, 1, 6)
			s.waitForHistory(t, 1, 1, 6, 20*time.Second)
			p.clickSelector(t, "#btnLapse")
			p.waitFor(t, `document.getElementById('timelapse').hidden === false`, 20*time.Second)

			type small struct {
				Desc string  `json:"desc"`
				W    float64 `json:"w"`
				H    float64 `json:"h"`
			}
			var tooSmall []small
			p.evalInto(t, `(() => {
				const sel = 'a[href], button:not([disabled]), input:not([disabled]), ' +
					'select:not([disabled]), [tabindex]:not([tabindex="-1"])';
				const out = [];
				for (const n of document.querySelectorAll(sel)) {
					if (n.hidden || !n.getClientRects().length) continue;
					const style = getComputedStyle(n);
					if (style.display === 'none' || style.visibility === 'hidden') continue;
					// A visually hidden control is operated through the label
					// wrapped round it, and the canvas is the whole page.
					if (n.classList.contains('sr-only') || n.id === 'board') continue;
					// The target of a checkbox is the label you click, not the
					// thirteen pixel box the browser draws.
					const target = n.closest('label') || n;
					const r = target.getBoundingClientRect();
					if (r.width >= 24 && r.height >= 24) continue;
					const cls = typeof n.className === 'string' && n.className.trim()
						? '.' + n.className.trim().split(/\s+/).join('.') : '';
					out.push({
						desc: n.tagName.toLowerCase() + (n.id ? '#' + n.id : '') + cls,
						w: Math.round(r.width * 10) / 10, h: Math.round(r.height * 10) / 10,
					});
				}
				return out;
			})()`, &tooSmall)

			if len(tooSmall) > 0 {
				var b strings.Builder
				for _, c := range tooSmall {
					fmt.Fprintf(&b, "\n  %s is %.1f x %.1f", c.Desc, c.W, c.H)
				}
				t.Errorf("%d control(s) are under the 24 x 24 minimum:%s", len(tooSmall), b.String())
			}

			// And a floor under the check itself: if nothing was measured the
			// assertion above is vacuous.
			if n := p.evalInt(t, `document.querySelectorAll('#palette .swatch').length`); n < 10 {
				t.Fatalf("only %d swatches were on screen to measure", n)
			}
			if !p.evalBool(t, `document.getElementById('scrub').getClientRects().length > 0`) {
				t.Fatal("the time-lapse scrubber was never on screen, so the smallest target on " +
					"the page was measured by not being measured")
			}

			p.requireQuietConsole(t)
		})
	}
}

// ============================================================ motion =======

// TestReducedMotionStopsTheAnimationsThatLoop. The old rule set every duration
// to .01ms, which does nothing for an animation that runs `infinite` - it
// restarts it every frame, which is a faster flicker than the animation it
// replaced and precisely what somebody sets this preference to avoid.
func TestReducedMotionStopsTheAnimationsThatLoop(t *testing.T) {
	s := openRoom(t, "Reduced motion", desktopViewport)
	p := s.page
	p.mustCall(t, "Emulation.setEmulatedMedia", map[string]any{
		"features": []map[string]string{{"name": "prefers-reduced-motion", "value": "reduce"}},
	})
	p.settle(t)

	// The dot only animates while the transport is reconnecting, so put it
	// there rather than assert about a state the page is not in.
	p.mustEval(t, `document.getElementById('connDot').className = 'conn-dot wait'`)

	for _, tc := range []struct{ name, selector string }{
		{"the boot grid's pulse", "#boot .boot-grid i"},
		{"the reconnecting dot", "#connDot"},
		{"the feed's slide-in", "#feed li, #feed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got struct {
				Found     bool    `json:"found"`
				Duration  string  `json:"duration"`
				Iteration string  `json:"iteration"`
				Millis    float64 `json:"ms"`
			}
			p.evalInto(t, `(() => {
				const n = document.querySelector(`+jsString(tc.selector)+`);
				if (!n) return { found: false };
				const s = getComputedStyle(n);
				return {
					found: true,
					duration: s.animationDuration,
					iteration: s.animationIterationCount,
					ms: parseFloat(s.animationDuration) * (s.animationDuration.endsWith('ms') ? 1 : 1000),
				};
			})()`, &got)
			if !got.Found {
				t.Skipf("%s is not on the page right now", tc.selector)
			}
			if got.Millis > 1 {
				t.Errorf("%s still animates for %s under prefers-reduced-motion", tc.name, got.Duration)
			}
			// The part the old rule missed. An infinite animation at .01ms is
			// not a stopped animation, it is a strobe.
			if got.Iteration != "1" {
				t.Errorf("%s runs %s iterations under prefers-reduced-motion; at a duration of %s "+
					"that is a restart every frame rather than a stop", tc.name, got.Iteration, got.Duration)
			}
		})
	}

	p.requireQuietConsole(t)
}

// ============================================================ other pages ==

// TestHomePageFormIsLabelledAndNavigable. The creation form is where somebody
// meets this product, and its three "radio groups" were buttons wearing
// role="radio" with no aria-checked on any of them - which announces four
// unchecked options and no way to tell which size the form will submit.
func TestHomePageFormIsLabelledAndNavigable(t *testing.T) {
	dsn := testDSN(t)
	browser := launchChrome(t)
	srv := newServer(t, dsn)

	p := browser.newPage(t)
	p.setViewport(t, desktopViewport)
	p.navigate(t, srv.URL+"/")
	p.waitFor(t, `document.querySelectorAll('#fSizes [role=radio]').length > 0 &&
		document.querySelectorAll('#fPalettes [role=radio]').length > 0`, 20*time.Second)

	// A skip link, and it actually goes somewhere focusable.
	p.mustEval(t, `document.activeElement.blur()`)
	tab(t, p)
	if got := focusedName(t, p); got != "a.skip-link" {
		t.Errorf("the first Tab on the home page lands on %s, want a skip link", got)
	}
	if got := p.evalString(t, `document.querySelector('.skip-link').getAttribute('href')`); got != "#main" {
		t.Errorf("the skip link points at %q, which is not the main landmark", got)
	}
	if !p.evalBool(t, `!!document.querySelector('main#main')`) {
		t.Error("the skip link's target does not exist")
	}

	// Headings in order, because a screen reader user navigates by them and a
	// jump from h1 to h3 reads as content having gone missing.
	var levels []int
	p.evalInto(t, `[...document.querySelectorAll('main h1, main h2, main h3, main h4')]
		.map(h => Number(h.tagName.slice(1)))`, &levels)
	if len(levels) == 0 || levels[0] != 1 {
		t.Fatalf("the home page's headings start at %v, want an h1 first", levels)
	}
	for i := 1; i < len(levels); i++ {
		if levels[i] > levels[i-1]+1 {
			t.Errorf("headings jump from h%d to h%d at position %d: %v",
				levels[i-1], levels[i], i, levels)
		}
	}

	// Open the creator, which is a disclosure and has to say so.
	p.clickSelector(t, "#btnStart")
	p.waitFor(t, `document.getElementById('creator').hidden === false`, 5*time.Second)
	if got := p.evalString(t, `document.getElementById('btnStart').getAttribute('aria-expanded')`); got != "true" {
		t.Errorf("#btnStart reports aria-expanded=%q after revealing the form", got)
	}
	if got := focusedName(t, p); got != "input#fName" {
		t.Errorf("revealing the form left focus on %s, want the first field", got)
	}

	for _, group := range []string{"fSizes", "fCooldowns", "fPalettes"} {
		t.Run(group, func(t *testing.T) {
			var state struct {
				Options int    `json:"options"`
				Checked int    `json:"checked"`
				Stops   int    `json:"stops"`
				Name    string `json:"name"`
				Unnamed int    `json:"unnamed"`
			}
			p.evalInto(t, `(() => {
				const g = document.getElementById(`+jsString(group)+`);
				const items = [...g.querySelectorAll('[role=radio]')];
				const by = g.getAttribute('aria-labelledby');
				const label = by && document.getElementById(by);
				return {
					options: items.length,
					checked: items.filter(n => n.getAttribute('aria-checked') === 'true').length,
					stops: items.filter(n => n.tabIndex === 0).length,
					name: label ? label.textContent.trim() : (g.getAttribute('aria-label') || ''),
					unnamed: items.filter(n => !(n.getAttribute('aria-label') || n.textContent).trim()).length,
				};
			})()`, &state)
			if state.Options < 2 {
				t.Fatalf("%s has %d options", group, state.Options)
			}
			if state.Checked != 1 {
				t.Errorf("%s has %d of %d options marked aria-checked, want exactly 1: a radio group "+
					"with no checked option announces every choice as unselected", group, state.Checked, state.Options)
			}
			if state.Stops != 1 {
				t.Errorf("%s is %d tab stops, want 1 with the arrows moving inside it", group, state.Stops)
			}
			if state.Name == "" {
				t.Errorf("%s has no accessible name, so it announces as an unlabelled group", group)
			}
			if state.Unnamed > 0 {
				t.Errorf("%s has %d options with nothing to announce", group, state.Unnamed)
			}

			// Arrowing moves the selection, which is what makes one tab stop a
			// group rather than a cage.
			before := p.evalInt(t, `[...document.getElementById(`+jsString(group)+
				`).querySelectorAll('[role=radio]')].findIndex(n => n.getAttribute('aria-checked') === 'true')`)
			p.mustEval(t, `[...document.getElementById(`+jsString(group)+
				`).querySelectorAll('[role=radio]')].find(n => n.tabIndex === 0).focus()`)
			arrow(t, p, "Right", 0)
			after := p.evalInt(t, `[...document.getElementById(`+jsString(group)+
				`).querySelectorAll('[role=radio]')].findIndex(n => n.getAttribute('aria-checked') === 'true')`)
			if after == before {
				t.Errorf("arrowing inside %s left the selection on option %d", group, before)
			}
		})
	}

	// The name field is labelled by the thing that visually labels it.
	if got := p.evalString(t, `(() => {
		const n = document.getElementById('fName');
		return n.labels && n.labels.length ? n.labels[0].textContent.trim() : '(unlabelled)';
	})()`); !strings.Contains(strings.ToLower(got), "name") {
		t.Errorf("the canvas name field's label is %q", got)
	}

	// A failure has to reach somebody who is not looking at that corner of the
	// page. The server sanitises rather than refuses, so the honest way to make
	// this fail is the way it fails in the world: the request does not arrive.
	p.watchNetwork(t)
	p.mustCall(t, "Network.emulateNetworkConditions", map[string]any{
		"offline": true, "latency": 0, "downloadThroughput": -1, "uploadThroughput": -1,
	})
	t.Cleanup(func() {
		_, _ = p.call("Network.emulateNetworkConditions", map[string]any{
			"offline": false, "latency": 0, "downloadThroughput": -1, "uploadThroughput": -1,
		})
	})
	// Focused and pressed rather than clicked: revealing the form scrolls it
	// into view, and a click aimed at a rectangle that is still moving lands
	// somewhere else. Pressing the button a keyboard user has already reached
	// does not depend on where it happens to be this frame.
	p.mustEval(t, `document.getElementById('btnCreate').focus()`)
	activate(t, p)
	p.waitFor(t, `document.getElementById('createError').hidden === false`, 15*time.Second)
	if got := p.evalString(t, `document.getElementById('createError').getAttribute('role') || '(none)'`); got != "alert" {
		t.Errorf("the creation error is role=%q, so nothing announces it when the form refuses", got)
	}
}

// TestEmbedAndNotFoundAreDescribed covers the two pages that are easy to
// forget. An embed is somebody else's page as far as its visitor is concerned,
// and a bare <canvas> with an aria-label on it is a label on nothing: canvas
// has no implicit role, so the label is dropped.
func TestEmbedAndNotFoundAreDescribed(t *testing.T) {
	dsn := testDSN(t)
	browser := launchChrome(t)
	srv := newServer(t, dsn)
	s := &session{dsn: dsn, srv: srv, chrome: browser, api: newClient(), name: "Embedded"}
	s.createRoom(t, roomSpec{name: "Embedded", viewport: desktopViewport})

	p := browser.newPage(t)
	p.setViewport(t, desktopViewport)

	t.Run("the embed", func(t *testing.T) {
		p.navigate(t, srv.URL+"/embed/"+s.slug)
		p.waitFor(t, `!!document.getElementById('board')`, 20*time.Second)
		if got := p.evalString(t, `document.getElementById('board').getAttribute('role') || '(none)'`); got != "img" {
			t.Errorf("the embedded canvas is role=%q; a <canvas> has no implicit role, so its "+
				"aria-label is a label on nothing", got)
		}
		if got := p.evalString(t, `document.getElementById('board').getAttribute('aria-label') || ''`); !strings.Contains(got, s.name) {
			t.Errorf("the embedded canvas is labelled %q, which never names the room", got)
		}
		if got := p.evalString(t, `(document.querySelector('h1') || {}).textContent || '(none)'`); !strings.Contains(got, s.name) {
			t.Errorf("the embed's heading is %q; a framed document with no heading gives a screen "+
				"reader nothing to say about what it landed in", got)
		}
		// The badge opens a new tab, and something has to say so.
		if got := p.evalString(t, `document.getElementById('embedLink').textContent.replace(/\s+/g, ' ')`); !strings.Contains(got, "new tab") {
			t.Errorf("the embed link reads %q and never warns that it opens elsewhere", got)
		}
	})

	t.Run("not found", func(t *testing.T) {
		p.navigate(t, srv.URL+"/r/definitely-not-a-canvas")
		p.waitFor(t, `document.readyState === 'complete'`, 20*time.Second)
		if n := p.evalInt(t, `document.querySelectorAll('h1').length`); n != 1 {
			t.Errorf("the 404 page has %d h1 elements, want exactly 1", n)
		}
		if !p.evalBool(t, `!!document.querySelector('main')`) {
			t.Error("the 404 page has no main landmark")
		}
		// The decorative grid must not be read out as nine empty things.
		if !p.evalBool(t, `document.querySelector('.nf-art').getAttribute('aria-hidden') === 'true'`) {
			t.Error("the 404 page's decorative pixel grid is exposed to assistive technology")
		}
	})
}

// TestRoomPageStructureIsAnnounceable checks the handful of attributes that
// decide whether the room reads as an application or as a pile of unlabelled
// boxes. Each of these is one deletion away from silently regressing, and none
// of them changes a single pixel when it does.
func TestRoomPageStructureIsAnnounceable(t *testing.T) {
	s := openRoom(t, "Structure", desktopViewport)
	p := s.page

	type attrCase struct {
		selector string
		attr     string
		want     string
		why      string
	}
	for _, tc := range []attrCase{
		{"#board", "role", "application",
			"without it NVDA and JAWS keep the arrow keys for their own reading cursor and the canvas never sees them"},
		{"#board", "tabindex", "0", "the canvas cannot be reached at all without it"},
		{"#minimap", "aria-hidden", "true",
			"it is a second drawing of the canvas already on screen, and its one interaction has a keyboard equivalent"},
		{"#srSelf", "aria-live", "polite", "nothing the person does would be announced"},
		{"#srPeers", "aria-live", "polite", "nothing anybody else does would be announced"},
	} {
		t.Run(tc.selector+" "+tc.attr, func(t *testing.T) {
			got := p.evalString(t, `(() => {
				const n = document.querySelector(`+jsString(tc.selector)+`);
				return n ? (n.getAttribute(`+jsString(tc.attr)+`) || '(unset)') : '(missing)';
			})()`)
			if got != tc.want {
				t.Errorf("%s has %s=%q, want %q: %s", tc.selector, tc.attr, got, tc.want, tc.why)
			}
		})
	}

	// The canvas's name has to say what it is and how big it is, and its keys
	// have to be somewhere that is not the name - a label read on every focus
	// cannot be a paragraph.
	label := p.evalString(t, `document.getElementById('board').getAttribute('aria-label') || ''`)
	if !strings.Contains(label, fmt.Sprint(s.width)) || !strings.Contains(label, fmt.Sprint(s.height)) {
		t.Errorf("the canvas is labelled %q, which never says how big it is", label)
	}
	if len(label) > 120 {
		t.Errorf("the canvas label is %d characters; it is read on every focus", len(label))
	}
	described := p.evalString(t, `(() => {
		const by = document.getElementById('board').getAttribute('aria-describedby');
		const n = by && document.getElementById(by);
		return n ? n.textContent.replace(/\s+/g, ' ').trim() : '';
	})()`)
	for _, want := range []string{"Arrow keys", "Enter", "paints"} {
		if !strings.Contains(described, want) {
			t.Errorf("the canvas description never mentions %q: %q", want, described)
		}
	}

	// Every image and every control has something to announce.
	var unnamed []string
	p.evalInto(t, `(() => {
		const out = [];
		for (const n of document.querySelectorAll('button, a[href], input, select')) {
			if (n.hidden || !n.getClientRects().length) continue;
			if (n.closest('[aria-hidden="true"]')) continue;
			const label = (n.getAttribute('aria-label') || '').trim();
			const text = (n.textContent || '').trim();
			const labelled = n.labels && n.labels.length
				? [...n.labels].map(l => l.textContent.trim()).join(' ') : '';
			const by = n.getAttribute('aria-labelledby');
			const ref = by && document.getElementById(by) ? document.getElementById(by).textContent.trim() : '';
			if (label || text || labelled || ref) continue;
			const cls = typeof n.className === 'string' && n.className.trim()
				? '.' + n.className.trim().split(/\s+/).join('.') : '';
			out.push(n.tagName.toLowerCase() + (n.id ? '#' + n.id : '') + cls);
		}
		return out;
	})()`, &unnamed)
	if len(unnamed) > 0 {
		t.Errorf("%d control(s) have no accessible name: %s", len(unnamed), strings.Join(unnamed, ", "))
	}

	p.requireQuietConsole(t)
}
