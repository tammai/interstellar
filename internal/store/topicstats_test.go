package store

import (
	"testing"
	"time"
)

func today() string { return RunDate(time.Now()) }

func newTopic(t *testing.T, db *DB, title string) int64 {
	t.Helper()
	id, err := db.CreateTopic(Topic{Title: title, Definition: "Anything about " + title, Active: true})
	if err != nil {
		t.Fatalf("create topic %s: %v", title, err)
	}
	return id
}

func TestCommitClassifyPassWritesStatsAndCursor(t *testing.T) {
	db := openTemp(t)
	topic := newTopic(t, db, "Agents")

	err := db.CommitClassifyPass(today(), []TopicStat{
		{TopicID: topic, ItemsConsidered: 30, MatchesKept: 4, Batches: 1},
	}, map[int64]string{topic: "2026-08-12 09:40:00#17"})
	if err != nil {
		t.Fatalf("CommitClassifyPass: %v", err)
	}

	rates, err := db.TopicHitRates(7)
	if err != nil {
		t.Fatalf("TopicHitRates: %v", err)
	}
	if len(rates) != 1 {
		t.Fatalf("got %d rows, want 1", len(rates))
	}
	got := rates[0]
	if got.ItemsConsidered != 30 || got.MatchesKept != 4 || got.Batches != 1 {
		t.Errorf("got considered=%d matched=%d batches=%d, want 30/4/1",
			got.ItemsConsidered, got.MatchesKept, got.Batches)
	}
	if got.HitRate < 0.132 || got.HitRate > 0.134 {
		t.Errorf("hit rate = %v, want ~0.1333", got.HitRate)
	}
	if got.RunDays != 1 {
		t.Errorf("run days = %d, want 1", got.RunDays)
	}

	// The cursor rides along in the same transaction.
	got2, err := db.Topic(topic)
	if err != nil {
		t.Fatalf("Topic: %v", err)
	}
	if got2.ClassifiedThrough != "2026-08-12 09:40:00#17" {
		t.Errorf("cursor = %q, want the value the pass committed", got2.ClassifiedThrough)
	}
}

// A day can hold several passes -- the scheduled run plus any manual refresh --
// and each sees only what the watermark had not already covered. So the day's
// totals must add up, not overwrite.
func TestCommitClassifyPassAccumulatesWithinADay(t *testing.T) {
	db := openTemp(t)
	topic := newTopic(t, db, "Agents")
	day := today()

	for _, pass := range [][]TopicStat{
		{{TopicID: topic, ItemsConsidered: 30, MatchesKept: 3, Batches: 1}},
		{{TopicID: topic, ItemsConsidered: 12, MatchesKept: 2, Batches: 1}},
	} {
		if err := db.CommitClassifyPass(day, pass, nil); err != nil {
			t.Fatalf("CommitClassifyPass: %v", err)
		}
	}

	rates, _ := db.TopicHitRates(7)
	got := rates[0]
	if got.ItemsConsidered != 42 || got.MatchesKept != 5 || got.Batches != 2 {
		t.Errorf("got considered=%d matched=%d batches=%d, want 42/5/2 (passes must add up)",
			got.ItemsConsidered, got.MatchesKept, got.Batches)
	}
	// Still one date, so the window reports one day of data rather than two.
	if got.RunDays != 1 {
		t.Errorf("run days = %d, want 1: both passes ran on the same date", got.RunDays)
	}
}

// A topic missing from the cursor map keeps its cursor: a pass that could not
// move one topic must not blank it.
func TestCommitClassifyPassLeavesOtherCursorsAlone(t *testing.T) {
	db := openTemp(t)
	moved := newTopic(t, db, "Agents")
	stuck := newTopic(t, db, "Weights")

	if err := db.CommitClassifyPass(today(), nil, map[int64]string{
		moved: "2026-08-11 09:00:00#1", stuck: "2026-08-11 09:00:00#1",
	}); err != nil {
		t.Fatalf("seed cursors: %v", err)
	}
	if err := db.CommitClassifyPass(today(),
		[]TopicStat{{TopicID: moved, ItemsConsidered: 5, MatchesKept: 1, Batches: 1}},
		map[int64]string{moved: "2026-08-12 09:00:00#9"}); err != nil {
		t.Fatalf("CommitClassifyPass: %v", err)
	}

	if got, _ := db.Topic(stuck); got.ClassifiedThrough != "2026-08-11 09:00:00#1" {
		t.Errorf("untouched cursor = %q, want the seeded value", got.ClassifiedThrough)
	}
	if got, _ := db.Topic(moved); got.ClassifiedThrough != "2026-08-12 09:00:00#9" {
		t.Errorf("moved cursor = %q", got.ClassifiedThrough)
	}
}

