package pipeline

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/tammai/interstellar/internal/config"
	"github.com/tammai/interstellar/internal/store"
)

func testDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testRunDate is the run date the pipeline stamps stats with. Today's local date,
// not a fixed string: TopicHitRates filters on date('now','localtime'), so a
// hardcoded date would fall outside every window and the stats would look unwritten.
var testRunDate = store.RunDate(time.Now())

func testConfig() config.Runtime {
	return config.Runtime{
		HNMinPoints: 25, ClassifyBatchSize: 30, MatchThreshold: 4,
		HotThreshold: 3, SkyDays: 7, MaxItemAgeDays: 30,
	}
}

// feedServer serves a one-item RSS feed pointing at articleURL.
func feedServer(t *testing.T, title, articleURL string) *httptest.Server {
	t.Helper()
	body := fmt.Sprintf(`<?xml version="1.0"?><rss version="2.0"><channel><title>Feed</title>
	  <item><title>%s</title><link>%s</link><description>Body text.</description></item>
	</channel></rss>`, title, articleURL)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func addSource(t *testing.T, db *store.DB, name, feedURL string) int64 {
	t.Helper()
	res, err := db.Exec(
		`INSERT INTO sources (kind, name, config, active) VALUES ('rss', ?, ?, 1)`,
		name, fmt.Sprintf(`{"url":%q}`, feedURL),
	)
	if err != nil {
		t.Fatalf("add source %s: %v", name, err)
	}
	id, _ := res.LastInsertId()
	return id
}

func countRows(t *testing.T, db *store.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// §10: "re-ingest creates zero duplicates."
func TestIngestIsIdempotent(t *testing.T) {
	db := testDB(t)
	srv := feedServer(t, "A post", "https://example.test/a-post")
	addSource(t, db, "Test Feed", srv.URL)

	first, err := Ingest(context.Background(), db, testConfig(), quietLogger())
	if err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	if first.ItemsNew != 1 {
		t.Fatalf("first run stored %d items, want 1", first.ItemsNew)
	}

	second, err := Ingest(context.Background(), db, testConfig(), quietLogger())
	if err != nil {
		t.Fatalf("second ingest: %v", err)
	}
	if second.ItemsNew != 0 {
		t.Errorf("second run stored %d new items, want 0", second.ItemsNew)
	}
	if second.ItemsSeen != 1 {
		t.Errorf("second run saw %d known items, want 1", second.ItemsSeen)
	}

	if n := countRows(t, db, "SELECT count(*) FROM items"); n != 1 {
		t.Errorf("items table has %d rows after two runs, want 1", n)
	}
	if n := countRows(t, db, "SELECT count(*) FROM item_sources"); n != 1 {
		t.Errorf("item_sources has %d rows after two runs, want 1", n)
	}
}

// The reason item_sources exists. Three feeds carrying one story must collapse
// to one item but keep all three attributions -- that count is what turns a
// signal gold (§6).
func TestIngestKeepsCrossSourceAttribution(t *testing.T) {
	db := testDB(t)

	// Same article, three spellings a real feed might use.
	a := feedServer(t, "Qwen 72B", "https://example.test/qwen-72b")
	b := feedServer(t, "Qwen 72B open weights", "https://www.example.test/qwen-72b/?utm_source=newsletter")
	c := feedServer(t, "Qwen 72B released", "https://example.test/qwen-72b#details")

	addSource(t, db, "Blog A", a.URL)
	addSource(t, db, "Blog B", b.URL)
	addSource(t, db, "Blog C", c.URL)

	if _, err := Ingest(context.Background(), db, testConfig(), quietLogger()); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	if n := countRows(t, db, "SELECT count(*) FROM items"); n != 1 {
		t.Fatalf("got %d item rows, want 1 -- canonicalization did not collapse the variants", n)
	}

	sourceCount := countRows(t, db,
		`SELECT count(DISTINCT source_id) FROM item_sources
		  WHERE item_id = (SELECT id FROM items LIMIT 1)`)
	if sourceCount != 3 {
		t.Errorf("distinct sources = %d, want 3; the hot flag would under-fire", sourceCount)
	}
}

// §10: "dead feed doesn't break ingest."
func TestIngestSurvivesDeadFeeds(t *testing.T) {
	db := testDB(t)

	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer dead.Close()

	garbage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "<rss><channel><item><title>unclosed")
	}))
	defer garbage.Close()

	healthy := feedServer(t, "Still here", "https://example.test/alive")

	addSource(t, db, "Dead Feed", dead.URL)
	addSource(t, db, "Garbage Feed", garbage.URL)
	addSource(t, db, "Healthy Feed", healthy.URL)
	// An unreachable host: DNS failure rather than an HTTP error.
	addSource(t, db, "Vanished Feed", "http://127.0.0.1:1/feed.xml")

	result, err := Ingest(context.Background(), db, testConfig(), quietLogger())
	if err != nil {
		t.Fatalf("ingest returned a fatal error for a dead feed: %v", err)
	}

	if result.ItemsNew != 1 {
		t.Errorf("stored %d items, want 1 from the healthy feed", result.ItemsNew)
	}
	if result.SourcesOK != 1 {
		t.Errorf("SourcesOK = %d, want 1", result.SourcesOK)
	}
	if len(result.Failures) != 3 {
		t.Errorf("recorded %d failures, want 3", len(result.Failures))
	}

	// Failures must be attributable in the UI, not just counted.
	for _, f := range result.Failures {
		if f.Name == "" || f.Err == "" {
			t.Errorf("failure missing detail: %+v", f)
		}
	}
}

