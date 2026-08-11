-- Interstellar Phase 0 initial schema.
-- Spec §4, plus three approved additions (item_sources, topics.pos_*, llm_calls.provider).
-- Single user, single writer: no users table, no row-level anything.

CREATE TABLE topics (
  id         INTEGER PRIMARY KEY,
  title      TEXT NOT NULL,
  definition TEXT NOT NULL,
  priority   INTEGER NOT NULL DEFAULT 0,
  active     INTEGER NOT NULL DEFAULT 1,
  -- Constellation centre, 0..1 of the usable canvas. Assigned once on create and
  -- never recomputed: §6 protects spatial memory, so adding a topic must not move
  -- the topics already in the sky.
  pos_x      REAL,
  pos_y      REAL,
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE sources (
  id          INTEGER PRIMARY KEY,
  kind        TEXT NOT NULL CHECK (kind IN ('rss','hn_search','youtube_rss')),
  name        TEXT NOT NULL,
  config      TEXT NOT NULL,                    -- JSON, shape depends on kind
  active      INTEGER NOT NULL DEFAULT 1,
  last_run_at TEXT,                             -- advances on success only
  created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE items (
  id            INTEGER PRIMARY KEY,
  source_id     INTEGER NOT NULL REFERENCES sources(id),  -- first seen on
  canonical_url TEXT NOT NULL UNIQUE,
  title         TEXT NOT NULL,
  summary       TEXT,
  published_at  TEXT,
  fetched_at    TEXT NOT NULL DEFAULT (datetime('now'))
);

-- Cross-source attribution. items.canonical_url is UNIQUE, so the same story
-- arriving from three feeds collapses to one item row -- this table is what keeps
-- the fact that three sources carried it, which is what drives signals.source_count
-- and therefore the gold "hot" flag (§6) and the panel's "Seen on" list.
CREATE TABLE item_sources (
  item_id       INTEGER NOT NULL REFERENCES items(id),
  source_id     INTEGER NOT NULL REFERENCES sources(id),
  detail        TEXT,                           -- per-source annotation, e.g. "412 points"
  first_seen_at TEXT NOT NULL DEFAULT (datetime('now')),
  PRIMARY KEY (item_id, source_id)
);

CREATE TABLE matches (                          -- classifier verdicts, score >= 4 only
  item_id  INTEGER NOT NULL REFERENCES items(id),
  topic_id INTEGER NOT NULL REFERENCES topics(id),
  score    REAL NOT NULL,
  reason   TEXT,
  PRIMARY KEY (item_id, topic_id)
);

-- One star = one signal (cross-source story cluster), composed by the synthesizer.
CREATE TABLE signals (
  id           INTEGER PRIMARY KEY,
  run_date     TEXT NOT NULL,
  topic_id     INTEGER NOT NULL REFERENCES topics(id),
  title        TEXT NOT NULL,
  reason       TEXT NOT NULL,
  score        REAL NOT NULL,                   -- max over member items
  source_count INTEGER NOT NULL,                -- distinct sources; drives hot flag (>=3) and glow
  primary_url  TEXT NOT NULL,
  created_at   TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE signal_items (
  signal_id INTEGER NOT NULL REFERENCES signals(id) ON DELETE CASCADE,
  item_id   INTEGER NOT NULL REFERENCES items(id),
  PRIMARY KEY (signal_id, item_id)
);

-- One row per call ATTEMPT, including fallback hops, so a chain that silently
-- fell through to another vendor is visible in the cost log (§3.1).
CREATE TABLE llm_calls (
  id         INTEGER PRIMARY KEY,
  run_date   TEXT NOT NULL,
  role       TEXT NOT NULL,                     -- classifier | synthesizer | test
  provider   TEXT,                              -- registry entry name
  model      TEXT NOT NULL,
  tokens_in  INTEGER,
  tokens_out INTEGER,
  ok         INTEGER NOT NULL DEFAULT 1,
  error      TEXT,
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE INDEX idx_items_fetched_at      ON items(fetched_at);
CREATE INDEX idx_item_sources_source   ON item_sources(source_id);
CREATE INDEX idx_matches_topic         ON matches(topic_id);
CREATE INDEX idx_signals_run_topic     ON signals(run_date, topic_id);
CREATE INDEX idx_signal_items_item     ON signal_items(item_id);
CREATE INDEX idx_llm_calls_run_date    ON llm_calls(run_date);
