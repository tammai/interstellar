package store

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tammai/interstellar/seed"
)

// Settings keys. Everything the owner can change lives here rather than in env,
// because §3.1 requires changes to take effect without a restart.
const (
	KeyProviders          = "providers"            // JSON array of {name, base_url, api_key_env}
	KeyClassifierModel    = "classifier_model"     // "provider/model"
	KeySynthesizerModel   = "synthesizer_model"    // "provider/model"
	KeyClassifierFallback = "classifier_fallback"  // JSON array of "provider/model"
	KeySynthFallback      = "synthesizer_fallback" // JSON array of "provider/model"
	KeyChatModel          = "chat_model"           // "provider/model", the chat screen
	KeyChatFallback       = "chat_fallback"        // JSON array of "provider/model"

	KeyScheduleIngest    = "schedule_ingest"     // "HH:MM" local
	KeyScheduleSynthesis = "schedule_synthesize" // "HH:MM" local
	// The weekly round-up: a day name and a time, both local. Empty day switches
	// it off.
	KeyScheduleWeeklyDay  = "schedule_weekly_day"
	KeyScheduleWeeklyTime = "schedule_weekly_time"

	KeyMaxCallsPerRun = "max_calls_per_run"
	KeyMaxCallsPerDay = "max_calls_per_day"
	// Chat's own daily allowance, on top of the global one. Chat must not be able
	// to spend the budget tomorrow morning's run needs.
	KeyMaxChatCallsPerDay = "max_chat_calls_per_day"

	// Videos to transcribe per run, and the caption language to prefer. Zero
	// switches the stage off entirely.
	KeyTranscriptsPerRun = "transcripts_per_run"
	KeyTranscriptLang    = "transcript_lang"

	// Delivery, off by default. Spec decision #10 dropped email from Phase 0 on
	// purpose; these exist so Phase 2 is a settings change rather than a rebuild.
	//
	// The password is referenced by ENV VAR NAME, never stored -- the same rule as
	// provider keys. Defaults point at Resend's relay, which authenticates with the
	// literal username "resend" and an API key as the password.
	KeyDigestEnabled   = "digest_enabled"
	KeySMTPHost        = "smtp_host"
	KeySMTPPort        = "smtp_port"
	KeySMTPUsername    = "smtp_username"
	KeySMTPPasswordEnv = "smtp_password_env"
	// KeyYouTubeAPIKeyEnv holds the NAME of the env var carrying a YouTube Data
	// API key, never the key. Same contract as the provider registry and SMTP.
	KeyYouTubeAPIKeyEnv = "youtube_api_key_env"
	KeyDigestFrom       = "digest_from"
	KeyDigestTo         = "digest_to"
	KeyPingEnabled      = "ping_enabled"
	KeyPingURL          = "ping_url"

	// Slack delivery for the daily delta digest.
	//
	// Separate keys from ping_*, which belong to the weekly round-up: different
	// cadence, different content, and a different rule about what may travel. The
	// webhook is referenced by env var NAME, never stored -- a Slack incoming
	// webhook URL is a bearer credential, not an address, and anyone holding it can
	// post into the workspace. Same contract as the provider keys and SMTP.
	KeySlackEnabled    = "slack_enabled"
	KeySlackWebhookEnv = "slack_webhook_env"

	KeyHNMinPoints = "hn_min_points"
	// Items published longer ago than this are not news and are skipped at
	// ingest. Feeds routinely serve their whole archive.
	KeyMaxItemAgeDays    = "max_item_age_days"
	KeySkyDays           = "sky_days"
	KeyMatchThreshold    = "match_threshold"
	KeyHotThreshold      = "hot_threshold"
	KeyClassifyBatchSize = "classify_batch_size"

	// The instance-wide classify watermark, retired by migration 011 in favour of
	// a cursor per topic (topics.classified_through). Nothing reads it any more
	// except that migration, which seeds the cursors from it.
	KeyClassifyWatermark = "last_classified_fetched_at"

	// Claim extraction (migration 009). Its own watermark, deliberately separate
	// from the classify one: the two stages advance independently, and sharing a
	// mark would make either stage's stall silently skip items for the other.
	KeyExtractWatermark = "last_extracted_fetched_at"
	KeyExtractBatchSize = "extract_batch_size"
	// Zero switches extraction off entirely, the same way transcripts_per_run: 0
	// does. The stage is new and reads nothing downstream yet, so an owner who
	// wants their morning exactly as it was can have it.
	KeyExtractEnabled = "extract_enabled"
)

