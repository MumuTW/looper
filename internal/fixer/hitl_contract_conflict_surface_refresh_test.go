package fixer

import (
	"context"
	"testing"

	"github.com/nexu-io/looper/internal/hitl"
	"github.com/nexu-io/looper/internal/storage"
)

// Pure conflict/check parks have no review FixItems. Live refresh must still list
// the project's review surfaces so a new actionable review opened while parked
// diverges the fingerprint and blocks resume with stale FixItems.
func TestHITLContract_ConflictOnlyParkDetectsNewGitHubReview(t *testing.T) {
	t.Parallel()
	const updated = "2026-07-28T00:00:00Z"
	// Ask-time baseline: conflict-only FixItems → empty review FP when surfaces
	// were not listed. With surface refresh enabled, ask-time also lists; here we
	// seed ask FP as empty (no open reviews at park) and inject a new thread live.
	conflictItems := []FixItem{{Type: "conflict", Summary: "merge conflict with main"}}
	askFP := computeReviewContentFingerprint(conflictItems)
	if askFP == "" {
		// empty content still produces a stable hash
		askFP = hitl.FingerprintContent()
		_ = askFP
	}
	askFP = computeReviewContentFingerprint(conflictItems)

	github := &fakeGitHubGateway{
		threads: []ReviewThread{{
			ID: "t-new",
			Comments: []ReviewThreadComment{{
				ID: "c-new", Body: "Also fix the timeout", UpdatedAt: updated, Author: "reviewer",
			}},
		}},
	}
	runner := New(Options{GitHub: github, HITLEnabled: true})
	liveFP, err := runner.liveReviewContentFingerprint(context.Background(), stepInput{
		Project: storage.ProjectRecord{ID: "project_1", RepoPath: "/tmp"},
		Repo:    "acme/looper", PRNumber: 1,
	}, conflictItems)
	if err != nil {
		t.Fatalf("liveReviewContentFingerprint error = %v", err)
	}
	if liveFP == askFP {
		t.Fatal("new GitHub review during conflict-only park must change review content fingerprint")
	}
	if hitl.MaterialFingerprintsMatch("h", "h", askFP, liveFP, "i", "i") {
		t.Fatal("parked conflict answer must not inject when a new review appeared mid-park")
	}
}

func TestHITLContract_ConflictOnlyParkUnchangedSurfacesStillMatch(t *testing.T) {
	t.Parallel()
	conflictItems := []FixItem{{Type: "conflict", Summary: "merge conflict with main"}}
	// No open review threads at ask or resume.
	github := &fakeGitHubGateway{threads: nil}
	runner := New(Options{GitHub: github, HITLEnabled: true})
	input := stepInput{
		Project: storage.ProjectRecord{ID: "project_1", RepoPath: "/tmp"},
		Repo:    "acme/looper", PRNumber: 1,
		Checkpoint: fixerCheckpoint{FixItems: conflictItems},
	}
	askFP := runner.askTimeReviewContentFingerprint(context.Background(), input)
	liveFP, err := runner.liveReviewContentFingerprint(context.Background(), input, conflictItems)
	if err != nil {
		t.Fatalf("liveReviewContentFingerprint error = %v", err)
	}
	if liveFP != askFP {
		t.Fatalf("unchanged empty surfaces must match:\n ask=%s\nlive=%s", askFP, liveFP)
	}
}
