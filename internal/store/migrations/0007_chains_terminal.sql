-- 0007_chains_terminal: the chains view learns how a chain ended, and learns to
-- be filtered cheaply.
--
-- Two changes, one recreated view. Nothing existing changes meaning: every
-- column the old view had keeps its name and its value (checked row for row
-- against the old definition on 20k occurrences, zero differences either way),
-- and chains.go and CountCompletedChains select by name. Only the view is
-- recreated, no table, so this file stays off sqlc.yaml's schema list like 0002
-- and 0004.
--
-- 1. Three new columns, because statistics need them and 04-data-model.md
--    already states each as a rule on a chain rather than on a row. Reading
--    them from occurrences in the stats query would be the one thing V6
--    forbids - a second definition of what a chain is.
--
--      terminal_status  the status of the chain's last link, the one with the
--                       greatest snooze_depth. A snooze chain is linear (snooze
--                       is legal only from notified and writes exactly one
--                       child), so "last" is unambiguous.
--      terminal_note    that link's resolution_note, so a skip's reason travels
--                       with the chain it closed.
--      was_reconciled   any link was named by a check-in (reconciled_at set).
--                       Next to a non-terminal terminal_status this reads
--                       "awaiting a check-in answer" rather than merely "not
--                       resolved yet".
--
-- 2. The walk starts per root instead of from every root. The old definition
--    was one recursive CTE seeded with *every* parentless occurrence and then
--    grouped, and SQLite cannot push a predicate into a recursive CTE, so
--    `WHERE scheduled_at >= ? AND scheduled_at < ?` filtered the finished
--    result: a one-week range cost exactly what "all" cost (measured: 170 ms at
--    20k occurrences, 1.9 s at 200k). Here the outer FROM is the plain
--    occurrences table, so a predicate on scheduled_at (idx_occ_starts) or on
--    item_id (idx_occ_item_starts) selects roots *before* anything is walked,
--    and each surviving root walks only its own descendants through the
--    parent_occurrence_id index. The same week is under a millisecond, and
--    "all" is faster than before (67 ms against 166 ms) because the work is
--    now proportional to roots rather than to a join over the whole walk.
--
--    The cost is that the walk is written out once per column. Chains are one
--    to four links, so each is a handful of index probes; it is spelled out
--    rather than shared because SQLite has no lateral join to share it with.
--
--    Every walk-derived column below is the same recursion over the same root;
--    change one and change all, and the naviseed chain checks will say if you
--    forgot.

DROP VIEW chains;

CREATE VIEW chains AS
SELECT
  r.id                                               AS root_id,
  r.item_id,
  r.starts_at                                        AS scheduled_at,
  (WITH RECURSIVE walk(id) AS (
     SELECT r.id
     UNION ALL
     SELECT o.id FROM occurrences o JOIN walk w ON o.parent_occurrence_id = w.id)
   SELECT MAX(o.snooze_depth) FROM walk w JOIN occurrences o ON o.id = w.id)
                                                     AS snooze_count,
  (WITH RECURSIVE walk(id) AS (
     SELECT r.id
     UNION ALL
     SELECT o.id FROM occurrences o JOIN walk w ON o.parent_occurrence_id = w.id)
   SELECT MAX(o.status = 'completed') FROM walk w JOIN occurrences o ON o.id = w.id)
                                                     AS was_completed,
  (WITH RECURSIVE walk(id) AS (
     SELECT r.id
     UNION ALL
     SELECT o.id FROM occurrences o JOIN walk w ON o.parent_occurrence_id = w.id)
   SELECT MIN(CASE WHEN o.status = 'completed' THEN o.resolved_at END)
     FROM walk w JOIN occurrences o ON o.id = w.id)
                                                     AS completed_at,
  r.notified_at,
  (WITH RECURSIVE walk(id) AS (
     SELECT r.id
     UNION ALL
     SELECT o.id FROM occurrences o JOIN walk w ON o.parent_occurrence_id = w.id)
   SELECT MAX(o.reconciled_at IS NOT NULL) FROM walk w JOIN occurrences o ON o.id = w.id)
                                                     AS was_reconciled,
  (WITH RECURSIVE walk(id) AS (
     SELECT r.id
     UNION ALL
     SELECT o.id FROM occurrences o JOIN walk w ON o.parent_occurrence_id = w.id)
   SELECT o.status FROM walk w JOIN occurrences o ON o.id = w.id
    ORDER BY o.snooze_depth DESC LIMIT 1)
                                                     AS terminal_status,
  (WITH RECURSIVE walk(id) AS (
     SELECT r.id
     UNION ALL
     SELECT o.id FROM occurrences o JOIN walk w ON o.parent_occurrence_id = w.id)
   SELECT o.resolution_note FROM walk w JOIN occurrences o ON o.id = w.id
    ORDER BY o.snooze_depth DESC LIMIT 1)
                                                     AS terminal_note
FROM occurrences r
WHERE r.parent_occurrence_id IS NULL;
