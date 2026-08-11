package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/tammai/interstellar/internal/config"
	"github.com/tammai/interstellar/internal/store"
)

// Scheduler fires the daily run.
//
// §2's cadence is ingest ~09:30, synthesize ~10:30, sky fresh by 11:00. Both
// entries call the same full pipeline: ingest is cheap and idempotent, and the
// watermark means the 10:30 pass classifies only what arrived since 09:30. That
// is simpler than two partial pipelines and degrades better -- if the 09:30 run
// is missed entirely, 10:30 still produces a sky.
//
// Reload() exists because §3.1 forbids requiring a restart. Changing the times in
// Settings rebuilds the cron entries in place.
type Scheduler struct {
	db     *store.DB
	env    config.Env
	runner *Runner
	log    *slog.Logger

	mu   sync.Mutex
	cron *cron.Cron
}

// NewScheduler builds a scheduler. Call Start to begin firing.
func NewScheduler(db *store.DB, env config.Env, runner *Runner, log *slog.Logger) *Scheduler {
	return &Scheduler{db: db, env: env, runner: runner, log: log}
}

// Start reads the schedule from settings and begins firing.
func (s *Scheduler) Start() error { return s.Reload() }

// Reload rebuilds the schedule from current settings. Safe to call while running:
// the old cron is stopped only after the new one is built, so a bad schedule
// leaves the previous one in place rather than silently disabling the product.
func (s *Scheduler) Reload() error {
	cfg, err := config.Load(s.db)
	if err != nil {
		return err
	}

	ingestSpec, err := cronSpec(cfg.ScheduleIngest)
	if err != nil {
		return fmt.Errorf("ingest schedule %q: %w", cfg.ScheduleIngest, err)
	}
	synthSpec, err := cronSpec(cfg.ScheduleSynthesize)
	if err != nil {
		return fmt.Errorf("synthesize schedule %q: %w", cfg.ScheduleSynthesize, err)
	}

	// Location, not UTC: §7 says the schedule honors TZ, and the owner thinks in
	// local time.
	next := cron.New(cron.WithLocation(s.env.Location))

	if _, err := next.AddFunc(ingestSpec, func() { s.fire("scheduled ingest") }); err != nil {
		return fmt.Errorf("add ingest schedule: %w", err)
	}
	if _, err := next.AddFunc(synthSpec, func() { s.fire("scheduled synthesis") }); err != nil {
		return fmt.Errorf("add synthesize schedule: %w", err)
	}

	s.mu.Lock()
	previous := s.cron
	s.cron = next
	s.mu.Unlock()

	next.Start()
	if previous != nil {
		previous.Stop()
	}

	s.log.Info("schedule set",
		"ingest", cfg.ScheduleIngest,
		"synthesize", cfg.ScheduleSynthesize,
		"tz", s.env.Location.String(),
	)
	return nil
}

// Stop halts the scheduler. In-flight runs are not interrupted here; the runner
// owns that.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	c := s.cron
	s.mu.Unlock()
	if c != nil {
		c.Stop()
	}
}

// NextRuns reports upcoming fire times, for the Settings screen.
func (s *Scheduler) NextRuns() []time.Time {
	s.mu.Lock()
	c := s.cron
	s.mu.Unlock()
	if c == nil {
		return nil
	}
	var out []time.Time
	for _, e := range c.Entries() {
		out = append(out, e.Next)
	}
	return out
}

// fire starts a run, tolerating an overlap. A scheduled fire that lands while a
// manual refresh is going is skipped rather than queued: the later run would
// classify the same watermark window and produce the same signals.
func (s *Scheduler) fire(reason string) {
	if err := s.runner.RunNow(context.Background(), reason); err != nil {
		s.log.Info("scheduled run skipped", "reason", reason, "err", err)
	}
}

// cronSpec converts "HH:MM" into a daily cron expression.
//
// Settings holds a wall-clock time because that is what an owner wants to type;
// cron syntax would be a needless puzzle for "when should my sky be ready".
func cronSpec(hhmm string) (string, error) {
	hhmm = strings.TrimSpace(hhmm)
	hourStr, minStr, found := strings.Cut(hhmm, ":")
	if !found {
		return "", fmt.Errorf("want HH:MM")
	}
	hour, err := strconv.Atoi(strings.TrimSpace(hourStr))
	if err != nil || hour < 0 || hour > 23 {
		return "", fmt.Errorf("hour must be 0-23")
	}
	minute, err := strconv.Atoi(strings.TrimSpace(minStr))
	if err != nil || minute < 0 || minute > 59 {
		return "", fmt.Errorf("minute must be 0-59")
	}
	return fmt.Sprintf("%d %d * * *", minute, hour), nil
}
