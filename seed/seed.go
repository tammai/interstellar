// Package seed holds the topics and sources a new instance starts with.
//
// default.json is embedded, so a fresh container needs nothing mounted to produce a
// sky on day one. An instance can override it with DATA_DIR/seed.json. Either file
// is read only when a database is new -- see store.BootstrapWith -- so editing one
// changes nothing on an instance that has already started. See README.md here for
// the format and the authoring workflow.
package seed

import (
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

//go:embed default.json
var defaultSeed []byte

// FileName is the override's name inside DATA_DIR.
const FileName = "seed.json"

// Default returns the embedded seed.
func Default() []byte { return defaultSeed }

// Resolve returns the seed a new database in dataDir should get: the override if
// one exists there, else the embedded default. origin names which, for the log.
//
// A missing override is the ordinary case. An override that exists and cannot be
// read is an error rather than a fallback: silently seeding the defaults would
// hand the owner sixty-eight sources they went out of their way to replace.
func Resolve(dataDir string) (data []byte, origin string, err error) {
	path := filepath.Join(dataDir, FileName)
	data, err = os.ReadFile(path)
	switch {
	case err == nil:
		return data, path, nil
	case errors.Is(err, fs.ErrNotExist):
		return defaultSeed, "embedded default", nil
	default:
		return nil, path, fmt.Errorf("read seed %s: %w", path, err)
	}
}
