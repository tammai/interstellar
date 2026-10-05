package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"

	"github.com/tammai/interstellar/internal/llm"
	"github.com/tammai/interstellar/internal/sources"
	"github.com/tammai/interstellar/internal/store"
)

// How much the model is allowed to propose in one pass.
//
// Bounded because every feed costs a probe -- a real HTTP request against a URL
// nobody has vetted -- and because a list longer than this stops being something
// the owner reads and starts being something they skim past.
const (
	maxSuggestedFeeds    = 8
	maxSuggestedKeywords = 6
	// suggestProbeWorkers bounds concurrent probes, mirroring the import path's
	// verification pool. Read-only and off the database entirely, so it cannot
	// contend with the single connection.
	suggestProbeWorkers = 6
	// maxDefinitionChars caps what goes into the prompt. A topic definition is
	// prose the owner wrote and has no length limit of its own.
	maxDefinitionChars = 1200
)

// Hacker News keyword bars. The floor is low on purpose: the median AI-shaped HN
// story scores 3, and a high bar throws away exactly the model releases a keyword
// exists to catch. The ceiling stops a model that has decided everything needs
// 500 points from silencing a keyword before the owner ever sees it work.
const (
	minSuggestedPoints = 5
	maxSuggestedPoints = 150
	// defaultSuggestedPoints is used when the model gives no bar or an unusable
	// one. Low, per the convention: precision comes from typo tolerance being off,
	// not from popularity.
	defaultSuggestedPoints = 10
)

// Outcome statuses. Four rather than the import path's three, because a keyword
// and a feed do not end the same way -- see SuggestSources.
const (
	suggestAdded    = "added"    // a feed row was written, paused
	suggestProposed = "proposed" // a keyword, waiting on one click
	suggestSkipped  = "skipped"  // already configured here
	suggestRejected = "rejected" // did not survive the probe, or was not usable
)

// SuggestedSource is one candidate and what became of it.
type SuggestedSource struct {
	Kind      string `json:"kind"` // rss | hn_search
	Name      string `json:"name,omitempty"`
	URL       string `json:"url,omitempty"`
	Query     string `json:"query,omitempty"`
	MinPoints int    `json:"min_points,omitempty"`

	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
	Items  int    `json:"items,omitempty"`

	// FinalURL is set when the probe landed somewhere other than URL, so the
	// screen can show that the feed it accepted is not the one it was given.
	FinalURL string `json:"final_url,omitempty"`
	// FeedTitle is the feed's own name. It is the only trustworthy answer to "is
	// this the publication the model claimed", and the owner reads it before
	// switching the row on.
	FeedTitle string `json:"feed_title,omitempty"`
}

// SuggestResult is one suggestion pass.
type SuggestResult struct {
	TopicID    int64  `json:"topic_id"`
	TopicTitle string `json:"topic_title"`

	Feeds    []SuggestedSource `json:"feeds"`
	Keywords []SuggestedSource `json:"keywords"`

	// Added counts feed rows actually written. The number the owner acts on: it is
	// how many paused rows are now waiting on the Sources screen.
	Added int `json:"added"`

	Calls     int `json:"calls"`
	TokensIn  int `json:"tokens_in"`
	TokensOut int `json:"tokens_out"`
}

// suggestReply is the model's output shape.
//
// An OBJECT wrapping the lists, not a bare array, for the reason spelled out on
// classifySystemPrompt: under JSONMode a top-level-array prompt silently returns
// one element on several providers. Two arrays are wanted here anyway, which is
// what the object shape is actually for.
type suggestReply struct {
	Feeds []struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	} `json:"feeds"`
	Keywords []struct {
		Query     string `json:"query"`
		MinPoints int    `json:"min_points"`
	} `json:"keywords"`
}

