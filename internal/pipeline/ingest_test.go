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

func testConfig() config.Runtime {
	return config.Runtime{HNMinPoints: 25, ClassifyBatchSize: 30, MatchThreshold: 4, HotThreshold: 3, SkyDays: 7}
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
