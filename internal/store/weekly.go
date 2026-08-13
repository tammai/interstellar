package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// WeeklyDigest is one week's round-up.
type WeeklyDigest struct {
	ID        int64  `json:"id"`
	WeekStart string `json:"week_start"`
	WeekEnd   string `json:"week_end"`
	Body      string `json:"body"`

	Signals int `json:"signals"`
	Topics  int `json:"topics"`

	TokensIn  int `json:"tokens_in"`
	TokensOut int `json:"tokens_out"`

	SentAt    string `json:"sent_at,omitempty"`
	SentVia   string `json:"sent_via,omitempty"`
	SendError string `json:"send_error,omitempty"`

	CreatedAt string `json:"created_at"`
}

// WeekBounds returns the Monday and Sunday of the week containing t.
//
// Monday-start because the round-up is about a working week, and a Sunday-start
// week splits Friday's news from the Monday people read it on. Go's Weekday puts
// Sunday at 0, which is the off-by-one worth naming rather than hiding.
func WeekBounds(t time.Time) (start, end string) {
	offset := (int(t.Weekday()) + 6) % 7 // Monday -> 0, Sunday -> 6
	monday := t.AddDate(0, 0, -offset)
	return RunDate(monday), RunDate(monday.AddDate(0, 0, 6))
}

// PreviousWeekBounds returns the week before the one containing t.
//
// What the scheduler wants: a round-up written on Monday morning is about the week
// that just finished, not the four hours of the one that just started.
func PreviousWeekBounds(t time.Time) (start, end string) {
	return WeekBounds(t.AddDate(0, 0, -7))
}

// SaveWeeklyDigest writes or replaces a week's round-up.
//
// Replaces on conflict so writing the same week twice is idempotent, the same
// property ReplaceSignals gives compose. Delivery state is deliberately NOT reset
// here: a re-write of an already-sent week must not make it look unsent and get
// mailed again.
func (db *DB) SaveWeeklyDigest(d WeeklyDigest) (int64, error) {
	res, err := db.Exec(
		`INSERT INTO weekly_digests
		        (week_start, week_end, body, signals, topics, tokens_in, tokens_out)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(week_start) DO UPDATE SET
		        week_end = excluded.week_end, body = excluded.body,
		        signals = excluded.signals, topics = excluded.topics,
		        tokens_in = excluded.tokens_in, tokens_out = excluded.tokens_out`,
		d.WeekStart, d.WeekEnd, d.Body, d.Signals, d.Topics, d.TokensIn, d.TokensOut,
	)
	if err != nil {
		return 0, fmt.Errorf("save weekly digest for %s: %w", d.WeekStart, err)
	}
	if id, err := res.LastInsertId(); err == nil && id > 0 {
		return id, nil
	}
	// An upsert that took the UPDATE path reports no insert id.
	var id int64
	if err := db.QueryRow(
		`SELECT id FROM weekly_digests WHERE week_start = ?`, d.WeekStart).Scan(&id); err != nil {
		return 0, fmt.Errorf("read back weekly digest %s: %w", d.WeekStart, err)
	}
	return id, nil
}

// WeeklyDigestFor returns one week's round-up.
func (db *DB) WeeklyDigestFor(weekStart string) (WeeklyDigest, error) {
	return db.weeklyOne(`WHERE week_start = ?`, weekStart)
}

// LatestWeeklyDigest returns the most recent round-up.
func (db *DB) LatestWeeklyDigest() (WeeklyDigest, error) {
	return db.weeklyOne(`ORDER BY week_start DESC LIMIT 1`)
}

func (db *DB) weeklyOne(clause string, args ...any) (WeeklyDigest, error) {
	var (
		d                          WeeklyDigest
		sentAt, sentVia, sendError sql.NullString
	)
	err := db.QueryRow(
		`SELECT id, week_start, week_end, body, signals, topics,
		        tokens_in, tokens_out, sent_at, sent_via, send_error, created_at
		   FROM weekly_digests `+clause, args...,
	).Scan(&d.ID, &d.WeekStart, &d.WeekEnd, &d.Body, &d.Signals, &d.Topics,
		&d.TokensIn, &d.TokensOut, &sentAt, &sentVia, &sendError, &d.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return WeeklyDigest{}, ErrNotFound
	}
	if err != nil {
		return WeeklyDigest{}, fmt.Errorf("read weekly digest: %w", err)
	}
	d.SentAt, d.SentVia, d.SendError = sentAt.String, sentVia.String, sendError.String
	return d, nil
}

// WeeklyDigests lists round-ups, newest first.
func (db *DB) WeeklyDigests(limit int) ([]WeeklyDigest, error) {
	if limit <= 0 {
		limit = 12
	}
	rows, err := db.Query(
		`SELECT id, week_start, week_end, body, signals, topics,
		        tokens_in, tokens_out, sent_at, sent_via, send_error, created_at
		   FROM weekly_digests
		  ORDER BY week_start DESC
		  LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list weekly digests: %w", err)
	}
	defer rows.Close()

	var out []WeeklyDigest
	for rows.Next() {
		var (
			d                          WeeklyDigest
			sentAt, sentVia, sendError sql.NullString
		)
		if err := rows.Scan(&d.ID, &d.WeekStart, &d.WeekEnd, &d.Body, &d.Signals, &d.Topics,
			&d.TokensIn, &d.TokensOut, &sentAt, &sentVia, &sendError, &d.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan weekly digest: %w", err)
		}
		d.SentAt, d.SentVia, d.SendError = sentAt.String, sentVia.String, sendError.String
		out = append(out, d)
	}
	return out, rows.Err()
}

// MarkWeeklySent records a delivery attempt.
func (db *DB) MarkWeeklySent(weekStart, via, sendErr string) error {
	var sentAt any
	if sendErr == "" {
		sentAt = time.Now().UTC().Format(time.RFC3339)
	}
	_, err := db.Exec(
		`UPDATE weekly_digests
		    SET sent_at = ?, sent_via = ?, send_error = ?
		  WHERE week_start = ?`,
		sentAt, nullIfEmpty(via), nullIfEmpty(sendErr), weekStart,
	)
	if err != nil {
		return fmt.Errorf("mark weekly %s sent: %w", weekStart, err)
	}
	return nil
}

// SignalsForWeek returns a week's signals, best first, for the round-up prompt.
func (db *DB) SignalsForWeek(weekStart, weekEnd string) ([]SearchHit, error) {
	rows, err := db.Query(
		`SELECT s.id, s.title, s.reason, s.primary_url, s.run_date,
		        s.topic_id, t.title, s.source_count, s.score
		   FROM signals s
		   JOIN topics t ON t.id = s.topic_id
		  WHERE s.run_date >= ? AND s.run_date <= ?
		  ORDER BY s.source_count DESC, s.score DESC, s.run_date DESC`,
		weekStart, weekEnd)
	if err != nil {
		return nil, fmt.Errorf("signals for week %s: %w", weekStart, err)
	}
	defer rows.Close()

	var out []SearchHit
	for rows.Next() {
		h := SearchHit{Kind: "signal"}
		if err := rows.Scan(&h.ID, &h.Title, &h.Reason, &h.URL, &h.Date,
			&h.TopicID, &h.TopicTitle, &h.SourceCount, &h.Score); err != nil {
			return nil, fmt.Errorf("scan week signal: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
