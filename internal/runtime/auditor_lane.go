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

type auditorGateway interface {
	GetBranchHeadSHA(context.Context, githubinfra.BranchHeadInput) (string, error)
	ListPullRequestCheckRuns(context.Context, githubinfra.PullRequestCheckRunsInput) (githubinfra.PullRequestCheckRuns, error)
	ListCheckRunAnnotations(context.Context, githubinfra.CheckRunAnnotationsInput) ([]githubinfra.CheckRunAnnotation, error)
	ListPullRequestFiles(context.Context, githubinfra.ViewPullRequestInput) ([]string, error)
	RerequestCheckSuite(context.Context, githubinfra.RerequestCheckSuiteInput) error
}

// runAuditorLane observes, but never mutates, a project's default branch. The
// operator's roles.auditor setting is the authority to run it; a successful
// observation only appends durable evidence for a later confirmation/action pass.
func runAuditorLane(ctx context.Context, input defaultSchedulerTickInput, project storage.ProjectRecord, repo string) error {
	if input.Config == nil || input.Repos == nil || input.Repos.Events == nil || input.GitHubGateway == nil || strings.TrimSpace(repo) == "" {
		return nil
	}
	role := auditorRoleForProject(*input.Config, project.ID)
	if !role.Enabled {
		return nil
	}
	baseBranch := deployBaseBranch(*input.Config, project)
	if err := observePostMergeFailure(ctx, input.Repos, input.GitHubGateway, project, repo, baseBranch, role, input.Now); err != nil {
		return err
	}
	return progressAuditorConfirmation(ctx, input.Repos, input.GitHubGateway, project, repo, baseBranch, role, input.Now)
}

func observePostMergeFailure(ctx context.Context, repos *storage.Repositories, gateway auditorGateway, project storage.ProjectRecord, repo, baseBranch string, role config.AuditorRoleConfig, now func() time.Time) error {
	if repos == nil || repos.Events == nil || gateway == nil || !role.Enabled {
		return nil
	}
	events := repos.Events
	if role.WindowMinutes <= 0 {
		return fmt.Errorf("auditor enabled for project %s without a positive observation window", project.ID)
	}
	if now == nil {
		now = time.Now
	}
	observedAt := now().UTC()
	baseBranch = strings.TrimSpace(baseBranch)
	if baseBranch == "" {
		return fmt.Errorf("auditor project %s has no base branch", project.ID)
	}
	// Merge events are local durable evidence. Filter them before touching the
	// provider so an enabled auditor with no recent Looper merges costs no GitHub
	// API calls on every scheduler tick.
	since := eventlog.FormatJavaScriptISOString(observedAt.Add(-time.Duration(role.WindowMinutes) * time.Minute))
	mergeEvents, err := events.ListSince(ctx, since)
	if err != nil {
		return err
	}
	candidates, err := auditor.CandidatesFromMergeOutcomes(mergeEvents)
	if err != nil {
		return fmt.Errorf("auditor project merge evidence: %w", err)
	}
	candidatePRSet := make(map[int64]struct{}, len(candidates))
	for _, candidate := range candidates {
		if candidate.ProjectID == project.ID && strings.EqualFold(candidate.Repo, repo) {
			candidatePRSet[candidate.PRNumber] = struct{}{}
		}
	}
	if len(candidatePRSet) == 0 {
		// A recent clean baseline is enough to avoid repeating the provider read.
		// Without one, sample the provider even during a quiet period so the first
		// post-quiet merge can still be attributed against a durable baseline.
		if known, _ := auditorCleanBaseline(mergeEvents, project.ID, repo, since); known {
			return nil
		}
	}
	headSHA, err := gateway.GetBranchHeadSHA(ctx, githubinfra.BranchHeadInput{Repo: repo, Branch: baseBranch, CWD: project.RepoPath})
	if err != nil {
		return fmt.Errorf("auditor read default branch head: %w", err)
	}
	headSHA = strings.TrimSpace(headSHA)
	if headSHA == "" {
		return fmt.Errorf("auditor read default branch head: empty SHA")
	}
	checks, err := gateway.ListPullRequestCheckRuns(ctx, githubinfra.PullRequestCheckRunsInput{Repo: repo, Ref: headSHA, CWD: project.RepoPath})
	if err != nil {
		return fmt.Errorf("auditor read default branch checks: %w", err)
	}
	if checks.TotalCount > len(checks.CheckRuns) {
		return fmt.Errorf("auditor read default branch checks: truncated check-run snapshot (%d of %d)", len(checks.CheckRuns), checks.TotalCount)
	}
	failureEvidence := failedAuditorCheckEvidence(checks)
	if len(failureEvidence.Names) == 0 {
		if !cleanAuditorCheckEvidence(checks) {
			// Absence of a visible failure is not a clean baseline while any
			// check/status is pending, non-success, or truncated by pagination.
			return nil
		}
		return recordAuditorBaseline(ctx, repos, project, repo, headSHA, observedAt)
	}
	candidatePRs := make([]int64, 0, len(candidatePRSet))
	for prNumber := range candidatePRSet {
		candidatePRs = append(candidatePRs, prNumber)
	}
	sort.Slice(candidatePRs, func(i, j int) bool { return candidatePRs[i] < candidatePRs[j] })
	entityType, entityID := "branch_head", repo+"@"+headSHA
	existing, err := events.ListByEntity(ctx, entityType, entityID)
	if err != nil {
		return err
	}
	for _, event := range existing {
		if event.EventType == auditor.ObservedFailureEventType {
			return nil
		}
	}
	failingPaths, failureSignatures, pathEvidenceComplete := failedAuditorCheckEvidenceWithPaths(ctx, gateway, repo, project.RepoPath, checks)
	baselineKnown, baseline := auditorCleanBaseline(mergeEvents, project.ID, repo, since)
	projectID := project.ID
	return eventlog.Append(ctx, repos, eventlog.AppendInput{
		EventType: auditor.ObservedFailureEventType, ProjectID: &projectID, EntityType: &entityType, EntityID: &entityID,
		Payload: auditor.FailureObservation{Version: 5, ProjectID: project.ID, Repo: repo, HeadSHA: headSHA, FailedChecks: failureEvidence.Names, FailingPaths: failingPaths, FailureSignatures: failureSignatures, FailingPathEvidenceComplete: pathEvidenceComplete, CheckSuiteIDs: failureEvidence.SuiteIDs, CandidatePRs: candidatePRs, BaselineKnown: baselineKnown, BaselineHeadSHA: baseline.HeadSHA, BaselineObservedAt: baseline.ObservedAt, ObservedAt: eventlog.FormatJavaScriptISOString(observedAt)}, CreatedAt: observedAt,
	})
}

