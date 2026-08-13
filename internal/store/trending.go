package store

import "fmt"

// Trending reads the signals history. No new tables: spec §"Roadmap" says this
// builds on the signals table with no prep needed, and it was right.

// TopicTrend is one topic's volume over a window, split so a direction can be read
// off it rather than inferred.
type TopicTrend struct {
	TopicID    int64  `json:"topic_id"`
	Title      string `json:"title"`
	Active     bool   `json:"active"`
	NeedsSetup bool   `json:"needs_setup"`

	Signals int `json:"signals"`
	// Hot counts signals that reached the gold threshold. A topic producing volume
	// but never corroboration is a different situation from one producing less that
	// several outlets agree on.
	Hot    int     `json:"hot"`
	Days   int     `json:"days_with_signals"`
	MaxSrc int     `json:"max_sources"`
	AvgSrc float64 `json:"avg_sources"`

	// Recent and Prior split the window in half, so the shape of the change is
	// visible without the caller doing arithmetic on a series.
	Recent int `json:"recent"`
	Prior  int `json:"prior"`
}

// Direction summarizes the halves as one word.
//
// A word rather than a percentage: with a handful of signals per topic per week,
// "up 200%" means three instead of one and reads as far more than it is.
func (t TopicTrend) Direction() string {
	switch {
	case t.Recent == 0 && t.Prior == 0:
		return "quiet"
	case t.Prior == 0:
		return "new"
	case t.Recent == 0:
		return "stopped"
	case t.Recent > t.Prior:
		return "rising"
	case t.Recent < t.Prior:
		return "falling"
	default:
		return "steady"
	}
}

// TopicTrends aggregates per-topic signal history over the last `days` days.
//
// Every topic comes back, including silent ones -- a topic that has stopped
// producing is the most useful thing this view can show, so the join must not
// drop it.
func (db *DB) TopicTrends(days int) ([]TopicTrend, error) {
	if days < 2 {
		days = 2 // a single day cannot be split into halves
	}
	half := days / 2

	rows, err := db.Query(
		`SELECT t.id, t.title, t.active, t.definition,
		        count(s.id),
		        COALESCE(sum(CASE WHEN s.source_count >= 3 THEN 1 ELSE 0 END), 0),
		        count(DISTINCT s.run_date),
		        COALESCE(max(s.source_count), 0),
		        COALESCE(avg(s.source_count), 0),
		        COALESCE(sum(CASE WHEN s.run_date >= date('now','localtime',?) THEN 1 ELSE 0 END), 0),
		        COALESCE(sum(CASE WHEN s.run_date <  date('now','localtime',?) THEN 1 ELSE 0 END), 0)
		   FROM topics t
		   LEFT JOIN signals s
		     ON s.topic_id = t.id
		    AND s.run_date >= date('now','localtime',?)
		  GROUP BY t.id
		  ORDER BY count(s.id) DESC, t.priority DESC, t.id`,
		fmt.Sprintf("-%d days", half-1),
		fmt.Sprintf("-%d days", half-1),
		fmt.Sprintf("-%d days", days-1),
	)
	if err != nil {
		return nil, fmt.Errorf("topic trends: %w", err)
	}
	defer rows.Close()

	var out []TopicTrend
	for rows.Next() {
		var (
			tr         TopicTrend
			definition string
		)
		if err := rows.Scan(&tr.TopicID, &tr.Title, &tr.Active, &definition,
			&tr.Signals, &tr.Hot, &tr.Days, &tr.MaxSrc, &tr.AvgSrc,
			&tr.Recent, &tr.Prior); err != nil {
			return nil, fmt.Errorf("scan topic trend: %w", err)
		}
		tr.NeedsSetup = Topic{Definition: definition}.NeedsSetup()
		out = append(out, tr)
	}
	return out, rows.Err()
}

// DailyVolume is one day's signal count, for a sparkline.
type DailyVolume struct {
	RunDate string `json:"run_date"`
	Signals int    `json:"signals"`
	Hot     int    `json:"hot"`
}

