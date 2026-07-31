package mergewatch

// MarkReadyBlocker names the reason a draft Pull Request was left in draft.
// Every value is a reason to do nothing, so a blocker is never an error: the
// merge-watch tick that produced it will simply look again next time, and the
// draft stays exactly as the human left it.
type MarkReadyBlocker string

const (
	MarkReadyBlockerNone           MarkReadyBlocker = ""
	MarkReadyBlockerNotDraft       MarkReadyBlocker = "NotDraft"
	MarkReadyBlockerNotOpen        MarkReadyBlocker = "NotOpen"
	MarkReadyBlockerOutOfScope     MarkReadyBlocker = "OutOfScope"
	MarkReadyBlockerHeld           MarkReadyBlocker = "Held"
	MarkReadyBlockerConflict       MarkReadyBlocker = "Conflict"
	MarkReadyBlockerMergeUnknown   MarkReadyBlocker = "MergeabilityUnknown"
	MarkReadyBlockerChecksNotGreen MarkReadyBlocker = "ChecksNotGreen"
	MarkReadyBlockerHumanCommits   MarkReadyBlocker = "HumanCommits"
	MarkReadyBlockerCommitsUnknown MarkReadyBlocker = "CommitAuthorshipUnknown"
)

// MarkReadySnapshot is everything the mark-ready decision reads. It carries no
// pointers into gateway types on purpose: the decision is a pure function of a
// value, so the guard set is enumerable in a test table rather than reachable
// only through a live forge.
type MarkReadySnapshot struct {
	Repo        string
	PRNumber    int64
	IssueNumber int64
	HeadSHA     string
	Draft       bool
	Open        bool

	// InScope is the Looper-only scope already evaluated by the caller: the
	// looper: label AND a tracked-Issue link, both required. A draft a human
	// opened is never in scope, which is the whole safety story of this lane.
	InScope bool

	// Held is any human veto label on the Pull Request.
	Held bool

	Mergeable      *bool
	MergeableState string
	RequiredChecks RequiredCheckSummary

	// ForeignCommitAuthors lists the logins on the branch that are not the
	// daemon's own. Non-empty means a human (or another bot) has pushed here
	// and the draft is not Looper's to publish.
	ForeignCommitAuthors []string

	// UnattributedCommits counts commits GitHub could not attribute to any
	// account. Authorship is unknown rather than Looper's, so they block too.
	UnattributedCommits int
}

type MarkReadyDecision struct {
	MarkReady bool
	Blocker   MarkReadyBlocker
}

// DecideMarkReady reports whether a looper-authored draft may be published.
//
// Order matters only for which blocker is reported, never for the outcome:
// every condition below must hold, so the function is a conjunction with named
// failure cases. The default is to leave the draft alone — an unrecognised or
// half-known state falls through to a blocker, not to publication.
func DecideMarkReady(snapshot MarkReadySnapshot) MarkReadyDecision {
	switch {
	case !snapshot.Draft:
		return MarkReadyDecision{Blocker: MarkReadyBlockerNotDraft}
	case !snapshot.Open:
		return MarkReadyDecision{Blocker: MarkReadyBlockerNotOpen}
	case !snapshot.InScope:
		return MarkReadyDecision{Blocker: MarkReadyBlockerOutOfScope}
	case snapshot.Held:
		return MarkReadyDecision{Blocker: MarkReadyBlockerHeld}
	}
	// GitHub reports mergeable_state "draft" for a draft Pull Request no
	// matter what the branch looks like, so "dirty" alone cannot be trusted to
	// surface a conflict here: the mergeable boolean is the signal that
	// survives draft state, and a nil one means GitHub is still computing.
	switch {
	case snapshot.MergeableState == "dirty":
		return MarkReadyDecision{Blocker: MarkReadyBlockerConflict}
	case snapshot.Mergeable == nil || snapshot.MergeableState == "unknown":
		return MarkReadyDecision{Blocker: MarkReadyBlockerMergeUnknown}
	case !*snapshot.Mergeable:
		return MarkReadyDecision{Blocker: MarkReadyBlockerConflict}
	}
	if len(snapshot.RequiredChecks.Failed) > 0 || len(snapshot.RequiredChecks.Pending) > 0 || len(snapshot.RequiredChecks.Missing) > 0 {
		return MarkReadyDecision{Blocker: MarkReadyBlockerChecksNotGreen}
	}
	if len(snapshot.ForeignCommitAuthors) > 0 {
		return MarkReadyDecision{Blocker: MarkReadyBlockerHumanCommits}
	}
	if snapshot.UnattributedCommits > 0 {
		return MarkReadyDecision{Blocker: MarkReadyBlockerCommitsUnknown}
	}
	return MarkReadyDecision{MarkReady: true}
}
