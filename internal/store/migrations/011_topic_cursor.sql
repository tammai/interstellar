-- Each topic carries its own classify cursor.
--
-- The classify watermark was one value for the whole instance, so it could only say
-- "every topic has judged everything up to here" -- and a topic that was not there
-- for that judgement never got one. A topic added on day 2 classified only what
-- arrived after day 2; a topic paused for a week and resumed skipped that week; a
-- topic created empty and given its definition later skipped everything in
-- between. Nothing failed, the topic just started late and stayed that way.
--
-- With a cursor per topic, "behind" is a fact about one topic, and the classify
-- pass catches it up over the sky's window without re-judging anything for the
-- topics that are current. Same (fetched_at, id) format as store.FormatWatermark.
--
-- Topics that were being classified inherit the old instance-wide mark, so an
-- upgrade re-reads nothing. Paused or undefined ones get NULL: they genuinely
-- missed those items, and NULL is what makes them catch up when they come back.
ALTER TABLE topics ADD COLUMN classified_through TEXT;

UPDATE topics
   SET classified_through = (SELECT value FROM settings WHERE key = 'last_classified_fetched_at')
 WHERE active = 1
   AND definition <> ''
   AND COALESCE((SELECT value FROM settings WHERE key = 'last_classified_fetched_at'), '') <> '';
