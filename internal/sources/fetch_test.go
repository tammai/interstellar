package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const sampleRSS = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0"><channel>
  <title>Test Blog</title>
  <item>
    <title>Verification loops as the core primitive</title>
    <link>https://example.test/verification-loops?utm_source=rss</link>
    <description>&lt;p&gt;A post about &lt;b&gt;agent harnesses&lt;/b&gt; and loops.&lt;/p&gt;</description>
    <pubDate>Mon, 10 Aug 2026 09:00:00 GMT</pubDate>
  </item>
  <item>
    <title>Context compaction strategies</title>
    <link>https://example.test/compaction</link>
    <description>Comparing five designs.</description>
    <pubDate>Sun, 09 Aug 2026 09:00:00 GMT</pubDate>
  </item>
</channel></rss>`

const sampleAtom = `<?xml version="1.0" encoding="utf-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <title>Channel</title>
  <entry>
    <title>On-prem inference, nine months in</title>
    <link rel="alternate" href="https://youtube.test/watch?v=abc123"/>
    <published>2026-08-10T12:00:00+00:00</published>
    <summary>A practitioner walkthrough.</summary>
  </entry>
</feed>`

func TestRSSFetcherParsesAndCleans(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		w.Write([]byte(sampleRSS))
	}))
	defer srv.Close()

	items, err := RSSFetcher{}.Fetch(context.Background(), cfg(t, map[string]string{"url": srv.URL}), Options{})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2", len(items))
	}

	first := items[0]
	if first.Title != "Verification loops as the core primitive" {
		t.Errorf("title = %q", first.Title)
	}
	// Markup is pure token cost on every classify call, so it gets stripped.
	want := "A post about agent harnesses and loops."
	if first.Summary != want {
		t.Errorf("summary = %q, want %q", first.Summary, want)
	}
	if first.PublishedAt.IsZero() {
		t.Error("published date not parsed")
	}
}

// YouTube discovery runs on the Data API because the Atom feeds are gone --
// every /feeds/videos.xml id form answers 404 from YouTube's own RSS server,
// while the same URL with no parameters answers 400. See youtube.go; the point
// of writing it down is that nine sources broke at once and the obvious
// diagnosis ("our channel ids went stale") is the wrong one.
func TestYouTubeFetcherListsUploadsThroughTheAPI(t *testing.T) {
	var gotPath, gotPlaylist, gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotPlaylist = r.URL.Query().Get("playlistId")
		gotKey = r.URL.Query().Get("key")
		json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{
				{"snippet": map[string]any{
					"title":       "  Agents in production  ",
					"description": "How we shipped it.",
					"publishedAt": "2026-09-07T10:00:00Z",
					"resourceId":  map[string]string{"videoId": "abcdefghijk"},
				}},
				// A deleted upload keeps its playlist slot. Letting it through would
				// put a permanently empty item in front of the classifier every run.
				{"snippet": map[string]any{
					"title":      "Deleted video",
					"resourceId": map[string]string{"videoId": "zzzzzzzzzzz"},
				}},
				// No video id at all: private, or mid-removal.
				{"snippet": map[string]any{"title": "Something", "resourceId": map[string]string{"videoId": ""}}},
			},
		})
	}))
	defer srv.Close()

	restore := youtubeAPIBase
	youtubeAPIBase = srv.URL + "/youtube/v3/playlistItems"
	defer func() { youtubeAPIBase = restore }()

	items, err := YouTubeFetcher{}.Fetch(context.Background(),
		cfg(t, map[string]string{"channel_id": "UCtest123"}),
		Options{YouTubeAPIKey: "k-test"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	// The uploads playlist is the channel id with UC swapped for UU. This is what
	// keeps discovery to ONE quota unit: search.list costs 100.
	if gotPlaylist != "UUtest123" {
		t.Errorf("playlistId = %q, want UUtest123", gotPlaylist)
	}
	if gotKey != "k-test" {
		t.Errorf("key = %q, want the key from Options", gotKey)
	}
	if gotPath != "/youtube/v3/playlistItems" {
		t.Errorf("path = %q", gotPath)
	}

	if len(items) != 1 {
		t.Fatalf("got %d items, want 1 (deleted and id-less uploads dropped)", len(items))
	}
	got := items[0]
	if got.URL != "https://www.youtube.com/watch?v=abcdefghijk" {
		t.Errorf("url = %q", got.URL)
	}
	if got.Title != "Agents in production" {
		t.Errorf("title = %q, want it trimmed", got.Title)
	}
	if got.Summary != "How we shipped it." {
		t.Errorf("summary = %q", got.Summary)
	}
	if got.Detail != "video" {
		t.Errorf("detail = %q, want video", got.Detail)
	}
	if got.PublishedAt.IsZero() {
		t.Error("publishedAt not parsed")
	}
}

// Rows written before the API move carry only the retired feed URL. Reading the
// id back out of it is what makes this change need no data migration.
func TestYouTubeFetcherRecoversChannelIDFromALegacyFeedURL(t *testing.T) {
	var gotPlaylist string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPlaylist = r.URL.Query().Get("playlistId")
		json.NewEncoder(w).Encode(map[string]any{"items": []any{}})
	}))
	defer srv.Close()

	restore := youtubeAPIBase
	youtubeAPIBase = srv.URL
	defer func() { youtubeAPIBase = restore }()

	_, err := YouTubeFetcher{}.Fetch(context.Background(),
		cfg(t, map[string]string{
			"url": "https://www.youtube.com/feeds/videos.xml?channel_id=UColdrow99",
		}),
		Options{YouTubeAPIKey: "k"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if gotPlaylist != "UUoldrow99" {
		t.Errorf("playlistId = %q, want the id recovered from the legacy url", gotPlaylist)
	}
}

// An unset key is something the environment did to you, so the error names the
// variable rather than saying "missing" -- the whole point of storing the NAME.
func TestYouTubeFetcherNamesTheKeyVariableWhenUnset(t *testing.T) {
	_, err := YouTubeFetcher{}.Fetch(context.Background(),
		cfg(t, map[string]string{"channel_id": "UCtest"}), Options{})
	if err == nil {
		t.Fatal("fetch without an API key succeeded")
	}
	if !strings.Contains(err.Error(), "YOUTUBE_API_KEY") {
		t.Errorf("error does not name the variable: %v", err)
	}
}

func TestYouTubeFetcherRejectsAChannelIDThatIsNotOne(t *testing.T) {
	_, err := YouTubeFetcher{}.Fetch(context.Background(),
		cfg(t, map[string]string{"channel_id": "lexfridman"}),
		Options{YouTubeAPIKey: "k"})
	if err == nil {
		t.Fatal("a handle was accepted as a channel id")
	}
}

// The API's error envelope is unwrapped to its sentence, same reasoning as
// llm.snippet: "API key not valid" is the only part that says what to do.
func TestYouTubeFetcherUnwrapsTheAPIErrorSentence(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":{"code":403,"message":"The request cannot be completed because you have exceeded your quota.","errors":[{"reason":"quotaExceeded"}]}}`))
	}))
	defer srv.Close()

	restore := youtubeAPIBase
	youtubeAPIBase = srv.URL
	defer func() { youtubeAPIBase = restore }()

	_, err := YouTubeFetcher{}.Fetch(context.Background(),
		cfg(t, map[string]string{"channel_id": "UCtest"}),
		Options{YouTubeAPIKey: "k"})
	if err == nil {
		t.Fatal("a 403 was treated as success")
	}
	if !strings.Contains(err.Error(), "exceeded your quota") {
		t.Errorf("error lost the API's sentence: %v", err)
	}
	// The key travels in the query string, so it must not be echoed by an error
	// built from the body.
	if strings.Contains(err.Error(), "key=") {
		t.Errorf("error may leak the key: %v", err)
	}
}

