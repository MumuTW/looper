package reproduction

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nexu-io/looper/internal/eventlog"
	"github.com/nexu-io/looper/internal/storage"
)

// Event types. Following Triager, the Reproduction Record lives in the existing
// event log against the existing `github_issue` entity: no new table, no
// migration, and replay works by re-reading the log rather than by inferring
// state from git or GitHub.
const (
	// AttemptEventType is written before any agent call, so a crash between
	// authoring and acknowledgement has a durable identity to resume against.
	AttemptEventType = "reproduction.attempted"
	// RecordedEventType carries the Reproduction Record and is the Authority.
	RecordedEventType = "reproduction.recorded"
	// UnreproducibleEventType is a decision, not a failure: it stops the Issue
	// before any planning cost is paid and hands it to a human.
	UnreproducibleEventType = "reproduction.unreproducible"
	// WaivedEventType records a human authorizing progress without a
	// reproduction. Without it, an unreproducible Issue could never proceed
	// except by disabling the Role for the whole project.
	WaivedEventType = "reproduction.waived"
)

// EntityType is deliberately the same entity Triager writes against, so one
// Issue's triage and reproduction history read as a single timeline.
const EntityType = "github_issue"

const recordVersion = 1

// Attempt is the durable claim that a reproduction run started.
type Attempt struct {
	Version        int    `json:"version"`
	IdempotencyKey string `json:"idempotencyKey"`
	ProjectID      string `json:"projectId"`
	Repo           string `json:"repo"`
	IssueNumber    int64  `json:"issueNumber"`
	Branch         string `json:"branch"`
	TriageReportID string `json:"triageReportId,omitempty"`
	AttemptedAt    string `json:"attemptedAt"`
}

// Unreproducible is the structured `cannot reproduce` record. It names what was
// attempted, what was observed instead, and what information is missing — the
// three things a human needs in order to act, and the three things a bare
// "failed" status cannot supply.
type Unreproducible struct {
	Version            int      `json:"version"`
	IdempotencyKey     string   `json:"idempotencyKey"`
	ProjectID          string   `json:"projectId"`
	Repo               string   `json:"repo"`
	IssueNumber        int64    `json:"issueNumber"`
	Reason             Reason   `json:"reason,omitempty"`
	Attempted          []string `json:"attempted,omitempty"`
	ObservedInstead    string   `json:"observedInstead,omitempty"`
	MissingInformation []string `json:"missingInformation,omitempty"`
	Summary            string   `json:"summary,omitempty"`
	LoopID             string   `json:"loopId,omitempty"`
	RecordedAt         string   `json:"recordedAt"`
}

// Waiver is a human's authorization to proceed without a reproduction.
type Waiver struct {
	Version     int    `json:"version"`
	ProjectID   string `json:"projectId"`
	Repo        string `json:"repo"`
	IssueNumber int64  `json:"issueNumber"`
	Answer      string `json:"answer"`
	LoopID      string `json:"loopId,omitempty"`
	WaivedAt    string `json:"waivedAt"`
}

// Status is one Issue's reproduction state as reconstructed from the log.
type Status struct {
	Attempted      bool
	Attempt        Attempt
	Record         *Record
	Unreproducible *Unreproducible
	Waived         *Waiver
}

// Settled reports whether the Issue needs no further Reproducer work. An
// unreproducible Issue is settled from Reproducer's point of view even though a
// human may later waive it.
func (s Status) Settled() bool {
	return s.Record != nil || s.Unreproducible != nil || s.Waived != nil
}

// PlannerAllowed reports whether Planner may be reached for this Issue. This is
// the gate that makes "cannot reproduce" stop the Issue before planning cost is
// paid rather than after.
func (s Status) PlannerAllowed() bool {
	return s.Record != nil || s.Waived != nil
}

// EntityID matches Triager's entity identifier exactly so both Roles' records
// hang off the same timeline entry.
func EntityID(projectID, repo string, issueNumber int64) string {
	return fmt.Sprintf("%s:%s#%d", projectID, strings.ToLower(strings.TrimSpace(repo)), issueNumber)
}

// IdempotencyKey derives a stable identity for one reproduction attempt from
// the triage report that authorized it. Re-deriving the same key on replay is
// what keeps a resumed run from producing a second reproduction commit.
func IdempotencyKey(projectID, repo string, issueNumber int64, triageReportKey string) string {
	raw := fmt.Sprintf("reproducer:v1:%s:%s:%d:%s", projectID, strings.ToLower(strings.TrimSpace(repo)), issueNumber, triageReportKey)
	sum := sha256.Sum256([]byte(raw))
	return "reproduction:" + hex.EncodeToString(sum[:])
}

// CommitMarker is embedded in the reproduction commit message. It is how a
// resumed run recognises its own already-committed work on the branch instead
// of authoring a second reproduction.
func CommitMarker(idempotencyKey string) string {
	return "looper-reproduction: " + idempotencyKey
}

// LoadStatus reconstructs one Issue's reproduction state.
func LoadStatus(ctx context.Context, repos *storage.Repositories, projectID, repo string, issueNumber int64) (Status, error) {
	if repos == nil || repos.Events == nil {
		return Status{}, fmt.Errorf("events repository is not configured")
	}
	events, err := repos.Events.ListByEntity(ctx, EntityType, EntityID(projectID, repo, issueNumber))
	if err != nil {
		return Status{}, err
	}
	return statusFromEvents(events)
}

