package store

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// NewItem is one normalized entry ready to persist.
type NewItem struct {
	SourceID     int64
	CanonicalURL string
	Title        string
	Summary      string
	PublishedAt  time.Time // zero means unknown
	Detail       string    // per-source annotation, e.g. "412 points"
}

// UpsertItem stores an item and its source attribution, returning whether the
// item itself was new.
//
// The two writes are not the same question. The item is deduplicated by
// canonical URL, so the same story from three feeds yields one row. The
// attribution is per (item, source), so all three feeds are still recorded --
// and that is what signals.source_count counts, which is what makes a signal
// gold. Losing the second write would silently break the hot flag.
func (db *DB) UpsertItem(it NewItem) (itemID int64, isNew bool, err error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, false, fmt.Errorf("upsert item: begin: %w", err)
	}
	defer tx.Rollback()

	var published any
	if !it.PublishedAt.IsZero() {
		published = it.PublishedAt.UTC().Format(time.RFC3339)
	}

	res, err := tx.Exec(
		`INSERT INTO items (source_id, canonical_url, title, summary, published_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(canonical_url) DO NOTHING`,
		it.SourceID, it.CanonicalURL, it.Title, it.Summary, published,
	)
	if err != nil {
		return 0, false, fmt.Errorf("insert item %q: %w", it.CanonicalURL, err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return 0, false, fmt.Errorf("insert item %q: rows affected: %w", it.CanonicalURL, err)
	}
	isNew = affected > 0

	if isNew {
		itemID, err = res.LastInsertId()
		if err != nil {
			return 0, false, fmt.Errorf("insert item %q: last id: %w", it.CanonicalURL, err)
		}
	} else {
		err = tx.QueryRow(`SELECT id FROM items WHERE canonical_url = ?`, it.CanonicalURL).Scan(&itemID)
		if err != nil {
			return 0, false, fmt.Errorf("look up item %q: %w", it.CanonicalURL, err)
		}
	}

	// Attribution. DO UPDATE rather than DO NOTHING so a re-fetch refreshes a
	// changing annotation (an HN score climbs during the day) without creating
	// a duplicate row.
	_, err = tx.Exec(
		`INSERT INTO item_sources (item_id, source_id, detail)
		 VALUES (?, ?, ?)
		 ON CONFLICT(item_id, source_id) DO UPDATE SET detail = excluded.detail`,
		itemID, it.SourceID, nullIfEmpty(it.Detail),
	)
	if err != nil {
		return 0, false, fmt.Errorf("attribute item %d to source %d: %w", itemID, it.SourceID, err)
	}

	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("upsert item %q: commit: %w", it.CanonicalURL, err)
	}
	return itemID, isNew, nil
}

