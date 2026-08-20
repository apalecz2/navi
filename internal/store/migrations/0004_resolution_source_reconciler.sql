-- 0004_resolution_source_reconciler: widen occurrences.resolution_source to
-- admit 'reconciler'.
--
-- The reconciler is the fourth resolution surface and the first one to assign
-- missed for the reason D-008 and K6 actually name: asked, and got nothing
-- inside the grace window. Writing 'sweeper' instead would have cost nothing
-- today and corrupted the one instrument Q-15 is waiting on. resolution_source
-- exists so that after a month this deployment knows where resolutions really
-- come from, and internal/sweeper resolves nothing at all -- a label naming a
-- loop that never wrote a resolution makes that number unreadable for exactly
-- as long as it takes to collect.
--
-- SQLite cannot widen a CHECK constraint in place, so this is the documented
-- table rebuild. Three things about the order below are load-bearing.
--
-- 1. The view goes first. With legacy_alter_table off -- the default -- an
--    ALTER TABLE ... RENAME reparses every object in the schema and rewrites
--    references to the renamed table inside view bodies. Leaving chains in
--    place would either fail the rename outright or silently repoint the view
--    at occurrences_old, which is worse because it would still work.
--
-- 2. The old table is renamed out of the way and the new one is created with
--    its final name and its final DDL. The other order -- build
--    occurrences_new, copy, then rename it into place -- depends on SQLite
--    rewriting the self-referencing REFERENCES clause during the rename, which
--    it only does when foreign_keys is on. It is on here (the writer DSN sets
--    _foreign_keys=1, internal/store/store.go), but if that ever stopped being
--    true the surviving table would reference a name that does not exist, every
--    later insert would fail "foreign key mismatch", and schema_version would
--    already read 4. This order has no such dependency: what is written below
--    is what survives.
--
-- 3. There is no ORDER BY on the copy, and none is needed. Immediate foreign
--    key constraints are evaluated at the conclusion of the statement, not row
--    by row, so one INSERT ... SELECT that copies every row leaves no dangling
--    parent_occurrence_id when it ends. The same rule is what makes DROP TABLE
--    occurrences_old safe despite its implicit delete firing the
--    self-referencing key: the table ends empty. Do not add an ORDER BY on the
--    theory that parents must be inserted before their children.
--
-- store/migrate.go runs this whole file inside one transaction, so a failure at
-- any statement leaves both the schema and the recorded version untouched.
-- PRAGMA foreign_keys cannot be changed inside a transaction and is
-- deliberately not attempted here -- see point 3 for why it is not needed.
--
-- This file is deliberately absent from sqlc.yaml's schema list, for the reason
-- 0002 is absent: it recreates the recursive-CTE view sqlc's SQLite analyzer
-- rejects. It adds no table and no column, only a widened CHECK that sqlc does
-- not model, so the generated catalog is still correct without it.

DROP VIEW chains;

ALTER TABLE occurrences RENAME TO occurrences_old;

CREATE TABLE occurrences (
  id                    TEXT PRIMARY KEY,
  item_id               TEXT NOT NULL REFERENCES items(id) ON DELETE CASCADE,

  starts_at             TEXT NOT NULL,             -- ISO-8601 UTC
  ends_at               TEXT,                      -- NULL for instants (all reminders)

  status                TEXT NOT NULL DEFAULT 'pending',

  -- override and snooze chain
  is_override           INTEGER NOT NULL DEFAULT 0,
  parent_occurrence_id  TEXT REFERENCES occurrences(id),
  snooze_depth          INTEGER NOT NULL DEFAULT 0,

  -- lifecycle timestamps
  notified_at           TEXT,
  reconciled_at         TEXT,
  resolved_at           TEXT,

  -- resolution detail
  resolution_note       TEXT,                      -- e.g. "on vacation"
  resolution_source     TEXT CHECK (resolution_source IN
                          ('notification', 'web', 'agent', 'sweeper', 'reconciler')),

  -- generated message
  message_text          TEXT,
  message_model         TEXT,
  message_generated_at  TEXT,
  generation_attempts   INTEGER NOT NULL DEFAULT 0,
  generation_pass       INTEGER NOT NULL DEFAULT 0, -- 0 none, 1 safety net, 2 refreshed

  created_at            TEXT NOT NULL
);

INSERT INTO occurrences (
  id, item_id, starts_at, ends_at, status,
  is_override, parent_occurrence_id, snooze_depth,
  notified_at, reconciled_at, resolved_at,
  resolution_note, resolution_source,
  message_text, message_model, message_generated_at,
  generation_attempts, generation_pass, created_at
)
SELECT
  id, item_id, starts_at, ends_at, status,
  is_override, parent_occurrence_id, snooze_depth,
  notified_at, reconciled_at, resolved_at,
  resolution_note, resolution_source,
  message_text, message_model, message_generated_at,
  generation_attempts, generation_pass, created_at
FROM occurrences_old;

DROP TABLE occurrences_old;

-- The five indexes, verbatim from 0001_init.sql. They went with the old table.

-- scheduler hot path: due and pending
CREATE INDEX idx_occ_due ON occurrences(starts_at) WHERE status = 'pending';

-- copywriter: needs text, due soon
CREATE INDEX idx_occ_ungenerated ON occurrences(starts_at)
  WHERE status = 'pending' AND message_text IS NULL;

-- reconciler and day view: everything for a date
CREATE INDEX idx_occ_status_starts ON occurrences(status, starts_at);

-- calendar range query and per-item history
CREATE INDEX idx_occ_item_starts ON occurrences(item_id, starts_at);

-- chain walking
CREATE INDEX idx_occ_parent ON occurrences(parent_occurrence_id)
  WHERE parent_occurrence_id IS NOT NULL;

-- And the view, verbatim from 0002_chains_view.sql. A chain counts once and any
-- completed link completes it (D-010, D-011); reading it is the whole roll-up.

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
