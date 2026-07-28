package fixer

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/hitl"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/storage"
)

func TestHITLContract_MissingThreadChangesReviewFP(t *testing.T) {
	t.Parallel()
	items := []FixItem{{
		Type: "comment", ID: "c1", ThreadID: "t-gone",
		Summary: "Please restore configurable strategy", Body: "",
		ThreadFingerprint: "c1@2026-07-28T00:00:00Z",
	}}
	askFP := computeReviewContentFingerprint(items)

	// No matching thread → missing marker, FP must diverge.
	github := &fakeGitHubGateway{threads: []ReviewThread{}}
	runner := New(Options{GitHub: github, HITLEnabled: true})
	liveFP, err := runner.liveReviewContentFingerprint(context.Background(), stepInput{
		Project: storage.ProjectRecord{RepoPath: "/tmp"},
		Repo:    "acme/looper", PRNumber: 1,
	}, items)
	if err != nil {
		t.Fatalf("liveReviewContentFingerprint error = %v", err)
	}
	if liveFP == askFP {
		t.Fatal("missing/deleted thread must change review content fingerprint")
	}
	if hitl.MaterialFingerprintsMatch("h", "h", askFP, liveFP, "i", "i") {
		t.Fatal("missing thread FP must not MaterialFingerprintsMatch ask-time FP")
	}
}

func TestHITLContract_ViewReviewThreadProviderErrorFailsClosed(t *testing.T) {
	t.Parallel()
	items := []FixItem{{
		Type: "comment", ID: "c1", ThreadID: "t1",
		Summary: "Please restore configurable strategy", Body: "",
		ThreadFingerprint: "c1@2026-07-28T00:00:00Z",
	}}
	github := &fakeGitHubGateway{viewThreadErr: fmt.Errorf("API rate limit exceeded")}
	runner := New(Options{GitHub: github, HITLEnabled: true})
	_, err := runner.liveReviewContentFingerprint(context.Background(), stepInput{
		Project: storage.ProjectRecord{RepoPath: "/tmp"},
		Repo:    "acme/looper", PRNumber: 1,
	}, items)
	if err == nil {
		t.Fatal("provider/rate-limit ViewReviewThread error must fail closed, not mark missing")
	}
	if !strings.Contains(err.Error(), "live review thread refresh failed") {
		t.Fatalf("error = %v, want live review thread refresh failed", err)
	}
}

func TestHITLContract_ViewReviewThreadNotFoundMarksMissing(t *testing.T) {
	t.Parallel()
	items := []FixItem{{
		Type: "comment", ID: "c1", ThreadID: "t-gone",
		Summary: "Please restore configurable strategy", Body: "",
		ThreadFingerprint: "c1@2026-07-28T00:00:00Z",
	}}
	askFP := computeReviewContentFingerprint(items)
	github := &fakeGitHubGateway{
		viewThreadErr: &githubinfra.ReviewThreadNotFoundError{ThreadID: "t-gone"},
	}
	runner := New(Options{GitHub: github, HITLEnabled: true})
	liveFP, err := runner.liveReviewContentFingerprint(context.Background(), stepInput{
		Project: storage.ProjectRecord{RepoPath: "/tmp"},
		Repo:    "acme/looper", PRNumber: 1,
	}, items)
	if err != nil {
		t.Fatalf("not-found must mark missing, not return error: %v", err)
	}
	if liveFP == askFP {
		t.Fatal("not-found thread must change review content fingerprint")
	}
}

func TestHITLContract_MissingTargetCommentChangesReviewFP(t *testing.T) {
	t.Parallel()
	items := []FixItem{{
		Type: "comment", ID: "c-target", ThreadID: "t1",
		Summary: "Please restore configurable strategy", Body: "",
		ThreadFingerprint: "c-target@2026-07-28T00:00:00Z",
	}}
	askFP := computeReviewContentFingerprint(items)

	// Thread still exists but the targeted comment id is gone (sibling remains).
	github := &fakeGitHubGateway{
		threads: []ReviewThread{{
			ID: "t1",
			Comments: []ReviewThreadComment{
				{ID: "c-sibling", Body: "Please restore configurable strategy", UpdatedAt: "2026-07-28T00:00:00Z"},
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
	if liveFP == askFP {
		t.Fatal("missing target comment must change review FP even when a sibling remains")
	}
}
