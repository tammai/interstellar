# Interstellar — Phase 0 Design Spec

> Personal AI news intelligence tool. Per-user, self-contained, runs locally via Docker.
> **Revision 3 — FINAL for build.** Supersedes rev 2. Companion artifact: `interstellar-starmap-prototype.html` (approved UI concept).
> **Revision 4 — amends rev 3, 2026-08-17.** Closes §6's canvas-performance build problem with what measurement actually found, and records the sky-layer preferences it produced. Rev 3's text stands as written; rev 4 is additive — see §0 "Added in rev 4" and §11.
> **Revision 5 — amends rev 3, 2026-08-18.** Moves Settings out of the star map's popover-and-sheet and onto its own page with horizontal tabs, and widens the signal detail panel. Both **supersede** parts of §6 rather than adding to it — see §0 "Changed in rev 5", "Also changed in rev 5", and the note under §6's Settings bullet.
> **Revision 6 — amends rev 3, 2026-09-05.** Changes the seeded default provider from DeepSeek's own endpoint to OpenRouter, carrying the same model. Decision #5 (pay-as-you-go API keys) and the cheap-first posture of §3.1 are unchanged — this changes the route, not the model or the billing model. See §0 "Changed in rev 6".
> **Revision 7 — amends rev 3, 2026-09-05.** Settles which phase the **email digest** belongs to: this document contradicted itself, listing it in `## Phase 1 backlog` and in the Phase 2 row of the roadmap table. The table wins and the backlog paragraph is superseded for that one item. No gate moves and no work is authorised by this — it is a bookkeeping correction to a contradiction that had been noted and left. See §0 "Changed in rev 7".

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

### Added in rev 4 — cause: canvas performance measured rather than assumed
| # | Decision | Choice | Rejected |
|---|----------|--------|----------|
| 13 | Where sky layer visibility lives | **Browser storage, owned by the Sky** | The `settings` table. Nothing here reaches a run, is read by the pipeline, or changes what was ingested/matched/composed. Putting it in `settings` would mean a migration and API surface to express "this reader prefers less scenery", and would inherit §0's "applies on the next run" semantics, which is wrong for something that applies on the next frame |
| 14 | Which layers may be hidden | **Dust, Milky Way, graticule, constellation lines, meteors, motion** | Constellation **labels** and the **stars themselves**. The label is the only place the sky renders §10's "No signal today", so a switch that hid it would turn an acceptance criterion off; the stars are the product |
| 15 | Motion toggle vs system preference | **OR, never override** — the switch can turn motion off, never on | Letting the switch win. §6's quality floor is a guarantee, not a default; a settings control must not be able to defeat `prefers-reduced-motion` |
| 16 | Repainting a static sky | **Skip the frame when nothing can have changed** | Repainting unconditionally. With motion off and the view settled, every frame is identical to the one already on screen |
| 18 | Constellation joins | **A minimum spanning tree over each topic's stars**, and the layer **defaults off** | A polyline in array order — which is score order, not spatial, so a 45-star topic zigzagged across itself and several topics' joins crossed the whole canvas. §6 specifies the links, so defaulting them off is a default and not a removal: at today's density the sky reads better without them, and the Layers panel turns them on |
| 19 | Shared-star ties | **Removed** | Drawing all 49 (a tangle), and drawing the hovered star's only (tried, still no value). §6's "place star between constellations" is met by *placement*; membership is already stated by the tooltip, the detail panel and both list groups. A line was never what carried it |
| 17 | The galactic band | **A bundled photograph**, composited additively into the baked background; the hand-drawn band stays as the fallback | Procedural-only. The photograph carries grain and dust structure no reasonable amount of arc-drawing reproduces, bakes in 3ms against 7.2ms, and adds 66KB to a 20MB binary. Bundled next to the fonts, never fetched — §6's offline rule covers images too |

