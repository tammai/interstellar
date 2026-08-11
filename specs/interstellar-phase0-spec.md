# Interstellar — Phase 0 Design Spec

> Personal AI news intelligence tool. Per-user, self-contained, runs locally via Docker.
> **Revision 3 — FINAL for build.** Supersedes rev 2. Companion artifact: `interstellar-starmap-prototype.html` (approved UI concept).

## 0. Decision log

### Standing decisions (unchanged from rev 1–2)
| # | Decision | Choice | Rejected |
|---|----------|--------|----------|
| 1 | Name | **Interstellar** | — |
| 2 | Cadence | Ingest ~09:30 · synthesize ~10:30 · sky fresh by **11:00**; on-demand refresh anytime | Twice-daily ingest (Phase 1 if staleness hurts) |
| 3 | Language | **English** (UI + signals) | — |
| 4 | HN threshold | **min_points = 25** | 10 |
| 5 | LLM billing | **API keys, pay-as-you-go** | `claude -p` / subscription (per-user Agent SDK credit not suited to standing automation; fine for dev iteration) |
| 6 | Sources | RSS + HN Algolia + YouTube channel RSS | LinkedIn (no read API; scraping = ToS/legal risk) |

### Reversed in rev 3 — cause: deployment model changed to per-user independent instances
| # | Was (rev 2) | Now | Why |
|---|-------------|-----|-----|
| 7 | n8n workflows on shared Mac mini | **Standalone single-container app** | n8n was "existing infra" only for Tam; distributable tool can't require each user to stand up n8n + import workflows |
| 8 | Postgres | **SQLite** (file in mounted volume) | Single user, single writer; zero extra containers; backup = copy file |
| 9 | LiteLLM proxy container | **Embedded thin LLM client** (OpenAI-compatible endpoint + model IDs + fallback list, all config) | Proxy per single-user instance is overhead; keep its *spirit*: model-as-config, fallback, spend cap |
| 10 | Email digest as primary channel | **Web UI (star map + list) as front door**; email dropped from Phase 0 | Per-instance pull model; local SMTP config per user is friction. Email → Phase 1 optional |
| 11 | n8n Form for topics | **Settings screens in-app** (Topics, Sources, Model, Schedule) | Same UI, no external dependency |
| 12 | Shared multi-user instance | **One instance per person** (Tam's and Long's are unrelated) | Owner's call. Accepted trade-off: no cross-user analytics; source lists will drift between instances |

## 1. Goal

Same question, new yardstick: **"Do we each actually open it every day?"** Ship a Docker image either of us runs with one command; 2–4 weeks of real use decides Phase 1.

## 2. Non-goals (Phase 0)

No email/notifications; no auth (localhost, single user); no embeddings; no X/LinkedIn; no YouTube transcripts; no multi-instance sync; no archive search.

## 3. Architecture

```
docker run -p 8422:8422 -v interstellar-data:/data \
  -e LLM_API_KEY=... interstellar

┌─ container ──────────────────────────────────────┐
│ scheduler (in-process cron)                       │
│    09:30 ingest ─▶ SQLite (/data/interstellar.db) │
│   10:30 classify + compose signals                │
│ LLM client → OpenAI-compatible endpoint (env/cfg) │
│ HTTP server @ :8422                               │
│   /        sky UI (embedded static assets)       │
│   /api/*   sky data, signal detail, settings,    │
│            POST /api/refresh (on-demand run)      │
└───────────────────────────────────────────────────┘
```

**Stack: Go, single binary, embedded UI (`go:embed`), SQLite via a pure-Go driver (no cgo) → trivial multi-arch image (amd64 + arm64 — assume Apple Silicon on at least one machine).** Rejected: Node/TS (heavier image, no offsetting win), Python (packaging friction). Scratch/distroless base; image target < 30 MB.

### 3.1 LLM client — provider registry (any model, per role)

Hard requirement: **either role can run on any model from any provider, switched via settings without touching code or restarting.**

