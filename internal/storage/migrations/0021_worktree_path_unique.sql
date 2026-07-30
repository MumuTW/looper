-- Migration 0004 rebuilt worktrees and accidentally dropped the physical
-- checkout identity constraint from 0002. Do not reconcile duplicates here:
-- a collision is ambiguous and must fail loudly for operator intervention.
CREATE UNIQUE INDEX idx_worktrees_path ON worktrees(worktree_path);
