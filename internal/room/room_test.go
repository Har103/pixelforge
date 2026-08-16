package room

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Har103/pixelforge/internal/canvas"
	"github.com/Har103/pixelforge/internal/hub"
)

// TestZeroCooldownIsHonoured is a regression test with a story. The first cut
// of Normalise used a single clamp helper that treated 0 as "unset", so the
// creation form's "No cooldown" option silently produced a 750ms cooldown and
// a canvas that felt broken. Zero is a real answer here.
func TestZeroCooldownIsHonoured(t *testing.T) {
	s := Spec{Name: "x", CooldownMs: 0}
	s.Normalise()
	if s.CooldownMs != 0 {
		t.Errorf("cooldown = %d, want 0 — an explicit zero must survive normalisation", s.CooldownMs)
	}
}

func TestUnsetCooldownGetsTheDefault(t *testing.T) {
	s := Spec{Name: "x", CooldownMs: -1}
	s.Normalise()
	if s.CooldownMs != 750 {
		t.Errorf("cooldown = %d, want the 750ms default", s.CooldownMs)
	}
}

func TestNormaliseClamps(t *testing.T) {
	s := Spec{Name: "x", Width: 5, Height: 99999, CooldownMs: 1 << 30}
	s.Normalise()
	if s.Width != MinDim {
		t.Errorf("width = %d, want the minimum %d", s.Width, MinDim)
	}
	if s.Height != MaxDim {
		t.Errorf("height = %d, want the maximum %d", s.Height, MaxDim)
	}
	if s.CooldownMs != MaxCooldownMs {
		t.Errorf("cooldown = %d, want the maximum", s.CooldownMs)
	}
}

func TestNormaliseDefaultsDimensions(t *testing.T) {
	s := Spec{Name: "x"}
	s.Normalise()
	if s.Width != 128 || s.Height != 128 {
		t.Errorf("default size = %dx%d, want 128x128", s.Width, s.Height)
	}
}

func TestNormaliseNames(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "Untitled canvas"},
		{"   ", "Untitled canvas"},
		{"  Friday doodle  ", "Friday doodle"},
		{"drop\x00the\x07controls", "dropthecontrols"},
		{strings.Repeat("a", 200), strings.Repeat("a", MaxNameLen)},
	}
	for _, tc := range cases {
		s := Spec{Name: tc.in}
		s.Normalise()
		if s.Name != tc.want {
			t.Errorf("Normalise(%q) name = %q, want %q", tc.in, s.Name, tc.want)
		}
	}
}

func TestNormaliseFallsBackToAKnownPalette(t *testing.T) {
	s := Spec{Name: "x", Palette: "does-not-exist"}
	s.Normalise()
	if s.Palette != canvas.DefaultPaletteKey {
		t.Errorf("palette = %q, want the default", s.Palette)
	}
	s = Spec{Name: "x", Palette: "neon"}
	s.Normalise()
	if s.Palette != "neon" {
		t.Errorf("a real palette should be kept, got %q", s.Palette)
	}
}

func TestSlugsAreValidAndVaried(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		s := NewSlug("Friday standup doodle", 0)
		if !ValidSlug(s) {
			t.Fatalf("generated slug %q is not valid", s)
		}
		if !strings.HasPrefix(s, "friday-standup-doodle-") {
			t.Fatalf("slug %q should be derived from the name", s)
		}
		seen[s] = true
	}
	if len(seen) < 100 {
		t.Errorf("only %d distinct slugs in 200 draws; the suffix is not random enough", len(seen))
	}
}

func TestSlugFallsBackWhenTheNameIsUnusable(t *testing.T) {
	for _, name := range []string{"", "!!!", "日本語", "   "} {
		s := NewSlug(name, 0)
		if !ValidSlug(s) {
			t.Errorf("NewSlug(%q) = %q, which is not a valid slug", name, s)
		}
	}
	// A retry must not reuse the name, or a collision would repeat forever.
	retry := NewSlug("collide", 1)
	if strings.HasPrefix(retry, "collide-") {
		t.Errorf("retry slug %q reuses the name that just collided", retry)
	}
	if !ValidSlug(retry) {
		t.Errorf("retry slug %q is not valid", retry)
	}
}

