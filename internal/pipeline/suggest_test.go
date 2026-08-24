package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tammai/interstellar/internal/sources"
	"github.com/tammai/interstellar/internal/store"
)

const suggestFeedBody = `<?xml version="1.0"?><rss version="2.0"><channel>
  <title>Feed Title</title>
  <item><title>A story</title><link>https://x.test/1</link><description>Body.</description></item>
</channel></rss>`

// feedHost serves a feed at every path in `feeds`, redirects per `redirects`, and
// 404s everything else -- enough to play a handful of candidate URLs at once.
func feedHost(t *testing.T, feeds []string, redirects map[string]string) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if to, ok := redirects[r.URL.Path]; ok {
			http.Redirect(w, r, srv.URL+to, http.StatusTemporaryRedirect)
			return
		}
		for _, p := range feeds {
			if r.URL.Path == p {
				w.Header().Set("Content-Type", "application/rss+xml")
				w.Write([]byte(suggestFeedBody))
				return
			}
		}
		http.Error(w, "no such thing", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// suggestModel replies with a fixed suggestion object.
func suggestModel(t *testing.T, feeds []map[string]string, keywords []map[string]any) *fakeModel {
	t.Helper()
	body, err := json.Marshal(map[string]any{"feeds": feeds, "keywords": keywords})
	if err != nil {
		t.Fatal(err)
	}
	return newFakeModel(t, func(string) string { return string(body) })
}

func sourceURLs(t *testing.T, db *store.DB) map[string]store.Source {
	t.Helper()
	all, err := db.AllSources()
	if err != nil {
		t.Fatalf("AllSources: %v", err)
	}
	out := map[string]store.Source{}
	for _, s := range all {
		var cfg struct {
			URL string `json:"url"`
		}
		json.Unmarshal(s.Config, &cfg)
		if cfg.URL != "" {
			out[cfg.URL] = s
		}
	}
	return out
}

func suggestTopic(t *testing.T, db *store.DB) store.Topic {
	t.Helper()
	id := seedTopic(t, db, "Agents", "Anything about agent harnesses and tool use.")
	topic, err := db.Topic(id)
	if err != nil {
		t.Fatalf("Topic: %v", err)
	}
	return topic
}

// A verified feed becomes a row, and the row is switched OFF.
//
// The owner asked for suggestions, not for their sky to change while they were
// reading about it -- and a model's guess is exactly the thing that should not
// reach tomorrow's run without someone looking at it.
func TestSuggestSourcesAddsVerifiedFeedsPaused(t *testing.T) {
	db := testDB(t)
	topic := suggestTopic(t, db)
	host := feedHost(t, []string{"/feed"}, nil)

	model := suggestModel(t, []map[string]string{
		{"name": "Good Blog", "url": host.URL + "/feed"},
	}, nil)

	res, err := SuggestSources(context.Background(), db, clientFor(t, db, model), topic, quietLogger())
	if err != nil {
		t.Fatalf("SuggestSources: %v", err)
	}
	if res.Added != 1 {
		t.Fatalf("added %d, want 1 (feeds: %+v)", res.Added, res.Feeds)
	}

	got, ok := sourceURLs(t, db)[host.URL+"/feed"]
	if !ok {
		t.Fatalf("no source row written; have %v", sourceURLs(t, db))
	}
	if got.Active {
		t.Error("the row is active; a suggestion must not start ingesting before the owner has seen it")
	}
	if got.Name != "Good Blog" {
		t.Errorf("name = %q, want the proposed name", got.Name)
	}
	// Provenance, so the Sources screen row can say where it came from.
	var cfg struct {
		SuggestedFor int64 `json:"suggested_for"`
	}
	json.Unmarshal(got.Config, &cfg)
	if cfg.SuggestedFor != topic.ID {
		t.Errorf("suggested_for = %d, want topic %d", cfg.SuggestedFor, topic.ID)
	}
	if res.Feeds[0].Items != 1 {
		t.Errorf("items = %d, want the count the real parser got", res.Feeds[0].Items)
	}
}

// The whole reason every URL is probed: a model recalls feed paths that do not
// exist, and a wrong one does not fail loudly. It becomes a source row that
// quietly never yields anything.
func TestSuggestSourcesRejectsUrlsThatAreNotFeeds(t *testing.T) {
	db := testDB(t)
	topic := suggestTopic(t, db)

	notAFeed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html><head><title>A page</title></head><body>hi</body></html>"))
	}))
	defer notAFeed.Close()

	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<?xml version="1.0"?><rss version="2.0"><channel><title>Empty</title></channel></rss>`))
	}))
	defer empty.Close()

	model := suggestModel(t, []map[string]string{
		{"name": "Invented", "url": notAFeed.URL + "/rss.xml"},
		{"name": "Empty", "url": empty.URL + "/feed"},
		{"name": "Nonsense", "url": "not-a-url-at-all"},
		{"name": "Wrong scheme", "url": "ftp://files.test/feed"},
	}, nil)

	res, err := SuggestSources(context.Background(), db, clientFor(t, db, model), topic, quietLogger())
	if err != nil {
		t.Fatalf("SuggestSources: %v", err)
	}
	if res.Added != 0 {
		t.Errorf("added %d rows, want 0: none of these is a working feed", res.Added)
	}
	if n := len(sourceURLs(t, db)); n != 0 {
		t.Errorf("%d source rows written, want 0", n)
	}
	for _, f := range res.Feeds {
		if f.Status != suggestRejected {
			t.Errorf("%q status = %q, want rejected", f.Name, f.Status)
		}
		if f.Reason == "" {
			t.Errorf("%q was rejected with no reason; the panel has nothing to show", f.Name)
		}
	}
	// The two that could be judged without the network must not have cost a
	// request -- there is nothing at "not-a-url-at-all" to ask.
	if strings.Contains(res.Feeds[2].Reason, "answered") {
		t.Errorf("a malformed URL was probed: %q", res.Feeds[2].Reason)
	}
}

// The redirect trap, end to end. `openai.com/blog/rss.xml` 307s to
// `/news/rss.xml`: a candidate can look distinct from everything configured right
// up until it is fetched. Dedup has to happen AFTER the probe, or the same
// masthead gets two rows and is double-counted in every source_count it touches.
func TestSuggestSourcesDedupsAfterFollowingRedirects(t *testing.T) {
	db := testDB(t)
	topic := suggestTopic(t, db)
	host := feedHost(t, []string{"/news/rss.xml"}, map[string]string{
		"/blog/rss.xml": "/news/rss.xml",
	})

	// Already configured, at the URL the redirect lands on.
	if _, err := db.CreateSource(store.Source{
		Kind: "rss", Name: "OpenAI",
		Config: json.RawMessage(fmt.Sprintf(`{"url":%q}`, host.URL+"/news/rss.xml")),
		Active: true,
	}); err != nil {
		t.Fatal(err)
	}

	model := suggestModel(t, []map[string]string{
		{"name": "OpenAI blog", "url": host.URL + "/blog/rss.xml"},
	}, nil)

	res, err := SuggestSources(context.Background(), db, clientFor(t, db, model), topic, quietLogger())
	if err != nil {
		t.Fatalf("SuggestSources: %v", err)
	}
	if res.Added != 0 {
		t.Errorf("added %d, want 0: that feed is already configured, one redirect away", res.Added)
	}
	if res.Feeds[0].Status != suggestSkipped {
		t.Errorf("status = %q (%s), want skipped", res.Feeds[0].Status, res.Feeds[0].Reason)
	}
	if n := len(sourceURLs(t, db)); n != 1 {
		t.Errorf("%d source rows, want the 1 that was already there", n)
	}
}

// Two candidates landing on the same feed: one row, not two. Which one wins has to
// be list order rather than whichever probe finished first.
func TestSuggestSourcesCollapsesTwoCandidatesOnOneFeed(t *testing.T) {
	db := testDB(t)
	topic := suggestTopic(t, db)
	host := feedHost(t, []string{"/news/rss.xml"}, map[string]string{
		"/blog/rss.xml": "/news/rss.xml",
		"/feed.xml":     "/news/rss.xml",
	})

	model := suggestModel(t, []map[string]string{
		{"name": "First", "url": host.URL + "/blog/rss.xml"},
		{"name": "Second", "url": host.URL + "/feed.xml"},
	}, nil)

	res, err := SuggestSources(context.Background(), db, clientFor(t, db, model), topic, quietLogger())
	if err != nil {
		t.Fatalf("SuggestSources: %v", err)
	}
	if res.Added != 1 {
		t.Fatalf("added %d, want 1 (%+v)", res.Added, res.Feeds)
	}
	if res.Feeds[0].Status != suggestAdded || res.Feeds[1].Status != suggestSkipped {
		t.Errorf("statuses = %q/%q, want added then skipped in list order",
			res.Feeds[0].Status, res.Feeds[1].Status)
	}
	// Stored where it landed, not where it pointed: a redirect stored as a source
	// is an extra hop on every ingest, forever.
	if _, ok := sourceURLs(t, db)[host.URL+"/news/rss.xml"]; !ok {
		t.Errorf("stored the redirect rather than its destination: %v", sourceURLs(t, db))
	}
}

// Keywords are PROPOSED, never written.
//
// There is exactly one Hacker News row (migration 007) and it is live, so
// appending to it would put the model's guess into tomorrow's run without the
// review the owner asked for. A keyword cannot be paused on its own -- the row is
// the unit of active -- so the click is the review.
func TestSuggestSourcesDoesNotWriteKeywords(t *testing.T) {
	db := testDB(t)
	topic := suggestTopic(t, db)
	host := feedHost(t, []string{"/feed"}, nil)

	hnCfg := `{"queries":[{"query":"existing"}],"firehose_min_points":150}`
	hnID, err := db.CreateSource(store.Source{
		Kind: "hn_search", Name: "Hacker News",
		Config: json.RawMessage(hnCfg), Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	model := suggestModel(t, []map[string]string{
		{"name": "Blog", "url": host.URL + "/feed"},
	}, []map[string]any{
		{"query": "agent harness", "min_points": 20},
		{"query": "existing", "min_points": 30},
		{"query": "MCP", "min_points": 9000},
		{"query": "", "min_points": 5},
	})

	res, err := SuggestSources(context.Background(), db, clientFor(t, db, model), topic, quietLogger())
	if err != nil {
		t.Fatalf("SuggestSources: %v", err)
	}

	// The Hacker News row is untouched, config and all.
	after, err := db.SourceByID(hnID)
	if err != nil {
		t.Fatal(err)
	}
	if string(after.Config) != hnCfg {
		t.Errorf("the Hacker News config changed:\n got %s\nwant %s", after.Config, hnCfg)
	}
	// No second Hacker News row, ever.
	all, _ := db.AllSources()
	hnRows := 0
	for _, s := range all {
		if s.Kind == "hn_search" {
			hnRows++
		}
	}
	if hnRows != 1 {
		t.Errorf("%d Hacker News rows, want 1 — a second undoes migration 007 one keyword at a time", hnRows)
	}

	byQuery := map[string]SuggestedSource{}
	for _, k := range res.Keywords {
		byQuery[k.Query] = k
	}
	if got := byQuery["agent harness"]; got.Status != suggestProposed {
		t.Errorf("new keyword status = %q, want proposed", got.Status)
	}
	if got := byQuery["existing"]; got.Status != suggestSkipped {
		t.Errorf("already-watched keyword status = %q, want skipped", got.Status)
	}
	// Out of range in either direction is a quiet failure: too high silences the
	// keyword and looks like a keyword that simply found no news.
	if got := byQuery["MCP"]; got.MinPoints != maxSuggestedPoints {
		t.Errorf("min_points = %d, want it clamped to %d", got.MinPoints, maxSuggestedPoints)
	}
	if _, ok := byQuery[""]; ok {
		if byQuery[""].Status != suggestRejected {
			t.Errorf("an empty keyword was not rejected; Algolia answers it with the whole of Hacker News")
		}
	}
}

// A feed already on the list costs no request at all: it is decidable before the
// network, and doing it first means a list of duplicates is free.
func TestSuggestSourcesSkipsAlreadyConfiguredFeedsWithoutProbing(t *testing.T) {
	db := testDB(t)
	topic := suggestTopic(t, db)

	var hits int
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write([]byte(suggestFeedBody))
	}))
	defer host.Close()

	if _, err := db.CreateSource(store.Source{
		Kind: "rss", Name: "Already Here",
		Config: json.RawMessage(fmt.Sprintf(`{"url":%q}`, host.URL+"/feed")),
		Active: true,
	}); err != nil {
		t.Fatal(err)
	}

	model := suggestModel(t, []map[string]string{
		{"name": "Already Here", "url": host.URL + "/feed"},
	}, nil)

	res, err := SuggestSources(context.Background(), db, clientFor(t, db, model), topic, quietLogger())
	if err != nil {
		t.Fatalf("SuggestSources: %v", err)
	}
	if res.Feeds[0].Status != suggestSkipped {
		t.Errorf("status = %q, want skipped", res.Feeds[0].Status)
	}
	if hits != 0 {
		t.Errorf("the feed was fetched %d times; a duplicate is decidable without the network", hits)
	}
}

// The model's name is not automatically the publication's name. When it gives
// none, the feed's own title is the honest fallback -- and it is also what the
// owner reads to check the row is who it claims to be.
func TestSuggestSourcesFallsBackToTheFeedsOwnTitle(t *testing.T) {
	db := testDB(t)
	topic := suggestTopic(t, db)
	host := feedHost(t, []string{"/feed"}, nil)

	model := suggestModel(t, []map[string]string{
		{"name": "", "url": host.URL + "/feed"},
	}, nil)

	res, err := SuggestSources(context.Background(), db, clientFor(t, db, model), topic, quietLogger())
	if err != nil {
		t.Fatalf("SuggestSources: %v", err)
	}
	if res.Added != 1 {
		t.Fatalf("added %d, want 1 (%+v)", res.Added, res.Feeds)
	}
	if got := sourceURLs(t, db)[host.URL+"/feed"]; got.Name != "Feed Title" {
		t.Errorf("name = %q, want the feed's own title", got.Name)
	}
}

// Unparseable output is an error the screen reports, not a silent zero. A pass
// that proposed nothing and a pass whose reply could not be read are different
// situations and only one of them is worth pressing the button again for.
func TestSuggestSourcesReportsUnparseableOutput(t *testing.T) {
	db := testDB(t)
	topic := suggestTopic(t, db)

	model := newFakeModel(t, func(string) string { return "Sure! Here are some great feeds for you." })

	_, err := SuggestSources(context.Background(), db, clientFor(t, db, model), topic, quietLogger())
	if err == nil {
		t.Fatal("prose was accepted as a suggestion list")
	}
}

// The parse gate is recorded per model, like every other reply. Completeness is
// deliberately NOT: the prompt asks for "up to" a count, so a model returning four
// good feeds instead of eight has dropped nothing, and recording a soft target as
// a denominator would make every honest reply read as incomplete.
func TestSuggestSourcesRecordsTheParseGateButNoCompleteness(t *testing.T) {
	db := testDB(t)
	topic := suggestTopic(t, db)
	host := feedHost(t, []string{"/feed"}, nil)

	model := suggestModel(t, []map[string]string{
		{"name": "Blog", "url": host.URL + "/feed"},
	}, nil)

	if _, err := SuggestSources(context.Background(), db, clientFor(t, db, model), topic, quietLogger()); err != nil {
		t.Fatalf("SuggestSources: %v", err)
	}

	rows, err := db.ModelReliabilityOver(1)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Role != "suggest" {
			continue
		}
		if r.ParseRate != 1 {
			t.Errorf("parse rate %v, want 1", r.ParseRate)
		}
		if r.Completeness != -1 {
			t.Errorf("completeness %v, want the -1 sentinel: 'up to N' is not a coverage requirement",
				r.Completeness)
		}
		return
	}
	t.Fatalf("no 'suggest' row in the cost log; rows: %+v", rows)
}

// Guard rail on the prompt side: the definition is what the model reasons from, so
// it has to actually reach the prompt -- and be cut without splitting a rune, per
// §9's Vietnamese definitions.
func TestSuggestPromptCarriesTheDefinition(t *testing.T) {
	long := strings.Repeat("cải tiến mô hình ngôn ngữ ", 200)
	got := suggestPrompt(store.Topic{Title: "Mô hình", Definition: long})

	if !strings.Contains(got, "Mô hình") {
		t.Error("the title is not in the prompt")
	}
	if !strings.Contains(got, "cải tiến") {
		t.Error("the definition is not in the prompt")
	}
	if len(got) > maxDefinitionChars+600 {
		t.Errorf("prompt is %d bytes; the definition should have been truncated", len(got))
	}
	if strings.ContainsRune(got, '�') {
		t.Error("truncation split a rune — use sources.Truncate")
	}
	_ = sources.Truncate
}