// ItemSourceNames returns the display names of every source that carried an
// item, with each source's annotation appended when it has one -- the panel's
// "Seen on" list.
func (db *DB) ItemSourceNames(itemID int64) ([]string, error) {
	rows, err := db.Query(
		`SELECT s.name, COALESCE(i.detail, '')
		   FROM item_sources i
		   JOIN sources s ON s.id = i.source_id
		  WHERE i.item_id = ?
		  ORDER BY s.name`,
		itemID,
	)
	if err != nil {
		return nil, fmt.Errorf("item %d sources: %w", itemID, err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var name, detail string
		if err := rows.Scan(&name, &detail); err != nil {
			return nil, fmt.Errorf("scan item source: %w", err)
		}
		if detail != "" {
			name = fmt.Sprintf("%s (%s)", name, detail)
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// ItemsSince returns items after the watermark, oldest first. This is the
// classifier's work queue: an empty watermark means everything.
func (db *DB) ItemsSince(watermark string, limit int) ([]ClassifiableItem, error) {
	at, id := ParseWatermark(watermark)
	query := `SELECT i.id, i.title, COALESCE(i.summary, ''), i.fetched_at,
	                 COALESCE((SELECT s.name FROM sources s WHERE s.id = i.source_id), '')
	            FROM items i
	           WHERE (i.fetched_at, i.id) > (?, ?)
	           ORDER BY i.fetched_at, i.id
	           LIMIT ?`
	rows, err := db.Query(query, at, id, limit)
	if err != nil {
		return nil, fmt.Errorf("items since %q: %w", watermark, err)
	}
	defer rows.Close()

	var out []ClassifiableItem
	for rows.Next() {
		var it ClassifiableItem
		if err := rows.Scan(&it.ID, &it.Title, &it.Summary, &it.FetchedAt, &it.SourceName); err != nil {
			return nil, fmt.Errorf("scan item: %w", err)
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// CountItemsSince counts what is still waiting behind a watermark -- the backlog a
// budget-limited pass left for the next run.
func (db *DB) CountItemsSince(watermark string) (int, error) {
	at, id := ParseWatermark(watermark)
	var n int
	err := db.QueryRow(
		`SELECT count(*) FROM items WHERE (fetched_at, id) > (?, ?)`, at, id).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count items since %q: %w", watermark, err)
	}
	return n, nil
}

// FormatWatermark is the classify and extract cursor: the last item a pass
// claimed, as fetched_at AND id.
//
// fetched_at alone is not a position. It has second resolution and ingest writes a
// feed's items inside one second -- 135 of them in one second on a real instance --
// while a pass checkpoints every 30. A timestamp-only mark that landed inside such
// a group made `fetched_at > mark` skip the rest of the group forever, which is the
// silent loss the prefix rule exists to prevent arriving by a different door.
// (fetched_at, id) is the same order ItemsSince reads in, so it is exact.
func FormatWatermark(fetchedAt string, id int64) string {
	return fmt.Sprintf("%s#%d", fetchedAt, id)
}

// LaterWatermark returns whichever of two watermarks is further along. An empty
// one is the start of time.
func LaterWatermark(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	aAt, aID := ParseWatermark(a)
	bAt, bID := ParseWatermark(b)
	if aAt > bAt || (aAt == bAt && aID >= bID) {
		return a
	}
	return b
}

// ParseWatermark splits a stored watermark into its cursor.
//
// A bare timestamp is the pre-cursor format, still in every upgraded instance's
// settings. It meant "everything up to and including this second", so it reads as
// that second with an id no item can exceed -- the upgrade re-reads nothing and
// skips nothing it did not already skip.
func ParseWatermark(s string) (fetchedAt string, id int64) {
	if i := strings.LastIndexByte(s, '#'); i >= 0 {
		if n, err := strconv.ParseInt(s[i+1:], 10, 64); err == nil {
			return s[:i], n
		}
	}
	return s, math.MaxInt64
}

// ClassifiableItem is the projection the classifier prompt needs -- deliberately
// narrow, because every field costs tokens on every batch.
type ClassifiableItem struct {
	ID         int64
	Title      string
	Summary    string
	SourceName string
	FetchedAt  string
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ErrNotFound is returned when a lookup by id finds nothing.
var ErrNotFound = errors.New("not found")

// ItemCountsBySource counts each source's contributions, via item_sources rather
// than items.source_id -- a source that was the second to carry a story still
// contributed it, and crediting only the first finder would undersell every feed
// that isn't fastest.
func (db *DB) ItemCountsBySource() (map[int64]int, error) {
	rows, err := db.Query(`SELECT source_id, count(*) FROM item_sources GROUP BY source_id`)
	if err != nil {
		return nil, fmt.Errorf("item counts by source: %w", err)
	}
	defer rows.Close()

	out := map[int64]int{}
	for rows.Next() {
		var sourceID int64
		var n int
		if err := rows.Scan(&sourceID, &n); err != nil {
			return nil, fmt.Errorf("scan item count: %w", err)
		}
		out[sourceID] = n
	}
	return out, rows.Err()
}

// TitleRef is an item's id, title and canonical URL, for duplicate detection.
//
// The URL is carried alongside the title because the two answer different
// questions and the order matters: an identical URL is an exact re-fetch, while a
// similar title at a DIFFERENT URL is a republish. Without the URL here, a second
// run finds each item's own title in the index and reports every unchanged item as
// a fuzzy merge -- which inflates the one counter that would warn of the matcher
// misfiring.
type TitleRef struct {
	ID    int64
	Title string
	URL   string
}

// RecentTitles returns items fetched in the last `days` days, newest first.
//
// Title and canonical URL together: see TitleRef.
//
// Loaded once per run and compared in memory, never queried per candidate item.
// The pool is a single connection: a lookup inside the ingest loop would be one
// query per feed entry, and the loop is already holding the connection to write
// with.
func (db *DB) RecentTitles(days, limit int) ([]TitleRef, error) {
	if days < 1 {
		days = 1
	}
	rows, err := db.Query(
		`SELECT id, title, canonical_url FROM items
		  WHERE fetched_at >= datetime('now', ?)
		  ORDER BY id DESC
		  LIMIT ?`,
		fmt.Sprintf("-%d days", days), limit,
	)
	if err != nil {
		return nil, fmt.Errorf("recent titles: %w", err)
	}
	defer rows.Close()

	var out []TitleRef
	for rows.Next() {
		var t TitleRef
		if err := rows.Scan(&t.ID, &t.Title, &t.URL); err != nil {
			return nil, fmt.Errorf("scan title: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// AttributeItem records that a source also carried an existing item.
//
// This is what a fuzzy duplicate becomes. Not a discard: two outlets carrying the
// same story is exactly the signal source_count exists to measure, and it drives
// the gold hot flag (§6). Dropping the second copy silently would throw that away.
func (db *DB) AttributeItem(itemID, sourceID int64, detail string) error {
	_, err := db.Exec(
		`INSERT INTO item_sources (item_id, source_id, detail)
		 VALUES (?, ?, ?)
		 ON CONFLICT(item_id, source_id) DO UPDATE SET detail = excluded.detail`,
		itemID, sourceID, nullIfEmpty(detail),
	)
	if err != nil {
		return fmt.Errorf("attribute item %d to source %d: %w", itemID, sourceID, err)
	}
	return nil
}
