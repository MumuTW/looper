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
	entityType := "branch_head"
	events, err := repos.Events.ListByProjectAndEntityType(ctx, project.ID, entityType)
	if err != nil {
		return err
	}
	observationEvent, observation, entityID, ok, err := latestPendingAuditorObservation(events, project.ID, repo, headSHA)
	if err != nil || !ok {
		return err
	}

	requests, err := auditorRerunRequests(events, observationEvent.ID)
	if err != nil {
		return err
	}
	if len(observation.CheckSuiteIDs) == 0 {
		return appendAuditorConfirmation(ctx, repos, project.ID, entityType, entityID, observationEvent.ID, observation.HeadSHA, auditor.ConfirmationResult{Outcome: auditor.ConfirmationInconclusive}, auditor.Decision{Action: auditor.ActionEscalate, Reason: "missing_failed_check_suite_identity"}, now())
	}
	checks, err := gateway.ListPullRequestCheckRuns(ctx, githubinfra.PullRequestCheckRunsInput{Repo: repo, Ref: observation.HeadSHA, CWD: project.RepoPath})
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
			if err := appendAuditorRerunRequest(ctx, repos, project.ID, entityType, entityID, observationEvent.ID, repo, observation.HeadSHA, suiteID, requestedAt, observation.FailedChecks, observation.FailingPaths, observation.FailingPathsByCheck); err != nil {
				return err
			}
			return nil
		}
		requestedAt := now().UTC()
		if err := gateway.RerequestCheckSuite(ctx, githubinfra.RerequestCheckSuiteInput{Repo: repo, CheckSuiteID: suiteID, CWD: project.RepoPath}); err != nil {
			return fmt.Errorf("auditor rerequest check suite %d: %w", suiteID, err)
		}
		if err := appendAuditorRerunRequest(ctx, repos, project.ID, entityType, entityID, observationEvent.ID, repo, observation.HeadSHA, suiteID, requestedAt, observation.FailedChecks, observation.FailingPaths, observation.FailingPathsByCheck); err != nil {
			return err
		}
		return nil
	}

	rerunCompleted, rerunFailedChecks := completedAuditorRerun(checks, observation.CheckSuiteIDs, requests)
	if !rerunCompleted {
		if auditorRerunTimedOut(requests, now(), defaultAuditorRerunTimeout) {
			return appendAuditorConfirmation(ctx, repos, project.ID, entityType, entityID, observationEvent.ID, observation.HeadSHA, auditor.ConfirmationResult{Outcome: auditor.ConfirmationInconclusive}, auditor.Decision{Action: auditor.ActionEscalate, Reason: "rerun_timeout"}, now())
		}
		return nil
	}
	rerunPaths, rerunPathsByCheck, rerunPathsComplete := failedAuditorRerunPathEvidence(ctx, gateway, repo, project.RepoPath, checks, requests)
	if !observation.FailingPathEvidenceComplete || !rerunPathsComplete {
		return appendAuditorConfirmation(ctx, repos, project.ID, entityType, entityID, observationEvent.ID, observation.HeadSHA, auditor.ConfirmationResult{Outcome: auditor.ConfirmationInconclusive}, auditor.Decision{Action: auditor.ActionEscalate, Reason: "failure_path_evidence_incomplete"}, now())
	}
	confirmation := auditor.ConfirmFailure(auditor.ConfirmationInput{InitialFailedChecks: observation.FailedChecks, InitialFailedPaths: observation.FailingPaths, InitialFailedPathsByCheck: observation.FailingPathsByCheck, InitialFailureSignatures: observation.FailureSignatures, RerunCompleted: true, RerunFailedChecks: rerunFailedChecks, RerunFailedPaths: rerunPaths, RerunFailedPathsByCheck: rerunPathsByCheck, RerunFailureSignatures: failureSignaturesFromPathMap(rerunPathsByCheck)})
	confirmedPathsByCheck := intersectAuditorPathsByCheck(observation.FailingPathsByCheck, rerunPathsByCheck, confirmation.ConfirmedChecks)
	decision, err := auditorConfirmationDecisionWithGateway(ctx, repos.Events, gateway, project.RepoPath, project.ID, repo, observation, confirmation, confirmedPathsByCheck, role.WindowMinutes)
	if err != nil {
		return err
	}
	return appendAuditorConfirmation(ctx, repos, project.ID, entityType, entityID, observationEvent.ID, observation.HeadSHA, confirmation, decision, now())
}

