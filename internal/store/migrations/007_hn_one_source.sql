-- Collapse the per-keyword Hacker News sources into one.
--
-- WHY, and it is not tidiness: signals.source_count is `count(DISTINCT source_id)`
-- over item_sources, and it is what makes a star gold (§6, >= 3 sources). With one
-- source row per keyword, a single HN story matching three keywords presented as
-- three independent outlets carrying it. Measured on a real instance before this
-- ran: 25 of 31 gold stars were gold on Hacker News alone, and one Launch HN post
-- was counted seven times. Hacker News is one outlet. It gets one row.
--
-- The keywords themselves are NOT lost -- they move into the surviving row's
-- config as a `queries` array, each keeping its own min_points, because the
-- threshold is per keyword on purpose (a broad term needs a higher bar than a
-- niche one). sources.HNConfig reads either shape.

-- The survivor is the lowest-numbered hn_search row: it is the one whose
-- attributions and items are most likely to be the oldest, so repointing moves
-- the fewest rows.
CREATE TEMP TABLE hn_keep AS
  SELECT min(id) AS id FROM sources WHERE kind = 'hn_search';

-- Every keyword, in id order so the merged list reads the way the list read.
-- COALESCE over both shapes: a row may already carry the new one, and a hand-added
-- row may carry neither a query nor points.
CREATE TEMP TABLE hn_merged AS
  SELECT json_group_array(json_object(
           'query', q,
           'min_points', p
         )) AS queries
    FROM (
      SELECT json_extract(config, '$.query') AS q,
             COALESCE(json_extract(config, '$.min_points'), 0) AS p
        FROM sources
       WHERE kind = 'hn_search'
         AND COALESCE(json_extract(config, '$.query'), '') != ''
       ORDER BY id
    );

-- Attributions move to the survivor. OR IGNORE because a story that matched three
-- keywords has three rows here that all collapse onto one (item_id, source_id)
-- primary key -- that collapse IS the fix, so the conflict is the expected case
-- and not an error. The leftovers are then dropped.
UPDATE OR IGNORE item_sources
   SET source_id = (SELECT id FROM hn_keep)
 WHERE source_id IN (SELECT id FROM sources WHERE kind = 'hn_search')
   AND source_id != (SELECT id FROM hn_keep);

DELETE FROM item_sources
 WHERE source_id IN (SELECT id FROM sources WHERE kind = 'hn_search')
   AND source_id != (SELECT id FROM hn_keep);

-- items.source_id is "first seen on" and a NOT NULL foreign key, so it has to be
-- repointed before the rows it names are deleted.
UPDATE items
   SET source_id = (SELECT id FROM hn_keep)
 WHERE source_id IN (SELECT id FROM sources WHERE kind = 'hn_search')
   AND source_id != (SELECT id FROM hn_keep);

UPDATE sources
   SET name = 'Hacker News',
       config = json_object('queries', json((SELECT queries FROM hn_merged)))
 WHERE id = (SELECT id FROM hn_keep);

DELETE FROM sources
 WHERE kind = 'hn_search' AND id != (SELECT id FROM hn_keep);

DROP TABLE hn_keep;
DROP TABLE hn_merged;
