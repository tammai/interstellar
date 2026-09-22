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

	ScheduleWeeklyDay  string
	ScheduleWeeklyTime string

	DigestEnabled    bool
	SMTPHost         string
	SMTPPort         int
	SMTPUsername     string
	SMTPPasswordEnv  string
	YouTubeAPIKeyEnv string
	DigestFrom       string
	DigestTo         string
	PingEnabled      bool
	PingURL          string

	SlackEnabled    bool
	SlackWebhookEnv string

	TranscriptsPerRun int
	TranscriptLang    string

	MaxCallsPerRun int
	MaxCallsPerDay int

	HNMinPoints       int
	MaxItemAgeDays    int
	SkyDays           int
	ClassifyBatchSize int
	MatchThreshold    float64
	HotThreshold      int

	ClassifyWatermark string

	ExtractEnabled   bool
	ExtractBatchSize int
	ExtractWatermark string
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

		// These fall back only when the row is missing, but they still have to
		// agree with what bootstrap seeds -- a fallback quietly stricter than the
		// seed would abort a run that the seeded instance next door completes.
		// Read raw, NOT through str(): that helper treats an empty value as absent and
		// substitutes the default, which makes "empty switches the round-up off"
		// impossible to express. The default for this key lives in bootstrap's seed,
		// where it is applied once to a missing row rather than on every read.
		ScheduleWeeklyDay:  s[store.KeyScheduleWeeklyDay],
		ScheduleWeeklyTime: str(s, store.KeyScheduleWeeklyTime, "11:00"),

		DigestEnabled:    integer(s, store.KeyDigestEnabled, 0) == 1,
		SMTPHost:         str(s, store.KeySMTPHost, "smtp.resend.com"),
		SMTPPort:         integer(s, store.KeySMTPPort, 587),
		SMTPUsername:     str(s, store.KeySMTPUsername, "resend"),
		SMTPPasswordEnv:  str(s, store.KeySMTPPasswordEnv, "RESEND_API_KEY"),
		YouTubeAPIKeyEnv: str(s, store.KeyYouTubeAPIKeyEnv, "YOUTUBE_API_KEY"),
		DigestFrom:       str(s, store.KeyDigestFrom, ""),
		DigestTo:         str(s, store.KeyDigestTo, ""),
		PingEnabled:      integer(s, store.KeyPingEnabled, 0) == 1,
		PingURL:          str(s, store.KeyPingURL, ""),

		SlackEnabled:    integer(s, store.KeySlackEnabled, 0) == 1,
		SlackWebhookEnv: str(s, store.KeySlackWebhookEnv, "SLACK_WEBHOOK_URL"),

		TranscriptsPerRun: integer(s, store.KeyTranscriptsPerRun, 6),
		TranscriptLang:    str(s, store.KeyTranscriptLang, "en"),

		MaxCallsPerRun: integer(s, store.KeyMaxCallsPerRun, 400),
		MaxCallsPerDay: integer(s, store.KeyMaxCallsPerDay, 1000),

		HNMinPoints:       integer(s, store.KeyHNMinPoints, 25),
		MaxItemAgeDays:    integer(s, store.KeyMaxItemAgeDays, 30),
		SkyDays:           integer(s, store.KeySkyDays, 7),
		ClassifyBatchSize: integer(s, store.KeyClassifyBatchSize, 30),
		MatchThreshold:    float(s, store.KeyMatchThreshold, 4),
		HotThreshold:      integer(s, store.KeyHotThreshold, 3),

		ClassifyWatermark: str(s, store.KeyClassifyWatermark, ""),

		ExtractEnabled:   integer(s, store.KeyExtractEnabled, 1) == 1,
		ExtractBatchSize: integer(s, store.KeyExtractBatchSize, 30),
		// Read through str() like the classify mark: an empty value here and an
		// absent row mean the same thing -- start from the beginning -- so there is
		// nothing for the empty-is-meaningful trap to catch.
		ExtractWatermark: str(s, store.KeyExtractWatermark, ""),
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

// SlackWebhook resolves the webhook URL from the environment at call time.
//
// The settings table holds the variable's NAME; the value is read here and never
// written anywhere. Same contract as SMTPConfig.Password and the provider
// registry, and for a sharper reason than either: a Slack incoming webhook URL is
// a bearer credential, so storing it would put the ability to post into the
// workspace in a file whose whole purpose is to be copied around as a backup.
func (r Runtime) SlackWebhook() (string, bool) {
	if r.SlackWebhookEnv == "" {
		return "", false
	}
	v := os.Getenv(r.SlackWebhookEnv)
	return v, v != ""
}
