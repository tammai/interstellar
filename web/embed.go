// Package web embeds the UI so the binary ships with no external assets.
//
// The assets live in a subdirectory rather than beside this file because
// go:embed cannot embed the directory containing its own source. Anything
// dropped into web/assets is served automatically -- no Go change needed.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:assets
var assets embed.FS

// FS returns the UI filesystem rooted at index.html.
func FS() fs.FS {
	sub, err := fs.Sub(assets, "assets")
	if err != nil {
		// Unreachable: the embed above guarantees assets/ exists at build time.
		panic(err)
	}
	return sub
}
