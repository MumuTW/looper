package gatekeeper

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/eventlog"
	"github.com/MumuTW/looper/internal/storage"
)

// AdviceAgreementEventType records the forge-observed outcome of one advise
// verdict in one terminal lifecycle epoch. The Gate report remains the
// authority for what Gatekeeper said; the terminal pull-request state returned
// by the forge is the authority for what happened afterwards.
const AdviceAgreementEventType = "pull_request.merge_gate.advice_agreement_recorded"

type AdviceOutcome string

const (
	AdviceOutcomeMergedAsIs        AdviceOutcome = "merged_as_is"
	AdviceOutcomeMergedAfterChange AdviceOutcome = "merged_after_changes"
	AdviceOutcomeClosed            AdviceOutcome = "closed"
	AdviceOutcomeHeld              AdviceOutcome = "held"
	AdviceOutcomeOverridden        AdviceOutcome = "overridden"
)

// AdviceAgreement is immutable attribution for an advise verdict in a
// terminal lifecycle epoch. CausationID points at the exact Gate report it
// resolves, so a later verdict cannot change the historical outcome of an
// earlier epoch.
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
	// TerminalEpoch identifies the forge-observed closure/merge epoch. A
	// closure can later be reopened, so a subsequent terminal observation is a
	// distinct immutable agreement rather than being suppressed by the first
	// closed-state record.
	TerminalEpoch string `json:"terminalEpoch"`
	RecordedAt    string `json:"recordedAt"`
}

// recordTerminalAdviceOutcomes resolves every as-yet-unresolved advise verdict
// for the observed terminal lifecycle epoch. It deliberately makes no
// additional forge call: the terminal Report already contains the fresh
// provider state that caused this evaluation.
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
		if record.EventType != AdviceAgreementEventType {
			continue
		}
		if projectID := eventProjectID(record); projectID != "" && projectID != terminal.ProjectID {
			continue
		}
		if key := agreementResolutionKeyFromRecord(record); key != "" {
			resolved[key] = struct{}{}
		}
	}
	for _, record := range events {
		if record.EventType != GateReportEventType || record.ID == terminalReportEventID {
			continue
		}
		var verdict Report
		if err := json.Unmarshal([]byte(record.PayloadJSON), &verdict); err != nil {
			continue
		}
		if !isAdviseVerdict(verdict) {
			continue
		}
		if verdict.ProjectID != "" && verdict.ProjectID != terminal.ProjectID {
			continue
		}
		if key := agreementResolutionKey(record.ID, terminal); key != "" {
			if _, alreadyResolved := resolved[key]; alreadyResolved {
				continue
			}
		}
		if err := r.appendAdviceAgreement(ctx, terminal, record.ID, verdict); err != nil {
			return err
		}
	}
	// The terminal evaluation is itself a verdict. Resolve it too, so every
	// advice verdict has an inspectable outcome even when a closed webhook is
	// the first evaluation Looper sees.
	if _, alreadyResolved := resolved[agreementResolutionKey(terminalReportEventID, terminal)]; !alreadyResolved && isAdviseVerdict(terminal) {
		if err := r.appendAdviceAgreement(ctx, terminal, terminalReportEventID, terminal); err != nil {
			return err
		}
	}
	return nil
}

func isAdviseVerdict(report Report) bool {
	// Version 1 and 2 both use the same advise meaning; version 1 only used
	// `observe_only` for its observe reports. Keep historical advise records
	// readable across upgrades, while refusing future schemas whose Mode may
	// have changed semantics.
	return report.Version > 0 && report.Version <= reportVersion && config.GatekeeperTrustLevel(strings.ToLower(strings.TrimSpace(report.Mode))) == config.GatekeeperTrustAdvise
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
		TerminalHeadSHA: terminalHeadSHA(terminal), TerminalAt: terminalTime(terminal), TerminalEpoch: terminalEpoch(terminal),
		RecordedAt: r.now().UTC().Format(time.RFC3339Nano),
	}
	agreementID := agreementEventID(verdictEventID, terminal)
	if err := eventlog.Append(ctx, r.repos, eventlog.AppendInput{
		ID:        agreementID,
		EventType: AdviceAgreementEventType, ProjectID: &projectID, EntityType: &entityType, EntityID: &entityID,
		CausationID: &verdictEventID, Payload: agreementRecord, CreatedAt: r.now(),
	}); err != nil {
		if r.adviceAgreementExists(ctx, entityID, agreementID) {
			return nil
		}
		return fmt.Errorf("persist advise agreement: %w", err)
	}
	return nil
}

