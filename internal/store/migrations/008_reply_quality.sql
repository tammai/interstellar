-- The quality half of the cost log.
--
-- llm_calls already records what every attempt COST: provider, model, tokens, and
-- whether the transport succeeded. What it has never recorded is whether the reply
-- was any GOOD, and those are different questions with different answers. A call
-- that returns 200, well-formed JSON, and one verdict for a batch of thirty has
-- succeeded by every measure this table could see, and thrown away 29 items.
--
-- That is not hypothetical. A prompt asking for a top-level JSON array under
-- response_format {"type":"json_object"} comes back as a single object on several
-- providers, with no error anywhere -- measured on openrouter 2026-08-21, a 12-item
-- batch answered with one verdict by two different models. It cost days of
-- near-empty skies, and nothing in the schema could have shown it: the runs read as
-- topic definitions drawn too tight. The seeded deepseek-chat tolerated the array
-- shape, so it only appeared when the roles moved.
--
-- So: what the request asked for, what the reply delivered, and whether it parsed
-- at all. Grouped by provider and model alongside the spend, these are what turn
-- §3.1's escalation ladder ("classifier fails the JSON gate -> flash tier ->
-- haiku") from a runbook into a measurement.
--
-- All four are nullable or defaulted, because most rows will never carry them: a
-- chat answer, a transcript summary and a test call have no record count to be
-- complete about, and an attempt that never got a reply has no quality to report.
-- NULL means "not asked", never "zero" -- that difference is the whole reason a
-- rate computed from these columns has an honest denominator.
ALTER TABLE llm_calls ADD COLUMN items_in  INTEGER;
ALTER TABLE llm_calls ADD COLUMN items_out INTEGER;
ALTER TABLE llm_calls ADD COLUMN parse_ok  INTEGER;
ALTER TABLE llm_calls ADD COLUMN repair    INTEGER NOT NULL DEFAULT 0;