- **Provider registry** (settings, JSON): named entries `{base_url, api_key_env}`. Presets shipped: `deepseek`, `anthropic`, `google`, `openrouter`, `litellm`, `ollama` (keyless) — plus user-defined entries for anything OpenAI-compatible. Keys referenced by env var name, never stored in SQLite.
- **Per-role model refs:** `classifier = deepseek/deepseek-chat`, `synthesizer = anthropic/<sonnet>` — roles mix providers freely.
- **Cross-provider fallback chains** per role (e.g. `classifier_fallback = [google/gemini-flash-lite, anthropic/<haiku>]`). Retry w/ backoff on timeout/429/5xx → next entry, even on another provider.
- **Interface contract: OpenAI-compatible chat completions only.** Verified Aug 2026, this one contract covers DeepSeek (native OpenAI format), Google (compat endpoint), Anthropic (OpenAI SDK compat layer over Messages API), OpenRouter/LiteLLM/Groq/Together, and Ollama — effectively any hosted or local model. Explicitly out of scope: Bedrock/Vertex/Foundry native auth (IAM/Entra); if ever needed (Phase 2b corporate billing), point a registry entry at a LiteLLM gateway fronting them — never native cloud SDKs in-app.
- **Settings → Model screen:** manage providers, assign role → provider/model, order fallbacks, plus a **"test call" button** per role (canned prompt, shows parsed result) so any swap is verifiable in 10 seconds.
- **Phase 0 defaults — cheap-first (decided 2026-08-11):** both roles `deepseek/deepseek-chat` (V4 Flash, $0.14/$0.28 per 1M as of Aug 2026 → ~$3/month/instance). **Escalation rule:** classifier fails the JSON parse gate → step up (google flash tier → anthropic haiku); digest reads poorly → step synthesizer up (→ anthropic sonnet). Each step is a settings change. Caveats: DeepSeek's announced (not yet effective) 2× peak-hour pricing catches the 10:30 ICT run — immaterial at this scale; data path goes to a China-based provider — each instance owner decides for their own topic definitions.
- **Spend guard:** hard cap on LLM calls per run + per day; breached → run aborts, UI banner shows why.
- Per-call provider/model/token log → `llm_calls` table (add `provider` column; feeds the DuckDB habit later).
- Ollama shadow-mode (Phase 1b) is now literally: add registry entry, flip role, compare — zero code, no restart.

## 4. Data model (SQLite)

```sql
-- single user ⇒ no users table
CREATE TABLE topics (
  id INTEGER PRIMARY KEY,
  title TEXT NOT NULL, definition TEXT NOT NULL,
  priority INTEGER NOT NULL DEFAULT 0, active INTEGER NOT NULL DEFAULT 1,
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE sources (
  id INTEGER PRIMARY KEY,
  kind TEXT NOT NULL CHECK (kind IN ('rss','hn_search','youtube_rss')),
  name TEXT NOT NULL, config TEXT NOT NULL,        -- JSON
  active INTEGER NOT NULL DEFAULT 1, last_run_at TEXT,
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE items (
  id INTEGER PRIMARY KEY, source_id INTEGER NOT NULL REFERENCES sources(id),
  canonical_url TEXT NOT NULL UNIQUE,
  title TEXT NOT NULL, summary TEXT, published_at TEXT,
  fetched_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE matches (            -- classifier verdicts, score >= 4 only
  item_id INTEGER NOT NULL REFERENCES items(id),
  topic_id INTEGER NOT NULL REFERENCES topics(id),
  score REAL NOT NULL, reason TEXT,
  PRIMARY KEY (item_id, topic_id)
);
-- one star = one signal (cross-source story cluster), composed by synthesizer
CREATE TABLE signals (
  id INTEGER PRIMARY KEY, run_date TEXT NOT NULL,
  topic_id INTEGER NOT NULL REFERENCES topics(id),
  title TEXT NOT NULL, reason TEXT NOT NULL,
  score REAL NOT NULL,            -- max over member items
  source_count INTEGER NOT NULL,  -- drives hot flag (>=3) and glow
  primary_url TEXT NOT NULL,
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE signal_items (
  signal_id INTEGER NOT NULL REFERENCES signals(id),
  item_id INTEGER NOT NULL REFERENCES items(id),
  PRIMARY KEY (signal_id, item_id)
);
CREATE TABLE llm_calls (
  id INTEGER PRIMARY KEY, run_date TEXT NOT NULL, role TEXT NOT NULL,
  model TEXT NOT NULL, tokens_in INTEGER, tokens_out INTEGER,
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
```

**Signals are the unit the sky renders** — the mock's `srcs` count becomes `signals.source_count`. Cross-source merging happens in the synthesizer pass (LLM clusters matched items into stories); URL-level dedup stays at insert time. This resolves the "dedup vs trending are the same problem" note from rev 1 concretely.

## 5. Pipeline (scheduler or POST /api/refresh)

1. **Ingest** — per active source, fetch + normalize + canonicalize URL, `INSERT OR IGNORE`. Per-source try/continue; `last_run_at` on success only.
2. **Classify** — per topic, batch 30 items (title+summary+source), strict JSON out `[{item_id, score, reason}]`, keep ≥ 4 → `matches`. Per-topic calls, not a 7-topic mega-prompt (sharper verdicts, per-topic hit-rate stats free).
3. **Compose signals** — synthesizer receives today's matches per topic; clusters same-story items; emits signals JSON (title ≤ 120 chars, reason, member item ids). App computes score = max, source_count = distinct sources.
4. Sky reads last **7 days** of signals; age drives dimming.

## 6. UI requirements (locked by prototype)

Reference implementation: `interstellar-starmap-prototype.html`. Contract, not suggestion:

