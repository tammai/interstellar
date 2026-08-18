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
//
// One source, many queries. The single-query fields are the ORIGINAL shape and
// still parse, because config is an unmigrated JSON blob per row and a hand-added
// source in the wild may still carry it -- Queries() folds either shape into one
// list.
//
// The reason this is one source rather than one per keyword is not tidiness: it
// is signals.source_count. That count is `count(DISTINCT source_id)` over
// item_sources, and it decides the gold "hot" flag (§6, >= 3) -- so with a row
// per keyword, one Hacker News story matching three of them presented as three
// independent outlets carrying it. Measured on a real instance: 25 of 31 gold
// stars were gold on Hacker News alone, one story counted seven times. Hacker
// News is one outlet, so it gets one row and contributes one to the count.
type HNConfig struct {
	Queries []HNQuery `json:"queries,omitempty"`

	// FirehoseMinPoints adds one keyword-less pass over everything Hacker News
	// carried above that score. Zero switches it off.
	//
	// It exists because keywords can only find vocabulary someone thought of, and
	// the big story nobody anticipated has no keyword. It is deliberately set HIGH
	// where the keywords are set low, because the two cover opposite failures:
	// measured over 30 days, a firehose at 50 points sees only 7% of the
	// AI-shaped stories on HN (the median such story scores 3), while dropping the
	// bar to 1 means reading 1,008 stories a day of which ~95% are not about AI at
	// all. High bar plus precise keywords is the only combination that is both
	// affordable and complete.
	FirehoseMinPoints int `json:"firehose_min_points,omitempty"`

	// MinPoints is the source-level floor a query with no bar of its own falls
	// back to, before the global setting. It is ALSO the original single-query
	// shape's threshold; both readings agree, since a legacy row has one query.
	MinPoints int `json:"min_points,omitempty"`

	// The original single-query shape, kept parsing. Do not write this.
	Query string `json:"query,omitempty"`
}

// HNQuery is one keyword and the bar it has to clear.
//
// Zero means fall back to the source default, then the global setting -- which is
// the normal case now. A per-keyword bar was once how noise was controlled, and
// that turned out to be treating the symptom: the noise came from Algolia's typo
// tolerance, not from unpopular stories (see hnFetch). With typo tolerance off,
// points stop correlating with junk and the bar should be LOW, because a model
// release rarely trends -- "DeepSeek announced to raise its API price" scored 32,
// and 1,435 of 1,549 topic-shaped stories in 30 days sat under 50 points.
type HNQuery struct {
	Query     string `json:"query"`
	MinPoints int    `json:"min_points,omitempty"`
}

// Normalized folds either config shape into the list the fetcher runs. Blank
// queries are dropped rather than sent: Algolia answers an empty query with the
// whole of Hacker News, and asking for that is what FirehoseMinPoints is for --
// deliberately, with a bar, never by accident through an empty keyword box.
func (c HNConfig) Normalized() []HNQuery {
	out := make([]HNQuery, 0, len(c.Queries)+1)
	for _, q := range c.Queries {
		if strings.TrimSpace(q.Query) == "" {
			continue
		}
		q.Query = strings.TrimSpace(q.Query)
		out = append(out, q)
	}
	// The original shape. Appended rather than preferred, so a row carrying both
	// keeps every keyword instead of silently dropping one set.
	if strings.TrimSpace(c.Query) != "" {
		out = append(out, HNQuery{Query: strings.TrimSpace(c.Query), MinPoints: c.MinPoints})
	}
	return out
}

// HNFetcher searches Hacker News by date, filtered on points. The threshold is
// decision #4 (25, not 10): a low bar floods the sky with noise that the
// classifier then has to pay to reject.
type HNFetcher struct{}

type hnResponse struct {
	Hits    []hnHit `json:"hits"`
	NbPages int     `json:"nbPages"`
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
	queries := c.Normalized()
	if len(queries) == 0 && c.FirehoseMinPoints <= 0 {
		return nil, fmt.Errorf("hn config: at least one query, or firehose_min_points, is required")
	}

	// Per-PASS try/continue, mirroring the per-source rule in §10: one keyword
	// that errors must not cost the others their morning. The run is only
	// reported as failed when every pass failed -- that is a dead source, and
	// last_run_at should not advance for it.
	var (
		items    []Item
		byURL    = map[string]int{}
		failures []string
		ok, runs int
	)
	collect := func(hits []Item) {
		for _, it := range hits {
			// The same story routinely matches several keywords, and the firehose
			// re-finds what a keyword already found. It is ONE story from ONE
			// outlet, so it collapses here.
			if i, seen := byURL[it.URL]; seen {
				items[i] = mergeHNItem(items[i], it)
				continue
			}
			byURL[it.URL] = len(items)
			items = append(items, it)
		}
	}

	for _, q := range queries {
		runs++
		hits, err := hnSearch(ctx, q, c.MinPoints, opts)
		if err != nil {
			failures = append(failures, err.Error())
			continue
		}
		ok++
		collect(hits)
	}

	if c.FirehoseMinPoints > 0 {
		runs++
		hits, err := hnFirehose(ctx, c.FirehoseMinPoints)
		if err != nil {
			failures = append(failures, err.Error())
		} else {
			ok++
			collect(hits)
		}
	}

	if ok == 0 {
		return nil, fmt.Errorf("hn search: all %d passes failed: %s",
			runs, Truncate(strings.Join(failures, "; "), 300))
	}
	return items, nil
}

