// Package config resolves runtime configuration.
//
// Two layers, deliberately separated:
//
//   - Env is process-level and read once at startup (port, data dir, timezone).
//     Changing it requires a restart, which is fine because none of it affects
//     model routing.
//   - Runtime comes from the settings table and is resolved fresh on every run.
//     §3.1 requires provider and model changes to take effect without a restart,
//     so nothing here may be cached across runs.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/tammai/interstellar/internal/store"
)

// Env is the process-level configuration, read once at startup.
type Env struct {
	Port     string
	DBPath   string
	Location *time.Location
}

// FromEnv reads the process environment. Provider API keys are deliberately not
// read here: the registry stores env var *names* and resolves values at call
// time, so a key added to the environment needs no code path through config.
func FromEnv() (Env, error) {
	port := getenv("PORT", "8422")

	dataDir := getenv("DATA_DIR", "/data")

	// TZ drives the schedule (§7). time.LoadLocation("") yields UTC, which is a
	// sane default; an unparseable TZ is an error rather than a silent fallback,
	// because a run firing at the wrong hour is hard to notice.
	tz := os.Getenv("TZ")
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return Env{}, fmt.Errorf("TZ %q: %w", tz, err)
	}

	return Env{
		Port:     port,
		DBPath:   filepath.Join(dataDir, "interstellar.db"),
		Location: loc,
	}, nil
}

// Runtime is the settings-backed configuration, resolved per run.
//
// Provider registry and per-role model refs are not here: they live in
// internal/llm, which owns env-var resolution and the fallback chain.
type Runtime struct {
	ScheduleIngest     string // "HH:MM" local
	ScheduleSynthesize string // "HH:MM" local

	MaxCallsPerRun int
	MaxCallsPerDay int

	HNMinPoints       int
	SkyDays           int
	ClassifyBatchSize int
	MatchThreshold    float64
	HotThreshold      int

	ClassifyWatermark string
}

// Load resolves Runtime from the settings table. Malformed values fall back to
// the documented default rather than failing the run: a typo'd number in
// Settings should not take the whole instance down.
func Load(db *store.DB) (Runtime, error) {
	s, err := db.Settings()
	if err != nil {
		return Runtime{}, err
	}
	return Runtime{
		ScheduleIngest:     str(s, store.KeyScheduleIngest, "09:30"),
		ScheduleSynthesize: str(s, store.KeyScheduleSynthesis, "10:30"),

		MaxCallsPerRun: integer(s, store.KeyMaxCallsPerRun, 200),
		MaxCallsPerDay: integer(s, store.KeyMaxCallsPerDay, 500),

		HNMinPoints:       integer(s, store.KeyHNMinPoints, 25),
		SkyDays:           integer(s, store.KeySkyDays, 7),
		ClassifyBatchSize: integer(s, store.KeyClassifyBatchSize, 30),
		MatchThreshold:    float(s, store.KeyMatchThreshold, 4),
		HotThreshold:      integer(s, store.KeyHotThreshold, 3),

		ClassifyWatermark: str(s, store.KeyClassifyWatermark, ""),
	}, nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func str(s map[string]string, key, def string) string {
	if v, ok := s[key]; ok && v != "" {
		return v
	}
	return def
}

func integer(s map[string]string, key string, def int) int {
	v, ok := s[key]
	if !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func float(s map[string]string, key string, def float64) float64 {
	v, ok := s[key]
	if !ok {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}