// SuggestSources proposes sources for one topic and accepts the ones that survive
// a real fetch.
//
// The shape of this, and why it is not symmetric:
//
//   - FEEDS are written, as PAUSED rss rows. Every URL is probed first, and one
//     that does not answer, or answers with something that is not a feed, or is a
//     feed carrying nothing, is REJECTED rather than parked. That is a deliberate
//     divergence from source import, which parks a doubtful feed paused-with-a-
//     reason: there, the owner named the feed and is owed the row; here the MODEL
//     named it, everything accepted is already paused, so "paused" carries no
//     signal and a rejected guess would just be a row to delete.
//
//   - KEYWORDS are only proposed, never written. A Hacker News keyword cannot be
//     paused -- the ROW is the unit of active, and there is exactly one HN row by
//     design (migration 007) -- so appending to the owner's live row would put the
//     model's guess into tomorrow's run without the review they asked for. The
//     screen offers one click per keyword, through the same PATCH the manual add
//     uses, which also keeps the "route into the existing row, never a second one"
//     rule in one place.
//
// Nothing the model returns reaches the database unchecked. That is the same
// posture as Asker.sanitizePlan and for the same reason: a wrong URL here does not
// fail loudly, it becomes a source row that quietly never yields anything.
func SuggestSources(
	ctx context.Context,
	db *store.DB,
	client *llm.Client,
	topic store.Topic,
	log *slog.Logger,
) (SuggestResult, error) {
	result := SuggestResult{TopicID: topic.ID, TopicTitle: topic.Title}

	// The synthesizer's chain, logged as its own role: recalling real publications
	// is world knowledge, not the mechanical work the cheap-first role exists for.
	resp, err := client.CompleteAs(ctx, llm.RoleSynthesizer, llm.RoleSuggest, llm.Request{
		Messages: []llm.Message{
			{Role: "system", Content: suggestSystemPrompt},
			{Role: "user", Content: suggestPrompt(topic)},
		},
		JSONMode: true,
		// Headroom for a reasoning model's thinking, which this limit also counts.
		MaxTokens: 4000,
		// No Expect: the prompt asks for "up to" a count, so a model returning
		// four good feeds instead of eight has not dropped anything. Recording a
		// soft target as a denominator would make every honest reply read as
		// incomplete. The parse gate is still worth having.
	})
	result.Calls = 1
	result.TokensIn, result.TokensOut = resp.Usage.TokensIn, resp.Usage.TokensOut
	if err != nil {
		return result, err
	}

	reply, parseErr := parseSuggestions(resp.Text)
	client.ReportReply(resp.CallID, 0, parseErr == nil)
	if parseErr != nil {
		return result, fmt.Errorf("could not read the model's suggestions: %w", parseErr)
	}

	existing, err := db.SourceIdentities()
	if err != nil {
		return result, err
	}

	result.Feeds = probeFeeds(ctx, sanitizeFeeds(reply, existing), existing, log)
	result.Keywords = sanitizeKeywords(reply, db)

	// Write last, on one goroutine, in list order so the result reads top to
	// bottom the way the screen renders it.
	for i := range result.Feeds {
		f := &result.Feeds[i]
		if f.Status != suggestAdded {
			continue
		}
		cfg, err := json.Marshal(suggestedFeedConfig{URL: f.FinalURL, SuggestedFor: topic.ID})
		if err != nil {
			f.Status, f.Reason = suggestRejected, "could not build the source config"
			continue
		}
		if _, err := db.CreateSource(store.Source{
			Kind: "rss", Name: f.Name, Config: cfg,
			// Paused. The owner asked for suggestions, not for their sky to
			// change while they were reading about it.
			Active: false,
		}); err != nil {
			f.Status, f.Reason = suggestRejected, "could not save"
			log.Error("save suggested source", "name", f.Name, "err", err)
			continue
		}
		result.Added++
	}

	log.Info("suggested sources",
		"topic", topic.Title,
		"feeds_added", result.Added,
		"feeds_considered", len(result.Feeds),
		"keywords_proposed", len(result.Keywords),
	)
	return result, nil
}

// suggestedFeedConfig is an ordinary rss config carrying its provenance.
//
// suggested_for is a note, not a reference: SourceExport.Identity keys on url
// alone, so the extra field cannot affect dedup, and a topic later deleted leaves
// a dangling id that harms nothing. The manual inbox stores {"manual":true} the
// same way, for the same reason -- sources.config is an unmigrated per-row blob,
// which is exactly what makes it the right place for this.
type suggestedFeedConfig struct {
	URL          string `json:"url"`
	SuggestedFor int64  `json:"suggested_for,omitempty"`
}

func parseSuggestions(text string) (suggestReply, error) {
	var reply suggestReply
	obj, err := llm.ExtractObject(text)
	if err != nil {
		return reply, err
	}
	if err := json.Unmarshal(obj, &reply); err != nil {
		return reply, err
	}
	if len(reply.Feeds) == 0 && len(reply.Keywords) == 0 {
		return reply, fmt.Errorf("no feeds or keywords in the reply")
	}
	return reply, nil
}

