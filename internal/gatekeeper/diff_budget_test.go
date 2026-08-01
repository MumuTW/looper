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
	fixture.github.detail.BaseSHA = "base-1"
	fixture.github.mergeable.BaseSHA = "base-1"
	fixture.github.finalBaseSHA = "base-1"
	runner := diffBudgetRunner(t, fixture, config.GatekeeperDiffBudget{MaxChangedFiles: 20, MaxDeletions: 500}, config.GatekeeperTrustObserve)

	report, err := runner.EvaluatePullRequest(context.Background(), EvaluationInput{ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1"})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	if !report.Eligible || len(report.Reasons) != 0 {
		t.Fatalf("report = %#v, want eligible at both limits", report)
	}
	if report.Evidence.DiffBudget == nil || report.Evidence.DiffBudget.ChangedFiles != 20 || report.Evidence.DiffBudget.Deletions != 500 || report.Evidence.DiffBudget.BaseSHA != "base-1" {
		t.Fatalf("diff budget evidence = %#v, want observed counts anchored to base-1", report.Evidence.DiffBudget)
	}
}

func TestDiffBudgetExceededBlocksAndExplainsObservedValues(t *testing.T) {
	t.Parallel()
	fixture := newGatekeeperFixture(t)
	fixture.github.detail.DiffStats = &githubinfra.PullRequestDiffStats{ChangedFiles: 21, Deletions: 501}
	fixture.github.detail.BaseSHA = "base-1"
	fixture.github.mergeable.BaseSHA = "base-1"
	fixture.github.finalBaseSHA = "base-1"
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

// A provider that returns diff statistics but omits both base SHAs leaves no way
// to establish which merge base produced the counts. The gate must fail closed
// rather than record an eligible verdict it cannot anchor, because the final
// revalidation would otherwise have no base to compare and would bypass the
// stale-base check entirely.
func TestDiffBudgetFailsClosedWhenBaseSHAMissing(t *testing.T) {
	t.Parallel()
	fixture := newGatekeeperFixture(t)
	fixture.github.detail.DiffStats = &githubinfra.PullRequestDiffStats{ChangedFiles: 5}
	// Clear both observed base SHAs so the provider returns stats without a base
	// to anchor them; the fixture normally supplies a realistic base anchor.
	fixture.github.detail.BaseSHA = ""
	fixture.github.mergeable.BaseSHA = ""
	runner := diffBudgetRunner(t, fixture, config.GatekeeperDiffBudget{MaxChangedFiles: 20}, config.GatekeeperTrustAuto)

	report, err := runner.EvaluatePullRequest(context.Background(), EvaluationInput{ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1"})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	if report.Eligible || len(report.Reasons) != 1 || report.Reasons[0].Code != ReasonProviderStateAmbiguous || report.Reasons[0].Subject != "diff_budget_base" {
		t.Fatalf("report = %#v, want a single diff_budget_base ambiguous block (base SHA missing while stats present)", report)
	}
	if report.Evidence.DiffBudget != nil {
		t.Fatalf("diff budget evidence = %#v, want nil (no verdict recorded when the base cannot be established)", report.Evidence.DiffBudget)
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
	fixture.github.detail.BaseSHA = "base-1"
	fixture.github.mergeable.BaseSHA = "base-1"
	fixture.github.finalBaseSHA = "base-1"
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

// projectCWD resolves a configured project ID to its repo path with an exact
// SQLite lookup. A whitespace-padded configured ID is a distinct, valid ID, so
// trimming it before the lookup would return no path. On GitHub Enterprise that
// empty CWD strands ListReviewThreads on the wrong provider hostname, so the
// exact (padded) key must be preserved through the lookup.
func TestProjectCDWPreservesPaddedProjectID(t *testing.T) {
	t.Parallel()
	fixture := newGatekeeperFixture(t)
	nowISO := fixture.now.Format(time.RFC3339Nano)
	repoPath := t.TempDir()
	if err := fixture.repos.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: " padded ", Name: "Padded", RepoPath: repoPath, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	runner := New(Options{Repos: fixture.repos, GitHub: fixture.github, Now: func() time.Time { return fixture.now }})

	if got := runner.projectCWD(context.Background(), " padded "); got != repoPath {
		t.Fatalf("projectCWD(\" padded \") = %q, want %q (exact padded ID must resolve)", got, repoPath)
	}
	if got := runner.projectCWD(context.Background(), "padded"); got != "" {
		t.Fatalf("projectCWD(\"padded\") = %q, want empty (trimmed key is a different ID with no project)", got)
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
	fixture.github.finalBaseSHA = "base-2"
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
	fixture.github.finalBaseSHA = "base-1"
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

// The diff-budget verdict is captured at the merge-watch read, but several
// provider queries (branch protection, checks, review threads) and the final
// head revalidation run after it. When the base branch advances in that window
// without moving the head, the head revalidation alone cannot detect it: the
// recorded counts still describe the previous merge base while GitHub would
// merge against the new one. The final revalidation must revalidate the base
// too and fail closed, so an eligible verdict is not persisted on stale counts.
func TestDiffBudgetFailsClosedWhenBaseAdvancesBeforeFinalRevalidation(t *testing.T) {
	t.Parallel()
	fixture := newGatekeeperFixture(t)
	fixture.github.detail.DiffStats = &githubinfra.PullRequestDiffStats{ChangedFiles: 5}
	fixture.github.detail.BaseSHA = "base-1"
	fixture.github.mergeable.BaseSHA = "base-1"
	fixture.github.finalBaseSHA = "base-2"
	runner := diffBudgetRunner(t, fixture, config.GatekeeperDiffBudget{MaxChangedFiles: 20}, config.GatekeeperTrustAuto)

	report, err := runner.EvaluatePullRequest(context.Background(), EvaluationInput{ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1"})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	if report.Eligible || len(report.Reasons) != 1 || report.Reasons[0].Code != ReasonProviderStateAmbiguous || report.Reasons[0].Subject != "diff_budget_base" {
		t.Fatalf("report = %#v, want a single diff_budget_base ambiguous block (base advanced after merge-watch)", report)
	}
}

// When the budget is disabled, a base advance between the merge-watch read and
// the final revalidation is not a gate input, so the final revalidation must not
// fail closed on it.
func TestDiffBudgetBaseRevalidationSkippedWhenBudgetDisabled(t *testing.T) {
	t.Parallel()
	fixture := newGatekeeperFixture(t)
	fixture.github.detail.BaseSHA = "base-1"
	fixture.github.mergeable.BaseSHA = "base-1"
	fixture.github.finalBaseSHA = "base-2"
	runner := diffBudgetRunner(t, fixture, config.GatekeeperDiffBudget{}, config.GatekeeperTrustObserve)

	report, err := runner.EvaluatePullRequest(context.Background(), EvaluationInput{ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1"})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	if !report.Eligible {
		t.Fatalf("report = %#v, want eligible (base advance is not a gate input when the budget is disabled)", report)
	}
}