- **Sky view (front door):** size = score; gold + pulse = hot (source_count ≥ 3); opacity fades with age; constellation = topic with dashed atlas-style links + labels; celestial graticule background.
- **Star rendering:** color-temperature variation for normal stars; tapered diffraction spikes (vertical pair longer); rotating glint on hot stars; hard bright core. 
- **Navigation:** wheel zoom to cursor (clamp 0.55–3.5×), drag pan, double-click/⌂ reset, touch pan + pinch; **two-layer parallax dust** (0.25× / 0.55×). **No 3D rotation** — rejected: no third data dimension; breaks glanceability and spatial memory.
- **Detail panel:** floating frosted-glass card (blur ≥ 16px, translucent gradient, glow border, top hairline highlight), fade + zoom open/close with slight overshoot, origin right-center, Esc closes.
- **List view:** same visual language (dot size/gold/fade), grouped by topic, empty topic renders "No signal today".
- **Settings:** gear button by Sky/List toggle → glass popover → screens for Topics CRUD, Sources CRUD, Model config, Schedule.
- **Typography: Noto Sans for UI/body (400/500/600) + system fallback; Playfair Display 600 for display (wordmark, panel titles). Both OFL-licensed — font files BUNDLED in the image, no hotlinking, renders offline.**
- **Quality floor:** `prefers-reduced-motion` disables twinkle/pulse/glint/parallax easing; keyboard focus visible; responsive to ~380px.

### Known build problems (not solved in mock)
- Collision spacing when a constellation exceeds ~15 stars → simple force-relaxation pass.
- One signal matching multiple topics → place star between constellations; render in both topic groups in list view.
- Canvas perf at 100+ stars with gradient spikes → measure; if needed, pre-render star sprites to offscreen canvas (`drawImage`), WebGL only as last resort.

## 7. Config & distribution

- Env: provider API keys by name (e.g. `DEEPSEEK_API_KEY`, `ANTHROPIC_API_KEY` — only the ones your registry references), `PORT` (8422), `TZ` (schedule honors it).
- Volume: `/data` (SQLite). Backup = copy one file.
- First-run: DB bootstrapped with seed sources + placeholder topics; UI shows setup hint until topics edited.
- Multi-arch image (amd64/arm64), README with the one-line `docker run`.

## 8. Cost model

Same order of magnitude as rev 2 (~250 items/day, cheap classifier + strong synthesizer): **< $1/day per instance** — each user pays their own key. Numbers are estimates from recalled pricing; verify before quoting. Hard guard: per-run/per-day call caps (§3.1).

## 9. Seed data

Long's 7 topics (verbatim definitions). Sources: rev 2 list (~20 blogs/HN queries) + YouTube RSS (Latent Space, AI Engineer, Anthropic + channels Long actually watches). Each instance owner prunes their own.

## 10. Acceptance criteria

- [ ] `docker run` one-liner → sky at `localhost:8422` on amd64 and arm64.
- [ ] Scheduled run completes by 11:00 local; sky shows today's signals.
- [ ] POST /api/refresh completes < ~3 min; UI shows progress state.
- [ ] Dead feed doesn't break ingest; re-ingest creates zero duplicates.
- [ ] Classifier JSON parses 100% on a 200-item sample (gate before any model swap).
- [ ] Role switched to a different provider via Settings takes effect on next run without restart; "test call" verifies in-UI.
- [ ] Hot signals (≥3 sources) render gold; empty topics say "No signal today".
- [ ] Zoom/pan/reset + touch work; reduced-motion verified.
- [ ] Call-cap guard aborts a runaway run and surfaces a UI banner.
- [ ] Kill container mid-run → restart → no corruption, next run clean.

## Phase 1 backlog

Optional email digest (SMTP config in settings); YouTube transcripts + video summaries; Ollama shadow-mode via registry entry; fuzzy dedup upgrades; trending over time (constellation history); manual-forward inbox (paste-a-link, the LinkedIn path); per-topic hit-rate view; source-list export/import (counters instance drift); signal search/archive.

---

## Roadmap beyond Phase 0 — gates, not dates

Later phases are intentionally unspecced; each opens only when its gate trips during real use. Whichever failure mode appears first gets built first.

| Phase | Theme | Gate (real-use signal) | Candidate work |
|-------|-------|------------------------|----------------|
| 1 | Signal quality | We open it daily but complain about content | Fuzzy dedup; **YouTube transcripts + video summaries**; manual-forward inbox (the LinkedIn path); per-topic hit-rate view; source list import/export |
| 1b | Model & cost | Curiosity or bill pressure (independent, non-blocking) | Ollama shadow-mode via registry entry → compare verdicts 2 weeks → cutover decision |
| 2 | Reach & habit | "I forgot to open it" — pull-model failure | Optional email digest (SMTP in settings) or notification ping. **If this gate trips, it outranks all of Phase 1** — an unopened tool has no quality |
| 2b | Multi-user | A third person asks for it | Either polished self-host distribution (keep per-instance) or shared instance + auth (one-way door: privacy, ops ownership — decide only when real users exist) |
| 3 | Intelligence | ≥2–3 months of accumulated signals | Trending over time (constellation history); weekly synthesis; semantic search/archive; ask-the-sky Q&A. Builds on existing `signals` table — no prep needed now |
| — | Never (unless the world changes) | — | X API (pricing), LinkedIn scraping (ToS/legal), 3D rotation (rejected with cause, §6) |
