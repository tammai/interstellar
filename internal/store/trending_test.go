package store

import (
	"testing"
	"time"
)

// seedHistory writes signals across a span of days so the halves are meaningful.
func seedHistory(t *testing.T, db *DB) (rising, falling, silent int64) {
	t.Helper()

	rising = mustTopic(t, db, "Rising")
	falling = mustTopic(t, db, "Falling")
	silent = mustTopic(t, db, "Silent")

	day := func(back int) string {
		return RunDate(time.Now().AddDate(0, 0, -back))
	}
	add := func(topic int64, back int, title string, srcCount int) {
		if _, err := db.Exec(
			`INSERT INTO signals (run_date, topic_id, title, reason, score, source_count, primary_url)
			 VALUES (?, ?, ?, 'Because.', 5, ?, 'https://x.test/a')`,
			day(back), topic, title, srcCount,
		); err != nil {
			t.Fatalf("seed signal: %v", err)
		}
	}

	// Rising: nothing in the older half, three in the recent half.
	add(rising, 1, "Rising story one", 4)
	add(rising, 2, "Rising story two", 3)
	add(rising, 3, "Rising story three", 1)

	// Falling: three in the OLDER half, nothing recent. For a 30-day window the
	// halves split at 15 days, so these have to sit beyond that — 12-14 days back
	// is still the recent half, which is what the first version of this fixture got
	// wrong.
	add(falling, 20, "Falling story one", 2)
	add(falling, 22, "Falling story two", 1)
	add(falling, 25, "Falling story three", 1)

	// A story that keeps coming back, reworded each day as the synthesizer would.
	add(rising, 1, "Agent harness patterns converge", 4)
	add(rising, 2, "Agent harness patterns: converge", 3)
	add(rising, 3, "Agent harness patterns converge", 5)

	return rising, falling, silent
}

func mustTopic(t *testing.T, db *DB, title string) int64 {
	t.Helper()
	id, err := db.CreateTopic(Topic{Title: title, Definition: "About " + title, Active: true})
	if err != nil {
		t.Fatalf("create topic: %v", err)
	}
	return id
}

func TestTopicTrendsReadsDirectionFromTheHalves(t *testing.T) {
	db := openTemp(t)
	rising, falling, silent := seedHistory(t, db)

	trends, err := db.TopicTrends(30)
	if err != nil {
		t.Fatalf("TopicTrends: %v", err)
	}

	byID := map[int64]TopicTrend{}
	for _, tr := range trends {
		byID[tr.TopicID] = tr
	}
	if len(byID) != 3 {
		t.Fatalf("got %d topics, want 3 — a silent topic must not be dropped", len(byID))
	}

	if got := byID[rising].Direction(); got != "new" && got != "rising" {
		t.Errorf("rising topic reports %q (recent=%d prior=%d)",
			got, byID[rising].Recent, byID[rising].Prior)
	}
	if got := byID[falling].Direction(); got != "stopped" {
		t.Errorf("falling topic reports %q (recent=%d prior=%d)",
			got, byID[falling].Recent, byID[falling].Prior)
	}
	// The most useful row in the view: a topic producing nothing at all.
	if got := byID[silent].Direction(); got != "quiet" {
		t.Errorf("silent topic reports %q, want quiet", got)
	}
	if byID[silent].Signals != 0 {
		t.Errorf("silent topic has %d signals", byID[silent].Signals)
	}

	// Hot counts only signals that reached the gold threshold.
	if byID[rising].Hot == 0 {
		t.Error("rising topic reports no hot signals despite 3+ source counts")
	}
	if byID[rising].MaxSrc != 5 {
		t.Errorf("max sources = %d, want 5", byID[rising].MaxSrc)
	}
}

func TestDailyVolumesAreOldestFirstAndOnlyRealDays(t *testing.T) {
	db := openTemp(t)
	seedHistory(t, db)

	daily, err := db.DailyVolumes(30)
	if err != nil {
		t.Fatalf("DailyVolumes: %v", err)
	}
	if len(daily) == 0 {
		t.Fatal("no daily volumes")
	}
	for i := 1; i < len(daily); i++ {
		if daily[i-1].RunDate > daily[i].RunDate {
			t.Error("daily volumes are not oldest-first")
			break
		}
	}
	// A day with no signals must be absent, not zero: a zero row cannot be told
	// apart from a day the scheduler never ran.
	for _, d := range daily {
		if d.Signals == 0 {
			t.Errorf("%s has a zero row; gaps must be absent", d.RunDate)
		}
	}
}

