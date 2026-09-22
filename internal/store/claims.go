package store

import (
	"database/sql"
	"fmt"
)

// NewClaim is one extracted change, ready to write.
type NewClaim struct {
	EntityID   int64
	ItemID     int64
	ChangeType string
	SourceType string
	Summary    string
	URL        string
	Confidence float64 // 0 means the model gave none; stored as NULL
	Hash       string
	SeenAt     string
}

// EntitySighting records that an entity was mentioned, whether or not anything
// changed about it.
//
// Separate from a claim because "mentioned" and "changed" are different facts and
// the staleness job needs the first one. An item that discusses Cursor without
// announcing anything produces no claim -- there is no change to record -- but it
// is still evidence that Cursor is being written about, which is exactly what
// distinguishes a quiet tool from a dead one.
type EntitySighting struct {
	EntityID int64
	At       string
	// Verified marks a sighting from the vendor itself (source_type first_party or
	// vendor_blog). Only these advance last_verified_at: "nobody has written about
	// this in 60 days" and "the vendor has not shipped in 60 days" are both worth
	// knowing, and a staleness job fed by the first one measures press attention.
	Verified bool
}

// ExtractableItem is the projection the extractor needs.
//
// Wider than ClassifiableItem on purpose rather than by extending it: the URL and
// the source kind never reach a prompt, they are used locally to derive
// source_type. ClassifiableItem's comment says it is narrow because every field
// costs tokens on every batch, and quietly adding fields that do not is how that
// stops being true of it.
type ExtractableItem struct {
	ID           int64
	Title        string
	Summary      string
	CanonicalURL string
	SourceName   string
	SourceKind   string
	FetchedAt    string
}

// ItemsForExtraction returns items fetched after the watermark, oldest first.
func (db *DB) ItemsForExtraction(watermark string, limit int) ([]ExtractableItem, error) {
	rows, err := db.Query(
		`SELECT i.id, i.title, COALESCE(i.summary, ''), i.canonical_url,
		        COALESCE(s.name, ''), COALESCE(s.kind, ''), i.fetched_at
		   FROM items i
		   LEFT JOIN sources s ON s.id = i.source_id
		  WHERE i.fetched_at > ?
		  ORDER BY i.fetched_at, i.id
		  LIMIT ?`, watermark, limit)
	if err != nil {
		return nil, fmt.Errorf("items for extraction since %q: %w", watermark, err)
	}
	defer rows.Close()

	var out []ExtractableItem
	for rows.Next() {
		var it ExtractableItem
		if err := rows.Scan(&it.ID, &it.Title, &it.Summary, &it.CanonicalURL,
			&it.SourceName, &it.SourceKind, &it.FetchedAt); err != nil {
			return nil, fmt.Errorf("scan extractable item: %w", err)
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// CommitExtractPass writes a pass's claims, sightings and watermark together.
//
// One transaction, for the same reason CommitClassifyPass is one: these are not
// independent writes. A watermark that advanced without its claims would skip
// those items forever, and claims written without the watermark would be
// re-extracted next run -- harmless for the claims themselves, which are
// idempotent under UNIQUE (item_id, entity_id, change_type), but it would move
// last_seen_at again on a second pass over the same evidence and make a burst of
// attention out of one.
//
// An empty watermark means "do not move it", which is how a partially successful
// pass commits the work it did without claiming items it never reached.
func (db *DB) CommitExtractPass(claims []NewClaim, sightings []EntitySighting, watermark string) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("commit extract pass: begin: %w", err)
	}
	defer tx.Rollback()

	for _, c := range claims {
		var conf any
		if c.Confidence > 0 {
			conf = c.Confidence
		}
		// OR IGNORE, not OR REPLACE: the first extraction of a change is the one
		// dated correctly. A re-run over the same item must not restamp seen_at and
		// make an old change look like today's news.
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO claims
			   (entity_id, item_id, change_type, source_type, summary, url, confidence, hash, seen_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, COALESCE(NULLIF(?, ''), datetime('now')))`,
			c.EntityID, c.ItemID, c.ChangeType, c.SourceType, c.Summary, c.URL,
			conf, c.Hash, c.SeenAt,
		); err != nil {
			return fmt.Errorf("insert claim for entity %d: %w", c.EntityID, err)
		}
	}

	for _, s := range sightings {
		// MAX, never assignment: items arrive oldest first and a backfill walks
		// history forward, so a straight write would leave last_seen_at holding the
		// oldest thing in the batch rather than the newest.
		if _, err := tx.Exec(
			`UPDATE entities SET last_seen_at = MAX(COALESCE(last_seen_at, ''), ?) WHERE id = ?`,
			s.At, s.EntityID,
		); err != nil {
			return fmt.Errorf("update entity %d last_seen_at: %w", s.EntityID, err)
		}
		if s.Verified {
			if _, err := tx.Exec(
				`UPDATE entities SET last_verified_at = MAX(COALESCE(last_verified_at, ''), ?)
				  WHERE id = ?`,
				s.At, s.EntityID,
			); err != nil {
				return fmt.Errorf("update entity %d last_verified_at: %w", s.EntityID, err)
			}
		}
	}

	if watermark != "" {
		if _, err := tx.Exec(
			`INSERT INTO settings (key, value) VALUES (?, ?)
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
			KeyExtractWatermark, watermark,
		); err != nil {
			return fmt.Errorf("set extract watermark: %w", err)
		}
	}

	return tx.Commit()
}

