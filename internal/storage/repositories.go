package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/MumuTW/looper/internal/domain"
)

var ErrQueueItemNotActive = errors.New("queue item not active")

// ErrLoopHumanHeld rejects blind writes that would change human takeover
// ownership. Only the loops service may cross this boundary after validating a
// status transition.
var ErrLoopHumanHeld = errors.New("loop is held by human takeover")

func IsLoopHumanHeldError(err error) bool { return errors.Is(err, ErrLoopHumanHeld) }

// ErrAgentExecutionConflict is returned when an agent_executions upsert is
// rejected by the terminal-observation immutability guard (zero rows updated).
// Callers must not treat this as success.
var ErrAgentExecutionConflict = errors.New("agent execution observation conflict")

// IsActiveAgentExecutionStatus reports whether status is a live (non-terminal)
// observation. Active statuses may be updated freely; terminal rows cannot
// regress back into these values.
func IsActiveAgentExecutionStatus(status string) bool {
	switch status {
	case "running", "cancelling":
		return true
	default:
		return false
	}
}

// IsTerminalAgentExecutionStatus reports whether status is a durable terminal
// observation. Terminal status is immutable once written: no terminal→active
// and no terminal→other-terminal transitions. Same-terminal field enrichment
// (e.g. native resume metadata) is allowed; see AgentExecutionsRepository.Upsert.
func IsTerminalAgentExecutionStatus(status string) bool {
	switch status {
	case "completed", "failed", "timeout", "killed", "success":
		return true
	default:
		return false
	}
}

type sqliteQuerier interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type Repositories struct {
	q                    sqliteQuerier
	Projects             *ProjectsRepository
	Loops                *LoopsRepository
	Runs                 *RunsRepository
	AgentExecutions      *AgentExecutionsRepository
	PullRequestSnapshots *PullRequestSnapshotsRepository
	Events               *EventsRepository
	Locks                *LocksRepository
	Queue                *QueueRepository
	Notifications        *NotificationsRepository
	Worktrees            *WorktreesRepository
	WebhookForwarders    *WebhookForwardersRepository
	WebhookTunnelHooks   *WebhookTunnelHooksRepository
	FeishuThreads        *FeishuThreadsRepository
	PlannerWorkGraphs    *PlannerWorkGraphsRepository
}

func NewRepositories(q sqliteQuerier) *Repositories {
	return &Repositories{
		q:                    q,
		Projects:             &ProjectsRepository{q: q},
		Loops:                &LoopsRepository{q: q},
		Runs:                 &RunsRepository{q: q},
		AgentExecutions:      &AgentExecutionsRepository{q: q},
		PullRequestSnapshots: &PullRequestSnapshotsRepository{q: q},
		Events:               &EventsRepository{q: q},
		Locks:                &LocksRepository{q: q, now: time.Now},
		Queue:                &QueueRepository{q: q},
		Notifications:        &NotificationsRepository{q: q},
		Worktrees:            &WorktreesRepository{q: q},
		WebhookForwarders:    &WebhookForwardersRepository{q: q},
		WebhookTunnelHooks:   &WebhookTunnelHooksRepository{q: q},
		FeishuThreads:        &FeishuThreadsRepository{q: q},
		PlannerWorkGraphs:    &PlannerWorkGraphsRepository{q: q},
	}
}

// WithTransaction runs a repository operation against one SQLite transaction.
// Event-log projections that must be observed together (for example a Gate
// report and its terminal agreement) use this boundary so a partial append
// cannot become the durable source of truth.
func (r *Repositories) WithTransaction(ctx context.Context, fn func(*Repositories) error) error {
	if r == nil || r.q == nil {
		return fmt.Errorf("repository transaction storage is not configured")
	}
	if fn == nil {
		return fmt.Errorf("repository transaction callback is nil")
	}
	if _, ok := r.q.(*sql.Tx); ok {
		return fn(r)
	}
	db, ok := r.q.(txBeginner)
	if !ok {
		return fmt.Errorf("repository transaction storage is not transactional")
	}
	return WithTransaction(ctx, db, nil, func(tx *sql.Tx) error {
		return fn(NewRepositories(tx))
	})
}