// The synthesizer rewords a title every day, so grouping on the raw title would
// find nothing recurring at all.
func TestRecurringStoriesGroupAcrossRewordedTitles(t *testing.T) {
	db := openTemp(t)
	seedHistory(t, db)

	stories, err := db.RecurringStories(30, 3, 10)
	if err != nil {
		t.Fatalf("RecurringStories: %v", err)
	}
	if len(stories) == 0 {
		t.Fatal("no recurring stories found; the three-day run was missed")
	}

	var found *RecurringStory
	for i := range stories {
		if stories[i].Days >= 3 {
			found = &stories[i]
		}
	}
	if found == nil {
		t.Fatalf("no story spans 3 days: %+v", stories)
	}
	if found.FirstSeen >= found.LastSeen {
		t.Errorf("first seen %s is not before last seen %s", found.FirstSeen, found.LastSeen)
	}
	if found.TopicTitle == "" {
		t.Error("recurring story has no topic title")
	}
	if found.MaxSources != 5 {
		t.Errorf("max sources = %d, want 5", found.MaxSources)
	}
}

// Appearing once is not recurring.
func TestRecurringStoriesNeedMoreThanOneDay(t *testing.T) {
	db := openTemp(t)
	topic := mustTopic(t, db, "Agents")

	if _, err := db.Exec(
		`INSERT INTO signals (run_date, topic_id, title, reason, score, source_count, primary_url)
		 VALUES (date('now'), ?, 'A one-off story', 'Because.', 5, 3, 'https://x.test/a')`,
		topic); err != nil {
		t.Fatal(err)
	}

	stories, err := db.RecurringStories(30, 2, 10)
	if err != nil {
		t.Fatalf("RecurringStories: %v", err)
	}
	if len(stories) != 0 {
		t.Errorf("a story seen once was reported as recurring: %+v", stories)
	}
}

// Out-of-window history must not leak in.
func TestTrendsHonorTheWindow(t *testing.T) {
	db := openTemp(t)
	topic := mustTopic(t, db, "Agents")

	old := RunDate(time.Now().AddDate(0, 0, -60))
	if _, err := db.Exec(
		`INSERT INTO signals (run_date, topic_id, title, reason, score, source_count, primary_url)
		 VALUES (?, ?, 'Ancient history', 'Because.', 5, 4, 'https://x.test/a')`,
		old, topic); err != nil {
		t.Fatal(err)
	}

	trends, err := db.TopicTrends(30)
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range trends {
		if tr.Signals != 0 {
			t.Errorf("a 60-day-old signal leaked into a 30-day window: %+v", tr)
		}
	}

	wide, err := db.TopicTrends(90)
	if err != nil {
		t.Fatal(err)
	}
	var total int
	for _, tr := range wide {
		total += tr.Signals
	}
	if total != 1 {
		t.Errorf("90-day window found %d signals, want 1", total)
	}
}

// A one-day window cannot be split into halves; it must clamp rather than divide
// by zero or produce a nonsense bound.
func TestTopicTrendsClampsTinyWindows(t *testing.T) {
	db := openTemp(t)
	seedHistory(t, db)

	for _, days := range []int{-5, 0, 1, 2} {
		if _, err := db.TopicTrends(days); err != nil {
			t.Errorf("TopicTrends(%d): %v", days, err)
		}
	}
	if _, err := db.DailyVolumes(0); err != nil {
		t.Errorf("DailyVolumes(0): %v", err)
	}
	if _, err := db.RecurringStories(0, 0, 0); err != nil {
		t.Errorf("RecurringStories(0,0,0): %v", err)
	}
}

func TestDirectionCoversEveryShape(t *testing.T) {
	cases := []struct {
		recent, prior int
		want          string
	}{
		{0, 0, "quiet"}, {3, 0, "new"}, {0, 3, "stopped"},
		{5, 2, "rising"}, {2, 5, "falling"}, {3, 3, "steady"},
	}
	for _, c := range cases {
		got := TopicTrend{Recent: c.recent, Prior: c.prior}.Direction()
		if got != c.want {
			t.Errorf("recent=%d prior=%d gave %q, want %q", c.recent, c.prior, got, c.want)
		}
	}
}
