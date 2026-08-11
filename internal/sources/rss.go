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

// YouTubeConfig is the sources.config shape for kind "youtube_rss". Either a
// channel_id (turned into the standard feed URL) or an explicit url.
type YouTubeConfig struct {
	ChannelID string `json:"channel_id"`
	URL       string `json:"url"`
}

// YouTubeFetcher reads a channel's Atom feed. YouTube's feed is ordinary Atom,
// so this is the RSS path with URL construction in front. Transcripts and video
// summaries are explicitly Phase 1 (§2).
type YouTubeFetcher struct{}

func (YouTubeFetcher) Fetch(ctx context.Context, cfg json.RawMessage, _ Options) ([]Item, error) {
	var c YouTubeConfig
	if err := json.Unmarshal(cfg, &c); err != nil {
		return nil, fmt.Errorf("youtube config: %w", err)
	}
	url := c.URL
	if url == "" {
		if c.ChannelID == "" {
			return nil, fmt.Errorf("youtube config: channel_id or url is required")
		}
		url = "https://www.youtube.com/feeds/videos.xml?channel_id=" + c.ChannelID
	}
	return parseFeed(ctx, url, "video")
}

func parseFeed(ctx context.Context, url, detail string) ([]Item, error) {
	fp := gofeed.NewParser()
	fp.Client = DefaultClient

	feed, err := fp.ParseURLWithContext(url, ctx)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", url, err)
	}

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
	return items, nil
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
