package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"regexp"
	"strings"

	"github.com/mmcdole/gofeed"
)

// RSSConfig is the sources.config shape for kind "rss".
type RSSConfig struct {
	URL string `json:"url"`
}

// RSSFetcher reads any RSS, Atom, or JSON feed.
type RSSFetcher struct{}

func (RSSFetcher) Fetch(ctx context.Context, cfg json.RawMessage, _ Options) ([]Item, error) {
	var c RSSConfig
	if err := json.Unmarshal(cfg, &c); err != nil {
		return nil, fmt.Errorf("rss config: %w", err)
	}
	if c.URL == "" {
		return nil, fmt.Errorf("rss config: url is required")
	}
	return parseFeed(ctx, c.URL, "")
}

// YouTubeConfig is the sources.config shape for kind "youtube_rss". The kind
// keeps its name for migration reasons (see youtube.go); `url` is now only a
// legacy carrier of the channel id, which is read back out of it.
type YouTubeConfig struct {
	ChannelID string `json:"channel_id"`
	URL       string `json:"url"`
}

func parseFeed(ctx context.Context, url, detail string) ([]Item, error) {
	fp := gofeed.NewParser()
	fp.Client = DefaultClient

	feed, err := fp.ParseURLWithContext(url, ctx)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", url, err)
	}
	return normalizeFeed(feed, detail), nil
}

// normalizeFeed turns a parsed feed into items.
//
// Split out of parseFeed so ProbeFeed can share it: a probe does its own HTTP
// request (it has to, to see where redirects landed) and must still produce items
// through exactly the same normalization, or "verified through the real fetcher"
// stops being true of the number it reports.
func normalizeFeed(feed *gofeed.Feed, detail string) []Item {
	items := make([]Item, 0, len(feed.Items))
	for _, e := range feed.Items {
		if e.Link == "" || e.Title == "" {
			continue // nothing to link to, or nothing to classify
		}
		item := Item{
			URL:     e.Link,
			Title:   strings.TrimSpace(html.UnescapeString(e.Title)),
			Summary: truncate(cleanText(firstNonEmpty(e.Description, e.Content)), maxSummary),
			Detail:  detail,
		}
		if e.PublishedParsed != nil {
			item.PublishedAt = *e.PublishedParsed
		} else if e.UpdatedParsed != nil {
			item.PublishedAt = *e.UpdatedParsed
		}
		items = append(items, item)
	}
	return items
}

var (
	tagRE = regexp.MustCompile(`<[^>]*>`)
	wsRE  = regexp.MustCompile(`\s+`)
	// Tags become spaces so "<p>a</p><p>b</p>" doesn't run together, which
	// leaves a gap before any punctuation that followed an inline tag.
	spaceBeforePunctRE = regexp.MustCompile(`\s+([,.;:!?)\]])`)
)

// cleanText flattens feed HTML into something worth sending to a classifier.
// The model is being asked to judge relevance, and markup is pure token cost.
func cleanText(s string) string {
	s = tagRE.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	s = wsRE.ReplaceAllString(s, " ")
	s = spaceBeforePunctRE.ReplaceAllString(s, "$1")
	return strings.TrimSpace(s)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
