package mergewatch

import "testing"

func TestDecideMarkReady(t *testing.T) {
	t.Parallel()
	mergeable := true
	conflicted := false
	ready := func(mutate ...func(*MarkReadySnapshot)) MarkReadySnapshot {
		snapshot := MarkReadySnapshot{
			Repo:           "acme/looper",
			PRNumber:       42,
			IssueNumber:    7,
			HeadSHA:        "abc123",
			Draft:          true,
			Open:           true,
			InScope:        true,
			Mergeable:      &mergeable,
			MergeableState: "draft",
		}
		for _, fn := range mutate {
			fn(&snapshot)
		}
		return snapshot
	}

	for _, tc := range []struct {
		name        string
		snapshot    MarkReadySnapshot
		wantReady   bool
		wantBlocker MarkReadyBlocker
	}{
		{name: "green draft is published", snapshot: ready(), wantReady: true},
		{name: "already published", snapshot: ready(func(s *MarkReadySnapshot) { s.Draft = false }), wantBlocker: MarkReadyBlockerNotDraft},
		{name: "closed", snapshot: ready(func(s *MarkReadySnapshot) { s.Open = false }), wantBlocker: MarkReadyBlockerNotOpen},
		{name: "out of scope", snapshot: ready(func(s *MarkReadySnapshot) { s.InScope = false }), wantBlocker: MarkReadyBlockerOutOfScope},
		{name: "held", snapshot: ready(func(s *MarkReadySnapshot) { s.Held = true }), wantBlocker: MarkReadyBlockerHeld},
		{name: "dirty mergeable state", snapshot: ready(func(s *MarkReadySnapshot) { s.MergeableState = "dirty" }), wantBlocker: MarkReadyBlockerConflict},
		{name: "conflicting branch behind draft state", snapshot: ready(func(s *MarkReadySnapshot) { s.Mergeable = &conflicted }), wantBlocker: MarkReadyBlockerConflict},
		{name: "mergeability still computing", snapshot: ready(func(s *MarkReadySnapshot) { s.Mergeable = nil }), wantBlocker: MarkReadyBlockerMergeUnknown},
		{name: "mergeability unknown", snapshot: ready(func(s *MarkReadySnapshot) { s.MergeableState = "unknown" }), wantBlocker: MarkReadyBlockerMergeUnknown},
		{name: "failed required check", snapshot: ready(func(s *MarkReadySnapshot) { s.RequiredChecks.Failed = []string{"verify"} }), wantBlocker: MarkReadyBlockerChecksNotGreen},
		{name: "pending required check", snapshot: ready(func(s *MarkReadySnapshot) { s.RequiredChecks.Pending = []string{"verify"} }), wantBlocker: MarkReadyBlockerChecksNotGreen},
		{name: "missing required check", snapshot: ready(func(s *MarkReadySnapshot) { s.RequiredChecks.Missing = []string{"verify"} }), wantBlocker: MarkReadyBlockerChecksNotGreen},
		{name: "human commits", snapshot: ready(func(s *MarkReadySnapshot) { s.ForeignCommitAuthors = []string{"octo"} }), wantBlocker: MarkReadyBlockerHumanCommits},
		{name: "unattributable commits", snapshot: ready(func(s *MarkReadySnapshot) { s.UnattributedCommits = 1 }), wantBlocker: MarkReadyBlockerCommitsUnknown},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			decision := DecideMarkReady(tc.snapshot)
			if decision.MarkReady != tc.wantReady {
				t.Fatalf("MarkReady = %v, want %v", decision.MarkReady, tc.wantReady)
			}
			if decision.Blocker != tc.wantBlocker {
				t.Fatalf("Blocker = %q, want %q", decision.Blocker, tc.wantBlocker)
			}
		})
	}
}
