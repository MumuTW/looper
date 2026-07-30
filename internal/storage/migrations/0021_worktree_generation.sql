-- Worktree generation is the containment Authority for startup recovery (#149).
--
-- A generation identifies one daemon's claim on a checkout. Retiring a
-- generation is a durable, locally-provable fact that the daemon writes itself:
-- it does not require knowing whether the previous agent process is alive.
-- The next claim allocates generation+1 with a different directory name, so a
-- stale writer keeps writing into a path no daemon reads, pushes from, or
-- cleans.
--
-- Generation 1 keeps today's directory name, so no existing checkout moves.

ALTER TABLE worktrees ADD COLUMN generation INTEGER NOT NULL DEFAULT 1;
ALTER TABLE worktrees ADD COLUMN retired_at TEXT;

-- 0004 made (project_id, branch) unique across the whole table, which two live
-- generations for one branch would violate. Uniqueness now applies only to the
-- live generation; retired rows are history and may repeat the branch.
DROP INDEX IF EXISTS idx_worktrees_project_branch;
CREATE UNIQUE INDEX idx_worktrees_project_branch_live
  ON worktrees (project_id, branch) WHERE retired_at IS NULL;
