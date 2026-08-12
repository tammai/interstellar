# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Status

Phase 0 is built and every §10 acceptance criterion passes. What happens next is decided by real use, not by a backlog — see "Phase discipline" below.

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
- `specs/` is history. Amend it by adding a revision, not by silently rewriting a decision.
- **Distribution is local `docker build`, decided 2026-08-11.** No image is published and none should be: an instance is one person's container built from this repo. The plan carried "publish to GHCR" as an open item; it is now closed as *won't do*, so don't reopen it as tidying. `make docker-multiarch` still verifies amd64 and arm64 both build.
- **Never seed a source URL you have not run through the real fetcher.** `make feedcheck URLS="…"` does it with the app's own parser. A feed can return 200 in a browser and yield zero items; worse, a YouTube channel ID scraped from page HTML will happily serve *someone else's* channel — a `"channelId":"UC…"` regex matches the recommended-channels sidebar, which is how Karpathy's row once got 3Blue1Brown's ID. Resolve channels from `rel="canonical"` and confirm against the title the feed itself returns.
- **The canvas draws from `sky.shown`, never `sky.stars`.** `shown` is the filtered subset; rendering, hit-testing and constellation lines must all read it or they disagree with each other — a filtered-out star that stays clickable is the failure mode.
- **Star rendering bakes its gradients and draws its core.** `drawStar` builds ~6 gradients per call (0.22ms/star, flat with scale); `spriteFor` bakes layers once and blits them (~0.05ms/star, improving with scale as sprites are shared). Layers split on two axes and both matter: by **motion**, because a sprite can only carry a rigid transform (`pulsing` scales with the hot pulse, `glint` also rotates, `glow` does neither, so a cold star's four soft layers collapse into one `body` sprite); and by **required sharpness**, because blitting at a fractional scale resamples — the hard core is never baked, since softening it visibly breaks §6's "pinprick that makes it read as a star". Only the hovered star takes the full direct path. Measured 2–4.5× faster, growing with star count; this is what stops the sky slowing down as the day fills it.
- Filter thresholds derive from the encoding functions (`starRadius`, `starAlpha`), not from raw scores. The classifier returns 1–5 and the match threshold clips it at 4, so a plausible-looking `score >= 6` matches nothing at all.

## Phase discipline

Spec §"Roadmap beyond Phase 0" defines later phases as **gates, not dates** — each opens only when a specific failure mode shows up in real use. Do not build Phase 1 items (email digest, YouTube transcripts, fuzzy dedup, Ollama shadow mode, trending over time, search/archive) into Phase 0 because they'd be easy to add now. The Phase 0 non-goals list in §2 is short and deliberate.
