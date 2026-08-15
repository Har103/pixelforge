package httpapi

import (
	"io/fs"

	"github.com/Har103/pixelforge/web"
)

// testFS serves the real embedded front end, so the page tests exercise the
// markup that actually ships rather than a fixture that can drift from it.
func testFS() fs.FS { return web.FS() }