func failedAuditorCheckEvidenceWithPaths(ctx context.Context, gateway auditorGateway, repo, cwd string, checks githubinfra.PullRequestCheckRuns) ([]string, []auditor.FailurePathSignature, bool) {
	paths := make(map[string]struct{})
	byCheck := make(map[string]map[string]struct{})
	complete := true
	for _, check := range checks.CheckRuns {
		if check.ID <= 0 || !isFailedCheckState(check.Status, check.Conclusion) {
			continue
		}
		annotations, err := gateway.ListCheckRunAnnotations(ctx, githubinfra.CheckRunAnnotationsInput{Repo: repo, CheckRunID: check.ID, CWD: cwd})
		if err != nil {
			complete = false
			continue
		}
		for _, annotation := range annotations {
			if !strings.EqualFold(strings.TrimSpace(annotation.Level), "failure") {
				continue
			}
			if path := strings.TrimSpace(annotation.Path); path != "" {
				paths[path] = struct{}{}
				checkName := strings.TrimSpace(check.Name)
				if checkName != "" {
					if byCheck[checkName] == nil {
						byCheck[checkName] = make(map[string]struct{})
					}
					byCheck[checkName][path] = struct{}{}
				}
			}
		}
	}
	result := make([]string, 0, len(paths))
	for path := range paths {
		result = append(result, path)
	}
	sort.Strings(result)
	signatures := make([]auditor.FailurePathSignature, 0, len(byCheck))
	for checkName, checkPaths := range byCheck {
		values := make([]string, 0, len(checkPaths))
		for path := range checkPaths {
			values = append(values, path)
		}
		sort.Strings(values)
		signatures = append(signatures, auditor.FailurePathSignature{Check: checkName, Paths: values})
	}
	sort.Slice(signatures, func(i, j int) bool {
		return strings.ToLower(signatures[i].Check) < strings.ToLower(signatures[j].Check)
	})
	return result, signatures, complete
}

