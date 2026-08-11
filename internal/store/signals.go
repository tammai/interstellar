package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Match is one classifier verdict worth keeping.
type Match struct {
	ItemID  int64
	TopicID int64
	Score   float64
	Reason  string
}

// SaveMatches upserts verdicts. Upsert rather than insert so re-running a
// classify pass over the same window converges instead of erroring.
func (db *DB) SaveMatches(matches []Match) error {
	if len(matches) == 0 {
		return nil
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("save matches: begin: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(
		`INSERT INTO matches (item_id, topic_id, score, reason) VALUES (?, ?, ?, ?)
		 ON CONFLICT(item_id, topic_id) DO UPDATE SET score = excluded.score, reason = excluded.reason`,
	)
	if err != nil {
		return fmt.Errorf("save matches: prepare: %w", err)
	}
	defer stmt.Close()

	for _, m := range matches {
		if _, err := stmt.Exec(m.ItemID, m.TopicID, m.Score, m.Reason); err != nil {
			return fmt.Errorf("save match item=%d topic=%d: %w", m.ItemID, m.TopicID, err)
		}
	}
	return tx.Commit()
}

// MatchedItem is a match joined with enough of its item to compose a signal.
type MatchedItem struct {
	ItemID       int64
	Score        float64
	Reason       string
	Title        string
	Summary      string
	CanonicalURL string
	SourceCount  int
}

// MatchesForTopic returns a topic's matches over items fetched in the last
// `days` days, best first. This is the synthesizer's input.
func (db *DB) MatchesForTopic(topicID int64, days int) ([]MatchedItem, error) {
	rows, err := db.Query(
		`SELECT m.item_id, m.score, COALESCE(m.reason, ''),
		        i.title, COALESCE(i.summary, ''), i.canonical_url,
		        (SELECT count(DISTINCT source_id) FROM item_sources WHERE item_id = i.id)
		   FROM matches m
		   JOIN items i ON i.id = m.item_id
		  WHERE m.topic_id = ?
		    AND i.fetched_at >= datetime('now', ?)
		  ORDER BY m.score DESC, i.fetched_at DESC`,
		topicID, fmt.Sprintf("-%d days", days),
	)
	if err != nil {
		return nil, fmt.Errorf("matches for topic %d: %w", topicID, err)
	}
	defer rows.Close()

	var out []MatchedItem
	for rows.Next() {
		var m MatchedItem
		if err := rows.Scan(&m.ItemID, &m.Score, &m.Reason, &m.Title, &m.Summary, &m.CanonicalURL, &m.SourceCount); err != nil {
			return nil, fmt.Errorf("scan matched item: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// NewSignal is a composed signal ready to persist.
type NewSignal struct {
	RunDate string
	TopicID int64
	Title   string
	Reason  string
	ItemIDs []int64
}

// ReplaceSignals swaps a (run_date, topic) group atomically.
//
// Delete-then-insert in one transaction is what makes compose idempotent: a
// second run for the same day produces the same set rather than duplicating
// stars, and a crash leaves either the old group or the new one, never half of
// each.
//
// score and source_count are computed here, from the database, rather than taken
// from the model: score is the max over member matches, source_count the
// distinct sources across member items. Asking an LLM to count is both wasteful
// and unreliable, and source_count in particular decides whether a star is gold.
func (db *DB) ReplaceSignals(runDate string, topicID int64, signals []NewSignal) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("replace signals: begin: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(
		`DELETE FROM signal_items
		  WHERE signal_id IN (SELECT id FROM signals WHERE run_date = ? AND topic_id = ?)`,
		runDate, topicID,
	); err != nil {
		return fmt.Errorf("clear old signal items: %w", err)
	}
	if _, err := tx.Exec(
		`DELETE FROM signals WHERE run_date = ? AND topic_id = ?`, runDate, topicID,
	); err != nil {
		return fmt.Errorf("clear old signals: %w", err)
	}

	for _, s := range signals {
		if len(s.ItemIDs) == 0 {
			continue // a signal with no members is not a story
		}

		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(s.ItemIDs)), ",")
		args := make([]any, 0, len(s.ItemIDs)+1)
		args = append(args, topicID)
		for _, id := range s.ItemIDs {
			args = append(args, id)
		}

		// score = max over member matches for THIS topic.
		var score float64
		err := tx.QueryRow(
			`SELECT COALESCE(max(score), 0) FROM matches
			  WHERE topic_id = ? AND item_id IN (`+placeholders+`)`, args...,
		).Scan(&score)
		if err != nil {
			return fmt.Errorf("compute signal score: %w", err)
		}

		// source_count = distinct sources across member items. This is why
		// item_sources exists.
		itemArgs := args[1:]
		var sourceCount int
		err = tx.QueryRow(
			`SELECT count(DISTINCT source_id) FROM item_sources
			  WHERE item_id IN (`+placeholders+`)`, itemArgs...,
		).Scan(&sourceCount)
		if err != nil {
			return fmt.Errorf("compute signal source count: %w", err)
		}

		// The primary link is the highest-scoring member: the one the classifier
		// judged most on-topic, not whichever the model happened to list first.
		var primaryURL string
		err = tx.QueryRow(
			`SELECT i.canonical_url
			   FROM items i JOIN matches m ON m.item_id = i.id AND m.topic_id = ?
			  WHERE i.id IN (`+placeholders+`)
			  ORDER BY m.score DESC, i.id
			  LIMIT 1`, args...,
		).Scan(&primaryURL)
		if errors.Is(err, sql.ErrNoRows) {
			continue // members vanished; skip rather than write a broken star
		}
		if err != nil {
			return fmt.Errorf("pick signal primary url: %w", err)
		}

		res, err := tx.Exec(
			`INSERT INTO signals (run_date, topic_id, title, reason, score, source_count, primary_url)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			runDate, topicID, s.Title, s.Reason, score, sourceCount, primaryURL,
		)
		if err != nil {
			return fmt.Errorf("insert signal: %w", err)
		}
		signalID, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("insert signal: last id: %w", err)
		}

		for _, itemID := range s.ItemIDs {
			if _, err := tx.Exec(
				`INSERT INTO signal_items (signal_id, item_id) VALUES (?, ?)
				 ON CONFLICT(signal_id, item_id) DO NOTHING`,
				signalID, itemID,
			); err != nil {
				return fmt.Errorf("link signal %d to item %d: %w", signalID, itemID, err)
			}
		}
	}

	return tx.Commit()
}

// Signal is a composed signal as the sky renders it.
type Signal struct {
	ID          int64   `json:"id"`
	RunDate     string  `json:"run_date"`
	TopicID     int64   `json:"topic_id"`
	Title       string  `json:"title"`
	Reason      string  `json:"reason"`
	Score       float64 `json:"score"`
	SourceCount int     `json:"source_count"`
	PrimaryURL  string  `json:"primary_url"`
	AgeDays     int     `json:"age_days"`
}

// RecentSignals returns the last `days` days of signals, newest first.
//
// age_days is computed in SQL against the run date so the client never has to
// reason about timezones to decide how much to dim a star.
func (db *DB) RecentSignals(days int) ([]Signal, error) {
	rows, err := db.Query(
		`SELECT id, run_date, topic_id, title, reason, score, source_count, primary_url,
		        CAST(julianday('now', 'localtime') - julianday(run_date) AS INTEGER)
		   FROM signals
		  WHERE run_date >= date('now', 'localtime', ?)
		  ORDER BY run_date DESC, score DESC, id`,
		fmt.Sprintf("-%d days", days-1),
	)
	if err != nil {
		return nil, fmt.Errorf("recent signals: %w", err)
	}
	defer rows.Close()

	var out []Signal
	for rows.Next() {
		var s Signal
		if err := rows.Scan(&s.ID, &s.RunDate, &s.TopicID, &s.Title, &s.Reason,
			&s.Score, &s.SourceCount, &s.PrimaryURL, &s.AgeDays); err != nil {
			return nil, fmt.Errorf("scan signal: %w", err)
		}
		if s.AgeDays < 0 {
			s.AgeDays = 0
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// SignalMember is one article inside a signal, for the detail panel.
type SignalMember struct {
	ItemID       int64    `json:"item_id"`
	Title        string   `json:"title"`
	CanonicalURL string   `json:"url"`
	Sources      []string `json:"sources"`
}

// SignalDetail is a signal plus everything the panel shows.
type SignalDetail struct {
	Signal
	TopicTitle string         `json:"topic_title"`
	Members    []SignalMember `json:"members"`
	// SeenOn is the flattened, de-duplicated source list across all members --
	// the mock's "Seen on" block.
	SeenOn []string `json:"seen_on"`
}

// SignalByID loads a signal with its members and attribution.
func (db *DB) SignalByID(id int64) (SignalDetail, error) {
	var d SignalDetail
	err := db.QueryRow(
		`SELECT s.id, s.run_date, s.topic_id, s.title, s.reason, s.score,
		        s.source_count, s.primary_url,
		        CAST(julianday('now', 'localtime') - julianday(s.run_date) AS INTEGER),
		        t.title
		   FROM signals s JOIN topics t ON t.id = s.topic_id
		  WHERE s.id = ?`, id,
	).Scan(&d.ID, &d.RunDate, &d.TopicID, &d.Title, &d.Reason, &d.Score,
		&d.SourceCount, &d.PrimaryURL, &d.AgeDays, &d.TopicTitle)
	if errors.Is(err, sql.ErrNoRows) {
		return SignalDetail{}, ErrNotFound
	}
	if err != nil {
		return SignalDetail{}, fmt.Errorf("read signal %d: %w", id, err)
	}
	if d.AgeDays < 0 {
		d.AgeDays = 0
	}

	// Two passes, deliberately. The connection pool is capped at one, so issuing
	// a second query while these rows are still open would wait forever for a
	// connection that this very statement is holding. Drain first, then enrich.
	rows, err := db.Query(
		`SELECT i.id, i.title, i.canonical_url
		   FROM signal_items si JOIN items i ON i.id = si.item_id
		   LEFT JOIN matches m ON m.item_id = i.id AND m.topic_id = ?
		  WHERE si.signal_id = ?
		  ORDER BY COALESCE(m.score, 0) DESC, i.id`,
		d.TopicID, id,
	)
	if err != nil {
		return SignalDetail{}, fmt.Errorf("read signal %d members: %w", id, err)
	}

	for rows.Next() {
		var m SignalMember
		if err := rows.Scan(&m.ItemID, &m.Title, &m.CanonicalURL); err != nil {
			rows.Close()
			return SignalDetail{}, fmt.Errorf("scan signal member: %w", err)
		}
		d.Members = append(d.Members, m)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return SignalDetail{}, fmt.Errorf("read signal %d members: %w", id, err)
	}
	rows.Close()

	seen := map[string]bool{}
	for i := range d.Members {
		names, err := db.ItemSourceNames(d.Members[i].ItemID)
		if err != nil {
			return SignalDetail{}, err
		}
		d.Members[i].Sources = names
		for _, n := range names {
			if !seen[n] {
				seen[n] = true
				d.SeenOn = append(d.SeenOn, n)
			}
		}
	}
	return d, nil
}
