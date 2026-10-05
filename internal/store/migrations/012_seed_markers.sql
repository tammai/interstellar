-- Seed defaults once per database, not whenever a table is empty.
--
-- Bootstrap used to seed topics and sources whenever either table was empty, so an
-- owner who deleted every topic or every source to start clean got the full
-- default list back on the next restart. It now writes a marker the first time
-- and never seeds again once it is there.
--
-- Any database with settings rows has already been bootstrapped, so it is marked
-- here: an upgrade must not seed into a table its owner emptied on purpose. A fresh
-- database has no settings yet when migrations run, gets no marker, and is seeded
-- by Bootstrap as before.
INSERT INTO settings (key, value)
SELECT 'seeded_topics', '1' WHERE EXISTS (SELECT 1 FROM settings);

INSERT INTO settings (key, value)
SELECT 'seeded_sources', '1' WHERE EXISTS (SELECT 1 FROM settings);
