-- Runs are timestamped at millisecond precision, so fast retries can collide on
-- (started_at, created_at) and latest-run selection needs a durable monotonic
-- tie-breaker owned by storage. The implicit rowid is not durable (VACUUM may
-- renumber it), so runs get an explicit seq assigned on insert.
ALTER TABLE runs ADD COLUMN seq INTEGER;

-- Backfill existing rows in chronological order, using insertion order (rowid)
-- to break exact timestamp ties, producing a unique rank per row.
UPDATE runs SET seq = (
  SELECT COUNT(*)
  FROM runs AS other
  WHERE other.started_at < runs.started_at
     OR (other.started_at = runs.started_at AND other.created_at < runs.created_at)
     OR (other.started_at = runs.started_at AND other.created_at = runs.created_at AND other.rowid <= runs.rowid)
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_runs_seq ON runs (seq);