### Changed in rev 5 — cause: six screens behind a menu, over the surface they configure
| # | Decision | Choice | Rejected |
|---|----------|--------|----------|
| 20 | Where Settings lives | **Its own document at `/settings/`, with a horizontal tab strip** — the same shape as the Ask screen, sharing only `tokens.css` | The gear popover into a sliding sheet, as §6 specified. Two faults, both structural rather than cosmetic. The sheet was a 560px overlay on the right of the star map, so the sky being configured was behind the thing configuring it — and the panel that shows a signal's detail occupies that same corner. And the popover listed the six screens one line each, so the shape of the configuration was recitable but never visible: every screen was a click into a menu and a click out of it. A tab strip states all six at once and switching costs one click. Two further things fall out of it, neither the reason but both real: `app.js` stops importing 1,700 lines of forms to draw a canvas (measured: the sky now loads six assets, the settings page three, and it calls `requestAnimationFrame` zero times), and each screen gets an address — `settings/#model` — which is how the sky's "needs attention" banner now opens the screen that can fix it |
| 21 | What replaces the sheet's close button | **The back link in the header, and the browser's own Back** | A close button. A page is left the way pages are left; the fragment router means Back steps between screens first and then to the sky, which is the order you arrived in |

### Also changed in rev 5 — cause: the panel is where a signal is actually read
| # | Decision | Choice | Rejected |
|---|----------|--------|----------|
| 22 | Detail panel width | **`min(560px, 100vw - 32px)`** — full width to a 560px cap | Two rejections, not one. The prototype's `min(430px, …)`, which §6 makes a contract and this therefore supersedes: the panel's content is a title, a paragraph of reasoning and one line per source, the source list is the part anyone reads, and at 430px each outlet wrapped to two or three lines while the reasoning ran eight words to a line — the card was mostly re-reading. And **800px, which was tried first**: it fixed the wrapping and lost the point of the panel, which is that it sits over the sky. At that width it reads as a page in front of the stars, and the star it describes is behind it. 560 is where the source lines stop wrapping and the sky is still the screen. The ≤640px rule — full-width sheet from the bottom edge — is untouched by any of it |

### Changed in rev 6 — cause: a default whose escalation ladder needed a restart to climb
| # | Was (rev 3) | Now | Why |
|---|-------------|-----|-----|
| 23 | Both roles seeded to `deepseek/deepseek-chat` against `https://api.deepseek.com/v1`, keyed by `DEEPSEEK_API_KEY` | **All three roles seeded to `openrouter/deepseek/deepseek-chat`**, keyed by `OPENROUTER_API_KEY` | Same model, same cheap-first posture, same per-call cost order — only the route changes. §3.1 requires a role to move to any model on any provider "without touching code or restarting", and the seeded default satisfied only half of that: every rung of the escalation ladder (google flash tier → anthropic haiku → anthropic sonnet) is a *different vendor*, so climbing it meant adding an environment variable, which §7 fixes at boot — a restart. One OpenRouter key reaches every rung, so the ladder becomes what the spec says it is: a settings change. Two costs, both accepted. OpenRouter bills a margin over the vendor's own price, which is what buys the single key. And the data path now runs through OpenRouter *and* whichever vendor it routes to, so §3.1's note that each instance owner decides where their topic definitions travel applies to two parties rather than one. Note the ref nests — `ParseModelRef` splits on the first slash only, so the provider is `openrouter` and the model is `deepseek/deepseek-chat`. The registry presets are untouched: DeepSeek's own entry still ships, and pointing a role back at `deepseek/deepseek-chat` restores rev 3's routing exactly |


