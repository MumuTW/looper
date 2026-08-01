package storage

import "strings"

// Repository projections are the code-owned scan contract. Each list is kept
// in the same order as its scan function so an unrelated column added by a
// migration cannot change the number or position of values returned by a
// repository query. This deliberately replaces wildcard projections rather than adding a
// second schema validator: a missing or renamed required column still fails at
// the named query boundary, while extra columns remain harmless.
const (
	projectColumns = `id, name, repo_path, base_branch, archived, metadata_json, created_at, updated_at`
	loopColumns    = `id, seq, project_id, type, target_type, target_id, repo, pr_number, status, config_json, metadata_json, last_run_at, next_run_at, created_at, updated_at`
	runColumns     = `id, loop_id, status, current_step, last_completed_step, checkpoint_json, summary, error_message, started_at, last_heartbeat_at, ended_at, created_at, updated_at, agent_snapshot_json, seq`

	agentExecutionColumns = `id, project_id, loop_id, run_id, vendor, status, pid, command_json, cwd, summary, parse_status, completion_signal, heartbeat_count, last_heartbeat_at, output_json, error_message, native_session_id, native_resume_mode, native_resume_status, native_resume_error, started_at, ended_at, metadata_json, created_at, updated_at`

	pullRequestSnapshotColumns = `id, project_id, repo, pr_number, head_sha, base_sha, title, body, author, diff_ref, checks_summary, unresolved_thread_count, review_state, payload_json, captured_at, created_at`
	eventLogColumns            = `id, event_type, project_id, loop_id, run_id, entity_type, entity_id, correlation_id, causation_id, actor_type, actor_id, actor_display_name, payload_json, created_at`
	notificationColumns        = `id, project_id, loop_id, run_id, entity_type, entity_id, channel, level, title, subtitle, body, status, dedupe_key, error_message, payload_json, sent_at, created_at, updated_at`
	lockColumns                = `key, owner, reason, expires_at, created_at, updated_at`
	queueItemColumns           = `id, project_id, loop_id, type, target_type, target_id, repo, pr_number, dedupe_key, priority, status, available_at, attempts, max_attempts, claimed_by, claimed_at, started_at, finished_at, lock_key, payload_json, last_error, last_error_kind, created_at, updated_at`
	worktreeColumns            = `id, project_id, repo_path, worktree_path, branch, base_branch, status, head_sha, metadata_json, created_at, updated_at, cleaned_at`
)

// qualifiedColumns qualifies every identifier in a projection for a query
// using an aliased table (for example, "r.id, r.loop_id, ...").
func qualifiedColumns(alias, columns string) string {
	parts := strings.Split(columns, ", ")
	for index := range parts {
		parts[index] = alias + "." + parts[index]
	}
	return strings.Join(parts, ", ")
}