// mergeHNItem keeps the better copy when two keywords return the same story.
//
// Points climb during the day and the two responses can disagree; the higher
// number is the later reading. Summary is kept when one copy has one and the
// other does not -- Algolia omits story_text on some hits.
func mergeHNItem(existing, incoming Item) Item {
	if hnPoints(incoming.Detail) > hnPoints(existing.Detail) {
		existing.Detail = incoming.Detail
	}
	if existing.Summary == "" {
		existing.Summary = incoming.Summary
	}
	if existing.PublishedAt.IsZero() {
		existing.PublishedAt = incoming.PublishedAt
	}
	return existing
}

// hnPoints reads the count back out of a "412 points" annotation. Parsing the
// display string keeps Item free of a field only one fetcher would ever set.
func hnPoints(detail string) int {
	n, err := strconv.Atoi(strings.TrimSuffix(detail, " points"))
	if err != nil {
		return 0
	}
	return n
}

// hnSearch runs one keyword. sourceDefault is the source-level floor, applied
// when the query does not state its own.
func hnSearch(ctx context.Context, q HNQuery, sourceDefault int, opts Options) ([]Item, error) {
	minPoints := q.MinPoints
	if minPoints <= 0 {
		minPoints = sourceDefault
	}
	if minPoints <= 0 {
		minPoints = opts.HNMinPoints
	}
	if minPoints <= 0 {
		minPoints = 25
	}

	params := url.Values{}
	params.Set("query", q.Query)
	// TYPO TOLERANCE OFF. This is the single most consequential parameter here.
	// Algolia is fuzzy by default, so short terms match almost the whole corpus:
	// measured over 30 days, "MoE" returned 554 stories with NONE containing the
	// term, "xAI" 520 at 2%, "RAG" 275 at 0%, and "GLM" matched Gmail while the
	// actual GLM-5.3 release sat buried among 126 junk hits. With it off the same
	// queries return 2-4 stories each and nearly all of them are on-term.
	//
	// Not paired with restrictSearchableAttributes=title, deliberately: that would
	// reach 100% precision by dropping every story that names the model in its
	// body, and a "Show HN: ... using open-weight models" post is exactly the kind
	// of thing this is meant to find.
	params.Set("typoTolerance", "false")
	return hnFetch(ctx, params, minPoints, fmt.Sprintf("query %q", q.Query))
}

// hnFirehose runs one keyword-less pass: everything above a score, whatever it is
// about. The classifier is the relevance gate for it.
func hnFirehose(ctx context.Context, minPoints int) ([]Item, error) {
	return hnFetch(ctx, url.Values{}, minPoints, fmt.Sprintf("firehose >=%d points", minPoints))
}

// hnFetch runs one Algolia pass, following pages until it has everything in the
// window or hits the page cap.
//
// Paging is not optional at these volumes. One page is 100 hits and the window is
// 72 hours; a firehose at 100 points is ~41 stories a day, so a single page would
// silently return only the most recent day and a half and call it a full result.
// search_by_date sorts newest first, so truncation always loses the OLDEST -- the
// stories a missed run most needed to catch up on.
func hnFetch(ctx context.Context, params url.Values, minPoints int, label string) ([]Item, error) {
	// Only look back a few days: the pipeline runs daily, and the sky only
	// shows 7 days, so anything older is already someone else's problem.
	since := time.Now().Add(-72 * time.Hour).Unix()

	params.Set("tags", "story")
	params.Set("hitsPerPage", strconv.Itoa(hnPageSize))
	params.Set("numericFilters", fmt.Sprintf("points>=%d,created_at_i>%d", minPoints, since))

	var items []Item
	for page := 0; page < hnMaxPages; page++ {
		params.Set("page", strconv.Itoa(page))
		body, err := hnGet(ctx, params, label)
		if err != nil {
			return nil, err
		}
		items = append(items, hnItems(body.Hits)...)
		// NbPages is what Algolia says it has; stopping on a short page as well
		// covers a response that omits it.
		if len(body.Hits) < hnPageSize || page+1 >= body.NbPages {
			break
		}
	}
	return items, nil
}

// hnPageSize is Algolia's per-request maximum that HN's proxy honours reliably.
// hnMaxPages bounds a runaway: 5 pages is 500 stories per pass, far above the
// ~120 a 72-hour firehose produces, and a pass wanting more than that is a
// misconfiguration rather than a day's news.
const (
	hnPageSize = 100
	hnMaxPages = 5
)

func hnGet(ctx context.Context, params url.Values, label string) (hnResponse, error) {
	endpoint := HNSearchBase + "?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return hnResponse{}, fmt.Errorf("hn request: %w", err)
	}
	resp, err := DefaultClient.Do(req)
	if err != nil {
		return hnResponse{}, fmt.Errorf("hn %s: %w", label, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return hnResponse{}, fmt.Errorf("hn %s: status %d", label, resp.StatusCode)
	}
	var body hnResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return hnResponse{}, fmt.Errorf("hn %s: decode: %w", label, err)
	}
	return body, nil
}

func hnItems(hits []hnHit) []Item {
	items := make([]Item, 0, len(hits))
	for _, h := range hits {
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
	return items
}