// The whole point of the view is spotting a topic that produces nothing, so a
// topic with no stats at all has to come back -- not be dropped by the join.
func TestTopicHitRatesIncludesTopicsWithNoStats(t *testing.T) {
	db := openTemp(t)
	busy := newTopic(t, db, "Agents")
	newTopic(t, db, "Silent")

	if err := db.CommitClassifyPass(today(),
		[]TopicStat{{TopicID: busy, ItemsConsidered: 10, MatchesKept: 1, Batches: 1}}, nil); err != nil {
		t.Fatalf("CommitClassifyPass: %v", err)
	}

	rates, err := db.TopicHitRates(7)
	if err != nil {
		t.Fatalf("TopicHitRates: %v", err)
	}
	if len(rates) != 2 {
		t.Fatalf("got %d rows, want 2 (a topic with no stats must still appear)", len(rates))
	}

	var silent TopicHitRate
	for _, r := range rates {
		if r.Title == "Silent" {
			silent = r
		}
	}
	if silent.TopicID == 0 {
		t.Fatal(`the "Silent" topic is missing from the view`)
	}
	if silent.HasData() {
		t.Error("a topic with no stats reports HasData")
	}
	// -1, not 0: "never classified" must be distinguishable from "classified
	// plenty and matched none", which is the difference between doing nothing and
	// having a definition that needs rewriting.
	if silent.HitRate != -1 {
		t.Errorf("hit rate = %v for an unclassified topic, want the -1 sentinel", silent.HitRate)
	}
}

// Zero matches over a real denominator is a genuine 0%, and must not collapse into
// the same sentinel the never-classified case uses.
func TestTopicHitRatesDistinguishesZeroFromUnknown(t *testing.T) {
	db := openTemp(t)
	topic := newTopic(t, db, "Agents")

	if err := db.CommitClassifyPass(today(),
		[]TopicStat{{TopicID: topic, ItemsConsidered: 400, MatchesKept: 0, Batches: 14}}, nil); err != nil {
		t.Fatalf("CommitClassifyPass: %v", err)
	}

	rates, _ := db.TopicHitRates(7)
	got := rates[0]
	if !got.HasData() {
		t.Fatal("400 items considered should count as data")
	}
	if got.HitRate != 0 {
		t.Errorf("hit rate = %v, want exactly 0 for 0 of 400", got.HitRate)
	}
}

// Stats outside the window must not leak into it.
func TestTopicHitRatesHonorsTheWindow(t *testing.T) {
	db := openTemp(t)
	topic := newTopic(t, db, "Agents")

	old := RunDate(time.Now().AddDate(0, 0, -30))
	if err := db.CommitClassifyPass(old,
		[]TopicStat{{TopicID: topic, ItemsConsidered: 99, MatchesKept: 40, Batches: 4}}, nil); err != nil {
		t.Fatalf("CommitClassifyPass: %v", err)
	}
	if err := db.CommitClassifyPass(today(),
		[]TopicStat{{TopicID: topic, ItemsConsidered: 10, MatchesKept: 1, Batches: 1}}, nil); err != nil {
		t.Fatalf("CommitClassifyPass: %v", err)
	}

	week, _ := db.TopicHitRates(7)
	if week[0].ItemsConsidered != 10 {
		t.Errorf("7-day window considered %d, want 10: the 30-day-old row leaked in",
			week[0].ItemsConsidered)
	}
	if week[0].RunDays != 1 {
		t.Errorf("7-day window run days = %d, want 1", week[0].RunDays)
	}

	year, _ := db.TopicHitRates(365)
	if year[0].ItemsConsidered != 109 {
		t.Errorf("365-day window considered %d, want 109", year[0].ItemsConsidered)
	}
	if year[0].RunDays != 2 {
		t.Errorf("365-day window run days = %d, want 2", year[0].RunDays)
	}
}

// Deleting a topic drops its stats with it, the same way it drops matches and
// signals. A leftover row would keep a deleted topic's history in the totals.
func TestDeleteTopicRemovesItsStats(t *testing.T) {
	db := openTemp(t)
	topic := newTopic(t, db, "Agents")

	if err := db.CommitClassifyPass(today(),
		[]TopicStat{{TopicID: topic, ItemsConsidered: 10, MatchesKept: 1, Batches: 1}}, nil); err != nil {
		t.Fatalf("CommitClassifyPass: %v", err)
	}
	if err := db.DeleteTopic(topic); err != nil {
		t.Fatalf("DeleteTopic: %v", err)
	}

	var n int
	if err := db.QueryRow(`SELECT count(*) FROM topic_stats WHERE topic_id = ?`, topic).Scan(&n); err != nil {
		t.Fatalf("count stats: %v", err)
	}
	if n != 0 {
		t.Errorf("%d stat row(s) survived the topic", n)
	}
}
