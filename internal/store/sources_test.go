package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tammai/interstellar/internal/sources"
)

func addSource(t *testing.T, db *DB, kind, name string) int64 {
	t.Helper()
	res, err := db.Exec(
		`INSERT INTO sources (kind, name, config, active) VALUES (?, ?, '{}', 1)`, kind, name,
	)
	if err != nil {
		t.Fatalf("insert source %s: %v", name, err)
	}
	id, _ := res.LastInsertId()
	return id
}

func addItem(t *testing.T, db *DB, sourceID int64, url string) int64 {
	t.Helper()
	res, err := db.Exec(
		`INSERT INTO items (source_id, canonical_url, title) VALUES (?, ?, ?)`, sourceID, url, url,
	)
	if err != nil {
		t.Fatalf("insert item %s: %v", url, err)
	}
	id, _ := res.LastInsertId()
	if _, err := db.Exec(
		`INSERT INTO item_sources (item_id, source_id) VALUES (?, ?)`, id, sourceID,
	); err != nil {
		t.Fatalf("attribute item %s: %v", url, err)
	}
	return id
}

// A YouTube source is the one kind whose items carry transcript rows, and
// item_transcripts references items with no cascade. Deleting the items without
// clearing those rows fails the foreign key, so the whole delete rolls back and
// the source stays on the list -- which is how this shipped: every other kind
// deleted cleanly and YouTube channels could not be removed at all.
func TestDeleteSourceRemovesTranscripts(t *testing.T) {
	db := openTemp(t)

	src := addSource(t, db, "youtube_rss", "a channel")
	item := addItem(t, db, src, "https://youtube.test/watch?v=1")
	if _, err := db.Exec(
		`INSERT INTO item_transcripts (item_id, status, text) VALUES (?, 'ok', 'hello')`, item,
	); err != nil {
		t.Fatalf("insert transcript: %v", err)
	}
	// A failure row is written for videos with captions disabled, and those are
	// the common case -- they must not block the delete either.
	failed := addItem(t, db, src, "https://youtube.test/watch?v=2")
	if _, err := db.Exec(
		`INSERT INTO item_transcripts (item_id, status, error) VALUES (?, 'none', '')`, failed,
	); err != nil {
		t.Fatalf("insert failure row: %v", err)
	}

	if err := db.DeleteSource(src); err != nil {
		t.Fatalf("DeleteSource: %v", err)
	}

	if n := count(t, db, "SELECT count(*) FROM sources"); n != 0 {
		t.Errorf("sources left = %d, want 0", n)
	}
	if n := count(t, db, "SELECT count(*) FROM items"); n != 0 {
		t.Errorf("items left = %d, want 0", n)
	}
	if n := count(t, db, "SELECT count(*) FROM item_transcripts"); n != 0 {
		t.Errorf("transcripts left = %d, want 0", n)
	}
}

