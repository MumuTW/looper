package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/MumuTW/looper/internal/auditor"
	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/eventlog"
	githubinfra "github.com/MumuTW/looper/internal/infra/github"
	"github.com/MumuTW/looper/internal/storage"
)

// progressAuditorConfirmation advances one observed failure on the current
// default-branch head. Each successful GitHub rerequest is recorded before a
// later tick reads its result, so normal scheduler replay never rerequests a
// suite that already has durable acceptance evidence.
func progressAuditorConfirmation(ctx context.Context, repos *storage.Repositories, gateway auditorGateway, project storage.ProjectRecord, repo, baseBranch string, role config.AuditorRoleConfig, now func() time.Time) error {
	if repos == nil || repos.Events == nil || gateway == nil || !role.Enabled {
		return nil
	}
	if role.WindowMinutes <= 0 {
		return fmt.Errorf("auditor enabled for project %s without a positive observation window", project.ID)
	}
	if now == nil {
		now = time.Now
	}
	baseBranch = strings.TrimSpace(baseBranch)
	if baseBranch == "" {
		return fmt.Errorf("auditor project %s has no base branch", project.ID)
	}
	headSHA, err := gateway.GetBranchHeadSHA(ctx, githubinfra.BranchHeadInput{Repo: repo, Branch: baseBranch, CWD: project.RepoPath})
	if err != nil {
		return fmt.Errorf("auditor read default branch head for confirmation: %w", err)
	}
	headSHA = strings.TrimSpace(headSHA)
	if headSHA == "" {
		return fmt.Errorf("auditor read default branch head for confirmation: empty SHA")
	}
	entityType, entityID := "branch_head", repo+"@"+headSHA
	events, err := repos.Events.ListByEntity(ctx, entityType, entityID)
	if err != nil {
		return err
	}
	observationEvent, observation, ok, err := latestAuditorObservation(events, project.ID, repo, headSHA)
	if err != nil || !ok {
		return err
	}
	if hasAuditorConfirmation(events, observationEvent.ID) {
		return nil
	}

	requests, err := auditorRerunRequests(events, observationEvent.ID)
	if err != nil {
		return err
	}
	if len(observation.CheckSuiteIDs) == 0 {
		return appendAuditorConfirmation(ctx, repos, project.ID, entityType, entityID, observationEvent.ID, headSHA, auditor.ConfirmationResult{Outcome: auditor.ConfirmationInconclusive}, auditor.Decision{Action: auditor.ActionEscalate, Reason: "missing_failed_check_suite_identity"}, now())
	}
	checks, err := gateway.ListPullRequestCheckRuns(ctx, githubinfra.PullRequestCheckRunsInput{Repo: repo, Ref: headSHA, CWD: project.RepoPath})
	if err != nil {
		return fmt.Errorf("auditor read rerequested check suites: %w", err)
	}
	observedAt, observedAtErr := time.Parse(time.RFC3339Nano, observation.ObservedAt)
	if observedAtErr != nil {
		return fmt.Errorf("parse auditor observation timestamp: %w", observedAtErr)
	}
	for _, suiteID := range observation.CheckSuiteIDs {
		if _, exists := requests[suiteID]; exists {
			continue
		}
		if requestedAt, ok := auditorObservedRerun(checks, suiteID, observedAt); ok {
			if err := appendAuditorRerunRequest(ctx, repos, project.ID, entityType, entityID, observationEvent.ID, repo, headSHA, suiteID, requestedAt, observation.FailedChecks, observation.FailingPaths); err != nil {
				return err
			}
			return nil
		}
		requestedAt := now().UTC()
		if err := gateway.RerequestCheckSuite(ctx, githubinfra.RerequestCheckSuiteInput{Repo: repo, CheckSuiteID: suiteID, CWD: project.RepoPath}); err != nil {
			return fmt.Errorf("auditor rerequest check suite %d: %w", suiteID, err)
		}
		if err := appendAuditorRerunRequest(ctx, repos, project.ID, entityType, entityID, observationEvent.ID, repo, headSHA, suiteID, requestedAt, observation.FailedChecks, observation.FailingPaths); err != nil {
			return err
		}
		return nil
	}

	rerunCompleted, rerunFailedChecks := completedAuditorRerun(checks, observation.CheckSuiteIDs, requests)
	if !rerunCompleted {
		if auditorRerunTimedOut(requests, now(), time.Duration(role.WindowMinutes)*time.Minute) {
			return appendAuditorConfirmation(ctx, repos, project.ID, entityType, entityID, observationEvent.ID, headSHA, auditor.ConfirmationResult{Outcome: auditor.ConfirmationInconclusive}, auditor.Decision{Action: auditor.ActionEscalate, Reason: "rerun_timeout"}, now())
		}
		return nil
	}
	rerunPaths, rerunPathsComplete := failedAuditorRerunPaths(ctx, gateway, repo, project.RepoPath, checks, requests)
	if !observation.FailingPathEvidenceComplete || !rerunPathsComplete {
		return appendAuditorConfirmation(ctx, repos, project.ID, entityType, entityID, observationEvent.ID, headSHA, auditor.ConfirmationResult{Outcome: auditor.ConfirmationInconclusive}, auditor.Decision{Action: auditor.ActionEscalate, Reason: "failure_path_evidence_incomplete"}, now())
	}
	confirmation := auditor.ConfirmFailure(auditor.ConfirmationInput{InitialFailedChecks: observation.FailedChecks, InitialFailedPaths: observation.FailingPaths, RerunCompleted: true, RerunFailedChecks: rerunFailedChecks, RerunFailedPaths: rerunPaths})
	decision, err := auditorConfirmationDecision(ctx, repos.Events, project.ID, repo, observation, confirmation, role.WindowMinutes)
	if err != nil {
		return err
	}
	return appendAuditorConfirmation(ctx, repos, project.ID, entityType, entityID, observationEvent.ID, headSHA, confirmation, decision, now())
}

