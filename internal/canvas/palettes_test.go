package canvas

import (
	"sort"
	"strings"
	"testing"
	"time"
)

// A palette is chosen when a room is created and can never change, because
// every pixel ever stored in that room is an index into it. That makes a broken
// entry permanent for the room that picked it, so the checks here are on all of
// them rather than on the default.

// isHexColour reports whether s is a #rrggbb colour. Case is not part of the
// question: CSS and the PNG encoder both accept either, and insisting on one
// would be a house rule dressed up as a correctness check.
func isHexColour(s string) bool {
	if len(s) != 7 || s[0] != '#' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

func TestEveryPaletteIsWellFormed(t *testing.T) {
	if len(palettes) == 0 {
		t.Fatal("there are no palettes at all, so no room can be created")
	}
	for key, p := range palettes {
		t.Run(key, func(t *testing.T) {
			if p.Key != key {
				t.Errorf("the palette filed under %q calls itself %q; the UI posts back the key it was given", key, p.Key)
			}
			if strings.TrimSpace(p.Name) == "" || strings.TrimSpace(p.Note) == "" {
				t.Errorf("palette %q has name %q and note %q, want both filled in for the creation form",
					key, p.Name, p.Note)
			}
			if len(p.Colors) < 2 {
				t.Fatalf("palette %q has %d colours; index 0 is the background, so anything less than two is a canvas nobody can paint on",
					key, len(p.Colors))
			}
			if len(p.Colors) > 256 {
				t.Fatalf("palette %q has %d colours and a pixel is one byte", key, len(p.Colors))
			}
			seen := make(map[string]int, len(p.Colors))
			for i, hex := range p.Colors {
				if !isHexColour(hex) {
					t.Errorf("palette %q index %d is %q, want a #rrggbb colour: the PNG and GIF exports parse these",
						key, i, hex)
				}
				// Compared case-insensitively because two entries that differ
				// only in case are the same colour on screen: one swatch would
				// be unreachable, and the stats histogram - which is keyed by
				// the colour string - would show it as a separate slice.
				lower := strings.ToLower(hex)
				if first, dup := seen[lower]; dup {
					t.Errorf("palette %q index %d is %q, which is already index %d; one of the two swatches paints something the other already does",
						key, i, hex, first)
				}
				seen[lower] = i
			}
		})
	}
}

// TestEveryPaletteIndexIsPaintable is the link between a palette and the canvas
// that uses it: every index the palette declares has to be placeable, and the
// one past the end must not be.
func TestEveryPaletteIndexIsPaintable(t *testing.T) {
	for key, p := range palettes {
		t.Run(key, func(t *testing.T) {
			c := New(len(p.Colors)+1, 1, p.Colors, 0)
			for i := range p.Colors {
				if i == 0 {
					continue // the background is already there, so painting it is a no-op
				}
				if _, err := c.Place(i, 0, uint8(i), "u", time.Now()); err != nil {
					t.Errorf("colour %d of palette %q was refused: %v", i, key, err)
				}
			}
			if _, err := c.Place(0, 0, uint8(len(p.Colors)), "u", time.Now()); err == nil {
				t.Errorf("colour %d was accepted on palette %q, which has %d colours",
					len(p.Colors), key, len(p.Colors))
			}
		})
	}
}

// TestPaletteForAndNormaliseAgreeOnTheFallback covers the lookup a room does at
// every boot. The key comes out of the database and may name a palette that has
// since been removed, or may be whatever a client posted; either way the room
// has to open. The two functions have to fall back to the same place, or a room
// would be created under one palette and rendered with another.
func TestPaletteForAndNormaliseAgreeOnTheFallback(t *testing.T) {
	cases := []struct {
		name string
		key  string
		want string // the key whose colours are expected
	}{
		{name: "the default", key: "classic", want: "classic"},
		{name: "another real palette", key: "neon", want: "neon"},
		{name: "the smallest real palette", key: "gameboy", want: "gameboy"},
		{name: "empty", key: "", want: DefaultPaletteKey},
		{name: "unknown", key: "chartreuse", want: DefaultPaletteKey},
		// Keys are matched exactly, so these are all misses that fall back
		// rather than being cleaned up. That is a deliberate choice and this
		// pins it: silently accepting " Neon " would mean two spellings of one
		// palette reaching the database.
		{name: "mixed case", key: "Classic", want: DefaultPaletteKey},
		{name: "upper case", key: "NEON", want: DefaultPaletteKey},
		{name: "leading and trailing space", key: " classic ", want: DefaultPaletteKey},
		{name: "a newline", key: "classic\n", want: DefaultPaletteKey},
		{name: "a tab", key: "\tneon", want: DefaultPaletteKey},
		{name: "sql-ish nonsense", key: "'; drop table rooms; --", want: DefaultPaletteKey},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantColours := palettes[tc.want].Colors

			got := PaletteFor(tc.key)
			if len(got) != len(wantColours) || (len(got) > 0 && got[0] != wantColours[0]) {
				t.Errorf("PaletteFor(%q) returned %d colours starting %q, want the %d of %q starting %q",
					tc.key, len(got), firstColour(got), len(wantColours), tc.want, firstColour(wantColours))
			}
			if key := NormalisePaletteKey(tc.key); key != tc.want {
				t.Errorf("NormalisePaletteKey(%q) = %q, want %q", tc.key, key, tc.want)
			}
			if exists := PaletteExists(tc.key); exists != (tc.key == tc.want) {
				t.Errorf("PaletteExists(%q) = %v, want %v", tc.key, exists, tc.key == tc.want)
			}
			// The composition is what a room actually does: normalise the key on
			// the way in, look the colours up on the way out. Both routes have
			// to reach the same palette.
			if a, b := PaletteFor(tc.key), PaletteFor(NormalisePaletteKey(tc.key)); len(a) != len(b) || firstColour(a) != firstColour(b) {
				t.Errorf("PaletteFor(%q) and PaletteFor(NormalisePaletteKey(%q)) disagree: a room would be stored under one palette and drawn with the other",
					tc.key, tc.key)
			}
		})
	}
}

