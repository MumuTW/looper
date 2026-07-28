package fixer

import (
	"context"
	"testing"

	"github.com/nexu-io/looper/internal/hitl"
	"github.com/nexu-io/looper/internal/storage"
)

// A GitHub thread resolved while parked must invalidate the parked answer even
// when body text and comment timestamps are unchanged.
func TestHITLContract_ResolvedGitHubThreadInvalidatesAnswer(t *testing.T) {
	t.Parallel()
	const updated = "2026-07-28T00:00:00Z"
	items := []FixItem{{
		Type: "comment", ID: "c1", ThreadID: "t1",
		Summary: "Please restore configurable strategy", Body: "",
		ThreadFingerprint: "c1@" + updated,
	}}
	askFP := computeReviewContentFingerprint(items)

	github := &fakeGitHubGateway{
		threads: []ReviewThread{{
			ID:         "t1",
			IsResolved: true,
			Comments: []ReviewThreadComment{{
				ID: "c1", Body: "Please restore configurable strategy", UpdatedAt: updated,
			}},
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
		t.Fatal("resolved GitHub thread must change review content fingerprint")
	}
	if hitl.MaterialFingerprintsMatch("h", "h", askFP, liveFP, "i", "i") {
		t.Fatal("parked answer must not inject against a resolved review thread")
	}
}

// Forgejo native comments that become IsResolved while parked must invalidate
// the same way as resolved GitHub threads.
func TestHITLContract_ResolvedNativeCommentInvalidatesAnswer(t *testing.T) {
	t.Parallel()
	const updated = "2026-07-28T00:00:00Z"
	fp := NativeReviewCommentFingerprint(101, updated)
	items := []FixItem{{
		Type: "comment", Source: NativeReviewCommentSource,
		ID: NativeReviewCommentFixItemID(101), ProviderCommentID: 101,
		Summary: "Fix this", Body: "Fix this",
		ThreadFingerprint: fp, ObservedFingerprint: fp,
	}}
	askFP := computeReviewContentFingerprint(items)

	github := &fakeGitHubGateway{
		nativeComments: []NativeReviewComment{{
			ProviderCommentID: 101, Body: "Fix this", Author: "alice",
			ObservedFingerprint: fp, UpdatedAt: updated,
			IsResolved: true, ResolverPresent: true,
		}},
	}
	runner := New(Options{GitHub: github, HITLEnabled: true})
	liveFP, err := runner.liveReviewContentFingerprint(context.Background(), stepInput{
		Project: storage.ProjectRecord{RepoPath: "/tmp", ID: "project_1"},
		Repo:    "acme/looper", PRNumber: 1,
	}, items)
	if err != nil {
		t.Fatalf("liveReviewContentFingerprint error = %v", err)
	}
	if liveFP == askFP {
		t.Fatal("resolved native comment must change review content fingerprint")
	}
	if hitl.MaterialFingerprintsMatch("h", "h", askFP, liveFP, "i", "i") {
		t.Fatal("parked answer must not inject against a resolved native comment")
	}
}
