package store

import (
	"fmt"
	"strings"
)

// Delta is one change worth telling someone about: an entity, a kind of change,
// and every claim that reported it.
//
// It is a GROUP, not a claim, and that is the whole point. The brief's rule says
// "hash unseen in 14 days", and claimHash covers entity, change type and url --
// which makes it a correct identity for one article and the wrong one for a
// push. TechCrunch and The Verge covering the same launch are two urls, so two
// hashes, so two notifications for one event: exactly the duplicate that
// collapseWeeklyStories exists to prevent in the round-up, arriving instead in a
// Slack channel.
//
// Hashing entity and change type alone fails the other way, suppressing a second
// genuine launch inside the window. So the grouping happens here and the window
// is applied to the group: one notification per entity per kind of change, with
// the corroborating sources listed. Several outlets raise confidence, they do not
// repeat -- the same reading signals.source_count gives a gold star.
type Delta struct {
	EntityID   int64  `json:"entity_id"`
	Entity     string `json:"entity"`
	Layer      string `json:"layer"`
	ChangeType string `json:"change_type"`
	// Summary is the best single description: the one from the most authoritative
	// source, first-party ahead of independent, breaking ties on confidence.
	Summary string `json:"summary"`
	// URL matches Summary -- the link a reader should open first.
	URL string `json:"url"`
	// Sources counts DISTINCT sources carrying it, the same corroboration measure
	// signals.source_count uses.
	Sources  int      `json:"sources"`
	Claims   []int64  `json:"claim_ids"`
	SeenAt   string   `json:"seen_at"`
	AllURLs  []string `json:"urls,omitempty"`
	Verified bool     `json:"verified"` // at least one first-party or vendor-blog claim
}

// sourceRank orders source types by how much they settle the question. A vendor
// announcing its own launch is the fact; a roundup mentioning it is an echo.
func sourceRank(t string) int {
	switch t {
	case "first_party":
		return 3
	case "vendor_blog":
		return 2
	case "independent":
		return 1
	default: // aggregator
		return 0
	}
}

// PendingDeltas returns unpushed changes worth sending, newest group first.
//
// windowDays suppresses a group whose entity and change type were already pushed
// inside the window -- the brief's fourteen days, applied to the group rather
// than to a single claim.
//
// Claims with change_type "none" never reach here: extraction does not store
// them, because nothing changed. Entities are on the watchlist by construction,
// since a claim cannot exist without one.
func (db *DB) PendingDeltas(windowDays, limit int) ([]Delta, error) {
	// Everything is read in one query and drained before any grouping, because
	// the pool holds one connection: a follow-up lookup issued while these rows
	// are open would wait on a connection this loop has not released.
	rows, err := db.Query(fmt.Sprintf(`
		SELECT c.id, c.entity_id, e.name, e.layer, c.change_type, c.summary, c.url,
		       c.source_type, COALESCE(c.confidence, 0), c.seen_at, i.source_id
		  FROM claims c
		  JOIN entities e ON e.id = c.entity_id
		  JOIN items    i ON i.id = c.item_id
		 WHERE c.pushed_at IS NULL
		   AND NOT EXISTS (
		         SELECT 1 FROM claims p
		          WHERE p.entity_id   = c.entity_id
		            AND p.change_type = c.change_type
		            AND p.pushed_at IS NOT NULL
		            AND p.pushed_at >= datetime('now', '-%d days')
		       )
		 ORDER BY c.seen_at DESC, c.id DESC`, windowDays))
	if err != nil {
		return nil, fmt.Errorf("pending deltas: %w", err)
	}
	defer rows.Close()

	type row struct {
		claimID, entityID, sourceID int64
		entity, layer, changeType   string
		summary, url, sourceType    string
		confidence                  float64
		seenAt                      string
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.claimID, &r.entityID, &r.entity, &r.layer, &r.changeType,
			&r.summary, &r.url, &r.sourceType, &r.confidence, &r.seenAt, &r.sourceID); err != nil {
			return nil, fmt.Errorf("scan pending delta: %w", err)
		}
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	type group struct {
		delta    Delta
		sources  map[int64]bool
		urls     map[string]bool
		bestRank int
		bestConf float64
	}
	index := map[string]*group{}
	var order []string

	for _, r := range all {
		key := fmt.Sprintf("%d\x00%s", r.entityID, r.changeType)
		g := index[key]
		if g == nil {
			g = &group{
				delta: Delta{
					EntityID: r.entityID, Entity: r.entity, Layer: r.layer,
					ChangeType: r.changeType, SeenAt: r.seenAt,
				},
				sources:  map[int64]bool{},
				urls:     map[string]bool{},
				bestRank: -1,
			}
			index[key] = g
			order = append(order, key)
		}
		g.delta.Claims = append(g.delta.Claims, r.claimID)
		g.sources[r.sourceID] = true
		g.urls[r.url] = true

		rank := sourceRank(r.sourceType)
		if rank >= 2 {
			g.delta.Verified = true
		}
		// Most authoritative source wins the headline; confidence breaks ties.
		// Rows arrive newest first, so a strict > keeps the newest of equals.
		if rank > g.bestRank || (rank == g.bestRank && r.confidence > g.bestConf) {
			g.bestRank, g.bestConf = rank, r.confidence
			g.delta.Summary = r.summary
			g.delta.URL = r.url
		}
	}

	out := make([]Delta, 0, len(order))
	for _, key := range order {
		g := index[key]
		g.delta.Sources = len(g.sources)
		for u := range g.urls {
			g.delta.AllURLs = append(g.delta.AllURLs, u)
		}
		out = append(out, g.delta)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// MarkDeltasPushed stamps every claim in the given deltas as sent.
//
// Takes the claim ids the caller actually sent rather than re-running the
// selection: between selecting and sending, the delivery can fail, and a
// stamp applied to a wider set than was delivered loses changes silently.
func (db *DB) MarkDeltasPushed(claimIDs []int64) error {
	if len(claimIDs) == 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("mark deltas pushed: begin: %w", err)
	}
	defer tx.Rollback()

	// One statement, built from placeholders rather than interpolated ids: these
	// come from a query rather than a user, but the habit is what stops the one
	// that does not.
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(claimIDs)), ",")
	args := make([]any, 0, len(claimIDs))
	for _, id := range claimIDs {
		args = append(args, id)
	}
	if _, err := tx.Exec(
		`UPDATE claims SET pushed_at = datetime('now') WHERE id IN (`+placeholders+`)`,
		args...,
	); err != nil {
		return fmt.Errorf("mark deltas pushed: %w", err)
	}
	return tx.Commit()
}
