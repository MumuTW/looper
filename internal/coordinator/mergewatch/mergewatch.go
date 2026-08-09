package mergewatch

import (
	"time"

	githubinfra "github.com/MumuTW/looper/internal/infra/github"
)

type WatchActionKind string

const (
	ActionMerged                  WatchActionKind = "Merged"
	ActionStillPending            WatchActionKind = "StillPending"
	ActionIndeterminate           WatchActionKind = "Indeterminate"
	ActionConflict                WatchActionKind = "Conflict"
	ActionRedCI                   WatchActionKind = "RedCI"
	ActionBranchProtectionChanged WatchActionKind = "BranchProtectionChanged"
	ActionHumanDisabledAutoMerge  WatchActionKind = "HumanDisabledAutoMerge"
	ActionTransientError          WatchActionKind = "TransientError"
)

// PriorWatchMarker is the Watch marker: the
// <!-- looper:coordinator:merge-watch retries=N --> HTML-comment marker
// Coordinator places on the linked Issue rather than the PR — keeping
// Coordinator state rooted on the Issue — to carry merge-watch retry-counter
// state across ticks. Public, durable, idempotent; preserves ADR-0001's
// stateless property.
type PriorWatchMarker struct {
	PRNumber int64
	HeadSHA  string
	Retries  int
	// ConflictRepairs is intentionally independent from Retries: transient
	// provider failures may reset while the same PR keeps losing races with
	// the moving base branch.
	ConflictRepairs               int
	ConflictRegenerationPending   bool
	ConflictRegenerationEscalated bool
	// ConflictRegenerationEscalatedState records the fresh PR observation at
	// the human-escalation boundary so later ticks can distinguish an unchanged
	// pause from a pushed head, restored auto-merge, or resolved conflict.
	ConflictRegenerationEscalatedState string
	FirstUnknownAt                     *time.Time
	NextRetryAt                        *time.Time
}

type RetryBudget struct {
	Now                      time.Time
	TransientRetries         int
	MaxIndeterminateDuration time.Duration
}

type RequiredCheckSummary struct {
	Failed  []string
	Pending []string
	Missing []string
}

type TemporaryError struct {
	SuggestedDelay time.Duration
}

type PRSnapshot struct {
	Repo                   string
	PRNumber               int64
	IssueNumber            int64
	HeadSHA                string
	MergedAt               string
	MergedBy               string
	Merged                 bool
	Open                   bool
	AutoMergeEnabled       bool
	AutoMergeOwnedByLooper bool
	// AutoMergeRouteEnabled is true for either GitHub auto-merge owned by
	// Looper or the repository's Mergify auto-merge label route.
	AutoMergeRouteEnabled bool
	HasLooperLabel        bool
	Mergeable             *bool
	MergeableState        githubinfra.MergeabilityState
	RequiredChecks        RequiredCheckSummary
	TemporaryError        *TemporaryError
}

type WatchAction struct {
	Kind             WatchActionKind
	FirstUnknownAt   *time.Time
	DeadlineExceeded bool
	RetriesLeft      int
	SuggestedDelay   time.Duration
	Exhausted        bool
}

func Classify(snapshot PRSnapshot, prior *PriorWatchMarker, budget RetryBudget) WatchAction {
	normalized := normalizePrior(prior, snapshot, budget.TransientRetries)
	if snapshot.TemporaryError != nil {
		retriesLeft := normalized.Retries
		if prior != nil && prior.PRNumber == snapshot.PRNumber && prior.HeadSHA == snapshot.HeadSHA {
			if retriesLeft > 0 {
				retriesLeft--
			}
		}
		return WatchAction{Kind: ActionTransientError, RetriesLeft: retriesLeft, SuggestedDelay: snapshot.TemporaryError.SuggestedDelay, Exhausted: retriesLeft == 0}
	}
	if snapshot.Merged {
		return WatchAction{Kind: ActionMerged}
	}
	if prior != nil && prior.PRNumber == snapshot.PRNumber && !snapshot.AutoMergeEnabled && !snapshot.AutoMergeRouteEnabled {
		return WatchAction{Kind: ActionHumanDisabledAutoMerge}
	}
	if snapshot.Mergeable == nil || snapshot.MergeableState.IsUnknown() {
		firstUnknownAt := normalized.FirstUnknownAt
		if firstUnknownAt == nil {
			now := budget.Now.UTC()
			firstUnknownAt = &now
		}
		deadlineExceeded := budget.MaxIndeterminateDuration > 0 && budget.Now.UTC().Sub(*firstUnknownAt) > budget.MaxIndeterminateDuration
		if deadlineExceeded {
			return WatchAction{Kind: ActionBranchProtectionChanged, FirstUnknownAt: firstUnknownAt, DeadlineExceeded: true}
		}
		return WatchAction{Kind: ActionIndeterminate, FirstUnknownAt: firstUnknownAt}
	}
	if snapshot.MergeableState.HasConflict() {
		return WatchAction{Kind: ActionConflict}
	}
	if len(snapshot.RequiredChecks.Failed) > 0 {
		return WatchAction{Kind: ActionRedCI}
	}
	if len(snapshot.RequiredChecks.Missing) > 0 {
		return WatchAction{Kind: ActionBranchProtectionChanged}
	}
	return WatchAction{Kind: ActionStillPending}
}

func normalizePrior(prior *PriorWatchMarker, snapshot PRSnapshot, fallbackRetries int) PriorWatchMarker {
	if prior == nil || prior.PRNumber != snapshot.PRNumber || prior.HeadSHA != snapshot.HeadSHA {
		return PriorWatchMarker{PRNumber: snapshot.PRNumber, HeadSHA: snapshot.HeadSHA, Retries: fallbackRetries}
	}
	return *prior
}
