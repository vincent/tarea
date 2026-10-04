// Package webui embeds the compiled Svelte panel (built by `make web` into dist/).
package webui

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// FS returns the panel's static files rooted at the build output, or nil when
// the panel has not been built (only a .gitkeep is present).
func FS() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return nil
	}
	if _, err = fs.Stat(sub, "index.html"); err != nil {
		return nil
	}
	return sub
}
