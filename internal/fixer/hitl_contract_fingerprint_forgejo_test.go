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

// TestHITLContract_NewForgejoSummaryItemDuringParkChangesFP covers add-item-
// during-park for forgejo-reviewer-summary: a newly opened summary item while
// parked must change the live fingerprint.
func TestHITLContract_NewForgejoSummaryItemDuringParkChangesFP(t *testing.T) {
	t.Parallel()
	askSummary := forge.NewReviewerSummary(1, []forge.ReviewItem{
		{ReviewItemID: "sum-1", Status: forge.ReviewItemStatusOpen, Title: "open finding", Body: "detail text", LastSeenRoundID: 1},
	})
	items := forgejoReviewerSummaryFixItems(askSummary)
	askFP := computeReviewContentFingerprint(items)

	liveSummary := forge.NewReviewerSummary(1, []forge.ReviewItem{
		{ReviewItemID: "sum-1", Status: forge.ReviewItemStatusOpen, Title: "open finding", Body: "detail text", LastSeenRoundID: 1},
		{ReviewItemID: "sum-2", Status: forge.ReviewItemStatusOpen, Title: "new finding", Body: "appeared while parked", LastSeenRoundID: 1},
	})
	marker, err := forge.RenderReviewerSummary(liveSummary)
	if err != nil {
		t.Fatalf("RenderReviewerSummary: %v", err)
	}
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
	if liveFP == askFP {
		t.Fatal("new forgejo-reviewer-summary item during park must change review content fingerprint")
	}
}

// TestHITLContract_NewNativeCommentDuringParkChangesFP covers add-item-during-
// park for Forgejo native review comments.
func TestHITLContract_NewNativeCommentDuringParkChangesFP(t *testing.T) {
	t.Parallel()
	const updated = "2026-07-28T00:00:00Z"
	items := []FixItem{{
		Type: "comment", Source: NativeReviewCommentSource,
		ID: NativeReviewCommentFixItemID(11), ThreadID: NativeReviewCommentThreadID(11),
		ProviderCommentID: 11,
		Summary:           "native body", Body: "native body",
		ThreadFingerprint:   NativeReviewCommentFingerprint(11, updated),
		ObservedFingerprint: NativeReviewCommentFingerprint(11, updated),
	}}
	askFP := computeReviewContentFingerprint(items)

	github := &fakeGitHubGateway{
		nativeComments: []NativeReviewComment{
			{
				ProviderCommentID: 11, Body: "native body", UpdatedAt: updated,
				ObservedFingerprint: NativeReviewCommentFingerprint(11, updated),
				ResolverPresent:     true,
			},
			// New native comment while parked.
			{
				ProviderCommentID: 22, Body: "brand new native finding", UpdatedAt: "2026-07-28T03:00:00Z",
				ObservedFingerprint: NativeReviewCommentFingerprint(22, "2026-07-28T03:00:00Z"),
				ResolverPresent:     true,
			},
		},
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
		t.Fatal("new Forgejo native comment during park must change review content fingerprint")
	}
}
