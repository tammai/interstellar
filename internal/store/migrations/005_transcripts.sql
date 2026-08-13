-- Video transcripts, one row per item.
--
-- A separate table rather than a column on items, for two reasons. Transcripts are
-- large -- tens of thousands of characters where every other items column is tens
-- -- and items is read on every classify pass, every sky render and every search;
-- widening those rows to carry a payload none of them touch would be paid for
-- constantly. And the fetch has its own lifecycle: it succeeds, fails, or is not
-- applicable, and that state belongs next to the transcript rather than smeared
-- across nullable columns on items.
--
-- Rows are written for FAILURES too, with status telling them apart. That is the
-- point of the table existing at all: without a failure row, a video whose captions
-- are disabled would be retried on every run forever, and there are a lot of those.
CREATE TABLE item_transcripts (
  item_id    INTEGER PRIMARY KEY REFERENCES items(id),

  -- ok | none | error. `none` means the video genuinely has no captions, which is
  -- a permanent answer; `error` means the attempt failed and may be worth retrying
  -- when the code changes.
  status     TEXT    NOT NULL,
  text       TEXT,
  lang       TEXT,
  -- Whether these are YouTube's automatic captions. Kept because ASR mishears
  -- proper nouns, and a classifier matching on a misheard name is a real effect.
  generated  INTEGER NOT NULL DEFAULT 0,
  chars      INTEGER NOT NULL DEFAULT 0,

  -- The model-written summary that actually reaches the classifier. Stored so a
  -- re-run never pays to summarize the same video twice.
  summary    TEXT,

  -- The item's summary BEFORE any transcript work touched it -- normally the
  -- channel's own blurb.
  --
  -- This column is what makes applying a summary idempotent. The item's summary is
  -- built as "<transcript summary> — <original>", and without the original kept
  -- separately the only way to rebuild it is to prepend to whatever is already
  -- there. Saving twice then yields "summary — summary — blurb", and re-summarizing
  -- with a better model yields "new — old — blurb". Captured on first attempt and
  -- never overwritten, so the derived value is always recomputable from scratch.
  summary_base TEXT,

  error      TEXT,

  fetched_at TEXT    NOT NULL DEFAULT (datetime('now'))
);

-- The work query is "which items still need a transcript", which is an anti-join
-- against this table; the primary key serves it. This index serves the opposite
-- question -- how many failed, and why -- for the status screen.
CREATE INDEX idx_item_transcripts_status ON item_transcripts (status);
