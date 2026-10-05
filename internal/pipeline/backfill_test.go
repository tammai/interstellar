package pipeline

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// promptsFor counts the classifier prompts a fake model received for one topic.
func promptsFor(model *fakeModel, topic string) int {
	model.mu.Lock()
	defer model.mu.Unlock()
	n := 0
	for _, p := range model.prompts {
		if strings.Contains(p, "TOPIC: "+topic) {
			n++
		}
	}
	return n
}

// A topic added after the others have run used to start from the shared
// watermark, so it never judged anything that arrived before it existed -- a new
// constellation stayed empty until fresh news happened to land. Now it catches up
// over the sky's window, and the topics already current pay nothing for it.
func TestNewTopicBackfillsWithoutReclassifyingTheOthers(t *testing.T) {
	db := testDB(t)
	src := seedSource(t, db, "Blog")
	seedTopic(t, db, "Agents", "Agent harnesses.")
	for i := range 5 {
		seedItem(t, db, src, fmt.Sprintf("https://x.test/%d", i), fmt.Sprintf("Post %d", i))
	}

	model := newFakeModel(t, scoreAllHigh)
	cfg := testConfig()
	if _, err := Classify(context.Background(), db, clientFor(t, db, model), cfg, testRunDate, quietLogger()); err != nil {
		t.Fatalf("first Classify: %v", err)
	}

	added := seedTopic(t, db, "Open weights", "Open-weight releases.")

	backlog, pending, err := ClassifyBacklog(db, cfg)
	if err != nil {
		t.Fatalf("ClassifyBacklog: %v", err)
	}
	if pending != 5 {
		t.Errorf("pending = %d, want the 5 items the new topic has not judged", pending)
	}
	for _, b := range backlog {
		if b.TopicID == added && (!b.CatchingUp || b.Pending != 5) {
			t.Errorf("new topic backlog = %+v, want catching up on 5", b)
		}
		if b.TopicID != added && (b.CatchingUp || b.Pending != 0) {
			t.Errorf("current topic backlog = %+v, want 0 and not catching up", b)
		}
	}

	before := promptsFor(model, "Agents")
	res, err := Classify(context.Background(), db, clientFor(t, db, model), cfg, testRunDate, quietLogger())
	if err != nil {
		t.Fatalf("second Classify: %v", err)
	}
	if n := countRows(t, db, "SELECT count(*) FROM matches WHERE topic_id = ?", added); n != 5 {
		t.Errorf("new topic has %d matches, want all 5 earlier items", n)
	}
	if extra := promptsFor(model, "Agents") - before; extra != 0 {
		t.Errorf("the current topic was re-classified %d times for another topic's backfill", extra)
	}
	if res.ItemsPending != 0 {
		t.Errorf("pending after catch-up = %d, want 0", res.ItemsPending)
	}
}

// Same gap, other door: a topic paused for a while and switched back on skipped
// everything that arrived while it was off.
func TestResumedTopicCatchesUpOnWhatItMissed(t *testing.T) {
	db := testDB(t)
	src := seedSource(t, db, "Blog")
	topic := seedTopic(t, db, "Agents", "Agent harnesses.")
	seedTopic(t, db, "Open weights", "Open-weight releases.")
	seedItem(t, db, src, "https://x.test/a", "Before the pause")

	model := newFakeModel(t, scoreAllHigh)
	cfg := testConfig()
	run := func() {
		t.Helper()
		if _, err := Classify(context.Background(), db, clientFor(t, db, model), cfg, testRunDate, quietLogger()); err != nil {
			t.Fatalf("Classify: %v", err)
		}
	}
	run()

	if _, err := db.Exec(`UPDATE topics SET active = 0 WHERE id = ?`, topic); err != nil {
		t.Fatal(err)
	}
	missed := seedItem(t, db, src, "https://x.test/b", "During the pause")
	if _, err := db.Exec(`UPDATE items SET fetched_at = datetime('now', '+1 minute') WHERE id = ?`, missed); err != nil {
		t.Fatal(err)
	}
	run()

	if _, err := db.Exec(`UPDATE topics SET active = 1 WHERE id = ?`, topic); err != nil {
		t.Fatal(err)
	}
	run()

	if n := countRows(t, db, "SELECT count(*) FROM matches WHERE topic_id = ? AND item_id = ?", topic, missed); n != 1 {
		t.Error("the resumed topic never judged the item that arrived while it was paused")
	}
}

// Catching up stops at the sky's window: an item older than anything the sky can
// show would cost a call and reach no star.
func TestBackfillStopsAtTheSkyWindow(t *testing.T) {
	db := testDB(t)
	src := seedSource(t, db, "Blog")
	old := seedItem(t, db, src, "https://x.test/old", "Last month")
	recent := seedItem(t, db, src, "https://x.test/new", "This week")
	if _, err := db.Exec(`UPDATE items SET fetched_at = datetime('now', '-30 days') WHERE id = ?`, old); err != nil {
		t.Fatal(err)
	}
	topic := seedTopic(t, db, "Agents", "Agent harnesses.")

	model := newFakeModel(t, scoreAllHigh)
	if _, err := Classify(context.Background(), db, clientFor(t, db, model), testConfig(), testRunDate, quietLogger()); err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if n := countRows(t, db, "SELECT count(*) FROM matches WHERE topic_id = ? AND item_id = ?", topic, recent); n != 1 {
		t.Error("the recent item was not judged")
	}
	if n := countRows(t, db, "SELECT count(*) FROM matches WHERE topic_id = ? AND item_id = ?", topic, old); n != 0 {
		t.Error("an item older than the sky's window was judged")
	}
}

// A source added later needs no backfill: its items are timestamped when they are
// ingested, so they land after every topic's cursor. Pinned so a future change to
// what the cursor keys on cannot quietly break it.
func TestItemsFromANewSourceAreClassifiedForEveryTopic(t *testing.T) {
	db := testDB(t)
	first := seedSource(t, db, "Blog")
	a := seedTopic(t, db, "Agents", "Agent harnesses.")
	b := seedTopic(t, db, "Open weights", "Open-weight releases.")
	seedItem(t, db, first, "https://x.test/a", "From the first source")

	model := newFakeModel(t, scoreAllHigh)
	cfg := testConfig()
	if _, err := Classify(context.Background(), db, clientFor(t, db, model), cfg, testRunDate, quietLogger()); err != nil {
		t.Fatalf("Classify: %v", err)
	}

	added := seedSource(t, db, "New feed")
	item := seedItem(t, db, added, "https://y.test/b", "From the new source")
	if _, err := Classify(context.Background(), db, clientFor(t, db, model), cfg, testRunDate, quietLogger()); err != nil {
		t.Fatalf("Classify: %v", err)
	}
	for _, topic := range []int64{a, b} {
		if n := countRows(t, db, "SELECT count(*) FROM matches WHERE topic_id = ? AND item_id = ?", topic, item); n != 1 {
			t.Errorf("topic %d never judged the new source's item", topic)
		}
	}
}
