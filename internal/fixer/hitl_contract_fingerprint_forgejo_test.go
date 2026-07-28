package fixer

import (
	"context"
	"strings"
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
		currentUser: "looper",
		viewResponses: []PullRequestDetail{{
			Number: 1, State: "OPEN", HeadSHA: "h1", BaseRefName: "main",
			IssueComments: []map[string]any{{"id": int64(101), "body": marker, "user": map[string]any{"login": "looper"}}},
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
		currentUser: "looper",
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
		currentUser: "looper",
		viewResponses: []PullRequestDetail{{
			Number: 1, State: "OPEN", HeadSHA: "h1", BaseRefName: "main",
			IssueComments: []map[string]any{{"id": int64(101), "body": marker, "user": map[string]any{"login": "looper"}}},
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
		currentUser: "looper",
		nativeComments: []NativeReviewComment{
			{
				ProviderCommentID: 11, Body: "native body", Author: "alice", UpdatedAt: updated,
				ObservedFingerprint: NativeReviewCommentFingerprint(11, updated),
				ResolverPresent:     true,
			},
			// New native comment while parked.
			{
				ProviderCommentID: 22, Body: "brand new native finding", Author: "bob", UpdatedAt: "2026-07-28T03:00:00Z",
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

// TestHITLContract_SelfAuthoredNativeCommentDoesNotChangeLiveFP covers the
// resume-hash thrash path: a Looper-authored native review comment remains on
// the PR while parked. Attach-time collection excludes current-user comments;
// live refresh must apply the same filter so the parked answer still injects.
func TestHITLContract_SelfAuthoredNativeCommentDoesNotChangeLiveFP(t *testing.T) {
	t.Parallel()
	const updated = "2026-07-28T00:00:00Z"
	items := []FixItem{{
		Type: "comment", Source: NativeReviewCommentSource,
		ID: NativeReviewCommentFixItemID(11), ThreadID: NativeReviewCommentThreadID(11),
		ProviderCommentID: 11, Author: "alice",
		Summary: "please fix this", Body: "please fix this",
		ThreadFingerprint:   NativeReviewCommentFingerprint(11, updated),
		ObservedFingerprint: NativeReviewCommentFingerprint(11, updated),
	}}
	askFP := computeReviewContentFingerprint(items)

	github := &fakeGitHubGateway{
		currentUser: "looper",
		nativeComments: []NativeReviewComment{
			{
				ProviderCommentID: 11, Body: "please fix this", Author: "alice", UpdatedAt: updated,
				ObservedFingerprint: NativeReviewCommentFingerprint(11, updated),
				ResolverPresent:     true,
			},
			// Self-authored (Looper) native comment present throughout the park.
			// Must not enter the live fingerprint (excluded at ask-time too).
			{
				ProviderCommentID: 99, Body: "my own note", Author: "looper", UpdatedAt: "2026-07-28T01:00:00Z",
				ObservedFingerprint: NativeReviewCommentFingerprint(99, "2026-07-28T01:00:00Z"),
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
	if liveFP != askFP {
		t.Fatalf("self-authored native comment must not change live FP:\n ask=%s\nlive=%s", askFP, liveFP)
	}
}

// TestHITLContract_UntrustedForgejoSummaryMarkerDoesNotInvalidate covers resume
// after a second summary marker is posted by a non-authority commenter. Live
// refresh must sanitize via sanitizeForgejoSummaryAuthority before re-parsing so
// uniqueness errors do not mark the trusted summary missing.
func TestHITLContract_UntrustedForgejoSummaryMarkerDoesNotInvalidate(t *testing.T) {
	t.Parallel()
	summary := forge.NewReviewerSummary(1, []forge.ReviewItem{
		{ReviewItemID: "sum-1", Status: forge.ReviewItemStatusOpen, Title: "open finding", Body: "detail text", LastSeenRoundID: 1},
	})
	marker, err := forge.RenderReviewerSummary(summary)
	if err != nil {
		t.Fatalf("RenderReviewerSummary: %v", err)
	}
	// A second marker body that would uniqueness-error ParseUniqueReviewerSummaryComment.
	rogue := strings.Replace(marker, "sum-1", "sum-rogue", 1)
	items := forgejoReviewerSummaryFixItems(summary)
	if len(items) != 1 {
		t.Fatalf("items = %#v", items)
	}
	askFP := computeReviewContentFingerprint(items)

	github := &fakeGitHubGateway{
		currentUser: "looper",
		viewResponses: []PullRequestDetail{{
			Number: 1, State: "OPEN", HeadSHA: "h1", BaseRefName: "main",
			IssueComments: []map[string]any{
				{"id": int64(101), "body": marker, "user": map[string]any{"login": "looper"}},
				{"id": int64(202), "body": rogue, "user": map[string]any{"login": "attacker"}},
			},
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
		t.Fatalf("untrusted second summary marker must not invalidate trusted FP:\n ask=%s\nlive=%s", askFP, liveFP)
	}
}
