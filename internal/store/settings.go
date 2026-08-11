package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// Setting returns the raw string value for key. Typing and defaulting are the
// config package's job; this layer stays dumb on purpose.
func (db *DB) Setting(key string) (string, bool, error) {
	var v string
	err := db.QueryRow("SELECT value FROM settings WHERE key = ?", key).Scan(&v)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("read setting %q: %w", key, err)
	}
	return v, true, nil
}

// Settings returns every key/value pair. Callers resolve config per run rather
// than caching, because §3.1 requires provider swaps to take effect without a
// restart.
func (db *DB) Settings() (map[string]string, error) {
	rows, err := db.Query("SELECT key, value FROM settings")
	if err != nil {
		return nil, fmt.Errorf("read settings: %w", err)
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("scan setting: %w", err)
		}
		out[k] = v
	}
	return out, rows.Err()
}

// SetSetting upserts a single key.
func (db *DB) SetSetting(key, value string) error {
	_, err := db.Exec(
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value,
	)
	if err != nil {
		return fmt.Errorf("write setting %q: %w", key, err)
	}
	return nil
}

// SetSettings upserts many keys in one transaction.
func (db *DB) SetSettings(kv map[string]string) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("write settings: begin: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
	)
	if err != nil {
		return fmt.Errorf("write settings: prepare: %w", err)
	}
	defer stmt.Close()

	for k, v := range kv {
		if _, err := stmt.Exec(k, v); err != nil {
			return fmt.Errorf("write setting %q: %w", k, err)
		}
	}
	return tx.Commit()
}
