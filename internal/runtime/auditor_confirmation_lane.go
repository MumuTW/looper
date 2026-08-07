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
			if err := appendAuditorRerunRequest(ctx, repos, project.ID, entityType, entityID, observationEvent.ID, repo, headSHA, suiteID, requestedAt, observation.FailedChecks, observation.FailingPaths, observation.FailingPathsByCheck); err != nil {
				return err
			}
			return nil
		}
		requestedAt := now().UTC()
		if err := gateway.RerequestCheckSuite(ctx, githubinfra.RerequestCheckSuiteInput{Repo: repo, CheckSuiteID: suiteID, CWD: project.RepoPath}); err != nil {
			return fmt.Errorf("auditor rerequest check suite %d: %w", suiteID, err)
		}
		if err := appendAuditorRerunRequest(ctx, repos, project.ID, entityType, entityID, observationEvent.ID, repo, headSHA, suiteID, requestedAt, observation.FailedChecks, observation.FailingPaths, observation.FailingPathsByCheck); err != nil {
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
	rerunPaths, rerunPathsByCheck, rerunPathsComplete := failedAuditorRerunPathEvidence(ctx, gateway, repo, project.RepoPath, checks, requests)
	if !observation.FailingPathEvidenceComplete || !rerunPathsComplete {
		return appendAuditorConfirmation(ctx, repos, project.ID, entityType, entityID, observationEvent.ID, headSHA, auditor.ConfirmationResult{Outcome: auditor.ConfirmationInconclusive}, auditor.Decision{Action: auditor.ActionEscalate, Reason: "failure_path_evidence_incomplete"}, now())
	}
	confirmation := auditor.ConfirmFailure(auditor.ConfirmationInput{InitialFailedChecks: observation.FailedChecks, InitialFailedPaths: observation.FailingPaths, InitialFailedPathsByCheck: observation.FailingPathsByCheck, RerunCompleted: true, RerunFailedChecks: rerunFailedChecks, RerunFailedPaths: rerunPaths, RerunFailedPathsByCheck: rerunPathsByCheck})
	confirmedPathsByCheck := intersectAuditorPathsByCheck(observation.FailingPathsByCheck, rerunPathsByCheck, confirmation.ConfirmedChecks)
	decision, err := auditorConfirmationDecision(ctx, repos.Events, project.ID, repo, observation, confirmation, confirmedPathsByCheck, role.WindowMinutes)
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

func failedAuditorRerunPathEvidence(ctx context.Context, gateway auditorGateway, repo, cwd string, checks githubinfra.PullRequestCheckRuns, requests map[int64]time.Time) ([]string, map[string][]string, bool) {
	filtered := checks
	filtered.CheckRuns = make([]githubinfra.PullRequestCheckRun, 0, len(checks.CheckRuns))
	for _, check := range checks.CheckRuns {
		requestedAt, ok := requests[check.CheckSuiteID]
		if ok && checkRunStartedAfter(check, requestedAt) {
			filtered.CheckRuns = append(filtered.CheckRuns, check)
		}
	}
	paths, pathsByCheck, complete := failedAuditorCheckPathEvidence(ctx, gateway, repo, cwd, filtered)
	if checks.TotalCount > len(checks.CheckRuns) || checks.StatusesTotalCount > len(checks.Statuses) {
		complete = false
	}
	return paths, pathsByCheck, complete
}

func checkRunStartedAfter(check githubinfra.PullRequestCheckRun, requestedAt time.Time) bool {
	startedAt, err := time.Parse(time.RFC3339Nano, check.StartedAt)
	if err != nil {
		return false
	}
	return !startedAt.UTC().Before(requestedAt.UTC())
}

func auditorConfirmationDecision(ctx context.Context, events *storage.EventsRepository, projectID, repo string, observation auditor.FailureObservation, confirmation auditor.ConfirmationResult, confirmedPathsByCheck map[string][]string, windowMinutes int) (auditor.Decision, error) {
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
	pathsByCheck := observation.FailingPathsByCheck
	if confirmedPathsByCheck != nil {
		pathsByCheck = confirmedPathsByCheck
	}
	failingPaths := auditorPathsForChecks(pathsByCheck, confirmation.ConfirmedChecks)
	if len(observation.FailingPathsByCheck) == 0 && len(observation.FailingPaths) > 0 {
		return auditor.Decision{Action: auditor.ActionEscalate, Reason: "failure_path_evidence_incomplete"}, nil
	}
	attribution := auditor.Attribute(auditor.FailureEvidence{ObservedAt: observedAt, FailingPaths: failingPaths, BaselineKnown: observation.BaselineKnown, FailingPathEvidenceComplete: observation.FailingPathEvidenceComplete}, projectCandidates)
	decision := auditor.Decide(confirmation, attribution)
	if decision.Action != auditor.ActionProposeRevert || decision.Candidate == nil {
		return decision, nil
	}
	candidate := decision.Candidate
	mergeStrategy := strings.ToLower(strings.TrimSpace(candidate.MergeStrategy))
	if mergeStrategy == "" {
		return auditor.Decision{Action: auditor.ActionEscalate, Reason: "missing_merge_strategy_provenance"}, nil
	}
	if mergeStrategy == string(config.ReviewerAutoMergeStrategyRebase) {
		return auditor.Decision{Action: auditor.ActionEscalate, Reason: "rebase_merge_not_revertable"}, nil
	}
	if mergeStrategy != string(config.ReviewerAutoMergeStrategySquash) && mergeStrategy != string(config.ReviewerAutoMergeStrategyMerge) {
		return auditor.Decision{Action: auditor.ActionEscalate, Reason: "unsupported_merge_strategy"}, nil
	}
	if strings.TrimSpace(candidate.MergeCommitSHA) == "" {
		return auditor.Decision{Action: auditor.ActionEscalate, Reason: "missing_merge_commit_provenance"}, nil
	}
	if candidate.SourceIssue == nil || candidate.SourceIssue.Number <= 0 || !strings.EqualFold(strings.TrimSpace(candidate.SourceIssue.Repo), strings.TrimSpace(repo)) {
		return auditor.Decision{Action: auditor.ActionEscalate, Reason: "missing_unambiguous_source_issue"}, nil
	}
	return decision, nil
}

func intersectAuditorPathsByCheck(initial, rerun map[string][]string, checks []string) map[string][]string {
	if len(initial) == 0 || len(checks) == 0 {
		return nil
	}
	wanted := make(map[string]struct{}, len(checks))
	for _, check := range checks {
		if normalized := strings.ToLower(strings.TrimSpace(check)); normalized != "" {
			wanted[normalized] = struct{}{}
		}
	}
	result := make(map[string][]string)
	if len(rerun) == 0 {
		return result
	}
	for initialName, initialPaths := range initial {
		normalizedName := strings.ToLower(strings.TrimSpace(initialName))
		if _, ok := wanted[normalizedName]; !ok {
			continue
		}
		var rerunPaths []string
		for rerunName, paths := range rerun {
			if strings.EqualFold(strings.TrimSpace(rerunName), strings.TrimSpace(initialName)) {
				rerunPaths = paths
				break
			}
		}
		if len(rerunPaths) == 0 {
			continue
		}
		rerunSet := make(map[string]struct{}, len(rerunPaths))
		for _, path := range rerunPaths {
			if normalized := strings.TrimSpace(path); normalized != "" {
				rerunSet[normalized] = struct{}{}
			}
		}
		for _, path := range initialPaths {
			normalized := strings.TrimSpace(path)
			if _, ok := rerunSet[normalized]; ok && normalized != "" {
				result[initialName] = append(result[initialName], normalized)
			}
		}
		if len(result[initialName]) == 0 {
			delete(result, initialName)
		}
	}
	return result
}

func cloneAuditorPathMap(paths map[string][]string) map[string][]string {
	if len(paths) == 0 {
		return nil
	}
	clone := make(map[string][]string, len(paths))
	for check, values := range paths {
		clone[check] = append([]string(nil), values...)
	}
	return clone
}

func auditorPathsForChecks(pathsByCheck map[string][]string, checks []string) []string {
	if len(pathsByCheck) == 0 || len(checks) == 0 {
		return nil
	}
	set := make(map[string]struct{})
	for _, check := range checks {
		for name, paths := range pathsByCheck {
			if !strings.EqualFold(strings.TrimSpace(name), strings.TrimSpace(check)) {
				continue
			}
			for _, path := range paths {
				if path = strings.TrimSpace(path); path != "" {
					set[path] = struct{}{}
				}
			}
		}
	}
	result := make([]string, 0, len(set))
	for path := range set {
		result = append(result, path)
	}
	sort.Strings(result)
	return result
}

func appendAuditorRerunRequest(ctx context.Context, repos *storage.Repositories, projectID, entityType, entityID, observationEventID, repo, headSHA string, suiteID int64, requestedAt time.Time, initialFailedChecks, initialFailedPaths []string, initialFailedPathsByCheck map[string][]string) error {
	return eventlog.Append(ctx, repos, eventlog.AppendInput{
		EventType: auditor.RerunRequestedEventType, ProjectID: &projectID, EntityType: &entityType, EntityID: &entityID, CausationID: &observationEventID,
		Payload: auditor.RerunRequest{Version: 1, ObservationEventID: observationEventID, Repo: repo, HeadSHA: headSHA, CheckSuiteID: suiteID, InitialFailedChecks: append([]string(nil), initialFailedChecks...), InitialFailedPaths: append([]string(nil), initialFailedPaths...), InitialFailedPathsByCheck: cloneAuditorPathMap(initialFailedPathsByCheck), RequestedAt: eventlog.FormatJavaScriptISOString(requestedAt)}, CreatedAt: requestedAt,
	})
}

func appendAuditorConfirmation(ctx context.Context, repos *storage.Repositories, projectID, entityType, entityID, observationEventID, headSHA string, confirmation auditor.ConfirmationResult, decision auditor.Decision, confirmedAt time.Time) error {
	candidate := confirmedAuditorCandidate(decision)
	return eventlog.Append(ctx, repos, eventlog.AppendInput{
		EventType: auditor.ConfirmationEventType, ProjectID: &projectID, EntityType: &entityType, EntityID: &entityID, CausationID: &observationEventID,
		Payload: auditor.ConfirmationRecord{Version: 2, ObservationEventID: observationEventID, HeadSHA: headSHA, Outcome: confirmation.Outcome, ConfirmedChecks: confirmation.ConfirmedChecks, Decision: decision.Action, Reason: decision.Reason, Candidate: candidate, ConfirmedAt: eventlog.FormatJavaScriptISOString(confirmedAt)}, CreatedAt: confirmedAt,
	})
}

func confirmedAuditorCandidate(decision auditor.Decision) *auditor.ConfirmedCandidate {
	if decision.Action != auditor.ActionProposeRevert || decision.Candidate == nil || decision.Candidate.SourceIssue == nil {
		return nil
	}
	return &auditor.ConfirmedCandidate{PRNumber: decision.Candidate.PRNumber, MergeCommitSHA: decision.Candidate.MergeCommitSHA, MergeStrategy: decision.Candidate.MergeStrategy, SourceIssueNumber: decision.Candidate.SourceIssue.Number, SourceIssueRepo: decision.Candidate.SourceIssue.Repo}
}