// defaultProviders ships the presets §3.1 names. Keys are referenced by env var
// NAME -- the value is read from the environment at call time and never stored.
// An empty api_key_env means keyless (Ollama), and no Authorization header.
//
// The ollama and litellm entries point at host.docker.internal because this app
// runs in a container and those services run on the host; the owner can edit
// any of these in Settings -> Model.
const defaultProviders = `[
  {"name":"deepseek",  "base_url":"https://api.deepseek.com/v1",                          "api_key_env":"DEEPSEEK_API_KEY"},
  {"name":"anthropic", "base_url":"https://api.anthropic.com/v1",                         "api_key_env":"ANTHROPIC_API_KEY"},
  {"name":"google",    "base_url":"https://generativelanguage.googleapis.com/v1beta/openai","api_key_env":"GOOGLE_API_KEY"},
  {"name":"openrouter","base_url":"https://openrouter.ai/api/v1",                         "api_key_env":"OPENROUTER_API_KEY"},
  {"name":"litellm",   "base_url":"http://host.docker.internal:4000/v1",                  "api_key_env":"LITELLM_API_KEY"},
  {"name":"ollama",    "base_url":"http://host.docker.internal:11434/v1",                 "api_key_env":""}
]`

// defaultSettings is the cheap-first Phase 0 posture decided 2026-08-11 (§3.1),
// routed through OpenRouter since rev 6 (decision #23): the same
// deepseek-chat model, reached with a key that also reaches every other model
// the escalation ladder names. Stepping a role up a tier is then a settings
// change on its own, which is what §3.1 asks for -- against the vendor's own
// endpoint it also needed a new environment variable and a restart to supply it.
//
// Note the ref nests: OpenRouter's model ids contain a slash, and ParseModelRef
// splits on the first one only, so provider "openrouter" carries model
// "deepseek/deepseek-chat".
func defaultSettings() map[string]string {
	return map[string]string{
		KeyProviders:          defaultProviders,
		KeyClassifierModel:    "openrouter/deepseek/deepseek-chat",
		KeySynthesizerModel:   "openrouter/deepseek/deepseek-chat",
		KeyClassifierFallback: `[]`,
		KeySynthFallback:      `[]`,
		// Chat answers in prose a person reads, which is the synthesizer's kind of
		// work rather than the classifier's, so it starts on the same model.
		KeyChatModel:    "openrouter/deepseek/deepseek-chat",
		KeyChatFallback: `[]`,

		KeyScheduleIngest:    "09:30",
		KeyScheduleSynthesis: "10:30",
		// Monday morning, after the daily run has produced that day's sky. The
		// round-up covers the week that just finished, so Monday is the first day it
		// can be complete.
		KeyScheduleWeeklyDay:  "mon",
		KeyScheduleWeeklyTime: "11:00",

		// Sized against the seed list, not against a blank instance. The starter
		// set is ~62 active sources and max_item_age_days admits a 30-day
		// backlog, so a first run can reach 500-900 items; at a 30-item batch
		// across 7 topics that is 140-210 classifier calls before synthesis.
		// The old 200 would abort the very first run part-way and show the
		// runaway banner on entirely normal work -- the one run that has to go
		// well. Steady-state runs classify only what is past the watermark and
		// sit far below this; the cap is still a ceiling, not a budget.
		KeyMaxCallsPerRun: "400",
		KeyMaxCallsPerDay: "1000",
		// Two calls per question (plan, then answer), so 100 is roughly fifty
		// questions a day -- far more than a habit needs, and small enough that it
		// cannot eat the 1000 the pipeline shares with it.
		KeyMaxChatCallsPerDay: "100",

		// Eight seeded YouTube channels post a handful of videos a day between
		// them, so six per run clears the backlog within a couple of days and
		// costs six cheap calls. Set to 0 to switch transcripts off.
		KeyTranscriptsPerRun: "6",
		KeyTranscriptLang:    "en",

		// Off. The web UI is the front door (decision #10); this is here to be
		// switched on if the Phase 2 gate -- "I forgot to open it" -- ever trips.
		KeyDigestEnabled: "0",
		// Resend's relay. Username is the literal string "resend"; the password is
		// an API key, read from RESEND_API_KEY at send time and never stored.
		KeySMTPHost:        "smtp.resend.com",
		KeySMTPPort:        "587",
		KeySMTPUsername:    "resend",
		KeySMTPPasswordEnv: "RESEND_API_KEY",
		// Seeded to a name, not a value. Video discovery is dead without a key,
		// and the error says so by name -- see sources.YouTubeFetcher.
		KeyYouTubeAPIKeyEnv: "YOUTUBE_API_KEY",
		// onboarding@resend.dev works without verifying a domain, but only to the
		// address that owns the Resend account. Change both once a domain is set up.
		KeyDigestFrom:  "Interstellar <onboarding@resend.dev>",
		KeyDigestTo:    "",
		KeyPingEnabled: "0",
		KeyPingURL:     "",

		// Off until the owner sets the variable and switches it on. Seeded to a
		// NAME, like youtube_api_key_env: the error when it is unset then says
		// which variable to set rather than "no webhook configured".
		KeySlackEnabled:    "0",
		KeySlackWebhookEnv: "SLACK_WEBHOOK_URL",

		KeyHNMinPoints:       "25", // decision #4
		KeyMaxItemAgeDays:    "30",
		KeySkyDays:           "7",  // §5.4
		KeyMatchThreshold:    "4",  // §5.2
		KeyHotThreshold:      "3",  // §6, gold + pulse
		KeyClassifyBatchSize: "30", // §5.2

		// Seeded EMPTY on purpose, including on an instance that already holds a
		// month of items -- unlike the classify watermark, where an empty mark on a
		// fresh instance merely means "everything" and there is nothing behind it.
		//
		// Here the backlog is the point. Velocity ("how often has this been
		// mentioned in 14 days") and staleness ("nothing from this vendor in 60
		// days") are both meaningless against an empty history: a world model built
		// only from today says every entity is brand new and nothing is stale, which
		// is wrong about every entity at once. Starting from zero gives it the
		// history that is already sitting in `items`.
		//
		// The cost is bounded and small. extractWindowLimit caps a pass the way
		// classifyWindowLimit does, so a backfill is at most a few dozen batched
		// calls once, against a per-run cap that a steady-state run leaves mostly
		// unspent -- classify only ever sees what is past its own mark.
		KeyExtractWatermark: "",
		KeyExtractBatchSize: "30", // same as classify; one item's worth of prompt each

		// OFF, on measurement rather than caution. A first real pass over 1000 items
		// (2026-09-22, deepseek-chat) produced 43 claims across 12 of 134 entities
		// against 400 unresolved names -- and the claims themselves did not hold up:
		// "GPT-6 Astra is a significant advancement in AI technology" was stored as a
		// `launch`, which is commentary the prompt's own rule calls `none`, and two
		// claims landed on the wrong entity because the model named a near-neighbour
		// (langchain's alpha release attributed to LangGraph, V4.1 Flash to
		// V4-Pro). Alias resolution has no way to catch that: the name it was given
		// really does resolve, just not to the thing the item was about.
		//
		// Shipping it on would accumulate claims nobody should trust, which makes the
		// eventual evaluation harder rather than easier -- the table would have to be
		// cleared before any honest second measurement. Turn it on once the prompt
		// holds the "none" line and the watchlist has first-party sources; the stage,
		// its tests and its endpoints are all here and it is a one-key change.
		KeyExtractEnabled: "0",
	}
}

