-- The world model: what exists, and what changed about it.
--
-- Everything before this migration is per-run. Items accumulate, matches
-- accumulate, and llm_calls accumulates -- but nothing carries IDENTITY across
-- runs. ReplaceSignals deletes and re-inserts a topic's stars every compose, and
-- MatchesForTopic hands the synthesizer a rolling two-day window of raw items, so
-- a story that stays newsworthy gets a fresh row with a reworded title every
-- morning and nothing knows the two are the same thing.
--
-- That is already costing something measurable. collapseWeeklyStories exists
-- solely because of it: the round-up had to key on primary_url to stop listing
-- one story twice, and the comment there says why in the same words this one
-- does. The workaround is right, and it is the missing layer done ad hoc at a
-- single call site.
--
-- So: an entity is a thing that persists between runs (a tool, a vendor, a
-- model), and a claim is one dated, attributed statement that something changed
-- about it. Items stay exactly as they are -- a claim points AT an item, it does
-- not replace it. Nothing in this migration is read by the sky, the weekly
-- round-up, trending or Ask, all of which continue to read `signals` untouched.

-- Entities -- the watchlist.
--
-- Note what is deliberately NOT constrained here. `layer`, `kind` and `status`
-- are plain TEXT with no CHECK, and that is migration 001's lesson applied
-- rather than ignored: sources.kind pinned CHECK (kind IN (...)), SQLite cannot
-- widen a CHECK via ALTER TABLE, and the documented rebuild needs DROP TABLE
-- sources -- which fails while items.source_id references it. That trap is why
-- the paste-a-link inbox had to disguise itself as an `rss` row.
--
-- These three columns would hit it HARDER, because unlike sources.kind they are
-- all expected to change. A layer is mutable by design: candidate promotion runs
-- a layer-fit check and "none of these fit" is itself the signal that a new layer
-- exists, which is the single most interesting thing this table can tell you. A
-- status moves active -> retired when a tool is deprecated, and the row stays,
-- because the history is the point. Validation lives in Go, where a whitelist is
-- one edit rather than a table rebuild -- the same place and the same reason
-- Asker.sanitizePlan validates a plan instead of trusting it.
CREATE TABLE entities (
  id        INTEGER PRIMARY KEY,
  name      TEXT NOT NULL,
  -- Normalized form of name, for resolution. UNIQUE so that two rows cannot
  -- claim the same name: a duplicate entity silently splits one thing's history
  -- in two, and the halves are individually plausible.
  name_norm TEXT NOT NULL UNIQUE,
  layer     TEXT NOT NULL,
  kind      TEXT NOT NULL DEFAULT 'tool',   -- CSV calls this `type`; `kind` matches sources
  status    TEXT NOT NULL DEFAULT 'active', -- active | retired; retired keeps its history

  -- The vendor's own page. Doubles as the changelog-source list, and as the test
  -- for whether a claim is first-party: a claim whose url shares this host was
  -- said by the vendor about itself.
  first_party_url TEXT,

  -- Two timestamps, two different questions, and conflating them is how a
  -- staleness job ends up measuring aggregator chatter instead of vendor silence.
  -- last_seen_at moves when ANY claim mentions the entity. last_verified_at moves
  -- only for a first_party or vendor_blog claim -- that is, when the vendor
  -- itself said something. "Nobody has written about this in 60 days" and "the
  -- vendor has not shipped in 60 days" are both worth knowing and they are not
  -- the same fact.
  last_seen_at      TEXT,
  last_verified_at  TEXT,

  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

-- Aliases -- the other names one entity answers to.
--
-- Renames are the normal case in this field, not the exception: Windsurf was
-- absorbed into Devin Desktop, Q Developer became Kiro. Without aliases a rename
-- reads as one entity going quiet and a brand new one appearing, which is exactly
-- backwards from what happened.
--
-- norm is the PRIMARY KEY, so one alias can never point at two entities. That is
-- a real collision in the seed list, not a hypothetical -- `Codex` belongs to
-- OpenAI Codex, `Cascade` arrived with Devin via Windsurf -- and the right moment
-- to find out is at import, loudly, rather than at extraction time when a claim
-- quietly lands on whichever row was written first.
CREATE TABLE entity_aliases (
  norm      TEXT PRIMARY KEY,               -- normalized; the lookup key
  alias     TEXT NOT NULL,                  -- as written, for display
  entity_id INTEGER NOT NULL REFERENCES entities(id)
);

-- Claims -- one dated, attributed statement that something changed.
--
-- This is the residue a run leaves behind. Synthesis does not read it yet; the
-- first cut writes claims and nothing else, so that what accumulates can be
-- looked at before anything depends on it being right.
CREATE TABLE claims (
  id        INTEGER PRIMARY KEY,
  entity_id INTEGER NOT NULL REFERENCES entities(id),

  -- The item this was extracted from. NOT in the original design, and load-bearing
  -- twice over: it is the provenance link that lets a claim be traced back to the
  -- feed entry it came from, and it is what store.DeleteSource must clean up.
  -- No foreign key here declares ON DELETE CASCADE and the pool runs with
  -- foreign_keys(on), so a missed clause does not leave a stale row -- the delete
  -- fails, the transaction rolls back, and the source cannot be deleted at all.
  -- item_transcripts shipped exactly that bug: only video items carried a row, so
  -- every other kind deleted cleanly and a YouTube channel could not be removed.
  item_id   INTEGER NOT NULL REFERENCES items(id),

  -- launch | pricing | acquisition | deprecation | benchmark | incident |
  -- integration | none. Unconstrained here for the reason entities.layer is:
  -- this list will gain a value (funding, licence change, outage) long before
  -- anything else in the schema moves, and a CHECK would make that a table
  -- rebuild. The whitelist lives in Go.
  change_type TEXT NOT NULL,

  -- first_party | vendor_blog | independent | aggregator. Derived from the item's
  -- host against entities.first_party_url and the source's kind -- computed here,
  -- never asked of the model. It is cheap to determine, impossible to hallucinate
  -- when it is derived, and it gates last_verified_at above.
  source_type TEXT NOT NULL,

  summary TEXT NOT NULL,
  url     TEXT NOT NULL,

  -- What the model thought of its own answer. Nothing reads this yet, and that is
  -- deliberate rather than an oversight: model-reported confidence is badly
  -- calibrated and a threshold set before anyone has looked at the distribution is
  -- a guess wearing a number. It is stored so the distribution CAN be looked at --
  -- the same posture migration 008 took toward reply quality, where recording the
  -- measurement first is what made the escalation ladder a measurement rather
  -- than a runbook.
  confidence REAL,

  -- Identity of the CLAIM, for the delta rule's "hash unseen in 14 days". Computed
  -- in Go over the entity, the change type and the canonical url -- not over the
  -- summary, which the model rewords on every pass and which would therefore defeat
  -- the check it exists to serve. Not UNIQUE: the same hash recurring after 14 days
  -- is a genuine second event, and the window is the rule, not the column.
  hash TEXT NOT NULL,

  seen_at    TEXT NOT NULL DEFAULT (datetime('now')),
  created_at TEXT NOT NULL DEFAULT (datetime('now')),

  -- Re-extraction must be idempotent. The extract watermark can stall for the same
  -- reason the classify one can, and when it does the next run offers the same
  -- items again -- bounded and loud, which is the right way round, but only if a
  -- second pass over one item cannot double the claims it produced. This is the
  -- same job INSERT OR IGNORE does against items.canonical_url at ingest, which
  -- spec §4 names as the first of the dedup levels.
  UNIQUE (item_id, entity_id, change_type)
);

-- Claims about one entity, newest first: the entity history panel, the staleness
-- job, and "what changed about X" all read this way.
CREATE INDEX idx_claims_entity_seen ON claims(entity_id, seen_at);

-- The delta rule's 14-day window looks up a hash and asks how recently it was
-- seen. Leading with hash makes that one index seek rather than a scan of
-- everything inside the window.
CREATE INDEX idx_claims_hash_seen   ON claims(hash, seen_at);

-- DeleteSource deletes claims for orphaned items before deleting the items.
CREATE INDEX idx_claims_item        ON claims(item_id);

CREATE INDEX idx_entity_aliases_ent ON entity_aliases(entity_id);

-- Entities of one layer, and the staleness sweep's "not verified since" scan.
CREATE INDEX idx_entities_layer     ON entities(layer);