### Changed in rev 7 — cause: this document disagreed with itself, and both halves were being cited
| # | Was (rev 3) | Now | Why |
|---|-------------|-----|-----|
| 24 | The email digest listed **twice**: in `## Phase 1 backlog` ("Optional email digest (SMTP config in settings)") and in the roadmap table's **Phase 2** row, whose gate is "I forgot to open it" and which states that if that gate trips it "outranks all of Phase 1" | **Phase 2, per the roadmap table.** The `## Phase 1 backlog` paragraph is **superseded for this item only**; the rest of that list stands | The two passages are not equal in kind, which is what settles it. The backlog paragraph is an undifferentiated bag of everything left unspecced after Phase 0 — no gates, no ordering, no precedence. The table is the section immediately after it, and gates and ordering are its entire content; it also carries an explicit precedence rule for this exact item, which a paragraph listing it alphabetically among eight others cannot have meant to overrule. Where a considered statement and a leftover list disagree, the considered one is the decision. Note what this does **not** do. It authorises nothing: the Phase 2 gate has not tripped, and "we forgot to open it" is still the only thing that opens it. It also does not describe what exists — the weekly round-up ships composed-by-default with delivery off, which is decision #10 (delivery turned down, composition not) rather than this. That the capability was built ahead of its gate is the same pattern as the Ask screen at Phase 3, and is recorded in the project's status notes, not here |

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
  > **rev 5 widens this.** The prototype's 430px becomes `min(560px, 100vw - 32px)` — see §0 decision #22, which also records why it is not 800. Everything else in this bullet stands, including the glass, the overshoot and the origin.
- **List view:** same visual language (dot size/gold/fade), grouped by topic, empty topic renders "No signal today".
- **Settings:** gear button by Sky/List toggle → glass popover → screens for Topics CRUD, Sources CRUD, Model config, Schedule.
  > **rev 5 supersedes this line.** The gear is now a link to `/settings/`, its own document, and the screens sit behind a horizontal tab strip rather than a popover and a sheet — see §0 decision #20. Everything else in this bullet stands: the same screens, the same CRUD, and the same visual language, which is why the cards, fields, switches and tables moved across verbatim.
- **Typography: Noto Sans for UI/body (400/500/600) + system fallback; Playfair Display 600 for display (wordmark, panel titles). Both OFL-licensed — font files BUNDLED in the image, no hotlinking, renders offline.**
- **Quality floor:** `prefers-reduced-motion` disables twinkle/pulse/glint/parallax easing; keyboard focus visible; responsive to ~380px.

### Known build problems (not solved in mock)
- Collision spacing when a constellation exceeds ~15 stars → simple force-relaxation pass.
- One signal matching multiple topics → place star between constellations; render in both topic groups in list view.
- Canvas perf at 100+ stars with gradient spikes → measure; if needed, pre-render star sprites to offscreen canvas (`drawImage`), WebGL only as last resort.

> **rev 4:** measured and closed — see §11. Sprites were built and do help, but the star count was not what governed the frame rate; the dust batching was, and under it a fixed full-screen repaint cost. WebGL was never needed.

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

## 11. Canvas performance — what measurement found (rev 4)

Closes the third bullet of §6 "Known build problems". That bullet named the right
remedy for the wrong cause, and the correction is the useful part.

**The hypothesis was the stars.** "Canvas perf at 100+ stars with gradient spikes
→ pre-render star sprites." The sprite work was done and it does help. But
measured in a real browser at 117 stars, the star count turned out not to govern
the frame rate at all: it was identical whether 117 stars drew or 38 did at
maximum zoom.

**The cost was the dust.** Ablating one piece of the frame at a time put the
entire gap in the 200 parallax dust particles — 21fps with them, 59 without —
and the cost was the *batching*, not the dots. Bucketing the twinkled alpha and
collecting each bucket into one path for a single `fill()` looks like the
star-sprite optimization carried one step further. It is the opposite: the dots
in a bucket are scattered across the whole sky, so one path per bucket means ~18
fills a frame whose bounding boxes each span the entire canvas, and the
rasterizer sets up over the box rather than over the ink. Same colours, same
style-change count, one `fill()` per dot: 21fps → 55.

Batching draw calls is normally right. Batching *geometry this sparse* into one
path inverts it. That is the transferable lesson, and it applies to anything else
here tempted to merge scattered small shapes into one path.

**Underneath that is a floor no drawing change can move.** Every frame overwrites
the whole backing store — 4.4 megapixels at dpr 2 on a 1440×813 viewport — and
that alone caps out around 20–27fps on a loaded machine. Proved two ways:
replacing the background image with a 2×2 one scaled to full screen costs the
same, and a frame that draws nothing runs at 60. So the remaining lever is not
what is drawn but *whether* to draw: decision #16. With motion off and the view
settled the sky repaints nothing, which takes it from 17.7fps to a pinned 60.

