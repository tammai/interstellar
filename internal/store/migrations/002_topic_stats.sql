-- Per-topic classifier statistics, one row per (run date, topic).
--
-- The hit rate a topic earns is matches_kept / items_considered, and only the
-- numerator is recoverable from the tables that already exist: `matches` holds
-- score >= 4 verdicts only (§5.2), so the count of items the classifier actually
-- looked at is thrown away the moment a run ends. Without a denominator there is
-- no way to tell a topic that matched 2 of 4 items from one that matched 2 of 400
-- -- and telling those apart is the entire point.
--
-- Rows accumulate within a day rather than replace: the classify watermark means a
-- second run on the same date considers only items the first one had not seen, so
-- the day's totals are the sum of its passes.
CREATE TABLE topic_stats (
  run_date         TEXT    NOT NULL,
  topic_id         INTEGER NOT NULL REFERENCES topics(id),
  items_considered INTEGER NOT NULL DEFAULT 0,   -- from batches that succeeded only
  matches_kept     INTEGER NOT NULL DEFAULT 0,
  batches          INTEGER NOT NULL DEFAULT 0,   -- model calls this topic cost
  updated_at       TEXT    NOT NULL DEFAULT (datetime('now')),

  -- run_date leads the key, so the window queries this table exists to serve
  -- range-scan it without a second index.
  PRIMARY KEY (run_date, topic_id)
);
