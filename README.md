# ✦ Interstellar

Personal AI news intelligence. One container per person: it reads your feeds each
morning, decides what actually matters to *your* topics, and draws the day as a
star map.

One signal is one story — clustered across every source that carried it. Size is
relevance, gold is hot (three or more sources), and it dims as it ages.

> **Status:** in development. The pipeline and star map land over milestones M1–M7;
> today the container boots, migrates its database, and serves a placeholder page.

## Run it

```sh
docker run -p 8422:8422 -v interstellar-data:/data \
  -e TZ=Asia/Bangkok \
  -e DEEPSEEK_API_KEY=sk-... \
  interstellar
```

Then open <http://localhost:8422>.

No image is published yet — build it first with `make docker` (or `docker build -t interstellar .`).

## Configuration

Almost everything is configured in the app, under the gear icon, and takes effect
on the next run without a restart. The environment only carries what a container
must know at boot, plus your provider keys:

| Variable | Default | Purpose |
|---|---|---|
| `TZ` | `UTC` | Drives the schedule. Set it, or your sky arrives at the wrong hour. |
| `PORT` | `8422` | HTTP port inside the container. |
| `DATA_DIR` | `/data` | Where the SQLite file lives. Mount a volume here. |
| *provider keys* | — | Only the ones your registry actually references. |

**Provider keys are referenced by name, never stored.** The provider registry in
Settings holds the *name* of an environment variable (`DEEPSEEK_API_KEY`,
`ANTHROPIC_API_KEY`, …); the value is read from the environment when a call is
made. It never touches the database, and no endpoint returns it.

Out of the box both roles — the classifier that scores items against your topics,
and the synthesizer that clusters them into stories — run on
`deepseek/deepseek-chat`, which costs a few dollars a month at ~250 items/day. Any
role can be pointed at any provider from Settings → Model, including a local
Ollama, and a per-role **test call** button verifies a swap in about ten seconds.

Two things worth knowing about that default: DeepSeek has announced peak-hour
pricing that would catch the 10:30 run (immaterial at this volume), and the data
path runs through a China-based provider — your topic definitions and article
titles go with it. Both are one settings change away from something else.

## Backup

The entire instance is one file.

```sh
docker run --rm -v interstellar-data:/data -v "$PWD":/backup alpine \
  cp /data/interstellar.db /backup/interstellar-backup.db
```

## Development

```sh
make dev      # run against ./.devdata, no Docker
make test     # offline test suite
make lint     # vet + gofmt check
make size     # build and check the image against the <30MB target
```

`make docker-multiarch` verifies amd64 and arm64 both build; it creates a
dedicated buildx builder on first use and leaves your default one alone.

## Design

`specs/interstellar-phase0-spec.md` is the contract — architecture, data model,
pipeline, and the acceptance criteria that define done.
`specs/interstellar-starmap-prototype.html` is the approved UI, locked by §6 of
that spec. `CLAUDE.md` is the orientation for anyone (or anything) picking up the
code.
