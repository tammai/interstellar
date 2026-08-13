-- The weekly round-up: one written pass over a week of signals.
--
-- Keyed on week_start so a re-run replaces rather than duplicates, the same way
-- ReplaceSignals makes compose idempotent. Without that, pressing "write it now"
-- twice would leave two versions of the same week with no way to tell which the
-- scheduler produced.
CREATE TABLE weekly_digests (
  id         INTEGER PRIMARY KEY,
  week_start TEXT    NOT NULL UNIQUE,   -- Monday, YYYY-MM-DD, local
  week_end   TEXT    NOT NULL,          -- Sunday, inclusive

  body       TEXT    NOT NULL,          -- the round-up itself, plain prose

  -- What it was written from. Kept because the body is the only artifact a person
  -- reads, and "written from 4 signals" versus "from 40" is the difference between
  -- a thin week and a broken pipeline -- which the prose alone cannot tell you.
  signals    INTEGER NOT NULL DEFAULT 0,
  topics     INTEGER NOT NULL DEFAULT 0,

  tokens_in  INTEGER NOT NULL DEFAULT 0,
  tokens_out INTEGER NOT NULL DEFAULT 0,

  -- Whether this week's round-up has been delivered, and how. Nullable rather than
  -- a boolean: an instance with no email configured never sends, and "not sent"
  -- must be distinguishable from "failed to send".
  sent_at    TEXT,
  sent_via   TEXT,
  send_error TEXT,

  created_at TEXT    NOT NULL DEFAULT (datetime('now'))
);