// sanitizeFeeds is the whitelist for proposed feeds, before any of them is
// fetched. Everything here is decidable without the network, and doing it first
// means a malformed list costs no requests.
func sanitizeFeeds(reply suggestReply, existing map[string]int64) []SuggestedSource {
	out := make([]SuggestedSource, 0, len(reply.Feeds))
	seen := map[string]bool{}

	for _, f := range reply.Feeds {
		if len(out) == maxSuggestedFeeds {
			break
		}
		raw := strings.TrimSpace(f.URL)
		cand := SuggestedSource{
			Kind: "rss",
			Name: sources.Truncate(strings.TrimSpace(f.Name), 120),
			URL:  raw,
		}

		switch {
		case raw == "":
			cand.Status, cand.Reason = suggestRejected, "no URL"
		case !usableFeedURL(raw):
			// The ordinary shape of a hallucination: a bare hostname, a search
			// page, a made-up scheme. Cheaper to refuse than to probe.
			cand.Status, cand.Reason = suggestRejected, "not an http(s) URL"
		case seen[feedKey(raw)]:
			cand.Status, cand.Reason = suggestSkipped, "suggested twice"
		case existing[identityForURL(raw)] != 0:
			cand.Status, cand.Reason = suggestSkipped, "already configured here"
		default:
			seen[feedKey(raw)] = true
			cand.Status = suggestAdded // provisional; the probe decides
		}
		out = append(out, cand)
	}
	return out
}

// probeFeeds fetches every provisional candidate and settles its status.
//
// This is where "never seed a source URL you have not run through the real
// fetcher" is enforced, and the post-redirect dedup is the part that is easy to
// miss: `openai.com/blog/rss.xml` 307s to `/news/rss.xml`, so a candidate can be
// distinct from everything configured right up until it is fetched, and adding it
// would double-count that masthead in every source_count it touches.
func probeFeeds(
	ctx context.Context,
	cands []SuggestedSource,
	existing map[string]int64,
	log *slog.Logger,
) []SuggestedSource {
	type probed struct {
		probe sources.FeedProbe
		err   error
	}
	results := make([]probed, len(cands))

	var wg sync.WaitGroup
	sem := make(chan struct{}, suggestProbeWorkers)
	for i := range cands {
		if cands[i].Status != suggestAdded {
			continue
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			pctx, cancel := context.WithTimeout(ctx, sources.FetchTimeout)
			defer cancel()
			p, err := sources.ProbeFeed(pctx, cands[i].URL)
			results[i] = probed{probe: p, err: err}
		}(i)
	}
	wg.Wait()

	// Settle sequentially, so the post-redirect dedup sees a stable set: two
	// candidates redirecting to the SAME feed must not both be accepted, and which
	// one wins has to be list order rather than a race.
	landed := map[string]bool{}
	for i := range cands {
		c := &cands[i]
		if c.Status != suggestAdded {
			continue
		}
		r := results[i]

		if r.err != nil {
			c.Status, c.Reason = suggestRejected, r.err.Error()
			if r.probe.FinalURL != "" && r.probe.FinalURL != c.URL {
				c.FinalURL = r.probe.FinalURL
			}
			log.Debug("suggested feed rejected", "url", c.URL, "err", r.err)
			continue
		}

		final := r.probe.FinalURL
		if final == "" {
			final = c.URL
		}
		c.FeedTitle = sources.Truncate(r.probe.Title, 160)
		c.Items = len(r.probe.Items)
		if final != c.URL {
			c.FinalURL = final
		}

		switch {
		case c.Items == 0:
			// The exact trap the seeding convention exists for: 200 in a browser,
			// zero items in the parser.
			c.Status, c.Reason = suggestRejected, "fetched but returned no items"
		case landed[feedKey(final)]:
			c.Status, c.Reason = suggestSkipped, "redirects to a feed already suggested above"
		case existing[identityForURL(final)] != 0:
			c.Status, c.Reason = suggestSkipped, "redirects to a feed already configured here"
		default:
			landed[feedKey(final)] = true
			// Store where it landed, not where it pointed: a redirect stored as a
			// source is one extra hop on every ingest forever.
			c.FinalURL = final
			if c.Name == "" {
				// The feed's own title beats nothing at all, and beats the
				// model's guess when the model did not make one.
				c.Name = c.FeedTitle
			}
			if c.Name == "" {
				c.Status, c.Reason = suggestRejected, "the feed has no title and none was given"
			}
		}
	}
	return cands
}