func TestValidSlug(t *testing.T) {
	good := []string{"abc", "amber-otter-1234", "a-1", strings.Repeat("a", 48)}
	bad := []string{"", "ab", "Has-Capitals", "has_underscore", "has space",
		"has/slash", "..", strings.Repeat("a", 49), "unicodé"}

	for _, s := range good {
		if !ValidSlug(s) {
			t.Errorf("ValidSlug(%q) = false, want true", s)
		}
	}
	for _, s := range bad {
		if ValidSlug(s) {
			t.Errorf("ValidSlug(%q) = true, want false", s)
		}
	}
}

func TestLockContains(t *testing.T) {
	l := Lock{X1: 4, Y1: 4, X2: 8, Y2: 6}
	inside := [][2]int{{4, 4}, {8, 6}, {6, 5}, {4, 6}}
	outside := [][2]int{{3, 4}, {9, 5}, {6, 3}, {6, 7}}

	for _, p := range inside {
		if !l.Contains(p[0], p[1]) {
			t.Errorf("(%d,%d) should be inside the lock", p[0], p[1])
		}
	}
	for _, p := range outside {
		if l.Contains(p[0], p[1]) {
			t.Errorf("(%d,%d) should be outside the lock", p[0], p[1])
		}
	}
}

func TestPaletteCatalogue(t *testing.T) {
	all := canvas.Palettes()
	if len(all) < 3 {
		t.Fatal("expected several palettes to choose from")
	}
	if all[0].Key != canvas.DefaultPaletteKey {
		t.Errorf("the default palette should be listed first, got %q", all[0].Key)
	}
	for _, p := range all {
		if len(p.Colors) < 2 {
			t.Errorf("palette %q has %d colours", p.Key, len(p.Colors))
		}
		if len(p.Colors) > 256 {
			t.Errorf("palette %q has %d colours, which will not fit in a byte", p.Key, len(p.Colors))
		}
		if p.Name == "" || p.Note == "" {
			t.Errorf("palette %q is missing its display text", p.Key)
		}
		for i, hex := range p.Colors {
			if len(hex) != 7 || hex[0] != '#' {
				t.Errorf("palette %q colour %d = %q is not #rrggbb", p.Key, i, hex)
			}
		}
	}
}

// ----------------------------------------------------------- live cursors --

// Cursors need no database and no history: they are the one part of a room that
// is allowed to be forgotten. So these build the smallest Room the cursor code
// touches — a grid to bound coordinates against, a hub to broadcast through —
// and leave the store nil, which also proves nothing here reaches for it.
func newCursorRoom(width, height int) *Room {
	return &Room{
		Canvas:  canvas.New(width, height, nil, 0),
		Hub:     hub.New(slog.New(slog.NewTextHandler(io.Discard, nil))),
		cursors: map[string]Cursor{},
		stopped: make(chan struct{}),
	}
}

// TestCursorsExpireAfterTheirTTL covers the ghost: a tab closed by a phone
// going to sleep never sends a disconnect, so the only thing that clears its
// pointer is the pointer growing old.
func TestCursorsExpireAfterTheirTTL(t *testing.T) {
	rm := newCursorRoom(32, 32)
	rm.SetCursor("alec", 3, 4, 2)
	rm.SetCursor("bess", 5, 6, 3)
	rm.liveCursors() // consume their arrival, so anything dirty after this is the expiry

	rm.curMu.Lock()
	stale := rm.cursors["alec"]
	stale.at = time.Now().Add(-cursorTTL - time.Second)
	rm.cursors["alec"] = stale
	fresh := rm.cursors["bess"]
	fresh.at = time.Now().Add(-cursorTTL + time.Second) // old, but not yet gone
	rm.cursors["bess"] = fresh
	rm.curMu.Unlock()

	live, changed := rm.liveCursors()
	if len(live) != 1 || live[0].UID != "bess" {
		t.Fatalf("live cursors = %+v, want only bess, whose pointer is still inside the TTL", live)
	}
	if !changed {
		t.Error("an expiry left the set clean, so the ghost stays on every screen until somebody else happens to move")
	}

	rm.curMu.Lock()
	_, kept := rm.cursors["alec"]
	rm.curMu.Unlock()
	if kept {
		t.Error("the expired cursor is still in the map; it would be walked on every tick for the life of the room")
	}
}