func latestAuditorObservation(events []storage.EventLogRecord, projectID, repo, headSHA string) (storage.EventLogRecord, auditor.FailureObservation, bool, error) {
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		if event.EventType != auditor.ObservedFailureEventType {
			continue
		}
		var observation auditor.FailureObservation
		if err := json.Unmarshal([]byte(event.PayloadJSON), &observation); err != nil {
			return storage.EventLogRecord{}, auditor.FailureObservation{}, false, fmt.Errorf("decode auditor observation event %s: %w", event.ID, err)
		}
		if observation.ProjectID != projectID || !strings.EqualFold(observation.Repo, repo) || !strings.EqualFold(observation.HeadSHA, headSHA) {
			continue
		}
		return event, observation, true, nil
	}
	return storage.EventLogRecord{}, auditor.FailureObservation{}, false, nil
}

func hasAuditorConfirmation(events []storage.EventLogRecord, observationEventID string) bool {
	for _, event := range events {
		if event.EventType == auditor.ConfirmationEventType && event.CausationID != nil && *event.CausationID == observationEventID {
			return true
		}
	}
	return false
}

func auditorRerunRequests(events []storage.EventLogRecord, observationEventID string) (map[int64]time.Time, error) {
	requests := make(map[int64]time.Time)
	for _, event := range events {
		if event.EventType != auditor.RerunRequestedEventType || event.CausationID == nil || *event.CausationID != observationEventID {
			continue
		}
		var request auditor.RerunRequest
		if err := json.Unmarshal([]byte(event.PayloadJSON), &request); err != nil {
			return nil, fmt.Errorf("decode auditor rerequest event %s: %w", event.ID, err)
		}
		requestedAt, err := time.Parse(time.RFC3339Nano, request.RequestedAt)
		if err != nil {
			return nil, fmt.Errorf("parse auditor rerequest event %s timestamp: %w", event.ID, err)
		}
		if request.CheckSuiteID > 0 {
			requests[request.CheckSuiteID] = requestedAt.UTC()
		}
	}
	return requests, nil
}

