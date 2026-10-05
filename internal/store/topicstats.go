package store

import "fmt"

// TopicStat is one topic's share of one classify pass.
//
// ItemsConsidered counts only items in batches that came back parseable. A batch
// that failed had its items looked at in no meaningful sense, and counting them
// would blame the topic's definition for a provider's bad afternoon.
type TopicStat struct {
	TopicID         int64
	ItemsConsidered int
	MatchesKept     int
	Batches         int
}

// CommitClassifyPass records a pass: the per-topic stats, and each topic's cursor
// saying which items it is done with.
//
// One transaction, because the two must move together. Stats written without the
// cursor would count the same items again on the next run -- inflating every
// denominator and quietly understating every hit rate -- and a cursor written
// without the stats would lose the pass entirely with no way to recompute it.
//
// cursors holds only the topics whose cursor moved; stats may be empty, meaning
// the pass classified nothing.
func (db *DB) CommitClassifyPass(runDate string, stats []TopicStat, cursors map[int64]string) error {
	if len(stats) == 0 && len(cursors) == 0 {
		return nil
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("commit classify pass: begin: %w", err)
	}
	defer tx.Rollback()

	for _, s := range stats {
		// Accumulate rather than replace: a day can hold several passes, each
		// seeing only what the cursors had not already covered.
		if _, err := tx.Exec(
			`INSERT INTO topic_stats
			        (run_date, topic_id, items_considered, matches_kept, batches)
			 VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT(run_date, topic_id) DO UPDATE SET
			        items_considered = topic_stats.items_considered + excluded.items_considered,
			        matches_kept     = topic_stats.matches_kept     + excluded.matches_kept,
			        batches          = topic_stats.batches          + excluded.batches,
			        updated_at       = datetime('now')`,
			runDate, s.TopicID, s.ItemsConsidered, s.MatchesKept, s.Batches,
		); err != nil {
			return fmt.Errorf("save topic stats for topic %d: %w", s.TopicID, err)
		}
	}

	for topicID, cursor := range cursors {
		if _, err := tx.Exec(
			`UPDATE topics SET classified_through = ? WHERE id = ?`, cursor, topicID,
		); err != nil {
			return fmt.Errorf("advance classify cursor for topic %d: %w", topicID, err)
		}
	}

	return tx.Commit()
}

// TopicHitRate is a topic's classifier performance over a window.
type TopicHitRate struct {
	TopicID    int64  `json:"topic_id"`
	Title      string `json:"title"`
	Active     bool   `json:"active"`
	NeedsSetup bool   `json:"needs_setup"`

	ItemsConsidered int `json:"items_considered"`
	MatchesKept     int `json:"matches_kept"`
	Batches         int `json:"batches"`
	// RunDays counts dates with data, which is what makes a low total legible:
	// 6 matches over one day and over three weeks are different situations.
	RunDays int `json:"run_days"`
	// Signals is how many signals the synthesizer actually composed. Matches are
	// raw verdicts; signals are what reached the sky, and a topic matching plenty
	// while composing nothing is its own kind of problem.
	Signals int `json:"signals"`

	// HitRate is MatchesKept/ItemsConsidered, or -1 when nothing was considered.
	// A sentinel rather than 0: "never classified" and "classified 400 items and
	// matched none" both render as 0% otherwise, and only one of them is a
	// definition that needs rewriting.
	HitRate float64 `json:"hit_rate"`
}

// HasData reports whether the window holds anything to judge this topic on.
func (h TopicHitRate) HasData() bool { return h.ItemsConsidered > 0 }

// TopicHitRates aggregates per-topic classifier stats over the last `days` days.
//
// Every topic comes back, including ones with no stats at all -- a topic that has
// been classified zero times is exactly what this view exists to surface, so it
// must not be dropped by the join.
func (db *DB) TopicHitRates(days int) ([]TopicHitRate, error) {
	if days < 1 {
		days = 1
	}

	rows, err := db.Query(
		`SELECT t.id, t.title, t.active, t.definition,
		        COALESCE(sum(ts.items_considered), 0),
		        COALESCE(sum(ts.matches_kept), 0),
		        COALESCE(sum(ts.batches), 0),
		        count(ts.run_date)
		   FROM topics t
		   LEFT JOIN topic_stats ts
		     ON ts.topic_id = t.id
		    AND ts.run_date >= date('now', 'localtime', ?)
		  GROUP BY t.id
		  ORDER BY t.priority DESC, t.id`,
		fmt.Sprintf("-%d days", days-1),
	)
	if err != nil {
		return nil, fmt.Errorf("topic hit rates: %w", err)
	}

	var out []TopicHitRate
	for rows.Next() {
		var h TopicHitRate
		var definition string
		if err := rows.Scan(&h.TopicID, &h.Title, &h.Active, &definition,
			&h.ItemsConsidered, &h.MatchesKept, &h.Batches, &h.RunDays); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan topic hit rate: %w", err)
		}
		h.NeedsSetup = Topic{Definition: definition}.NeedsSetup()
		h.HitRate = -1
		if h.ItemsConsidered > 0 {
			h.HitRate = float64(h.MatchesKept) / float64(h.ItemsConsidered)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("topic hit rates: %w", err)
	}

	// Close before the next query: the pool is one connection, and holding these
	// rows open across SignalCountsByTopic would deadlock rather than error. See
	// the package doc.
	rows.Close()

	signals, err := db.SignalCountsByTopic(days)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Signals = signals[out[i].TopicID]
	}

	return out, nil
}
