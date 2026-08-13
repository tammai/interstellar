-- One row per answered question.
--
-- Kept for three reasons, in order of how much they matter:
--
--  1. The questions are the record. llm_calls already says what a call cost, and
--     signals say what the pipeline decided -- but what the owner actually wanted
--     to know is written down nowhere else, and it is the most direct evidence of
--     whether this tool is worth opening. Spec §"Roadmap" gates Phase 3 on habit
--     evidence; this is better evidence than a click log.
--  2. The page reopens where it was left, without holding a conversation in
--     browser memory that a reload throws away.
--  3. A wrong answer is almost always a wrong plan. Storing the resolved query
--     next to the answer is what makes that diagnosable after the fact instead of
--     unreproducible.
--
-- No conversation/thread column. Questions here are one-shot against the corpus,
-- not a threaded chat, and inventing a thread id before anything needs one would
-- be a column that is always the same value.
CREATE TABLE chat_turns (
  id         INTEGER PRIMARY KEY,
  run_date   TEXT    NOT NULL,              -- local calendar date, as llm_calls uses
  question   TEXT    NOT NULL,
  answer     TEXT    NOT NULL,

  -- The resolved AskQuery, as JSON. Stored whole rather than as columns because
  -- it is read as a unit for display and never filtered on.
  query      TEXT,
  -- Whether the planner call succeeded. A fallback answer is still an answer, but
  -- it searched the question's raw words, and that changes how to read it.
  planned    INTEGER NOT NULL DEFAULT 1,

  -- What was retrieved, as a JSON array of {kind, id}. The rows themselves are
  -- not copied: signals are rewritten every compose, so a snapshot would drift
  -- from the sky it claims to cite. Ids plus the answer's own [n] markers are
  -- enough to show what was cited, and a deleted signal is honestly a dead link.
  hit_refs   TEXT,
  hit_count  INTEGER NOT NULL DEFAULT 0,

  calls      INTEGER NOT NULL DEFAULT 0,
  tokens_in  INTEGER NOT NULL DEFAULT 0,
  tokens_out INTEGER NOT NULL DEFAULT 0,
  elapsed_ms INTEGER NOT NULL DEFAULT 0,

  created_at TEXT    NOT NULL DEFAULT (datetime('now'))
);

-- History is read newest-first and nothing else.
CREATE INDEX idx_chat_turns_recent ON chat_turns (id DESC);