func TestHNFetcherAppliesThresholdAndAnnotates(t *testing.T) {
	var gotFilters string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotFilters = r.URL.Query().Get("numericFilters")
		json.NewEncoder(w).Encode(map[string]any{
			"hits": []map[string]any{
				{
					"objectID":     "1001",
					"title":        "Qwen releases 72B open-weight model",
					"url":          "https://example.test/qwen-72b",
					"points":       412,
					"created_at_i": 1786000000,
				},
				{
					// Ask HN posts carry no outbound URL; the thread is the artifact.
					"objectID":     "1002",
					"title":        "Ask HN: how are you running evals?",
					"url":          "",
					"points":       96,
					"story_text":   "We keep <i>drifting</i>.",
					"created_at_i": 1786000100,
				},
			},
		})
	}))
	defer srv.Close()

	old := HNSearchBase
	HNSearchBase = srv.URL
	defer func() { HNSearchBase = old }()

	items, err := HNFetcher{}.Fetch(context.Background(),
		cfg(t, map[string]string{"query": "open weights"}), Options{HNMinPoints: 25})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2", len(items))
	}

	// Decision #4: the threshold is 25, and it must reach the API as a filter --
	// filtering client-side would mean paying to download the noise.
	if !contains(gotFilters, "points>=25") {
		t.Errorf("numericFilters = %q, want points>=25", gotFilters)
	}

	if items[0].Detail != "412 points" {
		t.Errorf("detail = %q, want '412 points'", items[0].Detail)
	}
	if items[1].URL != "https://news.ycombinator.com/item?id=1002" {
		t.Errorf("text post URL = %q, want the HN thread", items[1].URL)
	}
	if items[1].Summary != "We keep drifting." {
		t.Errorf("story text not cleaned: %q", items[1].Summary)
	}
}

