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
  -e DEEPSEEK_API_KEY=sk-... \
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
-e DEEPSEEK_API_KEY=sk-...      # the shipped default
-e ANTHROPIC_API_KEY=sk-ant-...  # if you step up a tier
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

Both default to `deepseek/deepseek-chat` — a few dollars a month at ~250
items/day. Two things to know about that default: DeepSeek has announced
peak-hour pricing that would catch the 10:30 run (immaterial at this volume), and
the data path runs through a China-based provider, so your topic definitions and
article titles go with it. Both are one settings change away from something else.

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