// §5.1: last_run_at advances on success only, so a broken feed stays visibly
// stale instead of looking healthy.
func TestIngestMarksOnlySuccessfulSources(t *testing.T) {
	db := testDB(t)

	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer dead.Close()
	healthy := feedServer(t, "Fine", "https://example.test/fine")

	deadID := addSource(t, db, "Dead", dead.URL)
	healthyID := addSource(t, db, "Healthy", healthy.URL)

	if _, err := Ingest(context.Background(), db, testConfig(), quietLogger()); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	if n := countRows(t, db, "SELECT count(*) FROM sources WHERE id = ? AND last_run_at IS NOT NULL", healthyID); n != 1 {
		t.Error("healthy source did not get last_run_at")
	}
	if n := countRows(t, db, "SELECT count(*) FROM sources WHERE id = ? AND last_run_at IS NULL", deadID); n != 1 {
		t.Error("dead source got last_run_at; it will look healthy in Settings")
	}
}

func TestIngestSkipsInactiveSources(t *testing.T) {
	db := testDB(t)
	srv := feedServer(t, "A post", "https://example.test/a")
	id := addSource(t, db, "Disabled", srv.URL)
	if _, err := db.Exec("UPDATE sources SET active = 0 WHERE id = ?", id); err != nil {
		t.Fatalf("deactivate: %v", err)
	}

	result, err := Ingest(context.Background(), db, testConfig(), quietLogger())
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if result.SourcesTried != 0 || result.ItemsNew != 0 {
		t.Errorf("inactive source was fetched: %+v", result)
	}
}

func TestIngestWithNoSources(t *testing.T) {
	db := testDB(t)
	result, err := Ingest(context.Background(), db, testConfig(), quietLogger())
	if err != nil {
		t.Fatalf("ingest on empty instance: %v", err)
	}
	if result.SourcesTried != 0 {
		t.Errorf("SourcesTried = %d, want 0", result.SourcesTried)
	}
}

// A source whose kind has no fetcher is a failure, not a panic.
func TestIngestUnknownKindIsRecordedNotFatal(t *testing.T) {
	db := testDB(t)
	healthy := feedServer(t, "Fine", "https://example.test/fine")
	addSource(t, db, "Healthy", healthy.URL)

	// The CHECK constraint blocks a bogus kind, so simulate the case a future
	// migration could create: a kind the registry doesn't know yet.
	if _, err := db.Exec(`INSERT INTO sources (kind, name, config, active)
	                      VALUES ('youtube_rss', 'Malformed config', '{}', 1)`); err != nil {
		t.Fatalf("insert: %v", err)
	}

	result, err := Ingest(context.Background(), db, testConfig(), quietLogger())
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if result.SourcesOK != 1 {
		t.Errorf("SourcesOK = %d, want 1", result.SourcesOK)
	}
	if len(result.Failures) != 1 {
		t.Errorf("failures = %d, want 1", len(result.Failures))
	}
}