// LoadStatuses reconstructs reproduction state for every Issue of one project,
// which is what the Reproducer lane needs to decide what to work on.
func LoadStatuses(ctx context.Context, repos *storage.Repositories, projectID, repo string) (map[int64]Status, error) {
	if repos == nil || repos.Events == nil {
		return nil, fmt.Errorf("events repository is not configured")
	}
	events, err := repos.Events.ListByProjectAndEntityType(ctx, projectID, EntityType)
	if err != nil {
		return nil, err
	}
	grouped := map[int64][]storage.EventLogRecord{}
	for _, event := range events {
		issueNumber, ok := issueNumberFor(event, projectID, repo)
		if !ok {
			continue
		}
		grouped[issueNumber] = append(grouped[issueNumber], event)
	}
	statuses := make(map[int64]Status, len(grouped))
	for issueNumber, issueEvents := range grouped {
		status, err := statusFromEvents(issueEvents)
		if err != nil {
			return nil, err
		}
		statuses[issueNumber] = status
	}
	return statuses, nil
}

// issueNumberFor extracts the issue this reproduction event belongs to. Only
// reproduction events are considered: the entity type is shared with Triager.
func issueNumberFor(event storage.EventLogRecord, projectID, repo string) (int64, bool) {
	switch event.EventType {
	case AttemptEventType, RecordedEventType, UnreproducibleEventType, WaivedEventType:
	default:
		return 0, false
	}
	var envelope struct {
		ProjectID   string `json:"projectId"`
		Repo        string `json:"repo"`
		IssueNumber int64  `json:"issueNumber"`
	}
	if err := json.Unmarshal([]byte(event.PayloadJSON), &envelope); err != nil {
		return 0, false
	}
	if envelope.ProjectID != projectID || !strings.EqualFold(strings.TrimSpace(envelope.Repo), strings.TrimSpace(repo)) {
		return 0, false
	}
	if envelope.IssueNumber <= 0 {
		return 0, false
	}
	return envelope.IssueNumber, true
}

func statusFromEvents(events []storage.EventLogRecord) (Status, error) {
	status := Status{}
	for _, event := range events {
		switch event.EventType {
		case AttemptEventType:
			var attempt Attempt
			if err := json.Unmarshal([]byte(event.PayloadJSON), &attempt); err != nil {
				return Status{}, fmt.Errorf("decode reproduction attempt: %w", err)
			}
			status.Attempted = true
			status.Attempt = attempt
		case RecordedEventType:
			var record Record
			if err := json.Unmarshal([]byte(event.PayloadJSON), &record); err != nil {
				return Status{}, fmt.Errorf("decode reproduction record: %w", err)
			}
			copied := record
			status.Record = &copied
		case UnreproducibleEventType:
			var unreproducible Unreproducible
			if err := json.Unmarshal([]byte(event.PayloadJSON), &unreproducible); err != nil {
				return Status{}, fmt.Errorf("decode cannot-reproduce record: %w", err)
			}
			copied := unreproducible
			status.Unreproducible = &copied
		case WaivedEventType:
			var waiver Waiver
			if err := json.Unmarshal([]byte(event.PayloadJSON), &waiver); err != nil {
				return Status{}, fmt.Errorf("decode reproduction waiver: %w", err)
			}
			copied := waiver
			status.Waived = &copied
		}
	}
	return status, nil
}

// AppendAttempt persists the pre-agent claim.
func AppendAttempt(ctx context.Context, repos *storage.Repositories, attempt Attempt) error {
	attempt.Version = recordVersion
	return appendEvent(ctx, repos, AttemptEventType, "reproduce-attempt", attempt.ProjectID,
		EntityID(attempt.ProjectID, attempt.Repo, attempt.IssueNumber), attempt.AttemptedAt, attempt)
}

// AppendRecord persists the Reproduction Record.
func AppendRecord(ctx context.Context, repos *storage.Repositories, record Record) error {
	record.Version = recordVersion
	return appendEvent(ctx, repos, RecordedEventType, "reproduce", record.ProjectID,
		EntityID(record.ProjectID, record.Repo, record.IssueNumber), record.RecordedAt, record)
}

// AppendUnreproducible persists the structured cannot-reproduce record.
func AppendUnreproducible(ctx context.Context, repos *storage.Repositories, value Unreproducible) error {
	value.Version = recordVersion
	return appendEvent(ctx, repos, UnreproducibleEventType, "reproduce-none", value.ProjectID,
		EntityID(value.ProjectID, value.Repo, value.IssueNumber), value.RecordedAt, value)
}

// AppendWaiver persists a human's authorization to proceed without one.
func AppendWaiver(ctx context.Context, repos *storage.Repositories, waiver Waiver) error {
	waiver.Version = recordVersion
	return appendEvent(ctx, repos, WaivedEventType, "reproduce-waive", waiver.ProjectID,
		EntityID(waiver.ProjectID, waiver.Repo, waiver.IssueNumber), waiver.WaivedAt, waiver)
}

func appendEvent(ctx context.Context, repos *storage.Repositories, eventType, idPrefix, projectID, entityID, createdAt string, payload any) error {
	if repos == nil || repos.Events == nil {
		return fmt.Errorf("events repository is not configured")
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	entityType := EntityType
	actorType, actorID := "system", "reproducer"
	return repos.Events.Append(ctx, storage.EventLogRecord{
		ID: eventlog.NewEventID(idPrefix), EventType: eventType, ProjectID: &projectID,
		EntityType: &entityType, EntityID: &entityID, ActorType: &actorType, ActorID: &actorID,
		PayloadJSON: string(encoded), CreatedAt: createdAt,
	})
}
