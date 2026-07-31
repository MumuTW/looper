package mergewatch

import (
	"strings"
	"time"
)

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
	MarkReadyBlockerForeignAuthor  MarkReadyBlocker = "NotDaemonAuthored"
	MarkReadyBlockerHumanDraft     MarkReadyBlocker = "HumanConvertedToDraft"
	MarkReadyBlockerHeld           MarkReadyBlocker = "Held"
	MarkReadyBlockerConflict       MarkReadyBlocker = "Conflict"
	MarkReadyBlockerMergeUnknown   MarkReadyBlocker = "MergeabilityUnknown"
	MarkReadyBlockerChecksUnknown  MarkReadyBlocker = "ChecksUnknown"
	MarkReadyBlockerChecksNotGreen MarkReadyBlocker = "ChecksNotGreen"
	MarkReadyBlockerHumanCommits   MarkReadyBlocker = "HumanCommits"
	MarkReadyBlockerCommitsUnknown MarkReadyBlocker = "CommitAuthorshipUnknown"
	MarkReadyBlockerHeadMoved      MarkReadyBlocker = "HeadMoved"
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

	// AuthoredByDaemon is whether the Pull Request itself belongs to the
	// account the daemon runs as. A maintainer can open a draft over a branch
	// of purely machine-written commits and label it: that satisfies both
	// halves of InScope and every commit check below, yet the Pull Request is
	// theirs to publish, not Looper's.
	AuthoredByDaemon bool

	// HumanConvertedToDraft is whether the timeline shows somebody other than
	// the daemon putting this Pull Request back into draft. That is an explicit
	// lifecycle command — "not ready" — and it outranks every green signal
	// below it.
	HumanConvertedToDraft bool

	// Held is any human veto label on the Pull Request.
	Held bool

	Mergeable      *bool
	MergeableState string
	RequiredChecks RequiredCheckSummary

	// ChecksUnknown is set when the caller could not establish what "green"
	// means for this head: branch protection names no required check and the
	// forge has reported no check of its own yet. An empty check set then says
	// nothing about CI — it is the same value a tick a second before the first
	// workflow registers would see — so it is a reason to wait, not to publish.
	ChecksUnknown bool

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
	case !snapshot.AuthoredByDaemon:
		return MarkReadyDecision{Blocker: MarkReadyBlockerForeignAuthor}
	case snapshot.HumanConvertedToDraft:
		return MarkReadyDecision{Blocker: MarkReadyBlockerHumanDraft}
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
	if snapshot.ChecksUnknown {
		return MarkReadyDecision{Blocker: MarkReadyBlockerChecksUnknown}
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

// ConfirmMarkReady re-runs the decision against a Pull Request read again
// immediately before the mutation.
//
// The evidence gathered per head — required checks, commit authorship, draft
// history — is carried over rather than re-read, which is sound only because
// the head is compared first: a push that landed since the evidence was
// gathered invalidates all of it, and this reports HeadMoved without consulting
// any of it. Everything the confirming read does carry — draft state, open
// state, scope, ownership, hold labels, mergeability — is re-evaluated in full,
// because each of those can change without moving the head.
func ConfirmMarkReady(evidence, confirming MarkReadySnapshot) MarkReadyDecision {
	if !sameHead(evidence.HeadSHA, confirming.HeadSHA) {
		return MarkReadyDecision{Blocker: MarkReadyBlockerHeadMoved}
	}
	confirming.RequiredChecks = evidence.RequiredChecks
	confirming.ChecksUnknown = evidence.ChecksUnknown
	confirming.ForeignCommitAuthors = evidence.ForeignCommitAuthors
	confirming.UnattributedCommits = evidence.UnattributedCommits
	confirming.HumanConvertedToDraft = evidence.HumanConvertedToDraft
	return DecideMarkReady(confirming)
}

func sameHead(left, right string) bool {
	left = strings.TrimSpace(left)
	right = strings.TrimSpace(right)
	return left != "" && right != "" && strings.EqualFold(left, right)
}

// MarkReadyDraftEvent is one draft-lifecycle event on a Pull Request timeline,
// reduced to the three things the decision below needs.
type MarkReadyDraftEvent struct {
	Event     string
	Actor     string
	CreatedAt string
}

const (
	draftEventConvertToDraft = "convert_to_draft"
	draftEventReadyForReview = "ready_for_review"
)

// HumanConvertedToDraft reports whether the draft in front of the lane is a
// human lifecycle command rather than the machine's own initial draft.
//
// The timeline is the authority because it is the only record that survives a
// daemon restart and cannot be forged by the state of the Pull Request itself:
// a Pull Request opened as a draft has no convert_to_draft event at all, so the
// initial machine draft and a human re-draft are distinguishable only here.
//
// A convert_to_draft by anyone but the daemon blocks. So does one whose actor
// GitHub does not name, and one attributed to the daemon that postdates a
// publish the daemon performed: the daemon has no code path that converts a
// Pull Request back to draft, so either shape means the event's provenance is
// not what it appears to be, and unknown provenance is not permission.
func HumanConvertedToDraft(events []MarkReadyDraftEvent, daemonLogin string) bool {
	daemon := strings.ToLower(strings.TrimSpace(daemonLogin))
	var converted, published draftEventOrder
	hasConverted, hasPublished := false, false
	for index, event := range events {
		ordered := draftEventOrder{
			at:    parseDraftEventTime(event.CreatedAt),
			index: index,
			actor: strings.ToLower(strings.TrimSpace(event.Actor)),
		}
		switch strings.ToLower(strings.TrimSpace(event.Event)) {
		case draftEventConvertToDraft:
			if !hasConverted || ordered.after(converted) {
				converted, hasConverted = ordered, true
			}
		case draftEventReadyForReview:
			if daemon == "" || ordered.actor != daemon {
				continue
			}
			if !hasPublished || ordered.after(published) {
				published, hasPublished = ordered, true
			}
		}
	}
	if !hasConverted {
		return false
	}
	if daemon == "" || converted.actor != daemon {
		return true
	}
	return hasPublished && !published.after(converted)
}

// draftEventOrder orders two timeline events. GitHub returns the timeline in
// chronological order already; comparing timestamps first and falling back to
// arrival order means neither an unparseable timestamp nor a reordered page can
// silently invert the answer.
type draftEventOrder struct {
	at    time.Time
	index int
	actor string
}

func (d draftEventOrder) after(other draftEventOrder) bool {
	if !d.at.Equal(other.at) {
		return d.at.After(other.at)
	}
	return d.index > other.index
}

func parseDraftEventTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(value))
	if err != nil {
		return time.Time{}
	}
	return parsed.UTC()
}