// Seed markers record that a table has had its defaults, so they arrive once per
// database rather than whenever the table is empty. Without them an owner who
// deleted every topic or source to start clean got the full default list back on
// the next restart. Migration 012 sets both on databases that predate them.
const (
	keySeededTopics  = "seeded_topics"
	keySeededSources = "seeded_sources"
)

// Bootstrap fills an empty database from the embedded default seed. See
// BootstrapWith.
func (db *DB) Bootstrap() error {
	return db.BootstrapWith(func() (Seed, error) { return ParseSeed(seed.Default()) })
}

// BootstrapWith fills an empty database: default settings, then topics and sources
// from the seed load returns. Each section is independently guarded, so this is
// safe to run on every boot and will not resurrect rows the owner deleted --
// including all of them.
//
// load is called only if a table still needs seeding, which on any database past
// its first boot is never. So a seed file edited, broken or removed later cannot
// stop an existing instance from starting; it only matters to a new one, and a new
// one refuses to start on a seed that does not parse rather than start half-filled.
func (db *DB) BootstrapWith(load func() (Seed, error)) error {
	if err := db.seedSettings(); err != nil {
		return err
	}

	var s *Seed
	get := func() (Seed, error) {
		if s == nil {
			loaded, err := load()
			if err != nil {
				return Seed{}, err
			}
			s = &loaded
		}
		return *s, nil
	}
	if err := db.seedTopics(get); err != nil {
		return err
	}
	return db.seedSources(get)
}

