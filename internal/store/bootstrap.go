package store

import "fmt"

// Settings keys. Everything the owner can change lives here rather than in env,
// because §3.1 requires changes to take effect without a restart.
const (
	KeyProviders          = "providers"            // JSON array of {name, base_url, api_key_env}
	KeyClassifierModel    = "classifier_model"     // "provider/model"
	KeySynthesizerModel   = "synthesizer_model"    // "provider/model"
	KeyClassifierFallback = "classifier_fallback"  // JSON array of "provider/model"
	KeySynthFallback      = "synthesizer_fallback" // JSON array of "provider/model"

	KeyScheduleIngest    = "schedule_ingest"     // "HH:MM" local
	KeyScheduleSynthesis = "schedule_synthesize" // "HH:MM" local

	KeyMaxCallsPerRun = "max_calls_per_run"
	KeyMaxCallsPerDay = "max_calls_per_day"

	KeyHNMinPoints       = "hn_min_points"
	KeySkyDays           = "sky_days"
	KeyMatchThreshold    = "match_threshold"
	KeyHotThreshold      = "hot_threshold"
	KeyClassifyBatchSize = "classify_batch_size"

	// Watermark: only items fetched after this are classified, so a second
	// refresh in the same day costs nothing.
	KeyClassifyWatermark = "last_classified_fetched_at"
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

// defaultSettings is the cheap-first Phase 0 posture decided 2026-08-11 (§3.1):
// both roles on deepseek/deepseek-chat, with the escalation ladder as the runbook
// when quality forces a change.
func defaultSettings() map[string]string {
	return map[string]string{
		KeyProviders:          defaultProviders,
		KeyClassifierModel:    "deepseek/deepseek-chat",
		KeySynthesizerModel:   "deepseek/deepseek-chat",
		KeyClassifierFallback: `[]`,
		KeySynthFallback:      `[]`,

		KeyScheduleIngest:    "09:30",
		KeyScheduleSynthesis: "10:30",

		KeyMaxCallsPerRun: "200",
		KeyMaxCallsPerDay: "500",

		KeyHNMinPoints:       "25", // decision #4
		KeySkyDays:           "7",  // §5.4
		KeyMatchThreshold:    "4",  // §5.2
		KeyHotThreshold:      "3",  // §6, gold + pulse
		KeyClassifyBatchSize: "30", // §5.2

		KeyClassifyWatermark: "",
	}
}

// placeholderTopics carries the seven topic names and the constellation
// positions from the approved prototype, so a fresh instance renders the sky the
// mock promised. Definitions are deliberately empty: §7 says the UI shows a
// setup hint until topics are edited, and an empty definition is that signal.
var placeholderTopics = []struct {
	Title      string
	PosX, PosY float64
}{
	{"Agents & Harness Engineering", 0.21, 0.34},
	{"Evolving Software Roles", 0.11, 0.72},
	{"Open-weight & On-prem", 0.46, 0.22},
	{"Voice AI", 0.74, 0.15},
	{"Claude Code & Coding Agents", 0.63, 0.50},
	{"LLM Ops", 0.85, 0.68},
	{"Small-team AI Product", 0.33, 0.78},
}

// seedSources is a starter set, not Long's curated list -- §9 references a rev-2
// list that never made it into rev 3. Every instance owner prunes their own, so
// these exist to make a fresh install produce a non-empty sky on day one.
//
// Every RSS URL here was verified to return 200 before being seeded. A feed that
// 404s forever is worse than no feed: it teaches the owner to ignore the failure
// list, which is the one place a genuinely broken source shows up.
var seedSources = []struct {
	Kind, Name, Config string
	Active             bool
}{
	{"rss", "Simon Willison", `{"url":"https://simonwillison.net/atom/everything/"}`, true},
	{"rss", "Latent Space", `{"url":"https://www.latent.space/feed"}`, true},
	{"rss", "Pragmatic Engineer", `{"url":"https://blog.pragmaticengineer.com/rss/"}`, true},
	{"rss", "Eugene Yan", `{"url":"https://eugeneyan.com/rss/"}`, true},
	{"rss", "Hamel Husain", `{"url":"https://hamel.dev/index.xml"}`, true},
	{"rss", "Interconnects", `{"url":"https://www.interconnects.ai/feed"}`, true},
	{"rss", "Ahead of AI", `{"url":"https://magazine.sebastianraschka.com/feed"}`, true},

	{"hn_search", "HN · AI agents", `{"query":"AI agents"}`, true},
	{"hn_search", "HN · open weights LLM", `{"query":"open weights LLM"}`, true},
	{"hn_search", "HN · LLM evaluation", `{"query":"LLM evaluation"}`, true},
	{"hn_search", "HN · voice AI", `{"query":"voice AI"}`, true},

	// Inactive on purpose: a YouTube feed needs a channel ID only the owner
	// knows (§9 says "channels Long actually watches"), and guessing one seeds a
	// permanent failure. This row exists so the config shape is discoverable in
	// Settings -- fill in the ID and switch it on.
	{"youtube_rss", "YouTube · add your channel ID", `{"channel_id":""}`, false},
}

// Bootstrap fills an empty database. Each section is independently guarded, so
// this is safe to run on every boot and will not resurrect rows the owner deleted.
func (db *DB) Bootstrap() error {
	if err := db.seedSettings(); err != nil {
		return err
	}
	if err := db.seedTopics(); err != nil {
		return err
	}
	return db.seedSources()
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

func (db *DB) seedTopics() error {
	var n int
	if err := db.QueryRow("SELECT count(*) FROM topics").Scan(&n); err != nil {
		return fmt.Errorf("count topics: %w", err)
	}
	if n > 0 {
		return nil
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("seed topics: begin: %w", err)
	}
	defer tx.Rollback()

	for i, t := range placeholderTopics {
		_, err := tx.Exec(
			`INSERT INTO topics (title, definition, priority, active, pos_x, pos_y)
			 VALUES (?, '', ?, 1, ?, ?)`,
			t.Title, len(placeholderTopics)-i, t.PosX, t.PosY,
		)
		if err != nil {
			return fmt.Errorf("seed topic %q: %w", t.Title, err)
		}
	}
	return tx.Commit()
}

func (db *DB) seedSources() error {
	var n int
	if err := db.QueryRow("SELECT count(*) FROM sources").Scan(&n); err != nil {
		return fmt.Errorf("count sources: %w", err)
	}
	if n > 0 {
		return nil
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("seed sources: begin: %w", err)
	}
	defer tx.Rollback()

	for _, s := range seedSources {
		active := 0
		if s.Active {
			active = 1
		}
		_, err := tx.Exec(
			`INSERT INTO sources (kind, name, config, active) VALUES (?, ?, ?, ?)`,
			s.Kind, s.Name, s.Config, active,
		)
		if err != nil {
			return fmt.Errorf("seed source %q: %w", s.Name, err)
		}
	}
	return tx.Commit()
}