func TestHNFetcherPerSourceThresholdOverrides(t *testing.T) {
	var gotFilters string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotFilters = r.URL.Query().Get("numericFilters")
		w.Write([]byte(`{"hits":[]}`))
	}))
	defer srv.Close()

	old := HNSearchBase
	HNSearchBase = srv.URL
	defer func() { HNSearchBase = old }()

	_, err := HNFetcher{}.Fetch(context.Background(),
		json.RawMessage(`{"query":"voice ai","min_points":100}`), Options{HNMinPoints: 25})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !contains(gotFilters, "points>=100") {
		t.Errorf("numericFilters = %q, want the per-source override points>=100", gotFilters)
	}
}

// §10: a dead feed must not break ingest. These are the shapes "dead" takes.
func TestFetchersReportDeadFeeds(t *testing.T) {
	t.Run("http 500", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}))
		defer srv.Close()

		if _, err := (RSSFetcher{}).Fetch(context.Background(), cfg(t, map[string]string{"url": srv.URL}), Options{}); err == nil {
			t.Error("500 response accepted")
		}
	})

	t.Run("malformed xml", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`<rss><channel><item><title>unclosed`))
		}))
		defer srv.Close()

		if _, err := (RSSFetcher{}).Fetch(context.Background(), cfg(t, map[string]string{"url": srv.URL}), Options{}); err == nil {
			t.Error("malformed feed accepted")
		}
	})

	t.Run("hn 500", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}))
		defer srv.Close()

		old := HNSearchBase
		HNSearchBase = srv.URL
		defer func() { HNSearchBase = old }()

		if _, err := (HNFetcher{}).Fetch(context.Background(), cfg(t, map[string]string{"query": "x"}), Options{}); err == nil {
			t.Error("HN 500 accepted")
		}
	})

	t.Run("hang", func(t *testing.T) {
		release := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-release
		}))
		defer func() { close(release); srv.Close() }()

		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()

		start := time.Now()
		if _, err := (RSSFetcher{}).Fetch(ctx, cfg(t, map[string]string{"url": srv.URL}), Options{}); err == nil {
			t.Error("hung feed accepted")
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("hung feed held the run for %v; the context deadline is not being honored", elapsed)
		}
	})
}

func TestForUnknownKind(t *testing.T) {
	if _, err := For("carrier_pigeon"); err == nil {
		t.Error("unknown kind accepted")
	}
	for kind := range Registry {
		if _, err := For(kind); err != nil {
			t.Errorf("registered kind %q not resolvable: %v", kind, err)
		}
	}
}

