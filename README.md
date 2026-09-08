# ✦ Interstellar

Personal AI news intelligence. One container per person: it reads your feeds each
morning, decides what actually matters to *your* topics, and draws the day as a
star map.

One **signal** is one story — clustered across every source that carried it. Size
is relevance, gold is hot (three or more sources), and it dims as it ages.

## Run it

```sh
docker build -t interstellar .

docker run -p 8422:8422 -v interstellar-data:/data \
  -e TZ=Asia/Bangkok \
  -e OPENROUTER_API_KEY=sk-or-... \
  interstellar
```

Open <http://localhost:8422>. Build locally — there is deliberately no published
image. Each instance is one person's container built from this repo, and a
registry would add a release step and a stale-tag problem to a project whose whole
deployment story is "one command on your own machine."

Three things happen on first run: the database is created, seed sources and seven
placeholder topics are added, and the gear icon gets a dot. Follow it — the sky
stays empty until you write topic definitions, because a definition is the only
thing the classifier has to judge against.

## Configuration

Almost everything is configured under the gear icon and takes effect on the next
run **without a restart**. The environment carries only what a container must
know at boot, plus your provider keys:

| Variable | Default | Purpose |
|---|---|---|
| `TZ` | `UTC` | Drives the schedule. Set it, or your sky arrives at the wrong hour. |
| `PORT` | `8422` | HTTP port inside the container. |
| `DATA_DIR` | `/data` | Where the SQLite file lives. Mount a volume here. |
| *provider keys* | — | Only the ones your registry references (see below). |

### Providers and keys

The provider registry stores the **name** of an environment variable, never a
key. The value is read from the environment when a call is made, so it never
touches the database and no endpoint returns it. Settings shows you `key set` or
`key missing` — that state is all the screen needs.

Presets ship for `deepseek`, `anthropic`, `google`, `openrouter`, `litellm`, and
`ollama` (keyless), and you can add any other OpenAI-compatible endpoint. Set the
variable for whichever you actually use:

```sh
-e OPENROUTER_API_KEY=sk-or-...  # the shipped default — one key, every model
-e GOOGLE_API_KEY=...            # the alternative: Google AI Studio
-e YOUTUBE_API_KEY=...           # video discovery (YouTube Data API v3)
```

Adding a **new key** needs a container restart, because the environment is fixed
at boot. Everything else — which provider a role uses, which model, fallback
order, schedule, caps — is live.

### Models

Two roles, either of which can run on any provider:

- **Classifier** scores every item against every topic. Most of the cost lives
  here, which is why a cheap model earns its place.
- **Synthesizer** groups matched items into stories. The hardest judgment in the
  pipeline.

Both default to `openrouter/deepseek/deepseek-chat` — a few dollars a month at
~250 items/day. OpenRouter is the default because one key reaches every model
here, so stepping a role up a tier needs no new environment variable and no
restart. Role refs are written `provider/model`, and since OpenRouter's own model
ids already contain a slash they simply nest: `openrouter/anthropic/claude-haiku-4.5`,
`openrouter/google/gemini-2.5-flash-lite`. Only the first slash separates.

Two things to know about that default: your topic definitions and article titles
travel through OpenRouter to whichever vendor it routes to — with
`deepseek/deepseek-chat` that is a China-based provider — and OpenRouter bills a
margin over the vendor's own price, which is what buys the single key. Both are
one settings change away from something else: the `deepseek` preset still ships,
so pointing a role at `deepseek/deepseek-chat` goes straight to the vendor again.

**Google AI Studio** is the alternative if you would rather hold one vendor's key
directly. Take a key from <https://aistudio.google.com/apikey>, pass it as
`GOOGLE_API_KEY`, and set either role to `google/gemini-2.5-flash-lite` — the
`google` preset already points at Google's OpenAI-compatible endpoint, so no
registry edit is needed. Its free tier is generous enough to run a day's
classifying at this volume, which makes it the cheapest way to try the tool
before paying anyone.

