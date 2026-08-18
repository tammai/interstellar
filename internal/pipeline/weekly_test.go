package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tammai/interstellar/internal/store"
)

func TestWeekBoundsStartOnMonday(t *testing.T) {
	// 2026-08-13 is a Thursday.
	thu := time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)
	start, end := store.WeekBounds(thu)
	if start != "2026-08-10" || end != "2026-08-16" {
		t.Errorf("WeekBounds(Thu) = %s..%s, want 2026-08-10..2026-08-16", start, end)
	}

	// Sunday is the END of its week, not the start. Go puts Sunday at 0, which is
	// the off-by-one this exists to pin.
	sun := time.Date(2026, 8, 16, 9, 0, 0, 0, time.UTC)
	start, end = store.WeekBounds(sun)
	if start != "2026-08-10" || end != "2026-08-16" {
		t.Errorf("WeekBounds(Sun) = %s..%s, want the same week as Thursday", start, end)
	}

	mon := time.Date(2026, 8, 10, 0, 1, 0, 0, time.UTC)
	start, _ = store.WeekBounds(mon)
	if start != "2026-08-10" {
		t.Errorf("WeekBounds(Mon) start = %s, want the same day", start)
	}

	// And the previous week is a clean seven days back.
	pStart, pEnd := store.PreviousWeekBounds(thu)
	if pStart != "2026-08-03" || pEnd != "2026-08-09" {
		t.Errorf("PreviousWeekBounds = %s..%s, want 2026-08-03..2026-08-09", pStart, pEnd)
	}
}

func seedWeek(t *testing.T, db *store.DB, weekStart string, n int) int64 {
	t.Helper()
	topic := seedTopic(t, db, "Agents", "Agent harnesses.")
	for i := range n {
		// A URL each. Signals are collapsed by primary_url now -- one URL is one story
		// -- so a fixture that gave five signals the same link would be describing one
		// story five times, and the round-up would rightly print it once.
		if _, err := db.Exec(
			`INSERT INTO signals (run_date, topic_id, title, reason, score, source_count, primary_url)
			 VALUES (date(?, ?), ?, ?, 'Several outlets covered it.', 5, ?, ?)`,
			weekStart, "+"+string(rune('0'+i%7))+" days", topic,
			"Story number "+string(rune('A'+i)), 4-i%3,
			fmt.Sprintf("https://x.test/%c", 'a'+i),
		); err != nil {
			t.Fatalf("seed signal: %v", err)
		}
	}
	return topic
}

// weeklyPlanFor builds the answer the composer now expects: a lead plus every id the
// prompt offered. Reading the ids back out of the prompt is what a real model does,
// and it means a test does not have to know what row numbers the fixture got.
func weeklyPlanFor(lead, prompt string) string {
	type pick struct {
		ID   int64  `json:"id"`
		Note string `json:"note"`
	}
	plan := struct {
		Lead  string `json:"lead"`
		Items []pick `json:"items"`
	}{Lead: lead}

	for _, id := range weeklyIDsIn(prompt) {
		plan.Items = append(plan.Items, pick{ID: id, Note: fmt.Sprintf("What changed in %d.", id)})
	}
	out, err := json.Marshal(plan)
	if err != nil {
		panic(err)
	}
	return string(out)
}

func weeklyIDsIn(prompt string) []int64 {
	var out []int64
	for _, line := range strings.Split(prompt, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "- id: ") {
			continue
		}
		if id, err := strconv.ParseInt(strings.TrimPrefix(line, "- id: "), 10, 64); err == nil {
			out = append(out, id)
		}
	}
	return out
}

