package store

import (
	"database/sql"
	"encoding/json"
	"errors"
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

// AllSources returns every source, for the settings screen.
func (db *DB) AllSources() ([]Source, error) {
	rows, err := db.Query(
		`SELECT id, kind, name, config, active, last_run_at
		   FROM sources ORDER BY kind, name`,
	)
	if err != nil {
		return nil, fmt.Errorf("list sources: %w", err)
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

// CreateSource inserts a source. The kind is validated by the schema's CHECK
// constraint, so an unsupported kind fails here rather than at fetch time.
func (db *DB) CreateSource(s Source) (int64, error) {
	if len(s.Config) == 0 {
		s.Config = json.RawMessage("{}")
	}
	res, err := db.Exec(
		`INSERT INTO sources (kind, name, config, active) VALUES (?, ?, ?, ?)`,
		s.Kind, s.Name, string(s.Config), boolToInt(s.Active),
	)
	if err != nil {
		return 0, fmt.Errorf("create source: %w", err)
	}
	return res.LastInsertId()
}

// UpdateSource saves an edit.
func (db *DB) UpdateSource(s Source) error {
	if len(s.Config) == 0 {
		s.Config = json.RawMessage("{}")
	}
	res, err := db.Exec(
		`UPDATE sources SET kind = ?, name = ?, config = ?, active = ? WHERE id = ?`,
		s.Kind, s.Name, string(s.Config), boolToInt(s.Active), s.ID,
	)
	if err != nil {
		return fmt.Errorf("update source %d: %w", s.ID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteSource removes a source and its attributions.
//
// Items themselves stay. An item may be carried by other sources, and even when
// it isn't, deleting it would orphan any signal that cites it -- the sky would
// develop holes retroactively. The attribution row is what goes, which correctly
// lowers future source_counts without rewriting history.
func (db *DB) DeleteSource(id int64) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("delete source %d: begin: %w", id, err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM item_sources WHERE source_id = ?`, id); err != nil {
		return fmt.Errorf("delete source %d attributions: %w", id, err)
	}
	// items.source_id is a foreign key, so any item whose "first seen on" was
	// this source has to be repointed. Point it at another source that carried
	// it if one exists.
	if _, err := tx.Exec(
		`UPDATE items SET source_id = COALESCE(
		   (SELECT source_id FROM item_sources WHERE item_id = items.id LIMIT 1),
		   source_id)
		 WHERE source_id = ?`, id,
	); err != nil {
		return fmt.Errorf("repoint items from source %d: %w", id, err)
	}
	// Anything still pointing here has no other attribution: the item exists
	// only because this source found it, so it goes with it.
	if _, err := tx.Exec(
		`DELETE FROM signal_items WHERE item_id IN (SELECT id FROM items WHERE source_id = ?)`, id,
	); err != nil {
		return fmt.Errorf("unlink orphaned items from signals: %w", err)
	}
	if _, err := tx.Exec(
		`DELETE FROM matches WHERE item_id IN (SELECT id FROM items WHERE source_id = ?)`, id,
	); err != nil {
		return fmt.Errorf("delete orphaned matches: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM items WHERE source_id = ?`, id); err != nil {
		return fmt.Errorf("delete orphaned items: %w", err)
	}

	res, err := tx.Exec(`DELETE FROM sources WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete source %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// Source returns one source by id.
func (db *DB) SourceByID(id int64) (Source, error) {
	var s Source
	var cfg string
	err := db.QueryRow(
		`SELECT id, kind, name, config, active, last_run_at FROM sources WHERE id = ?`, id,
	).Scan(&s.ID, &s.Kind, &s.Name, &cfg, &s.Active, &s.LastRunAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Source{}, ErrNotFound
	}
	if err != nil {
		return Source{}, fmt.Errorf("read source %d: %w", id, err)
	}
	s.Config = json.RawMessage(cfg)
	return s, nil
}
