package store

import (
	"fmt"
	"sort"
)

// QualityDetail is the Signal quality screen's second layer: where items go
// between arriving and reaching the sky, which sources earn their place, and what
// the window cost.
//
// The hit-rate table answers "is this topic's definition working". These answer
// the questions next to it that the hit rate cannot: a topic can match well and
// still compose nothing, a source can be busy and never match, and a model can be
// cheap per call and expensive per star.
type QualityDetail struct {
	Funnel  PipelineFunnel `json:"funnel"`
	Sources []SourceYield  `json:"sources"`
	Spend   []RoleSpend    `json:"spend"`
	Daily   []DailyQuality `json:"daily"`
}

// PipelineFunnel follows the window's items from ingest to the sky. Each step is
// a subset of the one before, so the drop between two steps is where items went.
type PipelineFunnel struct {
	ItemsIngested  int `json:"items_ingested"`
	ItemsMatched   int `json:"items_matched"`
	ItemsInSignals int `json:"items_in_signals"`
	Signals        int `json:"signals"`
	HotSignals     int `json:"hot_signals"`
}

// SourceYield is one source's contribution over the window. MatchRate is -1 when
// the source carried nothing, for the same reason TopicHitRate uses the sentinel:
// silent and useless are different findings.
type SourceYield struct {
	SourceID  int64   `json:"source_id"`
	Name      string  `json:"name"`
	Kind      string  `json:"kind"`
	Active    bool    `json:"active"`
	LastRunAt string  `json:"last_run_at,omitempty"`
	Items     int     `json:"items"`
	Matched   int     `json:"matched"`
	InSignals int     `json:"in_signals"`
	MatchRate float64 `json:"match_rate"`
}

// RoleSpend is one role's calls and tokens over the window, failures included --
// a failed attempt is still a request someone may be billed for.
type RoleSpend struct {
	Role      string `json:"role"`
	Calls     int    `json:"calls"`
	Failed    int    `json:"failed"`
	TokensIn  int    `json:"tokens_in"`
	TokensOut int    `json:"tokens_out"`
}

// DailyQuality is one local day of the window.
type DailyQuality struct {
	Date     string `json:"date"`
	Ingested int    `json:"ingested"`
	Matched  int    `json:"matched"`
	Signals  int    `json:"signals"`
	Calls    int    `json:"calls"`
}

// QualityDetailOver builds the detail for the last `days` local days, the same
// window TopicHitRates reads. hotThreshold is the source count that makes a star
// gold (§6).
//
// Every query is drained before the next: the pool is one connection.
func (db *DB) QualityDetailOver(days, hotThreshold int) (QualityDetail, error) {
	if days < 1 {
		days = 1
	}
	// fetched_at is UTC and run_date is local, so items are windowed from the
	// start of the first local day, converted back to UTC.
	back := fmt.Sprintf("-%d days", days-1)
	var out QualityDetail

	f := &out.Funnel
	err := db.QueryRow(
		`WITH w AS (
		   SELECT id FROM items
		    WHERE fetched_at >= datetime(date('now', 'localtime', ?), 'utc')
		 )
		 SELECT (SELECT count(*) FROM w),
		        (SELECT count(*) FROM w WHERE EXISTS (SELECT 1 FROM matches m WHERE m.item_id = w.id)),
		        (SELECT count(*) FROM w WHERE EXISTS (SELECT 1 FROM signal_items si WHERE si.item_id = w.id))`,
		back,
	).Scan(&f.ItemsIngested, &f.ItemsMatched, &f.ItemsInSignals)
	if err != nil {
		return out, fmt.Errorf("quality funnel: %w", err)
	}
	err = db.QueryRow(
		`SELECT count(*), COALESCE(sum(CASE WHEN source_count >= ? THEN 1 ELSE 0 END), 0)
		   FROM signals WHERE run_date >= date('now', 'localtime', ?)`,
		hotThreshold, back,
	).Scan(&f.Signals, &f.HotSignals)
	if err != nil {
		return out, fmt.Errorf("quality funnel signals: %w", err)
	}

	if out.Sources, err = db.sourceYield(back); err != nil {
		return out, err
	}
	if out.Spend, err = db.roleSpend(back); err != nil {
		return out, err
	}
	if out.Daily, err = db.dailyQuality(back); err != nil {
		return out, err
	}
	return out, nil
}

