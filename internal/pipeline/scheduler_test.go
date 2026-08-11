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
	if len(next) != 2 {
		t.Fatalf("got %d scheduled entries, want 2 (ingest and synthesize)", len(next))
	}

	// The seeded times are 09:30 and 10:30 local.
	wantHours := map[int]int{9: 30, 10: 30}
	for _, n := range next {
		local := n.In(bangkok)
		wantMin, ok := wantHours[local.Hour()]
		if !ok {
			t.Errorf("scheduled run at %s local; want 09:30 or 10:30", local.Format("15:04"))
			continue
		}
		if local.Minute() != wantMin {
			t.Errorf("scheduled at %s local, want minute %d", local.Format("15:04"), wantMin)
		}
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
