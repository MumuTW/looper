package runtime

import (
	"context"
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
	failureEvidence := failedAuditorCheckEvidence(checks)
	if len(failureEvidence.Names) == 0 {
		return nil
	}
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
		return nil
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
	projectID := project.ID
	return eventlog.Append(ctx, repos, eventlog.AppendInput{
		EventType: auditor.ObservedFailureEventType, ProjectID: &projectID, EntityType: &entityType, EntityID: &entityID,
		Payload: auditor.FailureObservation{Version: 2, ProjectID: project.ID, Repo: repo, HeadSHA: headSHA, FailedChecks: failureEvidence.Names, CheckSuiteIDs: failureEvidence.SuiteIDs, CandidatePRs: candidatePRs, ObservedAt: eventlog.FormatJavaScriptISOString(observedAt)}, CreatedAt: observedAt,
	})
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