func firstColour(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}

// TestPalettesIsStableAndPutsTheDefaultFirst covers the creation form. The list
// is built from a map, whose iteration order is deliberately random in Go, so
// without the sort the cards would rearrange themselves on every request and
// the default would wander.
func TestPalettesIsStableAndPutsTheDefaultFirst(t *testing.T) {
	listed := Palettes()
	if len(listed) != len(palettes) {
		t.Fatalf("Palettes lists %d of %d palettes", len(listed), len(palettes))
	}
	if listed[0].Key != DefaultPaletteKey {
		t.Errorf("the first card is %q, want the default %q", listed[0].Key, DefaultPaletteKey)
	}

	// Ten calls, because one call cannot show that a map's iteration order has
	// been dealt with.
	for i := 0; i < 10; i++ {
		got := Palettes()
		if len(got) != len(listed) {
			t.Fatalf("call %d lists %d palettes, want %d", i, len(got), len(listed))
		}
		for j := range got {
			if got[j].Key != listed[j].Key {
				t.Fatalf("call %d has %q at position %d, where the first call had %q",
					i, got[j].Key, j, listed[j].Key)
			}
		}
	}

	// Everything after the default is ordered by the name people see.
	rest := listed[1:]
	names := make([]string, len(rest))
	for i, p := range rest {
		names[i] = p.Name
	}
	if !sort.StringsAreSorted(names) {
		t.Errorf("the palettes after the default are listed %v, want them in name order", names)
	}

	seen := map[string]bool{}
	for _, p := range listed {
		if seen[p.Key] {
			t.Errorf("palette %q is listed twice", p.Key)
		}
		seen[p.Key] = true
		if !PaletteExists(p.Key) {
			t.Errorf("the form offers palette %q, which no room can be created with", p.Key)
		}
		if len(p.Colors) != len(PaletteFor(p.Key)) {
			t.Errorf("the card for %q shows %d colours and the room would get %d",
				p.Key, len(p.Colors), len(PaletteFor(p.Key)))
		}
	}
	for key := range palettes {
		if !seen[key] {
			t.Errorf("palette %q exists but is not offered on the creation form", key)
		}
	}
}

// TestDefaultPaletteIsTheOneEverythingElseAssumes ties the package-level
// shorthand to the map. A lot of code takes Palette without a lookup, and if it
// ever stopped being the palette a room gets by default, those two paths would
// disagree about what colour index 7 is.
func TestDefaultPaletteIsTheOneEverythingElseAssumes(t *testing.T) {
	if !PaletteExists(DefaultPaletteKey) {
		t.Fatalf("the default palette key %q is not a palette", DefaultPaletteKey)
	}
	byKey := PaletteFor(DefaultPaletteKey)
	if len(Palette) != len(byKey) {
		t.Fatalf("Palette has %d colours and PaletteFor(%q) has %d", len(Palette), DefaultPaletteKey, len(byKey))
	}
	for i := range Palette {
		if Palette[i] != byKey[i] {
			t.Errorf("index %d is %q in Palette and %q in the default palette", i, Palette[i], byKey[i])
		}
	}
}