func (db *DB) sourceYield(back string) ([]SourceYield, error) {
	rows, err := db.Query(
		`SELECT s.id, s.name, s.kind, s.active, COALESCE(s.last_run_at, ''),
		        count(x.item_id),
		        COALESCE(sum(CASE WHEN EXISTS (SELECT 1 FROM matches m WHERE m.item_id = x.item_id)
		                          THEN 1 ELSE 0 END), 0),
		        COALESCE(sum(CASE WHEN EXISTS (SELECT 1 FROM signal_items si WHERE si.item_id = x.item_id)
		                          THEN 1 ELSE 0 END), 0)
		   FROM sources s
		   LEFT JOIN item_sources x
		     ON x.source_id = s.id
		    AND x.first_seen_at >= datetime(date('now', 'localtime', ?), 'utc')
		  GROUP BY s.id
		  ORDER BY count(x.item_id) DESC, s.name`,
		back,
	)
	if err != nil {
		return nil, fmt.Errorf("source yield: %w", err)
	}
	defer rows.Close()

	var out []SourceYield
	for rows.Next() {
		var y SourceYield
		if err := rows.Scan(&y.SourceID, &y.Name, &y.Kind, &y.Active, &y.LastRunAt,
			&y.Items, &y.Matched, &y.InSignals); err != nil {
			return nil, fmt.Errorf("scan source yield: %w", err)
		}
		y.MatchRate = -1
		if y.Items > 0 {
			y.MatchRate = float64(y.Matched) / float64(y.Items)
		}
		out = append(out, y)
	}
	return out, rows.Err()
}

func (db *DB) roleSpend(back string) ([]RoleSpend, error) {
	rows, err := db.Query(
		`SELECT role, count(*), COALESCE(sum(CASE WHEN ok = 0 THEN 1 ELSE 0 END), 0),
		        COALESCE(sum(tokens_in), 0), COALESCE(sum(tokens_out), 0)
		   FROM llm_calls
		  WHERE run_date >= date('now', 'localtime', ?)
		  GROUP BY role
		  ORDER BY count(*) DESC, role`,
		back,
	)
	if err != nil {
		return nil, fmt.Errorf("role spend: %w", err)
	}
	defer rows.Close()

	var out []RoleSpend
	for rows.Next() {
		var r RoleSpend
		if err := rows.Scan(&r.Role, &r.Calls, &r.Failed, &r.TokensIn, &r.TokensOut); err != nil {
			return nil, fmt.Errorf("scan role spend: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// dailyQuality merges three per-day counts read separately -- items by local
// fetch date, signals and calls by run date -- into one row per day that has any.
func (db *DB) dailyQuality(back string) ([]DailyQuality, error) {
	byDate := map[string]*DailyQuality{}
	row := func(date string) *DailyQuality {
		d := byDate[date]
		if d == nil {
			d = &DailyQuality{Date: date}
			byDate[date] = d
		}
		return d
	}

	collect := func(query string, apply func(d *DailyQuality, a, b int)) error {
		rows, err := db.Query(query, back)
		if err != nil {
			return fmt.Errorf("daily quality: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var date string
			var a, b int
			if err := rows.Scan(&date, &a, &b); err != nil {
				return fmt.Errorf("scan daily quality: %w", err)
			}
			apply(row(date), a, b)
		}
		return rows.Err()
	}

	if err := collect(
		`SELECT date(fetched_at, 'localtime') AS d, count(*),
		        COALESCE(sum(CASE WHEN EXISTS (SELECT 1 FROM matches m WHERE m.item_id = items.id)
		                          THEN 1 ELSE 0 END), 0)
		   FROM items
		  WHERE fetched_at >= datetime(date('now', 'localtime', ?), 'utc')
		  GROUP BY d`,
		func(d *DailyQuality, a, b int) { d.Ingested, d.Matched = a, b },
	); err != nil {
		return nil, err
	}
	if err := collect(
		`SELECT run_date, count(*), 0 FROM signals
		  WHERE run_date >= date('now', 'localtime', ?) GROUP BY run_date`,
		func(d *DailyQuality, a, _ int) { d.Signals = a },
	); err != nil {
		return nil, err
	}
	if err := collect(
		`SELECT run_date, count(*), 0 FROM llm_calls
		  WHERE run_date >= date('now', 'localtime', ?) GROUP BY run_date`,
		func(d *DailyQuality, a, _ int) { d.Calls = a },
	); err != nil {
		return nil, err
	}

	out := make([]DailyQuality, 0, len(byDate))
	for _, d := range byDate {
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date > out[j].Date })
	return out, nil
}
