// Package store owns the SQLite database: connection setup, schema migrations,
// and the queries the rest of the app runs.
//
// Concurrency model: single user, single writer. The pool is capped at one
// connection, which removes SQLITE_BUSY as an entire category of bug at a cost
// (serialized reads) that is irrelevant at this scale.
package store

import (
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	_ "modernc.org/sqlite" // pure-Go driver: no cgo, so multi-arch images stay trivial
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// DB wraps *sql.DB so callers depend on this package rather than database/sql.
type DB struct {
	*sql.DB
	Path string
}

// Open connects to the SQLite file at path, applies pragmas, and migrates to the
// latest schema version. It is safe to call against a brand-new empty volume.
func Open(path string) (*DB, error) {
	dsn := path + "?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(on)" +
		"&_pragma=synchronous(normal)"

	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	// One writer, and therefore one connection. See package doc.
	sqlDB.SetMaxOpenConns(1)

	if err := sqlDB.Ping(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("ping %s: %w", path, err)
	}

	db := &DB{DB: sqlDB, Path: path}
	if err := db.migrate(); err != nil {
		sqlDB.Close()
		return nil, err
	}
	return db, nil
}

type migration struct {
	version int
	name    string
	sql     string
}

// migrate applies every embedded migration numbered above the database's current
// user_version, each in its own transaction. Migrations are append-only: never
// edit one that has shipped, add the next number instead.
func (db *DB) migrate() error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}

	var current int
	if err := db.QueryRow("PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}

	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("migration %d (%s): begin: %w", m.version, m.name, err)
		}
		if _, err := tx.Exec(m.sql); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d (%s): %w", m.version, m.name, err)
		}
		// PRAGMA user_version does not accept a bound parameter.
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", m.version)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d (%s): set user_version: %w", m.version, m.name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migration %d (%s): commit: %w", m.version, m.name, err)
		}
	}
	return nil
}

// loadMigrations reads migrations/NNN_name.sql and returns them in version order.
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read migrations dir: %w", err)
	}

	var out []migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		prefix, rest, found := strings.Cut(strings.TrimSuffix(e.Name(), ".sql"), "_")
		if !found {
			return nil, fmt.Errorf("migration %q: want NNN_name.sql", e.Name())
		}
		version, err := strconv.Atoi(prefix)
		if err != nil {
			return nil, fmt.Errorf("migration %q: bad version prefix: %w", e.Name(), err)
		}
		body, err := migrationFS.ReadFile(filepath.Join("migrations", e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", e.Name(), err)
		}
		out = append(out, migration{version: version, name: rest, sql: string(body)})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })

	for i, m := range out {
		if m.version != i+1 {
			return nil, fmt.Errorf("migrations must be numbered 1..N with no gaps; found %d at position %d", m.version, i+1)
		}
	}
	return out, nil
}

// Version reports the schema version currently applied.
func (db *DB) Version() (int, error) {
	var v int
	err := db.QueryRow("PRAGMA user_version").Scan(&v)
	return v, err
}