func cfg(t *testing.T, m map[string]string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	return b
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle ||
		len(needle) == 0 || indexOf(haystack, needle) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// --- page fetch (the manual-forward inbox) ---

func TestFetchPagePrefersOpenGraph(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, `<html><head>
			<title>Site Name | The Real Headline</title>
			<meta property="og:title" content="The Real Headline">
			<meta property="og:description" content="A clean summary of the piece.">
		</head><body>ignored</body></html>`)
	}))
	defer srv.Close()

	item, err := FetchPage(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("FetchPage: %v", err)
	}
	// og:title is what the author chose for sharing; <title> carries the site name.
	if item.Title != "The Real Headline" {
		t.Errorf("title = %q, want the og:title", item.Title)
	}
	if item.Summary != "A clean summary of the piece." {
		t.Errorf("summary = %q", item.Summary)
	}
}

// Attribute order is not fixed in real HTML, and content-before-property is common.
func TestFetchPageHandlesEitherAttributeOrder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, `<html><head>
			<meta content="Reversed Order Works" property="og:title">
			<meta content="So does the description." property="og:description">
		</head></html>`)
	}))
	defer srv.Close()

	item, err := FetchPage(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("FetchPage: %v", err)
	}
	if item.Title != "Reversed Order Works" {
		t.Errorf("title = %q", item.Title)
	}
	if item.Summary != "So does the description." {
		t.Errorf("summary = %q", item.Summary)
	}
}

func TestFetchPageFallsBackToTitleAndMetaDescription(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, `<html><head>
			<title>Plain  &amp;  Escaped   Title</title>
			<meta name="description" content="Old-school description.">
		</head></html>`)
	}))
	defer srv.Close()

	item, err := FetchPage(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("FetchPage: %v", err)
	}
	// Entities unescaped and whitespace collapsed, via rss.go's cleanText.
	if item.Title != "Plain & Escaped Title" {
		t.Errorf("title = %q, want entities decoded and spaces collapsed", item.Title)
	}
	if item.Summary != "Old-school description." {
		t.Errorf("summary = %q", item.Summary)
	}
}

// A page with no title is an error. An item with neither title nor summary can
// never match anything and would sit in the database forever.
func TestFetchPageRefusesAPageWithNoTitle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		io.WriteString(w, "%PDF-1.4 not html at all")
	}))
	defer srv.Close()

	if _, err := FetchPage(context.Background(), srv.URL); err == nil {
		t.Error("a page with no title should be an error, not an empty item")
	}
}

func TestFetchPageReportsWhereRedirectsLanded(t *testing.T) {
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, `<html><head><title>Destination</title></head></html>`)
	}))
	defer final.Close()

	shortener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL, http.StatusFound)
	}))
	defer shortener.Close()

	item, err := FetchPage(context.Background(), shortener.URL)
	if err != nil {
		t.Fatalf("FetchPage: %v", err)
	}
	// The destination is what must be deduplicated against, not the shortener.
	if item.URL != final.URL {
		t.Errorf("URL = %q, want the redirect destination %q", item.URL, final.URL)
	}
}

func TestFetchPageReportsANon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	if _, err := FetchPage(context.Background(), srv.URL); err == nil {
		t.Error("a 403 should be reported, not treated as an empty page")
	}
}

// One source, many keywords. The per-keyword threshold is the reason the
// collapse is not simply a list of strings: a broad term needs a higher bar than
// a niche one, and flattening them floods the sky.
func TestHNFetcherRunsEveryQueryWithItsOwnThreshold(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]string{} // query -> numericFilters
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.URL.Query().Get("query")] = r.URL.Query().Get("numericFilters")
		mu.Unlock()
		w.Write([]byte(`{"hits":[]}`))
	}))
	defer srv.Close()

	old := HNSearchBase
	HNSearchBase = srv.URL
	defer func() { HNSearchBase = old }()

	_, err := HNFetcher{}.Fetch(context.Background(), json.RawMessage(
		`{"queries":[{"query":"LLM","min_points":100},{"query":"MCP","min_points":25},{"query":"evals"}]}`,
	), Options{HNMinPoints: 40})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(seen) != 3 {
		t.Fatalf("ran %d queries, want 3: %v", len(seen), seen)
	}
	if !contains(seen["LLM"], "points>=100") {
		t.Errorf("LLM filters = %q, want points>=100", seen["LLM"])
	}
	if !contains(seen["MCP"], "points>=25") {
		t.Errorf("MCP filters = %q, want points>=25", seen["MCP"])
	}
	// No threshold of its own falls through to the global setting.
	if !contains(seen["evals"], "points>=40") {
		t.Errorf("evals filters = %q, want the global points>=40", seen["evals"])
	}
}