func completedAuditorRerun(checks githubinfra.PullRequestCheckRuns, suiteIDs []int64, requests map[int64]time.Time) (bool, []string) {
	wanted := make(map[int64]struct{}, len(suiteIDs))
	for _, suiteID := range suiteIDs {
		wanted[suiteID] = struct{}{}
	}
	seen := make(map[int64]bool, len(wanted))
	failed := make(map[string]struct{})
	for _, check := range checks.CheckRuns {
		if _, wantedSuite := wanted[check.CheckSuiteID]; !wantedSuite {
			continue
		}
		requestedAt, requested := requests[check.CheckSuiteID]
		if !requested {
			continue
		}
		if !checkRunStartedAfter(check, requestedAt) {
			if strings.EqualFold(strings.TrimSpace(check.Status), "completed") {
				continue
			}
			return false, nil
		}
		if !strings.EqualFold(strings.TrimSpace(check.Status), "completed") {
			return false, nil
		}
		seen[check.CheckSuiteID] = true
		if isFailedCheckState(check.Status, check.Conclusion) {
			failed[strings.TrimSpace(check.Name)] = struct{}{}
		}
	}
	for _, status := range checks.Statuses {
		if strings.EqualFold(strings.TrimSpace(status.State), "failure") || strings.EqualFold(strings.TrimSpace(status.State), "error") {
			return false, nil
		}
	}
	for suiteID := range wanted {
		if !seen[suiteID] {
			return false, nil
		}
	}
	result := make([]string, 0, len(failed))
	for name := range failed {
		if name != "" {
			result = append(result, name)
		}
	}
	sort.Strings(result)
	return true, result
}

func auditorObservedRerun(checks githubinfra.PullRequestCheckRuns, suiteID int64, observedAt time.Time) (time.Time, bool) {
	var latest time.Time
	for _, check := range checks.CheckRuns {
		if check.CheckSuiteID != suiteID {
			continue
		}
		if startedAt, err := time.Parse(time.RFC3339Nano, check.StartedAt); err == nil {
			if !startedAt.Before(observedAt) && startedAt.After(latest) {
				latest = startedAt
			}
			continue
		}
		// Some provider projections omit StartedAt. In that case completion is
		// the only post-observation evidence available; never use a completion
		// timestamp when a pre-observation start is known.
		if completedAt, err := time.Parse(time.RFC3339Nano, check.CompletedAt); err == nil && !completedAt.Before(observedAt) && completedAt.After(latest) {
			latest = completedAt
		}
	}
	return latest, !latest.IsZero()
}

func auditorRerunTimedOut(requests map[int64]time.Time, now time.Time, timeout time.Duration) bool {
	if len(requests) == 0 || timeout <= 0 {
		return false
	}
	for _, requestedAt := range requests {
		if now.Sub(requestedAt) < timeout {
			return false
		}
	}
	return true
}

func failedAuditorRerunPaths(ctx context.Context, gateway auditorGateway, repo, cwd string, checks githubinfra.PullRequestCheckRuns, requests map[int64]time.Time) ([]string, bool) {
	filtered := checks
	filtered.CheckRuns = make([]githubinfra.PullRequestCheckRun, 0, len(checks.CheckRuns))
	for _, check := range checks.CheckRuns {
		requestedAt, ok := requests[check.CheckSuiteID]
		if ok && checkRunStartedAfter(check, requestedAt) {
			filtered.CheckRuns = append(filtered.CheckRuns, check)
		}
	}
	return failedAuditorCheckPaths(ctx, gateway, repo, cwd, filtered)
}

func checkRunStartedAfter(check githubinfra.PullRequestCheckRun, requestedAt time.Time) bool {
	for _, value := range []string{check.StartedAt, check.CompletedAt} {
		when, err := time.Parse(time.RFC3339Nano, value)
		if err == nil && !when.Before(requestedAt) {
			return true
		}
	}
	return false
}