func TestWriteWeeklyComposesFromTheWeeksSignals(t *testing.T) {
	db := testDB(t)
	seedWeek(t, db, "2026-08-10", 5)

	var prompt string
	model := newFakeModel(t, func(p string) string {
		prompt = p
		return weeklyPlanFor("Harness convergence was the week's one real story.", p)
	})

	when := time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC)
	result, err := WriteWeekly(context.Background(), db, clientFor(t, db, model), when, quietLogger())
	if err != nil {
		t.Fatalf("WriteWeekly: %v", err)
	}

	if !result.Written {
		t.Fatalf("nothing written: %+v", result)
	}
	if result.WeekStart != "2026-08-10" || result.Signals != 5 {
		t.Errorf("week=%s signals=%d, want 2026-08-10 and 5", result.WeekStart, result.Signals)
	}
	// The prompt must carry source counts: they are the model's evidence of
	// significance, and the system prompt tells it to rank on them.
	if !strings.Contains(prompt, "sources:") {
		t.Error("the prompt does not carry source counts")
	}
	if !strings.Contains(prompt, "WEEK: 2026-08-10 to 2026-08-16") {
		t.Error("the prompt does not state the week")
	}
	// The ids are what let the model pick a story without having to reproduce its
	// title, its link or its source count.
	if !strings.Contains(prompt, "- id: ") {
		t.Error("the prompt does not carry signal ids")
	}

	stored, err := db.WeeklyDigestFor("2026-08-10")
	if err != nil {
		t.Fatalf("WeeklyDigestFor: %v", err)
	}
	// The digest is a list of named stories, and everything except the lead and the
	// notes is read from the database rather than taken from the model.
	for _, want := range []string{
		"Harness convergence was the week's one real story.",
		"1. ",              // a numbered list, most consequential first
		"Story number A",   // the title, from signals.title
		"4 sources",        // the corroboration count, from signals.source_count
		"Agents",           // the topic
		"https://x.test/a", // the link, from signals.primary_url
	} {
		if !strings.Contains(stored.Body, want) {
			t.Errorf("the round-up is missing %q:\n%s", want, stored.Body)
		}
	}
	if stored.Signals != 5 || stored.Topics != 1 {
		t.Errorf("stored counts: signals=%d topics=%d", stored.Signals, stored.Topics)
	}
	if stored.TokensIn == 0 {
		t.Error("token accounting was not stored")
	}

	// Logged under its own role, on the synthesizer's chain.
	if n := countRows(t, db, `SELECT count(*) FROM llm_calls WHERE role = 'weekly'`); n != 1 {
		t.Errorf("%d calls logged as weekly, want 1", n)
	}
	if n := countRows(t, db, `SELECT count(*) FROM llm_calls WHERE role = 'synthesizer'`); n != 0 {
		t.Errorf("%d calls billed to the synthesizer role", n)
	}
}

// An empty week gets no round-up and costs no call. "There was no news" is a
// sentence the absence of a digest already says, and writing it would occupy the
// week slot so a later re-run looked done.
func TestWriteWeeklySkipsAnEmptyWeek(t *testing.T) {
	db := testDB(t)
	seedTopic(t, db, "Agents", "Agent harnesses.")

	model := newFakeModel(t, func(string) string { return "SHOULD NOT BE CALLED" })
	when := time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC)

	result, err := WriteWeekly(context.Background(), db, clientFor(t, db, model), when, quietLogger())
	if err != nil {
		t.Fatalf("WriteWeekly: %v", err)
	}
	if result.Written {
		t.Error("a round-up was written for a week with no signals")
	}
	if result.Skipped == "" {
		t.Error("no reason given for skipping")
	}
	if model.callCount() != 0 {
		t.Errorf("model called %d times for an empty week", model.callCount())
	}
	if _, err := db.WeeklyDigestFor("2026-08-10"); err == nil {
		t.Error("an empty week left a digest row, which would make a re-run look done")
	}
}

// Writing the same week twice must replace, not duplicate.
func TestWriteWeeklyIsIdempotentPerWeek(t *testing.T) {
	db := testDB(t)
	seedWeek(t, db, "2026-08-10", 3)

	when := time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC)
	first := newFakeModel(t, func(p string) string { return weeklyPlanFor("First version.", p) })
	if _, err := WriteWeekly(context.Background(), db, clientFor(t, db, first), when, quietLogger()); err != nil {
		t.Fatal(err)
	}
	second := newFakeModel(t, func(p string) string { return weeklyPlanFor("Second version.", p) })
	if _, err := WriteWeekly(context.Background(), db, clientFor(t, db, second), when, quietLogger()); err != nil {
		t.Fatal(err)
	}

	if n := countRows(t, db, `SELECT count(*) FROM weekly_digests`); n != 1 {
		t.Errorf("%d digest rows for one week", n)
	}
	stored, _ := db.WeeklyDigestFor("2026-08-10")
	if !strings.Contains(stored.Body, "Second version.") {
		t.Errorf("body = %q, want the rewrite", stored.Body)
	}
}

// A re-write of an already-sent week must not make it look unsent, or the scheduler
// would mail it again.
func TestRewritingASentWeekKeepsItSent(t *testing.T) {
	db := testDB(t)
	seedWeek(t, db, "2026-08-10", 2)
	when := time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC)

	model := newFakeModel(t, func(p string) string { return weeklyPlanFor("Version one.", p) })
	if _, err := WriteWeekly(context.Background(), db, clientFor(t, db, model), when, quietLogger()); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkWeeklySent("2026-08-10", "email", ""); err != nil {
		t.Fatal(err)
	}

	again := newFakeModel(t, func(p string) string { return weeklyPlanFor("Version two.", p) })
	if _, err := WriteWeekly(context.Background(), db, clientFor(t, db, again), when, quietLogger()); err != nil {
		t.Fatal(err)
	}

	stored, _ := db.WeeklyDigestFor("2026-08-10")
	if stored.SentAt == "" {
		t.Error("rewriting cleared the sent timestamp; the week would be mailed twice")
	}
	if stored.SentVia != "email" {
		t.Errorf("sent_via = %q", stored.SentVia)
	}
}

