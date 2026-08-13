package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// ManualSourceName is the row pasted links are attributed to.
const ManualSourceName = "Saved links"

// manualConfig marks a source as the manual inbox rather than a feed.
//
// Why a config marker and not a new sources.kind: the kind column carries
// CHECK (kind IN ('rss','hn_search','youtube_rss')) from migration 001, and SQLite
// cannot widen a CHECK with ALTER TABLE. The documented rebuild needs
// DROP TABLE sources, which fails while items.source_id references it -- and
// PRAGMA foreign_keys cannot be turned off from inside the migration runner,
// because each migration runs in a transaction and the pragma is a no-op there.
//
// So the inbox reuses kind 'rss' and is identified by this marker, and is created
// PAUSED. Paused is what makes it correct rather than merely convenient: Ingest
// reads ActiveSources(), so a source with active = 0 is never fetched, and an rss
// source with no URL would otherwise fail on every run and clutter the one list
// where a genuinely broken feed is supposed to stand out.
const manualConfig = `{"manual":true}`

// IsManualSource reports whether a source is the paste-a-link inbox.
func IsManualSource(s Source) bool {
	var cfg struct {
		Manual bool `json:"manual"`
	}
	if err := json.Unmarshal(s.Config, &cfg); err != nil {
		return false
	}
	return cfg.Manual
}

// ManualSourceID returns the inbox source, creating it on first use.
//
// Lazily rather than in Bootstrap: an owner who never pastes a link should not
// find a row in Settings they did not ask for and cannot explain.
func (db *DB) ManualSourceID() (int64, error) {
	// Matched on the marker alone, NOT on the name. The name is the owner's to
	// change in Settings, and including it here would mean a rename silently
	// produces a second inbox -- with the old one still holding every link saved
	// before it.
	var id int64
	err := db.QueryRow(
		`SELECT id FROM sources WHERE config = ? ORDER BY id LIMIT 1`, manualConfig,
	).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("look up the manual source: %w", err)
	}

	id, err = db.CreateSource(Source{
		Kind:   "rss",
		Name:   ManualSourceName,
		Config: json.RawMessage(manualConfig),
		Active: false, // never fetched; see manualConfig
	})
	if err != nil {
		return 0, fmt.Errorf("create the manual source: %w", err)
	}
	return id, nil
}
