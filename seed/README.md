# Seed: what a new instance starts with

A new database gets its topics and sources from a seed file, once. That happens on
the first boot only: Bootstrap records that it seeded (`seeded_topics` /
`seeded_sources` in `settings`). After that, the seed is never read again, even if
you later delete every topic or source.

Which file is used:

1. `DATA_DIR/seed.json`, if it exists. This is the per-instance override.
2. Otherwise `seed/default.json`, which is embedded in the binary.

If the override exists but cannot be read or does not parse, the first boot
stops. The log lists every problem. It does not quietly seed the defaults
instead.

## Format

```json
{
  "version": 1,
  "topics": [
    {
      "title": "Voice AI",
      "definition": "Speech-to-text, text-to-speech and real-time voice agents ...",
      "priority": 5,
      "active": true,
      "pos_x": 0.74,
      "pos_y": 0.15
    }
  ],
  "sources": [
    { "kind": "rss", "name": "Simon Willison", "config": { "url": "https://simonwillison.net/atom/everything/" }, "active": true },
    { "kind": "hn_search", "name": "Hacker News", "config": { "queries": [{ "query": "MCP", "min_points": 5 }], "firehose_min_points": 300 }, "active": true },
    { "kind": "youtube_rss", "name": "YouTube · Fireship", "config": { "channel_id": "UCsBjURrPoezykLs9EqgamOA" }, "active": true }
  ]
}
```

- **Topics.** `title` is required and must be unique. If `definition` is empty,
  the topic shows the setup hint and the classifier skips it until a definition is
  written. `active` defaults to `true`. `pos_x`/`pos_y` are the constellation's
  centre, from 0 to 1; set both or neither. A topic without a position is placed
  automatically. Unknown fields are rejected, so a misspelled `definition` is an
  error rather than an empty one.
- **Sources** use the same shape as Settings → Sources → Export, so an exported
  list can be pasted in. `kind` is `rss`, `hn_search` or `youtube_rss`. There is
  at most **one** `hn_search` source, and every keyword goes in its `queries`
  (see CLAUDE.md on migration 007). Two rows that fetch the same feed are
  rejected.

## Writing one in a Claude Code session

1. Edit `seed/default.json` (the default for every new instance), or write a
   `seed.json` for one instance.
2. Run `make seedcheck`, or `make seedcheck SEED=path/to/seed.json`. It validates
   the file and fetches every source with the app's own fetchers. It prints each
   feed's first item title, so you can confirm the feed belongs to the
   publication you meant. Set `YOUTUBE_API_KEY` to include YouTube channels.
   `OFFLINE=1` checks only the file.
3. Start a fresh database to see it:
   - **Dev:** delete `.devdata/interstellar.db*`, then run `make dev`. Put an
     override at `.devdata/seed.json`.
   - **Docker:** rebuild the image (`make docker`) for a changed default, or mount
     an override:
     `-v $PWD/seed.json:/data/seed.json:ro` next to the data volume. Then start
     against a new volume.

Never seed a URL that `make seedcheck` has not fetched. A feed can return 200 in
a browser and still parse to nothing, and a channel id scraped from page HTML can
belong to someone else's channel.