// A model that answers with prose costs the round-up its lead and its ordering. It
// must not cost the owner the week: the stories are already in the database, already
// ranked by corroboration, and each already carries the reason the synthesizer gave
// it.
func TestWriteWeeklyFallsBackWhenTheModelWillNotReturnJSON(t *testing.T) {
	db := testDB(t)
	seedWeek(t, db, "2026-08-10", 4)

	model := newFakeModel(t, func(string) string {
		return "Behind that headline a clear shift into production-grade agent engineering accelerated."
	})
	when := time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC)

	result, err := WriteWeekly(context.Background(), db, clientFor(t, db, model), when, quietLogger())
	if err != nil {
		t.Fatalf("WriteWeekly: %v", err)
	}
	if !result.Written {
		t.Fatal("nothing written when the model returned prose")
	}

	stored, _ := db.WeeklyDigestFor("2026-08-10")
	if !strings.Contains(stored.Body, "1. Story number") {
		t.Errorf("the fallback did not list the week's stories:\n%s", stored.Body)
	}
	if !strings.Contains(stored.Body, "https://x.test/a") {
		t.Errorf("the fallback dropped the links:\n%s", stored.Body)
	}
	// The prose itself must not be published: it is not a lead, it is a failure.
	if strings.Contains(stored.Body, "production-grade agent engineering") {
		t.Errorf("unparsed model prose was published as the round-up:\n%s", stored.Body)
	}
}

// An id the week does not contain is the dangerous case: printed anyway it would
// carry a real-looking title and link from nowhere. Same whitelist reasoning as the
// chat planner's topic ids.
func TestWriteWeeklyDropsInventedIDs(t *testing.T) {
	db := testDB(t)
	seedWeek(t, db, "2026-08-10", 3)

	model := newFakeModel(t, func(p string) string {
		ids := weeklyIDsIn(p)
		if len(ids) == 0 {
			t.Fatal("the prompt carried no ids")
		}
		// One real story, one that does not exist, and the real one again.
		return fmt.Sprintf(
			`{"lead":"Lead.","items":[{"id":%d,"note":"Real."},{"id":999999,"note":"Invented."},{"id":%d,"note":"Repeat."}]}`,
			ids[0], ids[0])
	})
	when := time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC)

	if _, err := WriteWeekly(context.Background(), db, clientFor(t, db, model), when, quietLogger()); err != nil {
		t.Fatalf("WriteWeekly: %v", err)
	}
	stored, _ := db.WeeklyDigestFor("2026-08-10")

	if strings.Contains(stored.Body, "Invented.") {
		t.Errorf("a note for an id that is not in the week was published:\n%s", stored.Body)
	}
	if strings.Contains(stored.Body, "Repeat.") {
		t.Errorf("the same story was listed twice:\n%s", stored.Body)
	}
	if !strings.Contains(stored.Body, "Real.") {
		t.Errorf("the one usable pick was dropped with the bad ones:\n%s", stored.Body)
	}
	if !strings.Contains(stored.Body, "1. ") || strings.Contains(stored.Body, "2. ") {
		t.Errorf("want exactly one listed item:\n%s", stored.Body)
	}
}

