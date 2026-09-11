# Getting started

Zero to a sky full of stars, in about twenty minutes, with a Claude Code session
doing the typing.

This is the narrative version. The [README](../README.md) is the reference —
every knob, every endpoint, every trade-off. Come here first, go there when you
want depth.

---

## What you are setting up

One Docker container, for one person. It wakes up twice each morning: at **09:30**
it fetches your feeds, at **10:30** it asks a model which items matter to *your*
topics and groups the survivors into **stories**. A canvas star map draws today's
stories as a night sky.

The yardstick is habit. If you do not open it daily, it is not working, and no
amount of feature is going to fix that.

---

## Before you start

| You need | Why | Cost |
|---|---|---|
| Docker | The whole app is one container | — |
| An **OpenRouter** key | Both model roles route through it | a few dollars a month |
| A **YouTube Data API** key *(optional)* | Video sources; skip it and they simply stay quiet | free |

One OpenRouter key reaches every model the app can use, which is the point: moving
a role from a cheap model to an expensive one is then a settings change, not a new
environment variable and a restart.

Put them where your shell will export them — `~/.zshrc` or equivalent:

```sh
export OPENROUTER_API_KEY="sk-or-..."
export YOUTUBE_API_KEY="AIza..."        # optional
```

Open a new terminal afterwards, or they will not actually be exported.

> **Keys are never stored.** The database holds the *name* of the variable; the
> value is read from your environment at call time. No endpoint ever returns one.

---

## 1. Build and run

```sh
git clone https://github.com/tammai/interstellar.git
cd interstellar
make docker

docker run -d --name interstellar \
  -p 8422:8422 \
  -v interstellar-data:/data \
  -e TZ=Asia/Bangkok \
  -e OPENROUTER_API_KEY \
  -e YOUTUBE_API_KEY \
  interstellar:latest
```

Passing `-e NAME` without a value tells Docker to take it from your current shell,
so no key is ever typed into a command or a file.

Check it came up:

```sh
curl -s localhost:8422/api/health
# {"status":"ok","schema_version":8,"uptime_seconds":3,"timezone":"Asia/Bangkok"}
```

Then open <http://localhost:8422>. It will say **"Nothing yet today"**, which is
correct — nothing has run.

> **`TZ` matters.** The scheduler honours it. Set it wrong and your morning run
> happens in the middle of someone else's morning.

---

## 2. Tell it what you care about

Open **Settings → Topics** (the gear, top right).

A topic is a name and a **definition** — a paragraph in your own words describing
what you want to see. The definition is a private note to the classifier, not a
label; it can be in any language, and the stories it produces are always written
back in English.

Write them the way you would brief a colleague:

> **Open-weight & On-prem** — Releases of open-weight models, licence changes,
> and anything about running models on your own hardware. Not API pricing news,
> not closed-model benchmarks.

Saying what you *don't* want is as useful as saying what you do. Five to eight
topics is a comfortable range.

---

## 3. Give it somewhere to read

**Settings → Sources.** Three kinds:

- **RSS** — one row per publication. Prefer a section feed (`/category/ai/feed/`)
  over a whole-site one, or you will drown.
- **Hacker News** — exactly **one** row carrying many keywords. Never one row per
  keyword: source counts are what make a story "hot", and splitting HN across rows
  makes one HN post look like three independent outlets.
- **YouTube** — one row per channel.

Short on ideas? Open a topic and use **Suggest sources**. It asks a model for real
publications, then *probes every URL through the real fetcher* before offering it,
and anything it accepts arrives **paused** for you to review. A feed that returns
200 in a browser can still yield zero items, which is exactly what the probe
catches.

---

## 4. Don't wait until tomorrow

Press **Refresh** on the sky, or:

```sh
curl -X POST localhost:8422/api/refresh
```

It runs the whole pipeline now. A first run over a fresh source list takes a few
minutes and costs a few dozen model calls.

Then read the sky:

| What you see | What it means |
|---|---|
| One star | One story, for one topic |
| **Gold star** | Three or more distinct sources carried it |
| Bigger star | Higher relevance score |
| Fainter star | Older |
| Constellation | All the stories under one topic |

The header counts **stories**, and that number is deliberately smaller than the
signal count — a story matching two topics is drawn as one shared star.

Click a star for the reasoning and every source that carried it. Press the dock at
the bottom to **Ask** a question against your archive.

---

## 5. Driving it from Claude Code

This is where the container stops being something you administer and starts being
something you *ask about*. Open a Claude Code session in the repo and try:

**Check on a run**

> Did this morning's run work? Show me sources ok, new items, and any failures.

**Find out whether a model is any good**

> Read /api/models/stats for the last 7 days and tell me the parse and
> completeness rates per model.

Completeness is the one that catches the expensive failure: a model that returns
valid JSON, costs normal tokens, and quietly answers 12 of 30 items looks healthy
everywhere except here.

**Change a model and prove it works**

> Switch the synthesizer to openrouter/openai/gpt-5-nano and run the per-role
> test call.

Settings changes take effect on the **next run, with no restart**. The test call
exercises the real chain and reports provider, latency and tokens.

**Gate a model before trusting it**

```sh
make verify-classifier        # 200 real items, real money, opt-in on purpose
```

Ask Claude Code to run it and read the result. It must parse **100%**. Note it
reads whichever model your `DATA_DIR` is configured with, so gate the config you
actually run.

**Diagnose a thin sky**

> The hit rate is 8%. Is that the topic definitions or the classifier?

`Settings → Signal quality` answers this and nothing else can: the per-topic hit
rate says which topics are matching, and the model reliability table underneath
says whether the model was even answering. Same symptom, opposite causes.

---

## The rhythm, once it is running

| When | What |
|---|---|
| 09:30 | Ingest — fetch every source |
| 10:30 | Classify, then compose stories |
| Monday 11:00 | Weekly round-up, written (delivery is off unless you turn it on) |

A dead feed never breaks a run; each source is tried and skipped independently.

---

## Things that will confuse you once

**A stopped container leaves a permanent hole.** Each run composes *its own day's*
stories. Miss a day and that day has no stars, forever — the articles are still
ingested and classified later, but the sky for that date stays empty.

**Adding a new environment variable needs a restart.** The environment is fixed
when the process starts. Everything in Settings changes live; only new variables
need `docker run` again.

**A new key needs its API enabled.** A YouTube key from a project where the
YouTube Data API was never enabled looks perfectly valid and returns `403`. Enable
the API, not just the key.

**Editing CSS during development does nothing unless you use `make dev`.** The UI
is compiled into the binary. `make dev` serves it from disk instead.

**Nine sources failing at once is one cause, not nine.** When YouTube retired its
RSS feeds, every channel 404'd simultaneously and it looked like nine stale channel
IDs. It was one upstream change. Check whether a *known-good* URL of the same shape
also fails before you start fixing rows.

---

## Backing it up

Everything is one SQLite file in the volume:

```sh
docker stop interstellar
docker run --rm -v interstellar-data:/data -v "$PWD":/backup alpine \
  cp /data/interstellar.db /backup/interstellar-backup.db
docker start interstellar
```

That file is the whole instance — topics, sources, items, stories, cost log.

**Stop the container first.** SQLite keeps a write-ahead log, so a copy taken
mid-run can miss the most recent writes and restore to a state that never existed.

---

## Where to go next

- [README](../README.md) — configuration reference, provider registry, Ask, the
  weekly round-up, tuning topics
- `specs/interstellar-phase0-spec.md` — the design spec and its decision log
- `CLAUDE.md` — the conventions, and the traps behind each one
