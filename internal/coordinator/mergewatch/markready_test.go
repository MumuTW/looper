package mergewatch

import "testing"

func TestDecideMarkReady(t *testing.T) {
	t.Parallel()
	mergeable := true
	conflicted := false
	ready := func(mutate ...func(*MarkReadySnapshot)) MarkReadySnapshot {
		snapshot := MarkReadySnapshot{
			Repo:             "acme/looper",
			PRNumber:         42,
			IssueNumber:      7,
			HeadSHA:          "abc123",
			Draft:            true,
			Open:             true,
			InScope:          true,
			AuthoredByDaemon: true,
			Mergeable:        &mergeable,
			MergeableState:   "draft",
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
		{name: "pull request belongs to a human", snapshot: ready(func(s *MarkReadySnapshot) { s.AuthoredByDaemon = false }), wantBlocker: MarkReadyBlockerForeignAuthor},
		{name: "human converted it back to draft", snapshot: ready(func(s *MarkReadySnapshot) { s.HumanConvertedToDraft = true }), wantBlocker: MarkReadyBlockerHumanDraft},
		{name: "no check is known yet", snapshot: ready(func(s *MarkReadySnapshot) { s.ChecksUnknown = true }), wantBlocker: MarkReadyBlockerChecksUnknown},
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

// The evidence behind a publish is a statement about one head. ConfirmMarkReady
// is what stops it being applied to a different one, and what re-reads the
// guards that can change without the head moving at all.
func TestConfirmMarkReady(t *testing.T) {
	t.Parallel()
	mergeable := true
	snapshot := func(mutate ...func(*MarkReadySnapshot)) MarkReadySnapshot {
		value := MarkReadySnapshot{
			Repo:             "acme/looper",
			PRNumber:         42,
			HeadSHA:          "abc123",
			Draft:            true,
			Open:             true,
			InScope:          true,
			AuthoredByDaemon: true,
			Mergeable:        &mergeable,
			MergeableState:   "draft",
		}
		for _, fn := range mutate {
			fn(&value)
		}
		return value
	}

	for _, tc := range []struct {
		name        string
		confirming  MarkReadySnapshot
		wantReady   bool
		wantBlocker MarkReadyBlocker
	}{
		{name: "unchanged pull request is published", confirming: snapshot(), wantReady: true},
		{name: "push landed after the evidence", confirming: snapshot(func(s *MarkReadySnapshot) { s.HeadSHA = "def456" }), wantBlocker: MarkReadyBlockerHeadMoved},
		{name: "head unreadable", confirming: snapshot(func(s *MarkReadySnapshot) { s.HeadSHA = "" }), wantBlocker: MarkReadyBlockerHeadMoved},
		{name: "base branch retargeted", confirming: snapshot(func(s *MarkReadySnapshot) { s.BaseRefName = "release" }), wantBlocker: MarkReadyBlockerHeadMoved},
		{name: "same-head checks are freshly blocked", confirming: snapshot(func(s *MarkReadySnapshot) { s.RequiredChecks.Pending = []string{"verify"} }), wantBlocker: MarkReadyBlockerChecksNotGreen},
		{name: "hold label landed after the evidence", confirming: snapshot(func(s *MarkReadySnapshot) { s.Held = true }), wantBlocker: MarkReadyBlockerHeld},
		{name: "looper label removed after the evidence", confirming: snapshot(func(s *MarkReadySnapshot) { s.InScope = false }), wantBlocker: MarkReadyBlockerOutOfScope},
		{name: "human published it first", confirming: snapshot(func(s *MarkReadySnapshot) { s.Draft = false }), wantBlocker: MarkReadyBlockerNotDraft},
		{name: "closed after the evidence", confirming: snapshot(func(s *MarkReadySnapshot) { s.Open = false }), wantBlocker: MarkReadyBlockerNotOpen},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			decision := ConfirmMarkReady(snapshot(), tc.confirming)
			if decision.MarkReady != tc.wantReady {
				t.Fatalf("MarkReady = %v, want %v", decision.MarkReady, tc.wantReady)
			}
			if decision.Blocker != tc.wantBlocker {
				t.Fatalf("Blocker = %q, want %q", decision.Blocker, tc.wantBlocker)
			}
		})
	}
}

// A moved head invalidates the check and commit evidence gathered against the
// old one, so the comparison comes first: carrying green evidence onto a head
// nobody has evaluated is the failure this ordering exists to prevent.
func TestConfirmMarkReadyChecksHeadBeforeCarriedEvidence(t *testing.T) {
	t.Parallel()
	mergeable := true
	evidence := MarkReadySnapshot{
		HeadSHA: "abc123", BaseRefName: "main", Draft: true, Open: true, InScope: true, AuthoredByDaemon: true,
		Mergeable: &mergeable, MergeableState: "draft",
	}
	confirming := evidence
	confirming.HeadSHA = "def456"
	confirming.RequiredChecks.Pending = []string{"verify"}
	if decision := ConfirmMarkReady(evidence, confirming); decision.Blocker != MarkReadyBlockerHeadMoved {
		t.Fatalf("Blocker = %q, want %q", decision.Blocker, MarkReadyBlockerHeadMoved)
	}
}

func TestHumanConvertedToDraft(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		events []MarkReadyDraftEvent
		want   bool
	}{
		{name: "machine draft has no lifecycle events"},
		{
			name:   "daemon published and nobody re-drafted",
			events: []MarkReadyDraftEvent{{Event: "ready_for_review", Actor: "looper", CreatedAt: "2026-05-14T12:00:00Z"}},
		},
		{
			name: "human converted the published pull request back",
			events: []MarkReadyDraftEvent{
				{Event: "ready_for_review", Actor: "looper", CreatedAt: "2026-05-14T12:00:00Z"},
				{Event: "convert_to_draft", Actor: "octo", CreatedAt: "2026-05-14T12:05:00Z"},
			},
			want: true,
		},
		{
			name:   "human converted a pull request the daemon never published",
			events: []MarkReadyDraftEvent{{Event: "convert_to_draft", Actor: "octo", CreatedAt: "2026-05-14T12:05:00Z"}},
			want:   true,
		},
		{
			name:   "actor GitHub could not name",
			events: []MarkReadyDraftEvent{{Event: "convert_to_draft", CreatedAt: "2026-05-14T12:05:00Z"}},
			want:   true,
		},
		{
			name: "human re-drafted and the daemon published again",
			events: []MarkReadyDraftEvent{
				{Event: "convert_to_draft", Actor: "octo", CreatedAt: "2026-05-14T12:05:00Z"},
				{Event: "ready_for_review", Actor: "looper", CreatedAt: "2026-05-14T12:10:00Z"},
			},
			want: true,
		},
		{
			name: "conversion attributed to the daemon after its own publish",
			events: []MarkReadyDraftEvent{
				{Event: "ready_for_review", Actor: "Looper", CreatedAt: "2026-05-14T12:00:00Z"},
				{Event: "convert_to_draft", Actor: "LOOPER", CreatedAt: "2026-05-14T12:05:00Z"},
			},
			want: true,
		},
		{
			name: "unparseable timestamps fall back to arrival order",
			events: []MarkReadyDraftEvent{
				{Event: "convert_to_draft", Actor: "octo"},
				{Event: "ready_for_review", Actor: "looper"},
			},
			want: true,
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := HumanConvertedToDraft(tc.events, "looper"); got != tc.want {
				t.Fatalf("HumanConvertedToDraft() = %v, want %v", got, tc.want)
			}
		})
	}
}

// Without a daemon identity nothing on the timeline can be attributed to
// Looper, so every conversion is somebody else's.
func TestHumanConvertedToDraftWithoutDaemonLogin(t *testing.T) {
	t.Parallel()
	events := []MarkReadyDraftEvent{{Event: "convert_to_draft", Actor: "looper", CreatedAt: "2026-05-14T12:05:00Z"}}
	if !HumanConvertedToDraft(events, "  ") {
		t.Fatal("HumanConvertedToDraft() = false, want true when the daemon identity is unknown")
	}
}