// One story, listed once, however many signal rows carry it.
//
// Both causes in one fixture. Every run composes its own day's signals while the sky
// shows seven days, so a story that stays newsworthy gets a fresh row each morning
// with a reworded title -- and a story matching two topics IS two signals (§4), the
// case the canvas draws as one shared star. The round-up printed one item per row: two
// near-identical "Meta releases / Meta launches Muse Glimmer" entries at the top of a
// real digest, and the same coding agent at 5 and 12.
func TestWriteWeeklyListsARepeatedStoryOnce(t *testing.T) {
	db := testDB(t)
	agents := seedTopic(t, db, "Agents", "Agent harnesses.")
	ops := seedTopic(t, db, "LLM Ops", "Running them.")

	const url = "https://example.test/muse-glimmer"
	rows := []struct {
		day     string
		topic   int64
		title   string
		sources int
	}{
		// The same story, recomposed on two mornings under one topic...
		{"2026-08-11", agents, "Meta launches Muse Glimmer", 7},
		{"2026-08-12", agents, "Meta releases Muse Glimmer", 6},
		// ...and matched by a second topic as well.
		{"2026-08-12", ops, "Muse Glimmer lands for local agents", 5},
	}
	for _, r := range rows {
		if _, err := db.Exec(
			`INSERT INTO signals (run_date, topic_id, title, reason, score, source_count, primary_url)
			 VALUES (?, ?, ?, 'Widely covered.', 5, ?, ?)`,
			r.day, r.topic, r.title, r.sources, url,
		); err != nil {
			t.Fatalf("seed signal: %v", err)
		}
	}

	var prompt string
	model := newFakeModel(t, func(p string) string {
		prompt = p
		return weeklyPlanFor("Muse Glimmer was the week.", p)
	})
	when := time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC)

	result, err := WriteWeekly(context.Background(), db, clientFor(t, db, model), when, quietLogger())
	if err != nil {
		t.Fatalf("WriteWeekly: %v", err)
	}
	// The row count is still the row count -- three signals exist and the digest says so.
	if result.Signals != 3 {
		t.Errorf("Signals = %d, want the 3 rows", result.Signals)
	}

	// The prompt offers the story ONCE. This is the actual fix: a model handed the same
	// story under three ids picked it three times, correctly.
	if n := strings.Count(prompt, "- id: "); n != 1 {
		t.Errorf("the prompt offered %d ids for one story:\n%s", n, prompt)
	}

	stored, _ := db.WeeklyDigestFor("2026-08-10")
	if !strings.Contains(stored.Body, "1. ") || strings.Contains(stored.Body, "2. ") {
		t.Errorf("want exactly one listed item:\n%s", stored.Body)
	}
	// The surviving reading is the best-corroborated one, and the count is the MAXIMUM
	// rather than the sum: three readings of one story are not 18 outlets.
	if !strings.Contains(stored.Body, "Meta launches Muse Glimmer") {
		t.Errorf("the best-corroborated title did not survive:\n%s", stored.Body)
	}
	if !strings.Contains(stored.Body, "7 sources") {
		t.Errorf("source count is not the maximum of the collapsed rows:\n%s", stored.Body)
	}
	for _, bad := range []string{"18 sources", "13 sources", "12 sources"} {
		if strings.Contains(stored.Body, bad) {
			t.Errorf("source counts were summed (%s):\n%s", bad, stored.Body)
		}
	}
	// Both topics are named. A story crossing two topics is the most interesting thing
	// a week contains, and printing one of them hides exactly that.
	for _, topic := range []string{"Agents", "LLM Ops"} {
		if !strings.Contains(stored.Body, topic) {
			t.Errorf("topic %q is missing from the collapsed item:\n%s", topic, stored.Body)
		}
	}
}

func TestMarkWeeklySentSeparatesFailureFromNeverTried(t *testing.T) {
	db := testDB(t)
	if _, err := db.SaveWeeklyDigest(store.WeeklyDigest{
		WeekStart: "2026-08-10", WeekEnd: "2026-08-16", Body: "Body.",
	}); err != nil {
		t.Fatal(err)
	}

	// Never tried.
	d, _ := db.WeeklyDigestFor("2026-08-10")
	if d.SentAt != "" || d.SendError != "" {
		t.Errorf("a fresh digest looks delivered: %+v", d)
	}

	// Tried and failed: no timestamp, but a reason.
	if err := db.MarkWeeklySent("2026-08-10", "", "relay refused the sender"); err != nil {
		t.Fatal(err)
	}
	d, _ = db.WeeklyDigestFor("2026-08-10")
	if d.SentAt != "" {
		t.Error("a failed send recorded a sent timestamp")
	}
	if d.SendError == "" {
		t.Error("a failed send recorded no reason")
	}
}

func TestWeeklySpecBuildsACronExpression(t *testing.T) {
	spec, err := weeklySpec("mon", "11:00")
	if err != nil || spec != "0 11 * * 1" {
		t.Errorf("weeklySpec(mon, 11:00) = %q, %v; want \"0 11 * * 1\"", spec, err)
	}
	if spec, _ := weeklySpec("Sunday", "07:30"); spec != "30 7 * * 0" {
		t.Errorf("Sunday spec = %q, want \"30 7 * * 0\"", spec)
	}

	// Empty switches it off, and that is not an error.
	for _, off := range []string{"", "off", "never", "  "} {
		spec, err := weeklySpec(off, "11:00")
		if err != nil || spec != "" {
			t.Errorf("weeklySpec(%q) = %q, %v; want off with no error", off, spec, err)
		}
	}

	for _, bad := range []string{"someday", "13", "mon?"} {
		if _, err := weeklySpec(bad, "11:00"); err == nil {
			t.Errorf("weeklySpec(%q) accepted a bad day", bad)
		}
	}
	if _, err := weeklySpec("mon", "25:00"); err == nil {
		t.Error("weeklySpec accepted a bad time")
	}
}
