-- 0006_occ_starts_index: the calendar's unfiltered month query.
--
-- docs/04-data-model.md lists idx_occ_item_starts as "calendar range query and
-- per-item history", and it is - once the query names an item. Measured with
-- EXPLAIN QUERY PLAN, a range over starts_at alone uses neither listed index:
-- both lead with another column, so SQLite walks one of them end to end for
-- every month of every view. idx_occ_status_starts serves a status filter and
-- idx_occ_item_starts serves an item filter; this serves the case where the
-- caller names neither, which is the calendar's main case.
--
-- Not partial and not covering: the table is a few thousand rows a year, and
-- the point is a bounded seek rather than a smaller scan.

CREATE INDEX idx_occ_starts ON occurrences(starts_at);
