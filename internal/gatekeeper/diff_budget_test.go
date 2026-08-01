package gatekeeper

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/config"
	githubinfra "github.com/MumuTW/looper/internal/infra/github"
	"github.com/MumuTW/looper/internal/storage"
)

func diffBudgetRunner(t *testing.T, fixture *gatekeeperFixture, budget config.GatekeeperDiffBudget, trust config.GatekeeperTrustLevel) *Runner {
	t.Helper()
	return New(Options{
		Repos:  fixture.repos,
		GitHub: fixture.github,
		Now:    func() time.Time { return fixture.now },
		PolicyPermitsTarget: func(string, string, string) bool {
			return fixture.policyPermits
		},
		TrustForProject: func(string) config.GatekeeperTrustLevel { return trust },
		DiffBudgetForProject: func(string) config.GatekeeperDiffBudget {
			return budget
		},
	})
}

func TestDiffBudgetAtLimitPassesAndRecordsEvidence(t *testing.T) {
	t.Parallel()
	fixture := newGatekeeperFixture(t)
	fixture.github.detail.DiffStats = &githubinfra.PullRequestDiffStats{ChangedFiles: 20, Deletions: 500}
	runner := diffBudgetRunner(t, fixture, config.GatekeeperDiffBudget{MaxChangedFiles: 20, MaxDeletions: 500}, config.GatekeeperTrustObserve)

	report, err := runner.EvaluatePullRequest(context.Background(), EvaluationInput{ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1"})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	if !report.Eligible || len(report.Reasons) != 0 {
		t.Fatalf("report = %#v, want eligible at both limits", report)
	}
	if report.Evidence.DiffBudget == nil || report.Evidence.DiffBudget.ChangedFiles != 20 || report.Evidence.DiffBudget.Deletions != 500 {
		t.Fatalf("diff budget evidence = %#v, want observed counts", report.Evidence.DiffBudget)
	}
}

func TestDiffBudgetExceededBlocksAndExplainsObservedValues(t *testing.T) {
	t.Parallel()
	fixture := newGatekeeperFixture(t)
	fixture.github.detail.DiffStats = &githubinfra.PullRequestDiffStats{ChangedFiles: 21, Deletions: 501}
	runner := diffBudgetRunner(t, fixture, config.GatekeeperDiffBudget{MaxChangedFiles: 20, MaxDeletions: 500}, config.GatekeeperTrustAdvise)

	report, err := runner.EvaluatePullRequest(context.Background(), EvaluationInput{ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1"})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	if report.Eligible || len(report.Reasons) != 1 || report.Reasons[0].Code != ReasonDiffBudgetExceeded {
		t.Fatalf("report = %#v, want one diff-budget reason", report)
	}
	if !strings.Contains(report.Reasons[0].Subject, "changed files 21 > max 20") || !strings.Contains(report.Reasons[0].Subject, "deletions 501 > max 500") {
		t.Fatalf("reason subject = %q, want observed values and bounds", report.Reasons[0].Subject)
	}
	if body := BuildVerdictComment(report); !strings.Contains(body, "exceeds the configured diff budget") || !strings.Contains(body, "changed files 21 > max 20") {
		t.Fatalf("verdict body = %q, want diff-budget explanation", body)
	}
}

func TestDiffBudgetZeroBoundsDisableTheGate(t *testing.T) {
	t.Parallel()
	fixture := newGatekeeperFixture(t)
	runner := diffBudgetRunner(t, fixture, config.GatekeeperDiffBudget{}, config.GatekeeperTrustObserve)

	report, err := runner.EvaluatePullRequest(context.Background(), EvaluationInput{ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1"})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	if !report.Eligible || report.Evidence.DiffBudget != nil {
		t.Fatalf("report = %#v, want disabled budget with no evidence", report)
	}
}

func TestDiffBudgetRequiresProviderStatsWhenEnabled(t *testing.T) {
	t.Parallel()
	fixture := newGatekeeperFixture(t)
	runner := diffBudgetRunner(t, fixture, config.GatekeeperDiffBudget{MaxChangedFiles: 1}, config.GatekeeperTrustObserve)

	report, err := runner.EvaluatePullRequest(context.Background(), EvaluationInput{ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1"})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	if report.Eligible || len(report.Reasons) != 1 || report.Reasons[0].Code != ReasonProviderStateUnavailable || report.Reasons[0].Subject != "diff_stats" {
		t.Fatalf("report = %#v, want unavailable diff stats block", report)
	}
}

func TestDiffBudgetIsRecheckedBeforeAutoMerge(t *testing.T) {
	t.Parallel()
	fixture := newGatekeeperFixture(t)
	fixture.github.detail.DiffStats = &githubinfra.PullRequestDiffStats{ChangedFiles: 20}
	views := 0
	fixture.github.beforeView = func(github *fakeGatekeeperGitHub) {
		views++
		if views > 1 {
			github.detail.DiffStats = &githubinfra.PullRequestDiffStats{ChangedFiles: 21}
		}
	}
	runner := diffBudgetRunner(t, fixture, config.GatekeeperDiffBudget{MaxChangedFiles: 20}, config.GatekeeperTrustAuto)

	if _, err := runner.EvaluatePullRequest(context.Background(), EvaluationInput{ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1"}); err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	if len(fixture.github.merges) != 0 {
		t.Fatalf("merges = %#v, want confirming diff-budget block", fixture.github.merges)
	}
	outcomes := mergeOutcomes(t, fixture.repos)
	if len(outcomes) != 1 || outcomes[0].Reason != refusalNoLongerClean || len(outcomes[0].ConfirmingReasons) != 1 || outcomes[0].ConfirmingReasons[0].Code != ReasonDiffBudgetExceeded {
		t.Fatalf("outcomes = %#v, want confirming diff-budget refusal", outcomes)
	}
}

// Configured project IDs are matched by exact equality and validation accepts
// surrounding whitespace as a distinct ID, so evaluation must preserve the
// caller's original project ID through every lookup. Trimming it would diverge
// from the discovery fingerprint (which uses the original key) and fall back to
// the global budget and default trust for a whitespace-padded configured ID.
func TestDiffBudgetPreservesExactProjectIDThroughEvaluation(t *testing.T) {
	t.Parallel()
	fixture := newGatekeeperFixture(t)
	fixture.github.detail.DiffStats = &githubinfra.PullRequestDiffStats{ChangedFiles: 5}
	nowISO := fixture.now.Format(time.RFC3339Nano)
	if err := fixture.repos.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: " padded ", Name: "Padded", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	runner := New(Options{
		Repos:  fixture.repos,
		GitHub: fixture.github,
		Now:    func() time.Time { return fixture.now },
		PolicyPermitsTarget: func(string, string, string) bool {
			return fixture.policyPermits
		},
		DiffBudgetForProject: func(projectID string) config.GatekeeperDiffBudget {
			if projectID == " padded " {
				return config.GatekeeperDiffBudget{MaxChangedFiles: 10}
			}
			return config.GatekeeperDiffBudget{}
		},
		TrustForProject: func(projectID string) config.GatekeeperTrustLevel {
			if projectID == " padded " {
				return config.GatekeeperTrustAdvise
			}
			return config.GatekeeperTrustObserve
		},
	})

	report, err := runner.EvaluatePullRequest(context.Background(), EvaluationInput{
		ProjectID: " padded ", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1",
	})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	if report.Evidence.DiffBudget == nil || report.Evidence.DiffBudget.MaxChangedFiles != 10 {
		t.Fatalf("diff budget evidence = %#v, want the project override MaxChangedFiles=10 applied for the exact padded ID", report.Evidence.DiffBudget)
	}
	if report.Mode != string(config.GatekeeperTrustAdvise) {
		t.Fatalf("report.Mode = %q, want advise (project trust override preserved for the exact padded ID)", report.Mode)
	}
}

// The diff statistics are observed against the detail read's base SHA. When the
// base branch advances before the merge-watch read, GitHub recomputes the change
// size against a new merge base without moving the head, so the budget verdict
// must revalidate against the merge-watch's current base rather than the stale
// detail counts.
func TestDiffBudgetRevalidatesAgainstObservedBaseSHA(t *testing.T) {
	t.Parallel()
	fixture := newGatekeeperFixture(t)
	fixture.github.detail.DiffStats = &githubinfra.PullRequestDiffStats{ChangedFiles: 5}
	fixture.github.detail.BaseSHA = "base-1"
	fixture.github.mergeable.BaseSHA = "base-2"
	fixture.github.mergeable.DiffStats = &githubinfra.PullRequestDiffStats{ChangedFiles: 21}
	runner := diffBudgetRunner(t, fixture, config.GatekeeperDiffBudget{MaxChangedFiles: 20}, config.GatekeeperTrustAdvise)

	report, err := runner.EvaluatePullRequest(context.Background(), EvaluationInput{ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1"})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	if report.Eligible || len(report.Reasons) != 1 || report.Reasons[0].Code != ReasonDiffBudgetExceeded {
		t.Fatalf("report = %#v, want diff-budget exceeded against the advanced base", report)
	}
	if report.Evidence.DiffBudget == nil || report.Evidence.DiffBudget.ChangedFiles != 21 {
		t.Fatalf("diff budget evidence = %#v, want changedFiles=21 from the merge-watch read (current base)", report.Evidence.DiffBudget)
	}
}

// When the base is unchanged between the detail and merge-watch reads, the
// detail's diff statistics remain authoritative and are not discarded.
func TestDiffBudgetKeepsDetailStatsWhenBaseUnchanged(t *testing.T) {
	t.Parallel()
	fixture := newGatekeeperFixture(t)
	fixture.github.detail.DiffStats = &githubinfra.PullRequestDiffStats{ChangedFiles: 5}
	fixture.github.detail.BaseSHA = "base-1"
	fixture.github.mergeable.BaseSHA = "base-1"
	fixture.github.mergeable.DiffStats = &githubinfra.PullRequestDiffStats{ChangedFiles: 21}
	runner := diffBudgetRunner(t, fixture, config.GatekeeperDiffBudget{MaxChangedFiles: 20}, config.GatekeeperTrustAdvise)

	report, err := runner.EvaluatePullRequest(context.Background(), EvaluationInput{ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1"})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	if !report.Eligible {
		t.Fatalf("report = %#v, want eligible (detail stats 5 <= 20 are authoritative when the base is unchanged)", report)
	}
	if report.Evidence.DiffBudget == nil || report.Evidence.DiffBudget.ChangedFiles != 5 {
		t.Fatalf("diff budget evidence = %#v, want changedFiles=5 from the detail read (base unchanged)", report.Evidence.DiffBudget)
	}
}
