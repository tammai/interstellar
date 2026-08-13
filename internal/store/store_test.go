package store

import (
	"path/filepath"
	"testing"
)

func openTemp(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestOpenMigratesEmptyDatabase(t *testing.T) {
	db := openTemp(t)

	got, err := db.Version()
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	want := len(mustLoadMigrations(t))
	if got != want {
		t.Errorf("schema version = %d, want %d", got, want)
	}

	// Every table the pipeline depends on should exist.
	for _, table := range []string{
		"topics", "sources", "items", "item_sources",
		"matches", "signals", "signal_items", "llm_calls", "settings",
		"topic_stats",
	} {
		var n int
		err := db.QueryRow(
			"SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?", table,
		).Scan(&n)
		if err != nil {
			t.Fatalf("look up %s: %v", table, err)
		}
		if n != 1 {
			t.Errorf("table %s missing", table)
		}
	}
}

// Reopening must be a no-op: the migration runner has to be safe on every boot,
// not just the first.
func TestMigrateIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	first, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	v1, _ := first.Version()
	first.Close()

	second, err := Open(path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer second.Close()
	v2, _ := second.Version()

	if v1 != v2 {
		t.Errorf("version changed across reopen: %d then %d", v1, v2)
	}
}

func TestForeignKeysEnforced(t *testing.T) {
	db := openTemp(t)

	// items.source_id references sources(id); source 999 does not exist.
	_, err := db.Exec(
		`INSERT INTO items (source_id, canonical_url, title) VALUES (999, 'https://x.test/a', 'a')`,
	)
	if err == nil {
		t.Fatal("insert with dangling source_id succeeded; foreign_keys pragma is not on")
	}
}

func TestBootstrapSeedsAndIsIdempotent(t *testing.T) {
	db := openTemp(t)

	if err := db.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	settings, err := db.Settings()
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	for _, key := range []string{
		KeyProviders, KeyClassifierModel, KeySynthesizerModel,
		KeyScheduleIngest, KeyMaxCallsPerRun, KeyHNMinPoints,
	} {
		if settings[key] == "" {
			t.Errorf("setting %q not seeded", key)
		}
	}

	topics := count(t, db, "SELECT count(*) FROM topics")
	sources := count(t, db, "SELECT count(*) FROM sources")
	if topics != len(placeholderTopics) {
		t.Errorf("seeded %d topics, want %d", topics, len(placeholderTopics))
	}
	if sources != len(seedSources) {
		t.Errorf("seeded %d sources, want %d", sources, len(seedSources))
	}

	// Seeded topics carry the prototype's constellation positions, so a fresh
	// instance renders the sky the approved mock promised.
	var nullPositions int
	err = db.QueryRow("SELECT count(*) FROM topics WHERE pos_x IS NULL OR pos_y IS NULL").Scan(&nullPositions)
	if err != nil {
		t.Fatalf("count positions: %v", err)
	}
	if nullPositions != 0 {
		t.Errorf("%d seeded topics have no constellation position", nullPositions)
	}

	if err := db.Bootstrap(); err != nil {
		t.Fatalf("second Bootstrap: %v", err)
	}
	if got := count(t, db, "SELECT count(*) FROM topics"); got != topics {
		t.Errorf("second Bootstrap duplicated topics: %d then %d", topics, got)
	}
	if got := count(t, db, "SELECT count(*) FROM sources"); got != sources {
		t.Errorf("second Bootstrap duplicated sources: %d then %d", sources, got)
	}
}

// A setting the owner changed must survive a reboot -- seeding fills gaps, it
// does not reset.
func TestBootstrapPreservesOwnerSettings(t *testing.T) {
	db := openTemp(t)
	if err := db.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if err := db.SetSetting(KeyClassifierModel, "ollama/llama3"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if err := db.Bootstrap(); err != nil {
		t.Fatalf("second Bootstrap: %v", err)
	}

	got, _, err := db.Setting(KeyClassifierModel)
	if err != nil {
		t.Fatalf("Setting: %v", err)
	}
	if got != "ollama/llama3" {
		t.Errorf("classifier model = %q, want the owner's value to survive", got)
	}
}

// Deleting a seeded source must stick: bootstrap only fills an empty table.
func TestBootstrapDoesNotResurrectDeletedSources(t *testing.T) {
	db := openTemp(t)
	if err := db.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if _, err := db.Exec("DELETE FROM sources WHERE id = (SELECT min(id) FROM sources)"); err != nil {
		t.Fatalf("delete source: %v", err)
	}
	before := count(t, db, "SELECT count(*) FROM sources")

	if err := db.Bootstrap(); err != nil {
		t.Fatalf("second Bootstrap: %v", err)
	}
	if after := count(t, db, "SELECT count(*) FROM sources"); after != before {
		t.Errorf("source count %d -> %d; bootstrap resurrected a deleted source", before, after)
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	db := openTemp(t)

	if _, ok, _ := db.Setting("nope"); ok {
		t.Error("missing key reported as present")
	}

	if err := db.SetSetting("k", "v1"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if err := db.SetSetting("k", "v2"); err != nil {
		t.Fatalf("SetSetting overwrite: %v", err)
	}

	v, ok, err := db.Setting("k")
	if err != nil || !ok {
		t.Fatalf("Setting: v=%q ok=%v err=%v", v, ok, err)
	}
	if v != "v2" {
		t.Errorf("value = %q, want v2", v)
	}
}

func mustLoadMigrations(t *testing.T) []migration {
	t.Helper()
	m, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	return m
}

func count(t *testing.T, db *DB, query string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}
