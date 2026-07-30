package triager

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/eventlog"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/storage"
)

// AskEventType records that Triager has asked a human to release a held report.
// It is the idempotency authority for the question: a report carrying an Ask is
// never asked again, however many ticks pass before an answer arrives.
const AskEventType = "triage.asked"

// askCommentMarker identifies Triager's question on the Issue. It is not an
// authority — `triage.asked` is — but it lets a reader tell Looper's question
// from ordinary discussion.
const askCommentMarker = "<!-- looper:triage:ask v=1 -->"

// Ask is the durable record of a question Triager asked about a held report.
type Ask struct {
	ReportKey string   `json:"reportKey"`
	Questions []string `json:"questions"`
	Reasons   []string `json:"reasons"`
	CommentID int64    `json:"commentId,omitempty"`
	AskedAt   string   `json:"askedAt"`
}

// ensureAsked posts Triager's question exactly once per held report.
//
// This is also the only delivery channel for the report's confirmation token.
// The token is minted locally and never leaves the daemon otherwise, so without
// this comment a held report cannot be confirmed by anyone who is not reading
// the event log.
//
// The comment is posted before the event is recorded: a duplicate comment after
// a crash is noise, whereas recording first and crashing would leave a report
// that believes it asked a question nobody ever saw — and whose token is
// therefore unreachable.
func (r *Runner) ensureAsked(ctx context.Context, project storage.ProjectRecord, repo string, report Report) error {
	if strings.TrimSpace(report.ConfirmationToken) == "" {
		return fmt.Errorf("triage report %s is held without a confirmation token", report.IdempotencyKey)
	}
	asked, err := r.alreadyAsked(ctx, report)
	if err != nil || asked {
		return err
	}
	questions := append([]string(nil), report.Decision.MissingInformation...)
	ask := Ask{
		ReportKey: report.IdempotencyKey,
		Questions: questions,
		Reasons:   append([]string(nil), report.Policy.Reasons...),
		AskedAt:   r.now().UTC().Format(time.RFC3339Nano),
	}
	created, err := r.github.CreateIssueComment(ctx, githubinfra.IssueCommentInput{
		Repo:        repo,
		IssueNumber: report.IssueNumber,
		Body:        buildAskComment(questions, report.Policy.Reasons, report.ConfirmationToken),
		CWD:         project.RepoPath,
	})
	if err != nil {
		return err
	}
	ask.CommentID = created.ID
	return r.persistAsk(ctx, report, ask)
}

func (r *Runner) alreadyAsked(ctx context.Context, report Report) (bool, error) {
	events, err := r.repos.Events.ListByEntity(ctx, reportEntityType, reportEntityID(report.ProjectID, report.Repo, report.IssueNumber))
	if err != nil {
		return false, err
	}
	for _, event := range events {
		if event.EventType != AskEventType {
			continue
		}
		var ask Ask
		if err := json.Unmarshal([]byte(event.PayloadJSON), &ask); err != nil {
			return false, fmt.Errorf("decode triage ask: %w", err)
		}
		if ask.ReportKey == report.IdempotencyKey {
			return true, nil
		}
	}
	return false, nil
}

func (r *Runner) persistAsk(ctx context.Context, report Report, ask Ask) error {
	payload, err := json.Marshal(ask)
	if err != nil {
		return err
	}
	projectID := report.ProjectID
	entityType := reportEntityType
	entityID := reportEntityID(report.ProjectID, report.Repo, report.IssueNumber)
	actorType, actorID := "system", "triager"
	return r.repos.Events.Append(ctx, storage.EventLogRecord{
		ID: eventlog.NewEventID("triage-ask"), EventType: AskEventType, ProjectID: &projectID,
		EntityType: &entityType, EntityID: &entityID, ActorType: &actorType, ActorID: &actorID,
		PayloadJSON: string(payload), CreatedAt: ask.AskedAt,
	})
}

func buildAskComment(questions, reasons []string, token string) string {
	var b strings.Builder
	b.WriteString(askCommentMarker)
	b.WriteString("\n🤔 **Looper needs input before planning this.**\n\n")
	if len(questions) > 0 {
		b.WriteString("Missing information:\n")
		for _, question := range questions {
			b.WriteString("- " + strings.TrimSpace(question) + "\n")
		}
		b.WriteString("\n")
	}
	if len(reasons) > 0 {
		b.WriteString("Held because: `" + strings.Join(reasons, "`, `") + "`\n\n")
	}
	b.WriteString("Reply with:\n\n```\n")
	b.WriteString(confirmationCommand(token))
	b.WriteString(" <your answer>\n```\n\n")
	b.WriteString("The command is specific to this report, so an older comment cannot start planning by accident. ")
	b.WriteString("Anything after it is passed to the planner as a clarification; send the command on its own to proceed as-is.\n")
	return b.String()
}

// parseConfirmComment reports whether a comment authorizes this report, and
// returns any clarification carried with it. The report-specific command must be
// the whole first line: text before it is discussion, and requiring the token
// keeps a comment written before this report existed from authorizing anything.
func parseConfirmComment(body, token string) (confirms bool, clarification string) {
	command := confirmationCommand(strings.TrimSpace(token))
	trimmed := strings.TrimSpace(body)
	if trimmed == "" || strings.TrimSpace(token) == "" {
		return false, ""
	}
	firstLine, rest, _ := strings.Cut(trimmed, "\n")
	firstLine = strings.TrimSpace(firstLine)
	if firstLine == command {
		return true, strings.TrimSpace(rest)
	}
	if !strings.HasPrefix(firstLine, command+" ") {
		return false, ""
	}
	inline := strings.TrimSpace(strings.TrimPrefix(firstLine, command))
	return true, strings.TrimSpace(inline + "\n" + strings.TrimSpace(rest))
}

// clarificationsForReport collects the answers recorded against a report, in the
// order they were confirmed.
func (r *Runner) clarificationsForReport(ctx context.Context, report Report) ([]string, error) {
	events, err := r.repos.Events.ListByEntity(ctx, reportEntityType, reportEntityID(report.ProjectID, report.Repo, report.IssueNumber))
	if err != nil {
		return nil, err
	}
	clarifications := make([]string, 0, 1)
	for _, event := range events {
		if event.EventType != ConfirmationEventType {
			continue
		}
		var confirmation Confirmation
		if err := json.Unmarshal([]byte(event.PayloadJSON), &confirmation); err != nil {
			return nil, fmt.Errorf("decode triage confirmation: %w", err)
		}
		if confirmation.ReportKey != report.IdempotencyKey {
			continue
		}
		if text := strings.TrimSpace(confirmation.Clarification); text != "" {
			clarifications = append(clarifications, text)
		}
	}
	return clarifications, nil
}
