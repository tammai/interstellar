package seed

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolvePrefersTheOverride(t *testing.T) {
	dir := t.TempDir()
	if _, origin, err := Resolve(dir); err != nil || origin != "embedded default" {
		t.Fatalf("no override: origin=%q err=%v", origin, err)
	}

	want := []byte(`{"version":1,"topics":[],"sources":[]}`)
	if err := os.WriteFile(filepath.Join(dir, FileName), want, 0o600); err != nil {
		t.Fatal(err)
	}
	data, origin, err := Resolve(dir)
	if err != nil || string(data) != string(want) || origin != filepath.Join(dir, FileName) {
		t.Errorf("override: origin=%q err=%v data=%s", origin, err, data)
	}
}

// An override that exists but cannot be read is an error, never a quiet fall back
// to the defaults the owner meant to replace.
func TestResolveRefusesAnUnreadableOverride(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, FileName), 0o700); err != nil { // a directory cannot be read as a file
		t.Fatal(err)
	}
	if _, _, err := Resolve(dir); err == nil {
		t.Error("an unreadable override fell back to the defaults")
	}
}
