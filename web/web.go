// Package web embeds the front end so the compiled binary is the whole
// application: no asset directory to mount, no static file server to configure,
// and a container image that is one file.
package web

import (
	"embed"
	"io/fs"
)

//go:embed *.html assets
var files embed.FS

// FS returns the embedded front end.
func FS() fs.FS { return files }