// The whole reason for the collapse: one story matching several keywords is one
// story from one outlet. It must come back once, or item_sources records it once
// but the fetcher has still paid for the duplicates.
func TestHNFetcherCollapsesTheSameStoryAcrossQueries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The points climb between the two reads, as they do during a real day.
		points := 300
		if r.URL.Query().Get("query") == "agent" {
			points = 412
		}
		json.NewEncoder(w).Encode(map[string]any{"hits": []map[string]any{{
			"objectID": "1001", "title": "Launch HN: a coding agent",
			"url": "https://example.test/launch", "points": points, "created_at_i": 1786000000,
		}}})
	}))
	defer srv.Close()

	old := HNSearchBase
	HNSearchBase = srv.URL
	defer func() { HNSearchBase = old }()

	items, err := HNFetcher{}.Fetch(context.Background(), json.RawMessage(
		`{"queries":[{"query":"LLM"},{"query":"agent"}]}`), Options{HNMinPoints: 25})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1 -- the same story matched both keywords", len(items))
	}
	// The higher reading wins: it is the later one.
	if items[0].Detail != "412 points" {
		t.Errorf("detail = %q, want the higher reading '412 points'", items[0].Detail)
	}
}

// Per-query try/continue, the same rule §10 sets for sources. One keyword that
// errors must not cost the others their morning.
func TestHNFetcherSurvivesOneFailingQuery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("query") == "boom" {
			http.Error(w, "nope", http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"hits": []map[string]any{{
			"objectID": "1", "title": "survivor", "url": "https://example.test/a",
			"points": 99, "created_at_i": 1786000000,
		}}})
	}))
	defer srv.Close()

	old := HNSearchBase
	HNSearchBase = srv.URL
	defer func() { HNSearchBase = old }()

	items, err := HNFetcher{}.Fetch(context.Background(), json.RawMessage(
		`{"queries":[{"query":"boom"},{"query":"fine"}]}`), Options{HNMinPoints: 25})
	if err != nil {
		t.Fatalf("one failing query broke the whole source: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d items, want the 1 the healthy query returned", len(items))
	}
}

// Every query failing IS a dead source, though -- last_run_at must not advance
// on it, and that is driven by the error.
func TestHNFetcherReportsAllQueriesFailing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()

	old := HNSearchBase
	HNSearchBase = srv.URL
	defer func() { HNSearchBase = old }()

	if _, err := (HNFetcher{}).Fetch(context.Background(), json.RawMessage(
		`{"queries":[{"query":"a"},{"query":"b"}]}`), Options{HNMinPoints: 25}); err == nil {
		t.Fatal("every query failed and the source reported success")
	}
}

// A blank keyword is dropped, never sent: Algolia answers an empty query with the
// whole front page, which is the flood the thresholds exist to stop.
func TestHNFetcherDropsBlankQueries(t *testing.T) {
	var mu sync.Mutex
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		mu.Unlock()
		w.Write([]byte(`{"hits":[]}`))
	}))
	defer srv.Close()

	old := HNSearchBase
	HNSearchBase = srv.URL
	defer func() { HNSearchBase = old }()

	if _, err := (HNFetcher{}).Fetch(context.Background(), json.RawMessage(
		`{"queries":[{"query":"real"},{"query":"   "}]}`), Options{HNMinPoints: 25}); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if n != 1 {
		t.Errorf("made %d requests, want 1 -- the blank keyword was sent", n)
	}
	if _, err := (HNFetcher{}).Fetch(context.Background(), json.RawMessage(
		`{"queries":[{"query":""}]}`), Options{}); err == nil {
		t.Error("a config with nothing but a blank keyword was accepted")
	}
}

