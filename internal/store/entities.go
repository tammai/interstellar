package store

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// Entity is one thing the watchlist tracks.
type Entity struct {
	ID             int64  `json:"id"`
	Name           string `json:"name"`
	Layer          string `json:"layer"`
	Kind           string `json:"kind"`
	Status         string `json:"status"`
	FirstPartyURL  string `json:"first_party_url,omitempty"`
	LastSeenAt     string `json:"last_seen_at,omitempty"`
	LastVerifiedAt string `json:"last_verified_at,omitempty"`
}

// NormalizeEntityName folds a name to the form aliases are matched on.
//
// Lowercase, then every run of characters that is neither a letter nor a digit
// becomes a single space. That is what makes "Claude Code", "claude-code" and
// "Claude  Code" one key, which matters because the seed list carries exactly
// that pair as a name and its own alias.
//
// It walks RUNES, not bytes. §9 keeps topic definitions in Vietnamese and
// Vietnamese feeds carry Vietnamese names, so a byte-wise fold would split
// multi-byte runes -- the same class of bug sources.Truncate exists to prevent,
// and one that json.Marshal will not report because it substitutes U+FFFD rather
// than failing.
//
// Deliberately NOT aggressive: it does not strip punctuation inside a token to
// nothing, so "v0.dev" folds to "v0 dev" and stays distinct from "v0". Two
// entities merged by an over-eager fold share one history and neither half is
// recoverable, while two that stay separate are visible and fixable with an
// alias.
func NormalizeEntityName(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	space := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			space = false
			b.WriteRune(r)
			continue
		}
		space = true
	}
	return b.String()
}

