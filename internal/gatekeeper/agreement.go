package gatekeeper

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/eventlog"
)

// AdviceAgreementEventType records the forge-observed outcome of one advise
// verdict. The Gate report remains the authority for what Gatekeeper said; the
// terminal pull-request state returned by the forge is the authority for what
// happened afterwards.
const AdviceAgreementEventType = "pull_request.merge_gate.advice_agreement_recorded"

type AdviceOutcome string

const (
	AdviceOutcomeMergedAsIs        AdviceOutcome = "merged_as_is"
	AdviceOutcomeMergedAfterChange AdviceOutcome = "merged_after_changes"
	AdviceOutcomeClosed            AdviceOutcome = "closed"
	AdviceOutcomeHeld              AdviceOutcome = "held"
	AdviceOutcomeOverridden        AdviceOutcome = "overridden"
)

// AdviceAgreement is immutable attribution for an advise verdict. CausationID
// points at the exact Gate report it resolves, so a later verdict cannot change
// the historical outcome of an earlier one.
type AdviceAgreement struct {
	Version         int           `json:"version"`
	VerdictEventID  string        `json:"verdictEventId"`
	ProjectID       string        `json:"projectId"`
	Repo            string        `json:"repo"`
	PRNumber        int64         `json:"prNumber"`
	VerdictEligible bool          `json:"verdictEligible"`
	VerdictHeadSHA  string        `json:"verdictHeadSha,omitempty"`
	Outcome         AdviceOutcome `json:"outcome"`
	Agreement       bool          `json:"agreement"`
	TerminalState   string        `json:"terminalState"`
	TerminalHeadSHA string        `json:"terminalHeadSha,omitempty"`
	TerminalAt      string        `json:"terminalAt"`
	RecordedAt      string        `json:"recordedAt"`
}

// recordTerminalAdviceOutcomes resolves every as-yet-unresolved advise verdict
// when the existing webhook/tick evaluation observes a closed pull request. It
// deliberately makes no additional forge call: the terminal Report already
// contains the fresh provider state that caused this evaluation.
func (r *Runner) recordTerminalAdviceOutcomes(ctx context.Context, terminal Report, terminalReportEventID string) error {
	if !isTerminalAdviceReport(terminal) {
		return nil
	}
	entityID := fmt.Sprintf("%s#%d", terminal.Repo, terminal.PRNumber)
	events, err := r.repos.Events.ListByEntity(ctx, "pull_request", entityID)
	if err != nil {
		return fmt.Errorf("list advise verdict lifecycle: %w", err)
	}
	resolved := make(map[string]struct{})
	for _, record := range events {
		if record.EventType == AdviceAgreementEventType && record.CausationID != nil {
			resolved[*record.CausationID] = struct{}{}
		}
	}
	for _, record := range events {
		if record.EventType != GateReportEventType || record.ID == terminalReportEventID {
			continue
		}
		if _, alreadyResolved := resolved[record.ID]; alreadyResolved {
			continue
		}
		var verdict Report
		if err := json.Unmarshal([]byte(record.PayloadJSON), &verdict); err != nil {
			continue
		}
		if !isAdviseVerdict(verdict) {
			continue
		}
		if err := r.appendAdviceAgreement(ctx, terminal, record.ID, verdict); err != nil {
			return err
		}
	}
	// The terminal evaluation is itself a verdict. Resolve it too, so every
	// advice verdict has an inspectable outcome even when a closed webhook is
	// the first evaluation Looper sees.
	if _, alreadyResolved := resolved[terminalReportEventID]; !alreadyResolved && isAdviseVerdict(terminal) {
		if err := r.appendAdviceAgreement(ctx, terminal, terminalReportEventID, terminal); err != nil {
			return err
		}
	}
	return nil
}

func isAdviseVerdict(report Report) bool {
	return report.Version >= reportVersion && config.GatekeeperTrustLevel(strings.TrimSpace(report.Mode)) == config.GatekeeperTrustAdvise
}

func isTerminalAdviceReport(report Report) bool {
	switch strings.ToUpper(strings.TrimSpace(report.Evidence.PullRequestState)) {
	case "CLOSED", "MERGED":
		return true
	default:
		return strings.TrimSpace(report.Evidence.MergedAt) != ""
	}
}

func (r *Runner) appendAdviceAgreement(ctx context.Context, terminal Report, verdictEventID string, verdict Report) error {
	outcome, agreement := classifyAdviceOutcome(verdict, terminal)
	entityType := "pull_request"
	entityID := fmt.Sprintf("%s#%d", terminal.Repo, terminal.PRNumber)
	projectID := terminal.ProjectID
	agreementRecord := AdviceAgreement{
		Version: 1, VerdictEventID: verdictEventID,
		ProjectID: projectID, Repo: terminal.Repo, PRNumber: terminal.PRNumber,
		VerdictEligible: verdict.Eligible, VerdictHeadSHA: verdict.ObservedHeadSHA,
		Outcome: outcome, Agreement: agreement,
		TerminalState:   strings.ToUpper(strings.TrimSpace(terminal.Evidence.PullRequestState)),
		TerminalHeadSHA: terminalHeadSHA(terminal), TerminalAt: terminalTime(terminal),
		RecordedAt: r.now().UTC().Format(time.RFC3339Nano),
	}
	if err := eventlog.Append(ctx, r.repos, eventlog.AppendInput{
		ID:        "agreement_" + verdictEventID,
		EventType: AdviceAgreementEventType, ProjectID: &projectID, EntityType: &entityType, EntityID: &entityID,
		CausationID: &verdictEventID, Payload: agreementRecord, CreatedAt: r.now(),
	}); err != nil {
		if r.adviceAgreementExists(ctx, entityID, verdictEventID) {
			return nil
		}
		return fmt.Errorf("persist advise agreement: %w", err)
	}
	return nil
}

func (r *Runner) adviceAgreementExists(ctx context.Context, entityID, verdictEventID string) bool {
	events, err := r.repos.Events.ListByEntity(ctx, "pull_request", entityID)
	if err != nil {
		return false
	}
	for _, event := range events {
		if event.EventType == AdviceAgreementEventType && event.CausationID != nil && *event.CausationID == verdictEventID {
			return true
		}
	}
	return false
}

func classifyAdviceOutcome(verdict, terminal Report) (AdviceOutcome, bool) {
	if strings.TrimSpace(terminal.Evidence.MergedAt) != "" || strings.EqualFold(terminal.Evidence.PullRequestState, "MERGED") {
		if !verdict.Eligible {
			return AdviceOutcomeOverridden, false
		}
		if strings.TrimSpace(verdict.ObservedHeadSHA) == terminalHeadSHA(terminal) {
			return AdviceOutcomeMergedAsIs, true
		}
		return AdviceOutcomeMergedAfterChange, false
	}
	if len(terminal.Evidence.HoldLabels) > 0 {
		return AdviceOutcomeHeld, false
	}
	return AdviceOutcomeClosed, false
}

func terminalHeadSHA(report Report) string {
	if head := strings.TrimSpace(report.Evidence.FinalObservedHeadSHA); head != "" {
		return head
	}
	return strings.TrimSpace(report.ObservedHeadSHA)
}

func terminalTime(report Report) string {
	if mergedAt := strings.TrimSpace(report.Evidence.MergedAt); mergedAt != "" {
		return mergedAt
	}
	if closedAt := strings.TrimSpace(report.Evidence.ClosedAt); closedAt != "" {
		return closedAt
	}
	if evaluatedAt := strings.TrimSpace(report.EvaluatedAt); evaluatedAt != "" {
		return evaluatedAt
	}
	return ""
}
