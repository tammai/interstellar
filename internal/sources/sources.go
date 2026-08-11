// Package sources fetches and normalizes items from the three feed kinds Phase 0
// supports (§ decision 6): RSS/Atom, Hacker News search, and YouTube channel feeds.
//
// Fetchers are pure: they take a source config and return normalized items. They
// do not touch the database, which keeps them trivially testable against an
// httptest server and keeps all writes on one goroutine.
package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Item is one fetched entry, normalized across feed kinds and not yet persisted.
type Item struct {
	URL         string // raw; the ingester canonicalizes
	Title       string
	Summary     string
	PublishedAt time.Time // zero if the feed didn't say

	// Detail is a per-source annotation shown in the panel's "Seen on" list,
	// e.g. "412 points". It is attribution, not content.
	Detail string
}

// Options carries settings a fetcher may need beyond its own config.
type Options struct {
	HNMinPoints int // decision #4: 25
}

// Fetcher turns a source's JSON config into items.
type Fetcher interface {
	Fetch(ctx context.Context, cfg json.RawMessage, opts Options) ([]Item, error)
}

// FetchTimeout bounds any single source. One slow feed must not hold up a run.
const FetchTimeout = 20 * time.Second

// DefaultClient is the HTTP client every fetcher uses.
var DefaultClient = &http.Client{Timeout: FetchTimeout}

// Registry maps sources.kind to its fetcher. The CHECK constraint on
// sources.kind and this map must agree.
var Registry = map[string]Fetcher{
	"rss":         RSSFetcher{},
	"youtube_rss": YouTubeFetcher{},
	"hn_search":   HNFetcher{},
}

// For returns the fetcher for a source kind.
func For(kind string) (Fetcher, error) {
	f, ok := Registry[kind]
	if !ok {
		return nil, fmt.Errorf("unknown source kind %q", kind)
	}
	return f, nil
}

// maxSummary caps stored summaries. The classifier truncates further; this just
// stops a pathological feed from writing a novel into every row.
const maxSummary = 1000

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Trim to a rune boundary so we never store a broken UTF-8 sequence.
	for n > 0 && !utf8Start(s[n]) {
		n--
	}
	return s[:n]
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
