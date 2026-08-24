package sources

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const atomFeed = `<?xml version="1.0"?><rss version="2.0"><channel>
  <title>The Real Publication</title>
  <item><title>A story</title><link>https://x.test/1</link><description>Body.</description></item>
  <item><title>Another</title><link>https://x.test/2</link><description>Body.</description></item>
</channel></rss>`

// The reason ProbeFeed exists rather than reusing RSSFetcher: a candidate URL and
// the feed it actually serves are not always the same URL.
//
// `openai.com/blog/rss.xml` 307s to `/news/rss.xml`. Both are "the OpenAI feed",
// and a caller that dedups on what it was given would happily add the second row
// for a masthead it already has -- which double-counts that outlet in every
// source_count it touches, and source_count is what makes a star gold.
func TestProbeFeedReportsWhereRedirectsLanded(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/blog/rss.xml" {
			http.Redirect(w, r, srv.URL+"/news/rss.xml", http.StatusTemporaryRedirect)
			return
		}
		w.Header().Set("Content-Type", "application/rss+xml")
		w.Write([]byte(atomFeed))
	}))
	defer srv.Close()

	got, err := ProbeFeed(context.Background(), srv.URL+"/blog/rss.xml")
	if err != nil {
		t.Fatalf("ProbeFeed: %v", err)
	}
	if want := srv.URL + "/news/rss.xml"; got.FinalURL != want {
		t.Errorf("FinalURL = %q, want %q", got.FinalURL, want)
	}
	if len(got.Items) != 2 {
		t.Errorf("got %d items, want 2", len(got.Items))
	}
	if got.Title != "The Real Publication" {
		t.Errorf("Title = %q; the feed's own name is the check on whether it is the publication it claimed", got.Title)
	}
}

// The trap the seeding convention exists for, and the most common shape of a
// hallucinated feed URL: a real page, a 200, and no feed anywhere in it.
func TestProbeFeedRefusesAPageThatIsNotAFeed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<!doctype html><html><head><title>A blog</title></head><body>Hi</body></html>`))
	}))
	defer srv.Close()

	_, err := ProbeFeed(context.Background(), srv.URL+"/rss.xml")
	if err == nil {
		t.Fatal("a 200 HTML page was accepted as a feed")
	}
	if !strings.Contains(err.Error(), "is not a feed") {
		t.Errorf("error %q does not say what is wrong in words the owner can act on", err)
	}
}

// A feed that answers and carries nothing is NOT an error. It is a finding, and
// the caller decides what to do about it -- which is the whole difference between
// "the URL is wrong" and "the URL is right and the feed is empty".
func TestProbeFeedReportsAnEmptyFeedWithoutFailing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<?xml version="1.0"?><rss version="2.0"><channel>
		  <title>Nothing Here Yet</title></channel></rss>`))
	}))
	defer srv.Close()

	got, err := ProbeFeed(context.Background(), srv.URL+"/feed")
	if err != nil {
		t.Fatalf("an empty feed must not be an error: %v", err)
	}
	if len(got.Items) != 0 {
		t.Errorf("got %d items, want 0", len(got.Items))
	}
	if got.Title != "Nothing Here Yet" {
		t.Errorf("Title = %q, want the feed's title", got.Title)
	}
}

func TestProbeFeedReportsANon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer srv.Close()

	got, err := ProbeFeed(context.Background(), srv.URL+"/feed")
	if err == nil {
		t.Fatal("a 404 was accepted")
	}
	// The URL still comes back: the caller wants to show where it looked.
	if got.FinalURL == "" {
		t.Error("no FinalURL reported for a failed probe")
	}
}
