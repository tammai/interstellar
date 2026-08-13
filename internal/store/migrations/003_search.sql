-- Full-text search over signals and items.
--
-- FTS5 ships inside the pure-Go driver, so this costs no new dependency and does
-- not touch CGO_ENABLED=0 -- the whole reason multi-arch images stay trivial.
--
-- External-content tables (content=/content_rowid=): the index stores only the
-- inverted terms and reads column values back from the source table on demand.
-- That halves the storage of a contentless-vs-duplicated choice and, more
-- importantly, means there is exactly one copy of the truth. The cost is that
-- SQLite will NOT keep the index in sync on its own -- the triggers below are
-- load-bearing, not an optimization.
--
-- remove_diacritics 2 is deliberate. §9 keeps topic definitions in Vietnamese and
-- feeds carry Vietnamese titles, so "tieng viet" typed on a plain keyboard has to
-- find "Tiếng Việt". Option 2 rather than 1 because 1 leaves some codepoints
-- untouched.

CREATE VIRTUAL TABLE signals_fts USING fts5(
  title,
  reason,
  content='signals',
  content_rowid='id',
  tokenize = "unicode61 remove_diacritics 2"
);

CREATE VIRTUAL TABLE items_fts USING fts5(
  title,
  summary,
  content='items',
  content_rowid='id',
  tokenize = "unicode61 remove_diacritics 2"
);

-- Backfill what is already there. An instance that has been running for weeks
-- must not come back with an empty index.
INSERT INTO signals_fts(rowid, title, reason)
  SELECT id, title, reason FROM signals;
INSERT INTO items_fts(rowid, title, summary)
  SELECT id, title, COALESCE(summary, '') FROM items;

-- Signals are rewritten per (run_date, topic) by ReplaceSignals: DELETE then
-- INSERT, inside one transaction. So the delete trigger is not a rare path -- it
-- fires on every compose, for every topic, and an index that only handled inserts
-- would accumulate yesterday's deleted stars forever and return them as hits.
CREATE TRIGGER signals_fts_ai AFTER INSERT ON signals BEGIN
  INSERT INTO signals_fts(rowid, title, reason)
    VALUES (new.id, new.title, new.reason);
END;

CREATE TRIGGER signals_fts_ad AFTER DELETE ON signals BEGIN
  INSERT INTO signals_fts(signals_fts, rowid, title, reason)
    VALUES ('delete', old.id, old.title, old.reason);
END;

-- Nothing updates a signal in place today. The trigger exists anyway: the day
-- something does, a missing update trigger corrupts the index silently, and
-- migrations are append-only so the fix would need a whole new file.
CREATE TRIGGER signals_fts_au AFTER UPDATE ON signals BEGIN
  INSERT INTO signals_fts(signals_fts, rowid, title, reason)
    VALUES ('delete', old.id, old.title, old.reason);
  INSERT INTO signals_fts(rowid, title, reason)
    VALUES (new.id, new.title, new.reason);
END;

-- Items are insert-only in practice (INSERT ... ON CONFLICT DO NOTHING, and
-- DeleteTopic leaves items alone by design). Same reasoning as above for keeping
-- all three.
CREATE TRIGGER items_fts_ai AFTER INSERT ON items BEGIN
  INSERT INTO items_fts(rowid, title, summary)
    VALUES (new.id, new.title, COALESCE(new.summary, ''));
END;

CREATE TRIGGER items_fts_ad AFTER DELETE ON items BEGIN
  INSERT INTO items_fts(items_fts, rowid, title, summary)
    VALUES ('delete', old.id, old.title, COALESCE(old.summary, ''));
END;

CREATE TRIGGER items_fts_au AFTER UPDATE ON items BEGIN
  INSERT INTO items_fts(items_fts, rowid, title, summary)
    VALUES ('delete', old.id, old.title, COALESCE(old.summary, ''));
  INSERT INTO items_fts(rowid, title, summary)
    VALUES (new.id, new.title, COALESCE(new.summary, ''));
END;
