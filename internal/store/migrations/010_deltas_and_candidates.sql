-- What has been pushed, and what the watchlist does not know about yet.
--
-- Migration 009 gave the world model somewhere to remember things. This gives it
-- the two facts a push needs: which changes have already gone out, and which
-- names keep turning up that no entity answers to.

-- Already pushed.
--
-- NULL means "not yet sent", which is what the delta query selects on. A column
-- rather than a separate log table because the question is always asked about a
-- claim -- "has this gone out" -- and a join that only ever returns one row per
-- claim is a table that wanted to be a column.
ALTER TABLE claims ADD COLUMN pushed_at TEXT;

CREATE INDEX idx_claims_pushed ON claims(pushed_at);

-- Candidates -- names that turned up and resolved to nothing.
--
-- The watchlist is a precision instrument, and in a field this young the most
-- valuable signal is routinely the thing not on it yet. Without this, an unknown
-- name is simply dropped and the only evidence it existed is a counter in a run
-- result that nobody reads twice.
--
-- Promotion is proposed, never automatic: three distinct sources inside fourteen
-- days makes a candidate worth SHOWING, and a person decides whether it is worth
-- tracking. That is the same contract SuggestSources has -- the model proposes, one
-- click is the review -- and it exists for the same reason. A watchlist that
-- rewrites itself is a watchlist nobody can trust to mean what it said yesterday.
CREATE TABLE candidates (
  id         INTEGER PRIMARY KEY,
  name       TEXT NOT NULL,          -- as first written, for display
  name_norm  TEXT NOT NULL UNIQUE,   -- the same fold entities use, so promotion can check collisions
  -- Dismissed candidates are kept, not deleted. A name rejected once ("Python",
  -- "GitHub") will be seen again next week, and without a record the same
  -- rejection is offered back every run until someone acts on it. This is the
  -- difference between a suggestion and a nag.
  dismissed  INTEGER NOT NULL DEFAULT 0,
  -- Set when the candidate becomes an entity, so a promoted name is neither
  -- offered again nor counted as still-pending.
  promoted_entity_id INTEGER REFERENCES entities(id),
  first_seen_at TEXT NOT NULL DEFAULT (datetime('now')),
  last_seen_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

-- One row per (candidate, item). The promotion rule counts DISTINCT SOURCES, not
-- mentions, so the source has to be carried: one aggregator repeating itself
-- three times is one outlet with an opinion, while three outlets is the field
-- noticing something. That is the same distinction signals.source_count draws,
-- and the same reason migration 007 collapsed Hacker News into one row.
--
-- item_id is here for the same two reasons it is on claims: provenance, and so
-- store.DeleteSource has something to clean up.
CREATE TABLE candidate_sightings (
  candidate_id INTEGER NOT NULL REFERENCES candidates(id),
  item_id      INTEGER NOT NULL REFERENCES items(id),
  source_id    INTEGER NOT NULL REFERENCES sources(id),
  seen_at      TEXT NOT NULL DEFAULT (datetime('now')),
  PRIMARY KEY (candidate_id, item_id)
);

CREATE INDEX idx_candidate_sightings_seen ON candidate_sightings(candidate_id, seen_at);
CREATE INDEX idx_candidate_sightings_item ON candidate_sightings(item_id);