// Claim is one stored claim with its entity's name, for reading back.
type Claim struct {
	ID         int64   `json:"id"`
	EntityID   int64   `json:"entity_id"`
	Entity     string  `json:"entity"`
	Layer      string  `json:"layer"`
	ItemID     int64   `json:"item_id"`
	ChangeType string  `json:"change_type"`
	SourceType string  `json:"source_type"`
	Summary    string  `json:"summary"`
	URL        string  `json:"url"`
	Confidence float64 `json:"confidence,omitempty"`
	Hash       string  `json:"hash"`
	SeenAt     string  `json:"seen_at"`
}

// ClaimsSince returns claims seen at or after a timestamp, newest first.
func (db *DB) ClaimsSince(since string, limit int) ([]Claim, error) {
	rows, err := db.Query(
		`SELECT c.id, c.entity_id, e.name, e.layer, c.item_id, c.change_type,
		        c.source_type, c.summary, c.url, c.confidence, c.hash, c.seen_at
		   FROM claims c
		   JOIN entities e ON e.id = c.entity_id
		  WHERE c.seen_at >= ?
		  ORDER BY c.seen_at DESC, c.id DESC
		  LIMIT ?`, since, limit)
	if err != nil {
		return nil, fmt.Errorf("claims since %q: %w", since, err)
	}
	defer rows.Close()

	var out []Claim
	for rows.Next() {
		var c Claim
		var conf sql.NullFloat64
		if err := rows.Scan(&c.ID, &c.EntityID, &c.Entity, &c.Layer, &c.ItemID,
			&c.ChangeType, &c.SourceType, &c.Summary, &c.URL, &conf, &c.Hash,
			&c.SeenAt); err != nil {
			return nil, fmt.Errorf("scan claim: %w", err)
		}
		c.Confidence = conf.Float64
		out = append(out, c)
	}
	return out, rows.Err()
}

// ClaimHashesSince returns the claim hashes seen at or after a timestamp.
//
// This is the delta rule's "unseen in 14 days" check, done as one query rather
// than one per candidate claim: the pool is capped at one connection, so a lookup
// issued while a result set is open would deadlock, and the whole set of recent
// hashes is small enough to hold.
func (db *DB) ClaimHashesSince(since string) (map[string]bool, error) {
	rows, err := db.Query(`SELECT DISTINCT hash FROM claims WHERE seen_at >= ?`, since)
	if err != nil {
		return nil, fmt.Errorf("claim hashes since %q: %w", since, err)
	}
	defer rows.Close()

	out := map[string]bool{}
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, fmt.Errorf("scan claim hash: %w", err)
		}
		out[h] = true
	}
	return out, rows.Err()
}
