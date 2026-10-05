package httpapi

import (
	"net/http"
	"testing"
	"time"

	"github.com/tammai/interstellar/internal/store"
)

// The funnel follows the window's items from ingest to the sky, and each source
// is credited with what it carried.
func TestQualityDetailFunnelAndSourceYield(t *testing.T) {
	_, h, db := testServer(t)

	topics, err := db.AllTopics()
	if err != nil || len(topics) == 0 {
		t.Fatalf("need seeded topics: %v", err)
	}
	res, err := db.Exec(`INSERT INTO sources (kind, name, config, active) VALUES ('rss', 'Yield test', '{"url":"https://y.test/f"}', 1)`)
	if err != nil {
		t.Fatal(err)
	}
	src, _ := res.LastInsertId()

	var ids []int64
	for _, url := range []string{"https://y.test/a", "https://y.test/b", "https://y.test/c", "https://y.test/d"} {
		id, _, err := db.UpsertItem(store.NewItem{SourceID: src, CanonicalURL: url, Title: "Post"})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	// Two of four matched; one of those reached a signal.
	if err := db.SaveMatches([]store.Match{
		{ItemID: ids[0], TopicID: topics[0].ID, Score: 5},
		{ItemID: ids[1], TopicID: topics[0].ID, Score: 4},
	}); err != nil {
		t.Fatal(err)
	}
	day := store.RunDate(time.Now())
	if err := db.ReplaceSignals(day, topics[0].ID, []store.NewSignal{
		{Title: "A story", Reason: "It matters.", ItemIDs: []int64{ids[0]}},
	}); err != nil {
		t.Fatal(err)
	}

	w := do(t, h, "GET", "/api/quality/detail?days=7", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	got := decode[struct {
		Days int `json:"days"`
		store.QualityDetail
	}](t, w)

	f := got.Funnel
	if f.ItemsIngested != 4 || f.ItemsMatched != 2 || f.ItemsInSignals != 1 || f.Signals != 1 {
		t.Errorf("funnel = %+v, want 4 ingested, 2 matched, 1 in signals, 1 signal", f)
	}

	var found bool
	for _, y := range got.Sources {
		if y.SourceID != src {
			continue
		}
		found = true
		if y.Items != 4 || y.Matched != 2 || y.InSignals != 1 || y.MatchRate != 0.5 {
			t.Errorf("source yield = %+v, want 4/2/1 at 0.5", y)
		}
	}
	if !found {
		t.Error("the source is missing from the yield table")
	}
	if len(got.Daily) == 0 || got.Daily[0].Ingested != 4 {
		t.Errorf("daily = %+v, want today's 4 ingested first", got.Daily)
	}

	if w := do(t, h, "GET", "/api/quality/detail?days=0", ""); w.Code != http.StatusBadRequest {
		t.Errorf("days=0 status = %d, want 400", w.Code)
	}
}

// Status carries the classifier backlog the Schedule screen draws.
func TestStatusReportsClassifierBacklog(t *testing.T) {
	_, h, db := testServer(t)
	if _, err := db.Exec(`INSERT INTO sources (kind, name, config, active) VALUES ('rss', 'B', '{"url":"https://b.test/f"}', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.UpsertItem(store.NewItem{SourceID: 1, CanonicalURL: "https://b.test/a", Title: "Post"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE topics SET definition = 'Something.' WHERE definition = ''`); err != nil {
		t.Fatal(err)
	}

	got := decode[statusResponse](t, do(t, h, "GET", "/api/status", ""))
	if got.ItemsPending != 1 {
		t.Errorf("items_pending = %d, want 1", got.ItemsPending)
	}
	if len(got.Backlog) == 0 {
		t.Fatal("no per-topic backlog")
	}
	for _, b := range got.Backlog {
		if b.Pending != 1 {
			t.Errorf("topic %q pending = %d, want 1", b.Title, b.Pending)
		}
	}
	if got.CallCapRun == 0 {
		t.Error("call_cap_run missing")
	}
}
