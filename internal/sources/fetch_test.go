package sources

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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

func TestYouTubeFetcherBuildsChannelURL(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/atom+xml")
		w.Write([]byte(sampleAtom))
	}))
	defer srv.Close()

	// Explicit url wins over channel_id, which is how the test reaches the stub.
	items, err := YouTubeFetcher{}.Fetch(context.Background(),
		cfg(t, map[string]string{"url": srv.URL + "?channel_id=UCtest"}), Options{})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}
	if gotQuery != "channel_id=UCtest" {
		t.Errorf("query = %q", gotQuery)
	}
	if items[0].Detail != "video" {
		t.Errorf("detail = %q, want video", items[0].Detail)
	}
}

func TestYouTubeFetcherRequiresChannelOrURL(t *testing.T) {
	_, err := YouTubeFetcher{}.Fetch(context.Background(), cfg(t, map[string]string{}), Options{})
	if err == nil {
		t.Fatal("empty youtube config accepted")
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
