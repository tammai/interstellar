// Package web embeds the UI so the binary ships with no external assets.
//
// The assets live in a subdirectory rather than beside this file because
// go:embed cannot embed the directory containing its own source. Anything
// dropped into web/assets is served automatically -- no Go change needed.
package web

import (
	"embed"
	"io/fs"
	"os"
)

//go:embed all:assets
var assets embed.FS

// DevDirEnv names an environment variable that, when set to a directory, serves
// the UI from disk instead of the embedded copy.
//
// This exists because go:embed bakes assets in at build time: without it, every
// CSS tweak needs a rebuild and a restart, and it is very easy to spend an hour
// debugging a stylesheet the running binary has never seen. Production ignores
// this -- the variable is unset, and the embedded copy is what ships.
const DevDirEnv = "INTERSTELLAR_WEB_DIR"

// FS returns the UI filesystem rooted at index.html.
func FS() fs.FS {
	if dir := os.Getenv(DevDirEnv); dir != "" {
		return os.DirFS(dir)
	}
	sub, err := fs.Sub(assets, "assets")
	if err != nil {
		// Unreachable: the embed above guarantees assets/ exists at build time.
		panic(err)
	}
	return sub
}

// Embedded reports whether the UI is being served from the binary. Logged at
// startup so "my change did nothing" is answerable at a glance.
func Embedded() bool { return os.Getenv(DevDirEnv) == "" }
