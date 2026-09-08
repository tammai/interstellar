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
	"strings"
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

	// YouTubeAPIKey is the resolved key VALUE, read from the environment by the
	// caller at run time. Options carries it for the same reason it carries
	// HNMinPoints: a fetcher needs it and has no business reading settings. Empty
	// means unset, which YouTubeFetcher reports by env var name.
	YouTubeAPIKey string
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

// Truncate cuts a string to at most n BYTES, never mid-rune.
//
// Exported because the pipeline needs it too, and a second copy would drift. The
// naive s[:n] is not a nitpick here: on Vietnamese text -- which §9 keeps topic
// definitions in, and which Vietnamese feeds carry -- 27% of byte offsets fall
// inside a multi-byte rune. json.Marshal does not error on that, it silently
// substitutes U+FFFD, so the damage arrives as a mojibake character on screen and
// in the database rather than as a failure anyone would notice.
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// strings.ToValidUTF8 drops the partial rune the cut may have created. Stdlib
	// rather than walking backwards by hand: this project had four hand-rolled
	// copies of that walk, which is three too many.
	return strings.ToValidUTF8(s[:n], "")
}

func truncate(s string, n int) string { return Truncate(s, n) }
