-- 0002_chains_view: statistics operate on chains, not rows.
--
-- A chain is an occurrence plus its snooze descendants, identified by walking
-- parent_occurrence_id to the root. A chain counts once, and any completed link
-- completes it — which is why snoozing is a child row rather than a mutated
-- starts_at (D-010, R7). If a snooze broke a streak, the rational response would
-- be to ignore the notification instead of snoozing, which produces no data at
-- all. This view is what makes honest snoozing free.
--
-- Both the dashboard and the agent's get_stats tool read this view rather than
-- the table, so they agree by construction (V6).

CREATE VIEW chains AS
WITH RECURSIVE walk(root_id, id) AS (
  SELECT id, id FROM occurrences WHERE parent_occurrence_id IS NULL
  UNION ALL
  SELECT w.root_id, o.id
    FROM occurrences o JOIN walk w ON o.parent_occurrence_id = w.id
)
SELECT
  w.root_id,
  r.item_id,
  r.starts_at                                        AS scheduled_at,
  MAX(o.snooze_depth)                                AS snooze_count,
  MAX(o.status = 'completed')                        AS was_completed,
  MIN(CASE WHEN o.status = 'completed' THEN o.resolved_at END) AS completed_at,
  r.notified_at
FROM walk w
JOIN occurrences o ON o.id = w.id
JOIN occurrences r ON r.id = w.root_id
GROUP BY w.root_id;
