package store

import "testing"

// seedSameSecond inserts n items that all carry one fetched_at, which is what
// ingest produces for a feed: its items land inside a single second.
func seedSameSecond(t *testing.T, db *DB, n int) []int64 {
	t.Helper()
	res, err := db.Exec(
		`INSERT INTO sources (kind, name, config, active) VALUES ('rss', 'Blog', '{"url":"https://x.test/f"}', 1)`)
	if err != nil {
		t.Fatalf("insert source: %v", err)
	}
	src, _ := res.LastInsertId()

	var ids []int64
	for i := range n {
		id, _, err := db.UpsertItem(NewItem{
			SourceID: src, CanonicalURL: "https://x.test/" + string(rune('a'+i)), Title: "Post",
		})
		if err != nil {
			t.Fatalf("upsert item: %v", err)
		}
		ids = append(ids, id)
	}
	if _, err := db.Exec(`UPDATE items SET fetched_at = '2026-09-05 09:30:19'`); err != nil {
		t.Fatalf("pin fetched_at: %v", err)
	}
	return ids
}

// A cursor inside a same-second group must resume inside it. A timestamp-only mark
// read `fetched_at > mark` and skipped the rest of the group forever.
func TestItemsSinceResumesInsideASameSecondGroup(t *testing.T) {
	db := openTemp(t)
	ids := seedSameSecond(t, db, 5)

	mark := FormatWatermark("2026-09-05 09:30:19", ids[1])
	got, err := db.ItemsSince(mark, 100)
	if err != nil {
		t.Fatalf("ItemsSince: %v", err)
	}
	if len(got) != 3 || got[0].ID != ids[2] {
		t.Fatalf("got %d items starting at %v, want the 3 after id %d", len(got), got, ids[1])
	}

	if n, err := db.CountItemsSince(mark); err != nil || n != 3 {
		t.Errorf("CountItemsSince = %d, %v; want 3", n, err)
	}

	extract, err := db.ItemsForExtraction(mark, 100)
	if err != nil {
		t.Fatalf("ItemsForExtraction: %v", err)
	}
	if len(extract) != 3 || extract[0].ID != ids[2] {
		t.Errorf("extraction got %d items, want the same 3", len(extract))
	}
}

// An upgraded instance holds a bare timestamp. It meant "this second is done", and
// must keep meaning exactly that: no re-read, no newly skipped items.
func TestLegacyWatermarkKeepsItsMeaning(t *testing.T) {
	db := openTemp(t)
	seedSameSecond(t, db, 3)

	if got, _ := db.ItemsSince("2026-09-05 09:30:19", 100); len(got) != 0 {
		t.Errorf("legacy mark at the group's second returned %d items, want 0", len(got))
	}
	if got, _ := db.ItemsSince("2026-09-05 09:30:18", 100); len(got) != 3 {
		t.Errorf("legacy mark a second earlier returned %d items, want 3", len(got))
	}
	if got, _ := db.ItemsSince("", 100); len(got) != 3 {
		t.Errorf("empty mark returned %d items, want all 3", len(got))
	}
}

func TestWatermarkRoundTrips(t *testing.T) {
	at, id := ParseWatermark(FormatWatermark("2026-09-05 09:30:19", 42))
	if at != "2026-09-05 09:30:19" || id != 42 {
		t.Errorf("round trip = %q, %d", at, id)
	}
}
