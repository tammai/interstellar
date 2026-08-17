# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Status

Phase 0 is built and every §10 acceptance criterion passes. What happens next is decided by real use, not by a backlog — see "Phase discipline" below.

**Phase 1 opened 2026-08-12 by owner decision, ahead of its gate.** The spec gates Phase 1 on "we open it daily but complain about content"; the owner started it before that signal existed, on the argument that the diagnostic should come first. Done: the **per-topic hit-rate view** (`topic_stats`, `GET /api/topics/stats`, Settings → Signal quality). Still untouched and still gated: YouTube transcripts, fuzzy dedup, manual-forward inbox, source list import/export.

**The Ask screen landed 2026-08-13, also by owner decision, and it is a Phase 3 item.** Spec §"Roadmap" gates "ask-the-sky Q&A" and "semantic search/archive" on 2–3 months of accumulated signals; both were built on day one instead. What that means in practice: retrieval quality is untested against real volume. FTS5 keyword matching was chosen over embeddings deliberately — see the search conventions below — and the decision to revisit is *whether recall is actually the problem*, which needs real questions against real data to answer. Do not add an embeddings provider before that evidence exists; §3.1's "one transport, forever" is the constraint it would break.

Note for whoever picks this up: the spec is internally inconsistent about the **email digest**. The `## Phase 1 backlog` paragraph lists it, but the roadmap table puts it in Phase 2 and says that if its gate trips it "outranks all of Phase 1". The table is the more considered statement. Settling this needs a spec revision, not an edit.

## Working on this

```sh
make dev      # run at :8422 against ./.devdata, UI served from disk
make test     # offline suite; never touches a network or a provider
make test-ui  # the star map's pure layout logic (node)
make lint     # vet + gofmt
```

Two conventions worth knowing before you write code here:

- **Tests never make live calls.** `make test` must stay runnable with a provider key exported in the shell and cost nothing. Test helpers clear provider env vars explicitly; a test that resolved a real provider once did bill a real account. The single exception is `cmd/verify-classifier`, which is opt-in and says so.
- **`go:embed` bakes the UI into the binary.** `make dev` sets `INTERSTELLAR_WEB_DIR` to serve `web/assets` from disk instead. Without it, a CSS or JS edit does nothing until you rebuild, and the resulting "my change had no effect" is a genuinely confusing hour.

## Authoritative documents

Read these before making design decisions. They are contracts, not suggestions:

- `specs/interstellar-phase0-spec.md` — Phase 0 design spec, revision 3. Section 0 is a decision log; several rev-2 decisions were **reversed** in rev 3 (n8n → single container, Postgres → SQLite, LiteLLM → embedded client, email → web UI). Do not resurrect a rejected option without an explicit new decision. §3.1 was rewritten later in rev 3 from a single configured endpoint into a **provider registry** — if you find a copy of this spec describing `LLM_BASE_URL` and a single `LLM_API_KEY`, it is stale.
- `specs/interstellar-starmap-prototype.html` — the approved UI. Spec §6 says "Contract, not suggestion." It is a self-contained mock with hardcoded data; the real UI keeps its visual language and interaction model and swaps mock arrays for API calls.

## What this is

A per-user AI news intelligence tool. One Docker container per person, `docker run -p 8422:8422 -v interstellar-data:/data`. A scheduler ingests feeds each morning, an LLM classifies items against the owner's topics, a second LLM pass clusters matched items into **signals** (cross-source stories), and a canvas star map renders today's signals as a sky. The yardstick is habit: "do we each actually open it every day?"

## Architecture

Single Go binary, no sidecars. Everything ships inside it:

```
scheduler (in-process cron, honors TZ)
  09:30 ingest ──▶ SQLite /data/interstellar.db
  10:30 classify + compose signals
HTTP server :8422
  /        embedded UI (go:embed)
  /api/*   sky data, signal detail, settings, POST /api/refresh
LLM client ──▶ provider registry ──▶ OpenAI-compatible /chat/completions
```

