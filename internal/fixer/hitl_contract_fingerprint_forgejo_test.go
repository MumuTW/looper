package fixer

import (
	"context"
	"testing"

	"github.com/nexu-io/looper/internal/forge"
	"github.com/nexu-io/looper/internal/storage"
)

func TestHITLContract_ForgejoSummaryLiveRefreshMatches(t *testing.T) {
	t.Parallel()
	summary := forge.NewReviewerSummary(1, []forge.ReviewItem{
		{ReviewItemID: "sum-1", Status: forge.ReviewItemStatusOpen, Title: "open finding", Body: "detail text", LastSeenRoundID: 1},
	})
	marker, err := forge.RenderReviewerSummary(summary)
	if err != nil {
		t.Fatalf("RenderReviewerSummary: %v", err)
	}
	items := forgejoReviewerSummaryFixItems(summary)
	if len(items) != 1 {
		t.Fatalf("items = %#v", items)
	}
	askFP := computeReviewContentFingerprint(items)

	github := &fakeGitHubGateway{
		viewResponses: []PullRequestDetail{{
			Number: 1, State: "OPEN", HeadSHA: "h1", BaseRefName: "main",
			IssueComments: []map[string]any{{"id": int64(101), "body": marker}},
		}},
	}
	runner := New(Options{GitHub: github, HITLEnabled: true})
	liveFP, err := runner.liveReviewContentFingerprint(context.Background(), stepInput{
		Project: storage.ProjectRecord{RepoPath: "/tmp"},
		Repo:    "acme/looper", PRNumber: 1,
	}, items)
	if err != nil {
		t.Fatalf("liveReviewContentFingerprint error = %v", err)
	}
	if liveFP != askFP {
		t.Fatalf("unchanged forgejo summary FP mismatch:\n ask=%s\nlive=%s", askFP, liveFP)
	}
}

func TestHITLContract_ForgejoSummaryMissingItemChangesFP(t *testing.T) {
	t.Parallel()
	summary := forge.NewReviewerSummary(1, []forge.ReviewItem{
		{ReviewItemID: "sum-1", Status: forge.ReviewItemStatusOpen, Title: "open finding", Body: "detail text", LastSeenRoundID: 1},
	})
	items := forgejoReviewerSummaryFixItems(summary)
	askFP := computeReviewContentFingerprint(items)

	// Live PR has no reviewer-summary comment → item marked missing.
	github := &fakeGitHubGateway{
		viewResponses: []PullRequestDetail{{
			Number: 1, State: "OPEN", HeadSHA: "h1", BaseRefName: "main",
		}},
	}
	runner := New(Options{GitHub: github, HITLEnabled: true})
	liveFP, err := runner.liveReviewContentFingerprint(context.Background(), stepInput{
		Project: storage.ProjectRecord{RepoPath: "/tmp"},
		Repo:    "acme/looper", PRNumber: 1,
	}, items)
	if err != nil {
		t.Fatalf("liveReviewContentFingerprint error = %v", err)
	}
	if liveFP == askFP {
		t.Fatal("missing forgejo summary item must change review content fingerprint")
	}
}
