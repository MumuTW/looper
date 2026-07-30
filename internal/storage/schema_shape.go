package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ErrSchemaIncompatible marks every refusal to run against a database this
// build cannot prove it understands, whether the evidence is the migration
// ledger (ValidateCompatibility) or the tables themselves
// (ValidateSchemaShape).
//
// It is a sentinel rather than a message convention because the caller acts on
// it: looperd exits with a distinct code so a supervisor restarting the daemon
// every few seconds can tell "this will never succeed" apart from a transient
// start failure. #188 is what that distinction costs when it is missing — nine
// minutes of identical fatal exits that read as flakiness.
var ErrSchemaIncompatible = errors.New("database schema is incompatible with this binary")

// scannedTableColumns is the column shape this build reads, per table, in the
// order its scan helper consumes them.
//
// Every read in this package selects `*` and scans positionally into a fixed
// argument list, so the shape is not documentation: a database whose table has
// one more column than the build expects fails at the first read with
// `sql: expected 15 destination arguments in Scan, not 14`, from whichever
// call site happened to read first. That is what #188 observed — 28 fatal
// exits in nine minutes, one every launchd restart interval, with nothing in
// the message naming a schema, a table, or a migration.
//
// ValidateSchemaShape turns that into one line at boot. Keeping the lists here
// rather than deriving them from the scan helpers is deliberate: a derived
// check could only ever agree with the code, while a written-down shape
// disagrees with a database that drifted, which is the whole point.
//
// Adding a column to one of these tables therefore means updating this map;
// TestScannedTableColumnsMatchMigratedSchema fails otherwise.
var scannedTableColumns = map[string][]string{
	"projects": {"id", "name", "repo_path", "base_branch", "archived", "metadata_json", "created_at", "updated_at"},
	"loops": {"id", "seq", "project_id", "type", "target_type", "target_id", "repo", "pr_number", "status",
		"config_json", "metadata_json", "last_run_at", "next_run_at", "created_at", "updated_at"},
	"runs": {"id", "loop_id", "status", "current_step", "last_completed_step", "checkpoint_json", "summary",
		"error_message", "started_at", "last_heartbeat_at", "ended_at", "created_at", "updated_at",
		"agent_snapshot_json", "seq"},
	"queue_items": {"id", "project_id", "loop_id", "type", "target_type", "target_id", "repo", "pr_number",
		"dedupe_key", "priority", "status", "available_at", "attempts", "max_attempts", "claimed_by", "claimed_at",
		"started_at", "finished_at", "lock_key", "payload_json", "last_error", "last_error_kind", "created_at",
		"updated_at"},
	"event_logs": {"id", "event_type", "project_id", "loop_id", "run_id", "entity_type", "entity_id",
		"correlation_id", "causation_id", "actor_type", "actor_id", "actor_display_name", "payload_json",
		"created_at"},
	"worktrees": {"id", "project_id", "repo_path", "worktree_path", "branch", "base_branch", "status", "head_sha",
		"metadata_json", "created_at", "updated_at", "cleaned_at"},
	"notifications": {"id", "project_id", "loop_id", "run_id", "entity_type", "entity_id", "channel", "level",
		"title", "subtitle", "body", "status", "dedupe_key", "error_message", "payload_json", "sent_at",
		"created_at", "updated_at"},
	"pull_request_snapshots": {"id", "project_id", "repo", "pr_number", "head_sha", "base_sha", "title", "body",
		"author", "diff_ref", "checks_summary", "unresolved_thread_count", "review_state", "payload_json",
		"captured_at", "created_at"},
	"locks":            {"key", "owner", "reason", "expires_at", "created_at", "updated_at"},
	"agent_executions": {"id", "project_id", "loop_id", "run_id", "vendor", "status", "pid", "command_json", "cwd", "summary", "parse_status", "completion_signal", "heartbeat_count", "last_heartbeat_at", "output_json", "error_message", "started_at", "ended_at", "metadata_json", "created_at", "updated_at", "native_session_id", "native_resume_mode", "native_resume_status", "native_resume_error"},
}