Locked stack choices (spec §3, with reasons — don't relitigate):

- **Go, single binary, `go:embed` for UI and fonts.** Rejected: Node/TS (heavier image), Python (packaging friction).
- **Pure-Go SQLite driver, `CGO_ENABLED=0`.** The whole point is trivial multi-arch (amd64 + arm64). A cgo driver breaks that.
- **SQLite, one file in `/data`.** Single user, single writer; backup is `cp`. Not Postgres.
- **Embedded LLM client with a provider registry, not a proxy.** See below — this is the part most likely to be misremembered.
- **Web UI is the front door.** No email, no notifications, no auth in Phase 0.

### The LLM layer is a registry, and one transport

§3.1 is a hard requirement, not a preference: *either role can run on any model from any provider, switched via settings without touching code or restarting.*

- **Providers** are named registry entries `{base_url, api_key_env}` stored as JSON in `settings`. Presets ship for deepseek, anthropic, google, openrouter, litellm, and ollama (keyless), plus anything the owner adds. An entry with an empty `api_key_env` sends no `Authorization` header.
- **Keys are referenced by env var name, never stored.** The registry holds the *name*; the value is read from the environment at call time. No endpoint ever returns a key value — `/api/settings` returns the name plus a resolved/unresolved boolean.
- **Roles are `provider/model` refs.** `classifier = deepseek/deepseek-chat`, `synthesizer = anthropic/<sonnet>`. Roles mix providers freely.
- **Fallback chains are cross-provider.** A retry can land on an entirely different vendor, so base URL and auth are rebuilt per hop, and every attempt gets its own `llm_calls` row — a fallback that fires must be visible in the cost log, not silent.
- **One transport, forever.** OpenAI-compatible chat completions is the only wire format. §3.1 puts native Bedrock/Vertex/Foundry auth explicitly out of scope: *"never native cloud SDKs in-app."* If corporate billing ever matters, that's a registry entry pointing at a LiteLLM gateway. Do not add a vendor SDK dependency to solve a provider problem — the registry is the extension point.
- **No restart, ever.** Effective config is resolved per run, not cached at boot. Nothing may hold a settings snapshot across runs, and a settings change touching the schedule rebuilds the cron entries in place.

Phase 0 seeds both roles to `deepseek/deepseek-chat` (cheap-first, decided 2026-08-11, ~$3/month/instance). When quality forces a change, §3.1's escalation ladder is the runbook — classifier fails the JSON gate → google flash tier → anthropic haiku; digest reads poorly → anthropic sonnet. Each step is a settings change, not a code change.

### The unit of the product is a *signal*, not an item

An `item` is one URL from one feed. A `signal` is one cross-source story for one topic, composed by the synthesizer LLM, and it is what the sky renders — one star per signal. `source_count` drives the gold "hot" flag (≥3) and the glow; `score` drives size; age drives opacity. When you touch the pipeline or the API, think in signals.

Deduplication happens at two levels and they are not the same mechanism: URL canonicalization + `INSERT OR IGNORE` at ingest time (exact same link from two feeds), and LLM clustering at synthesis time (different links telling the same story). Spec §4 calls this out explicitly.

### Pipeline stages

`ingest → classify → compose signals`, each idempotent and separately restartable. Per-source try/continue: a dead feed must never abort the run (`last_run_at` updates on success only). Classification is **per topic**, batched ~30 items — deliberately not one 7-topic mega-prompt, because per-topic calls give sharper verdicts and free per-topic hit-rate stats.

## Non-negotiable behaviors

These come straight from spec §10 (acceptance criteria) and §6 (UI contract). Breaking one is a bug even if tests pass:

- A dead feed does not break ingest; re-ingesting creates zero duplicates.
- Reassigning a role to a different provider in Settings takes effect on the **next run, with no restart**, and the per-role "test call" button verifies it in-UI.
- Classifier JSON parses 100% on a 200-item sample. This gates any model swap. Assume the model returns fenced or prose-wrapped JSON sometimes and extract tolerantly.
- The LLM call cap aborts a runaway run and surfaces a banner in the UI — the cap is the replacement for LiteLLM's budget guard, so it must actually stop the run, not just log.
- Kill the container mid-run, restart, next run is clean and the DB is intact.
- `prefers-reduced-motion` disables twinkle, pulse, glint, and parallax easing. Keyboard focus stays visible. Layout works to ~380px.
- Empty topics render "No signal today" rather than disappearing.
- Fonts are **bundled** (Noto Sans, Playfair Display — both OFL). No hotlinking; the UI renders offline. The prototype's Google Fonts `<link>` must not survive into the real UI.
- No 3D rotation of the star map. Rejected with cause: there is no third data dimension and it breaks spatial memory.

## Conventions

- Provider API keys come from env only, referenced by the name the registry stores (`DEEPSEEK_API_KEY`, `ANTHROPIC_API_KEY`, …). Never write a key value to the DB, never return one from an endpoint, never log one.
- Config lives in the `settings` table and is resolved per run. `PORT` and `TZ` are the only process-level env knobs besides the provider keys.
- Every LLM call attempt gets a row in `llm_calls` (run date, role, **provider**, model, token counts). Cost visibility is a feature, and it feeds the Phase 3 habit analysis.
- Schema changes go through the embedded migration path, never hand-edited SQL against a live DB. Migrations are append-only: never edit one that has shipped, add the next number.
- **The connection pool is capped at one.** Never issue a query while a `*sql.Rows` is still open — the open rows hold the only connection, so the inner call waits on a connection the outer loop won't release. It hangs rather than erroring. Drain into a slice, close, then do the follow-up work; `store.SignalByID` is the worked example.
- **Per-topic stats and the classify watermark commit together, in one transaction** (`store.CommitClassifyPass`). They are not independent writes. Stats written without the watermark count the same items again on the next pass, inflating every denominator and understating every hit rate; the watermark without the stats loses a pass that can never be recomputed, because `matches` keeps score ≥ 4 verdicts only and the denominator exists nowhere else. For the same reason an **aborted** pass — cap breached, context cancelled — records neither, including for the batches that did succeed. `ItemsConsidered` counts only batches the model answered: a failed batch is not a denominator, or a provider's bad afternoon reads as a badly defined topic.
- **Search is FTS5 inside the pure-Go driver, and user input never reaches `MATCH`.** `modernc.org/sqlite` ships FTS5 with `bm25()` and `snippet()`, so full-text cost no new dependency and `CGO_ENABLED=0` is intact. `store.BuildMatch` is the only way prose becomes a query: it quotes every token and OR-joins them, because FTS5 treats `AND OR NOT NEAR " * : ^ - ( )` as operators and an ordinary question like `what's happening with agents?` is otherwise a syntax error — or worse, silently parses as something else. It returns `""` for unsearchable input, which callers must treat as **filter-only, never match-everything**. The indexes are `content=`-external, so **the triggers in migration 003 are load-bearing**: `ReplaceSignals` deletes and re-inserts on every compose, so a missing delete trigger means yesterday's removed stars keep coming back as hits.
- **The chat planner emits a typed `AskQuery`, never SQL,** and `Asker.sanitizePlan` is the whitelist every field passes through. A hallucinated topic id is the dangerous case: left in place it filters everything out and produces a confident "nothing happened", so unknown ids are dropped rather than trusted. Text-to-SQL is rejected here on *correctness* grounds before security ones — wrong SQL returns a plausible answer to a different question, with nothing to notice it by.
- **Chat is budgeted separately and is not part of `Ready()`.** `RoleChat` has its own `max_chat_calls_per_day` and is exempt from the per-run cap, because a question is not a run. Critically, `Config.Readiness()` still covers only classifier and synthesizer: `run.go` refuses to start when `Ready()` is false, so folding chat in would mean an owner who cleared `chat_model` silently broke their morning ingest. Use `RoleReadiness(RoleChat)` for the chat screen instead. A test pins this.
- **The Ask screen loads none of the star map.** `web/assets/chat/` is a separate document sharing only `tokens.css`. It creates no canvas and never calls `requestAnimationFrame` — verified in-browser, not assumed. Keep it that way: the reason it exists is that `sky.js` is 56KB and a frame loop, and answering a question needs neither. Palette and `@font-face` live in `tokens.css` precisely so two screens cannot drift into two palettes.
- **`chat.css` carries a blanket `[hidden] { display: none !important }`.** Same trap as `#sheet` and `.field` before it: an author `display` beats the UA `[hidden]` rule, and `chat.js` hides `.sources` (which is `display: flex`) by class rather than by id — so the id-based guard in `web/css.test.js` cannot see it. The blanket rule covers whatever gets added to that page next; a test pins it.
- **Deduplication now has three levels, not two.** Spec §4 names URL canonicalization at ingest and LLM clustering at synthesis; `sources.SameArticle` sits between them and catches the same article at a different URL (a syndicated republish, an AMP variant, changed permalinks). A fuzzy match becomes an **attribution, never a discard** — two outlets carrying one story is exactly what `source_count` measures and what makes a star gold. It is deliberately conservative, and the reason is asymmetric cost: a miss leaves one extra row the synthesizer usually clusters anyway, while a false merge destroys a story before anything judged it, with no trace in the sky. Similarity alone was not enough — token-overlap scored `GPT-5` vs `GPT-5.1` at 0.94 and `is fair use` vs `is not fair use` at 0.95 — so numbers and negations are hard disqualifiers on top of the 0.92 threshold. `TestKnownMissSingleSubstitutedPreposition` pins the trade-off; read it before retuning any of those numbers.
- **`sources.kind` cannot gain a new value without a table rebuild.** Migration 001 pins `CHECK (kind IN ('rss','hn_search','youtube_rss'))`, SQLite cannot widen a CHECK via ALTER TABLE, and the documented rebuild needs `DROP TABLE sources` — which fails while `items.source_id` references it, with no way to disable foreign keys from inside a migration (each runs in a transaction, where the pragma is a no-op). The paste-a-link inbox works around this as an ordinary `rss` row carrying `{"manual":true}` and created **paused**; `Ingest` skips it explicitly. Anything else needing a new kind faces the same wall.
- **Source import is additive and verifies by default.** It never deletes what a file omits, because an export goes stale as soon as either side edits and losing the owner's pruning is not a recoverable trade. New feeds are fetched through the real fetcher first, per the seeding convention above, and anything that fails lands paused-with-a-reason rather than dropped.
- **Trending normalizes titles in SQL, and the double-space collapse is load-bearing.** `RecurringStories` groups on a normalized title because the synthesizer rewords its titles every run. Turning `:` and `-` into spaces leaves two spaces where there was one, so without the two `replace(…,'  ',' ')` passes the reworded copies do not group and the function silently finds nothing — which is exactly how it failed the first time.
- **Transcripts write into `items.summary`, which is why the stage runs before classify.** The classifier reads title and summary and nothing else, so a summary landing after classification makes every video a run late — indistinguishable from the feature not working. `item_transcripts.summary_base` holds the item's original description so applying a summary is idempotent: rebuilt from the base, never prepended to whatever is already there. Failure rows are written deliberately — without them a video with captions disabled is refetched every run forever — and `none` (permanent) is stored differently from `error` (retryable).
- **The weekly round-up defaults to written, not sent.** `schedule_weekly_day` seeds to `mon`, so a fresh instance has **three** cron entries; delivery is what decision #10 turned down, not composition. An empty day switches composition off — and note `config.str()` cannot express that, because it substitutes the default for an empty value, so `ScheduleWeeklyDay` is read raw from the settings map. Defaults for that key live in bootstrap's seed, not in the read.
- **A bad weekly schedule must never take the daily one down.** The daily sky is the product; `Scheduler.Reload` logs and skips a malformed weekly spec rather than returning an error, and a test pins that the two daily entries survive.
- **SMTP credentials follow the provider-key contract exactly**: the settings row holds an env var *name*, `/api/settings` returns the name plus a resolved boolean, and no endpoint ever returns the value. Defaults target Resend's relay (`smtp.resend.com:587`, username the literal `resend`, password `RESEND_API_KEY`). If STARTTLS is advertised and then fails, the send fails — never a plaintext fallback.
- **A UI action must not re-render the sheet it just reported into.** `sheetFrame` calls `replaceChildren()`, so `rerenderCurrent()` destroys the confirmation message that triggered it. This has now bitten the Model screen and the weekly screen; update the affected element in place instead.
- **The classify watermark may only pass items EVERY ready topic judged.** It is a single high-water mark, so it advances over an unbroken *prefix* of item ranges: if one topic's batch failed on the first slice, nothing behind it can be claimed either. Advancing regardless left a permanent hole in that topic's coverage, visible only as a topic that mysteriously matched nothing. The cost is that a persistently failing topic stalls the watermark and re-classifies the same items each run — which is the right way round, because it is bounded by the per-run cap and loud (a problem every run, a zero in the hit-rate view), whereas silent loss is neither. **Per-topic stats deliberately do NOT follow that prefix**: gating them the same way made one failing topic blank the hit-rate view for every topic, destroying the diagnostic that would identify it. Re-classifying double-counts numerator and denominator together, so the rate is unchanged.
- **Every setting that changes when the scheduler fires belongs in `scheduleKeys`.** The reload used to be a two-key if/else, so adding the weekly round-up meant changing its day silently required a restart — while the screen that changed it said "the schedule was rebuilt". A test pins the weekly case.
- **Never truncate a string with `s[:n]`. Use `sources.Truncate`.** On Vietnamese text -- which §9 keeps topic definitions in, and which Vietnamese feeds carry -- 27% of byte offsets fall inside a multi-byte rune (measured, not estimated). `json.Marshal` does not error on invalid UTF-8; it substitutes U+FFFD, so the damage arrives as a mojibake character in the database and on screen rather than as a failure anyone would notice. Nine sites had this before it was found, across four hand-rolled copies of the same rune walk; there is now one implementation, and it delegates to `strings.ToValidUTF8`.
- `specs/` is history. Amend it by adding a revision, not by silently rewriting a decision.
- **Distribution is local `docker build`, decided 2026-08-11.** No image is published and none should be: an instance is one person's container built from this repo. The plan carried "publish to GHCR" as an open item; it is now closed as *won't do*, so don't reopen it as tidying. `make docker-multiarch` still verifies amd64 and arm64 both build.
- **Never seed a source URL you have not run through the real fetcher.** `make feedcheck URLS="…"` does it with the app's own parser. A feed can return 200 in a browser and yield zero items; worse, a YouTube channel ID scraped from page HTML will happily serve *someone else's* channel — a `"channelId":"UC…"` regex matches the recommended-channels sidebar, which is how Karpathy's row once got 3Blue1Brown's ID. Resolve channels from `rel="canonical"` and confirm against the title the feed itself returns.
- **The canvas draws from `sky.shown`, never `sky.stars`.** `shown` is the filtered subset; rendering, hit-testing and constellation lines must all read it or they disagree with each other — a filtered-out star that stays clickable is the failure mode.
- **Star rendering bakes its gradients and draws its core.** `drawStar` builds ~6 gradients per call (0.22ms/star, flat with scale); `spriteFor` bakes layers once and blits them (~0.05ms/star, improving with scale as sprites are shared). Layers split on two axes and both matter: by **motion**, because a sprite can only carry a rigid transform (`pulsing` scales with the hot pulse, `glint` also rotates, `glow` does neither, so a cold star's four soft layers collapse into one `body` sprite); and by **required sharpness**, because blitting at a fractional scale resamples — the hard core is never baked, since softening it visibly breaks §6's "pinprick that makes it read as a star". Only the hovered star takes the full direct path. Measured 2–4.5× faster, growing with star count; this is what stops the sky slowing down as the day fills it.
- **Never merge scattered small shapes into one path.** `drawDust` bucketed 200 particles by alpha and issued one `fill()` per bucket — the same instinct as the star sprites, and it cost two thirds of the frame rate (21fps against 55). The dots in a bucket are spread across the whole sky, so one path per bucket meant ~18 fills a frame whose bounding boxes each spanned the entire canvas, and the rasterizer sets up over the box, not the ink. Batching draw calls is usually right; batching geometry this sparse inverts it. Spec §11 has the numbers.
- **Measuring canvas cost: only end-to-end frame rate is trustworthy.** Timing the draw calls measures JS record time, because rasterization is deferred. Forcing a flush with `getImageData` charges a readback whose cost tracks the dirtied area, not the work — that one made full-screen blits look like 40ms and dust like 0.3ms. Interleave conditions across rounds so machine drift cancels, and compare ratios, not absolutes: the same code measured 55fps and 16fps on this machine hours apart, purely on load.
- **The sky skips frames, so anything that changes the picture must set `dirty`.** With motion off and the view settled every frame is identical, and repainting 4.4M device pixels to reproduce it is the single largest cost in the renderer — the floor no layer toggle can move. `frame()` returns early when `still && !moved && !dirty`. The failure mode is silent and one-directional: a missed `dirty` leaves a stale image with nothing to explain it, while a spurious one only costs a frame. Set it in anything touching layout, filters, layer visibility, hover identity, or the motion gate. `easeView` must also snap at its threshold — the ease is asymptotic, so without it the sky never settles and never skips.
- **Sky layer visibility is view state, not config.** `SKY_LAYERS` lives in the browser via `loadShow`/`saveShow`, never the `settings` table: nothing about it reaches a run, and the settings table's "applies on the next run" contract is wrong for something that applies on the next frame. Labels and stars are deliberately not toggleable — the label is the only place §10's "No signal today" is rendered, and the stars are the product. The motion toggle ORs with `prefers-reduced-motion` so it can turn motion off but never on.
- **The constellation join is a spanning tree, and the `lines` layer defaults off.** `constellationEdges` runs Prim's over a topic's stars in `buildConstellations` — once per rebuild, never per frame, like everything else in there. It replaced a polyline in array order, which is the API's *score* order: on a 45-star topic that drew 44 segments zigzagging across itself, joining stars adjacent in score and far apart on screen. Edges hold references to the caller's star objects, not copies, so they track relayout. **Shared-star ties are a separate mechanism and are drawn for the hovered star only** — a shared star sits beside its primary topic but ties to every topic it matched, so all 49 at once crossed the sky and hid the joins. Both are gated on the `lines` layer; a hover change already sets `dirty`, which is what makes a tie appear while the loop is idle.
- **`web/assets/img/milky-way.webp` is AI-generated and personal, non-commercial use only.** That turns the "no published image" decision from a preference into a licence condition — see `web/assets/img/README.md`, which ships inside the binary with it. `drawMilkyWay` is the fallback, so removing the file degrades the background rather than breaking the build.
- **The Milky Way is a bundled photograph, composited with `lighter`.** `web/assets/img/milky-way.webp` is baked into `bgLayer` once per layout, so it costs a decode at startup and nothing per frame — and 3ms to bake against the hand-drawn band's 7.2ms. Additive is load-bearing: the image is light on black, so `lighter` adds the band and its grain while the empty sky between the stars contributes nothing; source-over paints that black over the gradient and flattens the depth. The procedural nebula is skipped when the photo is in use for the same reason. `drawMilkyWay` is the fallback, not dead code — it renders before the image decodes and if it never does, which is what stops §6's offline guarantee resting on one asset. `loadBand` must set `dirty`, since the frame loop may be idle when the image lands.
- **UI symbols are inline SVG, not text characters.** The bundled Noto Sans subset does not cover them: `⌂` (U+2302) was rendering in a system fallback font, which is why it needed a hand-written `font-size` and would have been tofu on a machine without one. §6 requires the UI to render offline from bundled fonts. A button holding an SVG also needs explicit flex centring — a `<button>` centres a text character on its own, but an inline `<svg>` sits on the text baseline.
- Filter thresholds derive from the encoding functions (`starRadius`, `starAlpha`), not from raw scores. The classifier returns 1–5 and the match threshold clips it at 4, so a plausible-looking `score >= 6` matches nothing at all.

## Phase discipline

Spec §"Roadmap beyond Phase 0" defines later phases as **gates, not dates** — each opens only when a specific failure mode shows up in real use. Do not build Phase 1 items (email digest, YouTube transcripts, fuzzy dedup, Ollama shadow mode, trending over time, search/archive) into Phase 0 because they'd be easy to add now. The Phase 0 non-goals list in §2 is short and deliberate.