// EntityAliasMap returns every name and alias, normalized, pointing at its
// entity id.
//
// Loaded whole rather than queried per item, for two reasons. The watchlist is
// ~134 rows, so the whole map is smaller than one batch of items. And the pool
// is capped at one connection, so a lookup issued while the extractor is walking
// an open *sql.Rows would wait forever on a connection the outer loop is holding
// -- it hangs rather than erroring, which is the worst way for it to fail.
func (db *DB) EntityAliasMap() (map[string]int64, error) {
	out := map[string]int64{}

	rows, err := db.Query(`SELECT name_norm, id FROM entities`)
	if err != nil {
		return nil, fmt.Errorf("read entity names: %w", err)
	}
	for rows.Next() {
		var norm string
		var id int64
		if err := rows.Scan(&norm, &id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan entity name: %w", err)
		}
		out[norm] = id
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = db.Query(`SELECT norm, entity_id FROM entity_aliases`)
	if err != nil {
		return nil, fmt.Errorf("read entity aliases: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var norm string
		var id int64
		if err := rows.Scan(&norm, &id); err != nil {
			return nil, fmt.Errorf("scan entity alias: %w", err)
		}
		// A name always wins over an alias. They cannot collide through the
		// importer, which refuses that case, but a registry edited some other way
		// should resolve to the entity that IS the name rather than one that merely
		// answers to it.
		if _, taken := out[norm]; !taken {
			out[norm] = id
		}
	}
	return out, rows.Err()
}

// EntityByID returns one entity.
func (db *DB) EntityByID(id int64) (Entity, error) {
	var e Entity
	var url, seen, verified sql.NullString
	err := db.QueryRow(
		`SELECT id, name, layer, kind, status, first_party_url, last_seen_at, last_verified_at
		   FROM entities WHERE id = ?`, id,
	).Scan(&e.ID, &e.Name, &e.Layer, &e.Kind, &e.Status, &url, &seen, &verified)
	if errors.Is(err, sql.ErrNoRows) {
		return Entity{}, ErrNotFound
	}
	if err != nil {
		return Entity{}, fmt.Errorf("read entity %d: %w", id, err)
	}
	e.FirstPartyURL, e.LastSeenAt, e.LastVerifiedAt = url.String, seen.String, verified.String
	return e, nil
}

// Entities lists the watchlist, layer then name.
func (db *DB) Entities() ([]Entity, error) {
	rows, err := db.Query(
		`SELECT id, name, layer, kind, status, first_party_url, last_seen_at, last_verified_at
		   FROM entities ORDER BY layer, name`)
	if err != nil {
		return nil, fmt.Errorf("list entities: %w", err)
	}
	defer rows.Close()

	var out []Entity
	for rows.Next() {
		var e Entity
		var url, seen, verified sql.NullString
		if err := rows.Scan(&e.ID, &e.Name, &e.Layer, &e.Kind, &e.Status,
			&url, &seen, &verified); err != nil {
			return nil, fmt.Errorf("scan entity: %w", err)
		}
		e.FirstPartyURL, e.LastSeenAt, e.LastVerifiedAt = url.String, seen.String, verified.String
		out = append(out, e)
	}
	return out, rows.Err()
}

// EntityImport is one row of a watchlist file.
type EntityImport struct {
	Name          string
	Aliases       []string
	Layer         string
	Kind          string
	Status        string
	FirstPartyURL string
}

// WatchlistResult reports what an import did.
type WatchlistResult struct {
	Added        int      `json:"added"`
	Updated      int      `json:"updated"`
	AliasesAdded int      `json:"aliases_added"`
	Skipped      int      `json:"skipped"`
	Conflicts    []string `json:"conflicts,omitempty"`
}

// ImportWatchlist loads entities and aliases, additively.
//
// Additive for the same reason source import is: a file goes stale as soon as
// either side is edited, and an import that deleted what the file omits would
// throw away the owner's own curation with no way back. An entity retired by
// hand stays retired; one dropped from the file stays in the database.
//
// Mutable fields ARE updated, because layer and status are mutable by design --
// a tool moving layer or being deprecated is precisely what the watchlist is for.
// An alias is only ever added.
//
// Conflicts are reported, never resolved. One alias resolving to two entities
// would silently attach claims to whichever row happened to be written first,
// and the two halves of a split history are individually plausible, so there is
// nothing downstream that could notice. The row is skipped and named.
func (db *DB) ImportWatchlist(rows []EntityImport) (WatchlistResult, error) {
	var res WatchlistResult

	// Load the whole resolution map before opening a transaction: the pool holds
	// one connection, so this cannot be interleaved with the writes below.
	taken, err := db.EntityAliasMap()
	if err != nil {
		return res, err
	}
	names, err := db.entityNameIDs()
	if err != nil {
		return res, err
	}

	tx, err := db.Begin()
	if err != nil {
		return res, fmt.Errorf("import watchlist: begin: %w", err)
	}
	defer tx.Rollback()

	conflicts := map[string]bool{}

	for _, r := range rows {
		name := strings.TrimSpace(r.Name)
		norm := NormalizeEntityName(name)
		if norm == "" {
			res.Skipped++
			continue
		}

		layer := strings.TrimSpace(r.Layer)
		if layer == "" {
			conflicts[fmt.Sprintf("%q has no layer", name)] = true
			res.Skipped++
			continue
		}
		kind := firstNonBlank(r.Kind, "tool")
		status := firstNonBlank(r.Status, "active")

		id, exists := names[norm]
		switch {
		case exists:
			if _, err := tx.Exec(
				`UPDATE entities SET name = ?, layer = ?, kind = ?, status = ?,
				        first_party_url = ?
				  WHERE id = ?`,
				name, layer, kind, status, nullIfEmpty(r.FirstPartyURL), id,
			); err != nil {
				return res, fmt.Errorf("update entity %q: %w", name, err)
			}
			res.Updated++
		default:
			// The name is free as a name, but it may already be somebody's alias.
			if owner, ok := taken[norm]; ok {
				conflicts[fmt.Sprintf("%q is already an alias of entity %d", name, owner)] = true
				res.Skipped++
				continue
			}
			err := tx.QueryRow(
				`INSERT INTO entities (name, name_norm, layer, kind, status, first_party_url)
				 VALUES (?, ?, ?, ?, ?, ?) RETURNING id`,
				name, norm, layer, kind, status, nullIfEmpty(r.FirstPartyURL),
			).Scan(&id)
			if err != nil {
				return res, fmt.Errorf("insert entity %q: %w", name, err)
			}
			res.Added++
			names[norm] = id
			taken[norm] = id
		}

		for _, alias := range r.Aliases {
			alias = strings.TrimSpace(alias)
			aNorm := NormalizeEntityName(alias)
			if aNorm == "" || aNorm == norm {
				continue // an alias equal to the name carries no information
			}
			if owner, ok := taken[aNorm]; ok {
				if owner != id {
					conflicts[fmt.Sprintf("alias %q wanted by %q and by entity %d",
						alias, name, owner)] = true
				}
				continue
			}
			if _, err := tx.Exec(
				`INSERT INTO entity_aliases (norm, alias, entity_id) VALUES (?, ?, ?)`,
				aNorm, alias, id,
			); err != nil {
				return res, fmt.Errorf("insert alias %q: %w", alias, err)
			}
			taken[aNorm] = id
			res.AliasesAdded++
		}
	}

	if err := tx.Commit(); err != nil {
		return res, fmt.Errorf("import watchlist: commit: %w", err)
	}

	for c := range conflicts {
		res.Conflicts = append(res.Conflicts, c)
	}
	sort.Strings(res.Conflicts)
	return res, nil
}

// entityNameIDs maps normalized NAMES only (not aliases) to ids.
func (db *DB) entityNameIDs() (map[string]int64, error) {
	rows, err := db.Query(`SELECT name_norm, id FROM entities`)
	if err != nil {
		return nil, fmt.Errorf("read entity names: %w", err)
	}
	defer rows.Close()

	out := map[string]int64{}
	for rows.Next() {
		var norm string
		var id int64
		if err := rows.Scan(&norm, &id); err != nil {
			return nil, fmt.Errorf("scan entity name: %w", err)
		}
		out[norm] = id
	}
	return out, rows.Err()
}

func firstNonBlank(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}