// SchemaShapeMismatch names one table whose columns differ from what this build
// scans, with both sides spelled out so an operator can act without opening
// SQLite.
type SchemaShapeMismatch struct {
	Table string
	// Unexpected are columns the database has and this build does not know.
	Unexpected []string
	// Missing are columns this build scans and the database does not have.
	Missing []string
}

func (m SchemaShapeMismatch) String() string {
	parts := make([]string, 0, 2)
	if len(m.Unexpected) > 0 {
		parts = append(parts, "unknown column(s) "+strings.Join(m.Unexpected, ", "))
	}
	if len(m.Missing) > 0 {
		parts = append(parts, "missing column(s) "+strings.Join(m.Missing, ", "))
	}
	return m.Table + " has " + strings.Join(parts, " and ")
}

// ValidateSchemaShape compares the column shape of every table this build reads
// against the database's actual shape.
//
// It complements the migration-id check in ValidateCompatibility rather than
// duplicating it. That check answers "was this database migrated by a build
// newer than mine?" from the migration ledger, and so is blind to a column
// added outside the migration framework, or to a ledger that agrees while the
// tables do not — the open question #188 raised about its own incident. This
// check reads the tables themselves, so it holds whatever wrote them.
//
// A table absent from the database is not a mismatch: this runs before the
// database is guaranteed to be migrated (package.autoMigrateOnStartup may be
// off), and an unmigrated database is the migration runner's business, not a
// shape violation. Only a table that exists with the wrong columns is one.
func ValidateSchemaShape(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return nil
	}
	tables := make([]string, 0, len(scannedTableColumns))
	for table := range scannedTableColumns {
		tables = append(tables, table)
	}
	sort.Strings(tables)

	mismatches := make([]string, 0)
	for _, table := range tables {
		actual, err := tableColumns(ctx, db, table)
		if err != nil {
			return err
		}
		if len(actual) == 0 {
			continue
		}
		mismatch := compareTableShape(table, scannedTableColumns[table], actual)
		if mismatch == nil {
			continue
		}
		mismatches = append(mismatches, mismatch.String())
	}
	if len(mismatches) == 0 {
		return nil
	}
	return fmt.Errorf(
		"%w: %s; this build was compiled against a different schema — install a build that matches the database, or restore a backup taken with this build",
		ErrSchemaIncompatible,
		strings.Join(mismatches, "; "),
	)
}

func compareTableShape(table string, expected, actual []string) *SchemaShapeMismatch {
	expectedSet := make(map[string]struct{}, len(expected))
	for _, column := range expected {
		expectedSet[column] = struct{}{}
	}
	actualSet := make(map[string]struct{}, len(actual))
	for _, column := range actual {
		actualSet[column] = struct{}{}
	}

	mismatch := SchemaShapeMismatch{Table: table}
	for _, column := range actual {
		if _, ok := expectedSet[column]; !ok {
			mismatch.Unexpected = append(mismatch.Unexpected, column)
		}
	}
	for _, column := range expected {
		if _, ok := actualSet[column]; !ok {
			mismatch.Missing = append(mismatch.Missing, column)
		}
	}
	if len(mismatch.Unexpected) == 0 && len(mismatch.Missing) == 0 {
		return nil
	}
	return &mismatch
}

// tableColumns returns the table's columns in declaration order, or nil when
// the table does not exist.
func tableColumns(ctx context.Context, db *sql.DB, table string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT name FROM pragma_table_info(?) ORDER BY cid`, table)
	if err != nil {
		return nil, fmt.Errorf("read %s column shape: %w", table, err)
	}
	defer rows.Close()

	columns := make([]string, 0)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan %s column shape: %w", table, err)
		}
		columns = append(columns, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate %s column shape: %w", table, err)
	}
	return columns, nil
}
