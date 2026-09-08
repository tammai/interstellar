package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// YouTube video discovery, over the Data API rather than Atom.
//
// YouTube retired its public RSS feeds. Confirmed 2026-09-08, and worth writing
// down because the symptom invites the wrong diagnosis: every
// `/feeds/videos.xml?channel_id=…` request answers **404**, from a response whose
// own `server:` header reads "YouTube RSS Feeds server" and whose body is a
// genuine Google "that's an error". The `?playlist_id=` and `?user=` forms 404
// too, while the same URL with **no parameters at all returns 400** -- so the
// route is live and still validating query strings, it simply resolves no id for
// anybody. A network or IP block would not draw that distinction, and channel
// pages answer 200 throughout.
//
// The wrong diagnosis is "our channel ids are stale", because nine sources broke
// at once and this repo already warns that a scraped channel id can serve someone
// else's channel. It is not the ids: a known-stable channel (Google Developers)
// 404s identically, with a browser User-Agent, from a working network. Do not
// spend an afternoon re-resolving ids.
//
// The kind stays `youtube_rss`. Migration 001 pins
// `CHECK (kind IN ('rss','hn_search','youtube_rss'))` and SQLite cannot widen a
// CHECK without a table rebuild that `items.source_id` blocks, so a new kind
// would be a wall for no gain -- what changed is how the kind is fetched, not
// what it means. Existing rows keep working untouched.

// youtubeAPIBase is a var, not a const, only so tests can point it at a stub --
// the same reason DefaultClient is one. Nothing in the app reassigns it.
var youtubeAPIBase = "https://www.googleapis.com/youtube/v3/playlistItems"

// youtubeMaxResults is one page. A channel posting more than 50 videos between
// two daily runs is not a case worth paging for, and ingest dedupes anyway.
const youtubeMaxResults = 50

// YouTubeFetcher lists a channel's recent uploads through the Data API.
//
// Discovery only. Transcripts are a separate stage and a separate breakage: the
// signed caption baseUrl returns an empty body, so on this instance the
// transcript stage has never once succeeded (0 ok against 144 attempts, measured
// 2026-09-08). That turned out not to matter for the thing YouTube is here for --
// its items still matched a topic 56% of the time on title and description
// alone, roughly double Hacker News -- which is why discovery was worth restoring
// on its own.
type YouTubeFetcher struct{}

func (YouTubeFetcher) Fetch(ctx context.Context, cfg json.RawMessage, opts Options) ([]Item, error) {
	var c YouTubeConfig
	if err := json.Unmarshal(cfg, &c); err != nil {
		return nil, fmt.Errorf("youtube config: %w", err)
	}

	channelID := c.ChannelID
	if channelID == "" {
		// Rows created before the API move carry only the old feed URL. Reading the
		// id back out of it means no data migration and no hand-edited config: the
		// id was always the only part of that URL that mattered.
		channelID = channelIDFromFeedURL(c.URL)
	}
	if channelID == "" {
		return nil, fmt.Errorf("youtube config: channel_id is required (url %q carries none)", c.URL)
	}

	if opts.YouTubeAPIKey == "" {
		// Named, not just "missing": the whole point of the env-var-name contract is
		// that the error tells you which variable to set.
		return nil, fmt.Errorf("youtube: no API key -- set the variable named in Settings (default YOUTUBE_API_KEY)")
	}

	// A channel's uploads live in a playlist whose id is its channel id with the
	// "UC" prefix swapped for "UU". This is stable and documented, and it is what
	// makes one cheap playlistItems.list call enough -- no search.list, which costs
	// 100 quota units against playlistItems.list's 1.
	uploads, err := uploadsPlaylistID(channelID)
	if err != nil {
		return nil, fmt.Errorf("youtube: %w", err)
	}

	q := url.Values{}
	q.Set("part", "snippet")
	q.Set("playlistId", uploads)
	q.Set("maxResults", fmt.Sprint(youtubeMaxResults))
	q.Set("key", opts.YouTubeAPIKey)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, youtubeAPIBase+"?"+q.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("youtube: %w", err)
	}

	resp, err := DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("youtube %s: %w", channelID, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("youtube %s: read: %w", channelID, err)
	}
	if resp.StatusCode != http.StatusOK {
		// The key never reaches this message: it is in the query string, not the
		// body, and youtubeError reads only the body.
		return nil, fmt.Errorf("youtube %s: http %d: %s", channelID, resp.StatusCode, youtubeError(body))
	}

	var payload struct {
		Items []struct {
			Snippet struct {
				Title       string `json:"title"`
				Description string `json:"description"`
				PublishedAt string `json:"publishedAt"`
				ResourceID  struct {
					VideoID string `json:"videoId"`
				} `json:"resourceId"`
			} `json:"snippet"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("youtube %s: parse: %w", channelID, err)
	}

	out := make([]Item, 0, len(payload.Items))
	for _, it := range payload.Items {
		sn := it.Snippet
		if sn.ResourceID.VideoID == "" {
			continue // a deleted or private upload still occupies a playlist slot
		}
		// Deleted uploads keep a slot and report themselves as such rather than
		// disappearing. Letting one through would put a permanently empty item in
		// front of the classifier every run.
		if sn.Title == "Deleted video" || sn.Title == "Private video" {
			continue
		}
		published, _ := time.Parse(time.RFC3339, sn.PublishedAt)
		out = append(out, Item{
			URL:   "https://www.youtube.com/watch?v=" + sn.ResourceID.VideoID,
			Title: strings.TrimSpace(sn.Title),
			// The description is what the classifier actually reads, since
			// transcripts write into items.summary and currently never arrive.
			Summary:     Truncate(strings.TrimSpace(sn.Description), maxYouTubeSummary),
			PublishedAt: published,
			Detail:      "video",
		})
	}
	return out, nil
}

// maxYouTubeSummary bounds a description. Some channels paste an entire episode's
// show notes, chapter list and sponsor block into it; the classifier reads a
// prefix regardless, and an unbounded one distorts every batch it lands in.
const maxYouTubeSummary = 4000

// uploadsPlaylistID converts a channel id to its uploads playlist id.
func uploadsPlaylistID(channelID string) (string, error) {
	if !strings.HasPrefix(channelID, "UC") || len(channelID) < 3 {
		return "", fmt.Errorf("channel id %q is not of the form UC…", channelID)
	}
	return "UU" + channelID[2:], nil
}

// channelIDFromFeedURL recovers the channel id from a retired feed URL, so rows
// written before the API move need no migration.
func channelIDFromFeedURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Query().Get("channel_id")
}

// youtubeError pulls the sentence out of a Data API error body.
//
// Same reasoning as llm.snippet: the API answers `{"error":{"message":…}}`, and
// the message is the only part that names what to do ("API key not valid",
// "quota exceeded", "playlist not found"). Falls back to the raw body, because a
// proxy or a wrong host answers with something else entirely and its first line
// is the clue.
func youtubeError(body []byte) string {
	var env struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err == nil && env.Error.Message != "" {
		return env.Error.Message
	}
	return Truncate(strings.TrimSpace(string(body)), 300)
}
