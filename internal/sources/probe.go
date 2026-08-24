package sources

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/mmcdole/gofeed"
)

// maxFeedBytes caps how much of a probed URL is read.
//
// A probe runs against a URL nobody has vetted -- in the suggestion path, one a
// model recalled and may have invented -- so the usual reason for a cap applies
// doubly: a URL that streams forever would otherwise hold the request open until
// the timeout, once per candidate.
const maxFeedBytes = 4 << 20

// FeedProbe is what a candidate feed turned out to be.
type FeedProbe struct {
	// FinalURL is where the request actually landed after redirects, which is not
	// always where it was pointed. `openai.com/blog/rss.xml` 307s to
	// `/news/rss.xml`: two URLs, one feed, and adding both would double-count
	// OpenAI in every source_count that touches it. Dedup on this, never on the
	// URL that was proposed.
	FinalURL string
	// Title is the feed's own name, which is the only trustworthy answer to "is
	// this the publication I think it is". The channel-ID trap is the cautionary
	// case: a scraped id served someone else's channel entirely, and the feed's
	// own title is what would have caught it.
	Title string
	Items []Item
}

// ProbeFeed fetches one candidate URL and reports what it really is.
//
// This is the enforcement point for "never seed a source URL you have not run
// through the real fetcher". It exists rather than reusing RSSFetcher because a
// probe needs two things the Fetcher interface deliberately does not carry: where
// redirects landed, and the feed's own title. Parsing and item normalization are
// shared with the fetcher, so the item count a probe reports is the count a run
// would get.
//
// A feed that answers 200 and yields zero items is NOT an error here. It is a
// finding, and the caller's job is to park it rather than accept it -- that exact
// case ("200 in a browser, zero items in the parser") is why this function exists.
func ProbeFeed(ctx context.Context, rawURL string) (FeedProbe, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return FeedProbe{}, fmt.Errorf("bad URL: %w", err)
	}
	// Identifying honestly, same as FetchPage: a publisher serving a challenge
	// page to an unknown agent is likelier to answer a named reader than a
	// pretend browser, and it is what a well-behaved client does.
	req.Header.Set("User-Agent", "Interstellar/0.1 (personal news reader)")
	req.Header.Set("Accept", "application/rss+xml, application/atom+xml, application/xml, text/xml, application/json")

	resp, err := DefaultClient.Do(req)
	if err != nil {
		return FeedProbe{}, err
	}
	defer resp.Body.Close()

	// resp.Request is the LAST request in the redirect chain, which is the whole
	// reason this is a hand-rolled request rather than gofeed's ParseURL.
	final := rawURL
	if resp.Request != nil && resp.Request.URL != nil {
		final = resp.Request.URL.String()
	}
	probe := FeedProbe{FinalURL: final}

	if resp.StatusCode >= 400 {
		return probe, fmt.Errorf("%s answered %s", final, resp.Status)
	}

	feed, err := gofeed.NewParser().Parse(io.LimitReader(resp.Body, maxFeedBytes))
	if err != nil {
		// The common shape of a hallucinated or wrong URL: a real page, served
		// with a 200, that is simply not a feed. Say so plainly -- "is not a
		// feed" is actionable, an XML parser's complaint about line 1 is not.
		return probe, fmt.Errorf("%s is not a feed: %w", final, err)
	}

	probe.Title = strings.TrimSpace(feed.Title)
	probe.Items = normalizeFeed(feed, "")
	return probe, nil
}
