package gatekeeper

import (
	"context"
	"testing"

	githubinfra "github.com/MumuTW/looper/internal/infra/github"
)

func TestDiscoverPullRequestsEvaluatesOpenPullRequestsWithoutLabelGate(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.github.openPullRequests = []githubinfra.PullRequestSummary{{
		Number: 42, State: "OPEN", IsDraft: true, HeadSHA: "head-1", Labels: nil,
	}}

	result, err := fixture.runner().DiscoverPullRequests(context.Background(), DiscoveryInput{
		ProjectID: "project_1",
		Repo:      "acme/looper",
	})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	if result.Evaluated != 1 || len(result.Reports) != 1 {
		t.Fatalf("result = %#v, want one evaluated pull request", result)
	}
	if result.Reports[0].ExpectedHeadSHA != "head-1" {
		t.Fatalf("report expected head = %q, want head-1 from discovery source", result.Reports[0].ExpectedHeadSHA)
	}
}
