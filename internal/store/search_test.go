package store

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// The single most important property of BuildMatch: no input from a person can
// produce a MATCH syntax error. FTS5's grammar treats a pile of ordinary
// punctuation as operators, so this is exercised against real prose rather than
// against a curated list of nice words.
func TestBuildMatchNeverProducesASyntaxError(t *testing.T) {
	db := openTemp(t)
	seedSearchCorpus(t, db)

	inputs := []string{
		`what's happening with agents?`,
		`AND OR NOT NEAR`,                 // bare operators
		`"unterminated quote`,             // unbalanced quote
		`agents AND (harness OR tooling)`, // parens
		`title: something`,                // column filter syntax
		`-excluded +required`,             // prefix operators
		`NEAR(a b, 3)`,                    // NEAR form
		`^anchored`,                       // initial-token operator
		`wildcard*`,                       // prefix operator
		`c++ / rust`,                      // punctuation soup
		`100% of "the" things`,            // quotes plus percent
		`Tiếng Việt có dấu`,               // diacritics
		`emoji ✦ and — dashes`,            // non-letter runes
		`   `,                             // whitespace only
		``,                                // empty
		`a`,                               // single char, dropped
		strings.Repeat("agents ", 200),    // long, repetitive
		`'; DROP TABLE signals; --`,       // the classic, which is not how this could break
	}

	for _, in := range inputs {
		// The expression itself must be usable, or empty.
		match := BuildMatch(in)
		if match == "" {
			continue // filter-only; nothing to execute
		}
		var n int
		err := db.QueryRow(`SELECT count(*) FROM signals_fts WHERE signals_fts MATCH ?`, match).Scan(&n)
		if err != nil {
			t.Errorf("input %q built %q which failed to execute: %v", in, match, err)
		}

		// And the full path must work too.
		if _, err := db.Search(SearchQuery{Text: in}); err != nil {
			t.Errorf("Search(%q): %v", in, err)
		}
	}
}