func recordAuditorBaseline(ctx context.Context, repos *storage.Repositories, project storage.ProjectRecord, repo, headSHA string, observedAt time.Time) error {
	entityType, entityID := "branch_head", repo+"@"+headSHA
	existing, err := repos.Events.ListByEntity(ctx, entityType, entityID)
	if err != nil {
		return err
	}
	for _, event := range existing {
		if event.EventType != auditor.BaselineEventType || event.ProjectID == nil || *event.ProjectID != project.ID {
			continue
		}
		var baseline auditor.BaselineObservation
		if json.Unmarshal([]byte(event.PayloadJSON), &baseline) != nil || !strings.EqualFold(baseline.Repo, repo) {
			continue
		}
		if baseline.ProjectID == project.ID {
			return nil
		}
	}
	projectID := project.ID
	return eventlog.Append(ctx, repos, eventlog.AppendInput{
		EventType: auditor.BaselineEventType, ProjectID: &projectID, EntityType: &entityType, EntityID: &entityID,
		Payload: auditor.BaselineObservation{Version: 1, ProjectID: project.ID, Repo: repo, HeadSHA: headSHA, ObservedAt: eventlog.FormatJavaScriptISOString(observedAt)}, CreatedAt: observedAt,
	})
}

func auditorCleanBaseline(events []storage.EventLogRecord, projectID, repo, since string) (bool, auditor.BaselineObservation) {
	sinceTime, err := time.Parse(time.RFC3339Nano, since)
	if err != nil {
		return false, auditor.BaselineObservation{}
	}
	var latest auditor.BaselineObservation
	var latestAt time.Time
	for _, event := range events {
		if event.EventType != auditor.BaselineEventType || event.ProjectID == nil || *event.ProjectID != projectID {
			continue
		}
		var baseline auditor.BaselineObservation
		if json.Unmarshal([]byte(event.PayloadJSON), &baseline) != nil || !strings.EqualFold(baseline.Repo, repo) {
			continue
		}
		observedAt, parseErr := time.Parse(time.RFC3339Nano, baseline.ObservedAt)
		if parseErr != nil {
			observedAt, parseErr = time.Parse(time.RFC3339Nano, event.CreatedAt)
		}
		if parseErr == nil && !observedAt.Before(sinceTime) && (latestAt.IsZero() || observedAt.After(latestAt)) {
			latestAt = observedAt
			latest = baseline
		}
	}
	return !latestAt.IsZero(), latest
}

func auditorRoleForProject(cfg config.Config, projectID string) config.AuditorRoleConfig {
	return config.ProjectRoleConfigs(cfg, projectID).Auditor
}

//nolint:unused
var _ = failedAuditorChecks

func failedAuditorChecks(checks githubinfra.PullRequestCheckRuns) []string {
	return failedAuditorCheckEvidence(checks).Names
}

type auditorCheckFailureEvidence struct {
	Names    []string
	SuiteIDs []int64
}

func failedAuditorCheckEvidence(checks githubinfra.PullRequestCheckRuns) auditorCheckFailureEvidence {
	failed := make(map[string]struct{})
	suites := make(map[int64]struct{})
	for _, check := range checks.CheckRuns {
		if isFailedCheckState(check.Status, check.Conclusion) {
			failed[strings.TrimSpace(check.Name)] = struct{}{}
			if check.CheckSuiteID > 0 {
				suites[check.CheckSuiteID] = struct{}{}
			}
		}
	}
	for _, status := range checks.Statuses {
		if strings.EqualFold(strings.TrimSpace(status.State), "failure") || strings.EqualFold(strings.TrimSpace(status.State), "error") {
			failed[strings.TrimSpace(status.Context)] = struct{}{}
		}
	}
	result := make([]string, 0, len(failed))
	for name := range failed {
		if name != "" {
			result = append(result, name)
		}
	}
	sort.Strings(result)
	suiteIDs := make([]int64, 0, len(suites))
	for suiteID := range suites {
		suiteIDs = append(suiteIDs, suiteID)
	}
	sort.Slice(suiteIDs, func(i, j int) bool { return suiteIDs[i] < suiteIDs[j] })
	return auditorCheckFailureEvidence{Names: result, SuiteIDs: suiteIDs}
}

func isFailedCheckState(status, conclusion string) bool {
	if !strings.EqualFold(strings.TrimSpace(status), "completed") {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(conclusion)) {
	case "failure", "cancelled", "timed_out", "action_required", "startup_failure":
		return true
	default:
		return false
	}
}

func cleanAuditorCheckEvidence(checks githubinfra.PullRequestCheckRuns) bool {
	if checks.TotalCount > len(checks.CheckRuns) || checks.StatusesTotalCount > len(checks.Statuses) {
		return false
	}
	observed := 0
	for _, check := range checks.CheckRuns {
		observed++
		if !strings.EqualFold(strings.TrimSpace(check.Status), "completed") || !strings.EqualFold(strings.TrimSpace(check.Conclusion), "success") {
			return false
		}
	}
	for _, status := range checks.Statuses {
		observed++
		if !strings.EqualFold(strings.TrimSpace(status.State), "success") {
			return false
		}
	}
	return observed > 0
}