// The original one-query-per-source shape still parses. Config is an unmigrated
// JSON blob per row, so a hand-added source in the wild may still carry it.
func TestHNFetcherStillReadsTheSingleQueryShape(t *testing.T) {
	var gotQuery, gotFilters string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("query")
		gotFilters = r.URL.Query().Get("numericFilters")
		w.Write([]byte(`{"hits":[]}`))
	}))
	defer srv.Close()

	old := HNSearchBase
	HNSearchBase = srv.URL
	defer func() { HNSearchBase = old }()

	if _, err := (HNFetcher{}).Fetch(context.Background(),
		json.RawMessage(`{"query":"voice ai","min_points":100}`), Options{HNMinPoints: 25}); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if gotQuery != "voice ai" || !contains(gotFilters, "points>=100") {
		t.Errorf("legacy shape lost: query=%q filters=%q", gotQuery, gotFilters)
	}
}

// Typo tolerance is the whole reason keyword search was returning junk: Algolia
// is fuzzy by default, so "GLM" matched Gmail and the real GLM-5.3 release sat
// buried among 126 unrelated stories. If this parameter ever stops being sent,
// precision collapses silently -- the fetch still succeeds, it just returns the
// wrong stories.
func TestHNFetcherDisablesTypoTolerance(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]string{} // query -> typoTolerance
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.URL.Query().Get("query")] = r.URL.Query().Get("typoTolerance")
		mu.Unlock()
		w.Write([]byte(`{"hits":[]}`))
	}))
	defer srv.Close()

	old := HNSearchBase
	HNSearchBase = srv.URL
	defer func() { HNSearchBase = old }()

	if _, err := (HNFetcher{}).Fetch(context.Background(), json.RawMessage(
		`{"queries":[{"query":"GLM"},{"query":"MoE"}]}`), Options{HNMinPoints: 25}); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	for _, q := range []string{"GLM", "MoE"} {
		if seen[q] != "false" {
			t.Errorf("query %q sent typoTolerance=%q, want \"false\"", q, seen[q])
		}
	}
}

// The firehose is a keyword-less pass. It must send NO query parameter at all --
// an empty one is a different request, and the bar is the only thing bounding it.
func TestHNFetcherFirehoseSendsNoQuery(t *testing.T) {
	var mu sync.Mutex
	var passes []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		q := r.URL.Query()
		passes = append(passes, q.Get("query")+"|"+q.Get("numericFilters"))
		mu.Unlock()
		w.Write([]byte(`{"hits":[]}`))
	}))
	defer srv.Close()

	old := HNSearchBase
	HNSearchBase = srv.URL
	defer func() { HNSearchBase = old }()

	if _, err := (HNFetcher{}).Fetch(context.Background(), json.RawMessage(
		`{"min_points":10,"firehose_min_points":100,"queries":[{"query":"Qwen"}]}`,
	), Options{HNMinPoints: 25}); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(passes) != 2 {
		t.Fatalf("made %d passes, want 2 (one keyword, one firehose): %v", len(passes), passes)
	}

	var keyword, firehose string
	for _, p := range passes {
		if strings.HasPrefix(p, "|") {
			firehose = p
		} else {
			keyword = p
		}
	}
	if firehose == "" {
		t.Fatalf("no keyword-less pass was made: %v", passes)
	}
	// The two bars are opposite on purpose: keywords low to catch model releases
	// that never trend, firehose high to stay affordable.
	if !contains(keyword, "points>=10") {
		t.Errorf("keyword pass = %q, want the source floor points>=10", keyword)
	}
	if !contains(firehose, "points>=100") {
		t.Errorf("firehose pass = %q, want points>=100", firehose)
	}
}