func auditorConfirmationDecision(ctx context.Context, events *storage.EventsRepository, projectID, repo string, observation auditor.FailureObservation, confirmation auditor.ConfirmationResult, windowMinutes int) (auditor.Decision, error) {
	if confirmation.Outcome != auditor.ConfirmationConfirmed {
		return auditor.Decide(confirmation, auditor.Attribution{}), nil
	}
	observedAt, err := time.Parse(time.RFC3339Nano, observation.ObservedAt)
	if err != nil {
		return auditor.Decision{}, fmt.Errorf("parse auditor observation timestamp: %w", err)
	}
	mergeEvents, err := events.ListSince(ctx, eventlog.FormatJavaScriptISOString(observedAt.Add(-time.Duration(windowMinutes)*time.Minute)))
	if err != nil {
		return auditor.Decision{}, err
	}
	candidates, err := auditor.CandidatesFromMergeOutcomes(mergeEvents)
	if err != nil {
		return auditor.Decision{}, err
	}
	projectCandidates := make([]auditor.MergeCandidate, 0, len(candidates))
	observedCandidates := make(map[int64]struct{}, len(observation.CandidatePRs))
	for _, prNumber := range observation.CandidatePRs {
		if prNumber > 0 {
			observedCandidates[prNumber] = struct{}{}
		}
	}
	for _, candidate := range candidates {
		if candidate.ProjectID == projectID && strings.EqualFold(candidate.Repo, repo) {
			if _, observed := observedCandidates[candidate.PRNumber]; !observed {
				continue
			}
			projectCandidates = append(projectCandidates, candidate)
		}
	}
	attribution := auditor.Attribute(auditor.FailureEvidence{ObservedAt: observedAt, FailingPaths: observation.FailingPaths, BaselineKnown: observation.BaselineKnown, FailingPathEvidenceComplete: observation.FailingPathEvidenceComplete}, projectCandidates)
	return auditor.Decide(confirmation, attribution), nil
}

func appendAuditorRerunRequest(ctx context.Context, repos *storage.Repositories, projectID, entityType, entityID, observationEventID, repo, headSHA string, suiteID int64, requestedAt time.Time, initialFailedChecks, initialFailedPaths []string) error {
	return eventlog.Append(ctx, repos, eventlog.AppendInput{
		EventType: auditor.RerunRequestedEventType, ProjectID: &projectID, EntityType: &entityType, EntityID: &entityID, CausationID: &observationEventID,
		Payload: auditor.RerunRequest{Version: 1, ObservationEventID: observationEventID, Repo: repo, HeadSHA: headSHA, CheckSuiteID: suiteID, InitialFailedChecks: append([]string(nil), initialFailedChecks...), InitialFailedPaths: append([]string(nil), initialFailedPaths...), RequestedAt: eventlog.FormatJavaScriptISOString(requestedAt)}, CreatedAt: requestedAt,
	})
}

func appendAuditorConfirmation(ctx context.Context, repos *storage.Repositories, projectID, entityType, entityID, observationEventID, headSHA string, confirmation auditor.ConfirmationResult, decision auditor.Decision, confirmedAt time.Time) error {
	return eventlog.Append(ctx, repos, eventlog.AppendInput{
		EventType: auditor.ConfirmationEventType, ProjectID: &projectID, EntityType: &entityType, EntityID: &entityID, CausationID: &observationEventID,
		Payload: auditor.ConfirmationRecord{Version: 1, ObservationEventID: observationEventID, HeadSHA: headSHA, Outcome: confirmation.Outcome, ConfirmedChecks: confirmation.ConfirmedChecks, Decision: decision.Action, Reason: decision.Reason, ConfirmedAt: eventlog.FormatJavaScriptISOString(confirmedAt)}, CreatedAt: confirmedAt,
	})
}
