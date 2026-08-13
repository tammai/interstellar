package store

import (
	"fmt"
	"strings"
	"unicode"
)

// SearchQuery is a resolved, executable query.
//
// It is deliberately a typed struct rather than a SQL string. M3's planner has an
// LLM turn a question into one of these, and a model that can only fill in these
// fields cannot invent a join, drop a WHERE clause, or read a table it was not
// meant to. Text-to-SQL would be a correctness hazard long before it was a
// security one: hallucinated SQL returns confidently wrong answers with nothing
// to notice them by.
type SearchQuery struct {
	// Text is free prose from a person. It is never interpolated into a MATCH
	// expression directly -- see BuildMatch.
	Text string
	// Kind is "signals", "items", or "" for both.
	Kind string
	// TopicIDs restricts to these topics. Empty means all.
	TopicIDs []int64
	// From and To are inclusive YYYY-MM-DD bounds. Empty means unbounded.
	From string
	To   string
	// Limit caps rows returned. Zero takes the default.
	Limit int
}

const (
	searchLimitDefault = 20
	searchLimitMax     = 100
)

// SearchHit is one result, from either corpus.
type SearchHit struct {
	Kind    string `json:"kind"` // signal | item
	ID      int64  `json:"id"`
	Title   string `json:"title"`
	Snippet string `json:"snippet,omitempty"`
	URL     string `json:"url"`
	Date    string `json:"date"`

	TopicID    int64  `json:"topic_id,omitempty"`
	TopicTitle string `json:"topic_title,omitempty"`

	// Signals only.
	SourceCount int     `json:"source_count,omitempty"`
	Score       float64 `json:"score,omitempty"`
	Reason      string  `json:"reason,omitempty"`

	// Items only.
	SourceName string `json:"source_name,omitempty"`

	// Rank is the bm25 score: lower is a better match, and it is zero on a
	// filter-only query with no text to rank by.
	Rank float64 `json:"rank"`
}

// BuildMatch turns prose into an FTS5 MATCH expression that cannot be a syntax
// error.
//
// This function is the reason user input never reaches MATCH directly. FTS5's
// query language treats AND, OR, NOT, NEAR, and the characters " * : ^ - ( ) as
// operators, so a perfectly ordinary question -- `what's happening with agents?`
// or a title containing a colon -- is either a syntax error or, worse, silently
// parsed as something else. Quoting every token turns each into a literal phrase
// and takes the whole operator grammar off the table.
//
// Tokens are OR-ed rather than AND-ed on purpose: AND across a prose question
// finds nothing, because no signal title contains every word someone typed. bm25
// then does the real work -- a row matching four of the terms outranks one
// matching a single common word -- so recall stays wide and ranking sorts it out.
//
// Returns "" when there is nothing searchable, which callers must treat as
// "filter only", never as "match everything".
func BuildMatch(raw string) string {
	var quoted []string
	for _, tok := range tokenize(raw) {
		// Doubling is FTS5's own escape for a quote inside a quoted phrase.
		quoted = append(quoted, `"`+strings.ReplaceAll(tok, `"`, `""`)+`"`)
	}
	return strings.Join(quoted, " OR ")
}

// tokenize splits on anything that is not a letter or digit, which is the same
// boundary the unicode61 tokenizer uses to build the index. Matching the
// indexer's own idea of a word is what keeps the two in agreement.
func tokenize(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})

	seen := map[string]bool{}
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		// Single characters carry no signal and match a large fraction of the
		// index, which costs a lot of rows to rank and improves nothing.
		if len([]rune(f)) < 2 {
			continue
		}
		lower := strings.ToLower(f)
		if seen[lower] {
			continue // a repeated word would just double its own weight
		}
		seen[lower] = true
		out = append(out, f)
	}
	return out
}

// Search runs a query across the requested corpora, best match first.
func (db *DB) Search(q SearchQuery) ([]SearchHit, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = searchLimitDefault
	}
	if limit > searchLimitMax {
		limit = searchLimitMax
	}

	wantSignals := q.Kind == "" || q.Kind == "signals"
	wantItems := q.Kind == "" || q.Kind == "items"

	var out []SearchHit

	if wantSignals {
		hits, err := db.searchSignals(q, limit)
		if err != nil {
			return nil, err
		}
		out = append(out, hits...)
	}
	if wantItems {
		hits, err := db.searchItems(q, limit)
		if err != nil {
			return nil, err
		}
		out = append(out, hits...)
	}

	// Both corpora were ranked independently, so interleave by rank. bm25 is
	// negative and lower is better; a filter-only query has rank 0 everywhere and
	// keeps whatever date order the branches returned.
	if wantSignals && wantItems {
		sortByRankThenDate(out)
		if len(out) > limit {
			out = out[:limit]
		}
	}
	return out, nil
}