// An item another source also carried survives the delete, transcript and all:
// only the attribution goes. Deleting the transcript of a surviving item would
// make the next run refetch it, and for a video with no captions that is a
// permanent answer being thrown away.
func TestDeleteSourceKeepsItemsCarriedElsewhere(t *testing.T) {
	db := openTemp(t)

	gone := addSource(t, db, "youtube_rss", "leaving")
	kept := addSource(t, db, "rss", "staying")
	item := addItem(t, db, gone, "https://youtube.test/watch?v=3")
	if _, err := db.Exec(
		`INSERT INTO item_sources (item_id, source_id) VALUES (?, ?)`, item, kept,
	); err != nil {
		t.Fatalf("second attribution: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO item_transcripts (item_id, status) VALUES (?, 'none')`, item,
	); err != nil {
		t.Fatalf("insert transcript: %v", err)
	}

	if err := db.DeleteSource(gone); err != nil {
		t.Fatalf("DeleteSource: %v", err)
	}

	if n := count(t, db, "SELECT count(*) FROM items"); n != 1 {
		t.Errorf("items left = %d, want 1", n)
	}
	if n := count(t, db, "SELECT count(*) FROM item_transcripts"); n != 1 {
		t.Errorf("transcripts left = %d, want 1", n)
	}
	var owner int64
	if err := db.QueryRow(`SELECT source_id FROM items WHERE id = ?`, item).Scan(&owner); err != nil {
		t.Fatalf("read item owner: %v", err)
	}
	if owner != kept {
		t.Errorf("item repointed to source %d, want %d", owner, kept)
	}
	if n := count(t, db, "SELECT count(*) FROM item_sources"); n != 1 {
		t.Errorf("attributions left = %d, want 1", n)
	}
}

// migrateTo builds a database stopped at a given schema version, so a data
// migration can be tested against the shape that actually existed before it.
func migrateTo(t *testing.T, path string, version int) *sql.DB {
	t.Helper()
	sqlDB, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for _, m := range mustLoadMigrations(t) {
		if m.version > version {
			break
		}
		if _, err := sqlDB.Exec(m.sql); err != nil {
			t.Fatalf("migration %d: %v", m.version, err)
		}
	}
	if _, err := sqlDB.Exec(fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
	return sqlDB
}

// Migration 007 collapses the per-keyword Hacker News sources into one, and the
// reason is signals.source_count: it counts DISTINCT source_id, so one story
// matching three keyword rows presented as three outlets and went gold. This
// pins both halves -- the keywords survive with their own thresholds, and the
// story ends up attributed to Hacker News exactly once.
func TestMigration007CollapsesHackerNewsKeywords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old := migrateTo(t, path, 6)

	for _, s := range []struct {
		name, config string
	}{
		{"HN · LLM", `{"query":"LLM","min_points":100}`},
		{"HN · MCP", `{"query":"MCP","min_points":25}`},
		{"HN · evals", `{"query":"eval"}`},
	} {
		if _, err := old.Exec(
			`INSERT INTO sources (kind, name, config, active) VALUES ('hn_search', ?, ?, 1)`,
			s.name, s.config,
		); err != nil {
			t.Fatalf("seed %s: %v", s.name, err)
		}
	}
	// One story that matched all three keywords -- the case that inflated the count.
	if _, err := old.Exec(
		`INSERT INTO items (id, source_id, canonical_url, title) VALUES (1, 1, 'https://x.test/a', 'a')`,
	); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	for _, sid := range []int{1, 2, 3} {
		if _, err := old.Exec(
			`INSERT INTO item_sources (item_id, source_id, detail) VALUES (1, ?, '412 points')`, sid,
		); err != nil {
			t.Fatalf("seed attribution %d: %v", sid, err)
		}
	}
	// An item whose "first seen on" is a row that is about to disappear.
	if _, err := old.Exec(
		`INSERT INTO items (id, source_id, canonical_url, title) VALUES (2, 3, 'https://x.test/b', 'b')`,
	); err != nil {
		t.Fatalf("seed second item: %v", err)
	}
	old.Close()

	db, err := Open(path) // applies 007
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if n := count(t, db, "SELECT count(*) FROM sources WHERE kind = 'hn_search'"); n != 1 {
		t.Fatalf("hn sources after migration = %d, want 1", n)
	}

	var name, cfg string
	if err := db.QueryRow(
		`SELECT name, config FROM sources WHERE kind = 'hn_search'`,
	).Scan(&name, &cfg); err != nil {
		t.Fatalf("read merged source: %v", err)
	}
	if name != "Hacker News" {
		t.Errorf("name = %q, want %q", name, "Hacker News")
	}

	var c sources.HNConfig
	if err := json.Unmarshal([]byte(cfg), &c); err != nil {
		t.Fatalf("merged config does not parse: %v (%s)", err, cfg)
	}
	got := c.Normalized()
	if len(got) != 3 {
		t.Fatalf("merged %d keywords, want 3: %s", len(got), cfg)
	}
	// Order follows the old rows' ids, so the list reads the way it read before.
	want := []sources.HNQuery{
		{Query: "LLM", MinPoints: 100},
		{Query: "MCP", MinPoints: 25},
		{Query: "eval", MinPoints: 0}, // no threshold of its own, and none invented
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("keyword %d = %+v, want %+v", i, got[i], w)
		}
	}

	// The point of the whole exercise.
	if n := count(t, db, "SELECT count(*) FROM item_sources WHERE item_id = 1"); n != 1 {
		t.Errorf("story attributed %d times, want 1 -- Hacker News is one outlet", n)
	}
	// Nothing is left pointing at a source that no longer exists.
	if n := count(t, db,
		`SELECT count(*) FROM items i LEFT JOIN sources s ON s.id = i.source_id WHERE s.id IS NULL`,
	); n != 0 {
		t.Errorf("%d items point at a deleted source", n)
	}
	if n := count(t, db, "SELECT count(*) FROM items"); n != 2 {
		t.Errorf("items = %d, want 2 -- the collapse must not drop stories", n)
	}
}

