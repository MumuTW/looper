-- Runs are timestamped at millisecond precision, so fast retries can collide on
-- (started_at, created_at) and latest-run selection needs a durable monotonic
-- tie-breaker owned by storage. The implicit rowid is not durable (VACUUM may
-- renumber it), so runs get an explicit seq assigned on insert.
ALTER TABLE runs ADD COLUMN seq INTEGER;

-- Backfill existing rows in chronological order, using insertion order (rowid)
-- to break exact timestamp ties, producing a unique rank per row. A single
-- window-function pass keeps the migration O(n log n) on large run histories.
UPDATE runs SET seq = ranked.rank
FROM (
  SELECT rowid AS rid,
         ROW_NUMBER() OVER (ORDER BY started_at ASC, created_at ASC, rowid ASC) AS rank
  FROM runs
) AS ranked
WHERE runs.rowid = ranked.rid;

CREATE UNIQUE INDEX IF NOT EXISTS idx_runs_seq ON runs (seq);