// Feeds routinely serve their whole archive. Without an age filter, a first run
// classifies years of writing and can render a 2017 post as today's news —
// observed on real data, where 27 of 52 signals came from articles over a year
// old.
func TestIngestSkipsArchiveContent(t *testing.T) {
	db := testDB(t)

	old := time.Now().AddDate(-3, 0, 0).Format(time.RFC1123Z)
	recent := time.Now().AddDate(0, 0, -2).Format(time.RFC1123Z)
	body := fmt.Sprintf(`<?xml version="1.0"?><rss version="2.0"><channel><title>Feed</title>
	  <item><title>Ancient post</title><link>https://example.test/old</link><pubDate>%s</pubDate></item>
	  <item><title>Recent post</title><link>https://example.test/new</link><pubDate>%s</pubDate></item>
	  <item><title>Undated post</title><link>https://example.test/undated</link></item>
	</channel></rss>`, old, recent)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, body)
	}))
	defer srv.Close()
	addSource(t, db, "Archive Feed", srv.URL)

	cfg := testConfig()
	cfg.MaxItemAgeDays = 30

	result, err := Ingest(context.Background(), db, cfg, quietLogger())
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}

	if result.ItemsTooOld != 1 {
		t.Errorf("ItemsTooOld = %d, want 1", result.ItemsTooOld)
	}
	// The undated one is kept: plenty of feeds omit a date, and dropping those
	// would silently lose real news.
	if result.ItemsNew != 2 {
		t.Errorf("stored %d items, want 2 (recent + undated)", result.ItemsNew)
	}
	if n := countRows(t, db, "SELECT count(*) FROM items WHERE canonical_url = 'https://example.test/old'"); n != 0 {
		t.Error("a three-year-old post was stored as news")
	}
	if n := countRows(t, db, "SELECT count(*) FROM items WHERE canonical_url = 'https://example.test/undated'"); n != 1 {
		t.Error("an undated post was dropped; feeds without dates would go dark")
	}
}

// Zero disables the filter, for anyone who wants the whole archive.
func TestIngestAgeFilterCanBeDisabled(t *testing.T) {
	db := testDB(t)
	old := time.Now().AddDate(-5, 0, 0).Format(time.RFC1123Z)
	body := fmt.Sprintf(`<?xml version="1.0"?><rss version="2.0"><channel><title>F</title>
	  <item><title>Ancient</title><link>https://example.test/a</link><pubDate>%s</pubDate></item>
	</channel></rss>`, old)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, body)
	}))
	defer srv.Close()
	addSource(t, db, "Feed", srv.URL)

	cfg := testConfig()
	cfg.MaxItemAgeDays = 0

	result, err := Ingest(context.Background(), db, cfg, quietLogger())
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if result.ItemsNew != 1 || result.ItemsTooOld != 0 {
		t.Errorf("filter still applied with maxAge=0: %+v", result)
	}
}

// --- fuzzy dedup ---

