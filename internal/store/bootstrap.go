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
	KeyChatModel          = "chat_model"           // "provider/model", the chat screen
	KeyChatFallback       = "chat_fallback"        // JSON array of "provider/model"

	KeyScheduleIngest    = "schedule_ingest"     // "HH:MM" local
	KeyScheduleSynthesis = "schedule_synthesize" // "HH:MM" local

	KeyMaxCallsPerRun = "max_calls_per_run"
	KeyMaxCallsPerDay = "max_calls_per_day"
	// Chat's own daily allowance, on top of the global one. Chat must not be able
	// to spend the budget tomorrow morning's run needs.
	KeyMaxChatCallsPerDay = "max_chat_calls_per_day"

	// Videos to transcribe per run, and the caption language to prefer. Zero
	// switches the stage off entirely.
	KeyTranscriptsPerRun = "transcripts_per_run"
	KeyTranscriptLang    = "transcript_lang"

	KeyHNMinPoints = "hn_min_points"
	// Items published longer ago than this are not news and are skipped at
	// ingest. Feeds routinely serve their whole archive.
	KeyMaxItemAgeDays    = "max_item_age_days"
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
		// Chat answers in prose a person reads, which is the synthesizer's kind of
		// work rather than the classifier's, so it starts on the same model.
		KeyChatModel:    "deepseek/deepseek-chat",
		KeyChatFallback: `[]`,

		KeyScheduleIngest:    "09:30",
		KeyScheduleSynthesis: "10:30",

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

		KeyHNMinPoints:       "25", // decision #4
		KeyMaxItemAgeDays:    "30",
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
	// --- practitioners and analysis ---
	{"rss", "Simon Willison", `{"url":"https://simonwillison.net/atom/everything/"}`, true},
	{"rss", "Latent Space", `{"url":"https://www.latent.space/feed"}`, true},
	{"rss", "Pragmatic Engineer", `{"url":"https://blog.pragmaticengineer.com/rss/"}`, true},
	{"rss", "Eugene Yan", `{"url":"https://eugeneyan.com/rss/"}`, true},
	{"rss", "Hamel Husain", `{"url":"https://hamel.dev/index.xml"}`, true},
	{"rss", "Interconnects", `{"url":"https://www.interconnects.ai/feed"}`, true},
	{"rss", "Ahead of AI", `{"url":"https://magazine.sebastianraschka.com/feed"}`, true},
	{"rss", "Lilian Weng", `{"url":"https://lilianweng.github.io/index.xml"}`, true},
	{"rss", "Chip Huyen", `{"url":"https://huyenchip.com/feed.xml"}`, true},
	{"rss", "Phil Schmid", `{"url":"https://www.philschmid.de/rss"}`, true},
	{"rss", "Answer.AI", `{"url":"https://www.answer.ai/index.xml"}`, true},
	{"rss", "Vicki Boykis", `{"url":"https://vickiboykis.com/index.xml"}`, true},

	// --- software roles and practice ---
	{"rss", "Martin Fowler", `{"url":"https://martinfowler.com/feed.atom"}`, true},
	{"rss", "ByteByteGo", `{"url":"https://blog.bytebytego.com/feed"}`, true},

	// --- labs and open weights ---
	{"rss", "Hugging Face", `{"url":"https://huggingface.co/blog/feed.xml"}`, true},
	{"rss", "OpenAI", `{"url":"https://openai.com/blog/rss.xml"}`, true},
	{"rss", "Google AI", `{"url":"https://blog.google/technology/ai/rss/"}`, true},
	{"rss", "DeepMind", `{"url":"https://deepmind.google/blog/rss.xml"}`, true},
	{"rss", "Qwen", `{"url":"https://qwenlm.github.io/blog/index.xml"}`, true},
	{"rss", "Together AI", `{"url":"https://www.together.ai/blog/rss.xml"}`, true},
	{"rss", "Replicate", `{"url":"https://replicate.com/blog/rss"}`, true},
	{"rss", "Ollama", `{"url":"https://ollama.com/blog/rss.xml"}`, true},
	{"rss", "Mozilla AI", `{"url":"https://blog.mozilla.ai/rss/"}`, true},
	{"rss", "LangChain", `{"url":"https://www.langchain.com/blog/rss.xml"}`, true},

	// GitHub release feeds. `releases.atom` is ordinary Atom, so these need no new
	// fetcher -- and deliberately so: a dedicated kind would mean widening the
	// CHECK constraint on sources.kind, which in SQLite is a whole-table rebuild,
	// bought for nothing but a nicer item title. The classifier already sees the
	// source name next to the title, so "v0.5.13" arrives as
	// "GitHub · ollama/ollama — v0.5.13" in everything but literal string terms.
	//
	// Which repos ship active was decided from measured release cadence, not taste:
	//   - dormant (no release in 90 days) are not seeded at all -- autogen,
	//     text-generation-inference, faster-whisper, aider and fish-speech were all
	//     dropped here
	//   - over ~8 releases a week is tag churn rather than news, and is seeded
	//     inactive: those repos publish nightly-ish builds whose titles are version
	//     numbers, so every one costs a classification call to score low. llama.cpp
	//     alone ships about 70 a week
	{"rss", "GitHub · anthropics/claude-code", `{"url":"https://github.com/anthropics/claude-code/releases.atom"}`, true},
	{"rss", "GitHub · sst/opencode", `{"url":"https://github.com/sst/opencode/releases.atom"}`, true},
	{"rss", "GitHub · All-Hands-AI/OpenHands", `{"url":"https://github.com/All-Hands-AI/OpenHands/releases.atom"}`, true},
	{"rss", "GitHub · continuedev/continue", `{"url":"https://github.com/continuedev/continue/releases.atom"}`, true},
	{"rss", "GitHub · browser-use/browser-use", `{"url":"https://github.com/browser-use/browser-use/releases.atom"}`, true},
	{"rss", "GitHub · modelcontextprotocol/servers", `{"url":"https://github.com/modelcontextprotocol/servers/releases.atom"}`, true},
	{"rss", "GitHub · modelcontextprotocol/python-sdk", `{"url":"https://github.com/modelcontextprotocol/python-sdk/releases.atom"}`, true},
	{"rss", "GitHub · openai/openai-agents-python", `{"url":"https://github.com/openai/openai-agents-python/releases.atom"}`, true},
	{"rss", "GitHub · crewAIInc/crewAI", `{"url":"https://github.com/crewAIInc/crewAI/releases.atom"}`, true},
	{"rss", "GitHub · ollama/ollama", `{"url":"https://github.com/ollama/ollama/releases.atom"}`, true},
	{"rss", "GitHub · vllm-project/vllm", `{"url":"https://github.com/vllm-project/vllm/releases.atom"}`, true},
	{"rss", "GitHub · huggingface/transformers", `{"url":"https://github.com/huggingface/transformers/releases.atom"}`, true},
	{"rss", "GitHub · ggml-org/whisper.cpp", `{"url":"https://github.com/ggml-org/whisper.cpp/releases.atom"}`, true},
	{"rss", "GitHub · k2-fsa/sherpa-onnx", `{"url":"https://github.com/k2-fsa/sherpa-onnx/releases.atom"}`, true},
	{"rss", "GitHub · pipecat-ai/pipecat", `{"url":"https://github.com/pipecat-ai/pipecat/releases.atom"}`, true},
	{"rss", "GitHub · livekit/agents", `{"url":"https://github.com/livekit/agents/releases.atom"}`, true},
	{"rss", "GitHub · BerriAI/litellm", `{"url":"https://github.com/BerriAI/litellm/releases.atom"}`, true},
	{"rss", "GitHub · langchain-ai/langchain", `{"url":"https://github.com/langchain-ai/langchain/releases.atom"}`, true},
	{"rss", "GitHub · run-llama/llama_index", `{"url":"https://github.com/run-llama/llama_index/releases.atom"}`, true},

	// Seeded inactive: all six release far too often for their titles to be news.
	// Switch one on if you want its churn.
	{"rss", "GitHub · ggml-org/llama.cpp (high volume)", `{"url":"https://github.com/ggml-org/llama.cpp/releases.atom"}`, false},
	{"rss", "GitHub · cline/cline (high volume)", `{"url":"https://github.com/cline/cline/releases.atom"}`, false},
	{"rss", "GitHub · unslothai/unsloth (high volume)", `{"url":"https://github.com/unslothai/unsloth/releases.atom"}`, false},
	{"rss", "GitHub · google-gemini/gemini-cli (high volume)", `{"url":"https://github.com/google-gemini/gemini-cli/releases.atom"}`, false},
	{"rss", "GitHub · Arize-ai/phoenix (high volume)", `{"url":"https://github.com/Arize-ai/phoenix/releases.atom"}`, false},
	{"rss", "GitHub · langfuse/langfuse (high volume)", `{"url":"https://github.com/langfuse/langfuse/releases.atom"}`, false},

	// Hacker News. Queries are deliberately broad single concepts: Algolia does
	// literal matching, so a phrase like "open weights LLM" matches almost
	// nothing. Breadth plus the points threshold is what produces signal --
	// narrow phrasing plus a threshold produces silence, which is what the first
	// seed list did.
	//
	// min_points is per query: a broad term needs a higher bar to stay useful,
	// while a niche one would go quiet at the same threshold.
	{"hn_search", "HN · LLM", `{"query":"LLM","min_points":100}`, true},
	{"hn_search", "HN · AI agents", `{"query":"agent","min_points":75}`, true},
	{"hn_search", "HN · open weights", `{"query":"open weight model","min_points":25}`, true},
	{"hn_search", "HN · local LLM", `{"query":"local LLM","min_points":25}`, true},
	{"hn_search", "HN · Claude", `{"query":"Claude","min_points":50}`, true},
	{"hn_search", "HN · coding agents", `{"query":"coding agent","min_points":25}`, true},
	{"hn_search", "HN · MCP", `{"query":"MCP","min_points":25}`, true},
	{"hn_search", "HN · evals", `{"query":"eval","min_points":50}`, true},
	{"hn_search", "HN · speech and voice", `{"query":"speech recognition","min_points":25}`, true},
	{"hn_search", "HN · text to speech", `{"query":"text to speech","min_points":25}`, true},
	{"hn_search", "HN · inference", `{"query":"inference","min_points":50}`, true},
	{"hn_search", "HN · AI engineer", `{"query":"AI engineer","min_points":25}`, true},

	// YouTube. Every ID below was resolved from the channel's rel="canonical" link
	// and then verified against the <title> its own feed returns -- not scraped
	// from a "channelId" match in the page HTML, which picks up the *recommended*
	// channels in the sidebar. That mistake seeded Karpathy's row with
	// 3Blue1Brown's ID and looked entirely plausible until the feed titles were
	// checked.
	//
	// §9 wants "channels Long actually watches", so treat these as a starting set
	// to prune rather than a recommendation.
	{"youtube_rss", "YouTube · Latent Space", `{"channel_id":"UCvi5jNRoRVm436TVAXet1kQ"}`, true},
	{"youtube_rss", "YouTube · AI Engineer", `{"channel_id":"UCLKPca3kwwd-B59HNr-_lvA"}`, true},
	{"youtube_rss", "YouTube · Andrej Karpathy", `{"channel_id":"UCXUPKJO5MZQN11PqgIvyuvQ"}`, true},
	{"youtube_rss", "YouTube · Yannic Kilcher", `{"channel_id":"UCZHmQk67mSJgfCCTn7xBfew"}`, true},
	{"youtube_rss", "YouTube · Two Minute Papers", `{"channel_id":"UCbfYPyITQ-7l4upoX8nvctg"}`, true},
	{"youtube_rss", "YouTube · IndyDevDan", `{"channel_id":"UC_x36zCEGilGpB1m-V4gmjg"}`, true},
	{"youtube_rss", "YouTube · ThePrimeagen", `{"channel_id":"UC8ENHE5xdFSwx71u3fDH5Xw"}`, true},

	// Inactive on purpose, and kept even though seven real channels now ship: it
	// is the only place the youtube_rss config shape is discoverable in Settings
	// without deleting a working row to look at it.
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
