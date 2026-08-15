package room

import (
	"strings"
	"testing"

	"github.com/Har103/pixelforge/internal/canvas"
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
