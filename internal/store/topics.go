package store

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
)

// Topic is one constellation.
type Topic struct {
	ID         int64
	Title      string
	Definition string
	Priority   int
	Active     bool
	PosX       float64
	PosY       float64
	// ClassifiedThrough is this topic's classify cursor (store.FormatWatermark),
	// empty when it has never been classified. Written only by
	// CommitClassifyPass -- never by CreateTopic or UpdateTopic.
	ClassifiedThrough string
}

// NeedsSetup reports whether the owner still has to write this topic's
// definition. §7 shows a setup hint until topics are edited, and an empty
// definition is that signal -- the classifier has nothing to judge against
// without one.
func (t Topic) NeedsSetup() bool { return t.Definition == "" }

// ActiveTopics returns topics to classify against, highest priority first.
func (db *DB) ActiveTopics() ([]Topic, error) {
	return db.topics("WHERE active = 1", "ORDER BY priority DESC, id")
}

// AllTopics returns every topic, for the settings screen.
func (db *DB) AllTopics() ([]Topic, error) {
	return db.topics("", "ORDER BY priority DESC, id")
}

func (db *DB) topics(where, order string) ([]Topic, error) {
	query := fmt.Sprintf(
		`SELECT id, title, definition, priority, active,
		        COALESCE(pos_x, 0), COALESCE(pos_y, 0), COALESCE(classified_through, '')
		   FROM topics %s %s`, where, order)

	rows, err := db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("list topics: %w", err)
	}
	defer rows.Close()

	var out []Topic
	for rows.Next() {
		var t Topic
		if err := rows.Scan(&t.ID, &t.Title, &t.Definition, &t.Priority, &t.Active, &t.PosX, &t.PosY,
			&t.ClassifiedThrough); err != nil {
			return nil, fmt.Errorf("scan topic: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// topicPositions reads only what placing a new constellation needs. Narrower than
// AllTopics on purpose: migration tests create topics against older schemas, and
// a column added since must not break a placement that never reads it.
func (db *DB) topicPositions() ([]Topic, error) {
	rows, err := db.Query(`SELECT id, COALESCE(pos_x, 0), COALESCE(pos_y, 0) FROM topics ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("topic positions: %w", err)
	}
	defer rows.Close()
	var out []Topic
	for rows.Next() {
		var t Topic
		if err := rows.Scan(&t.ID, &t.PosX, &t.PosY); err != nil {
			return nil, fmt.Errorf("scan topic position: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Topic returns one topic by id.
func (db *DB) Topic(id int64) (Topic, error) {
	var t Topic
	err := db.QueryRow(
		`SELECT id, title, definition, priority, active,
		        COALESCE(pos_x, 0), COALESCE(pos_y, 0), COALESCE(classified_through, '')
		   FROM topics WHERE id = ?`, id,
	).Scan(&t.ID, &t.Title, &t.Definition, &t.Priority, &t.Active, &t.PosX, &t.PosY, &t.ClassifiedThrough)
	if errors.Is(err, sql.ErrNoRows) {
		return Topic{}, ErrNotFound
	}
	if err != nil {
		return Topic{}, fmt.Errorf("read topic %d: %w", id, err)
	}
	return t, nil
}

// CreateTopic inserts a topic, assigning a constellation position if none was
// given.
func (db *DB) CreateTopic(t Topic) (int64, error) {
	if t.PosX == 0 && t.PosY == 0 {
		existing, err := db.topicPositions()
		if err != nil {
			return 0, err
		}
		t.PosX, t.PosY = NextTopicPosition(existing)
	}

	res, err := db.Exec(
		`INSERT INTO topics (title, definition, priority, active, pos_x, pos_y)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		t.Title, t.Definition, t.Priority, boolToInt(t.Active), t.PosX, t.PosY,
	)
	if err != nil {
		return 0, fmt.Errorf("create topic: %w", err)
	}
	return res.LastInsertId()
}

// UpdateTopic saves an edit. Position is intentionally not updatable here: §6
// protects spatial memory, so a constellation stays where the owner learned it.
func (db *DB) UpdateTopic(t Topic) error {
	res, err := db.Exec(
		`UPDATE topics SET title = ?, definition = ?, priority = ?, active = ? WHERE id = ?`,
		t.Title, t.Definition, t.Priority, boolToInt(t.Active), t.ID,
	)
	if err != nil {
		return fmt.Errorf("update topic %d: %w", t.ID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteTopic removes a topic and everything derived from it. Matches and
// signals are meaningless without their topic, so they go too -- but items stay,
// because they may be carried by other topics and re-fetching them is wasteful.
func (db *DB) DeleteTopic(id int64) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("delete topic %d: begin: %w", id, err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(
		`DELETE FROM signal_items WHERE signal_id IN (SELECT id FROM signals WHERE topic_id = ?)`, id,
	); err != nil {
		return fmt.Errorf("delete topic %d signal items: %w", id, err)
	}
	if _, err := tx.Exec(`DELETE FROM signals WHERE topic_id = ?`, id); err != nil {
		return fmt.Errorf("delete topic %d signals: %w", id, err)
	}
	if _, err := tx.Exec(`DELETE FROM matches WHERE topic_id = ?`, id); err != nil {
		return fmt.Errorf("delete topic %d matches: %w", id, err)
	}
	if _, err := tx.Exec(`DELETE FROM topic_stats WHERE topic_id = ?`, id); err != nil {
		return fmt.Errorf("delete topic %d stats: %w", id, err)
	}
	res, err := tx.Exec(`DELETE FROM topics WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete topic %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// NextTopicPosition picks a constellation centre for a new topic.
//
// Golden-angle placement on a disc: it spreads points evenly without ever
// recomputing the ones already placed, which is exactly the constraint §6
// imposes. A new topic gets its own patch of sky and every existing
// constellation stays put.
func NextTopicPosition(existing []Topic) (x, y float64) {
	const goldenAngle = 2.39996323 // radians, ~137.5 degrees

	// Keep stars inside the usable canvas: the header and legend occupy the
	// edges, so positions are clamped well away from 0 and 1.
	const (
		cx, cy   = 0.5, 0.48
		maxRadX  = 0.38
		maxRadY  = 0.34
		minTries = 40
	)

	best := struct {
		x, y, dist float64
	}{x: cx, y: cy, dist: -1}

	for i := len(existing); i < len(existing)+minTries; i++ {
		// sqrt spacing keeps density even rather than clustering at the centre.
		frac := math.Sqrt(float64(i+1) / float64(i+2))
		angle := float64(i) * goldenAngle
		px := cx + math.Cos(angle)*maxRadX*frac
		py := cy + math.Sin(angle)*maxRadY*frac

		nearest := math.Inf(1)
		for _, t := range existing {
			d := math.Hypot(px-t.PosX, py-t.PosY)
			if d < nearest {
				nearest = d
			}
		}
		if nearest > best.dist {
			best.x, best.y, best.dist = px, py, nearest
		}
		// Far enough from everything to stop looking.
		if nearest > 0.18 {
			break
		}
	}
	return clamp01(best.x), clamp01(best.y)
}

func clamp01(v float64) float64 {
	return math.Min(0.95, math.Max(0.05, v))
}