Each role has a **test call** button that exercises the real chain and reports
provider, latency and tokens. Use it after any swap; it answers in seconds
instead of waiting for a run.

When quality forces a change, the ladder is: classifier failing the JSON gate →
step up a tier; digest reading poorly → step the synthesizer up. Each step is a
settings change.

### Ollama

The `ollama` preset is keyless: the `ollama` CLI holds your credentials and the
local server proxies for you, so Interstellar needs no key of its own.

```sh
ollama signin                     # once, for cloud models
ollama pull <model>               # or, for local weights
```

Then set either role to `ollama/<model>` — for example
`ollama/deepseek-v4-flash:cloud`.

Two things to get right:

- **The base URL depends on where Interstellar runs.** In Docker it must be
  `http://host.docker.internal:11434/v1`, which is what ships. Running natively
  (`make dev`), change it to `http://localhost:11434/v1` — the container address
  does not resolve on the host.
- **Local weights need a GPU Ollama can actually use.** That means Apple Silicon
  (Metal) or Linux with CUDA/ROCm. On an Intel Mac, Ollama is CPU-only: a 7B
  model runs at single-digit tokens/second, and the classifier generates
  thousands of tokens per batch. Use `:cloud` models there instead.

### Shadow-mode: comparing a local model before you trust it

There is no shadow-mode feature, and there deliberately isn't one. The provider
registry already does the job, so comparing a local model against your current one
takes no code and no restart:

1. Add the Ollama entry if it isn't there (it ships as a preset, keyless).
2. **Export your current settings first** — note what `classifier_model` says. This
   is the value you are going back to.
3. Point the classifier at `ollama/<model>` and save. It takes effect on the next
   run.
4. Let it run for a few days. Then compare, using what the app already records:
   - **Settings → Signal quality** for the hit rate per topic. A local model that
     matches 40% of everything where DeepSeek matched 12% is not being cleverer,
     it is being indiscriminate.
   - `make verify-classifier` for the JSON gate, which is the hard bar. A model that
     cannot return parseable JSON on 200 real items is disqualified regardless of
     how good its verdicts look.
   - The cost log in the settings popover, for what you are actually saving.
5. Keep it or put the old value back. Both are one settings change.

Run it on the **classifier**, not the synthesizer. The classifier is where the
volume and the cost are, its output is checkable mechanically, and a bad verdict
costs one missed article — while a bad synthesis is what you actually read.

## Verifying a model swap

Before trusting a model you just switched to:

```sh
make verify-classifier          # or: go run ./cmd/verify-classifier -n 200
```

This is the acceptance gate — every batch of a 200-item sample must produce
parseable JSON. It samples **your** items, not synthetic ones, because a model
that handles clean titles can still choke on the punctuation and mixed languages
real feeds produce. It makes real, billable calls, which is exactly why it is not
part of `make test`.

It also reports how many items went unscored. Valid JSON that silently drops a
third of its input is not a model worth shipping, and nothing else would show you
that.

## Video sources

A YouTube source gives the classifier a title and the channel's blurb, which is
almost nothing — a talk about exactly your topic and one about nothing you care
about look identical from the outside.

So each run fetches captions for a few new videos, has a cheap model condense them
to two or three sentences, and puts that in front of the channel's description where
the classifier reads it. `transcripts_per_run` on the Schedule screen controls how
many; **0 turns it off**.

Things worth knowing:

- **There is no supported API for this.** Captions are read from the watch page, the
  way a browser does. It works today and may stop without notice, so every failure
  is per-video and never breaks a run. If it does break, videos quietly go back to
  being judged on their titles.
- **Plenty of videos have no captions at all.** That is recorded as a permanent
  answer, not retried forever. Settings shows the counts.
- **Automatic captions mishear names.** When a track is auto-generated the
  summarizer is told so and asked not to repeat a name it cannot make sense of.
- Summaries run on the **classifier's** model (condensing is mechanical work) but are
  logged under their own `transcript` role, so you can see what they cost.

## Ask

