package canvas

import "sort"

// A palette is chosen once when a room is created and never changes, which is
// what lets a cell be a single byte instead of a colour. Index 0 is always the
// background.
//
// Adding a palette here is safe at any time; removing one is not, because
// existing rooms reference it by name. Rooms with an unknown palette fall back
// to Classic rather than failing to load.

// PaletteInfo describes a palette for the room-creation UI.
type PaletteInfo struct {
	Key    string   `json:"key"`
	Name   string   `json:"name"`
	Note   string   `json:"note"`
	Colors []string `json:"colors"`
}

// DefaultPaletteKey is used when a room does not name one, or names one that no
// longer exists.
const DefaultPaletteKey = "classic"

var palettes = map[string]PaletteInfo{
	"classic": {
		Key:  "classic",
		Name: "Classic",
		Note: "Twenty colours with room for shading. The safe choice.",
		Colors: []string{
			"#12141c", "#ffffff", "#b8c1d9", "#6b7899", "#3a4361", "#000000",
			"#ff4d6d", "#d90429", "#8b1e3f", "#ff9f1c", "#ffd60a", "#8ac926",
			"#2a9d8f", "#00b4d8", "#3a86ff", "#5f27cd", "#c77dff", "#ff70a6",
			"#7f5539", "#c98f5f",
		},
	},
	"neon": {
		Key:  "neon",
		Name: "Neon",
		Note: "High contrast on near-black. Reads well on a stream.",
		Colors: []string{
			"#05060a", "#ffffff", "#00fff0", "#00d4ff", "#0066ff", "#7b2fff",
			"#c400ff", "#ff00c8", "#ff0066", "#ff2d00", "#ff8a00", "#ffd000",
			"#c6ff00", "#4dff00", "#00ff85", "#1b2233", "#39415c", "#5c6785",
			"#8f9ab8", "#c9d1e6",
		},
	},
	"pastel": {
		Key:  "pastel",
		Name: "Pastel",
		Note: "Soft and low-stakes. Nothing clashes, so group art stays calm.",
		Colors: []string{
			"#fdf6f0", "#ffffff", "#ffd6e0", "#ffb7c5", "#f4978e", "#f8ad9d",
			"#fbc4ab", "#ffdab9", "#fff1b6", "#e2f0cb", "#b5ead7", "#a0e7e5",
			"#a8dadc", "#bde0fe", "#a2c7ff", "#cdb4db", "#e0bbE4", "#d8bfd8",
			"#c9ada7", "#4a4453",
		},
	},
	"gameboy": {
		Key:  "gameboy",
		Name: "Game Boy",
		Note: "Four greens. Constraint is the point — try lettering in it.",
		Colors: []string{
			"#0f380f", "#306230", "#8bac0f", "#9bbc0f",
		},
	},
	"mono": {
		Key:  "mono",
		Name: "Monochrome",
		Note: "Nine greys. Good for line art, portraits and dithering.",
		Colors: []string{
			"#0a0b10", "#ffffff", "#dcdfe8", "#b3b9c9", "#8a91a5", "#616981",
			"#3f4658", "#262b38", "#14171f",
		},
	},
}

// Palette is the Classic palette, kept as a package-level value because it is
// the default and a lot of code wants it without a lookup.
var Palette = palettes[DefaultPaletteKey].Colors

// PaletteFor returns the colours for a palette key, falling back to Classic.
func PaletteFor(key string) []string {
	if p, ok := palettes[key]; ok {
		return p.Colors
	}
	return palettes[DefaultPaletteKey].Colors
}

// PaletteExists reports whether a key names a real palette.
func PaletteExists(key string) bool {
	_, ok := palettes[key]
	return ok
}

// NormalisePaletteKey maps an unknown or empty key onto the default.
func NormalisePaletteKey(key string) string {
	if PaletteExists(key) {
		return key
	}
	return DefaultPaletteKey
}

// Palettes lists every palette, ordered so the UI is stable across restarts.
func Palettes() []PaletteInfo {
	out := make([]PaletteInfo, 0, len(palettes))
	for _, p := range palettes {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		// Classic first, then by name, so the default is always the first card.
		if out[i].Key == DefaultPaletteKey {
			return true
		}
		if out[j].Key == DefaultPaletteKey {
			return false
		}
		return out[i].Name < out[j].Name
	})
	return out
}
