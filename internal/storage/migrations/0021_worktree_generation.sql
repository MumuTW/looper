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

-- checkout_key identifies WHICH managed checkout a row is, independently of its
-- generation: the generation-1 directory name. A branch is not that identity.
-- One project can hold an attached planner checkout and a detached PR checkout
-- of the same branch at the same time, and keying rows by branch collapses them
-- into one row — which loses the very path a crash recovery has to retire.
ALTER TABLE worktrees ADD COLUMN checkout_key TEXT NOT NULL DEFAULT '';

-- Every existing row is generation 1, so its checkout key is exactly its
-- directory name. rtrim(path, <every non-slash character in path>) strips the
-- trailing basename and leaves the dirname; its length is where the basename
-- starts.
UPDATE worktrees
SET checkout_key = substr(worktree_path, length(rtrim(worktree_path, replace(worktree_path, '/', ''))) + 1)
WHERE checkout_key = '' AND worktree_path IS NOT NULL;

-- 0004 made (project_id, branch) unique across the whole table. That is wrong in
-- two directions now: two live checkouts may legitimately share a branch, and
-- two generations of one checkout must coexist while the retired one is being
-- reclaimed. Uniqueness moves to the live generation of a checkout; branch stays
-- as a plain lookup index.
DROP INDEX IF EXISTS idx_worktrees_project_branch;
-- A row written without a checkout key falls back to its own path, which is
-- already unique per checkout: an unkeyed row must not collide with every other
-- unkeyed row of the same project.
CREATE UNIQUE INDEX idx_worktrees_project_checkout_live
  ON worktrees (project_id, CASE WHEN checkout_key = '' THEN worktree_path ELSE checkout_key END)
  WHERE retired_at IS NULL;
CREATE INDEX idx_worktrees_project_branch ON worktrees (project_id, branch);