// DailyVolumes returns signal counts per day, oldest first.
//
// Only days that produced signals appear. Filling the gaps is the caller's job:
// the UI knows the window it asked for, and a zero row here would be
// indistinguishable from a day the scheduler never ran.
func (db *DB) DailyVolumes(days int) ([]DailyVolume, error) {
	if days < 1 {
		days = 1
	}
	rows, err := db.Query(
		`SELECT run_date, count(*),
		        sum(CASE WHEN source_count >= 3 THEN 1 ELSE 0 END)
		   FROM signals
		  WHERE run_date >= date('now','localtime',?)
		  GROUP BY run_date
		  ORDER BY run_date`,
		fmt.Sprintf("-%d days", days-1),
	)
	if err != nil {
		return nil, fmt.Errorf("daily volumes: %w", err)
	}
	defer rows.Close()

	var out []DailyVolume
	for rows.Next() {
		var d DailyVolume
		if err := rows.Scan(&d.RunDate, &d.Signals, &d.Hot); err != nil {
			return nil, fmt.Errorf("scan daily volume: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// RecurringStory is a title that has appeared on several days.
type RecurringStory struct {
	Title      string `json:"title"`
	TopicID    int64  `json:"topic_id"`
	TopicTitle string `json:"topic_title"`
	Days       int    `json:"days"`
	FirstSeen  string `json:"first_seen"`
	LastSeen   string `json:"last_seen"`
	MaxSources int    `json:"max_sources"`
	SignalID   int64  `json:"signal_id"`
}

// RecurringStories finds stories that kept coming back.
//
// Grouped on the normalized title rather than the raw one, because the synthesizer
// writes a fresh title every day and the same story arrives slightly reworded. The
// normalization is done in SQL -- lower(), and strip the punctuation that actually
// varies -- rather than in Go, because doing it in Go would mean reading every
// signal in the window into memory to group them.
//
// A story running for several days is the thing a daily sky cannot show: each day
// it is one star among many, and only the history says it never went away.
func (db *DB) RecurringStories(days, minDays, limit int) ([]RecurringStory, error) {
	if days < 1 {
		days = 1
	}
	if minDays < 2 {
		minDays = 2 // appearing once is not recurring
	}
	if limit <= 0 {
		limit = 10
	}

	// Normalize in SQL, because doing it in Go would mean reading every signal in
	// the window into memory just to group them.
	//
	// The double-space collapse at the end is not decoration. Turning ':' and '-'
	// into spaces leaves "patterns:  converge" with two spaces where the original
	// had one, so without it "Agent harness patterns converge" and "Agent harness
	// patterns: converge" normalize to different strings and the recurring story
	// this function exists to find is missed entirely. Two passes handle the
	// consecutive punctuation real titles actually contain.
	const norm = `trim(` +
		`replace(replace(` +
		`replace(replace(replace(replace(lower(s.title),` +
		`'"',''),'''',''),'-',' '),':',' ')` +
		`,'  ',' '),'  ',' '))`

	rows, err := db.Query(
		`SELECT max(s.title), s.topic_id, max(t.title),
		        count(DISTINCT s.run_date),
		        min(s.run_date), max(s.run_date),
		        max(s.source_count), max(s.id)
		   FROM signals s
		   JOIN topics t ON t.id = s.topic_id
		  WHERE s.run_date >= date('now','localtime',?)
		  GROUP BY s.topic_id, `+norm+`
		 HAVING count(DISTINCT s.run_date) >= ?
		  ORDER BY count(DISTINCT s.run_date) DESC, max(s.source_count) DESC
		  LIMIT ?`,
		fmt.Sprintf("-%d days", days-1), minDays, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("recurring stories: %w", err)
	}
	defer rows.Close()

	var out []RecurringStory
	for rows.Next() {
		var r RecurringStory
		if err := rows.Scan(&r.Title, &r.TopicID, &r.TopicTitle,
			&r.Days, &r.FirstSeen, &r.LastSeen, &r.MaxSources, &r.SignalID); err != nil {
			return nil, fmt.Errorf("scan recurring story: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
