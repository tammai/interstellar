package store

import "testing"

// Migration 011 must neither re-read nor skip anything on upgrade: topics that were
// being classified inherit the instance-wide mark, and topics that were not --
// paused, or never defined -- start empty, which is what lets them catch up.
func TestTopicCursorMigrationSeedsFromTheOldWatermark(t *testing.T) {
	path := t.TempDir() + "/cursor.db"
	old := openAtVersion(t, path, 10)

	for _, q := range []string{
		`INSERT INTO settings (key, value) VALUES ('last_classified_fetched_at', '2026-09-30 09:30:00')`,
		`INSERT INTO topics (id, title, definition, active) VALUES (1, 'Live', 'Agents.', 1)`,
		`INSERT INTO topics (id, title, definition, active) VALUES (2, 'Paused', 'Weights.', 0)`,
		`INSERT INTO topics (id, title, definition, active) VALUES (3, 'Undefined', '', 1)`,
	} {
		if _, err := old.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	old.Close()

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	want := map[int64]string{1: "2026-09-30 09:30:00", 2: "", 3: ""}
	for id, cursor := range want {
		got, err := db.Topic(id)
		if err != nil {
			t.Fatalf("Topic %d: %v", id, err)
		}
		if got.ClassifiedThrough != cursor {
			t.Errorf("topic %d cursor = %q, want %q", id, got.ClassifiedThrough, cursor)
		}
	}
}