func (r *Runner) adviceAgreementExists(ctx context.Context, entityID, agreementID string) bool {
	events, err := r.repos.Events.ListByEntity(ctx, "pull_request", entityID)
	if err != nil {
		return false
	}
	for _, event := range events {
		if event.EventType == AdviceAgreementEventType && event.ID == agreementID {
			return true
		}
	}
	return false
}

func eventProjectID(record storage.EventLogRecord) string {
	if record.ProjectID == nil {
		return ""
	}
	return *record.ProjectID
}

func agreementResolutionKeyFromRecord(record storage.EventLogRecord) string {
	if record.CausationID == nil || strings.TrimSpace(*record.CausationID) == "" {
		return ""
	}
	var agreement AdviceAgreement
	if err := json.Unmarshal([]byte(record.PayloadJSON), &agreement); err != nil {
		return *record.CausationID
	}
	epoch := agreement.TerminalEpoch
	if epoch == "" {
		epoch = terminalEpochFromFields(agreement.TerminalState, agreement.TerminalHeadSHA, agreement.TerminalAt)
	}
	return agreementResolutionKeyFromEpoch(*record.CausationID, epoch)
}

func agreementResolutionKey(verdictEventID string, terminal Report) string {
	return agreementResolutionKeyFromEpoch(verdictEventID, terminalEpoch(terminal))
}

func agreementResolutionKeyFromEpoch(verdictEventID, epoch string) string {
	if strings.TrimSpace(verdictEventID) == "" {
		return ""
	}
	return verdictEventID + "\x00" + epoch
}

func terminalEpoch(report Report) string {
	return terminalEpochFromFields(
		strings.ToUpper(strings.TrimSpace(report.Evidence.PullRequestState)),
		terminalHeadSHA(report),
		terminalTime(report),
	)
}

func terminalEpochFromFields(state, headSHA, terminalAt string) string {
	return state + "\x00" + strings.TrimSpace(headSHA) + "\x00" + strings.TrimSpace(terminalAt)
}

func agreementEventID(verdictEventID string, terminal Report) string {
	sum := sha256.Sum256([]byte(terminalEpoch(terminal)))
	return "agreement_" + verdictEventID + "_" + hex.EncodeToString(sum[:8])
}

func classifyAdviceOutcome(verdict, terminal Report) (AdviceOutcome, bool) {
	if strings.TrimSpace(terminal.Evidence.MergedAt) != "" || strings.EqualFold(terminal.Evidence.PullRequestState, "MERGED") {
		// A changed head invalidates the old verdict before eligibility is
		// considered. A blocked verdict that is later merged after new commits
		// is not an "overridden" same-head decision.
		if strings.TrimSpace(verdict.ObservedHeadSHA) != terminalHeadSHA(terminal) {
			return AdviceOutcomeMergedAfterChange, false
		}
		if !verdict.Eligible {
			return AdviceOutcomeOverridden, false
		}
		if terminalHasNonTerminalBlockers(terminal) {
			return AdviceOutcomeOverridden, false
		}
		return AdviceOutcomeMergedAsIs, true
	}
	if len(terminal.Evidence.HoldLabels) > 0 {
		return AdviceOutcomeHeld, !verdict.Eligible
	}
	return AdviceOutcomeClosed, !verdict.Eligible
}

func terminalHasNonTerminalBlockers(terminal Report) bool {
	for _, reason := range terminal.Reasons {
		if reason.Code != ReasonPullRequestNotOpen {
			return true
		}
	}
	return false
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