func (db *DB) searchSignals(q SearchQuery, limit int) ([]SearchHit, error) {
	match := BuildMatch(q.Text)

	var (
		sb   strings.Builder
		args []any
	)

	if match != "" {
		sb.WriteString(`
			SELECT s.id, s.title, s.reason, s.primary_url, s.run_date,
			       s.topic_id, t.title, s.source_count, s.score,
			       snippet(signals_fts, -1, '<mark>', '</mark>', '…', 12),
			       bm25(signals_fts)
			  FROM signals_fts
			  JOIN signals s ON s.id = signals_fts.rowid
			  JOIN topics  t ON t.id = s.topic_id
			 WHERE signals_fts MATCH ?`)
		args = append(args, match)
	} else {
		sb.WriteString(`
			SELECT s.id, s.title, s.reason, s.primary_url, s.run_date,
			       s.topic_id, t.title, s.source_count, s.score,
			       '', 0.0
			  FROM signals s
			  JOIN topics t ON t.id = s.topic_id
			 WHERE 1 = 1`)
	}

	if len(q.TopicIDs) > 0 {
		sb.WriteString(` AND s.topic_id IN (` + placeholders(len(q.TopicIDs)) + `)`)
		for _, id := range q.TopicIDs {
			args = append(args, id)
		}
	}
	if q.From != "" {
		sb.WriteString(` AND s.run_date >= ?`)
		args = append(args, q.From)
	}
	if q.To != "" {
		sb.WriteString(` AND s.run_date <= ?`)
		args = append(args, q.To)
	}

	if match != "" {
		sb.WriteString(` ORDER BY bm25(signals_fts), s.run_date DESC`)
	} else {
		// No text to rank by, so the useful order is newest and most-corroborated.
		sb.WriteString(` ORDER BY s.run_date DESC, s.source_count DESC, s.score DESC`)
	}
	sb.WriteString(` LIMIT ?`)
	args = append(args, limit)

	rows, err := db.Query(sb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("search signals: %w", err)
	}

	var out []SearchHit
	for rows.Next() {
		h := SearchHit{Kind: "signal"}
		if err := rows.Scan(&h.ID, &h.Title, &h.Reason, &h.URL, &h.Date,
			&h.TopicID, &h.TopicTitle, &h.SourceCount, &h.Score,
			&h.Snippet, &h.Rank); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan signal hit: %w", err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("search signals: %w", err)
	}
	rows.Close()
	return out, nil
}

func (db *DB) searchItems(q SearchQuery, limit int) ([]SearchHit, error) {
	match := BuildMatch(q.Text)

	var (
		sb   strings.Builder
		args []any
	)

	// The source name comes from a scalar subquery rather than a join because the
	// pool is one connection: a follow-up query per row would deadlock against
	// the open rows. Same shape ItemsSince already uses.
	const cols = `
		SELECT i.id, i.title, i.canonical_url,
		       date(COALESCE(i.published_at, i.fetched_at)),
		       COALESCE((SELECT s.name FROM sources s WHERE s.id = i.source_id), '')`

	if match != "" {
		sb.WriteString(cols + `,
		       snippet(items_fts, -1, '<mark>', '</mark>', '…', 12),
		       bm25(items_fts)
		  FROM items_fts
		  JOIN items i ON i.id = items_fts.rowid
		 WHERE items_fts MATCH ?`)
		args = append(args, match)
	} else {
		sb.WriteString(cols + `, '', 0.0
		  FROM items i
		 WHERE 1 = 1`)
	}

	// An item has no topic of its own; its topics are the classifier's verdicts.
	if len(q.TopicIDs) > 0 {
		sb.WriteString(` AND EXISTS (SELECT 1 FROM matches m
		                              WHERE m.item_id = i.id
		                                AND m.topic_id IN (` + placeholders(len(q.TopicIDs)) + `))`)
		for _, id := range q.TopicIDs {
			args = append(args, id)
		}
	}
	if q.From != "" {
		sb.WriteString(` AND date(COALESCE(i.published_at, i.fetched_at)) >= ?`)
		args = append(args, q.From)
	}
	if q.To != "" {
		sb.WriteString(` AND date(COALESCE(i.published_at, i.fetched_at)) <= ?`)
		args = append(args, q.To)
	}

	if match != "" {
		sb.WriteString(` ORDER BY bm25(items_fts), i.id DESC`)
	} else {
		sb.WriteString(` ORDER BY COALESCE(i.published_at, i.fetched_at) DESC, i.id DESC`)
	}
	sb.WriteString(` LIMIT ?`)
	args = append(args, limit)

	rows, err := db.Query(sb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("search items: %w", err)
	}

	var out []SearchHit
	for rows.Next() {
		h := SearchHit{Kind: "item"}
		if err := rows.Scan(&h.ID, &h.Title, &h.URL, &h.Date, &h.SourceName,
			&h.Snippet, &h.Rank); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan item hit: %w", err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("search items: %w", err)
	}
	rows.Close()
	return out, nil
}

// sortByRankThenDate merges two independently ranked lists. Insertion sort: the
// slice is at most searchLimitMax*2 and already nearly sorted in two runs.
func sortByRankThenDate(hits []SearchHit) {
	for i := 1; i < len(hits); i++ {
		for j := i; j > 0; j-- {
			if better(hits[j], hits[j-1]) {
				hits[j], hits[j-1] = hits[j-1], hits[j]
				continue
			}
			break
		}
	}
}

func better(a, b SearchHit) bool {
	if a.Rank != b.Rank {
		return a.Rank < b.Rank // bm25 is negative; lower is a stronger match
	}
	return a.Date > b.Date
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
