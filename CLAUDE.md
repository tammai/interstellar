# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Status

Early implementation, building milestone by milestone against the approved plan. Verify what actually exists before assuming a package is there.

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
- Schema changes go through the embedded migration path, never hand-edited SQL against a live DB. First run must bootstrap an empty volume with seed sources and placeholder topics.
- `specs/` is history. Amend it by adding a revision, not by silently rewriting a decision.

## Phase discipline

Spec §"Roadmap beyond Phase 0" defines later phases as **gates, not dates** — each opens only when a specific failure mode shows up in real use. Do not build Phase 1 items (email digest, YouTube transcripts, fuzzy dedup, Ollama shadow mode, trending over time, search/archive) into Phase 0 because they'd be easy to add now. The Phase 0 non-goals list in §2 is short and deliberate.
