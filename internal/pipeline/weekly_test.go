package pipeline

import (
	"context"
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
		if _, err := db.Exec(
			`INSERT INTO signals (run_date, topic_id, title, reason, score, source_count, primary_url)
			 VALUES (date(?, ?), ?, ?, 'Several outlets covered it.', 5, ?, 'https://x.test/a')`,
			weekStart, "+"+string(rune('0'+i%7))+" days", topic,
			"Story number "+string(rune('A'+i)), 4-i%3,
		); err != nil {
			t.Fatalf("seed signal: %v", err)
		}
	}
	return topic
}

func TestWriteWeeklyComposesFromTheWeeksSignals(t *testing.T) {
	db := testDB(t)
	seedWeek(t, db, "2026-08-10", 5)

	var prompt string
	model := newFakeModel(t, func(p string) string {
		prompt = p
		return "The week's throughline was harness convergence."
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

	stored, err := db.WeeklyDigestFor("2026-08-10")
	if err != nil {
		t.Fatalf("WeeklyDigestFor: %v", err)
	}
	if stored.Body != "The week's throughline was harness convergence." {
		t.Errorf("stored body = %q", stored.Body)
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
	first := newFakeModel(t, func(string) string { return "First version." })
	if _, err := WriteWeekly(context.Background(), db, clientFor(t, db, first), when, quietLogger()); err != nil {
		t.Fatal(err)
	}
	second := newFakeModel(t, func(string) string { return "Second version." })
	if _, err := WriteWeekly(context.Background(), db, clientFor(t, db, second), when, quietLogger()); err != nil {
		t.Fatal(err)
	}

	if n := countRows(t, db, `SELECT count(*) FROM weekly_digests`); n != 1 {
		t.Errorf("%d digest rows for one week", n)
	}
	stored, _ := db.WeeklyDigestFor("2026-08-10")
	if stored.Body != "Second version." {
		t.Errorf("body = %q, want the rewrite", stored.Body)
	}
}

// A re-write of an already-sent week must not make it look unsent, or the scheduler
// would mail it again.
func TestRewritingASentWeekKeepsItSent(t *testing.T) {
	db := testDB(t)
	seedWeek(t, db, "2026-08-10", 2)
	when := time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC)

	model := newFakeModel(t, func(string) string { return "Version one." })
	if _, err := WriteWeekly(context.Background(), db, clientFor(t, db, model), when, quietLogger()); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkWeeklySent("2026-08-10", "email", ""); err != nil {
		t.Fatal(err)
	}

	again := newFakeModel(t, func(string) string { return "Version two." })
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
