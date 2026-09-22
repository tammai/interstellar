package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// Candidate is a name that keeps appearing and resolves to no entity.
type Candidate struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	FirstSeenAt string `json:"first_seen_at"`
	LastSeenAt  string `json:"last_seen_at"`
	// Mentions counts sightings; Sources counts DISTINCT sources among them. The
	// promotion rule reads Sources, because one aggregator repeating itself three
	// times is one outlet with an opinion, and three outlets is the field noticing
	// something. Mentions is shown alongside it so a name that is everywhere in
	// one place is visibly that.
	Mentions int      `json:"mentions"`
	Sources  int      `json:"sources"`
	Samples  []string `json:"samples,omitempty"` // item titles, for the review
}

// CandidateSighting is one unresolved mention, ready to record.
type CandidateSighting struct {
	Name     string
	ItemID   int64
	SourceID int64
	SeenAt   string
}

// RecordCandidates upserts names and their sightings.
//
// Idempotent: PRIMARY KEY (candidate_id, item_id) with INSERT OR IGNORE, so
// re-extracting an item cannot inflate a name's way over the promotion
// threshold. That matters more here than it looks -- the threshold is the only
// thing standing between the model's vocabulary and the owner's watchlist.
//
// A name that already exists as an entity or an alias is skipped by the caller,
// not here: this has no opinion about the watchlist, it only records what did not
// resolve.
func (db *DB) RecordCandidates(sightings []CandidateSighting) error {
	if len(sightings) == 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("record candidates: begin: %w", err)
	}
	defer tx.Rollback()

	ids := map[string]int64{}
	for _, s := range sightings {
		norm := NormalizeEntityName(s.Name)
		if norm == "" {
			continue
		}
		id, ok := ids[norm]
		if !ok {
			// Upsert by the normalized name. last_seen_at moves with MAX rather
			// than assignment because a backfill walks history forward and a
			// straight write would leave the oldest sighting in the batch.
			if err := tx.QueryRow(
				`INSERT INTO candidates (name, name_norm, first_seen_at, last_seen_at)
				 VALUES (?, ?, COALESCE(NULLIF(?, ''), datetime('now')),
				                COALESCE(NULLIF(?, ''), datetime('now')))
				 ON CONFLICT(name_norm) DO UPDATE SET
				   last_seen_at = MAX(candidates.last_seen_at, excluded.last_seen_at)
				 RETURNING id`,
				s.Name, norm, s.SeenAt, s.SeenAt,
			).Scan(&id); err != nil {
				return fmt.Errorf("upsert candidate %q: %w", s.Name, err)
			}
			ids[norm] = id
		}
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO candidate_sightings (candidate_id, item_id, source_id, seen_at)
			 VALUES (?, ?, ?, COALESCE(NULLIF(?, ''), datetime('now')))`,
			id, s.ItemID, s.SourceID, s.SeenAt,
		); err != nil {
			return fmt.Errorf("record sighting for %q: %w", s.Name, err)
		}
	}
	return tx.Commit()
}

// PromotableCandidates returns names seen by at least minSources DISTINCT sources
// inside the window, best-corroborated first.
//
// Proposed, never promoted automatically. Three sources makes a name worth
// SHOWING; a person decides whether it is worth tracking. Same contract as
// SuggestSources -- the model proposes, one click is the review -- and for the
// same reason: a watchlist that rewrites itself is one nobody can trust to mean
// what it said yesterday.
func (db *DB) PromotableCandidates(windowDays, minSources, sampleLimit int) ([]Candidate, error) {
	rows, err := db.Query(fmt.Sprintf(`
		SELECT c.id, c.name, c.first_seen_at, c.last_seen_at,
		       count(*) AS mentions, count(DISTINCT s.source_id) AS sources
		  FROM candidates c
		  JOIN candidate_sightings s ON s.candidate_id = c.id
		 WHERE c.dismissed = 0
		   AND c.promoted_entity_id IS NULL
		   AND s.seen_at >= datetime('now', '-%d days')
		 GROUP BY c.id
		HAVING sources >= ?
		 ORDER BY sources DESC, mentions DESC, c.last_seen_at DESC`, windowDays), minSources)
	if err != nil {
		return nil, fmt.Errorf("promotable candidates: %w", err)
	}

	var out []Candidate
	for rows.Next() {
		var c Candidate
		if err := rows.Scan(&c.ID, &c.Name, &c.FirstSeenAt, &c.LastSeenAt,
			&c.Mentions, &c.Sources); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan candidate: %w", err)
		}
		out = append(out, c)
	}
	// Closed before the sample queries below, not deferred: the pool holds one
	// connection, so asking for samples while these rows are open would wait on a
	// connection this loop has not released. store.SignalByID is the worked
	// example of the same shape.
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if sampleLimit <= 0 {
		return out, nil
	}
	for i := range out {
		titles, err := db.candidateSamples(out[i].ID, sampleLimit)
		if err != nil {
			return nil, err
		}
		out[i].Samples = titles
	}
	return out, nil
}

func (db *DB) candidateSamples(candidateID int64, limit int) ([]string, error) {
	rows, err := db.Query(
		`SELECT i.title FROM candidate_sightings s
		   JOIN items i ON i.id = s.item_id
		  WHERE s.candidate_id = ?
		  ORDER BY s.seen_at DESC LIMIT ?`, candidateID, limit)
	if err != nil {
		return nil, fmt.Errorf("candidate samples: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, fmt.Errorf("scan candidate sample: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// PromoteCandidate turns a candidate into an entity on the watchlist.
//
// The candidate row survives, carrying promoted_entity_id, so the name is neither
// offered again nor counted as still pending. Its sightings stay with it: they
// are the evidence that justified the promotion, and deleting them would make the
// decision unauditable a week later.
func (db *DB) PromoteCandidate(id int64, layer, kind string) (int64, error) {
	var name string
	err := db.QueryRow(
		`SELECT name FROM candidates WHERE id = ? AND promoted_entity_id IS NULL`, id,
	).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("read candidate %d: %w", id, err)
	}

	// Resolution is checked before the write, not caught after it: a name that is
	// already an alias must not become a second row for a thing the map already
	// resolves elsewhere. Same rule ImportWatchlist applies.
	aliases, err := db.EntityAliasMap()
	if err != nil {
		return 0, err
	}
	norm := NormalizeEntityName(name)
	if owner, taken := aliases[norm]; taken {
		return 0, fmt.Errorf("%q already resolves to entity %d", name, owner)
	}

	if kind == "" {
		kind = "tool"
	}

	tx, err := db.Begin()
	if err != nil {
		return 0, fmt.Errorf("promote candidate: begin: %w", err)
	}
	defer tx.Rollback()

	var entityID int64
	if err := tx.QueryRow(
		`INSERT INTO entities (name, name_norm, layer, kind, status)
		 VALUES (?, ?, ?, ?, 'active') RETURNING id`,
		name, norm, layer, kind,
	).Scan(&entityID); err != nil {
		return 0, fmt.Errorf("promote candidate %q: %w", name, err)
	}
	if _, err := tx.Exec(
		`UPDATE candidates SET promoted_entity_id = ? WHERE id = ?`, entityID, id,
	); err != nil {
		return 0, fmt.Errorf("mark candidate %d promoted: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("promote candidate: commit: %w", err)
	}
	return entityID, nil
}

// DismissCandidate marks a name as not worth tracking.
//
// Kept rather than deleted: a name rejected once ("Python", "GitHub") will be
// seen again next week, and without the record the same rejection is offered back
// every run. That is the difference between a suggestion and a nag.
func (db *DB) DismissCandidate(id int64) error {
	res, err := db.Exec(`UPDATE candidates SET dismissed = 1 WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("dismiss candidate %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// PruneCandidates removes never-promoted, never-dismissed names whose last
// sighting is older than idleDays.
//
// A name mentioned once and never again is noise, and noise that accumulates
// forever makes the review list useless -- which is the only way this feature
// fails: not by being wrong, but by being ignored. Dismissed and promoted rows
// are kept, because both are decisions.
func (db *DB) PruneCandidates(idleDays int) (int, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, fmt.Errorf("prune candidates: begin: %w", err)
	}
	defer tx.Rollback()

	cutoff := fmt.Sprintf("datetime('now', '-%d days')", idleDays)
	stale := `SELECT id FROM candidates
	           WHERE dismissed = 0 AND promoted_entity_id IS NULL
	             AND last_seen_at < ` + cutoff

	// Sightings first: they reference candidates, and no foreign key here declares
	// ON DELETE CASCADE.
	if _, err := tx.Exec(
		`DELETE FROM candidate_sightings WHERE candidate_id IN (` + stale + `)`,
	); err != nil {
		return 0, fmt.Errorf("prune candidate sightings: %w", err)
	}
	res, err := tx.Exec(`DELETE FROM candidates WHERE id IN (` + stale + `)`)
	if err != nil {
		return 0, fmt.Errorf("prune candidates: %w", err)
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("prune candidates: commit: %w", err)
	}
	return int(n), nil
}
