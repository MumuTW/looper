package fixer

import (
	"context"
	"testing"

	"github.com/nexu-io/looper/internal/hitl"
	"github.com/nexu-io/looper/internal/storage"
)

func TestHITLContract_GitHubSummaryShapeUnchangedThreadStillMatches(t *testing.T) {
	t.Parallel()
	// Ask-time GitHub fix item: Summary=body text, Body empty; ThreadFingerprint
	// covers the full non-Looper reply chain (id@updatedAt).
	const updated = "2026-07-28T00:00:00Z"
	items := []FixItem{{
		Type: "comment", ID: "c1", ThreadID: "t1",
		Summary: "Please restore configurable strategy", Body: "",
		ThreadFingerprint: "c1@" + updated,
	}}
	askFP := computeReviewContentFingerprint(items)

	// Live refresh with same thread body + fingerprint must produce identical FP.
	github := &fakeGitHubGateway{
		threads: []ReviewThread{{
			ID: "t1",
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
	if liveFP != askFP {
		t.Fatalf("unchanged GitHub-shaped thread FP mismatch:\n ask=%s\nlive=%s", askFP, liveFP)
	}
	if !hitl.MaterialFingerprintsMatch("h", "h", askFP, liveFP, "i", "i") {
		t.Fatal("MaterialFingerprintsMatch failed for identical review FPs")
	}
}

func TestHITLContract_NonRootReplyChangesReviewFP(t *testing.T) {
	t.Parallel()
	const rootUpdated = "2026-07-28T00:00:00Z"
	items := []FixItem{{
		Type: "comment", ID: "c1", ThreadID: "t1",
		Summary: "Please restore configurable strategy", Body: "",
		ThreadFingerprint: "c1@" + rootUpdated,
	}}
	askFP := computeReviewContentFingerprint(items)

	// Same primary comment body, but a human/reviewer reply was added mid-park.
	github := &fakeGitHubGateway{
		threads: []ReviewThread{{
			ID: "t1",
			Comments: []ReviewThreadComment{
				{ID: "c1", Body: "Please restore configurable strategy", UpdatedAt: rootUpdated},
				{ID: "c2", Body: "Also drop the hard-code in prod", UpdatedAt: "2026-07-28T01:00:00Z", Author: "reviewer"},
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
		t.Fatal("non-root reply add must change review content fingerprint via ThreadFingerprint")
	}
}

// TestHITLContract_ReopenedThreadWithDeclinedReplyMatchesAsk covers the second-
// reopen thrash path: a prior <!-- looper-fixer-reply-declined --> remains on
// the thread. Collect-time reviewThreadFingerprintFromNodes and resume-time
// liveReviewThreadFingerprint must exclude it identically so an answered HITL
// decision still MaterialFingerprintsMatch.
func TestHITLContract_ReopenedThreadWithDeclinedReplyMatchesAsk(t *testing.T) {
	t.Parallel()
	const (
		rootUpdated     = "2026-07-28T00:00:00Z"
		declinedUpdated = "2026-07-28T01:00:00Z"
	)
	// Ask-time ThreadFingerprint after collect-time filtering (root only;
	// declined marker excluded — see github.reviewThreadFingerprintFromNodes).
	// Bug shape: ask included "c1@…|c-declined@…" while live excluded declined.
	askThreadFP := "c1@" + rootUpdated
	items := []FixItem{{
		Type: "comment", ID: "c1", ThreadID: "t1",
		Summary: "Please restore configurable strategy", Body: "",
		ThreadFingerprint: askThreadFP,
	}}
	askFP := computeReviewContentFingerprint(items)

	// Live reopened thread still has the declined Looper reply; content unchanged.
	github := &fakeGitHubGateway{
		threads: []ReviewThread{{
			ID: "t1",
			Comments: []ReviewThreadComment{
				{ID: "c1", Body: "Please restore configurable strategy", UpdatedAt: rootUpdated},
				{
					ID: "c-declined", UpdatedAt: declinedUpdated,
					Body: "Not acting: conflicts with PR intent.\n\n<!-- looper-fixer-reply-declined thread:t1 fingerprint:fp1 -->",
				},
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
		t.Fatalf("reopened thread with declined reply must match ask FP:\n ask=%s\nlive=%s", askFP, liveFP)
	}
	if !hitl.MaterialFingerprintsMatch("h", "h", askFP, liveFP, "i", "i") {
		t.Fatal("answered HITL decision must inject when only declined Looper reply is present")
	}

	// Regression: if ask had incorrectly included the declined marker, live
	// (which excludes it) would diverge and block answer injection.
	staleAskItems := []FixItem{{
		Type: "comment", ID: "c1", ThreadID: "t1",
		Summary: "Please restore configurable strategy", Body: "",
		ThreadFingerprint: "c1@" + rootUpdated + "|c-declined@" + declinedUpdated,
	}}
	staleAskFP := computeReviewContentFingerprint(staleAskItems)
	if liveFP == staleAskFP {
		t.Fatal("live FP must differ from ask FP that incorrectly included declined reply")
	}
	if hitl.MaterialFingerprintsMatch("h", "h", staleAskFP, liveFP, "i", "i") {
		t.Fatal("stale declined-inclusive ask FP must not match live (documents the bug)")
	}

	// Live path must also exclude fixed-style reply markers the same way.
	liveDirect := liveReviewThreadFingerprint(ReviewThread{
		ID: "t1",
		Comments: []ReviewThreadComment{
			{ID: "c1", Body: "Please restore configurable strategy", UpdatedAt: rootUpdated},
			{
				ID: "c-declined", UpdatedAt: declinedUpdated,
				Body: "<!-- looper-fixer-reply-declined thread:t1 fingerprint:fp1 -->",
			},
			{
				ID: "c-fixed", UpdatedAt: "2026-07-28T02:00:00Z",
				Body: "<!-- looper-fixer-reply thread:t1 commit:abc -->",
			},
		},
	})
	if liveDirect != "c1@"+rootUpdated {
		t.Fatalf("liveReviewThreadFingerprint = %q, want root only", liveDirect)
	}
}
