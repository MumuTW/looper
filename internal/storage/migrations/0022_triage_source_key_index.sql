-- Triage lifecycle payloads are the authority for source identity. Index the
-- authoritative key in place instead of copying it into correlation_id or a
-- second status table that could drift from the event payload.
CREATE INDEX idx_event_logs_triage_source_key ON event_logs (
  project_id,
  entity_type,
  (CASE event_type
    WHEN 'triage.enrolled' THEN json_extract(payload_json, '$.idempotencyKey')
    WHEN 'triage.report' THEN json_extract(payload_json, '$.idempotencyKey')
    WHEN 'triage.confirmed' THEN json_extract(payload_json, '$.reportKey')
    WHEN 'triage.routed' THEN json_extract(payload_json, '$.reportKey')
    WHEN 'triage.retired' THEN json_extract(payload_json, '$.enrollmentKey')
  END),
  event_type
)
WHERE entity_type = 'github_issue'
  AND event_type IN (
    'triage.enrolled',
    'triage.report',
    'triage.confirmed',
    'triage.routed',
    'triage.retired'
  );