`/chat/` is a second screen — a text box that answers questions from what this
instance has collected, and nothing else. Reach it from the speech-bubble icon on
the star map.

It loads none of the star map: no canvas, no animation loop, no `sky.js`. That is
deliberate and measured — the page pulls only its own stylesheet and script, and
never calls `requestAnimationFrame`. If you are on a slow machine or a phone, this
is the cheap door.

How a question is answered:

1. **Plan.** A cheap model call turns your question into a *typed* search — keywords,
   optional topic, optional date window. Never SQL: a model that can only fill in
   those fields cannot quietly answer a different question than the one you asked.
2. **Retrieve.** SQLite FTS5, locally, in milliseconds. The page shows what it found
   before the answer arrives, because the search is free and the answer is not.
3. **Answer.** A second call writes prose from the retrieved rows only, citing each
   claim as `[1]`, `[2]`. The citations are links; click them.

The line under each question tells you what was actually searched. Read it when an
answer looks wrong — a wrong answer here is almost always a wrong plan, and a plan
that only looked at last week explains itself.

Two things worth knowing:

- **Chat has its own daily budget** (`max_chat_calls_per_day`, default 100 — about
  fifty questions). It is separate from the pipeline's cap on purpose: an evening of
  curiosity must not leave tomorrow morning's run with nothing to spend.
- **The chat model is its own role**, switchable in Settings like the others. It
  defaults to the same model as the synthesizer. If it is unset, chat says so and
  the pipeline is unaffected — an unconfigured chat screen never stops your ingest.

Questions and answers are kept in the database. They are the only record of what you
actually wanted to know, which is better evidence of whether this thing is worth
opening than any click count.

### Search without a model

`GET /api/search?q=…` is the same retrieval with no model attached — full-text over
signals and articles, with `topic_id`, `from`, `to`, `kind` and `limit` filters.
Free, instant, and diacritic-insensitive, so `tieng viet` finds `Tiếng Việt`.

## What changed over a month

The sky shows one day. **Settings → Signal quality** also carries the last 30 days:
signals per topic, how many were hot, and a one-word direction per topic.

Two rows are worth looking for:

- **stopped** — this topic produced signals earlier in the window and none lately.
  Sometimes the news dried up; sometimes a source broke. The Sources screen says
  which.
- **quiet** — nothing at all in 30 days. Either the definition is too tight or
  nothing is watching this subject.

**Kept coming back** lists stories the synthesizer composed on several separate
days. This is the one thing a daily sky structurally cannot show: each day the story
is just one star among many, and only the history reveals it never went away.
Matching is on the normalized title, because the synthesizer rewords its titles
every run.

## Tuning your topics

A topic is only as good as its definition, and the sky alone will not tell you
which definition is wrong. **Settings → Signal quality** shows the per-topic hit
rate: how much of what the classifier read against a topic it judged relevant,
over the last day, week or month.

The number to look at is the rate, not the match count. Two matches out of four
items and two out of four hundred both read as "2 matches" everywhere else in the
app, and only the second is a definition worth rewriting:

- **Under ~2%** — drawn too tight, or your sources simply do not cover it. Widen
  the definition, or add a source.
- **Over ~50%** — drawn too loose. Everything is getting through, which makes the
  constellation noise rather than signal.
- **An em dash** — never classified in this window. Not the same as 0%, which is
  why it is not shown as one.

Only rates with at least 50 items behind them are flagged; below that the sample
is too small to mean anything. `Read` counts only the batches a model actually
answered, so a provider having a bad afternoon does not show up as a badly defined
topic.

## The weekly round-up

Once a week a model reads that week's signals and writes 200-350 words about what
actually happened — grouped by story rather than by topic, because two topics
converging on one thing is the most interesting shape a week can have.

It appears under **Settings → Weekly round-up**, where you can also write last
week's on demand. Reading it there needs nothing configured. `schedule_weekly_day`
controls when it runs; **empty stops it being written at all**.