// SQL injection is not the live risk here -- everything is a bound parameter --
// but the table must still be there afterwards, and this pins that.
func TestSearchLeavesTheSchemaAlone(t *testing.T) {
	db := openTemp(t)
	seedSearchCorpus(t, db)

	if _, err := db.Search(SearchQuery{Text: `'; DROP TABLE signals; --`}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM signals`).Scan(&n); err != nil {
		t.Fatalf("signals table is gone: %v", err)
	}
	if n == 0 {
		t.Error("signals table was emptied")
	}
}

func TestBuildMatchShape(t *testing.T) {
	cases := map[string]string{
		"agent harness": `"agent" OR "harness"`,
		"agent AGENT":   `"agent"`,  // deduped, case-insensitively
		"a agents":      `"agents"`, // single char dropped
		"":              ``,
		"   ?!  ":       ``,
		`say "hi"`:      `"say" OR "hi"`, // quotes stripped by tokenizing
	}
	for in, want := range cases {
		if got := BuildMatch(in); got != want {
			t.Errorf("BuildMatch(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSearchRanksMultiTermMatchesHigher(t *testing.T) {
	db := openTemp(t)
	seedSearchCorpus(t, db)

	hits, err := db.Search(SearchQuery{Text: "agent harness tooling", Kind: "signals"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) < 2 {
		t.Fatalf("got %d hits, want at least 2", len(hits))
	}
	// The signal carrying all three terms must outrank the one carrying one.
	if !strings.Contains(strings.ToLower(hits[0].Title), "harness") {
		t.Errorf("top hit is %q; expected the multi-term match first", hits[0].Title)
	}
	if hits[0].Rank > hits[1].Rank {
		t.Errorf("ranks out of order: %v then %v", hits[0].Rank, hits[1].Rank)
	}
	if hits[0].Snippet == "" {
		t.Error("no snippet on a text match")
	}
}

// Diacritics matter: §9 keeps definitions in Vietnamese and feeds carry Vietnamese
// titles, so a plain-keyboard query has to find the accented text.
func TestSearchIsDiacriticInsensitive(t *testing.T) {
	db := openTemp(t)
	seedSearchCorpus(t, db)

	hits, err := db.Search(SearchQuery{Text: "tieng viet", Kind: "signals"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal(`"tieng viet" found nothing; remove_diacritics is not doing its job`)
	}
	if !strings.Contains(hits[0].Title, "Tiếng") {
		t.Errorf("top hit is %q, want the Vietnamese signal", hits[0].Title)
	}
}

// A query with no searchable text is a filter, not a match-everything.
func TestSearchWithNoTextIsAFilter(t *testing.T) {
	db := openTemp(t)
	seedSearchCorpus(t, db)

	hits, err := db.Search(SearchQuery{Kind: "signals", Limit: 50})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("a filter-only query returned nothing")
	}
	for _, h := range hits {
		if h.Rank != 0 {
			t.Errorf("filter-only hit has rank %v, want 0", h.Rank)
		}
	}
	// Newest first when there is nothing to rank by.
	for i := 1; i < len(hits); i++ {
		if hits[i-1].Date < hits[i].Date {
			t.Errorf("filter-only results not newest-first: %s before %s", hits[i-1].Date, hits[i].Date)
			break
		}
	}
}

func TestSearchFiltersByTopicAndDate(t *testing.T) {
	db := openTemp(t)
	ids := seedSearchCorpus(t, db)

	hits, err := db.Search(SearchQuery{Kind: "signals", TopicIDs: []int64{ids.voice}, Limit: 50})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("topic filter returned nothing")
	}
	for _, h := range hits {
		if h.TopicID != ids.voice {
			t.Errorf("topic filter leaked topic %d", h.TopicID)
		}
	}

	windowed, err := db.Search(SearchQuery{Kind: "signals", From: "2026-08-02", To: "2026-08-02", Limit: 50})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(windowed) == 0 {
		t.Fatal("date window returned nothing")
	}
	for _, h := range windowed {
		if h.Date != "2026-08-02" {
			t.Errorf("date filter leaked %s", h.Date)
		}
	}
}

func TestSearchCoversItemsAndBothCorpora(t *testing.T) {
	db := openTemp(t)
	seedSearchCorpus(t, db)

	items, err := db.Search(SearchQuery{Text: "transcript", Kind: "items"})
	if err != nil {
		t.Fatalf("Search items: %v", err)
	}
	if len(items) == 0 {
		t.Fatal("item search found nothing")
	}
	if items[0].Kind != "item" || items[0].SourceName == "" {
		t.Errorf("item hit missing kind or source: %+v", items[0])
	}

	both, err := db.Search(SearchQuery{Text: "agent transcript", Limit: 50})
	if err != nil {
		t.Fatalf("Search both: %v", err)
	}
	var sawSignal, sawItem bool
	for _, h := range both {
		switch h.Kind {
		case "signal":
			sawSignal = true
		case "item":
			sawItem = true
		}
	}
	if !sawSignal || !sawItem {
		t.Errorf("both-corpora search returned signal=%v item=%v", sawSignal, sawItem)
	}
	// Merged results must be in rank order across the two corpora.
	for i := 1; i < len(both); i++ {
		if both[i-1].Rank > both[i].Rank {
			t.Errorf("merged results out of rank order at %d: %v then %v", i, both[i-1].Rank, both[i].Rank)
			break
		}
	}
}

func TestSearchLimitIsClamped(t *testing.T) {
	db := openTemp(t)
	ids := seedSearchCorpus(t, db)

	// Enough rows to actually reach the ceiling. Inserted directly so this also
	// exercises the insert trigger at volume rather than one row at a time.
	for i := range searchLimitMax * 2 {
		if _, err := db.Exec(
			`INSERT INTO signals (run_date, topic_id, title, reason, score, source_count, primary_url)
			 VALUES ('2026-07-01', ?, ?, 'Bulk.', 4, 1, 'https://x.test/bulk')`,
			ids.agents, fmt.Sprintf("Bulk harness signal %d", i),
		); err != nil {
			t.Fatalf("bulk insert %d: %v", i, err)
		}
	}

	// Asking for more than the ceiling gets the ceiling, on both paths.
	text, err := db.Search(SearchQuery{Text: "harness", Kind: "signals", Limit: 100000})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(text) != searchLimitMax {
		t.Errorf("text search returned %d rows, want exactly the %d ceiling", len(text), searchLimitMax)
	}

	filter, err := db.Search(SearchQuery{Kind: "signals", Limit: 100000})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(filter) != searchLimitMax {
		t.Errorf("filter-only returned %d rows, want exactly the %d ceiling", len(filter), searchLimitMax)
	}

	// A zero limit takes the default, not "unlimited".
	def, err := db.Search(SearchQuery{Kind: "signals"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(def) != searchLimitDefault {
		t.Errorf("default limit returned %d rows, want %d", len(def), searchLimitDefault)
	}
}

// The index is external-content, so SQLite does not maintain it -- the triggers
// do. Signals are rewritten by ReplaceSignals on every compose (DELETE then
// INSERT), which makes the delete trigger a hot path, not an edge case. Without
// it, yesterday's deleted stars keep coming back as hits forever.
func TestSearchIndexFollowsReplaceSignals(t *testing.T) {
	db := openTemp(t)
	ids := seedSearchCorpus(t, db)

	before, err := db.Search(SearchQuery{Text: "harness", Kind: "signals"})
	if err != nil || len(before) == 0 {
		t.Fatalf("expected a harness hit up front: %d hits, %v", len(before), err)
	}

	// Replace that topic's group for the same run date with something unrelated.
	if err := db.ReplaceSignals("2026-08-01", ids.agents, []NewSignal{{
		TopicID: ids.agents,
		Title:   "Something else entirely",
		Reason:  "Unrelated.",
		ItemIDs: []int64{ids.item},
	}}); err != nil {
		t.Fatalf("ReplaceSignals: %v", err)
	}

	after, err := db.Search(SearchQuery{Text: "harness", Kind: "signals", Limit: 50})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, h := range after {
		if h.Date == "2026-08-01" && h.TopicID == ids.agents {
			t.Errorf("a replaced signal is still in the index: %+v", h)
		}
	}

	// And the replacement is findable, which proves the insert half too.
	repl, err := db.Search(SearchQuery{Text: "entirely", Kind: "signals"})
	if err != nil || len(repl) == 0 {
		t.Fatalf("the replacement signal is not indexed: %d hits, %v", len(repl), err)
	}

	// Finally, ask FTS5 itself whether index and content agree.
	if _, err := db.Exec(`INSERT INTO signals_fts(signals_fts) VALUES('integrity-check')`); err != nil {
		t.Errorf("signals_fts integrity-check failed: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO items_fts(items_fts) VALUES('integrity-check')`); err != nil {
		t.Errorf("items_fts integrity-check failed: %v", err)
	}
}