// A republish at a different URL must fold into the existing item AND credit the
// second source. Two outlets carrying one story is exactly what source_count
// measures, and it is what makes a signal gold (§6) — so a silent discard would
// throw away the signal this feature exists to sharpen.
func TestIngestMergesRepublishesAndCreditsBothSources(t *testing.T) {
	db := testDB(t)

	const title = "Agent harness patterns are converging on tool loops"
	origin := feedServer(t, title, "https://origin.test/harness-patterns")
	// Same headline, different URL, plus the syndicating outlet's suffix.
	mirror := feedServer(t, title+" | Mirror Outlet", "https://mirror.test/syndicated/harness")

	originID := addSource(t, db, "Origin", origin.URL)
	mirrorID := addSource(t, db, "Mirror", mirror.URL)

	result, err := Ingest(context.Background(), db, testConfig(), quietLogger())
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	if result.ItemsNew != 1 {
		t.Errorf("ItemsNew = %d, want 1 — the republish should not be a second item", result.ItemsNew)
	}
	if result.ItemsMerged != 1 {
		t.Errorf("ItemsMerged = %d, want 1", result.ItemsMerged)
	}
	if n := countRows(t, db, "SELECT count(*) FROM items"); n != 1 {
		t.Errorf("%d item rows, want 1", n)
	}

	// The point of the whole exercise: both sources are credited.
	if n := countRows(t, db, "SELECT count(*) FROM item_sources"); n != 2 {
		t.Errorf("%d attributions, want 2 — the second outlet was not credited", n)
	}
	for _, id := range []int64{originID, mirrorID} {
		if n := countRows(t, db, "SELECT count(*) FROM item_sources WHERE source_id = ?", id); n != 1 {
			t.Errorf("source %d has %d attributions, want 1", id, n)
		}
	}
}

// Two genuinely different stories must stay two items, however alike the headlines.
func TestIngestKeepsDistinctStoriesApart(t *testing.T) {
	db := testDB(t)

	a := feedServer(t, "OpenAI releases GPT-5 to all paid tiers", "https://a.test/gpt5")
	b := feedServer(t, "OpenAI releases GPT-5.1 to all paid tiers", "https://b.test/gpt51")
	addSource(t, db, "A", a.URL)
	addSource(t, db, "B", b.URL)

	result, err := Ingest(context.Background(), db, testConfig(), quietLogger())
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	if result.ItemsMerged != 0 {
		t.Errorf("ItemsMerged = %d: two different releases were merged", result.ItemsMerged)
	}
	if n := countRows(t, db, "SELECT count(*) FROM items"); n != 2 {
		t.Errorf("%d item rows, want 2", n)
	}
}

// The in-memory index grows as items land, so two feeds carrying the same
// republish within one run collapse too — not only against yesterday's items.
func TestIngestMergesWithinASingleRun(t *testing.T) {
	db := testDB(t)

	const title = "Retrieval quietly replaced the context window as the bottleneck"
	for i, host := range []string{"one", "two", "three"} {
		srv := feedServer(t, title, "https://"+host+".test/story")
		addSource(t, db, fmt.Sprintf("Feed %d", i), srv.URL)
	}

	result, err := Ingest(context.Background(), db, testConfig(), quietLogger())
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	if result.ItemsNew != 1 || result.ItemsMerged != 2 {
		t.Errorf("new = %d, merged = %d; want 1 and 2", result.ItemsNew, result.ItemsMerged)
	}
	if n := countRows(t, db, "SELECT count(*) FROM item_sources"); n != 3 {
		t.Errorf("%d attributions, want 3 — all three outlets carried it", n)
	}
}

// Re-ingesting must still add nothing, which is §10's requirement, and the merge
// path must not quietly break it.
func TestIngestMergeIsIdempotentAcrossRuns(t *testing.T) {
	db := testDB(t)

	const title = "Agent harness patterns are converging on tool loops"
	origin := feedServer(t, title, "https://origin.test/a")
	mirror := feedServer(t, title, "https://mirror.test/b")
	addSource(t, db, "Origin", origin.URL)
	addSource(t, db, "Mirror", mirror.URL)

	if _, err := Ingest(context.Background(), db, testConfig(), quietLogger()); err != nil {
		t.Fatal(err)
	}
	itemsAfterFirst := countRows(t, db, "SELECT count(*) FROM items")
	attrsAfterFirst := countRows(t, db, "SELECT count(*) FROM item_sources")

	second, err := Ingest(context.Background(), db, testConfig(), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	if second.ItemsNew != 0 {
		t.Errorf("second run created %d items", second.ItemsNew)
	}
	if n := countRows(t, db, "SELECT count(*) FROM items"); n != itemsAfterFirst {
		t.Errorf("items went from %d to %d across runs", itemsAfterFirst, n)
	}
	if n := countRows(t, db, "SELECT count(*) FROM item_sources"); n != attrsAfterFirst {
		t.Errorf("attributions went from %d to %d across runs", attrsAfterFirst, n)
	}
}