A week with no signals gets no round-up. "There was no news" is a sentence the
absence of a digest already makes, and writing it would occupy the slot so a later
re-run looked done.

### Having it emailed

Off by default, and that is the design: spec decision #10 made the web UI the front
door on purpose. This exists for the day "I forgot to open it" actually happens.

Defaults point at **Resend**, used as a plain SMTP relay:

```sh
docker run -p 8422:8422 -v interstellar-data:/data \
  -e TZ=Asia/Bangkok \
  -e OPENROUTER_API_KEY=sk-or-... \
  -e RESEND_API_KEY=re_...        # the SMTP password
  interstellar
```

Then in **Settings → Weekly round-up → Send it somewhere**, fill in the recipient and
flip *Email it*. The shipped values are `smtp.resend.com`, port `587`, username
`resend` — Resend authenticates with that literal username and your API key as the
password.

- **The password is never stored.** The settings row holds the *name* of an
  environment variable; the value is read at send time, exactly like a provider key.
  The screen shows whether it resolves, never what it is.
- **`onboarding@resend.dev` works without verifying a domain**, but only to the
  address that owns the Resend account. Change the From once you have a domain.
- **Adding the variable needs a container restart**, because the environment is fixed
  at boot. Everything else is live.
- **Press "Send a test"** rather than waiting a week to discover a typo. It uses the
  same code path the round-up does.
- TLS is not optional: if the relay advertises STARTTLS and then refuses it, the send
  fails rather than falling back to plaintext.

### Or just a ping

*Ping a webhook* posts a small JSON payload — the week, the counts, and one sentence
— to a URL you choose. The round-up itself stays in the app deliberately: a webhook
carrying the whole digest would publish a week of your reading interests to someone
else's server.

## Keeping two instances in step

Each instance is one person's container, so two people accumulate two different
source lists. **Settings → Sources → Move this list** exports yours as JSON and
imports someone else's.

Import only ever adds. It will not delete the feeds you have pruned, because an
export goes stale the moment either side edits theirs, and losing your pruning to
someone else's older file is not a trade worth making. Removing a source stays a
deliberate act.

Every new feed is fetched once before it is accepted, and anything that does not
return items is added **paused** with the reason next to it. That is not caution for
its own sake: a feed can answer 200 in a browser and carry nothing, and a YouTube
channel id copied from page HTML can quietly serve a different channel. A paused row
with an explanation is visible; a silently dropped one is not.

## Saving a link by hand

**Settings → Sources → Save a link** takes any URL, reads its title and description,
and files it as an item. It is classified on the next run like anything else.

This is the way in for the places no feed reaches — LinkedIn scraping is out on ToS
grounds and the X API on pricing, but pasting a link you are already reading breaks
neither. Pasted links are attributed to a "Saved links" row in Sources, which is
paused because it has no feed to fetch.

## Backup

The entire instance is one file.

```sh
docker run --rm -v interstellar-data:/data -v "$PWD":/backup alpine \
  cp /data/interstellar.db /backup/interstellar-backup.db
```

Restore by copying it back. Do it while the container is stopped — SQLite's
write-ahead log means a copy taken mid-run can miss the most recent writes.

## Development

```sh
make dev       # run against ./.devdata, serving the UI from disk
make test      # offline test suite — never touches a network or a provider
make test-ui   # the star map's layout logic
make lint      # vet + gofmt
make size      # build and check the image against the <30MB target
```

`make dev` sets `INTERSTELLAR_WEB_DIR`, which bypasses `go:embed` so a CSS or JS
edit shows up on reload instead of needing a rebuild. Production never sets it.

`make docker-multiarch` verifies amd64 and arm64 both build; it creates a
dedicated buildx builder on first use and leaves your default alone.

## Design

`specs/interstellar-phase0-spec.md` is the contract — architecture, data model,
pipeline, and the acceptance criteria that define done.
`specs/interstellar-starmap-prototype.html` is the approved UI, locked by §6 of
that spec. `CLAUDE.md` is the orientation for anyone picking up the code.