// TestCursorsOutsideTheGridAreDropped pins the decision not to clamp. A cursor
// pinned to the edge because a client sent nonsense claims somebody is pointing
// somewhere they are not, which is worse than showing no cursor at all.
func TestCursorsOutsideTheGridAreDropped(t *testing.T) {
	rm := newCursorRoom(16, 16)

	for _, bad := range [][2]int{{-1, 0}, {0, -1}, {16, 0}, {0, 16}, {999, 999}} {
		rm.SetCursor("alec", bad[0], bad[1], 1)
	}
	rm.SetCursor("", 4, 4, 1) // and a frame with no painter at all

	live, changed := rm.liveCursors()
	if len(live) != 0 {
		t.Errorf("live cursors = %+v, want none: every one of those was outside the grid", live)
	}
	if changed {
		t.Error("a rejected cursor marked the set dirty, which spends a broadcast on nothing")
	}

	// The far corner is inside the grid. An off-by-one here would drop the
	// cursor of everybody painting along the right and bottom edges.
	rm.SetCursor("alec", 0, 0, 1)
	rm.SetCursor("bess", 15, 15, 2)
	if live, _ = rm.liveCursors(); len(live) != 2 {
		t.Errorf("corner cursors = %+v, want both (0,0) and (15,15) kept", live)
	}
}

// TestTheCursorDirtyFlagTracksRealChanges guards the tick that costs nothing.
// Clients report their pointer on a timer, so most frames say exactly what the
// last one said; if those counted as changes, every room with a mouse in it
// would broadcast eight times a second forever.
func TestTheCursorDirtyFlagTracksRealChanges(t *testing.T) {
	rm := newCursorRoom(32, 32)

	rm.SetCursor("alec", 3, 4, 2)
	if _, changed := rm.liveCursors(); !changed {
		t.Fatal("a cursor that has just appeared is a change")
	}

	live, changed := rm.liveCursors()
	if changed {
		t.Error("the dirty flag survived being read, so the room broadcasts whether or not anybody moved")
	}
	if len(live) != 1 {
		t.Errorf("live cursors = %+v, want alec still there: a quiet tick must not forget where people are", live)
	}

	rm.SetCursor("alec", 3, 4, 2) // the same cell, the same colour
	if _, changed := rm.liveCursors(); changed {
		t.Error("re-sending an identical position counted as a change")
	}

	rm.SetCursor("alec", 3, 5, 2) // moved
	if _, changed := rm.liveCursors(); !changed {
		t.Error("a pointer that moved did not mark the set dirty")
	}

	rm.SetCursor("alec", 3, 5, 9) // same cell, new colour
	if _, changed := rm.liveCursors(); !changed {
		t.Error("picking a new colour did not mark the set dirty, so everyone else keeps seeing the old swatch under alec's pointer")
	}
}

func TestDropCursorRemovesItImmediately(t *testing.T) {
	rm := newCursorRoom(32, 32)
	rm.SetCursor("alec", 1, 2, 3)
	rm.liveCursors() // consume the change from the arrival

	rm.DropCursor("alec")
	live, changed := rm.liveCursors()
	if len(live) != 0 {
		t.Errorf("live cursors = %+v, want none: a closed socket should take its pointer with it rather than leave it for %v", live, cursorTTL)
	}
	if !changed {
		t.Error("a departure left the set clean, so every client keeps drawing a pointer for somebody who has gone")
	}

	// Dropping somebody who was never here is not news.
	rm.DropCursor("ghost")
	if _, changed := rm.liveCursors(); changed {
		t.Error("dropping an unknown painter marked the set dirty")
	}
}

