-- Triage's lifecycle authority is a source idempotency key, not an issue: one
-- issue can be reopened and create several independent source lifecycles.
-- Backfill the existing correlation_id column from the authoritative payload.
UPDATE event_logs
SET correlation_id = CASE event_type
  WHEN 'triage.enrolled' THEN json_extract(payload_json, '$.idempotencyKey')
  WHEN 'triage.report' THEN json_extract(payload_json, '$.idempotencyKey')
  WHEN 'triage.confirmed' THEN json_extract(payload_json, '$.reportKey')
  WHEN 'triage.asked' THEN json_extract(payload_json, '$.reportKey')
  WHEN 'triage.routed' THEN json_extract(payload_json, '$.reportKey')
  WHEN 'triage.retired' THEN json_extract(payload_json, '$.enrollmentKey')
END
WHERE entity_type = 'github_issue'
  AND event_type IN ('triage.enrolled', 'triage.report', 'triage.confirmed', 'triage.asked', 'triage.routed', 'triage.retired')
  AND correlation_id IS NULL;

CREATE INDEX IF NOT EXISTS idx_event_logs_triage_source_latest
  ON event_logs (project_id, entity_type, correlation_id, created_at DESC, id DESC);