const defaultAuditorRerunTimeout = 60 * time.Minute

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

func latestPendingAuditorObservation(events []storage.EventLogRecord, projectID, repo, currentHeadSHA string) (storage.EventLogRecord, auditor.FailureObservation, string, bool, error) {
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		if event.EventType != auditor.ObservedFailureEventType || event.EntityID == nil {
			continue
		}
		var observation auditor.FailureObservation
		if err := json.Unmarshal([]byte(event.PayloadJSON), &observation); err != nil {
			return storage.EventLogRecord{}, auditor.FailureObservation{}, "", false, fmt.Errorf("decode auditor observation event %s: %w", event.ID, err)
		}
		if observation.ProjectID != projectID || !strings.EqualFold(observation.Repo, repo) || hasAuditorConfirmation(events, event.ID) {
			continue
		}
		return event, observation, strings.TrimSpace(*event.EntityID), true, nil
	}
	event, observation, ok, err := latestAuditorObservation(events, projectID, repo, currentHeadSHA)
	if err != nil || !ok {
		return event, observation, strings.TrimSpace(repo + "@" + currentHeadSHA), ok, err
	}
	if hasAuditorConfirmation(events, event.ID) || event.EntityID == nil {
		return storage.EventLogRecord{}, auditor.FailureObservation{}, "", false, nil
	}
	return event, observation, strings.TrimSpace(*event.EntityID), true, nil
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
	if checks.TotalCount > len(checks.CheckRuns) || checks.StatusesTotalCount > len(checks.Statuses) {
		return false, nil
	}
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
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(check.Status), "completed") {
			return false, nil
		}
		switch strings.ToLower(strings.TrimSpace(check.Conclusion)) {
		case "success":
			seen[check.CheckSuiteID] = true
		case "failure", "cancelled", "timed_out", "action_required", "startup_failure":
			seen[check.CheckSuiteID] = true
			failed[strings.TrimSpace(check.Name)] = struct{}{}
		default:
			return false, nil
		}
	}
	for _, status := range checks.Statuses {
		switch strings.ToLower(strings.TrimSpace(status.State)) {
		case "success":
			continue
		case "failure", "error":
			if contextName := strings.TrimSpace(status.Context); contextName != "" {
				failed[contextName] = struct{}{}
			}
		default:
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

func failedAuditorRerunEvidence(ctx context.Context, gateway auditorGateway, repo, cwd string, checks githubinfra.PullRequestCheckRuns, requests map[int64]time.Time) ([]string, []auditor.FailurePathSignature, bool) {
	paths, byCheck, complete := failedAuditorRerunPathEvidence(ctx, gateway, repo, cwd, checks, requests)
	return paths, failureSignaturesFromPathMap(byCheck), complete
}

func failureSignaturesFromPathMap(pathsByCheck map[string][]string) []auditor.FailurePathSignature {
	if len(pathsByCheck) == 0 {
		return nil
	}
	signatures := make([]auditor.FailurePathSignature, 0, len(pathsByCheck))
	for check, paths := range pathsByCheck {
		signatures = append(signatures, auditor.FailurePathSignature{Check: check, Paths: append([]string(nil), paths...)})
	}
	sort.Slice(signatures, func(i, j int) bool {
		return strings.ToLower(strings.TrimSpace(signatures[i].Check)) < strings.ToLower(strings.TrimSpace(signatures[j].Check))
	})
	return signatures
}

func checkRunStartedAfter(check githubinfra.PullRequestCheckRun, requestedAt time.Time) bool {
	if startedAt, err := time.Parse(time.RFC3339Nano, check.StartedAt); err == nil {
		return !startedAt.UTC().Before(requestedAt.UTC())
	}
	if completedAt, err := time.Parse(time.RFC3339Nano, check.CompletedAt); err == nil {
		return !completedAt.UTC().Before(requestedAt.UTC())
	}
	return false
}

func auditorConfirmationDecision(ctx context.Context, events *storage.EventsRepository, projectID, repo string, observation auditor.FailureObservation, confirmation auditor.ConfirmationResult, confirmedPathsByCheck map[string][]string, windowMinutes int) (auditor.Decision, error) {
	return auditorConfirmationDecisionWithGateway(ctx, events, nil, "", projectID, repo, observation, confirmation, confirmedPathsByCheck, windowMinutes)
}

func auditorConfirmationDecisionWithGateway(ctx context.Context, events *storage.EventsRepository, gateway auditorGateway, cwd, projectID, repo string, observation auditor.FailureObservation, confirmation auditor.ConfirmationResult, confirmedPathsByCheck map[string][]string, windowMinutes int) (auditor.Decision, error) {
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
			if observation.BaselineObservedAt != "" {
				baselineAt, baselineErr := time.Parse(time.RFC3339Nano, observation.BaselineObservedAt)
				if baselineErr == nil && candidate.MergedAt.Before(baselineAt) {
					continue
				}
			}
			if !candidate.TouchedFilesAvailable && gateway != nil {
				files, filesErr := gateway.ListPullRequestFiles(ctx, githubinfra.ViewPullRequestInput{Repo: repo, PRNumber: candidate.PRNumber, CWD: cwd})
				if filesErr == nil {
					candidate.TouchedFiles = files
					candidate.TouchedFilesAvailable = true
				}
			}
			projectCandidates = append(projectCandidates, candidate)
		}
	}
	pathsByCheck := observation.FailingPathsByCheck
	if confirmedPathsByCheck != nil {
		pathsByCheck = confirmedPathsByCheck
	}
	failingPaths := auditorPathsForChecks(pathsByCheck, confirmation.ConfirmedChecks)
	if len(failingPaths) == 0 && len(pathsByCheck) == 0 {
		failingPaths = append([]string(nil), observation.FailingPaths...)
	}
	if len(observation.FailingPathsByCheck) == 0 && len(observation.FailingPaths) > 0 {
		return auditor.Decision{Action: auditor.ActionEscalate, Reason: "failure_path_evidence_incomplete"}, nil
	}
	attribution := auditor.Attribute(auditor.FailureEvidence{ObservedAt: observedAt, FailingPaths: failingPaths, BaselineKnown: observation.BaselineKnown, FailingPathEvidenceComplete: observation.FailingPathEvidenceComplete}, projectCandidates)
	decision := auditor.Decide(confirmation, attribution)
	if attribution.Confidence != auditor.ConfidenceHigh || attribution.Candidate == nil {
		return decision, nil
	}
	candidate := attribution.Candidate
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
	if decision.Action != auditor.ActionProposeRevert {
		return auditor.Decision{Action: auditor.ActionEscalate, Reason: "missing_revert_decision_provenance"}, nil
	}
	decision.Candidate = candidate
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
	record := auditor.ConfirmationRecord{Version: 2, ObservationEventID: observationEventID, HeadSHA: headSHA, Outcome: confirmation.Outcome, ConfirmedChecks: confirmation.ConfirmedChecks, Decision: decision.Action, Reason: decision.Reason, Candidate: candidate, ConfirmedAt: eventlog.FormatJavaScriptISOString(confirmedAt)}
	if decision.Candidate != nil {
		record.CandidatePRNumber = decision.Candidate.PRNumber
		record.CandidateHeadSHA = decision.Candidate.HeadSHA
		record.CandidateMergeCommitSHA = decision.Candidate.MergeCommitSHA
		record.CandidateSourceIssue = decision.Candidate.SourceIssue
	}
	return eventlog.Append(ctx, repos, eventlog.AppendInput{
		EventType: auditor.ConfirmationEventType, ProjectID: &projectID, EntityType: &entityType, EntityID: &entityID, CausationID: &observationEventID,
		Payload: record, CreatedAt: confirmedAt,
	})
}

func confirmedAuditorCandidate(decision auditor.Decision) *auditor.ConfirmedCandidate {
	if decision.Action != auditor.ActionProposeRevert || decision.Candidate == nil || decision.Candidate.SourceIssue == nil {
		return nil
	}
	return &auditor.ConfirmedCandidate{PRNumber: decision.Candidate.PRNumber, MergeCommitSHA: decision.Candidate.MergeCommitSHA, MergeStrategy: decision.Candidate.MergeStrategy, SourceIssueNumber: decision.Candidate.SourceIssue.Number, SourceIssueRepo: decision.Candidate.SourceIssue.Repo}
}