// TestCursorsSurviveConcurrentUse is the -race test. In a real room every
// pointer arrives on its own socket goroutine while the broadcast loop reads
// the set and closing sockets delete from it, so all three have to share.
func TestCursorsSurviveConcurrentUse(t *testing.T) {
	rm := newCursorRoom(64, 64)

	const painters, moves = 8, 200
	var wg sync.WaitGroup

	for i := 0; i < painters; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			uid := fmt.Sprintf("painter-%d", i)
			for m := 0; m < moves; m++ {
				rm.SetCursor(uid, m%64, (m*7)%64, uint8(m%16))
			}
		}(i)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < moves; i++ {
			rm.liveCursors()
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < moves; i++ {
			rm.DropCursor("painter-3")
		}
	}()

	wg.Wait()

	live, _ := rm.liveCursors()
	if len(live) > painters {
		t.Errorf("%d cursors for %d painters, so somebody has more than one pointer", len(live), painters)
	}
	for _, c := range live {
		if c.UID == "" {
			t.Errorf("cursor %+v belongs to nobody", c)
		}
		if c.X < 0 || c.X >= 64 || c.Y < 0 || c.Y >= 64 {
			t.Errorf("cursor %+v escaped the grid", c)
		}
	}
}

// cursorFrame is the shape broadcastCursors puts on the wire. Decoding into
// Cursor rather than a loose map also pins the field names the client reads.
type cursorFrame struct {
	T string   `json:"t"`
	C []Cursor `json:"c"`
}

// nextCursorFrame waits for a cursor broadcast, skipping the presence messages
// the hub sends of its own accord.
func nextCursorFrame(sub *hub.Subscriber, wait time.Duration) ([]Cursor, bool) {
	deadline := time.After(wait)
	for {
		select {
		case f, ok := <-sub.C:
			if !ok {
				return nil, false
			}
			var msg cursorFrame
			if err := json.Unmarshal(f.Data, &msg); err != nil || msg.T != "cursors" {
				continue
			}
			return msg.C, true
		case <-deadline:
			return nil, false
		}
	}
}

// TestCursorsAreNotBroadcastWhenNobodyIsListening keeps an empty room free.
// Consuming the change while there is nobody to send it to would be worse than
// wasteful: the next person to open the room would see no pointers at all until
// somebody happened to move.
func TestCursorsAreNotBroadcastWhenNobodyIsListening(t *testing.T) {
	rm := newCursorRoom(32, 32)
	go rm.broadcastCursors()
	defer close(rm.stopped)

	rm.SetCursor("alec", 3, 4, 2)
	time.Sleep(500 * time.Millisecond) // four ticks' worth

	rm.curMu.Lock()
	dirty, held := rm.curDirty, len(rm.cursors)
	rm.curMu.Unlock()

	if !dirty {
		t.Error("an empty room consumed the cursor change anyway, so the first client to connect sees nothing until somebody moves")
	}
	if held != 1 {
		t.Errorf("the room is holding %d cursors, want alec's kept for whoever arrives next", held)
	}
}

func TestCursorsBroadcastOnlyWhenSomethingMoved(t *testing.T) {
	rm := newCursorRoom(32, 32)
	sub := rm.Hub.Subscribe("sse")
	go rm.broadcastCursors()
	defer close(rm.stopped)

	rm.SetCursor("alec", 3, 4, 7)
	got, ok := nextCursorFrame(sub, 2*time.Second)
	if !ok {
		t.Fatal("a moved pointer was never broadcast to the client watching the room")
	}
	if len(got) != 1 || got[0].UID != "alec" || got[0].X != 3 || got[0].Y != 4 || got[0].Colour != 7 {
		t.Fatalf("broadcast cursors = %+v, want alec at (3,4) holding colour 7", got)
	}

	// Now nothing happens. The room has to go quiet rather than repost the same
	// positions eight times a second at everybody in it.
	if extra, ok := nextCursorFrame(sub, 600*time.Millisecond); ok {
		t.Errorf("an idle room broadcast cursors again: %+v", extra)
	}

	rm.SetCursor("alec", 8, 9, 7)
	moved, ok := nextCursorFrame(sub, 2*time.Second)
	if !ok {
		t.Fatal("the room stayed quiet after the pointer moved again")
	}
	if len(moved) != 1 || moved[0].X != 8 || moved[0].Y != 9 {
		t.Errorf("broadcast cursors = %+v, want alec at (8,9)", moved)
	}
}