// seedSettings inserts any missing key without overwriting one the owner has set.
func (db *DB) seedSettings() error {
	existing, err := db.Settings()
	if err != nil {
		return err
	}
	missing := map[string]string{}
	for k, v := range defaultSettings() {
		if _, ok := existing[k]; !ok {
			missing[k] = v
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return db.SetSettings(missing)
}

func (db *DB) seedTopics(get func() (Seed, error)) error {
	tx, proceed, err := db.beginSeed("topics", keySeededTopics)
	if err != nil || !proceed {
		return err
	}
	defer tx.Rollback()

	s, err := get()
	if err != nil {
		return err
	}
	// Placed in seed order, each against the ones before it, exactly as adding
	// them one at a time in Settings would place them.
	var placed []Topic
	for _, t := range s.Topics {
		pos := Topic{}
		if t.PosX != nil && t.PosY != nil {
			pos.PosX, pos.PosY = *t.PosX, *t.PosY
		} else {
			pos.PosX, pos.PosY = NextTopicPosition(placed)
		}
		placed = append(placed, pos)

		active := t.Active == nil || *t.Active
		if _, err := tx.Exec(
			`INSERT INTO topics (title, definition, priority, active, pos_x, pos_y)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			strings.TrimSpace(t.Title), strings.TrimSpace(t.Definition), t.Priority,
			boolToInt(active), pos.PosX, pos.PosY,
		); err != nil {
			return fmt.Errorf("seed topic %q: %w", t.Title, err)
		}
	}
	return tx.Commit()
}

func (db *DB) seedSources(get func() (Seed, error)) error {
	tx, proceed, err := db.beginSeed("sources", keySeededSources)
	if err != nil || !proceed {
		return err
	}
	defer tx.Rollback()

	s, err := get()
	if err != nil {
		return err
	}
	for _, src := range s.Sources {
		// Compacted, so a seeded row's config reads like one written by the UI.
		var cfg bytes.Buffer
		if err := json.Compact(&cfg, src.Config); err != nil {
			return fmt.Errorf("seed source %q: config: %w", src.Name, err)
		}
		if _, err := tx.Exec(
			`INSERT INTO sources (kind, name, config, active) VALUES (?, ?, ?, ?)`,
			src.Kind, strings.TrimSpace(src.Name), cfg.String(), boolToInt(src.Active),
		); err != nil {
			return fmt.Errorf("seed source %q: %w", src.Name, err)
		}
	}
	return tx.Commit()
}

// beginSeed decides whether a table gets its defaults, and opens the transaction
// they go in with the marker already written -- so the rows and the record that
// they were seeded commit together or not at all.
//
// A table that already has rows is marked and left alone: whatever is there is the
// owner's, seeded or not. proceed is false whenever there is nothing to insert, and
// the returned transaction is then nil.
func (db *DB) beginSeed(table, marker string) (tx *sql.Tx, proceed bool, err error) {
	if _, done, err := db.Setting(marker); err != nil || done {
		return nil, false, err
	}
	var n int
	if err := db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
		return nil, false, fmt.Errorf("count %s: %w", table, err)
	}

	tx, err = db.Begin()
	if err != nil {
		return nil, false, fmt.Errorf("seed %s: begin: %w", table, err)
	}
	if _, err := tx.Exec(`INSERT INTO settings (key, value) VALUES (?, '1')`, marker); err != nil {
		tx.Rollback()
		return nil, false, fmt.Errorf("mark %s seeded: %w", table, err)
	}
	if n > 0 {
		return nil, false, tx.Commit()
	}
	return tx, true, nil
}
