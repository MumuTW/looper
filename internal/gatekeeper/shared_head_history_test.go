package gatekeeper

import (
	"context"
	"fmt"
	"testing"

	githubinfra "github.com/MumuTW/looper/internal/infra/github"
	"github.com/MumuTW/looper/internal/labels"
)

func TestTargetedEvaluationAggregatesAllReportsForSharedHead(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	sharedSHA := "shared-head"
	fixture.github.detail.HeadSHA = sharedSHA
	fixture.github.mergeable.HeadSHA = sharedSHA
	fixture.github.finalHeadSHA = sharedSHA
	for index := 0; index < 101; index++ {
		report := Report{
			Version: reportVersion, Status: StatusEligible, Eligible: true,
			ProjectID: "project_1", Repo: "acme/looper", PRNumber: int64(1000 + index),
			ObservedHeadSHA: sharedSHA, ExpectedHeadSHA: sharedSHA,
			Evidence: Evidence{PullRequestState: "OPEN"}, EvaluatedAt: fixture.now.Format("2006-01-02T15:04:05.999999999Z07:00"),
		}
		if index == 0 {
			report.Eligible = false
			report.Status = StatusBlocked
			report.Reasons = []Reason{{Code: ReasonHold, Subject: labels.HoldGlobal}}
		}
		seedGateReport(t, fixture, report)
	}

	report, err := fixture.autoRunner().EvaluatePullRequest(context.Background(), EvaluationInput{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: sharedSHA,
	})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	// The routing-label design evaluates each pull request independently: a
	// blocked report on another PR with the same head no longer publishes a
	// shared commit status that would block this PR. PR 42 passes all gates
	// and is eligible.
	if !report.Eligible || report.Status != StatusEligible {
		t.Fatalf("report = %+v, want eligible (routing-label design evaluates PRs independently)", report)
	}
}

func TestDiscoveryLoadsAllPriorReportsForDepartedPullRequests(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.github.openPullRequests = nil
	fixture.github.detail = githubinfra.PullRequestDetail{Number: 42, State: "MERGED", HeadSHA: "merged-head", BaseRefName: "main"}
	fixture.github.mergeable = githubinfra.PullRequestDetail{Number: 42, HeadSHA: "merged-head"}
	seedGateReport(t, fixture, Report{
		Version: reportVersion, Status: StatusEligible, Eligible: true,
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42,
		ObservedHeadSHA: "merged-head", Evidence: Evidence{PullRequestState: "OPEN"}, EvaluatedAt: fixture.now.Format("2006-01-02T15:04:05.999999999Z07:00"),
	})
	for index := 0; index < 100; index++ {
		seedGateReport(t, fixture, Report{
			Version: reportVersion, Status: StatusEligible, Eligible: true,
			ProjectID: "project_1", Repo: "acme/looper", PRNumber: int64(1000 + index),
			ObservedHeadSHA: fmt.Sprintf("old-head-%d", index), Evidence: Evidence{PullRequestState: "OPEN"}, EvaluatedAt: fixture.now.Format("2006-01-02T15:04:05.999999999Z07:00"),
		})
	}

	result, err := fixture.runner().DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	foundMerged := false
	for _, report := range result.Reports {
		if report.PRNumber == 42 && report.Evidence.PullRequestState == "MERGED" {
			foundMerged = true
			break
		}
	}
	if !foundMerged {
		t.Fatalf("result = %#v, want departed PR 42 reconciled despite 101 prior reports", result)
	}
}