// sanitizeKeywords settles the proposed Hacker News keywords. Nothing is written:
// see SuggestSources on why a keyword is proposed rather than added.
func sanitizeKeywords(reply suggestReply, db *store.DB) []SuggestedSource {
	// The one Hacker News row's current keywords, so a term already being watched
	// is reported as such rather than offered again.
	watched := map[string]bool{}
	if srcs, err := db.AllSources(); err == nil {
		for _, s := range srcs {
			if s.Kind != "hn_search" {
				continue
			}
			var cfg sources.HNConfig
			if err := json.Unmarshal(s.Config, &cfg); err != nil {
				continue
			}
			for _, q := range cfg.Normalized() {
				watched[strings.ToLower(strings.TrimSpace(q.Query))] = true
			}
		}
	}

	out := make([]SuggestedSource, 0, len(reply.Keywords))
	seen := map[string]bool{}
	for _, k := range reply.Keywords {
		if len(out) == maxSuggestedKeywords {
			break
		}
		q := strings.TrimSpace(k.Query)
		cand := SuggestedSource{
			Kind:      "hn_search",
			Query:     sources.Truncate(q, 80),
			MinPoints: clampPoints(k.MinPoints),
		}
		key := strings.ToLower(cand.Query)

		switch {
		case q == "":
			cand.Status, cand.Reason = suggestRejected, "empty keyword"
		case seen[key]:
			cand.Status, cand.Reason = suggestSkipped, "suggested twice"
		case watched[key]:
			cand.Status, cand.Reason = suggestSkipped, "already a Hacker News keyword"
		default:
			seen[key] = true
			cand.Status = suggestProposed
		}
		out = append(out, cand)
	}
	return out
}

// clampPoints keeps a model's threshold inside the range the convention supports.
//
// Out of range in either direction is a real failure and neither is loud: too high
// silences the keyword (`MCP` at 500 returns nothing and looks like a keyword that
// simply found no news), too low floods the sky.
func clampPoints(n int) int {
	switch {
	case n <= 0:
		return defaultSuggestedPoints
	case n < minSuggestedPoints:
		return minSuggestedPoints
	case n > maxSuggestedPoints:
		return maxSuggestedPoints
	default:
		return n
	}
}

// usableFeedURL rejects what cannot be fetched before anything tries to fetch it.
func usableFeedURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	// A host with no dot is not a public feed -- it is "localhost", a bare word,
	// or a path the model forgot to prefix.
	return strings.Contains(u.Host, ".")
}

// feedKey is the comparison form for two feed URLs within one pass.
func feedKey(raw string) string {
	return strings.ToLower(strings.TrimRight(strings.TrimSpace(raw), "/"))
}

// identityForURL builds the same identity string SourceExport.Identity would, so
// dedup here and dedup on import agree about what "the same feed" means. Going
// through the real type rather than reimplementing its rule is the point.
func identityForURL(raw string) string {
	cfg, err := json.Marshal(suggestedFeedConfig{URL: raw})
	if err != nil {
		return ""
	}
	return store.SourceExport{Kind: "rss", Config: cfg}.Identity()
}

const suggestSystemPrompt = `You propose news sources for one topic in a personal news reader.

The reader ingests RSS/Atom/JSON feeds, and separately searches Hacker News by keyword.

Reply with ONLY a JSON object:
{"feeds":[{"name":"<the publication's name>","url":"<its feed URL>"}],
 "keywords":[{"query":"<a Hacker News search term>","min_points":<number>}]}

Feeds:
- Propose the FEED URL, not the site's homepage or an article.
- Prefer first-party sources: a company's own engineering blog, a researcher's own
  site, a publication's section feed for this subject rather than its whole-site feed.
- ONE entry per publication. Two paths to the same feed is a duplicate.
- Every URL is fetched and parsed before it is accepted, and one that is not a
  working feed is discarded. So propose feeds you are confident exist, and propose
  fewer rather than padding the list with guesses.

Keywords:
- Broad single concepts, not phrases: the search matches literally.
- min_points is the score a story must clear. A broad, common term needs a higher
  bar than a niche one, or it floods. Most terms want something between 5 and 50.

No markdown, no commentary, JSON object only.`

func suggestPrompt(topic store.Topic) string {
	var b strings.Builder
	fmt.Fprintf(&b, "TOPIC: %s\n", topic.Title)
	if def := strings.TrimSpace(topic.Definition); def != "" {
		fmt.Fprintf(&b, "\nWHAT THE OWNER MEANS BY IT:\n%s\n",
			sources.Truncate(def, maxDefinitionChars))
	}
	fmt.Fprintf(&b, "\nPropose up to %d feeds and up to %d Hacker News keywords for this topic. "+
		"Return the JSON object only.", maxSuggestedFeeds, maxSuggestedKeywords)
	return b.String()
}