// A firehose on its own is a valid source: no keywords at all.
func TestHNFetcherFirehoseAloneIsEnough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("query") != "" {
			t.Errorf("firehose-only config sent a query: %q", r.URL.Query().Get("query"))
		}
		json.NewEncoder(w).Encode(map[string]any{"hits": []map[string]any{{
			"objectID": "9", "title": "something big", "url": "https://example.test/big",
			"points": 500, "created_at_i": 1786000000,
		}}})
	}))
	defer srv.Close()

	old := HNSearchBase
	HNSearchBase = srv.URL
	defer func() { HNSearchBase = old }()

	items, err := HNFetcher{}.Fetch(context.Background(),
		json.RawMessage(`{"firehose_min_points":100}`), Options{})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}
	// And a config with neither is still an error.
	if _, err := (HNFetcher{}).Fetch(context.Background(),
		json.RawMessage(`{}`), Options{}); err == nil {
		t.Error("a config with no queries and no firehose was accepted")
	}
}

// The firehose re-finds what the keywords already found. One story, one item --
// otherwise every keyword hit above the firehose bar arrives twice.
func TestHNFetcherFirehoseCollapsesWithKeywords(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"hits": []map[string]any{{
			"objectID": "1", "title": "GLM-5.3 released", "url": "https://example.test/glm",
			"points": 1165, "created_at_i": 1786000000,
		}}})
	}))
	defer srv.Close()

	old := HNSearchBase
	HNSearchBase = srv.URL
	defer func() { HNSearchBase = old }()

	items, err := HNFetcher{}.Fetch(context.Background(), json.RawMessage(
		`{"firehose_min_points":100,"queries":[{"query":"GLM"}]}`), Options{HNMinPoints: 10})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1 -- the firehose re-found the keyword's story", len(items))
	}
}

// Paging: one page is 100 hits and the window is 72 hours, so a firehose that
// does not follow pages silently returns only the newest day and a half.
// search_by_date sorts newest first, so what gets lost is always the oldest.
func TestHNFetcherFollowsPages(t *testing.T) {
	var mu sync.Mutex
	var pages []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		page := r.URL.Query().Get("page")
		pages = append(pages, page)
		mu.Unlock()

		hits := make([]map[string]any, 0, 100)
		n := 100
		if page == "1" {
			n = 20 // a short page ends the walk
		}
		for i := 0; i < n; i++ {
			hits = append(hits, map[string]any{
				"objectID": fmt.Sprintf("%s-%d", page, i),
				"title":    fmt.Sprintf("story %s-%d", page, i),
				"url":      fmt.Sprintf("https://example.test/%s-%d", page, i),
				"points":   200, "created_at_i": 1786000000,
			})
		}
		json.NewEncoder(w).Encode(map[string]any{"hits": hits, "nbPages": 2})
	}))
	defer srv.Close()

	old := HNSearchBase
	HNSearchBase = srv.URL
	defer func() { HNSearchBase = old }()

	items, err := HNFetcher{}.Fetch(context.Background(),
		json.RawMessage(`{"firehose_min_points":100}`), Options{})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(pages) != 2 {
		t.Fatalf("requested pages %v, want 2", pages)
	}
	if len(items) != 120 {
		t.Errorf("got %d items, want 120 across both pages", len(items))
	}
}

// A firehose that fails must not take the keywords down with it, and vice versa.
func TestHNFetcherFirehoseFailureLeavesKeywords(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("query") == "" {
			http.Error(w, "firehose down", http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"hits": []map[string]any{{
			"objectID": "1", "title": "kept", "url": "https://example.test/a",
			"points": 40, "created_at_i": 1786000000,
		}}})
	}))
	defer srv.Close()

	old := HNSearchBase
	HNSearchBase = srv.URL
	defer func() { HNSearchBase = old }()

	items, err := HNFetcher{}.Fetch(context.Background(), json.RawMessage(
		`{"firehose_min_points":100,"queries":[{"query":"Qwen"}]}`), Options{HNMinPoints: 10})
	if err != nil {
		t.Fatalf("a failing firehose broke the whole source: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d items, want the 1 the keyword returned", len(items))
	}
}
