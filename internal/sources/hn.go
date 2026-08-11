package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// HNSearchBase is the Algolia endpoint. Overridable so tests can point at an
// httptest server.
var HNSearchBase = "https://hn.algolia.com/api/v1/search_by_date"

// HNConfig is the sources.config shape for kind "hn_search".
type HNConfig struct {
	Query string `json:"query"`
	// MinPoints overrides the global threshold for this one query. Zero means
	// use the global setting.
	MinPoints int `json:"min_points,omitempty"`
}

// HNFetcher searches Hacker News by date, filtered on points. The threshold is
// decision #4 (25, not 10): a low bar floods the sky with noise that the
// classifier then has to pay to reject.
type HNFetcher struct{}

type hnResponse struct {
	Hits []hnHit `json:"hits"`
}

type hnHit struct {
	ObjectID    string `json:"objectID"`
	Title       string `json:"title"`
	URL         string `json:"url"`
	Points      int    `json:"points"`
	StoryText   string `json:"story_text"`
	CreatedAtI  int64  `json:"created_at_i"`
	NumComments int    `json:"num_comments"`
}

func (HNFetcher) Fetch(ctx context.Context, cfg json.RawMessage, opts Options) ([]Item, error) {
	var c HNConfig
	if err := json.Unmarshal(cfg, &c); err != nil {
		return nil, fmt.Errorf("hn config: %w", err)
	}
	if strings.TrimSpace(c.Query) == "" {
		return nil, fmt.Errorf("hn config: query is required")
	}

	minPoints := c.MinPoints
	if minPoints <= 0 {
		minPoints = opts.HNMinPoints
	}
	if minPoints <= 0 {
		minPoints = 25
	}

	// Only look back a few days: the pipeline runs daily, and the sky only
	// shows 7 days, so anything older is already someone else's problem.
	since := time.Now().Add(-72 * time.Hour).Unix()

	q := url.Values{}
	q.Set("query", c.Query)
	q.Set("tags", "story")
	q.Set("hitsPerPage", "50")
	q.Set("numericFilters", fmt.Sprintf("points>=%d,created_at_i>%d", minPoints, since))

	endpoint := HNSearchBase + "?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("hn request: %w", err)
	}

	resp, err := DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("hn search %q: %w", c.Query, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("hn search %q: status %d", c.Query, resp.StatusCode)
	}

	var body hnResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("hn search %q: decode: %w", c.Query, err)
	}

	items := make([]Item, 0, len(body.Hits))
	for _, h := range body.Hits {
		if h.Title == "" {
			continue
		}
		link := h.URL
		if link == "" {
			// Ask HN / Show HN text posts have no outbound URL; the thread is
			// the artifact.
			link = "https://news.ycombinator.com/item?id=" + h.ObjectID
		}
		item := Item{
			URL:     link,
			Title:   strings.TrimSpace(h.Title),
			Summary: truncate(cleanText(h.StoryText), maxSummary),
			// Shown in the panel's "Seen on" list, mirroring the mock's
			// "Hacker News (412 pts)".
			Detail: strconv.Itoa(h.Points) + " points",
		}
		if h.CreatedAtI > 0 {
			item.PublishedAt = time.Unix(h.CreatedAtI, 0).UTC()
		}
		items = append(items, item)
	}
	return items, nil
}