// RetireQueuedLoop pauses a queued loop and cancels its selected queued item
// in one transaction. It returns false when either record stopped being queued
// before the transaction established its status guard.
func (r *Repositories) RetireQueuedLoop(ctx context.Context, loopID, queueID, updatedAt string, reason *string) (bool, error) {
	db, ok := r.q.(txBeginner)
	if !ok {
		return false, fmt.Errorf("queue retirement requires transactional storage")
	}
	return WithTransactionValue(ctx, db, nil, func(tx *sql.Tx) (bool, error) {
		repos := NewRepositories(tx)
		loop, err := repos.Loops.GetByID(ctx, loopID)
		if err != nil {
			return false, err
		}
		item, err := repos.Queue.GetByID(ctx, queueID)
		if err != nil {
			return false, err
		}
		if loop == nil || item == nil || item.LoopID == nil || loop.Status != "queued" || item.Status != "queued" || *item.LoopID != loopID {
			return false, nil
		}
		loop.Status = "paused"
		loop.NextRunAt = nil
		loop.UpdatedAt = updatedAt
		if err := repos.Loops.Upsert(ctx, *loop); err != nil {
			return false, err
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE queue_items
			SET status = 'cancelled', finished_at = ?, last_error = COALESCE(?, last_error), updated_at = ?
			WHERE id = ? AND loop_id = ? AND status = 'queued'
		`, updatedAt, reason, updatedAt, queueID, loopID)
		if err != nil {
			return false, fmt.Errorf("retire queued queue item: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return false, fmt.Errorf("read retired queue item rows affected: %w", err)
		}
		if affected != 1 {
			return false, fmt.Errorf("retire queued queue item %s: status changed during retirement", queueID)
		}
		return true, nil
	})
}

// ProjectRecord is the Project: a durable local registration binding one
// repository to one Provider and supplying the project-level policy Roles
// consume. This SQLite record is the runtime Authority for whether a Project
// exists and for its repository/Provider binding; [[projects]] is a startup
// import, not a parallel runtime Authority.
type ProjectRecord struct {
	ID           string
	Name         string
	RepoPath     string
	BaseBranch   *string
	Archived     bool
	MetadataJSON *string
	CreatedAt    string
	UpdatedAt    string
}

type LoopRecord struct {
	ID           string
	Seq          int64
	ProjectID    string
	Type         string
	TargetType   string
	TargetID     *string
	Repo         *string
	PRNumber     *int64
	Status       string
	ConfigJSON   *string
	MetadataJSON *string
	LastRunAt    *string
	NextRunAt    *string
	CreatedAt    string
	UpdatedAt    string
}

type RunRecord struct {
	ID                string
	LoopID            string
	Status            string
	CurrentStep       *string
	LastCompletedStep *string
	CheckpointJSON    *string
	Summary           *string
	ErrorMessage      *string
	StartedAt         string
	LastHeartbeatAt   *string
	EndedAt           *string
	CreatedAt         string
	UpdatedAt         string
	AgentSnapshotJSON *string // durable agent identity; set on create, immutable after insert
	Seq               int64   // storage-owned monotonic insertion sequence; assigned on insert, immutable
}

type AgentExecutionRecord struct {
	ID                 string
	ProjectID          *string
	LoopID             *string
	RunID              *string
	Vendor             string
	Status             string
	PID                *int64
	CommandJSON        *string
	CWD                *string
	Summary            *string
	ParseStatus        *string
	CompletionSignal   *string
	HeartbeatCount     int64
	LastHeartbeatAt    *string
	OutputJSON         *string
	ErrorMessage       *string
	NativeSessionID    *string
	NativeResumeMode   *string
	NativeResumeStatus *string
	NativeResumeError  *string
	StartedAt          string
	EndedAt            *string
	MetadataJSON       *string
	CreatedAt          string
	UpdatedAt          string
}

type PullRequestSnapshotRecord struct {
	ID                    string
	ProjectID             string
	Repo                  string
	PRNumber              int64
	HeadSHA               string
	BaseSHA               *string
	Title                 *string
	Body                  *string
	Author                *string
	DiffRef               *string
	ChecksSummary         *string
	UnresolvedThreadCount *int64
	ReviewState           *string
	PayloadJSON           *string
	CapturedAt            string
	CreatedAt             string
}

type EventLogRecord struct {
	ID               string
	EventType        string
	ProjectID        *string
	LoopID           *string
	RunID            *string
	EntityType       *string
	EntityID         *string
	CorrelationID    *string
	CausationID      *string
	ActorType        *string
	ActorID          *string
	ActorDisplayName *string
	PayloadJSON      string
	CreatedAt        string
}

type LockRecord struct {
	Key       string
	Owner     string
	Reason    *string
	ExpiresAt string
	CreatedAt string
	UpdatedAt string
}

type QueueItemRecord struct {
	ID            string
	ProjectID     *string
	LoopID        *string
	Type          string
	TargetType    string
	TargetID      string
	Repo          *string
	PRNumber      *int64
	DedupeKey     string
	Priority      int64
	Status        string
	AvailableAt   string
	Attempts      int64
	MaxAttempts   int64
	ClaimedBy     *string
	ClaimedAt     *string
	StartedAt     *string
	FinishedAt    *string
	LockKey       *string
	PayloadJSON   *string
	LastError     *string
	LastErrorKind *string
	CreatedAt     string
	UpdatedAt     string
}

type QueueStats struct {
	TotalQueued                      int64
	EligibleQueued                   int64
	BlockedByTerminalOrPausedLoop    int64
	BlockedByLockKey                 int64
	BlockedByReviewerFixerDependency int64
	ScheduledForFuture               int64
	StaleQueued                      int64
}

type QueueMarkRetryInput struct {
	ID           string
	AvailableAt  string
	Attempts     int64
	ErrorMessage *string
	ErrorKind    string
	UpdatedAt    string
}

type QueueFailInput struct {
	ID           string
	Attempts     int64
	FinishedAt   string
	ErrorMessage *string
	ErrorKind    string
	UpdatedAt    string
}

type NotificationRecord struct {
	ID           string
	ProjectID    *string
	LoopID       *string
	RunID        *string
	EntityType   *string
	EntityID     *string
	Channel      string
	Level        string
	Title        string
	Subtitle     *string
	Body         string
	Status       string
	DedupeKey    *string
	ErrorMessage *string
	PayloadJSON  *string
	SentAt       *string
	CreatedAt    string
	UpdatedAt    string
}

type WorktreeRecord struct {
	ID           string
	ProjectID    string
	RepoPath     string
	WorktreePath string
	Branch       string
	BaseBranch   *string
	Status       string
	HeadSHA      *string
	MetadataJSON *string
	CreatedAt    string
	UpdatedAt    string
	CleanedAt    *string
}

type NotificationsRepository struct{ q sqliteQuerier }

func (r *NotificationsRepository) Upsert(ctx context.Context, record NotificationRecord) error {
	_, err := r.q.ExecContext(ctx, `
		INSERT INTO notifications (id, project_id, loop_id, run_id, entity_type, entity_id, channel, level, title, subtitle, body, status, dedupe_key, error_message, payload_json, sent_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			project_id=excluded.project_id,
			loop_id=excluded.loop_id,
			run_id=excluded.run_id,
			entity_type=excluded.entity_type,
			entity_id=excluded.entity_id,
			channel=excluded.channel,
			level=excluded.level,
			title=excluded.title,
			subtitle=excluded.subtitle,
			body=excluded.body,
			status=excluded.status,
			dedupe_key=excluded.dedupe_key,
			error_message=excluded.error_message,
			payload_json=excluded.payload_json,
			sent_at=excluded.sent_at,
			updated_at=excluded.updated_at
	`, record.ID, record.ProjectID, record.LoopID, record.RunID, record.EntityType, record.EntityID, record.Channel, record.Level, record.Title, record.Subtitle, record.Body, record.Status, record.DedupeKey, record.ErrorMessage, record.PayloadJSON, record.SentAt, record.CreatedAt, record.UpdatedAt)
	if err != nil {
		return fmt.Errorf("upsert notification: %w", err)
	}

	return nil
}

func (r *NotificationsRepository) GetByID(ctx context.Context, id string) (*NotificationRecord, error) {
	row := r.q.QueryRowContext(ctx, `SELECT `+notificationColumns+` FROM notifications WHERE id = ?`, id)
	record, err := scanNotification(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get notification by id: %w", err)
	}

	return &record, nil
}

func (r *NotificationsRepository) List(ctx context.Context, limit int64) ([]NotificationRecord, error) {
	if limit <= 0 {
		limit = 100
	}

	rows, err := r.q.QueryContext(ctx, `SELECT `+notificationColumns+` FROM notifications ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list notifications: %w", err)
	}
	defer rows.Close()

	return scanNotifications(rows)
}

func (r *NotificationsRepository) GetLatestByDedupe(ctx context.Context, channel, dedupeKey string) (*NotificationRecord, error) {
	row := r.q.QueryRowContext(ctx, `SELECT `+notificationColumns+` FROM notifications WHERE channel = ? AND dedupe_key = ? ORDER BY created_at DESC LIMIT 1`, channel, dedupeKey)
	record, err := scanNotification(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get latest notification by dedupe: %w", err)
	}

	return &record, nil
}

// ListByDedupe returns every durable delivery attempt for a dedupe key. The
// digest retry path uses the complete history so a successful provider write
// remains authoritative even when the following sent-marker append failed.
func (r *NotificationsRepository) ListByDedupe(ctx context.Context, dedupeKey string) ([]NotificationRecord, error) {
	rows, err := r.q.QueryContext(ctx, `SELECT * FROM notifications WHERE dedupe_key = ? ORDER BY created_at ASC`, dedupeKey)
	if err != nil {
		return nil, fmt.Errorf("list notifications by dedupe: %w", err)
	}
	defer rows.Close()

	return scanNotifications(rows)
}

// FeishuThreadsRepository maps a Feishu message thread root to the loop whose
// notifications thread under it, in both directions.
type FeishuThreadsRepository struct{ q sqliteQuerier }

// Upsert records that rootMessageID is the thread root for loopID in chatID.
func (r *FeishuThreadsRepository) Upsert(ctx context.Context, rootMessageID, loopID, chatID, createdAt string) error {
	_, err := r.q.ExecContext(ctx, `
		INSERT INTO feishu_threads (root_message_id, loop_id, chat_id, created_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(root_message_id) DO UPDATE SET
			loop_id=excluded.loop_id,
			chat_id=excluded.chat_id
	`, rootMessageID, loopID, chatID, createdAt)
	if err != nil {
		return fmt.Errorf("upsert feishu thread: %w", err)
	}
	return nil
}

// RootByLoop returns the most recent thread root for a loop, or "" when none.
func (r *FeishuThreadsRepository) RootByLoop(ctx context.Context, loopID string) (string, error) {
	var rootMessageID string
	err := r.q.QueryRowContext(ctx, `SELECT root_message_id FROM feishu_threads WHERE loop_id = ? ORDER BY created_at DESC LIMIT 1`, loopID).Scan(&rootMessageID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("feishu thread root by loop: %w", err)
	}
	return rootMessageID, nil
}

// LoopByRoot returns the loop id a thread root belongs to, or "" when unknown.
func (r *FeishuThreadsRepository) LoopByRoot(ctx context.Context, rootMessageID string) (string, error) {
	var loopID string
	err := r.q.QueryRowContext(ctx, `SELECT loop_id FROM feishu_threads WHERE root_message_id = ?`, rootMessageID).Scan(&loopID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("feishu thread loop by root: %w", err)
	}
	return loopID, nil
}

type EventsRepository struct{ q sqliteQuerier }

// TriageSourceStateRecord is a bounded projection of authoritative triage
// lifecycle payloads. Projected and Retired mean that at least one terminal
// event exists for SourceKey; event timestamps do not establish terminality.
type TriageSourceStateRecord struct {
	SourceKey      string
	EnrollmentJSON *string
	ReportJSON     *string
	Projected      bool
	Retired        bool
}

const triageSourceKeySQL = `CASE event_type
	WHEN 'triage.enrolled' THEN json_extract(payload_json, '$.idempotencyKey')
	WHEN 'triage.report' THEN json_extract(payload_json, '$.idempotencyKey')
	WHEN 'triage.confirmed' THEN json_extract(payload_json, '$.reportKey')
	WHEN 'triage.routed' THEN json_extract(payload_json, '$.reportKey')
	WHEN 'triage.retired' THEN json_extract(payload_json, '$.enrollmentKey')
END`

const triageLifecyclePredicateSQL = `entity_type = 'github_issue'
	AND event_type IN ('triage.enrolled', 'triage.report', 'triage.confirmed', 'triage.routed', 'triage.retired')`

func (r *EventsRepository) Append(ctx context.Context, record EventLogRecord) error {
	_, err := r.q.ExecContext(ctx, `
		INSERT INTO event_logs (
			id, event_type, project_id, loop_id, run_id, entity_type, entity_id,
			correlation_id, causation_id, actor_type, actor_id, actor_display_name,
			payload_json, created_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, record.ID, record.EventType, record.ProjectID, record.LoopID, record.RunID, record.EntityType, record.EntityID, record.CorrelationID, record.CausationID, record.ActorType, record.ActorID, record.ActorDisplayName, record.PayloadJSON, record.CreatedAt)
	if err != nil {
		return fmt.Errorf("append event log: %w", err)
	}

	return nil
}

func (r *EventsRepository) List(ctx context.Context, limit int64) ([]EventLogRecord, error) {
	if limit <= 0 {
		limit = 100
	}

	rows, err := r.q.QueryContext(ctx, `SELECT `+eventLogColumns+` FROM event_logs ORDER BY created_at DESC, rowid DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list event logs: %w", err)
	}
	defer rows.Close()

	return scanEventLogs(rows)
}

// ListAll returns the complete durable event stream in insertion order. Audit
// projections must not silently lose evidence because an unrelated busy period
// filled an arbitrary recent-event window.
func (r *EventsRepository) ListAll(ctx context.Context) ([]EventLogRecord, error) {
	rows, err := r.q.QueryContext(ctx, `SELECT * FROM event_logs ORDER BY created_at ASC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("list all event logs: %w", err)
	}
	defer rows.Close()

	return scanEventLogs(rows)
}

func (r *EventsRepository) ListSince(ctx context.Context, sinceISO string) ([]EventLogRecord, error) {
	rows, err := r.q.QueryContext(ctx, `SELECT `+eventLogColumns+` FROM event_logs WHERE created_at >= ? ORDER BY created_at DESC, rowid DESC`, sinceISO)
	if err != nil {
		return nil, fmt.Errorf("list event logs since: %w", err)
	}
	defer rows.Close()

	return scanEventLogs(rows)
}

func (r *EventsRepository) ListByEntity(ctx context.Context, entityType, entityID string) ([]EventLogRecord, error) {
	rows, err := r.q.QueryContext(ctx, `SELECT `+eventLogColumns+` FROM event_logs WHERE entity_type = ? AND entity_id = ? ORDER BY created_at ASC, rowid ASC`, entityType, entityID)
	if err != nil {
		return nil, fmt.Errorf("list event logs by entity: %w", err)
	}
	defer rows.Close()

	return scanEventLogs(rows)
}

// ListByEntityAndEventTypes reads only the requested event types for one entity.
// Callers that project a single durable event (e.g. the Reviewer review marker)
// use this to avoid loading the entity's entire event history on every poll.
func (r *EventsRepository) ListByEntityAndEventTypes(ctx context.Context, entityType, entityID string, eventTypes []string) ([]EventLogRecord, error) {
	if len(eventTypes) == 0 {
		return []EventLogRecord{}, nil
	}
	args := make([]any, 0, len(eventTypes)+2)
	args = append(args, entityType, entityID)
	for _, eventType := range eventTypes {
		args = append(args, eventType)
	}
	rows, err := r.q.QueryContext(ctx, `SELECT `+eventLogColumns+` FROM event_logs WHERE entity_type = ? AND entity_id = ? AND event_type IN (`+sqlPlaceholders(len(eventTypes))+`) ORDER BY created_at ASC, rowid ASC`, args...)
	if err != nil {
		return nil, fmt.Errorf("list event logs by entity and event types: %w", err)
	}
	defer rows.Close()

	return scanEventLogs(rows)
}

// ListByEventType returns the newest durable events of one type. The optional
// project filter is applied in SQLite so a read-only projection cannot load an
// unrelated project's full event history into the daemon before applying its
// limit.
func (r *EventsRepository) ListByEventType(ctx context.Context, eventType, projectID string, limit int64) ([]EventLogRecord, error) {
	if strings.TrimSpace(eventType) == "" {
		return []EventLogRecord{}, nil
	}
	if limit <= 0 {
		limit = 100
	}

	query := "SELECT " + eventLogColumns + " FROM event_logs WHERE event_type = ?"
	args := []any{eventType}
	if strings.TrimSpace(projectID) != "" {
		query += " AND project_id = ?"
		args = append(args, projectID)
	}
	query += " ORDER BY created_at DESC, rowid DESC LIMIT ?"
	args = append(args, limit)

	rows, err := r.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list event logs by type: %w", err)
	}
	defer rows.Close()

	return scanEventLogs(rows)
}

// ListLatestByEventType returns the newest event for each durable entity in an
// event family. The optional project filter is applied before the window so a
// read-only projection can bound its scan without loading another project's
// history. Entity identity includes project_id because the same pull request
// can be observed by more than one registered project.
func (r *EventsRepository) ListLatestByEventType(ctx context.Context, eventType, projectID string, limit int64) ([]EventLogRecord, error) {
	if strings.TrimSpace(eventType) == "" {
		return []EventLogRecord{}, nil
	}
	if limit <= 0 {
		limit = 100
	}

	query := `SELECT ` + eventLogColumns + `
		FROM (
			SELECT ` + eventLogColumns + `, rowid AS event_rowid,
				ROW_NUMBER() OVER (
					PARTITION BY COALESCE(project_id, ''), COALESCE(entity_id, '')
					ORDER BY created_at DESC, rowid DESC
				) AS event_rank
			FROM event_logs
			WHERE event_type = ?`
	args := []any{eventType}
	if strings.TrimSpace(projectID) != "" {
		query += " AND project_id = ?"
		args = append(args, projectID)
	}
	query += `
		) AS ranked
		WHERE event_rank = 1
		ORDER BY created_at DESC, event_rowid DESC
		LIMIT ?`
	args = append(args, limit)

	rows, err := r.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list latest event logs by type: %w", err)
	}
	defer rows.Close()

	return scanEventLogs(rows)
}

// ListByEntityTypeAndEventTypes reads the complete lifecycle for one entity
// family without imposing an arbitrary status-page limit. Callers derive live
// projections from the returned durable events; this query does not record a
// second materialized status.
func (r *EventsRepository) ListByEntityTypeAndEventTypes(ctx context.Context, entityType string, eventTypes []string) ([]EventLogRecord, error) {
	if len(eventTypes) == 0 {
		return []EventLogRecord{}, nil
	}
	args := make([]any, 0, len(eventTypes)+1)
	args = append(args, entityType)
	for _, eventType := range eventTypes {
		args = append(args, eventType)
	}
	rows, err := r.q.QueryContext(ctx, `SELECT `+eventLogColumns+` FROM event_logs WHERE entity_type = ? AND event_type IN (`+sqlPlaceholders(len(eventTypes))+`) ORDER BY created_at ASC, rowid ASC`, args...)
	if err != nil {
		return nil, fmt.Errorf("list event logs by entity type and event types: %w", err)
	}
	defer rows.Close()

	return scanEventLogs(rows)
}

// ListLatestByEntityTypeAndEventTypes returns one record per entity: the most
// recent matching event for each distinct (project, entity) pair.
//
// Callers that only want the current state of each entity must not pay for its
// whole history. An entity is re-evaluated repeatedly, so its lifecycle grows
// with the lifetime of the daemon while the projection derived from it grows
// only with the number of entities; selecting the latest row in SQLite keeps
// the cost on the second curve, and nothing is decoded that is then discarded.
//
// The partition is (project_id, entity_id), not entity_id alone. An entity ID
// like `owner/repo#12` is unique only within one project: two projects can
// point at the same slug on different provider base URLs, and partitioning on
// the ID alone would silently drop one of them.
//
// projectID scopes the query to a single project; empty means every project.
func (r *EventsRepository) ListLatestByEntityTypeAndEventTypes(ctx context.Context, projectID, entityType string, eventTypes []string) ([]EventLogRecord, error) {
	if len(eventTypes) == 0 {
		return []EventLogRecord{}, nil
	}
	args := make([]any, 0, len(eventTypes)+2)
	args = append(args, entityType)
	for _, eventType := range eventTypes {
		args = append(args, eventType)
	}
	projectFilter := ""
	if strings.TrimSpace(projectID) != "" {
		projectFilter = " AND project_id = ?"
		args = append(args, projectID)
	}
	rows, err := r.q.QueryContext(ctx, `
		SELECT * FROM event_logs WHERE id IN (
			SELECT id FROM (
				-- rowid breaks the tie, not id: event ids are random hex, so two
				-- events written in the same millisecond would otherwise pick a
				-- winner at random. rowid is insertion order, which is what
				-- "latest" means when the clock cannot separate them.
				SELECT id, ROW_NUMBER() OVER (
					PARTITION BY project_id, entity_id ORDER BY created_at DESC, rowid DESC
				) AS row_rank
				FROM event_logs
				WHERE entity_type = ? AND event_type IN (`+sqlPlaceholders(len(eventTypes))+`)`+projectFilter+`
			) WHERE row_rank = 1
		)
		ORDER BY created_at ASC, rowid ASC
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("list latest event logs by entity type and event types: %w", err)
	}
	defer rows.Close()

	return scanEventLogs(rows)
}

// ListFirstEventTimestampsByType returns, for each supplied entity ID that has
// an event of eventType, the created_at of its earliest such event. Callers
// need both the durable evidence and when it was first written, so one pass
// yields both rather than letting a second query disagree with the first.
func (r *EventsRepository) ListFirstEventTimestampsByType(ctx context.Context, eventType, entityType string, entityIDs []string) (map[string]string, error) {
	matched := make(map[string]string)
	if len(entityIDs) == 0 {
		return matched, nil
	}
	for _, chunk := range chunkStrings(entityIDs, sqliteMaxVariables-2) {
		args := make([]any, 0, len(chunk)+2)
		args = append(args, eventType, entityType)
		for _, entityID := range chunk {
			args = append(args, entityID)
		}
		rows, err := r.q.QueryContext(ctx, `
			SELECT entity_id, MIN(created_at)
			FROM event_logs
			WHERE event_type = ? AND entity_type = ?
			AND entity_id IN (`+sqlPlaceholders(len(chunk))+`)
			GROUP BY entity_id
		`, args...)
		if err != nil {
			return nil, fmt.Errorf("list entity ids by event type: %w", err)
		}
		for rows.Next() {
			var entityID string
			var firstAt sql.NullString
			if err := rows.Scan(&entityID, &firstAt); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scan entity id by event type: %w", err)
			}
			matched[entityID] = firstAt.String
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("iterate entity ids by event type: %w", err)
		}
		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf("close entity ids by event type rows: %w", err)
		}
	}
	return matched, nil
}

func (r *EventsRepository) ListByProjectAndEntityType(ctx context.Context, projectID, entityType string) ([]EventLogRecord, error) {
	rows, err := r.q.QueryContext(ctx, `SELECT `+eventLogColumns+` FROM event_logs WHERE project_id = ? AND entity_type = ? ORDER BY created_at ASC, rowid ASC`, projectID, entityType)
	if err != nil {
		return nil, fmt.Errorf("list event logs by project and entity type: %w", err)
	}
	defer rows.Close()

	return scanEventLogs(rows)
}

// ListTriageSourceStates returns lifecycle state for the requested source
// keys. The JSON payload key is the source identity authority; the expression
// index only makes that authority directly addressable.
func (r *EventsRepository) ListTriageSourceStates(ctx context.Context, projectID string, sourceKeys []string) ([]TriageSourceStateRecord, error) {
	if len(sourceKeys) == 0 {
		return []TriageSourceStateRecord{}, nil
	}
	records := make([]TriageSourceStateRecord, 0, len(sourceKeys))
	for _, chunk := range chunkStrings(sourceKeys, sqliteMaxVariables-1) {
		args := make([]any, 0, len(chunk)+1)
		args = append(args, projectID)
		for _, sourceKey := range chunk {
			args = append(args, sourceKey)
		}
		rows, err := r.q.QueryContext(ctx, `
			SELECT `+triageSourceKeySQL+` AS source_key,
				MIN(CASE WHEN event_type = 'triage.enrolled' THEN payload_json END),
				MIN(CASE WHEN event_type = 'triage.report' THEN payload_json END),
				MAX(event_type = 'triage.routed'),
				MAX(event_type = 'triage.retired')
			FROM event_logs INDEXED BY idx_event_logs_triage_source_key
			WHERE project_id = ? AND `+triageLifecyclePredicateSQL+`
				AND `+triageSourceKeySQL+` IN (`+sqlPlaceholders(len(chunk))+`)
			GROUP BY source_key
		`, args...)
		if err != nil {
			return nil, fmt.Errorf("list triage source states: %w", err)
		}
		chunkRecords, err := scanTriageSourceStates(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, chunkRecords...)
	}
	return records, nil
}

// ListAwaitingTriageSourceStates returns one current lifecycle row per source
// that has not reached confirmation, routing, or retirement. The event log is
// append-only, so status readers must aggregate by source key in SQLite rather
// than reread every historical event on each poll. The returned projection is
// still authoritative only for the report and terminal-event evidence it
// contains; it does not create a second lifecycle record.
func (r *EventsRepository) ListAwaitingTriageSourceStates(ctx context.Context, projectID string) ([]TriageSourceStateRecord, error) {
	args := []any{}
	projectFilter := ""
	if strings.TrimSpace(projectID) != "" {
		projectFilter = " AND project_id = ?"
		args = append(args, projectID)
	}
	rows, err := r.q.QueryContext(ctx, `
		SELECT `+triageSourceKeySQL+` AS source_key,
			MIN(CASE WHEN event_type = 'triage.enrolled' THEN payload_json END),
			MIN(CASE WHEN event_type = 'triage.report' THEN payload_json END),
			MAX(event_type = 'triage.routed'),
			MAX(event_type = 'triage.retired')
		FROM event_logs INDEXED BY idx_event_logs_triage_source_key
		WHERE `+triageLifecyclePredicateSQL+projectFilter+`
		GROUP BY source_key
		HAVING MAX(CASE WHEN event_type IN ('triage.confirmed', 'triage.routed', 'triage.retired') THEN 1 ELSE 0 END) = 0
		ORDER BY source_key
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("list awaiting triage source states: %w", err)
	}
	return scanTriageSourceStates(rows)
}

// ListTriageSourceStateWindow returns at most limit source lifecycles, rotating
// after afterSourceKey and wrapping once. Terminal sources intentionally count
// against the window: asking SQL to skip unbounded terminal history until it
// found limit pending rows would leave each tick's database work unbounded.
func (r *EventsRepository) ListTriageSourceStateWindow(ctx context.Context, projectID, afterSourceKey string, limit int) ([]TriageSourceStateRecord, error) {
	if limit <= 0 {
		return []TriageSourceStateRecord{}, nil
	}
	records, err := r.listTriageSourceStateRange(ctx, projectID, afterSourceKey, ">", limit)
	if err != nil {
		return nil, err
	}
	if afterSourceKey == "" || len(records) == limit {
		return records, nil
	}
	wrapped, err := r.listTriageSourceStateRange(ctx, projectID, afterSourceKey, "<=", limit-len(records))
	if err != nil {
		return nil, err
	}
	return append(records, wrapped...), nil
}

func (r *EventsRepository) listTriageSourceStateRange(ctx context.Context, projectID, cursor, comparison string, limit int) ([]TriageSourceStateRecord, error) {
	if comparison != ">" && comparison != "<=" {
		return nil, fmt.Errorf("unsupported triage source cursor comparison %q", comparison)
	}
	rows, err := r.q.QueryContext(ctx, `
		SELECT `+triageSourceKeySQL+` AS source_key,
			MIN(CASE WHEN event_type = 'triage.enrolled' THEN payload_json END),
			MIN(CASE WHEN event_type = 'triage.report' THEN payload_json END),
			MAX(event_type = 'triage.routed'),
			MAX(event_type = 'triage.retired')
		FROM event_logs INDEXED BY idx_event_logs_triage_source_key
		WHERE project_id = ? AND `+triageLifecyclePredicateSQL+`
			AND `+triageSourceKeySQL+` `+comparison+` ?
		GROUP BY source_key
		ORDER BY source_key
		LIMIT ?
	`, projectID, cursor, limit)
	if err != nil {
		return nil, fmt.Errorf("list triage source state window: %w", err)
	}
	return scanTriageSourceStates(rows)
}

func scanTriageSourceStates(rows *sql.Rows) ([]TriageSourceStateRecord, error) {
	defer rows.Close()
	records := []TriageSourceStateRecord{}
	for rows.Next() {
		var record TriageSourceStateRecord
		var enrollmentJSON, reportJSON sql.NullString
		var projected, retired int
		if err := rows.Scan(&record.SourceKey, &enrollmentJSON, &reportJSON, &projected, &retired); err != nil {
			return nil, fmt.Errorf("scan triage source state: %w", err)
		}
		if enrollmentJSON.Valid {
			value := enrollmentJSON.String
			record.EnrollmentJSON = &value
		}
		if reportJSON.Valid {
			value := reportJSON.String
			record.ReportJSON = &value
		}
		record.Projected = projected != 0
		record.Retired = retired != 0
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate triage source states: %w", err)
	}
	return records, nil
}

type ProjectsRepository struct{ q sqliteQuerier }

type PlannerWorkGraphsRepository struct{ q sqliteQuerier }

func (r *PlannerWorkGraphsRepository) Create(ctx context.Context, record PlannerWorkGraphRecord) error {
	_, err := r.q.ExecContext(ctx, `
		INSERT INTO planner_work_graphs (
			id, project_id, parent_repo, parent_issue_number, planner_loop_id,
			base_branch, status, replan_reason, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, record.ID, record.ProjectID, record.ParentRepo, record.ParentIssueNumber,
		record.PlannerLoopID, record.BaseBranch, record.Status, record.ReplanReason,
		record.CreatedAt, record.UpdatedAt)
	if err != nil {
		return fmt.Errorf("create planner work graph: %w", err)
	}
	return nil
}

func (r *PlannerWorkGraphsRepository) GetByPlannerLoopID(ctx context.Context, plannerLoopID string) (*PlannerWorkGraphRecord, error) {
	row := r.q.QueryRowContext(ctx, `SELECT * FROM planner_work_graphs WHERE planner_loop_id = ?`, plannerLoopID)
	record, err := scanPlannerWorkGraph(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get planner work graph by planner loop: %w", err)
	}
	return &record, nil
}

func (r *PlannerWorkGraphsRepository) GetByID(ctx context.Context, graphID string) (*PlannerWorkGraphRecord, error) {
	row := r.q.QueryRowContext(ctx, `SELECT * FROM planner_work_graphs WHERE id = ?`, graphID)
	record, err := scanPlannerWorkGraph(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get planner work graph: %w", err)
	}
	return &record, nil
}

func (r *PlannerWorkGraphsRepository) UpdateStatus(ctx context.Context, graphID, status string, replanReason *string, updatedAt string) error {
	_, err := r.q.ExecContext(ctx, `UPDATE planner_work_graphs SET status = ?, replan_reason = ?, updated_at = ? WHERE id = ?`, status, replanReason, updatedAt, graphID)
	if err != nil {
		return fmt.Errorf("update planner work graph status: %w", err)
	}
	return nil
}

func (r *PlannerWorkGraphsRepository) DeleteByID(ctx context.Context, graphID string) error {
	_, err := r.q.ExecContext(ctx, `DELETE FROM planner_work_graphs WHERE id = ?`, graphID)
	if err != nil {
		return fmt.Errorf("delete planner work graph: %w", err)
	}
	return nil
}

func (r *PlannerWorkGraphsRepository) UpdateNodeBranch(ctx context.Context, graphID, nodeKey, branch, updatedAt string) error {
	_, err := r.q.ExecContext(ctx, `
		UPDATE planner_work_graph_nodes SET branch = ?, updated_at = ?
		WHERE graph_id = ? AND node_key = ?
	`, branch, updatedAt, graphID, nodeKey)
	if err != nil {
		return fmt.Errorf("update planner work graph node branch: %w", err)
	}
	return nil
}

func (r *PlannerWorkGraphsRepository) CreateNode(ctx context.Context, record PlannerWorkGraphNodeRecord) error {
	_, err := r.q.ExecContext(ctx, `
		INSERT INTO planner_work_graph_nodes (
			graph_id, node_key, goal, acceptance_criteria_json, expected_pr_scope,
			worker_loop_id, branch, state, blocked_reason, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, record.GraphID, record.NodeKey, record.Goal, record.AcceptanceCriteriaJSON,
		record.ExpectedPRScope, record.WorkerLoopID, record.Branch, record.State,
		record.BlockedReason, record.CreatedAt, record.UpdatedAt)
	if err != nil {
		return fmt.Errorf("create planner work graph node: %w", err)
	}
	return nil
}

func (r *PlannerWorkGraphsRepository) CreateDependency(ctx context.Context, graphID, nodeKey, dependsOnKey string) error {
	_, err := r.q.ExecContext(ctx, `INSERT INTO planner_work_graph_dependencies (graph_id, node_key, depends_on_key) VALUES (?, ?, ?)`, graphID, nodeKey, dependsOnKey)
	if err != nil {
		return fmt.Errorf("create planner work graph dependency: %w", err)
	}
	return nil
}

func (r *PlannerWorkGraphsRepository) ListNodes(ctx context.Context, graphID string) ([]PlannerWorkGraphNodeRecord, error) {
	rows, err := r.q.QueryContext(ctx, `SELECT * FROM planner_work_graph_nodes WHERE graph_id = ? ORDER BY node_key ASC`, graphID)
	if err != nil {
		return nil, fmt.Errorf("list planner work graph nodes: %w", err)
	}
	defer rows.Close()
	return scanPlannerWorkGraphNodes(rows)
}

func (r *PlannerWorkGraphsRepository) ListDependencies(ctx context.Context, graphID, nodeKey string) ([]string, error) {
	rows, err := r.q.QueryContext(ctx, `SELECT depends_on_key FROM planner_work_graph_dependencies WHERE graph_id = ? AND node_key = ? ORDER BY depends_on_key ASC`, graphID, nodeKey)
	if err != nil {
		return nil, fmt.Errorf("list planner work graph dependencies: %w", err)
	}
	defer rows.Close()
	keys := make([]string, 0)
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("scan planner work graph dependency: %w", err)
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate planner work graph dependencies: %w", err)
	}
	return keys, nil
}

func (r *PlannerWorkGraphsRepository) GetNodeByWorkerLoopID(ctx context.Context, workerLoopID string) (*PlannerWorkGraphNodeRecord, error) {
	row := r.q.QueryRowContext(ctx, `SELECT * FROM planner_work_graph_nodes WHERE worker_loop_id = ?`, workerLoopID)
	record, err := scanPlannerWorkGraphNode(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get planner work graph node by worker loop: %w", err)
	}
	return &record, nil
}

// TransitionNode atomically claims or settles a node from exactly one expected
// state. A false result is a normal duplicate delivery, not a successful
// transition.
func (r *PlannerWorkGraphsRepository) TransitionNode(ctx context.Context, graphID, nodeKey, from, to string, blockedReason *string, updatedAt string) (bool, error) {
	result, err := r.q.ExecContext(ctx, `
		UPDATE planner_work_graph_nodes
		SET state = ?, blocked_reason = ?, updated_at = ?
		WHERE graph_id = ? AND node_key = ? AND state = ?
	`, to, blockedReason, updatedAt, graphID, nodeKey, from)
	if err != nil {
		return false, fmt.Errorf("transition planner work graph node: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read planner work graph node transition rows: %w", err)
	}
	return affected == 1, nil
}

func (r *PlannerWorkGraphsRepository) BlockPendingChildrenAfterFailure(ctx context.Context, graphID, dependsOnKey, reason, updatedAt string) error {
	_, err := r.q.ExecContext(ctx, `
		UPDATE planner_work_graph_nodes SET blocked_reason = ?, updated_at = ?
		WHERE graph_id = ? AND state = 'pending' AND node_key IN (
			SELECT node_key FROM planner_work_graph_dependencies WHERE graph_id = ? AND depends_on_key = ?
		)
	`, reason, updatedAt, graphID, graphID, dependsOnKey)
	if err != nil {
		return fmt.Errorf("record blocked graph children: %w", err)
	}
	return nil
}

func (r *ProjectsRepository) Upsert(ctx context.Context, record ProjectRecord) error {
	_, err := r.q.ExecContext(ctx, `
		INSERT INTO projects (id, name, repo_path, base_branch, archived, metadata_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name=excluded.name,
			repo_path=excluded.repo_path,
			base_branch=excluded.base_branch,
			archived=excluded.archived,
			metadata_json=excluded.metadata_json,
			updated_at=excluded.updated_at
	`, record.ID, record.Name, record.RepoPath, record.BaseBranch, boolToInt(record.Archived), record.MetadataJSON, record.CreatedAt, record.UpdatedAt)
	if err != nil {
		return fmt.Errorf("upsert project: %w", err)
	}

	return nil
}

func (r *ProjectsRepository) GetByID(ctx context.Context, id string) (*ProjectRecord, error) {
	row := r.q.QueryRowContext(ctx, `SELECT `+projectColumns+` FROM projects WHERE id = ?`, id)
	record, err := scanProject(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get project by id: %w", err)
	}

	return &record, nil
}

func (r *ProjectsRepository) List(ctx context.Context) ([]ProjectRecord, error) {
	rows, err := r.q.QueryContext(ctx, `SELECT `+projectColumns+` FROM projects ORDER BY updated_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()

	return scanProjects(rows)
}

func (r *ProjectsRepository) Archive(ctx context.Context, id string, updatedAt string) (bool, error) {
	result, err := r.q.ExecContext(ctx, `UPDATE projects SET archived = 1, updated_at = ? WHERE id = ? AND archived = 0`, updatedAt, id)
	if err != nil {
		return false, fmt.Errorf("archive project: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("archive project rows affected: %w", err)
	}

	return rowsAffected > 0, nil
}

type LoopsRepository struct{ q sqliteQuerier }

func (r *LoopsRepository) Upsert(ctx context.Context, record LoopRecord) error {
	return r.upsertWithIssueClaimAdmission(ctx, record, false, false)
}

// UpsertChangingHumanHold is reserved for lifecycle operations that have
// already established authority through domain transition validation.
func (r *LoopsRepository) UpsertChangingHumanHold(ctx context.Context, record LoopRecord) error {
	return r.upsertWithIssueClaimAdmission(ctx, record, true, false)
}

// UpsertForcingIssueClaimAdmission is the explicit operator override for a
// manual loop create with force=true. It bypasses only source-issue admission;
// the human-takeover write guard still applies.
func (r *LoopsRepository) UpsertForcingIssueClaimAdmission(ctx context.Context, record LoopRecord) error {
	return r.upsertWithIssueClaimAdmission(ctx, record, false, true)
}

// upsertWithIssueClaimAdmission makes source-issue admission part of loop
// publication itself. Any new or changed conflicting-active source issue claim
// is checked in the same SQLite transaction as its write, covering create,
// rediscovery, recovery, resume, and retargeting without relying on individual
// callers to remember it.
func (r *LoopsRepository) upsertWithIssueClaimAdmission(ctx context.Context, record LoopRecord, changeHumanHold, forceIssueClaim bool) error {
	if db, ok := r.q.(txBeginner); ok {
		return WithTransaction(ctx, db, nil, func(tx *sql.Tx) error {
			return NewRepositories(tx).Loops.upsertWithIssueClaimAdmission(ctx, record, changeHumanHold, forceIssueClaim)
		})
	}
	current, err := r.GetByID(ctx, record.ID)
	if err != nil {
		return err
	}
	requiresAdmission, err := r.issueClaimAdmissionRequired(ctx, current, record)
	if err != nil {
		return err
	}
	forced, err := r.issueClaimAdmissionIsForced(ctx, record)
	if err != nil {
		return err
	}
	if !forceIssueClaim && !forced && requiresAdmission {
		if err := r.AssertIssueClaimAdmission(ctx, record, false); err != nil {
			return err
		}
	}
	return r.upsert(ctx, record, changeHumanHold)
}

// LoopForcesIssueClaimAdmission reports the durable operator override carried
// by a manually forced worker lifecycle. It is intentionally part of the
// worker's metadata so opening or adopting its pull request cannot silently
// forget the authority that admitted the source issue in the first place.
func LoopForcesIssueClaimAdmission(record LoopRecord) bool {
	if record.Type != string(domain.LoopTypeWorker) || record.MetadataJSON == nil {
		return false
	}
	var metadata struct {
		Worker struct {
			IssueClaimOverride bool `json:"issueClaimOverride"`
		} `json:"worker"`
	}
	return json.Unmarshal([]byte(*record.MetadataJSON), &metadata) == nil && metadata.Worker.IssueClaimOverride
}

// issueClaimAdmissionIsForced resolves the operator authority for a candidate
// loop. A forced worker retains the override in its metadata; its reviewer and
// fixer descendants inherit that authority through the persisted worker which
// originated their pull request. The source worker is the authority, not the
// descendant agent's output.
func (r *LoopsRepository) issueClaimAdmissionIsForced(ctx context.Context, record LoopRecord) (bool, error) {
	if LoopForcesIssueClaimAdmission(record) {
		return true, nil
	}
	if record.Type != string(domain.LoopTypeFixer) && record.Type != string(domain.LoopTypeReviewer) {
		return false, nil
	}
	worker, err := r.sourceWorkerForPullRequest(ctx, record)
	if err != nil || worker == nil {
		return false, err
	}
	return LoopForcesIssueClaimAdmission(*worker), nil
}

func (r *LoopsRepository) issueClaimAdmissionRequired(ctx context.Context, current *LoopRecord, candidate LoopRecord) (bool, error) {
	if !domain.IsConflictingActiveLoopStatus(domain.LoopStatus(candidate.Status)) {
		return false, nil
	}
	if current == nil || !domain.IsConflictingActiveLoopStatus(domain.LoopStatus(current.Status)) {
		return true, nil
	}

	currentClaim, currentClaimsIssue, err := r.issueClaimForLoop(ctx, *current)
	if err != nil {
		return false, err
	}
	candidateClaim, candidateClaimsIssue, err := r.issueClaimForLoop(ctx, candidate)
	if err != nil {
		return false, err
	}
	if !candidateClaimsIssue {
		return false, nil
	}
	if !currentClaimsIssue {
		return true, nil
	}
	return currentClaim.issueNumber != candidateClaim.issueNumber || !strings.EqualFold(currentClaim.repo, candidateClaim.repo), nil
}

func (r *LoopsRepository) upsert(ctx context.Context, record LoopRecord, changeHumanHold bool) error {
	insertSource := `VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	args := []any{record.ID, record.Seq, record.ProjectID, record.Type, record.TargetType, record.TargetID, record.Repo, record.PRNumber, record.Status, record.ConfigJSON, record.MetadataJSON, record.LastRunAt, record.NextRunAt, record.CreatedAt, record.UpdatedAt}
	guard := ""
	if !changeHumanHold {
		guard = ` WHERE (loops.status = ?) = (excluded.status = ?)`
		args = append(args, string(domain.LoopStatusHumanTakeover), string(domain.LoopStatusHumanTakeover))
	}
	result, err := r.q.ExecContext(ctx, `
		INSERT INTO loops (id, seq, project_id, type, target_type, target_id, repo, pr_number, status, config_json, metadata_json, last_run_at, next_run_at, created_at, updated_at)
		`+insertSource+`
		ON CONFLICT(id) DO UPDATE SET
			seq=excluded.seq,
			project_id=excluded.project_id,
			type=excluded.type,
			target_type=excluded.target_type,
			target_id=excluded.target_id,
			repo=excluded.repo,
			pr_number=excluded.pr_number,
			status=excluded.status,
			config_json=excluded.config_json,
			metadata_json=excluded.metadata_json,
			last_run_at=excluded.last_run_at,
			next_run_at=excluded.next_run_at,
			updated_at=excluded.updated_at`+guard+`
	`, args...)
	if err != nil {
		return fmt.Errorf("upsert loop: %w", err)
	}
	if !changeHumanHold {
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("upsert loop: read rows affected: %w", err)
		}
		if affected == 0 {
			return fmt.Errorf("%w: loop %s", ErrLoopHumanHeld, record.ID)
		}
	}

	_, err = r.q.ExecContext(ctx, `
		INSERT INTO counters (name, value)
		VALUES ('loop_seq', ?)
		ON CONFLICT(name) DO UPDATE SET value =
			CASE WHEN excluded.value > counters.value THEN excluded.value ELSE counters.value END
	`, record.Seq)
	if err != nil {
		return fmt.Errorf("upsert loop counter: %w", err)
	}

	return nil
}

func (r *LoopsRepository) GetByID(ctx context.Context, id string) (*LoopRecord, error) {
	row := r.q.QueryRowContext(ctx, `SELECT `+loopColumns+` FROM loops WHERE id = ?`, id)
	record, err := scanLoop(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get loop by id: %w", err)
	}

	return &record, nil
}

func (r *LoopsRepository) GetBySeq(ctx context.Context, seq int64) (*LoopRecord, error) {
	row := r.q.QueryRowContext(ctx, `SELECT `+loopColumns+` FROM loops WHERE seq = ?`, seq)
	record, err := scanLoop(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get loop by seq: %w", err)
	}

	return &record, nil
}

func (r *LoopsRepository) AllocateSeq(ctx context.Context) (int64, error) {
	var existing int64
	err := r.q.QueryRowContext(ctx, `SELECT value FROM counters WHERE name = 'loop_seq'`).Scan(&existing)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("read loop counter: %w", err)
	}

	if errors.Is(err, sql.ErrNoRows) {
		var currentValue int64
		if maxErr := r.q.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) AS value FROM loops`).Scan(&currentValue); maxErr != nil {
			return 0, fmt.Errorf("read max loop seq: %w", maxErr)
		}
		if _, insertErr := r.q.ExecContext(ctx, `INSERT INTO counters (name, value) VALUES ('loop_seq', ?)`, currentValue); insertErr != nil {
			return 0, fmt.Errorf("seed loop counter: %w", insertErr)
		}
	}

	var next int64
	if err := r.q.QueryRowContext(ctx, `UPDATE counters SET value = value + 1 WHERE name = 'loop_seq' RETURNING value`).Scan(&next); err != nil {
		return 0, fmt.Errorf("allocate loop seq: %w", err)
	}

	return next, nil
}

// ListLoopsOptions filters and paginates loop listings.
// Empty Status/ProjectID match all. Limit 0 means no limit (full list).
// Offset is always applied when > 0, including when Limit == 0 (SQLite LIMIT -1 OFFSET n).
type ListLoopsOptions struct {
	Status    string
	ProjectID string
	Limit     int64
	Offset    int64
}

func (r *LoopsRepository) List(ctx context.Context) ([]LoopRecord, error) {
	return r.ListFiltered(ctx, ListLoopsOptions{})
}

func (r *LoopsRepository) ListFiltered(ctx context.Context, opts ListLoopsOptions) ([]LoopRecord, error) {
	query, args := buildLoopsListQuery(`SELECT `+loopColumns+` FROM loops`, opts, true)
	rows, err := r.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list loops filtered: %w", err)
	}
	defer rows.Close()

	return scanLoops(rows)
}

func (r *LoopsRepository) CountFiltered(ctx context.Context, opts ListLoopsOptions) (int64, error) {
	query, args := buildLoopsListQuery(`SELECT COUNT(*) FROM loops`, opts, false)
	var total int64
	if err := r.q.QueryRowContext(ctx, query, args...).Scan(&total); err != nil {
		return 0, fmt.Errorf("count loops filtered: %w", err)
	}
	return total, nil
}

func buildLoopsListQuery(base string, opts ListLoopsOptions, withOrderAndLimit bool) (string, []any) {
	var clauses []string
	var args []any
	if status := strings.TrimSpace(opts.Status); status != "" {
		clauses = append(clauses, "status = ?")
		args = append(args, status)
	}
	if projectID := strings.TrimSpace(opts.ProjectID); projectID != "" {
		clauses = append(clauses, "project_id = ?")
		args = append(args, projectID)
	}

	query := base
	if len(clauses) > 0 {
		query += " WHERE " + strings.Join(clauses, " AND ")
	}
	if withOrderAndLimit {
		query += " ORDER BY updated_at DESC, seq DESC"
		if opts.Limit > 0 {
			query += " LIMIT ?"
			args = append(args, opts.Limit)
			if opts.Offset > 0 {
				query += " OFFSET ?"
				args = append(args, opts.Offset)
			}
		} else if opts.Offset > 0 {
			// SQLite: LIMIT -1 means no upper bound, so OFFSET-only paging is honest.
			query += " LIMIT -1 OFFSET ?"
			args = append(args, opts.Offset)
		}
	}
	return query, args
}

func (r *LoopsRepository) ListByRepoAndPR(ctx context.Context, repo string, prNumber int64) ([]LoopRecord, error) {
	rows, err := r.q.QueryContext(ctx, `SELECT `+loopColumns+` FROM loops WHERE repo = ? COLLATE NOCASE AND pr_number = ? ORDER BY updated_at DESC, seq DESC`, repo, prNumber)
	if err != nil {
		return nil, fmt.Errorf("list loops by repository and pull request: %w", err)
	}
	defer rows.Close()

	return scanLoops(rows)
}

// IssueClaimConflictError identifies the existing live loop that prevents a
// source-issue claim from being published. It is derived from loops, rather
// than persisted separately, so callers must use it inside their publication
// transaction.
type IssueClaimConflictError struct {
	IssueNumber int64
	LoopID      string
	LoopType    string
}

func (e *IssueClaimConflictError) Error() string {
	return fmt.Sprintf("source issue #%d is occupied by active %s loop %s", e.IssueNumber, e.LoopType, e.LoopID)
}

func IsIssueClaimConflictError(err error) (*IssueClaimConflictError, bool) {
	var conflict *IssueClaimConflictError
	return conflict, errors.As(err, &conflict)
}

type issueClaim struct {
	repo         string
	issueNumber  int64
	sourceWorker string
}

// AssertIssueClaimAdmission enforces the source-issue admission invariant for
// a loop publication. It deliberately decodes metadata in Go: malformed
// metadata remains available to its owning recovery path, but an unrelated row
// cannot make SQLite JSON extraction fail an otherwise independent dispatch.
//
// The caller must invoke this in the same immediate SQLite transaction as the
// subsequent Upsert. force is the explicit operator bypass.
func (r *LoopsRepository) AssertIssueClaimAdmission(ctx context.Context, candidate LoopRecord, force bool) error {
	if force || !domain.IsConflictingActiveLoopStatus(domain.LoopStatus(candidate.Status)) {
		return nil
	}
	forced, err := r.issueClaimAdmissionIsForced(ctx, candidate)
	if err != nil {
		return err
	}
	if forced {
		return nil
	}
	claim, ok, err := r.issueClaimForLoop(ctx, candidate)
	if err != nil || !ok {
		return err
	}

	statuses := domain.ConflictingActiveLoopStatuses()
	args := make([]any, 0, len(statuses)+5)
	args = append(args, candidate.ProjectID, string(domain.LoopTypeWorker), string(domain.LoopTypeFixer), string(domain.LoopTypeReviewer))
	for _, status := range statuses {
		args = append(args, string(status))
	}
	rows, err := r.q.QueryContext(ctx, `SELECT `+loopColumns+` FROM loops
		WHERE project_id = ?
		  AND type IN (?, ?, ?)
		  AND status IN (`+sqlPlaceholders(len(statuses))+`)
		  AND target_type IN (?, ?)
		ORDER BY updated_at DESC, seq DESC`, append(args, string(domain.LoopTargetTypeIssue), string(domain.LoopTargetTypePullRequest))...)
	if err != nil {
		return fmt.Errorf("list active source issue claims: %w", err)
	}
	// Resolve PR descendants only after closing this cursor. Those resolutions
	// query loops again through the same SQLite transaction, which must not run
	// while this cursor retains the transaction connection.
	candidates, scanErr := scanLoops(rows)
	closeErr := rows.Close()
	if scanErr != nil {
		return scanErr
	}
	if closeErr != nil {
		return fmt.Errorf("close active source issue claims: %w", closeErr)
	}

	for _, loop := range candidates {
		if candidate.ID != "" && loop.ID == candidate.ID {
			continue
		}
		existingClaim, exists, claimErr := r.issueClaimForLoop(ctx, loop)
		if claimErr != nil {
			return claimErr
		}
		if !exists || existingClaim.issueNumber != claim.issueNumber || !strings.EqualFold(existingClaim.repo, claim.repo) {
			continue
		}
		// A source worker and its PR descendants are one lifecycle, not
		// competing claims. Different workers (or an issue worker with no PR
		// source) are competing source-issue publications.
		if claim.sourceWorker != "" && existingClaim.sourceWorker == claim.sourceWorker {
			continue
		}
		return &IssueClaimConflictError{IssueNumber: claim.issueNumber, LoopID: loop.ID, LoopType: loop.Type}
	}
	return nil
}

func (r *LoopsRepository) issueClaimForLoop(ctx context.Context, loop LoopRecord) (issueClaim, bool, error) {
	if loop.TargetType == string(domain.LoopTargetTypeIssue) && loop.Type == string(domain.LoopTypeWorker) {
		repo, number, ok := parseIssueTargetID(loopString(loop.TargetID))
		return issueClaim{repo: repo, issueNumber: number}, ok, nil
	}
	if loop.TargetType != string(domain.LoopTargetTypePullRequest) || (loop.Type != string(domain.LoopTypeWorker) && loop.Type != string(domain.LoopTypeFixer) && loop.Type != string(domain.LoopTypeReviewer)) {
		return issueClaim{}, false, nil
	}
	if loop.Type == string(domain.LoopTypeWorker) {
		repo, number, ok := workerIssueFromMetadata(loop.MetadataJSON, loopString(loop.Repo))
		if !ok {
			return issueClaim{}, false, nil
		}
		return issueClaim{repo: repo, issueNumber: number, sourceWorker: loop.ID}, true, nil
	}

	repo := loopString(loop.Repo)
	if repo == "" || loopInt64(loop.PRNumber) <= 0 {
		issueRepo, issueNumber, ok := workerIssueFromMetadata(loop.MetadataJSON, repo)
		return issueClaim{repo: issueRepo, issueNumber: issueNumber}, ok, nil
	}
	worker, err := r.sourceWorkerForPullRequest(ctx, loop)
	if err != nil {
		return issueClaim{}, false, err
	}
	if worker != nil {
		issueRepo, issueNumber, ok := workerIssueFromMetadata(worker.MetadataJSON, loopString(worker.Repo))
		if ok {
			return issueClaim{repo: issueRepo, issueNumber: issueNumber, sourceWorker: worker.ID}, true, nil
		}
	}
	// Legacy fixer/reviewer records kept worker metadata on the PR loop itself.
	// Keep those records admissible without making fresh lifecycle links depend
	// on that metadata shape.
	issueRepo, issueNumber, ok := workerIssueFromMetadata(loop.MetadataJSON, repo)
	return issueClaim{repo: issueRepo, issueNumber: issueNumber}, ok, nil
}

func (r *LoopsRepository) sourceWorkerForPullRequest(ctx context.Context, loop LoopRecord) (*LoopRecord, error) {
	repo := loopString(loop.Repo)
	prNumber := loopInt64(loop.PRNumber)
	if loop.TargetType != string(domain.LoopTargetTypePullRequest) || repo == "" || prNumber <= 0 {
		return nil, nil
	}
	if sourceWorkerID := sourceWorkerIDFromMetadata(loop.MetadataJSON); sourceWorkerID != "" {
		worker, err := r.GetByID(ctx, sourceWorkerID)
		if err != nil {
			return nil, err
		}
		if worker == nil || worker.ProjectID != loop.ProjectID || worker.Type != string(domain.LoopTypeWorker) || worker.TargetType != string(domain.LoopTargetTypePullRequest) || !strings.EqualFold(loopString(worker.Repo), repo) || loopInt64(worker.PRNumber) != prNumber {
			return nil, fmt.Errorf("source worker link %q no longer matches pull request %s#%d", sourceWorkerID, repo, prNumber)
		}
		return worker, nil
	}
	rows, err := r.q.QueryContext(ctx, `SELECT `+loopColumns+` FROM loops
		WHERE project_id = ? AND type = ? AND target_type = ?
		  AND repo = ? COLLATE NOCASE AND pr_number = ?
		ORDER BY updated_at DESC, seq DESC`, loop.ProjectID, string(domain.LoopTypeWorker), string(domain.LoopTargetTypePullRequest), repo, prNumber)
	if err != nil {
		return nil, fmt.Errorf("find source worker for pull request: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		worker, scanErr := scanLoop(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		if _, _, ok := workerIssueFromMetadata(worker.MetadataJSON, loopString(worker.Repo)); ok {
			return &worker, nil
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate source workers for pull request: %w", err)
	}
	return nil, nil
}

func sourceWorkerIDFromMetadata(metadataJSON *string) string {
	if metadataJSON == nil || strings.TrimSpace(*metadataJSON) == "" {
		return ""
	}
	var metadata struct {
		SourceWorkerID string `json:"sourceWorkerId"`
	}
	if err := json.Unmarshal([]byte(*metadataJSON), &metadata); err != nil {
		return ""
	}
	return strings.TrimSpace(metadata.SourceWorkerID)
}

func parseIssueTargetID(targetID string) (string, int64, bool) {
	trimmed := strings.TrimSpace(targetID)
	parts := strings.Split(strings.TrimPrefix(trimmed, "issue:"), ":")
	if len(parts) < 2 || !strings.HasPrefix(trimmed, "issue:") {
		return "", 0, false
	}
	var number int64
	if _, err := fmt.Sscan(parts[len(parts)-1], &number); err != nil || number <= 0 {
		return "", 0, false
	}
	repo := strings.TrimSpace(strings.Join(parts[:len(parts)-1], ":"))
	return repo, number, repo != ""
}

func workerIssueFromMetadata(metadataJSON *string, fallbackRepo string) (string, int64, bool) {
	if metadataJSON == nil || strings.TrimSpace(*metadataJSON) == "" {
		return "", 0, false
	}
	var metadata struct {
		Worker struct {
			IssueNumber int64  `json:"issueNumber"`
			IssueRepo   string `json:"issueRepo"`
			Repo        string `json:"repo"`
		} `json:"worker"`
	}
	if err := json.Unmarshal([]byte(*metadataJSON), &metadata); err != nil || metadata.Worker.IssueNumber <= 0 {
		return "", 0, false
	}
	repo := firstNonEmptyString(metadata.Worker.IssueRepo, metadata.Worker.Repo, fallbackRepo)
	return repo, metadata.Worker.IssueNumber, repo != ""
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func loopString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func loopInt64(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

func (r *LoopsRepository) ListByStatuses(ctx context.Context, statuses []string) ([]LoopRecord, error) {
	if len(statuses) == 0 {
		return []LoopRecord{}, nil
	}
	args := make([]any, 0, len(statuses))
	for _, status := range statuses {
		args = append(args, status)
	}
	rows, err := r.q.QueryContext(ctx, `SELECT `+loopColumns+` FROM loops WHERE status IN (`+sqlPlaceholders(len(statuses))+`) ORDER BY updated_at DESC, seq DESC`, args...)
	if err != nil {
		return nil, fmt.Errorf("list loops by statuses: %w", err)
	}
	defer rows.Close()

	return scanLoops(rows)
}

func (r *LoopsRepository) ListByIDs(ctx context.Context, ids []string) ([]LoopRecord, error) {
	if len(ids) == 0 {
		return []LoopRecord{}, nil
	}
	chunks := chunkStrings(ids, sqliteMaxVariables)
	items := make([]LoopRecord, 0, len(ids))
	for _, chunk := range chunks {
		args := make([]any, 0, len(chunk))
		for _, id := range chunk {
			args = append(args, id)
		}
		rows, err := r.q.QueryContext(ctx, `SELECT `+loopColumns+` FROM loops WHERE id IN (`+sqlPlaceholders(len(chunk))+`) ORDER BY updated_at DESC, seq DESC`, args...)
		if err != nil {
			return nil, fmt.Errorf("list loops by ids: %w", err)
		}
		chunkItems, scanErr := scanLoops(rows)
		closeErr := rows.Close()
		if scanErr != nil {
			return nil, scanErr
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close list loops by ids rows: %w", closeErr)
		}
		items = append(items, chunkItems...)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].UpdatedAt != items[j].UpdatedAt {
			return items[i].UpdatedAt > items[j].UpdatedAt
		}
		if items[i].Seq != items[j].Seq {
			return items[i].Seq > items[j].Seq
		}
		return items[i].ID > items[j].ID
	})
	return items, nil
}

func (r *LoopsRepository) CountByTypeAndStatus(ctx context.Context) (map[string]map[string]int64, error) {
	rows, err := r.q.QueryContext(ctx, `SELECT type, status, COUNT(*) FROM loops GROUP BY type, status`)
	if err != nil {
		return nil, fmt.Errorf("count loops by type and status: %w", err)
	}
	defer rows.Close()

	counts := map[string]map[string]int64{}
	for rows.Next() {
		var loopType string
		var status string
		var count int64
		if err := rows.Scan(&loopType, &status, &count); err != nil {
			return nil, fmt.Errorf("scan loop counts by type and status: %w", err)
		}
		if counts[loopType] == nil {
			counts[loopType] = map[string]int64{}
		}
		counts[loopType][status] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate loop counts by type and status: %w", err)
	}

	return counts, nil
}

func (r *LoopsRepository) TerminateByProject(ctx context.Context, projectID, updatedAt string) (int64, error) {
	if strings.TrimSpace(projectID) == "" {
		return 0, nil
	}

	result, err := r.q.ExecContext(ctx, `
		UPDATE loops
		SET status = 'terminated',
			next_run_at = NULL,
			updated_at = ?
		WHERE project_id = ? AND status IN ('idle', 'queued', 'running', 'paused', 'waiting', 'failed', 'interrupted')
	`, updatedAt, projectID)
	if err != nil {
		return 0, fmt.Errorf("terminate loops by project: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read terminate loops by project rows affected: %w", err)
	}

	return affected, nil
}

type RunsRepository struct{ q sqliteQuerier }

type AgentExecutionsRepository struct{ q sqliteQuerier }

type PullRequestSnapshotsRepository struct{ q sqliteQuerier }

type LocksRepository struct {
	q   sqliteQuerier
	now func() time.Time
}

func (r *RunsRepository) Upsert(ctx context.Context, record RunRecord) error {
	// agent_snapshot_json and seq are insert-only: ON CONFLICT must not overwrite
	// an existing snapshot or reassign the storage-owned insertion sequence.
	_, err := r.q.ExecContext(ctx, `
		INSERT INTO runs (id, loop_id, status, current_step, last_completed_step, checkpoint_json, summary, error_message, started_at, last_heartbeat_at, ended_at, created_at, updated_at, agent_snapshot_json, seq)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, (SELECT COALESCE(MAX(existing.seq), 0) + 1 FROM runs AS existing))
		ON CONFLICT(id) DO UPDATE SET
			status=excluded.status,
			current_step=excluded.current_step,
			last_completed_step=excluded.last_completed_step,
			checkpoint_json=excluded.checkpoint_json,
			summary=excluded.summary,
			error_message=excluded.error_message,
			started_at=excluded.started_at,
			last_heartbeat_at=excluded.last_heartbeat_at,
			ended_at=excluded.ended_at,
			updated_at=excluded.updated_at
	`, record.ID, record.LoopID, record.Status, record.CurrentStep, record.LastCompletedStep, record.CheckpointJSON, record.Summary, record.ErrorMessage, record.StartedAt, record.LastHeartbeatAt, record.EndedAt, record.CreatedAt, record.UpdatedAt, record.AgentSnapshotJSON)
	if err != nil {
		return fmt.Errorf("upsert run: %w", err)
	}

	return nil
}

// AppendCheckpointSecondaryIssue appends one issue to $.outcome.secondaryIssues.
//
// Append rather than read-modify-write for the same reason the other merges exist:
// terminal cleanup runs after the run is written, so writing back a whole outcome
// would erase whatever a concurrent transition stored. json_insert with '$[#]'
// appends to the existing array, and the outcome/array are created when absent.
//
// The CASE guard normalizes a non-object checkpoint_json to an empty object; see
// MergeRunResumePolicy for why the column cannot be trusted to hold one.
func (r *RunsRepository) AppendCheckpointSecondaryIssue(ctx context.Context, id, issueJSON, updatedAt string) error {
	result, err := r.q.ExecContext(ctx, `
		WITH base(doc) AS (
			SELECT CASE
				WHEN json_valid(checkpoint_json) AND json_type(checkpoint_json) = 'object'
					THEN checkpoint_json
				ELSE '{}'
			END
			FROM runs WHERE id = ?
		)
		UPDATE runs
		SET checkpoint_json = (
				SELECT json_set(
					doc,
					'$.outcome.secondaryIssues',
					json_insert(
						CASE
							WHEN json_type(doc, '$.outcome.secondaryIssues') = 'array'
								THEN json_extract(doc, '$.outcome.secondaryIssues')
							ELSE json('[]')
						END,
						'$[#]', json(?)
					)
				)
				FROM base
			),
			updated_at = ?
		WHERE id = ?
	`, id, issueJSON, updatedAt, id)
	if err != nil {
		return fmt.Errorf("append run checkpoint secondary issue: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read appended run checkpoint secondary issue rows: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("append run checkpoint secondary issue: run not found: %s", id)
	}
	return nil
}

// ClearTimeoutProgress removes the timeout-preservation evidence only after an
// operator has explicitly authorized discarding the associated worktree. The
// checkpoint otherwise remains intact so the next worker run keeps its work,
// plan, and worktree identity while no longer trying to preserve discarded
// progress.
func (r *RunsRepository) ClearTimeoutProgress(ctx context.Context, id, updatedAt string) error {
	result, err := r.q.ExecContext(ctx, `
		UPDATE runs
		SET checkpoint_json = CASE
				WHEN json_valid(checkpoint_json) AND json_type(checkpoint_json) = 'object'
					THEN CASE
						WHEN json_extract(checkpoint_json, '$.execution.status') = 'timeout_observing'
						THEN json_set(json_remove(checkpoint_json, '$.execution.progressBeforeTimeout', '$.execution.progressSnapshotError', '$.continuation'), '$.execution.status', 'timeout')
						ELSE json_remove(checkpoint_json, '$.execution.progressBeforeTimeout', '$.execution.progressSnapshotError', '$.continuation')
					END
				ELSE checkpoint_json
			END,
			updated_at = ?
		WHERE id = ?
	`, updatedAt, id)
	if err != nil {
		return fmt.Errorf("clear run timeout progress: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read cleared run timeout progress rows: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("clear run timeout progress: run not found: %s", id)
	}
	return nil
}

// MergeRunResumePolicy rewrites only the checkpoint's resume policy.
//
// The retry-policy writers read a run, change this one field, and would otherwise
// write the whole row back. That loses any checkpoint field a concurrent terminal
// cleanup persisted in between -- the mirror of the race
// MergeWorktreeCleanupTimestamps exists to avoid, since the two writers can
// interleave in either order.
//
// The CASE guard normalizes a non-object checkpoint_json to an empty object. Without
// it, failure-streak recovery on a run with malformed legacy content aborted the
// transaction, and a run whose checkpoint was null/an array/a scalar reported success
// without writing the policy -- leaving the loop paused after an apparently
// successful handoff.
func (r *RunsRepository) MergeRunResumePolicy(ctx context.Context, id, resumePolicy, updatedAt string) error {
	result, err := r.q.ExecContext(ctx, `
		UPDATE runs
		SET checkpoint_json = json_set(
				CASE
					WHEN json_valid(checkpoint_json) AND json_type(checkpoint_json) = 'object'
						THEN checkpoint_json
					ELSE '{}'
				END,
				'$.resumePolicy', ?
			),
			updated_at = ?
		WHERE id = ?
	`, resumePolicy, updatedAt, id)
	if err != nil {
		return fmt.Errorf("merge run resume policy: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read merged run resume policy rows: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("merge run resume policy: run not found: %s", id)
	}
	return nil
}

// MergeWorktreeCleanupTimestamps records a terminal worktree cleanup attempt on a
// run without disturbing anything else about it.
//
// Cleanup runs after the run reached a terminal status and starts from a checkpoint
// captured earlier, so it must not write that stale copy back. Replacing the whole
// checkpoint_json would erase a concurrent checkpoint transition — an operator
// retry rewriting resumePolicy to restart_from_discover between completion and
// cleanup, for instance, which would leave the requeued run resuming the invalid
// downstream checkpoint it was retried to escape. Scalar columns being untouched
// is not enough; the merge has to be inside the JSON too.
//
// json_set writes only the two cleanup fields, creating $.worktree if the stored
// checkpoint has none and preserving its other fields when it does.
//
// The CASE guard replaces a non-object checkpoint_json with an empty object first.
// The column is arbitrary text: malformed content makes json_set abort the statement,
// and valid-but-non-object content (null, an array, a scalar) makes it return the
// input unchanged while the UPDATE still reports a row affected -- a silent no-op.
// Normalizing matches what the previous read-modify-write path did in Go.
func (r *RunsRepository) MergeWorktreeCleanupTimestamps(ctx context.Context, id, cleanupAttemptedAt, cleanedAt, updatedAt string) error {
	result, err := r.q.ExecContext(ctx, `
		UPDATE runs
		SET checkpoint_json = json_set(
				CASE
					WHEN json_valid(checkpoint_json) AND json_type(checkpoint_json) = 'object'
						THEN checkpoint_json
					ELSE '{}'
				END,
				'$.worktree.cleanupAttemptedAt', ?,
				'$.worktree.cleanedAt', ?
			),
			updated_at = ?
		WHERE id = ?
	`, cleanupAttemptedAt, cleanedAt, updatedAt, id)
	if err != nil {
		return fmt.Errorf("merge run worktree cleanup timestamps: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read merged run worktree cleanup rows: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("merge run worktree cleanup timestamps: run not found: %s", id)
	}
	return nil
}

func (r *RunsRepository) GetByID(ctx context.Context, id string) (*RunRecord, error) {
	row := r.q.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE id = ?`, id)
	record, err := scanRun(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get run by id: %w", err)
	}

	return &record, nil
}

func (r *RunsRepository) ListByIDs(ctx context.Context, ids []string) ([]RunRecord, error) {
	if len(ids) == 0 {
		return []RunRecord{}, nil
	}
	runs := make([]RunRecord, 0, len(ids))
	for _, chunk := range chunkStrings(ids, sqliteMaxVariables) {
		args := make([]any, 0, len(chunk))
		for _, id := range chunk {
			args = append(args, id)
		}
		rows, err := r.q.QueryContext(ctx, `SELECT `+runColumns+` FROM runs WHERE id IN (`+sqlPlaceholders(len(chunk))+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("list runs by ids: %w", err)
		}
		chunkRuns, scanErr := scanRuns(rows)
		closeErr := rows.Close()
		if scanErr != nil {
			return nil, scanErr
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close list runs by ids rows: %w", closeErr)
		}
		runs = append(runs, chunkRuns...)
	}
	return runs, nil
}

// latestRunOrder is the single ordering authority for latest-run selection:
// timestamps first (millisecond precision), then the storage-owned monotonic
// seq so same-millisecond retries resolve to the newest inserted attempt.
func latestRunOrder(alias string) string {
	return alias + ".started_at DESC, " + alias + ".created_at DESC, " + alias + ".seq DESC"
}

// RunNewer is the Go-side mirror of latestRunOrder for callers that compare
// already-loaded records; keep both in sync. It reports whether candidate is
// strictly newer than current.
func RunNewer(candidate, current RunRecord) bool {
	if candidate.StartedAt != current.StartedAt {
		return candidate.StartedAt > current.StartedAt
	}
	if candidate.CreatedAt != current.CreatedAt {
		return candidate.CreatedAt > current.CreatedAt
	}
	return candidate.Seq > current.Seq
}

// SortRunsLatestFirst orders runs newest-first under the RunNewer authority.
func SortRunsLatestFirst(runs []RunRecord) {
	sort.SliceStable(runs, func(i, j int) bool {
		return RunNewer(runs[i], runs[j])
	})
}

// TouchHeartbeat advances a run's last_heartbeat_at without rewriting the
// record, and only forward: the fixed-precision ISO-8601 format compares
// lexicographically in chronological order, so a stale writer cannot move
// liveness evidence backward and no other column is disturbed.
func (r *RunsRepository) TouchHeartbeat(ctx context.Context, id, heartbeatAtISO string) error {
	_, err := r.q.ExecContext(ctx, `
		UPDATE runs
		SET last_heartbeat_at = ?, updated_at = ?
		WHERE id = ? AND (last_heartbeat_at IS NULL OR last_heartbeat_at < ?)
	`, heartbeatAtISO, heartbeatAtISO, id, heartbeatAtISO)
	if err != nil {
		return fmt.Errorf("touch run heartbeat: %w", err)
	}
	return nil
}

func (r *RunsRepository) GetLatestByLoopID(ctx context.Context, loopID string) (*RunRecord, error) {
	row := r.q.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE loop_id = ? ORDER BY `+latestRunOrder("runs")+` LIMIT 1`, loopID)
	record, err := scanRun(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get latest run by loop id: %w", err)
	}

	return &record, nil
}

func (r *RunsRepository) ListLatestByLoopIDs(ctx context.Context, loopIDs []string) ([]RunRecord, error) {
	if len(loopIDs) == 0 {
		return []RunRecord{}, nil
	}
	chunks := chunkStrings(loopIDs, sqliteMaxVariables)
	items := make([]RunRecord, 0, len(loopIDs))
	for _, chunk := range chunks {
		args := make([]any, 0, len(chunk))
		for _, loopID := range chunk {
			args = append(args, loopID)
		}
		rows, err := r.q.QueryContext(ctx, `
			SELECT `+qualifiedColumns("r", runColumns)+`
			FROM runs r
			WHERE r.id IN (
				SELECT (
					SELECT latest.id FROM runs latest
					WHERE latest.loop_id = candidates.loop_id
					ORDER BY `+latestRunOrder("latest")+`
					LIMIT 1
				)
				FROM (
					SELECT DISTINCT loop_id FROM runs
					WHERE loop_id IN (`+sqlPlaceholders(len(chunk))+`)
				) AS candidates
			)
			ORDER BY `+latestRunOrder("r")+`
		`, args...)
		if err != nil {
			return nil, fmt.Errorf("list latest runs by loop ids: %w", err)
		}
		chunkItems, scanErr := scanRuns(rows)
		closeErr := rows.Close()
		if scanErr != nil {
			return nil, scanErr
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close list latest runs by loop ids rows: %w", closeErr)
		}
		items = append(items, chunkItems...)
	}
	SortRunsLatestFirst(items)
	return items, nil
}

func (r *RunsRepository) ListLatestByLoopStatusesAndResumePolicy(ctx context.Context, statuses []string, resumePolicy string) ([]RunRecord, error) {
	if len(statuses) == 0 || strings.TrimSpace(resumePolicy) == "" {
		return []RunRecord{}, nil
	}
	args := make([]any, 0, len(statuses)+1)
	for _, status := range statuses {
		args = append(args, status)
	}
	args = append(args, resumePolicy)
	rows, err := r.q.QueryContext(ctx, `
		SELECT `+qualifiedColumns("r", runColumns)+`
		FROM runs r
		WHERE r.id IN (
			SELECT (
				SELECT latest.id FROM runs latest
				WHERE latest.loop_id = l.id
				ORDER BY `+latestRunOrder("latest")+`
				LIMIT 1
			)
			FROM loops l
			WHERE l.status IN (`+sqlPlaceholders(len(statuses))+`)
		)
		AND json_valid(r.checkpoint_json)
		AND json_extract(r.checkpoint_json, '$.resumePolicy') = ?
		ORDER BY `+latestRunOrder("r")+`
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("list latest runs by loop statuses and resume policy: %w", err)
	}
	defer rows.Close()

	return scanRuns(rows)
}

func (r *RunsRepository) HasRunningByLoopID(ctx context.Context, loopID string) (bool, error) {
	var count int64
	if err := r.q.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE loop_id = ? AND status = 'running'`, loopID).Scan(&count); err != nil {
		return false, fmt.Errorf("check running run by loop id: %w", err)
	}

	return count > 0, nil
}

func (r *RunsRepository) List(ctx context.Context) ([]RunRecord, error) {
	rows, err := r.q.QueryContext(ctx, `SELECT `+runColumns+` FROM runs ORDER BY started_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	defer rows.Close()

	return scanRuns(rows)
}

func (r *RunsRepository) CountByStatus(ctx context.Context) (map[string]int64, error) {
	rows, err := r.q.QueryContext(ctx, `SELECT status, COUNT(*) FROM runs GROUP BY status`)
	if err != nil {
		return nil, fmt.Errorf("count runs by status: %w", err)
	}
	defer rows.Close()

	counts := map[string]int64{}
	for rows.Next() {
		var status string
		var count int64
		if err := rows.Scan(&status, &count); err != nil {
			return nil, fmt.Errorf("scan run counts by status: %w", err)
		}
		counts[status] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate run counts by status: %w", err)
	}

	return counts, nil
}

func (r *RunsRepository) ListSince(ctx context.Context, sinceISO string) ([]RunRecord, error) {
	rows, err := r.q.QueryContext(ctx, `SELECT `+runColumns+` FROM runs WHERE started_at >= ? ORDER BY started_at DESC, id DESC`, sinceISO)
	if err != nil {
		return nil, fmt.Errorf("list runs since: %w", err)
	}
	defer rows.Close()

	return scanRuns(rows)
}

func (r *RunsRepository) ListByStatus(ctx context.Context, status string) ([]RunRecord, error) {
	rows, err := r.q.QueryContext(ctx, `SELECT `+runColumns+` FROM runs WHERE status = ? ORDER BY started_at DESC, id DESC`, status)
	if err != nil {
		return nil, fmt.Errorf("list runs by status: %w", err)
	}
	defer rows.Close()

	return scanRuns(rows)
}

func (r *RunsRepository) ListByLoop(ctx context.Context, loopID string) ([]RunRecord, error) {
	rows, err := r.q.QueryContext(ctx, `SELECT `+runColumns+` FROM runs WHERE loop_id = ? ORDER BY started_at DESC`, loopID)
	if err != nil {
		return nil, fmt.Errorf("list runs by loop: %w", err)
	}
	defer rows.Close()

	return scanRuns(rows)
}

// Upsert writes a durable agent_executions observation (ADR-0015 R5 / #578).
//
// Terminal immutability policy (status column):
//   - Active statuses: running, cancelling.
//   - Terminal statuses: completed, failed, timeout, killed, success (legacy).
//   - Active → active and active → terminal are allowed.
//   - Terminal → active is forbidden (stale live writer cannot regress).
//   - Terminal → different terminal is forbidden (first terminal wins).
//   - Terminal → same terminal is allowed for non-status field enrichment
//     (e.g. native-resume metadata) without changing the terminal outcome.
//
// Zero-row / rejected conflict updates return ErrAgentExecutionConflict, never
// nil success. Callsites must surface conflict and must not treat it as durable
// success for finalize/release paths.
func (r *AgentExecutionsRepository) Upsert(ctx context.Context, record AgentExecutionRecord) error {
	result, err := r.q.ExecContext(ctx, `
		INSERT INTO agent_executions (id, project_id, loop_id, run_id, vendor, status, pid, command_json, cwd, summary, parse_status, completion_signal, heartbeat_count, last_heartbeat_at, output_json, error_message, native_session_id, native_resume_mode, native_resume_status, native_resume_error, started_at, ended_at, metadata_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			project_id=excluded.project_id,
			loop_id=excluded.loop_id,
			run_id=excluded.run_id,
			vendor=excluded.vendor,
			status=excluded.status,
			pid=excluded.pid,
			command_json=excluded.command_json,
			cwd=excluded.cwd,
			summary=excluded.summary,
			parse_status=excluded.parse_status,
			completion_signal=excluded.completion_signal,
			heartbeat_count=excluded.heartbeat_count,
			last_heartbeat_at=excluded.last_heartbeat_at,
			output_json=excluded.output_json,
			error_message=excluded.error_message,
			native_session_id=excluded.native_session_id,
			native_resume_mode=excluded.native_resume_mode,
			native_resume_status=excluded.native_resume_status,
			native_resume_error=excluded.native_resume_error,
			started_at=excluded.started_at,
			ended_at=excluded.ended_at,
			metadata_json=excluded.metadata_json,
			updated_at=excluded.updated_at
		WHERE agent_executions.status IN ('running', 'cancelling')
		   OR (
				agent_executions.status NOT IN ('running', 'cancelling')
				AND excluded.status = agent_executions.status
		   )
	`, record.ID, record.ProjectID, record.LoopID, record.RunID, record.Vendor, record.Status, record.PID, record.CommandJSON, record.CWD, record.Summary, record.ParseStatus, record.CompletionSignal, record.HeartbeatCount, record.LastHeartbeatAt, record.OutputJSON, record.ErrorMessage, record.NativeSessionID, record.NativeResumeMode, record.NativeResumeStatus, record.NativeResumeError, record.StartedAt, record.EndedAt, record.MetadataJSON, record.CreatedAt, record.UpdatedAt)
	if err != nil {
		return fmt.Errorf("upsert agent execution: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("upsert agent execution rows affected: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("%w: id=%s status=%s", ErrAgentExecutionConflict, record.ID, record.Status)
	}

	return nil
}

func (r *AgentExecutionsRepository) GetByID(ctx context.Context, id string) (*AgentExecutionRecord, error) {
	row := r.q.QueryRowContext(ctx, `SELECT `+agentExecutionColumns+` FROM agent_executions WHERE id = ?`, id)
	record, err := scanAgentExecution(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get agent execution by id: %w", err)
	}

	return &record, nil
}

func (r *AgentExecutionsRepository) GetLatestByRunID(ctx context.Context, runID string) (*AgentExecutionRecord, error) {
	row := r.q.QueryRowContext(ctx, `SELECT `+agentExecutionColumns+` FROM agent_executions WHERE run_id = ? ORDER BY started_at DESC, id DESC LIMIT 1`, runID)
	record, err := scanAgentExecution(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get latest agent execution by run id: %w", err)
	}

	return &record, nil
}

func (r *AgentExecutionsRepository) GetLatestActiveByRunID(ctx context.Context, runID string) (*AgentExecutionRecord, error) {
	row := r.q.QueryRowContext(ctx, `SELECT `+agentExecutionColumns+` FROM agent_executions WHERE run_id = ? AND status IN ('running', 'cancelling') ORDER BY started_at DESC, id DESC LIMIT 1`, runID)
	record, err := scanAgentExecution(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get latest active agent execution by run id: %w", err)
	}

	return &record, nil
}

func (r *AgentExecutionsRepository) GetLatestByLoopID(ctx context.Context, loopID string) (*AgentExecutionRecord, error) {
	row := r.q.QueryRowContext(ctx, `SELECT `+agentExecutionColumns+` FROM agent_executions WHERE loop_id = ? ORDER BY started_at DESC, id DESC LIMIT 1`, loopID)
	record, err := scanAgentExecution(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get latest agent execution by loop id: %w", err)
	}

	return &record, nil
}

func (r *AgentExecutionsRepository) ListActive(ctx context.Context) ([]AgentExecutionRecord, error) {
	rows, err := r.q.QueryContext(ctx, `SELECT `+agentExecutionColumns+` FROM agent_executions WHERE status IN ('running', 'cancelling') ORDER BY started_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("list active agent executions: %w", err)
	}
	defer rows.Close()

	return scanAgentExecutions(rows)
}

func (r *AgentExecutionsRepository) List(ctx context.Context) ([]AgentExecutionRecord, error) {
	rows, err := r.q.QueryContext(ctx, `SELECT `+agentExecutionColumns+` FROM agent_executions ORDER BY started_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("list agent executions: %w", err)
	}
	defer rows.Close()

	return scanAgentExecutions(rows)
}

func (r *AgentExecutionsRepository) ListSince(ctx context.Context, sinceISO string) ([]AgentExecutionRecord, error) {
	rows, err := r.q.QueryContext(ctx, `SELECT `+agentExecutionColumns+` FROM agent_executions WHERE started_at >= ? ORDER BY started_at DESC, id DESC`, sinceISO)
	if err != nil {
		return nil, fmt.Errorf("list agent executions since: %w", err)
	}
	defer rows.Close()

	return scanAgentExecutions(rows)
}

func (r *PullRequestSnapshotsRepository) Upsert(ctx context.Context, record PullRequestSnapshotRecord) error {
	_, err := r.q.ExecContext(ctx, `
		INSERT INTO pull_request_snapshots (id, project_id, repo, pr_number, head_sha, base_sha, title, body, author, diff_ref, checks_summary, unresolved_thread_count, review_state, payload_json, captured_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			project_id=excluded.project_id,
			repo=excluded.repo,
			pr_number=excluded.pr_number,
			head_sha=excluded.head_sha,
			base_sha=excluded.base_sha,
			title=excluded.title,
			body=excluded.body,
			author=excluded.author,
			diff_ref=excluded.diff_ref,
			checks_summary=excluded.checks_summary,
			unresolved_thread_count=excluded.unresolved_thread_count,
			review_state=excluded.review_state,
			payload_json=excluded.payload_json,
			captured_at=excluded.captured_at
	`, record.ID, record.ProjectID, record.Repo, record.PRNumber, record.HeadSHA, record.BaseSHA, record.Title, record.Body, record.Author, record.DiffRef, record.ChecksSummary, record.UnresolvedThreadCount, record.ReviewState, record.PayloadJSON, record.CapturedAt, record.CreatedAt)
	if err != nil {
		return fmt.Errorf("upsert pull request snapshot: %w", err)
	}

	return nil
}

func (r *PullRequestSnapshotsRepository) List(ctx context.Context) ([]PullRequestSnapshotRecord, error) {
	rows, err := r.q.QueryContext(ctx, `SELECT `+pullRequestSnapshotColumns+` FROM pull_request_snapshots ORDER BY captured_at DESC, created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list pull request snapshots: %w", err)
	}
	defer rows.Close()

	return scanPullRequestSnapshots(rows)
}

// ListLatest returns one snapshot per project/repository/pull request. Snapshot
// captures are append-only, and callers that render current state should not
// materialize historical payloads (which may include a full diff) just to
// discard them in memory.
func (r *PullRequestSnapshotsRepository) ListLatest(ctx context.Context) ([]PullRequestSnapshotRecord, error) {
	rows, err := r.q.QueryContext(ctx, `
		WITH ranked AS (
			SELECT id, ROW_NUMBER() OVER (
				PARTITION BY project_id, lower(repo), pr_number
				-- IDs are random; rowid preserves insertion order when both
				-- millisecond timestamps tie.
				ORDER BY captured_at DESC, created_at DESC, rowid DESC
			) AS row_number
			FROM pull_request_snapshots
		)
		SELECT `+qualifiedColumns("snapshots", pullRequestSnapshotColumns)+`
		FROM pull_request_snapshots snapshots
		JOIN ranked ON ranked.id = snapshots.id
		WHERE ranked.row_number = 1
		ORDER BY snapshots.captured_at DESC, snapshots.created_at DESC, snapshots.rowid DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("list latest pull request snapshots: %w", err)
	}
	defer rows.Close()

	return scanPullRequestSnapshots(rows)
}

func (r *PullRequestSnapshotsRepository) ListLatestByRepoAndPR(ctx context.Context, repo string, prNumber int64) ([]PullRequestSnapshotRecord, error) {
	rows, err := r.q.QueryContext(ctx, `
		WITH ranked AS (
			SELECT id, ROW_NUMBER() OVER (
				PARTITION BY project_id
				ORDER BY captured_at DESC, created_at DESC, rowid DESC
			) AS row_number
			FROM pull_request_snapshots
			WHERE repo = ? COLLATE NOCASE AND pr_number = ?
		)
		SELECT `+qualifiedColumns("snapshots", pullRequestSnapshotColumns)+`
		FROM pull_request_snapshots snapshots
		JOIN ranked ON ranked.id = snapshots.id
		WHERE ranked.row_number = 1
		ORDER BY snapshots.captured_at DESC, snapshots.created_at DESC, snapshots.rowid DESC
	`, repo, prNumber)
	if err != nil {
		return nil, fmt.Errorf("list latest pull request snapshots by repository and pull request: %w", err)
	}
	defer rows.Close()

	return scanPullRequestSnapshots(rows)
}

func (r *PullRequestSnapshotsRepository) GetLatest(ctx context.Context, repo string, prNumber int64) (*PullRequestSnapshotRecord, error) {
	records, err := r.ListLatestByRepoAndPR(ctx, repo, prNumber)
	if err != nil {
		return nil, fmt.Errorf("get latest pull request snapshot: %w", err)
	}
	if len(records) == 0 {
		return nil, nil
	}
	projectID := records[0].ProjectID
	for _, record := range records[1:] {
		if record.ProjectID != projectID {
			return nil, fmt.Errorf("get latest pull request snapshot: repository %s#%d matches multiple projects; use GetLatestByProject", repo, prNumber)
		}
	}
	return &records[0], nil
}

func (r *PullRequestSnapshotsRepository) GetLatestByProject(ctx context.Context, projectID, repo string, prNumber int64) (*PullRequestSnapshotRecord, error) {
	row := r.q.QueryRowContext(ctx, `SELECT `+pullRequestSnapshotColumns+` FROM pull_request_snapshots WHERE project_id = ? AND repo = ? COLLATE NOCASE AND pr_number = ? ORDER BY captured_at DESC, created_at DESC, rowid DESC LIMIT 1`, projectID, repo, prNumber)
	record, err := scanPullRequestSnapshot(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get latest pull request snapshot by project: %w", err)
	}

	return &record, nil
}

func (r *LocksRepository) SetNow(now func() time.Time) {
	if now == nil {
		r.now = time.Now
		return
	}

	r.now = now
}

func (r *LocksRepository) Acquire(ctx context.Context, record LockRecord) (bool, error) {
	nowISO := r.now().UTC().Format(javaScriptISOStringLayout)
	query := `
		INSERT INTO locks (key, owner, reason, expires_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET
			owner=excluded.owner,
			reason=excluded.reason,
			expires_at=excluded.expires_at,
			updated_at=excluded.updated_at
		WHERE locks.expires_at <= ?
	`
	args := []any{record.Key, record.Owner, record.Reason, record.ExpiresAt, record.CreatedAt, record.UpdatedAt, nowISO}
	legacyKey, scopedSuffix, kind, scoped, transitionKey := lockTransitionAlias(record.Key)
	if transitionKey {
		aliasPredicate := "key = ?"
		aliasArgs := []any{legacyKey}
		if !scoped {
			aliasPredicate = "key LIKE ? AND lower(substr(key, -?)) = lower(?)"
			aliasArgs = []any{kind + ":%", len(scopedSuffix), scopedSuffix}
		} else {
			aliasPredicate = "lower(key) = lower(?)"
		}
		query = `
			INSERT INTO locks (key, owner, reason, expires_at, created_at, updated_at)
			SELECT ?, ?, ?, ?, ?, ?
			WHERE NOT EXISTS (
				SELECT 1 FROM locks
				WHERE key != ? AND expires_at > ? AND (` + aliasPredicate + `)
			)
			ON CONFLICT(key) DO UPDATE SET
				owner=excluded.owner,
				reason=excluded.reason,
				expires_at=excluded.expires_at,
				updated_at=excluded.updated_at
			WHERE locks.expires_at <= ?
		`
		args = []any{record.Key, record.Owner, record.Reason, record.ExpiresAt, record.CreatedAt, record.UpdatedAt, record.Key, nowISO}
		args = append(args, aliasArgs...)
		args = append(args, nowISO)
	}
	result, err := r.q.ExecContext(ctx, query, args...)
	if err != nil {
		return false, fmt.Errorf("acquire lock: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read acquire lock rows affected: %w", err)
	}

	return affected > 0, nil
}

func (r *LocksRepository) Release(ctx context.Context, key string) error {
	_, err := r.q.ExecContext(ctx, `DELETE FROM locks WHERE key = ?`, key)
	if err != nil {
		return fmt.Errorf("release lock: %w", err)
	}

	return nil
}

func (r *LocksRepository) Get(ctx context.Context, key string) (*LockRecord, error) {
	row := r.q.QueryRowContext(ctx, `SELECT `+lockColumns+` FROM locks WHERE key = ?`, key)
	record, err := scanLock(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get lock: %w", err)
	}

	return &record, nil
}

func (r *LocksRepository) Refresh(ctx context.Context, record LockRecord) (bool, error) {
	result, err := r.q.ExecContext(ctx, `
		UPDATE locks
		SET reason = ?, expires_at = ?, updated_at = ?
		WHERE key = ? AND owner = ?
	`, record.Reason, record.ExpiresAt, record.UpdatedAt, record.Key, record.Owner)
	if err != nil {
		return false, fmt.Errorf("refresh lock: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read refresh lock rows affected: %w", err)
	}

	return affected > 0, nil
}

func (r *LocksRepository) ListExpired(ctx context.Context, nowISO string) ([]LockRecord, error) {
	rows, err := r.q.QueryContext(ctx, `SELECT `+lockColumns+` FROM locks WHERE expires_at <= ? ORDER BY expires_at ASC`, nowISO)
	if err != nil {
		return nil, fmt.Errorf("list expired locks: %w", err)
	}
	defer rows.Close()

	return scanLocks(rows)
}

type WorktreesRepository struct{ q sqliteQuerier }

type QueueRepository struct{ q sqliteQuerier }

func (r *QueueRepository) CreateOrGetActiveByDedupe(ctx context.Context, record QueueItemRecord) (QueueItemRecord, bool, error) {
	for attempt := 0; attempt < 2; attempt++ {
		persisted, didPersist, err := r.UpsertActiveByDedupeOrGetExisting(ctx, record)
		if err == nil {
			return persisted, didPersist, nil
		}
		if !shouldRecoverActiveDedupeConflict(record, err) {
			return QueueItemRecord{}, false, err
		}
	}
	return QueueItemRecord{}, false, fmt.Errorf("upsert queue item: active dedupe conflict for %s", record.DedupeKey)
}

func (r *QueueRepository) UpsertActiveByDedupeOrGetExisting(ctx context.Context, record QueueItemRecord) (QueueItemRecord, bool, error) {
	err := r.Upsert(ctx, record)
	if err == nil {
		return record, true, nil
	}
	if !shouldRecoverActiveDedupeConflict(record, err) {
		return QueueItemRecord{}, false, err
	}
	existing, findErr := r.FindActiveByDedupe(ctx, record.DedupeKey)
	if findErr != nil {
		return QueueItemRecord{}, false, findErr
	}
	if existing != nil {
		return *existing, false, nil
	}
	return QueueItemRecord{}, false, err
}

func shouldRecoverActiveDedupeConflict(record QueueItemRecord, err error) bool {
	if !isQueueActiveDedupeConstraintError(err) {
		return false
	}
	if record.DedupeKey == "" {
		return false
	}
	return record.Type == "reviewer" || record.Type == "fixer"
}

func (r *QueueRepository) Upsert(ctx context.Context, record QueueItemRecord) error {
	_, err := r.q.ExecContext(ctx, `
		INSERT INTO queue_items (
			id, project_id, loop_id, type, target_type, target_id, repo,
			pr_number, dedupe_key, priority, status, available_at, attempts,
			max_attempts, claimed_by, claimed_at, started_at, finished_at,
			lock_key, payload_json, last_error, last_error_kind, created_at,
			updated_at
		)
		VALUES (
			?, ?, ?, ?, ?, ?, ?,
			?, ?, ?, ?, ?, ?,
			?, ?, ?, ?, ?,
			?, ?, ?, ?, ?,
			?
		)
		ON CONFLICT(id) DO UPDATE SET
			project_id=excluded.project_id,
			loop_id=excluded.loop_id,
			type=excluded.type,
			target_type=excluded.target_type,
			target_id=excluded.target_id,
			repo=excluded.repo,
			pr_number=excluded.pr_number,
			dedupe_key=excluded.dedupe_key,
			priority=excluded.priority,
			status=excluded.status,
			available_at=excluded.available_at,
			attempts=excluded.attempts,
			max_attempts=excluded.max_attempts,
			claimed_by=excluded.claimed_by,
			claimed_at=excluded.claimed_at,
			started_at=excluded.started_at,
			finished_at=excluded.finished_at,
			lock_key=excluded.lock_key,
			payload_json=excluded.payload_json,
			last_error=excluded.last_error,
			last_error_kind=excluded.last_error_kind,
			updated_at=excluded.updated_at
	`, record.ID, record.ProjectID, record.LoopID, record.Type, record.TargetType, record.TargetID, record.Repo, record.PRNumber, record.DedupeKey, record.Priority, record.Status, record.AvailableAt, record.Attempts, record.MaxAttempts, record.ClaimedBy, record.ClaimedAt, record.StartedAt, record.FinishedAt, record.LockKey, record.PayloadJSON, record.LastError, record.LastErrorKind, record.CreatedAt, record.UpdatedAt)
	if err != nil {
		return fmt.Errorf("upsert queue item: %w", err)
	}

	return nil
}

func (r *QueueRepository) GetByID(ctx context.Context, id string) (*QueueItemRecord, error) {
	row := r.q.QueryRowContext(ctx, `SELECT `+queueItemColumns+` FROM queue_items WHERE id = ?`, id)
	record, err := scanQueueItem(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get queue item by id: %w", err)
	}

	return &record, nil
}

func (r *QueueRepository) GetLatestByLoopID(ctx context.Context, loopID string) (*QueueItemRecord, error) {
	row := r.q.QueryRowContext(ctx, `
		SELECT `+queueItemColumns+` FROM queue_items
		WHERE loop_id = ?
		ORDER BY updated_at DESC, created_at DESC, id DESC
		LIMIT 1
	`, loopID)
	record, err := scanQueueItem(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get latest queue item by loop: %w", err)
	}

	return &record, nil
}

// ListLatestByLoopIDs returns the latest queue item per loop_id for the given IDs.
// Ordering within a loop matches GetLatestByLoopID: updated_at, created_at, id DESC.
func (r *QueueRepository) ListLatestByLoopIDs(ctx context.Context, loopIDs []string) ([]QueueItemRecord, error) {
	if len(loopIDs) == 0 {
		return []QueueItemRecord{}, nil
	}
	chunks := chunkStrings(loopIDs, sqliteMaxVariables)
	items := make([]QueueItemRecord, 0, len(loopIDs))
	for _, chunk := range chunks {
		args := make([]any, 0, len(chunk))
		for _, loopID := range chunk {
			args = append(args, loopID)
		}
		rows, err := r.q.QueryContext(ctx, `
			SELECT
				id, project_id, loop_id, type, target_type, target_id, repo, pr_number, dedupe_key,
				priority, status, available_at, attempts, max_attempts, claimed_by, claimed_at,
				started_at, finished_at, lock_key, payload_json, last_error, last_error_kind,
				created_at, updated_at
			FROM (
				SELECT
					`+queueItemColumns+`,
					ROW_NUMBER() OVER (
						PARTITION BY loop_id
						ORDER BY updated_at DESC, created_at DESC, id DESC
					) AS row_num
				FROM queue_items
				WHERE loop_id IN (`+sqlPlaceholders(len(chunk))+`)
			)
			WHERE row_num = 1
			ORDER BY updated_at DESC, created_at DESC, id DESC
		`, args...)
		if err != nil {
			return nil, fmt.Errorf("list latest queue items by loop ids: %w", err)
		}
		chunkItems, scanErr := scanQueueItems(rows)
		closeErr := rows.Close()
		if scanErr != nil {
			return nil, scanErr
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close list latest queue items by loop ids rows: %w", closeErr)
		}
		items = append(items, chunkItems...)
	}
	return items, nil
}

func (r *QueueRepository) List(ctx context.Context) ([]QueueItemRecord, error) {
	rows, err := r.q.QueryContext(ctx, `SELECT `+queueItemColumns+` FROM queue_items ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list queue items: %w", err)
	}
	defer rows.Close()

	return scanQueueItems(rows)
}

// ListForTriage returns queue items whose status can still affect the
// operator board. Terminal history remains available through List, while the
// bounded read avoids decoding completed/failed/cancelled rows on every poll.
func (r *QueueRepository) ListForTriage(ctx context.Context) ([]QueueItemRecord, error) {
	rows, err := r.q.QueryContext(ctx, `
		SELECT `+queueItemColumns+`
		FROM queue_items
		WHERE status IS NULL OR status NOT IN ('completed', 'failed', 'cancelled')
		ORDER BY updated_at DESC, created_at DESC, id DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("list queue items for triage: %w", err)
	}
	defer rows.Close()

	return scanQueueItems(rows)
}

func (r *QueueRepository) ListByStatuses(ctx context.Context, statuses []string) ([]QueueItemRecord, error) {
	if len(statuses) == 0 {
		return []QueueItemRecord{}, nil
	}
	args := make([]any, 0, len(statuses))
	for _, status := range statuses {
		args = append(args, status)
	}
	rows, err := r.q.QueryContext(ctx, `SELECT `+queueItemColumns+` FROM queue_items WHERE status IN (`+sqlPlaceholders(len(statuses))+`) ORDER BY created_at DESC, id DESC`, args...)
	if err != nil {
		return nil, fmt.Errorf("list queue items by statuses: %w", err)
	}
	defer rows.Close()

	return scanQueueItems(rows)
}

func (r *QueueRepository) ListLatestByLoopStatuses(ctx context.Context, statuses []string) ([]QueueItemRecord, error) {
	if len(statuses) == 0 {
		return []QueueItemRecord{}, nil
	}
	args := make([]any, 0, len(statuses))
	for _, status := range statuses {
		args = append(args, status)
	}
	rows, err := r.q.QueryContext(ctx, `
		SELECT
			id, project_id, loop_id, type, target_type, target_id, repo, pr_number, dedupe_key,
			priority, status, available_at, attempts, max_attempts, claimed_by, claimed_at,
			started_at, finished_at, lock_key, payload_json, last_error, last_error_kind,
			created_at, updated_at
		FROM (
			SELECT
				`+queueItemColumns+`,
				ROW_NUMBER() OVER (
					PARTITION BY loop_id
					ORDER BY updated_at DESC, created_at DESC, id DESC
				) AS row_num
			FROM queue_items
			WHERE loop_id IS NOT NULL AND TRIM(loop_id) != ''
		)
		WHERE row_num = 1 AND status IN (`+sqlPlaceholders(len(statuses))+`)
		ORDER BY created_at DESC, id DESC
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("list latest queue items by loop statuses: %w", err)
	}
	defer rows.Close()

	return scanQueueItems(rows)
}

func (r *QueueRepository) CountByAllStatuses(ctx context.Context) (map[string]int64, error) {
	rows, err := r.q.QueryContext(ctx, `SELECT status, COUNT(*) FROM queue_items GROUP BY status`)
	if err != nil {
		return nil, fmt.Errorf("count queue items by all statuses: %w", err)
	}
	defer rows.Close()

	counts := map[string]int64{}
	for rows.Next() {
		var status string
		var count int64
		if err := rows.Scan(&status, &count); err != nil {
			return nil, fmt.Errorf("scan queue item counts by all statuses: %w", err)
		}
		counts[status] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate queue item counts by all statuses: %w", err)
	}

	return counts, nil
}

func (r *QueueRepository) ListQueued(ctx context.Context, limit int64) ([]QueueItemRecord, error) {
	if limit <= 0 {
		limit = 100
	}

	rows, err := r.q.QueryContext(ctx, `
		SELECT `+queueItemColumns+`
		FROM queue_items
		WHERE status = 'queued'
		ORDER BY priority ASC, available_at ASC, created_at ASC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("list queued queue items: %w", err)
	}
	defer rows.Close()

	return scanQueueItems(rows)
}

func (r *QueueRepository) CountByStatus(ctx context.Context, status string) (int64, error) {
	row := r.q.QueryRowContext(ctx, `SELECT COUNT(*) FROM queue_items WHERE status = ?`, status)
	var count int64
	if err := row.Scan(&count); err != nil {
		return 0, fmt.Errorf("count queue items by status: %w", err)
	}
	return count, nil
}

func (r *QueueRepository) CountActiveByLoopID(ctx context.Context, loopID string) (int64, error) {
	row := r.q.QueryRowContext(ctx, `SELECT COUNT(*) FROM queue_items WHERE loop_id = ? AND status IN ('queued', 'running')`, loopID)
	var count int64
	if err := row.Scan(&count); err != nil {
		return 0, fmt.Errorf("count active queue items by loop: %w", err)
	}
	return count, nil
}

func (r *QueueRepository) CountByLoopIDAndStatus(ctx context.Context, loopID, status string) (int64, error) {
	row := r.q.QueryRowContext(ctx, `SELECT COUNT(*) FROM queue_items WHERE loop_id = ? AND status = ?`, loopID, status)
	var count int64
	if err := row.Scan(&count); err != nil {
		return 0, fmt.Errorf("count queue items by loop and status: %w", err)
	}
	return count, nil
}

func (r *QueueRepository) FindActiveByDedupe(ctx context.Context, dedupeKey string) (*QueueItemRecord, error) {
	row := r.q.QueryRowContext(ctx, `
		SELECT `+queueItemColumns+` FROM queue_items
		WHERE dedupe_key = ? AND status IN ('queued', 'running')
		ORDER BY created_at DESC
		LIMIT 1
	`, dedupeKey)
	record, err := scanQueueItem(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("find active queue item by dedupe: %w", err)
	}

	return &record, nil
}

func (r *QueueRepository) FindActiveByLoopID(ctx context.Context, loopID string) (*QueueItemRecord, error) {
	row := r.q.QueryRowContext(ctx, `
		SELECT `+queueItemColumns+` FROM queue_items
		WHERE loop_id = ? AND status IN ('queued', 'running')
		ORDER BY created_at DESC
		LIMIT 1
	`, loopID)
	record, err := scanQueueItem(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("find active queue item by loop: %w", err)
	}

	return &record, nil
}

func (r *QueueRepository) ListScheduled(ctx context.Context, nowISO string, limit int64) ([]QueueItemRecord, error) {
	if limit <= 0 {
		limit = 100
	}

	rows, err := r.q.QueryContext(ctx, scheduledQueueQuery+` LIMIT ?`, nowISO, limit)
	if err != nil {
		return nil, fmt.Errorf("list scheduled queue items: %w", err)
	}
	defer rows.Close()

	return scanQueueItems(rows)
}

func (r *QueueRepository) Stats(ctx context.Context, nowISO string) (QueueStats, error) {
	stats := QueueStats{}
	queries := []struct {
		name string
		dest *int64
		sql  string
		now  bool
	}{
		{name: "total queued", dest: &stats.TotalQueued, sql: `SELECT COUNT(*) FROM queue_items WHERE status = 'queued'`},
		{name: "eligible queued", dest: &stats.EligibleQueued, sql: `SELECT COUNT(*) FROM (` + scheduledQueueBaseQuery + `)`, now: true},
		{name: "blocked by terminal or paused loop", dest: &stats.BlockedByTerminalOrPausedLoop, sql: `
			SELECT COUNT(*)
			FROM queue_items qi
			JOIN loops l ON l.id = qi.loop_id
			WHERE qi.status = 'queued'
				AND l.status IN ('paused', 'completed', 'failed', 'interrupted', 'terminated', 'stopped')
		`},
		{name: "blocked by lock key", dest: &stats.BlockedByLockKey, now: true, sql: `
			SELECT COUNT(*)
			FROM queue_items qi
			WHERE qi.status = 'queued'
				AND qi.available_at <= ?
				AND qi.lock_key IS NOT NULL
				AND EXISTS (
					SELECT 1
					FROM queue_items lock_blocker
					WHERE ` + queueLockConflictPredicate + `
						AND lock_blocker.status = 'running'
						AND lock_blocker.id != qi.id
				)
		`},
		{name: "blocked by reviewer/fixer dependency", dest: &stats.BlockedByReviewerFixerDependency, now: true, sql: `
			SELECT COUNT(*)
			FROM queue_items qi
			WHERE qi.status = 'queued'
				AND qi.available_at <= ?
				AND qi.type = 'fixer'
				AND qi.repo IS NOT NULL
				AND qi.pr_number IS NOT NULL
				AND EXISTS (
					SELECT 1
					FROM queue_items blocker
					WHERE blocker.type = 'reviewer'
						AND blocker.repo = qi.repo
						AND blocker.pr_number = qi.pr_number
						AND (blocker.project_id = qi.project_id OR blocker.project_id IS NULL OR qi.project_id IS NULL)
						AND blocker.status IN ('queued', 'running')
						AND blocker.id != qi.id
				)
		`},
		{name: "scheduled for future", dest: &stats.ScheduledForFuture, sql: `SELECT COUNT(*) FROM queue_items WHERE status = 'queued' AND available_at > ?`, now: true},
		{name: "stale queued", dest: &stats.StaleQueued, sql: staleQueuedCountQuery},
	}

	for _, query := range queries {
		args := []any{}
		if query.now {
			args = append(args, nowISO)
		}
		if err := r.q.QueryRowContext(ctx, query.sql, args...).Scan(query.dest); err != nil {
			return QueueStats{}, fmt.Errorf("count queue items %s: %w", query.name, err)
		}
	}
	return stats, nil
}

func (r *QueueRepository) CleanupStaleQueued(ctx context.Context, finishedAt string, reason string) (int64, error) {
	result, err := r.q.ExecContext(ctx, `
		UPDATE queue_items
		SET status = 'cancelled',
			finished_at = ?,
			last_error = ?,
			last_error_kind = 'non_retryable',
			updated_at = ?
		WHERE id IN (`+staleQueuedIDsQuery+`)
	`, finishedAt, reason, finishedAt)
	if err != nil {
		return 0, fmt.Errorf("cleanup stale queued items: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read cleanup stale queued items rows affected: %w", err)
	}
	return affected, nil
}

func (r *QueueRepository) ClaimNext(ctx context.Context, nowISO, claimedBy string) (*QueueItemRecord, error) {
	return r.claimNextMatching(ctx, nowISO, claimedBy, "", nil)
}

func (r *QueueRepository) ClaimNextNonLongTermRetry(ctx context.Context, nowISO, claimedBy string) (*QueueItemRecord, error) {
	return r.claimNextMatching(ctx, nowISO, claimedBy, `
		AND NOT (`+longTermRetryPredicateParam+`)
	`, []any{QueueLongTermRetryAttemptThreshold})
}

func (r *QueueRepository) ClaimNextLongTermRetry(ctx context.Context, nowISO, claimedBy string) (*QueueItemRecord, error) {
	return r.claimNextMatching(ctx, nowISO, claimedBy, `
		AND (`+longTermRetryPredicateParam+`)
	`, []any{QueueLongTermRetryAttemptThreshold})
}

// claimCtxForDurableClaim refuses already-cancelled callers, then detaches
// cancellation from the UPDATE...RETURNING while preserving any caller
// deadline. If the parent context is cancelled after SQLite commits the claim
// but before the row is scanned, QueryRowContext would return ctx.Err without
// the claimed item and strand a durable running row with no owner
// (ADR-0015 R6 / #579). context.WithoutCancel also strips Deadline, so a
// WithTimeout caller would otherwise hang indefinitely on a blocked SQLite
// claim; re-apply the deadline on the detached context. The returned cancel
// must be deferred by the caller to free the deadline timer.
func claimCtxForDurableClaim(ctx context.Context) (context.Context, context.CancelFunc, error) {
	noop := func() {}
	if ctx == nil {
		return context.Background(), noop, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	detached := context.WithoutCancel(ctx)
	if deadline, ok := ctx.Deadline(); ok {
		// WithoutCancel strips Deadline; re-apply so QueryRowContext still bounds.
		withDeadline, cancel := context.WithDeadline(detached, deadline)
		return withDeadline, cancel, nil
	}
	return detached, noop, nil
}

// ClaimNextNonLongTermRetryAmongTypes claims the next non-long-term-retry item
// whose type is in types. Empty types claims nothing.
func (r *QueueRepository) ClaimNextNonLongTermRetryAmongTypes(ctx context.Context, nowISO, claimedBy string, types []string) (*QueueItemRecord, error) {
	return r.ClaimNextNonLongTermRetryAmongTypeSets(ctx, nowISO, claimedBy, types, nil)
}

// ClaimNextLongTermRetryAmongTypes claims the next long-term-retry item whose
// type is in types. Empty types claims nothing.
func (r *QueueRepository) ClaimNextLongTermRetryAmongTypes(ctx context.Context, nowISO, claimedBy string, types []string) (*QueueItemRecord, error) {
	return r.ClaimNextLongTermRetryAmongTypeSets(ctx, nowISO, claimedBy, types, nil)
}

// ClaimNextNonLongTermRetryAmongTypeSets claims the next non-long-term-retry item
// that is either unrestricted by type or sticky-snapshot-only (latest run is
// failed/interrupted with a non-empty agent_snapshot_json vendor). Both type
// slices empty claims nothing.
func (r *QueueRepository) ClaimNextNonLongTermRetryAmongTypeSets(ctx context.Context, nowISO, claimedBy string, unrestrictedTypes, stickySnapshotTypes []string) (*QueueItemRecord, error) {
	return r.ClaimNextNonLongTermRetryAmongTypeSetsForProjects(ctx, nowISO, claimedBy, unrestrictedTypes, stickySnapshotTypes, nil, nil, nil)
}

// ClaimNextNonLongTermRetryAmongTypeSetsForProjects additionally restricts
// projectScopedTypes to queue items whose project_id is in projectIDs. Other
// queue types remain eligible. stickyOnlyProjectIDs extends the scope for
// projectScopedTypes to quarantined projects, but only for items whose latest
// run is a sticky snapshot (failed/interrupted with a non-empty agent snapshot
// vendor), so fresh work for quarantined projects is never admitted.
func (r *QueueRepository) ClaimNextNonLongTermRetryAmongTypeSetsForProjects(ctx context.Context, nowISO, claimedBy string, unrestrictedTypes, stickySnapshotTypes, projectScopedTypes, projectIDs, stickyOnlyProjectIDs []string) (*QueueItemRecord, error) {
	typePred, typeArgs := queueClaimTypePredicate(unrestrictedTypes, stickySnapshotTypes)
	if typePred == "" {
		return nil, nil
	}
	projectPred, projectArgs := queueClaimProjectPredicate(projectScopedTypes, projectIDs, stickyOnlyProjectIDs)
	extraArgs := append([]any{QueueLongTermRetryAttemptThreshold}, typeArgs...)
	extraArgs = append(extraArgs, projectArgs...)
	return r.claimNextMatching(ctx, nowISO, claimedBy, `
		AND NOT (`+longTermRetryPredicateParam+`)
		`+typePred+`
		`+projectPred+`
	`, extraArgs)
}

// ClaimNextLongTermRetryAmongTypeSets is the long-term-retry counterpart of
// ClaimNextNonLongTermRetryAmongTypeSets.
func (r *QueueRepository) ClaimNextLongTermRetryAmongTypeSets(ctx context.Context, nowISO, claimedBy string, unrestrictedTypes, stickySnapshotTypes []string) (*QueueItemRecord, error) {
	return r.ClaimNextLongTermRetryAmongTypeSetsForProjects(ctx, nowISO, claimedBy, unrestrictedTypes, stickySnapshotTypes, nil, nil, nil)
}

// ClaimNextLongTermRetryAmongTypeSetsForProjects is the long-term-retry
// counterpart of ClaimNextNonLongTermRetryAmongTypeSetsForProjects.
func (r *QueueRepository) ClaimNextLongTermRetryAmongTypeSetsForProjects(ctx context.Context, nowISO, claimedBy string, unrestrictedTypes, stickySnapshotTypes, projectScopedTypes, projectIDs, stickyOnlyProjectIDs []string) (*QueueItemRecord, error) {
	typePred, typeArgs := queueClaimTypePredicate(unrestrictedTypes, stickySnapshotTypes)
	if typePred == "" {
		return nil, nil
	}
	projectPred, projectArgs := queueClaimProjectPredicate(projectScopedTypes, projectIDs, stickyOnlyProjectIDs)
	extraArgs := append([]any{QueueLongTermRetryAttemptThreshold}, typeArgs...)
	extraArgs = append(extraArgs, projectArgs...)
	return r.claimNextMatching(ctx, nowISO, claimedBy, `
		AND (`+longTermRetryPredicateParam+`)
		`+typePred+`
		`+projectPred+`
	`, extraArgs)
}

func queueTypeInClause(types []string) (placeholders string, args []any) {
	parts := make([]string, 0, len(types))
	args = make([]any, 0, len(types))
	for _, t := range types {
		parts = append(parts, "?")
		args = append(args, t)
	}
	return strings.Join(parts, ", "), args
}

// queueClaimTypePredicate builds the AND (...) filter for unrestricted types
// and sticky-snapshot-only types. Sticky types require the loop's latest run
// (started_at DESC, created_at DESC — matching Runs.GetLatestByLoopID) to be
// failed/interrupted with a non-empty agent_snapshot_json vendor so fresh work
// for unconfigured roles is never claimed while sticky retries still are.
func queueClaimTypePredicate(unrestrictedTypes, stickySnapshotTypes []string) (predicate string, args []any) {
	parts := make([]string, 0, 2)
	if len(unrestrictedTypes) > 0 {
		placeholders, typeArgs := queueTypeInClause(unrestrictedTypes)
		parts = append(parts, "qi.type IN ("+placeholders+")")
		args = append(args, typeArgs...)
	}
	if len(stickySnapshotTypes) > 0 {
		placeholders, typeArgs := queueTypeInClause(stickySnapshotTypes)
		parts = append(parts, `(qi.type IN (`+placeholders+`) AND `+stickySnapshotExistsSQL()+`)`)
		args = append(args, typeArgs...)
	}
	if len(parts) == 0 {
		return "", nil
	}
	return "AND (" + strings.Join(parts, " OR ") + ")", args
}

// stickySnapshotExistsSQL returns the reusable SQL fragment that reports whether
// the queue item's loop has a sticky-snapshot latest run: failed or interrupted
// with a non-empty agent_snapshot_json vendor. It is shared by the type
// predicate (sticky-snapshot-only roles) and the project predicate (sticky-only
// scope for quarantined projects) so the two cannot drift on what "sticky" means.
func stickySnapshotExistsSQL() string {
	return `qi.loop_id IS NOT NULL
			AND EXISTS (
				SELECT 1
				FROM (
					SELECT status, agent_snapshot_json
					FROM runs
					WHERE loop_id = qi.loop_id
					ORDER BY ` + latestRunOrder("runs") + `
					LIMIT 1
				) latest
				WHERE latest.status IN ('failed', 'interrupted')
					AND latest.agent_snapshot_json IS NOT NULL
					AND length(trim(latest.agent_snapshot_json)) > 0
					AND length(trim(coalesce(json_extract(latest.agent_snapshot_json, '$.vendor'), ''))) > 0
			)`
}

// queueClaimProjectPredicate scopes projectScopedTypes to queue items whose
// project_id is in projectIDs. stickyOnlyProjectIDs additionally admits those
// types for quarantined projects, but only when the loop's latest run is a
// sticky snapshot, so fresh work for quarantined projects is never claimed
// while durable sticky retries still are. Other queue types remain eligible.
func queueClaimProjectPredicate(projectScopedTypes, projectIDs, stickyOnlyProjectIDs []string) (predicate string, args []any) {
	if len(projectScopedTypes) == 0 {
		return "", nil
	}
	typePlaceholders, typeArgs := queueTypeInClause(projectScopedTypes)
	args = append(args, typeArgs...)
	if len(projectIDs) == 0 && len(stickyOnlyProjectIDs) == 0 {
		return "AND qi.type NOT IN (" + typePlaceholders + ")", args
	}
	scopeClauses := make([]string, 0, 2)
	if len(projectIDs) > 0 {
		projectPlaceholders := make([]string, 0, len(projectIDs))
		for _, projectID := range projectIDs {
			projectPlaceholders = append(projectPlaceholders, "?")
			args = append(args, projectID)
		}
		scopeClauses = append(scopeClauses, "qi.project_id IN ("+strings.Join(projectPlaceholders, ", ")+")")
	}
	if len(stickyOnlyProjectIDs) > 0 {
		stickyPlaceholders := make([]string, 0, len(stickyOnlyProjectIDs))
		for _, projectID := range stickyOnlyProjectIDs {
			stickyPlaceholders = append(stickyPlaceholders, "?")
			args = append(args, projectID)
		}
		scopeClauses = append(scopeClauses, "(qi.project_id IN ("+strings.Join(stickyPlaceholders, ", ")+") AND "+stickySnapshotExistsSQL()+")")
	}
	return "AND (qi.type NOT IN (" + typePlaceholders + ") OR " + strings.Join(scopeClauses, " OR ") + ")", args
}

func (r *QueueRepository) claimNextMatching(ctx context.Context, nowISO, claimedBy, extraPredicate string, extraArgs []any) (*QueueItemRecord, error) {
	claimCtx, cancel, err := claimCtxForDurableClaim(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()
	row := r.q.QueryRowContext(claimCtx, `
		WITH candidate AS (
			`+scheduledQueueBaseQuery+extraPredicate+scheduledQueueOrderBy+`
			LIMIT 1
		)
		UPDATE queue_items
		SET status = 'running',
			claimed_by = ?,
			claimed_at = ?,
			started_at = COALESCE(started_at, ?),
			updated_at = ?
		WHERE id = (SELECT id FROM candidate)
			AND status = 'queued'
		RETURNING `+queueItemColumns+`
	`, append([]any{nowISO}, append(extraArgs, claimedBy, nowISO, nowISO, nowISO)...)...)

	record, err := scanQueueItem(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("claim next queue item: %w", err)
	}

	return &record, nil
}

func (r *QueueRepository) ClaimNextOfType(ctx context.Context, nowISO, claimedBy, queueType string) (*QueueItemRecord, error) {
	claimCtx, cancel, err := claimCtxForDurableClaim(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()
	row := r.q.QueryRowContext(claimCtx, `
		WITH candidate AS (
			`+scheduledQueueBaseQuery+`
			AND qi.type = ?
			`+scheduledQueueOrderBy+`
			LIMIT 1
		)
		UPDATE queue_items
		SET status = 'running',
			claimed_by = ?,
			claimed_at = ?,
			started_at = COALESCE(started_at, ?),
			updated_at = ?
		WHERE id = (SELECT id FROM candidate)
			AND status = 'queued'
		RETURNING `+queueItemColumns+`
	`, nowISO, queueType, claimedBy, nowISO, nowISO, nowISO)

	record, err := scanQueueItem(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("claim next queue item of type: %w", err)
	}

	return &record, nil
}

// ListRunningClaimedBy returns running queue items claimed by claimedBy at
// claimedAt. Used to recover from ambiguous ClaimNext failures after context
// cancellation when the durable UPDATE may have committed without returning
// the row to the caller.
func (r *QueueRepository) ListRunningClaimedBy(ctx context.Context, claimedBy, claimedAt string) ([]QueueItemRecord, error) {
	if claimedBy == "" {
		return []QueueItemRecord{}, nil
	}
	rows, err := r.q.QueryContext(ctx, `
		SELECT `+queueItemColumns+` FROM queue_items
		WHERE status = 'running'
			AND claimed_by = ?
			AND claimed_at = ?
		ORDER BY id ASC
	`, claimedBy, claimedAt)
	if err != nil {
		return nil, fmt.Errorf("list running queue items by claimed_by: %w", err)
	}
	defer rows.Close()

	return scanQueueItems(rows)
}

func (r *QueueRepository) Complete(ctx context.Context, id, finishedAt string) error {
	result, err := r.q.ExecContext(ctx, `
		UPDATE queue_items
		SET status = 'completed', finished_at = ?, updated_at = ?
		WHERE id = ? AND status IN ('queued', 'running')
	`, finishedAt, finishedAt, id)
	if err != nil {
		return fmt.Errorf("complete queue item: %w", err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read complete queue item rows affected: %w", err)
	}
	if rowsAffected == 0 {
		return fmt.Errorf("complete queue item: %w", ErrQueueItemNotActive)
	}

	return nil
}

func (r *QueueRepository) UpdateLockKey(ctx context.Context, id, lockKey, updatedAt string) error {
	_, err := r.q.ExecContext(ctx, `
		UPDATE queue_items
		SET lock_key = ?, updated_at = ?
		WHERE id = ?
	`, lockKey, updatedAt, id)
	if err != nil {
		return fmt.Errorf("update queue item lock key: %w", err)
	}

	return nil
}

func (r *QueueRepository) MarkRetry(ctx context.Context, input QueueMarkRetryInput) error {
	// Runner authority after processing: may requeue even when concurrent pause
	// CancelByLoop already terminalized the claim mid-run (resume later). Do not
	// status-guard here — stop/bind refuse uses MarkRetryIfRunning instead.
	_, err := r.q.ExecContext(ctx, `
		UPDATE queue_items
		SET status = 'queued',
			available_at = ?,
			attempts = ?,
			last_error = ?,
			last_error_kind = ?,
			claimed_by = NULL,
			claimed_at = NULL,
			finished_at = NULL,
			updated_at = ?
		WHERE id = ?
			AND NOT EXISTS (
				SELECT 1
				FROM projects p
				WHERE p.id = queue_items.project_id AND p.archived = 1
			)
	`, input.AvailableAt, input.Attempts, input.ErrorMessage, input.ErrorKind, input.UpdatedAt, input.ID)
	if err != nil {
		return fmt.Errorf("mark queue item for retry: %w", err)
	}

	return nil
}

// MarkRetryIfRunning requeues only while status is still running. Zero rows
// (already cancelled/completed/failed) is a no-op success so stop/bind refuse
// cannot resurrect terminal cancellation. Prefer MarkRetry for runner finalize.
func (r *QueueRepository) MarkRetryIfRunning(ctx context.Context, input QueueMarkRetryInput) error {
	_, err := r.q.ExecContext(ctx, `
		UPDATE queue_items
		SET status = 'queued',
			available_at = ?,
			attempts = ?,
			last_error = ?,
			last_error_kind = ?,
			claimed_by = NULL,
			claimed_at = NULL,
			finished_at = NULL,
			updated_at = ?
		WHERE id = ?
			AND status = 'running'
			AND NOT EXISTS (
				SELECT 1
				FROM projects p
				WHERE p.id = queue_items.project_id AND p.archived = 1
			)
	`, input.AvailableAt, input.Attempts, input.ErrorMessage, input.ErrorKind, input.UpdatedAt, input.ID)
	if err != nil {
		return fmt.Errorf("mark queue item for retry if running: %w", err)
	}

	return nil
}

func (r *QueueRepository) Fail(ctx context.Context, input QueueFailInput) error {
	_, err := r.q.ExecContext(ctx, `
		UPDATE queue_items
		SET status = ?,
			attempts = CASE WHEN ? > 0 THEN ? ELSE attempts END,
			finished_at = ?,
			last_error = ?,
			last_error_kind = ?,
			updated_at = ?
		WHERE id = ? AND status IN ('queued', 'running')
	`, "manual_intervention", input.Attempts, input.Attempts, input.FinishedAt, input.ErrorMessage, input.ErrorKind, input.UpdatedAt, input.ID)
	if err != nil {
		return fmt.Errorf("fail queue item: %w", err)
	}

	return nil
}

func (r *QueueRepository) RequeueRunningByLoop(ctx context.Context, loopID, queuedAt string) (int64, error) {
	result, err := r.q.ExecContext(ctx, `
		UPDATE queue_items
		SET status = 'queued',
			available_at = ?,
			claimed_by = NULL,
			claimed_at = NULL,
			started_at = NULL,
			finished_at = NULL,
			updated_at = ?
		WHERE loop_id = ? AND status = 'running'
	`, queuedAt, queuedAt, loopID)
	if err != nil {
		return 0, fmt.Errorf("requeue running queue items by loop: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read requeue queue items rows affected: %w", err)
	}

	return affected, nil
}

func (r *QueueRepository) RequeueLatestCancelledByLoop(ctx context.Context, loopID, queuedAt string) (int64, error) {
	queueID, err := r.findLatestCancelledQueueIDByLoop(ctx, loopID, true)
	if err != nil {
		return 0, err
	}
	if queueID == "" {
		queueID, err = r.findLatestCancelledQueueIDByLoop(ctx, loopID, false)
		if err != nil {
			return 0, err
		}
	}
	if queueID == "" {
		return 0, nil
	}

	result, err := r.q.ExecContext(ctx, `
		UPDATE queue_items
		SET status = 'queued',
			available_at = ?,
			claimed_by = NULL,
			claimed_at = NULL,
			started_at = NULL,
			finished_at = NULL,
			last_error = NULL,
			last_error_kind = NULL,
			updated_at = ?
		WHERE id = ? AND status = 'cancelled'
			AND NOT EXISTS (
				SELECT 1 FROM queue_items
				WHERE loop_id = ? AND status IN ('queued', 'running') AND id != ?
			)
	`, queuedAt, queuedAt, queueID, loopID, queueID)
	if err != nil {
		return 0, fmt.Errorf("requeue latest cancelled queue item by loop: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read requeue latest cancelled queue item rows affected: %w", err)
	}

	return affected, nil
}

// RequeueCompletedByID re-arms the just-completed queue item for a new human
// inbox turn. A completed worker turn is distinct from a retry, so attempts
// restart at zero. It never replaces another active item for the same loop.
func (r *QueueRepository) RequeueCompletedByID(ctx context.Context, loopID, queueID, queuedAt string) (int64, error) {
	result, err := r.q.ExecContext(ctx, `
		UPDATE queue_items
		SET status = 'queued',
			available_at = ?,
			attempts = 0,
			claimed_by = NULL,
			claimed_at = NULL,
			started_at = NULL,
			finished_at = NULL,
			last_error = NULL,
			last_error_kind = NULL,
			updated_at = ?
		WHERE id = ? AND loop_id = ? AND status = 'completed'
			AND NOT EXISTS (
				SELECT 1 FROM queue_items
				WHERE loop_id = ? AND status IN ('queued', 'running') AND id != ?
			)
	`, queuedAt, queuedAt, queueID, loopID, loopID, queueID)
	if err != nil {
		return 0, fmt.Errorf("requeue completed queue item: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read requeue completed queue item rows affected: %w", err)
	}
	return affected, nil
}

func (r *QueueRepository) RequeueLatestFailedByLoop(ctx context.Context, loopID, queuedAt string) (int64, error) {
	queueID, err := r.findLatestQueueIDByLoopStatuses(ctx, loopID, []string{"manual_intervention", "failed"})
	if err != nil {
		return 0, err
	}
	if queueID == "" {
		return 0, nil
	}

	return r.RequeueFailedByID(ctx, loopID, queueID, queuedAt)
}

func (r *QueueRepository) RequeueFailedByID(ctx context.Context, loopID, queueID, queuedAt string) (int64, error) {
	if loopID == "" || queueID == "" {
		return 0, nil
	}

	result, err := r.q.ExecContext(ctx, `
		UPDATE queue_items
		SET status = 'queued',
			available_at = ?,
			attempts = 0,
			claimed_by = NULL,
			claimed_at = NULL,
			started_at = NULL,
			finished_at = NULL,
			last_error = NULL,
			last_error_kind = NULL,
			updated_at = ?
		WHERE id = ? AND loop_id = ? AND status IN ('manual_intervention', 'failed')
			AND NOT EXISTS (
				SELECT 1 FROM queue_items
				WHERE loop_id = ? AND status IN ('queued', 'running') AND id != ?
			)
	`, queuedAt, queuedAt, queueID, loopID, loopID, queueID)
	if err != nil {
		return 0, fmt.Errorf("requeue failed queue item by id: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read requeue failed queue item rows affected: %w", err)
	}

	return affected, nil
}

func (r *QueueRepository) RequeueFailedByIDWithAttempts(ctx context.Context, loopID, queueID, queuedAt string, attempts int64) (int64, error) {
	if loopID == "" || queueID == "" {
		return 0, nil
	}
	if attempts < 0 {
		attempts = 0
	}

	result, err := r.q.ExecContext(ctx, `
		UPDATE queue_items
		SET status = 'queued',
			available_at = ?,
			attempts = ?,
			claimed_by = NULL,
			claimed_at = NULL,
			started_at = NULL,
			finished_at = NULL,
			last_error = NULL,
			last_error_kind = NULL,
			updated_at = ?
		WHERE id = ? AND loop_id = ? AND status IN ('manual_intervention', 'failed')
			AND NOT EXISTS (
				SELECT 1 FROM queue_items
				WHERE loop_id = ? AND status IN ('queued', 'running') AND id != ?
			)
	`, queuedAt, attempts, queuedAt, queueID, loopID, loopID, queueID)
	if err != nil {
		return 0, fmt.Errorf("requeue failed queue item by id with attempts: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read requeue failed queue item rows affected: %w", err)
	}

	return affected, nil
}

func (r *QueueRepository) findLatestQueueIDByLoopStatuses(ctx context.Context, loopID string, statuses []string) (string, error) {
	if len(statuses) == 0 {
		return "", nil
	}
	placeholders := make([]string, 0, len(statuses))
	args := []any{loopID}
	for _, status := range statuses {
		placeholders = append(placeholders, "?")
		args = append(args, status)
	}
	row := r.q.QueryRowContext(ctx, `
		SELECT id
		FROM queue_items
		WHERE loop_id = ? AND status IN (`+strings.Join(placeholders, ", ")+`)
		ORDER BY updated_at DESC, created_at DESC, id DESC LIMIT 1
	`, args...)
	var queueID string
	if err := row.Scan(&queueID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("get latest queue item by loop statuses %s: %w", strings.Join(statuses, ","), err)
	}
	return queueID, nil
}

func (r *QueueRepository) findLatestCancelledQueueIDByLoop(ctx context.Context, loopID string, onlyUnstarted bool) (string, error) {
	query := `
		SELECT id
		FROM queue_items
		WHERE loop_id = ? AND status = 'cancelled'
	`
	args := []any{loopID}
	if onlyUnstarted {
		query += ` AND started_at IS NULL`
	}
	query += ` ORDER BY updated_at DESC, created_at DESC, id DESC LIMIT 1`

	row := r.q.QueryRowContext(ctx, query, args...)
	var queueID string
	if err := row.Scan(&queueID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("get latest cancelled queue item by loop: %w", err)
	}
	return queueID, nil
}

func (r *QueueRepository) CancelByLoop(ctx context.Context, loopID, finishedAt string, reason *string) (int64, error) {
	result, err := r.q.ExecContext(ctx, `
		UPDATE queue_items
		SET status = 'cancelled',
			finished_at = ?,
			last_error = COALESCE(?, last_error),
			updated_at = ?
		WHERE loop_id = ? AND status IN ('queued', 'running')
	`, finishedAt, reason, finishedAt, loopID)
	if err != nil {
		return 0, fmt.Errorf("cancel queue items by loop: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read cancel queue items rows affected: %w", err)
	}

	return affected, nil
}

func (r *QueueRepository) CancelByProject(ctx context.Context, projectID, finishedAt string, reason *string) (int64, error) {
	if strings.TrimSpace(projectID) == "" {
		return 0, nil
	}

	result, err := r.q.ExecContext(ctx, `
		UPDATE queue_items
		SET status = 'cancelled',
			finished_at = ?,
			last_error = COALESCE(?, last_error),
			updated_at = ?
		WHERE project_id = ? AND status IN ('queued', 'running', 'failed', 'manual_intervention')
	`, finishedAt, reason, finishedAt, projectID)
	if err != nil {
		return 0, fmt.Errorf("cancel queue items by project: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read cancel queue items by project rows affected: %w", err)
	}

	return affected, nil
}

func (r *QueueRepository) CancelActiveByLoopExcept(ctx context.Context, loopID, keepID, finishedAt string, reason *string) (int64, error) {
	if strings.TrimSpace(loopID) == "" || strings.TrimSpace(keepID) == "" {
		return 0, nil
	}
	result, err := r.q.ExecContext(ctx, `
		UPDATE queue_items
		SET status = 'cancelled',
			finished_at = ?,
			last_error = COALESCE(?, last_error),
			updated_at = ?
		WHERE loop_id = ? AND status IN ('queued', 'running') AND id != ?
	`, finishedAt, reason, finishedAt, loopID, keepID)
	if err != nil {
		return 0, fmt.Errorf("cancel active queue items by loop except %s: %w", keepID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read cancel active queue items rows affected: %w", err)
	}
	return affected, nil
}

func (r *WorktreesRepository) Upsert(ctx context.Context, record WorktreeRecord) error {
	_, err := r.q.ExecContext(ctx, `
		INSERT INTO worktrees (id, project_id, repo_path, worktree_path, branch, base_branch, status, head_sha, metadata_json, created_at, updated_at, cleaned_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			project_id=excluded.project_id,
			repo_path=excluded.repo_path,
			worktree_path=excluded.worktree_path,
			branch=excluded.branch,
			base_branch=excluded.base_branch,
			status=excluded.status,
			head_sha=excluded.head_sha,
			metadata_json=excluded.metadata_json,
			updated_at=excluded.updated_at,
			cleaned_at=excluded.cleaned_at
	`, record.ID, record.ProjectID, record.RepoPath, record.WorktreePath, record.Branch, record.BaseBranch, record.Status, record.HeadSHA, record.MetadataJSON, record.CreatedAt, record.UpdatedAt, record.CleanedAt)
	if err != nil {
		return fmt.Errorf("upsert worktree: %w", err)
	}

	return nil
}

// AdoptPath upserts record as the durable identity for its physical checkout.
// If a different row currently owns the requested project/branch label, the
// label is retired in the same transaction so a failed adoption cannot strand
// the other active checkout under synthetic provenance.
func (r *WorktreesRepository) AdoptPath(ctx context.Context, record WorktreeRecord) error {
	if tx, ok := r.q.(*sql.Tx); ok {
		return (&WorktreesRepository{q: tx}).adoptPath(ctx, record)
	}
	db, ok := r.q.(txBeginner)
	if !ok {
		return fmt.Errorf("worktree path adoption requires transactional storage")
	}
	return WithTransaction(ctx, db, nil, func(tx *sql.Tx) error {
		return (&WorktreesRepository{q: tx}).adoptPath(ctx, record)
	})
}

func (r *WorktreesRepository) adoptPath(ctx context.Context, record WorktreeRecord) error {
	pathRecord, err := r.GetByPath(ctx, record.WorktreePath)
	if err != nil {
		return err
	}
	// A retired record is provenance for an abandoned physical checkout. Once
	// RestoreWorktree has proved that checkout missing and CreateWorktree has
	// recreated the path, the new durable claim replaces that provenance.
	if pathRecord != nil && pathRecord.ID != record.ID && pathRecord.Status == "retired" {
		if err := r.Delete(ctx, pathRecord.ID); err != nil {
			return fmt.Errorf("delete retired worktree %q before path adoption: %w", pathRecord.ID, err)
		}
	}
	branchRecord, err := r.GetByBranch(ctx, record.ProjectID, record.Branch)
	if err != nil {
		return err
	}
	if branchRecord != nil && branchRecord.ID != record.ID {
		if branchRecord.Status == "cleaned" || branchRecord.CleanedAt != nil {
			if err := r.Delete(ctx, branchRecord.ID); err != nil {
				return fmt.Errorf("delete colliding cleaned worktree %q: %w", branchRecord.ID, err)
			}
		} else {
			retired := *branchRecord
			retired.Branch = fmt.Sprintf("retired/%s/%s", branchRecord.ID, strings.ReplaceAll(branchRecord.Branch, "/", "-"))
			retired.UpdatedAt = record.UpdatedAt
			if err := r.Upsert(ctx, retired); err != nil {
				return fmt.Errorf("retire colliding worktree branch %q: %w", branchRecord.ID, err)
			}
		}
	}
	return r.Upsert(ctx, record)
}

// Delete removes a worktree identity row. Callers use this when two durable
// identities must collapse onto one physical path (or one branch label) and the
// discarded row no longer describes a checkout Looper should manage.
func (r *WorktreesRepository) Delete(ctx context.Context, id string) error {
	if _, err := r.q.ExecContext(ctx, `DELETE FROM worktrees WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete worktree: %w", err)
	}
	return nil
}

// RetireByProject keeps enough durable provenance to distinguish a checkout
// deliberately left behind by a project handoff from an interrupted creation
// that has not yet reached AdoptPath. Physical checkouts are deliberately left
// untouched: a handoff must not discard potentially dirty work, and a later
// CreateWorktree must refuse to adopt it.
func (r *WorktreesRepository) RetireByProject(ctx context.Context, projectID, updatedAt string) (int64, error) {
	records, err := r.ListByProject(ctx, projectID)
	if err != nil {
		return 0, err
	}
	for _, record := range records {
		if record.Status == "retired" {
			continue
		}
		retired := record
		retired.Status = "retired"
		retired.Branch = fmt.Sprintf("retired/%s/%s", record.ID, strings.ReplaceAll(record.Branch, "/", "-"))
		retired.UpdatedAt = updatedAt
		if err := r.Upsert(ctx, retired); err != nil {
			return 0, fmt.Errorf("retire worktree %q: %w", record.ID, err)
		}
	}
	return int64(len(records)), nil
}

func (r *WorktreesRepository) GetByID(ctx context.Context, id string) (*WorktreeRecord, error) {
	row := r.q.QueryRowContext(ctx, `SELECT `+worktreeColumns+` FROM worktrees WHERE id = ?`, id)
	record, err := scanWorktree(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get worktree by id: %w", err)
	}

	return &record, nil
}

func (r *WorktreesRepository) GetByBranch(ctx context.Context, projectID, branch string) (*WorktreeRecord, error) {
	row := r.q.QueryRowContext(ctx, `SELECT `+worktreeColumns+` FROM worktrees WHERE project_id = ? AND branch = ? LIMIT 1`, projectID, branch)
	record, err := scanWorktree(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get worktree by branch: %w", err)
	}

	return &record, nil
}

func (r *WorktreesRepository) GetByPath(ctx context.Context, worktreePath string) (*WorktreeRecord, error) {
	paths := equivalentWorktreePaths(worktreePath)
	query := `SELECT ` + worktreeColumns + ` FROM worktrees WHERE worktree_path = ? LIMIT 1`
	args := []any{paths[0]}
	if len(paths) == 2 {
		query = `SELECT ` + worktreeColumns + ` FROM worktrees WHERE worktree_path IN (?, ?) LIMIT 1`
		args = []any{paths[0], paths[1]}
	}
	row := r.q.QueryRowContext(ctx, query, args...)
	record, err := scanWorktree(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get worktree by path: %w", err)
	}
	return &record, nil
}

// equivalentWorktreePaths covers macOS's supported /var <-> /private/var
// aliases while leaving all other paths as exact identities.
func equivalentWorktreePaths(path string) []string {
	canonical := filepath.Clean(strings.TrimSpace(path))
	if runtime.GOOS != "darwin" {
		return []string{canonical}
	}
	if strings.HasPrefix(canonical, "/private/var/") {
		canonical = strings.TrimPrefix(canonical, "/private")
	}
	if strings.HasPrefix(canonical, "/var/") {
		return []string{canonical, "/private" + canonical}
	}
	return []string{canonical}
}

// WorktreePathsEquivalent reports whether two path spellings identify the
// same supported worktree location. In particular, macOS exposes /var through
// /private/var, and worktree discovery may persist either spelling.
func WorktreePathsEquivalent(a, b string) bool {
	aPaths := equivalentWorktreePaths(a)
	bPaths := equivalentWorktreePaths(b)
	for _, aPath := range aPaths {
		for _, bPath := range bPaths {
			if aPath == bPath {
				return true
			}
		}
	}
	return false
}

func (r *WorktreesRepository) ListByProject(ctx context.Context, projectID string) ([]WorktreeRecord, error) {
	rows, err := r.q.QueryContext(ctx, `SELECT `+worktreeColumns+` FROM worktrees WHERE project_id = ? ORDER BY updated_at DESC`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list worktrees by project: %w", err)
	}
	defer rows.Close()

	return scanWorktrees(rows)
}

func (r *WorktreesRepository) ListCleanupCandidates(ctx context.Context, limit int) ([]WorktreeRecord, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := r.q.QueryContext(ctx, `
		SELECT `+worktreeColumns+`
		FROM worktrees
		WHERE status NOT IN ('cleaned', 'retired')
		ORDER BY updated_at ASC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("list worktree cleanup candidates: %w", err)
	}
	defer rows.Close()

	return scanWorktrees(rows)
}

func (r *WorktreesRepository) ListActive(ctx context.Context) ([]WorktreeRecord, error) {
	rows, err := r.q.QueryContext(ctx, `SELECT `+worktreeColumns+` FROM worktrees WHERE cleaned_at IS NULL AND status != 'retired' ORDER BY updated_at ASC, created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("list active worktrees: %w", err)
	}
	defer rows.Close()

	return scanWorktrees(rows)
}

func (r *WorktreesRepository) TouchCleanupAttempt(ctx context.Context, id, updatedAt string) error {
	_, err := r.q.ExecContext(ctx, `
		UPDATE worktrees
		SET updated_at = ?
		WHERE id = ? AND status != 'cleaned'
	`, updatedAt, id)
	if err != nil {
		return fmt.Errorf("touch worktree cleanup attempt: %w", err)
	}

	return nil
}

func boolToInt(value bool) int {
	if value {
		return 1
	}

	return 0
}

func nullableString(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}

	stringValue := value.String
	return &stringValue
}

func nullableInt64(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}

	intValue := value.Int64
	return &intValue
}

func sqlPlaceholders(count int) string {
	if count <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", count), ",")
}

const sqliteMaxVariables = 900

func chunkStrings(values []string, chunkSize int) [][]string {
	if len(values) == 0 {
		return nil
	}
	if chunkSize <= 0 {
		chunkSize = len(values)
	}
	chunks := make([][]string, 0, (len(values)+chunkSize-1)/chunkSize)
	for start := 0; start < len(values); start += chunkSize {
		end := start + chunkSize
		if end > len(values) {
			end = len(values)
		}
		chunks = append(chunks, values[start:end])
	}
	return chunks
}

const queueLockConflictPredicate = `(
	lock_blocker.lock_key = qi.lock_key
	OR (
		lock_blocker.repo = qi.repo
		AND lock_blocker.target_type = qi.target_type
		AND lock_blocker.target_id = qi.target_id
		AND (lock_blocker.lock_key = lock_blocker.target_id OR qi.lock_key = qi.target_id)
	)
)`

// humanHoldClaimPredicate fences every queue item that could enter the same
// human-held checkout. The durable loop hold is the authority: queue discovery
// may materialize work before takeover, so checking only the held loop's item
// leaves sibling PR roles claimable after cancellation.
const humanHoldClaimPredicate = `NOT EXISTS (
			SELECT 1
			FROM loops held
			WHERE held.status = 'human_takeover'
				AND held.project_id = COALESCE(l.project_id, qi.project_id)
				AND (
					(
						held.target_type = 'pull_request'
						AND held.pr_number IS NOT NULL
						AND held.pr_number = COALESCE(l.pr_number, qi.pr_number)
						AND held.repo IS NOT NULL
						AND held.repo = COALESCE(l.repo, qi.repo)
					)
					OR (
						held.target_id IS NOT NULL
						AND held.target_type = COALESCE(l.target_type, qi.target_type)
						AND held.target_type != 'project'
						AND held.target_id = COALESCE(l.target_id, qi.target_id)
						AND (
							held.target_type != 'issue'
							OR (
								held.type = 'planner'
								AND COALESCE(l.type, qi.type) = 'planner'
							)
						)
					)
					OR held.id = qi.loop_id
				)
		)`

var scheduledQueueBaseQuery = `
	SELECT ` + qualifiedColumns("qi", queueItemColumns) + `
	FROM queue_items qi
	LEFT JOIN loops l ON l.id = qi.loop_id
	LEFT JOIN projects p ON p.id = qi.project_id
	WHERE qi.status = 'queued'
		AND qi.available_at <= ?
		AND (qi.project_id IS NULL OR p.archived = 0)
		AND COALESCE(l.status, 'queued') NOT IN ('paused', 'completed', 'failed', 'interrupted', 'terminated', 'stopped')
		AND ` + humanHoldClaimPredicate + `
		AND (
			qi.lock_key IS NULL
			OR NOT EXISTS (
				SELECT 1
				FROM queue_items lock_blocker
				WHERE ` + queueLockConflictPredicate + `
					AND lock_blocker.status = 'running'
					AND lock_blocker.id != qi.id
			)
		)
		AND (
			qi.type != 'fixer'
			OR qi.repo IS NULL
			OR qi.pr_number IS NULL
			OR NOT EXISTS (
				SELECT 1
				FROM queue_items blocker
				WHERE blocker.type = 'reviewer'
					AND blocker.repo = qi.repo
					AND blocker.pr_number = qi.pr_number
					AND (blocker.project_id = qi.project_id OR blocker.project_id IS NULL OR qi.project_id IS NULL)
					AND blocker.status IN ('queued', 'running')
					AND blocker.id != qi.id
			)
		)
`

const scheduledQueueOrderBy = `
	ORDER BY CASE WHEN ` + longTermRetryPredicateLiteral + ` THEN 1 ELSE 0 END ASC,
		qi.priority ASC, qi.available_at ASC, qi.created_at ASC
`

const longTermRetryPredicateLiteral = `qi.attempts >= 5 AND COALESCE(qi.last_error_kind, '') IN ('retryable_transient', 'retryable_after_resume', 'non_retryable')`
const longTermRetryPredicateParam = `qi.attempts >= ? AND COALESCE(qi.last_error_kind, '') IN ('retryable_transient', 'retryable_after_resume', 'non_retryable')`

var scheduledQueueQuery = scheduledQueueBaseQuery + scheduledQueueOrderBy

const staleQueuedIDsQuery = `
	SELECT qi.id
	FROM queue_items qi
	JOIN loops l ON l.id = qi.loop_id
	WHERE qi.status = 'queued'
		AND l.status IN ('completed', 'failed', 'interrupted', 'terminated', 'stopped')
`

const staleQueuedCountQuery = `SELECT COUNT(*) FROM (` + staleQueuedIDsQuery + `)`

func scanProjects(rows *sql.Rows) ([]ProjectRecord, error) {
	records := make([]ProjectRecord, 0)
	for rows.Next() {
		record, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate project rows: %w", err)
	}

	return records, nil
}

func scanProject(row interface{ Scan(...any) error }) (ProjectRecord, error) {
	var (
		record       ProjectRecord
		baseBranch   sql.NullString
		metadataJSON sql.NullString
		archived     int
	)

	err := row.Scan(&record.ID, &record.Name, &record.RepoPath, &baseBranch, &archived, &metadataJSON, &record.CreatedAt, &record.UpdatedAt)
	if err != nil {
		return ProjectRecord{}, err
	}
	record.BaseBranch = nullableString(baseBranch)
	record.Archived = archived == 1
	record.MetadataJSON = nullableString(metadataJSON)

	return record, nil
}

func scanLoops(rows *sql.Rows) ([]LoopRecord, error) {
	records := make([]LoopRecord, 0)
	for rows.Next() {
		record, err := scanLoop(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate loop rows: %w", err)
	}

	return records, nil
}

func scanLoop(row interface{ Scan(...any) error }) (LoopRecord, error) {
	var (
		record       LoopRecord
		targetID     sql.NullString
		repo         sql.NullString
		prNumber     sql.NullInt64
		configJSON   sql.NullString
		metadataJSON sql.NullString
		lastRunAt    sql.NullString
		nextRunAt    sql.NullString
	)

	err := row.Scan(&record.ID, &record.Seq, &record.ProjectID, &record.Type, &record.TargetType, &targetID, &repo, &prNumber, &record.Status, &configJSON, &metadataJSON, &lastRunAt, &nextRunAt, &record.CreatedAt, &record.UpdatedAt)
	if err != nil {
		return LoopRecord{}, err
	}
	record.TargetID = nullableString(targetID)
	record.Repo = nullableString(repo)
	record.PRNumber = nullableInt64(prNumber)
	record.ConfigJSON = nullableString(configJSON)
	record.MetadataJSON = nullableString(metadataJSON)
	record.LastRunAt = nullableString(lastRunAt)
	record.NextRunAt = nullableString(nextRunAt)

	return record, nil
}

func scanRuns(rows *sql.Rows) ([]RunRecord, error) {
	records := make([]RunRecord, 0)
	for rows.Next() {
		record, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate run rows: %w", err)
	}

	return records, nil
}

func scanAgentExecutions(rows *sql.Rows) ([]AgentExecutionRecord, error) {
	records := make([]AgentExecutionRecord, 0)
	for rows.Next() {
		record, err := scanAgentExecution(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate agent execution rows: %w", err)
	}

	return records, nil
}

func scanAgentExecution(row interface{ Scan(...any) error }) (AgentExecutionRecord, error) {
	var (
		record             AgentExecutionRecord
		projectID          sql.NullString
		loopID             sql.NullString
		runID              sql.NullString
		pid                sql.NullInt64
		commandJSON        sql.NullString
		cwd                sql.NullString
		summary            sql.NullString
		parseStatus        sql.NullString
		completionSignal   sql.NullString
		lastHeartbeatAt    sql.NullString
		outputJSON         sql.NullString
		errorMessage       sql.NullString
		nativeSessionID    sql.NullString
		nativeResumeMode   sql.NullString
		nativeResumeStatus sql.NullString
		nativeResumeError  sql.NullString
		endedAt            sql.NullString
		metadataJSON       sql.NullString
	)

	err := row.Scan(
		&record.ID,
		&projectID,
		&loopID,
		&runID,
		&record.Vendor,
		&record.Status,
		&pid,
		&commandJSON,
		&cwd,
		&summary,
		&parseStatus,
		&completionSignal,
		&record.HeartbeatCount,
		&lastHeartbeatAt,
		&outputJSON,
		&errorMessage,
		&nativeSessionID,
		&nativeResumeMode,
		&nativeResumeStatus,
		&nativeResumeError,
		&record.StartedAt,
		&endedAt,
		&metadataJSON,
		&record.CreatedAt,
		&record.UpdatedAt,
	)
	if err != nil {
		return AgentExecutionRecord{}, err
	}

	record.ProjectID = nullableString(projectID)
	record.LoopID = nullableString(loopID)
	record.RunID = nullableString(runID)
	record.PID = nullableInt64(pid)
	record.CommandJSON = nullableString(commandJSON)
	record.CWD = nullableString(cwd)
	record.Summary = nullableString(summary)
	record.ParseStatus = nullableString(parseStatus)
	record.CompletionSignal = nullableString(completionSignal)
	record.LastHeartbeatAt = nullableString(lastHeartbeatAt)
	record.OutputJSON = nullableString(outputJSON)
	record.ErrorMessage = nullableString(errorMessage)
	record.NativeSessionID = nullableString(nativeSessionID)
	record.NativeResumeMode = nullableString(nativeResumeMode)
	record.NativeResumeStatus = nullableString(nativeResumeStatus)
	record.NativeResumeError = nullableString(nativeResumeError)
	record.EndedAt = nullableString(endedAt)
	record.MetadataJSON = nullableString(metadataJSON)

	return record, nil
}

func scanRun(row interface{ Scan(...any) error }) (RunRecord, error) {
	var (
		record            RunRecord
		currentStep       sql.NullString
		lastCompletedStep sql.NullString
		checkpointJSON    sql.NullString
		summary           sql.NullString
		errorMessage      sql.NullString
		lastHeartbeatAt   sql.NullString
		endedAt           sql.NullString
		agentSnapshotJSON sql.NullString
		seq               sql.NullInt64
	)

	err := row.Scan(&record.ID, &record.LoopID, &record.Status, &currentStep, &lastCompletedStep, &checkpointJSON, &summary, &errorMessage, &record.StartedAt, &lastHeartbeatAt, &endedAt, &record.CreatedAt, &record.UpdatedAt, &agentSnapshotJSON, &seq)
	if err != nil {
		return RunRecord{}, err
	}
	record.Seq = seq.Int64
	record.CurrentStep = nullableString(currentStep)
	record.LastCompletedStep = nullableString(lastCompletedStep)
	record.CheckpointJSON = nullableString(checkpointJSON)
	record.Summary = nullableString(summary)
	record.ErrorMessage = nullableString(errorMessage)
	record.LastHeartbeatAt = nullableString(lastHeartbeatAt)
	record.EndedAt = nullableString(endedAt)
	record.AgentSnapshotJSON = nullableString(agentSnapshotJSON)

	return record, nil
}

func scanPullRequestSnapshots(rows *sql.Rows) ([]PullRequestSnapshotRecord, error) {
	records := make([]PullRequestSnapshotRecord, 0)
	for rows.Next() {
		record, err := scanPullRequestSnapshot(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pull request snapshot rows: %w", err)
	}

	return records, nil
}

func scanPullRequestSnapshot(row interface{ Scan(...any) error }) (PullRequestSnapshotRecord, error) {
	var (
		record                PullRequestSnapshotRecord
		baseSHA               sql.NullString
		title                 sql.NullString
		body                  sql.NullString
		author                sql.NullString
		diffRef               sql.NullString
		checksSummary         sql.NullString
		unresolvedThreadCount sql.NullInt64
		reviewState           sql.NullString
		payloadJSON           sql.NullString
	)

	err := row.Scan(&record.ID, &record.ProjectID, &record.Repo, &record.PRNumber, &record.HeadSHA, &baseSHA, &title, &body, &author, &diffRef, &checksSummary, &unresolvedThreadCount, &reviewState, &payloadJSON, &record.CapturedAt, &record.CreatedAt)
	if err != nil {
		return PullRequestSnapshotRecord{}, err
	}
	record.BaseSHA = nullableString(baseSHA)
	record.Title = nullableString(title)
	record.Body = nullableString(body)
	record.Author = nullableString(author)
	record.DiffRef = nullableString(diffRef)
	record.ChecksSummary = nullableString(checksSummary)
	record.UnresolvedThreadCount = nullableInt64(unresolvedThreadCount)
	record.ReviewState = nullableString(reviewState)
	record.PayloadJSON = nullableString(payloadJSON)

	return record, nil
}

func scanEventLogs(rows *sql.Rows) ([]EventLogRecord, error) {
	records := make([]EventLogRecord, 0)
	for rows.Next() {
		record, err := scanEventLog(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate event log rows: %w", err)
	}

	return records, nil
}

func scanEventLog(row interface{ Scan(...any) error }) (EventLogRecord, error) {
	var (
		record           EventLogRecord
		projectID        sql.NullString
		loopID           sql.NullString
		runID            sql.NullString
		entityType       sql.NullString
		entityID         sql.NullString
		correlationID    sql.NullString
		causationID      sql.NullString
		actorType        sql.NullString
		actorID          sql.NullString
		actorDisplayName sql.NullString
	)

	err := row.Scan(
		&record.ID,
		&record.EventType,
		&projectID,
		&loopID,
		&runID,
		&entityType,
		&entityID,
		&correlationID,
		&causationID,
		&actorType,
		&actorID,
		&actorDisplayName,
		&record.PayloadJSON,
		&record.CreatedAt,
	)
	if err != nil {
		return EventLogRecord{}, err
	}

	record.ProjectID = nullableString(projectID)
	record.LoopID = nullableString(loopID)
	record.RunID = nullableString(runID)
	record.EntityType = nullableString(entityType)
	record.EntityID = nullableString(entityID)
	record.CorrelationID = nullableString(correlationID)
	record.CausationID = nullableString(causationID)
	record.ActorType = nullableString(actorType)
	record.ActorID = nullableString(actorID)
	record.ActorDisplayName = nullableString(actorDisplayName)

	return record, nil
}

func scanNotifications(rows *sql.Rows) ([]NotificationRecord, error) {
	records := make([]NotificationRecord, 0)
	for rows.Next() {
		record, err := scanNotification(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate notification rows: %w", err)
	}

	return records, nil
}

func scanNotification(row interface{ Scan(...any) error }) (NotificationRecord, error) {
	var (
		record       NotificationRecord
		projectID    sql.NullString
		loopID       sql.NullString
		runID        sql.NullString
		entityType   sql.NullString
		entityID     sql.NullString
		subtitle     sql.NullString
		dedupeKey    sql.NullString
		errorMessage sql.NullString
		payloadJSON  sql.NullString
		sentAt       sql.NullString
	)

	err := row.Scan(
		&record.ID,
		&projectID,
		&loopID,
		&runID,
		&entityType,
		&entityID,
		&record.Channel,
		&record.Level,
		&record.Title,
		&subtitle,
		&record.Body,
		&record.Status,
		&dedupeKey,
		&errorMessage,
		&payloadJSON,
		&sentAt,
		&record.CreatedAt,
		&record.UpdatedAt,
	)
	if err != nil {
		return NotificationRecord{}, err
	}

	record.ProjectID = nullableString(projectID)
	record.LoopID = nullableString(loopID)
	record.RunID = nullableString(runID)
	record.EntityType = nullableString(entityType)
	record.EntityID = nullableString(entityID)
	record.Subtitle = nullableString(subtitle)
	record.DedupeKey = nullableString(dedupeKey)
	record.ErrorMessage = nullableString(errorMessage)
	record.PayloadJSON = nullableString(payloadJSON)
	record.SentAt = nullableString(sentAt)

	return record, nil
}

func scanLocks(rows *sql.Rows) ([]LockRecord, error) {
	records := make([]LockRecord, 0)
	for rows.Next() {
		record, err := scanLock(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate lock rows: %w", err)
	}

	return records, nil
}

func scanLock(row interface{ Scan(...any) error }) (LockRecord, error) {
	var (
		record LockRecord
		reason sql.NullString
	)

	err := row.Scan(&record.Key, &record.Owner, &reason, &record.ExpiresAt, &record.CreatedAt, &record.UpdatedAt)
	if err != nil {
		return LockRecord{}, err
	}
	record.Reason = nullableString(reason)

	return record, nil
}

func scanWorktrees(rows *sql.Rows) ([]WorktreeRecord, error) {
	records := make([]WorktreeRecord, 0)
	for rows.Next() {
		record, err := scanWorktree(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate worktree rows: %w", err)
	}

	return records, nil
}

func scanQueueItems(rows *sql.Rows) ([]QueueItemRecord, error) {
	records := make([]QueueItemRecord, 0)
	for rows.Next() {
		record, err := scanQueueItem(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate queue item rows: %w", err)
	}

	return records, nil
}

func scanPlannerWorkGraph(row interface{ Scan(...any) error }) (PlannerWorkGraphRecord, error) {
	var (
		record       PlannerWorkGraphRecord
		replanReason sql.NullString
	)
	err := row.Scan(
		&record.ID,
		&record.ProjectID,
		&record.ParentRepo,
		&record.ParentIssueNumber,
		&record.PlannerLoopID,
		&record.BaseBranch,
		&record.Status,
		&replanReason,
		&record.CreatedAt,
		&record.UpdatedAt,
	)
	if err != nil {
		return PlannerWorkGraphRecord{}, err
	}
	record.ReplanReason = nullableString(replanReason)
	return record, nil
}

func scanPlannerWorkGraphNodes(rows *sql.Rows) ([]PlannerWorkGraphNodeRecord, error) {
	records := make([]PlannerWorkGraphNodeRecord, 0)
	for rows.Next() {
		record, err := scanPlannerWorkGraphNode(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate planner work graph nodes: %w", err)
	}
	return records, nil
}

func scanPlannerWorkGraphNode(row interface{ Scan(...any) error }) (PlannerWorkGraphNodeRecord, error) {
	var (
		record        PlannerWorkGraphNodeRecord
		blockedReason sql.NullString
	)
	err := row.Scan(
		&record.GraphID,
		&record.NodeKey,
		&record.Goal,
		&record.AcceptanceCriteriaJSON,
		&record.ExpectedPRScope,
		&record.WorkerLoopID,
		&record.Branch,
		&record.State,
		&blockedReason,
		&record.CreatedAt,
		&record.UpdatedAt,
	)
	if err != nil {
		return PlannerWorkGraphNodeRecord{}, err
	}
	record.BlockedReason = nullableString(blockedReason)
	return record, nil
}

func scanQueueItem(row interface{ Scan(...any) error }) (QueueItemRecord, error) {
	var (
		record        QueueItemRecord
		projectID     sql.NullString
		loopID        sql.NullString
		repo          sql.NullString
		prNumber      sql.NullInt64
		claimedBy     sql.NullString
		claimedAt     sql.NullString
		startedAt     sql.NullString
		finishedAt    sql.NullString
		lockKey       sql.NullString
		payloadJSON   sql.NullString
		lastError     sql.NullString
		lastErrorKind sql.NullString
	)

	err := row.Scan(
		&record.ID,
		&projectID,
		&loopID,
		&record.Type,
		&record.TargetType,
		&record.TargetID,
		&repo,
		&prNumber,
		&record.DedupeKey,
		&record.Priority,
		&record.Status,
		&record.AvailableAt,
		&record.Attempts,
		&record.MaxAttempts,
		&claimedBy,
		&claimedAt,
		&startedAt,
		&finishedAt,
		&lockKey,
		&payloadJSON,
		&lastError,
		&lastErrorKind,
		&record.CreatedAt,
		&record.UpdatedAt,
	)
	if err != nil {
		return QueueItemRecord{}, err
	}

	record.ProjectID = nullableString(projectID)
	record.LoopID = nullableString(loopID)
	record.Repo = nullableString(repo)
	record.PRNumber = nullableInt64(prNumber)
	record.ClaimedBy = nullableString(claimedBy)
	record.ClaimedAt = nullableString(claimedAt)
	record.StartedAt = nullableString(startedAt)
	record.FinishedAt = nullableString(finishedAt)
	record.LockKey = nullableString(lockKey)
	record.PayloadJSON = nullableString(payloadJSON)
	record.LastError = nullableString(lastError)
	record.LastErrorKind = nullableString(lastErrorKind)

	return record, nil
}

func scanWorktree(row interface{ Scan(...any) error }) (WorktreeRecord, error) {
	var (
		record       WorktreeRecord
		baseBranch   sql.NullString
		headSHA      sql.NullString
		metadataJSON sql.NullString
		cleanedAt    sql.NullString
	)

	err := row.Scan(&record.ID, &record.ProjectID, &record.RepoPath, &record.WorktreePath, &record.Branch, &baseBranch, &record.Status, &headSHA, &metadataJSON, &record.CreatedAt, &record.UpdatedAt, &cleanedAt)
	if err != nil {
		return WorktreeRecord{}, err
	}
	record.BaseBranch = nullableString(baseBranch)
	record.HeadSHA = nullableString(headSHA)
	record.MetadataJSON = nullableString(metadataJSON)
	record.CleanedAt = nullableString(cleanedAt)

	return record, nil
}
