package store

import "fmt"

// Transcript statuses. `none` and `error` are deliberately different: no captions
// is a permanent fact about a video, while an error may be worth another attempt
// after a code change.
const (
	TranscriptOK    = "ok"
	TranscriptNone  = "none"
	TranscriptError = "error"
)

// TranscriptRecord is one attempt at a video's captions.
type TranscriptRecord struct {
	ItemID    int64
	Status    string
	Text      string
	Lang      string
	Generated bool
	Summary   string
	Err       string
}

// VideoNeedingTranscript is an item that looks like a video and has no transcript
// row yet.
type VideoNeedingTranscript struct {
	ItemID int64
	URL    string
	Title  string
}

// VideosNeedingTranscripts finds video items not yet attempted.
//
// Identified by their source kind rather than by URL shape: a youtube_rss source
// yields videos by definition, and matching on the URL would also drag in every
// ordinary article that happens to link to YouTube.
//
// The anti-join is what makes this cheap and what stops the pipeline retrying a
// video with captions disabled on every run forever -- a failure row counts as
// attempted.
func (db *DB) VideosNeedingTranscripts(limit int) ([]VideoNeedingTranscript, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := db.Query(
		`SELECT i.id, i.canonical_url, i.title
		   FROM items i
		  WHERE EXISTS (
		          SELECT 1 FROM item_sources isrc
		            JOIN sources s ON s.id = isrc.source_id
		           WHERE isrc.item_id = i.id AND s.kind = 'youtube_rss')
		    AND NOT EXISTS (SELECT 1 FROM item_transcripts t WHERE t.item_id = i.id)
		  ORDER BY i.id DESC
		  LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("videos needing transcripts: %w", err)
	}
	defer rows.Close()

	var out []VideoNeedingTranscript
	for rows.Next() {
		var v VideoNeedingTranscript
		if err := rows.Scan(&v.ItemID, &v.URL, &v.Title); err != nil {
			return nil, fmt.Errorf("scan video: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// SaveTranscript records an attempt, and folds the summary into the item.
//
// One transaction, because the summary landing on items.summary is the entire
// point: the classifier reads items.summary and nothing else, so a transcript
// stored without that update is work done for no effect. The FTS trigger on items
// picks the new summary up automatically.
func (db *DB) SaveTranscript(rec TranscriptRecord) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("save transcript: begin: %w", err)
	}
	defer tx.Rollback()

	// summary_base is captured on the first attempt and never overwritten -- see the
	// column comment in migration 005. COALESCE keeps whatever is already stored,
	// falling back to the item's current summary the first time through.
	if _, err := tx.Exec(
		`INSERT INTO item_transcripts
		        (item_id, status, text, lang, generated, chars, summary, summary_base, error)
		 VALUES (?, ?, ?, ?, ?, ?, ?,
		         COALESCE((SELECT summary FROM items WHERE id = ?), ''), ?)
		 ON CONFLICT(item_id) DO UPDATE SET
		        status = excluded.status, text = excluded.text, lang = excluded.lang,
		        generated = excluded.generated, chars = excluded.chars,
		        summary = excluded.summary, error = excluded.error,
		        fetched_at = datetime('now')`,
		rec.ItemID, rec.Status, nullIfEmpty(rec.Text), nullIfEmpty(rec.Lang),
		boolToInt(rec.Generated), len(rec.Text), nullIfEmpty(rec.Summary),
		rec.ItemID, nullIfEmpty(rec.Err),
	); err != nil {
		return fmt.Errorf("save transcript for item %d: %w", rec.ItemID, err)
	}

	if rec.Summary != "" {
		// Rebuilt from summary_base rather than prepended to what is there, so saving
		// twice -- or re-summarizing with a better model -- replaces the transcript
		// half instead of stacking another copy in front of it.
		//
		// The transcript summary leads because the classifier truncates at 400
		// characters; the channel's own blurb follows because it sometimes carries
		// links and chapter markers worth keeping.
		if _, err := tx.Exec(
			`UPDATE items
			    SET summary = ? || CASE
			          WHEN base.summary_base IS NULL OR base.summary_base = '' THEN ''
			          ELSE ' — ' || base.summary_base END
			  FROM (SELECT summary_base FROM item_transcripts WHERE item_id = ?) AS base
			  WHERE items.id = ?`,
			rec.Summary, rec.ItemID, rec.ItemID,
		); err != nil {
			return fmt.Errorf("apply transcript summary to item %d: %w", rec.ItemID, err)
		}
	}

	return tx.Commit()
}

// TranscriptCounts summarizes transcript state, for the status screen.
func (db *DB) TranscriptCounts() (map[string]int, error) {
	rows, err := db.Query(`SELECT status, count(*) FROM item_transcripts GROUP BY status`)
	if err != nil {
		return nil, fmt.Errorf("transcript counts: %w", err)
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return nil, fmt.Errorf("scan transcript count: %w", err)
		}
		out[status] = n
	}
	return out, rows.Err()
}
