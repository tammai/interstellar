package store

import "testing"

// Deleting every topic and source to start clean must stick across a restart.
// Bootstrap used to seed any empty table, so the full default list came back.
func TestBootstrapDoesNotReseedATableEmptiedOnPurpose(t *testing.T) {
	db := openTemp(t)
	if err := db.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if count(t, db, "SELECT count(*) FROM topics") == 0 || count(t, db, "SELECT count(*) FROM sources") == 0 {
		t.Fatal("a fresh database was not seeded")
	}

	for _, q := range []string{"DELETE FROM topics", "DELETE FROM sources"} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := db.Bootstrap(); err != nil {
		t.Fatalf("second Bootstrap: %v", err)
	}
	if n := count(t, db, "SELECT count(*) FROM topics"); n != 0 {
		t.Errorf("%d topics after restart, want 0: the defaults came back", n)
	}
	if n := count(t, db, "SELECT count(*) FROM sources"); n != 0 {
		t.Errorf("%d sources after restart, want 0: the defaults came back", n)
	}
}

// A database bootstrapped before the markers existed is marked by migration 012,
// so an upgrade does not seed into a table its owner had already emptied.
func TestSeedMarkerMigrationMarksExistingDatabases(t *testing.T) {
	path := t.TempDir() + "/markers.db"
	old := openAtVersion(t, path, 11)
	if _, err := old.Exec(`INSERT INTO settings (key, value) VALUES ('max_calls_per_run', '400')`); err != nil {
		t.Fatal(err)
	}
	old.Close()

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if n := count(t, db, "SELECT count(*) FROM topics"); n != 0 {
		t.Errorf("%d topics seeded into an existing, empty database", n)
	}
	if n := count(t, db, "SELECT count(*) FROM sources"); n != 0 {
		t.Errorf("%d sources seeded into an existing, empty database", n)
	}
}