// The seeded config has to survive the parser it is written for. A typo here
// would ship an instance whose only news source fetches nothing.
func TestSeededHackerNewsConfigParses(t *testing.T) {
	db := openTemp(t)
	if err := db.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	var cfg string
	if err := db.QueryRow(
		`SELECT config FROM sources WHERE kind = 'hn_search'`,
	).Scan(&cfg); err != nil {
		t.Fatalf("read seeded config: %v", err)
	}
	var c sources.HNConfig
	if err := json.Unmarshal([]byte(cfg), &c); err != nil {
		t.Fatalf("seeded hn config does not parse: %v", err)
	}
	queries := c.Normalized()
	if len(queries) == 0 {
		t.Fatal("the seeded Hacker News source watches no keywords")
	}
	// The two bars are opposite on purpose and the pairing is the design: a low
	// keyword floor catches model releases that never trend, a high firehose bar
	// keeps the keyword-less pass affordable. Every keyword must sit below the
	// firehose, or seeding them the wrong way round quietly undoes both halves.
	if c.FirehoseMinPoints == 0 {
		t.Error("the seed switches the firehose off")
	}
	for _, q := range queries {
		if q.MinPoints >= c.FirehoseMinPoints {
			t.Errorf("keyword %q needs %d points, not below the firehose's %d", q.Query, q.MinPoints, c.FirehoseMinPoints)
		}
		// Phrases need every word present in Algolia, so a multi-word keyword has
		// to be a real phrase people write, not a description of a concept.
		if len(strings.Fields(q.Query)) > 2 {
			t.Errorf("keyword %q is a description, not a search term", q.Query)
		}
	}
	if n := count(t, db, "SELECT count(*) FROM sources WHERE kind = 'hn_search'"); n != 1 {
		t.Errorf("seeded %d hn sources, want 1", n)
	}
}

// Claims hang off items with no cascade, exactly as transcripts do, and a missed
// clause in DeleteSource does not leave a stale row -- it makes the source
// undeletable, which is how the transcript case presented. Pinned in both
// directions: an orphaned item's claims go, and an item another source still
// carries keeps its claims along with the item.
func TestDeleteSourceRemovesClaims(t *testing.T) {
	db := openTemp(t)

	gone := addSource(t, db, "rss", "leaving")
	kept := addSource(t, db, "rss", "staying")

	var entity int64
	if err := db.QueryRow(
		`INSERT INTO entities (name, name_norm, layer) VALUES ('Cursor', 'cursor', 'coding_agents')
		 RETURNING id`,
	).Scan(&entity); err != nil {
		t.Fatalf("insert entity: %v", err)
	}

	orphan := addItem(t, db, gone, "https://example.test/orphan")
	shared := addItem(t, db, gone, "https://example.test/shared")
	if _, err := db.Exec(
		`INSERT INTO item_sources (item_id, source_id) VALUES (?, ?)`, shared, kept,
	); err != nil {
		t.Fatalf("second attribution: %v", err)
	}

	for _, item := range []int64{orphan, shared} {
		if _, err := db.Exec(
			`INSERT INTO claims (entity_id, item_id, change_type, source_type, summary, url, hash)
			 VALUES (?, ?, 'launch', 'independent', 's', 'u', ?)`,
			entity, item, fmt.Sprintf("h%d", item),
		); err != nil {
			t.Fatalf("insert claim: %v", err)
		}
	}

	if err := db.DeleteSource(gone); err != nil {
		t.Fatalf("DeleteSource: %v", err)
	}

	// The shared item survives, so its claim must too: the claim is a permanent
	// answer about an item that still exists, and dropping it would lose history
	// the entity is the whole point of keeping.
	if n := count(t, db, "SELECT count(*) FROM claims"); n != 1 {
		t.Errorf("claims left = %d, want 1", n)
	}
	if n := count(t, db, "SELECT count(*) FROM claims WHERE item_id = "+fmt.Sprint(shared)); n != 1 {
		t.Errorf("shared item's claim = %d, want 1", n)
	}
	// The entity itself is untouched by a source going away.
	if n := count(t, db, "SELECT count(*) FROM entities"); n != 1 {
		t.Errorf("entities left = %d, want 1", n)
	}
}
