-- Target leases are the durable authority for an actor that may mutate one
-- managed checkout. They intentionally do not expire: a stale owner may only
-- be reclaimed after process identity proves it is gone.
CREATE TABLE IF NOT EXISTS target_leases (
  target_key TEXT PRIMARY KEY,
  owner_token TEXT NOT NULL,
  owner_kind TEXT NOT NULL,
  owner_id TEXT NOT NULL,
  purpose TEXT NOT NULL,
  process_pid INTEGER,
  process_start_time INTEGER,
  process_boot_id TEXT,
  acquired_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_target_leases_owner ON target_leases (owner_kind, owner_id);

-- Existing human takeovers predate target leases. Preserve their checkout
-- authority during upgrade; new takeovers receive cryptographically random
-- tokens, while this deterministic compatibility token is reachable only from
-- the pre-existing loop row and is replaced on the next takeover lifecycle.
INSERT INTO target_leases (
  target_key, owner_token, owner_kind, owner_id, purpose, acquired_at, updated_at
)
SELECT
  CASE
    WHEN target_type = 'pull_request' AND repo IS NOT NULL AND pr_number IS NOT NULL
      THEN project_id || '|pull_request:' || repo || ':' || pr_number
    WHEN target_type = 'issue' AND target_id IS NOT NULL
      THEN project_id || '|' || target_id
    WHEN type = 'worker' AND target_type = 'project'
      THEN project_id || '|worker:' || id
  END,
  'legacy-human-' || id,
  'human',
  id,
  'takeover',
  updated_at,
  updated_at
FROM loops
WHERE status = 'human_takeover'
  AND (
    (target_type = 'pull_request' AND repo IS NOT NULL AND pr_number IS NOT NULL)
    OR (target_type = 'issue' AND target_id IS NOT NULL)
    OR (type = 'worker' AND target_type = 'project')
  )
ON CONFLICT(target_key) DO NOTHING;
