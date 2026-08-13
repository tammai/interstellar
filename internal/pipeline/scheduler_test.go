package pipeline

import (
	"testing"
	"time"

	"github.com/tammai/interstellar/internal/config"
	"github.com/tammai/interstellar/internal/store"
)

func TestCronSpec(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"09:30", "30 9 * * *", false},
		{"00:00", "0 0 * * *", false},
		{"23:59", "59 23 * * *", false},
		{" 7:05 ", "5 7 * * *", false},

		{"9am", "", true},
		{"25:00", "", true},
		{"12:60", "", true},
		{"", "", true},
		{"1230", "", true},
	}

	for _, tc := range tests {
		got, err := cronSpec(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("cronSpec(%q) = %q, want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("cronSpec(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("cronSpec(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// §7: the schedule honors TZ. A run firing an hour off is hard to notice and
// would quietly break the "sky fresh by 11:00" promise.
func TestSchedulerHonorsTimezone(t *testing.T) {
	db := testDB(t)
	if err := db.Bootstrap(); err != nil {
		t.Fatal(err)
	}

	bangkok, err := time.LoadLocation("Asia/Bangkok")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	env := config.Env{Port: "8422", Location: bangkok}

	runner := NewRunner(db, env, quietLogger())
	s := NewScheduler(db, env, runner, quietLogger())
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	next := s.NextRuns()
	// Three: the daily ingest and synthesize, plus the weekly round-up. The round-up
	// is written by default -- one synthesizer call a week -- while DELIVERING it is
	// off, which is the part decision #10 actually turned down.
	if len(next) != 3 {
		t.Fatalf("got %d scheduled entries, want 3 (ingest, synthesize, weekly)", len(next))
	}

	// The seeded times are 09:30, 10:30 and Monday 11:00, all local.
	wantHours := map[int]int{9: 30, 10: 30, 11: 0}
	var sawMonday bool
	for _, n := range next {
		local := n.In(bangkok)
		wantMin, ok := wantHours[local.Hour()]
		if !ok {
			t.Errorf("scheduled run at %s local; want 09:30, 10:30 or 11:00", local.Format("15:04"))
			continue
		}
		if local.Minute() != wantMin {
			t.Errorf("scheduled at %s local, want minute %d", local.Format("15:04"), wantMin)
		}
		// The 11:00 entry is the weekly one, and it must land on a Monday -- a
		// weekly cron with the day field wrong fires daily, which would be seven
		// round-ups a week and seven synthesizer calls.
		if local.Hour() == 11 {
			if local.Weekday() != time.Monday {
				t.Errorf("the weekly round-up is scheduled for %s, want Monday", local.Weekday())
			}
			sawMonday = true
		}
	}
	if !sawMonday {
		t.Error("no weekly entry found among the scheduled runs")
	}
}

// An empty weekly day switches the round-up off, leaving the daily schedule intact.
func TestSchedulerWeeklyCanBeSwitchedOff(t *testing.T) {
	db := testDB(t)
	if err := db.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	if err := db.SetSetting(store.KeyScheduleWeeklyDay, ""); err != nil {
		t.Fatal(err)
	}

	env := config.Env{Port: "8422", Location: time.UTC}
	s := NewScheduler(db, env, NewRunner(db, env, quietLogger()), quietLogger())
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	if n := len(s.NextRuns()); n != 2 {
		t.Errorf("got %d entries with the round-up off, want 2", n)
	}
}

// A bad weekly setting must not take the daily schedule down with it: the daily sky
// is the product, and a typo in a weekly day is not worth losing it over.
func TestSchedulerSurvivesABadWeeklySetting(t *testing.T) {
	db := testDB(t)
	if err := db.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	if err := db.SetSetting(store.KeyScheduleWeeklyDay, "someday"); err != nil {
		t.Fatal(err)
	}

	env := config.Env{Port: "8422", Location: time.UTC}
	s := NewScheduler(db, env, NewRunner(db, env, quietLogger()), quietLogger())
	if err := s.Start(); err != nil {
		t.Fatalf("Start returned an error for a bad weekly day: %v", err)
	}
	defer s.Stop()

	if n := len(s.NextRuns()); n != 2 {
		t.Errorf("got %d entries, want the 2 daily ones to survive", n)
	}
}

// The whole point of Reload: a schedule change must take effect without a
// restart (§3.1's requirement, applied to §7's schedule).
func TestSchedulerReloadAppliesNewTimesWithoutRestart(t *testing.T) {
	db := testDB(t)
	if err := db.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	env := config.Env{Port: "8422", Location: time.UTC}

	runner := NewRunner(db, env, quietLogger())
	s := NewScheduler(db, env, runner, quietLogger())
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	before := s.NextRuns()
	if len(before) == 0 {
		t.Fatal("no entries after Start")
	}

	if err := db.SetSetting(store.KeyScheduleIngest, "04:05"); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	var found bool
	for _, n := range s.NextRuns() {
		utc := n.In(time.UTC)
		if utc.Hour() == 4 && utc.Minute() == 5 {
			found = true
		}
	}
	if !found {
		t.Errorf("new ingest time not scheduled after Reload; entries = %v", s.NextRuns())
	}
}

// A bad schedule must not leave the instance with no schedule at all: the
// previous entries stay, and the owner gets an error to fix.
func TestSchedulerReloadKeepsPreviousScheduleOnError(t *testing.T) {
	db := testDB(t)
	if err := db.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	env := config.Env{Port: "8422", Location: time.UTC}

	runner := NewRunner(db, env, quietLogger())
	s := NewScheduler(db, env, runner, quietLogger())
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	before := s.NextRuns()

	if err := db.SetSetting(store.KeyScheduleIngest, "not a time"); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(); err == nil {
		t.Fatal("Reload accepted an invalid schedule")
	}

	after := s.NextRuns()
	if len(after) != len(before) {
		t.Errorf("entry count changed after a failed reload: %d -> %d", len(before), len(after))
	}
	for i := range before {
		if !before[i].Equal(after[i]) {
			t.Errorf("entry %d moved after a failed reload: %v -> %v", i, before[i], after[i])
		}
	}
}