// An instance that has been running for weeks must not come back with an empty
// index, so migration 003 backfills. Exercised for real: build a database at the
// schema version that shipped before search existed, put rows in it, then open it
// with the current binary and check they are findable.
func TestSearchMigrationBackfillsExistingRows(t *testing.T) {
	dir := t.TempDir() + "/backfill.db"

	// searchMigration is the version that introduced the index. Everything at or
	// above it must be absent from the starting database, or the runner has
	// nothing to do and this test proves nothing.
	const searchMigration = 3

	first := openAtVersion(t, dir, searchMigration-1)
	seedSearchCorpus(t, first)

	// The index cannot exist yet -- that is the whole premise.
	var n int
	if err := first.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE name = 'signals_fts'`).Scan(&n); err != nil {
		t.Fatalf("look for signals_fts: %v", err)
	}
	if n != 0 {
		t.Fatalf("signals_fts exists at version %d; this test is no longer testing a backfill",
			searchMigration-1)
	}
	first.Close()

	again, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer again.Close()

	if v, _ := again.Version(); v != len(mustLoadMigrations(t)) {
		t.Errorf("schema version = %d after reopen, want fully migrated", v)
	}

	hits, err := again.Search(SearchQuery{Text: "harness", Kind: "signals"})
	if err != nil {
		t.Fatalf("Search after re-migrate: %v", err)
	}
	if len(hits) == 0 {
		t.Error("migration 003 did not backfill rows that predated it")
	}

	items, err := again.Search(SearchQuery{Text: "transcript", Kind: "items"})
	if err != nil {
		t.Fatalf("Search items after re-migrate: %v", err)
	}
	if len(items) == 0 {
		t.Error("migration 003 did not backfill existing items")
	}
}

// --- fixtures ---

// openAtVersion builds a database at an older schema version, by applying only the
// migrations up to it.
//
// This is how an upgrade is tested honestly. The alternative -- migrate fully, then
// drop the objects the newest migration created and rewind user_version -- needs a
// hand-maintained list of those objects, and silently stops testing anything the
// day someone adds a migration and forgets to extend it.
func openAtVersion(t *testing.T, path string, version int) *DB {
	t.Helper()

	sqlDB, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(on)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	sqlDB.SetMaxOpenConns(1)

	for _, m := range mustLoadMigrations(t) {
		if m.version > version {
			break
		}
		if _, err := sqlDB.Exec(m.sql); err != nil {
			t.Fatalf("migration %d (%s): %v", m.version, m.name, err)
		}
	}
	if _, err := sqlDB.Exec(fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		t.Fatalf("set user_version: %v", err)
	}

	db := &DB{DB: sqlDB, Path: path}
	t.Cleanup(func() { db.Close() })
	return db
}

type corpusIDs struct {
	agents, voice int64
	item          int64
}

func seedSearchCorpus(t *testing.T, db *DB) corpusIDs {
	t.Helper()

	agents, err := db.CreateTopic(Topic{Title: "Agents", Definition: "Agent harnesses.", Active: true})
	if err != nil {
		t.Fatalf("create topic: %v", err)
	}
	voice, err := db.CreateTopic(Topic{Title: "Voice AI", Definition: "Speech.", Active: true})
	if err != nil {
		t.Fatalf("create topic: %v", err)
	}

	res, err := db.Exec(
		`INSERT INTO sources (kind, name, config, active) VALUES ('rss','Blog','{"url":"https://x.test/f"}',1)`)
	if err != nil {
		t.Fatalf("insert source: %v", err)
	}
	sourceID, _ := res.LastInsertId()

	newItem := func(url, title, summary string) int64 {
		id, _, err := db.UpsertItem(NewItem{
			SourceID: sourceID, CanonicalURL: url, Title: title, Summary: summary,
		})
		if err != nil {
			t.Fatalf("upsert item: %v", err)
		}
		if _, err := db.Exec(
			`INSERT OR IGNORE INTO item_sources (item_id, source_id) VALUES (?, ?)`, id, sourceID); err != nil {
			t.Fatalf("attribute item: %v", err)
		}
		return id
	}

	a := newItem("https://x.test/a", "Agent harness and tooling patterns",
		"A long look at tool loops, context windows and harness design.")
	b := newItem("https://x.test/b", "A video transcript of the keynote",
		"Transcript of a talk about speech models.")
	c := newItem("https://x.test/c", "Tiếng Việt có dấu trong tiêu đề", "Bản ghi tiếng Việt.")

	// Matches, so ReplaceSignals can compute score and primary_url.
	for _, m := range []Match{
		{ItemID: a, TopicID: agents, Score: 5, Reason: "on topic"},
		{ItemID: b, TopicID: voice, Score: 4, Reason: "speech"},
		{ItemID: c, TopicID: voice, Score: 4, Reason: "vietnamese"},
	} {
		if err := db.SaveMatches([]Match{m}); err != nil {
			t.Fatalf("save match: %v", err)
		}
	}

	if err := db.ReplaceSignals("2026-08-01", agents, []NewSignal{{
		TopicID: agents,
		Title:   "Agent harness and tooling converge",
		Reason:  "Several outlets described the same harness and tooling shift.",
		ItemIDs: []int64{a},
	}}); err != nil {
		t.Fatalf("replace signals: %v", err)
	}
	if err := db.ReplaceSignals("2026-08-02", voice, []NewSignal{
		{TopicID: voice, Title: "Speech models get an agent mode", Reason: "One outlet.", ItemIDs: []int64{b}},
		{TopicID: voice, Title: "Tiếng Việt trong bản ghi", Reason: "Bản ghi có dấu.", ItemIDs: []int64{c}},
	}); err != nil {
		t.Fatalf("replace signals: %v", err)
	}

	return corpusIDs{agents: agents, voice: voice, item: a}
}