**Two measurement traps**, recorded because both produced confident wrong answers
first and anyone re-measuring this will hit them:

- Timing the draw calls measures only JS record time. Canvas rasterization is
  deferred, so the call returns long before the work happens.
- Forcing a flush with `getImageData` charges a readback whose cost tracks the
  dirtied area, not the work done — which made full-screen blits look like 40ms
  and the dust like 0.3ms.

Only end-to-end frame-rate ablation, interleaved across conditions so machine
drift cancels, is trustworthy here. Absolute numbers move a lot with machine
load; the *ratios* between interleaved conditions hold.

**Idle skipping is the part most able to go wrong silently.** Skipping a frame
that was needed leaves a stale image with nothing on screen to explain it, so
invalidation is explicit and every wake path is verified by counting draw calls
rather than by trusting a flag: view movement, hover changing star, layer toggle,
filter change, relayout, resumed loop, and any change to the motion gate
(including the system preference flipping mid-session). The view ease also had to
learn to snap — it is asymptotic, so the view never becomes exactly the target
and the sky would otherwise repaint forever over thousandths of a pixel.

**The constellation join is a spanning tree now (decision 18).** §6 asks for
"atlas-style links", and an atlas joins neighbours. The join was a polyline
through the topic's stars in array order — the API's score order — so it linked
stars that are adjacent in score and far apart on screen. It was only tolerable
because it drew at 0.4 alpha, which concealed it rather than fixing it. A minimum
spanning tree is the shortest set of edges that still connects every star, and it
branches, which is what makes the figure read as a constellation. Computed once
per rebuild alongside `relax`, never per frame.

**Shared-star ties are gone (decision 19), and this is the second time they were
cut back — read this before drawing them a third time.**

They were the tangle the joins had been hiding. A shared star is anchored beside
its *primary* topic but tied to every topic it matched, so each line could cross
the sky, and 49 at once stated every membership simultaneously while
communicating none of them. Narrowing them to the hovered star fixed the tangle
exactly as intended — 0 lines at rest, 2 while pointing at a two-topic star — and
the owner's verdict on the result was that it still showed nothing worth having.

The lesson is worth keeping, because §6's second known build problem ("one signal
matching multiple topics → place star between constellations") reads like an
instruction to draw a line, and it is not. That requirement is met by
**placement**: the star sits at the centroid of the topics it belongs to, capped
so it stays anchored to its primary. Membership is then *stated*, in three places
that all still do it — the tooltip names every topic, the detail panel lists
them, and list view renders the signal under each. A line was never the thing
carrying that information; it only added geometry on top of it.

What remains of the mechanism is the per-star `shared` flag, still read for one
thing: a shared star must not lift a constellation's label, or the two topics'
names drift toward each other until they overprint.

**The band is a photograph now (decision 17).** It is composited with `lighter`
rather than painted over: the image is light on black, so additive blending lets
the band and its grain add to the gradient underneath while the empty sky between
the stars contributes nothing. Painting it source-over instead put the
photograph's black *over* the gradient and flattened the depth — the picture sat
on the sky rather than being part of it. For the same reason the procedural
nebula is skipped when the photograph is in use: a broad blue lift underneath
destroys the contrast that makes a real dust lane read as one. Verified by
sampling the baked layer, not by eye — an empty corner measures the same with the
photograph on or off, while the dust lane lifts and warms.

`drawMilkyWay` is kept, not deleted. It is what renders before the image decodes
and what renders if it never does, which is what keeps the offline guarantee from
resting on a single asset.

**§6's typography contract binds icons too.** The zoom controls were text
characters, and U+2302 (⌂) is not in the bundled Noto Sans subset — it was
rendering in a system font, and would have been tofu on a machine without one.
"Font files BUNDLED, no hotlinking, renders offline" is not satisfied by a glyph
that depends on the host. UI symbols are inline SVG.

## Phase 1 backlog

> **Partly superseded — see rev 7, decision #24.** The email digest below is **Phase 2**, per the roadmap table in the next section. This paragraph carries no gates and no ordering; the table does, and it is the statement to follow where the two disagree. Everything else in this list stands as written.

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
