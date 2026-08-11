package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// Source is a configured feed.
type Source struct {
	ID        int64
	Kind      string
	Name      string
	Config    json.RawMessage
	Active    bool
	LastRunAt sql.NullString
}

// ActiveSources returns every source ingest should try, oldest-run first so a
// source that keeps failing doesn't permanently starve the ones behind it.
func (db *DB) ActiveSources() ([]Source, error) {
	rows, err := db.Query(
		`SELECT id, kind, name, config, active, last_run_at
		   FROM sources
		  WHERE active = 1
		  ORDER BY last_run_at IS NOT NULL, last_run_at, id`,
	)
	if err != nil {
		return nil, fmt.Errorf("list active sources: %w", err)
	}
	defer rows.Close()

	var out []Source
	for rows.Next() {
		var s Source
		var cfg string
		if err := rows.Scan(&s.ID, &s.Kind, &s.Name, &cfg, &s.Active, &s.LastRunAt); err != nil {
			return nil, fmt.Errorf("scan source: %w", err)
		}
		s.Config = json.RawMessage(cfg)
		out = append(out, s)
	}
	return out, rows.Err()
}

// MarkSourceRun records a successful fetch. §5.1: last_run_at advances on
// success only, so a persistently broken feed stays visibly stale in Settings
// instead of looking healthy.
func (db *DB) MarkSourceRun(id int64, at time.Time) error {
	_, err := db.Exec(
		`UPDATE sources SET last_run_at = ? WHERE id = ?`,
		at.UTC().Format(time.RFC3339), id,
	)
	if err != nil {
		return fmt.Errorf("mark source %d run: %w", id, err)
	}
	return nil
}
