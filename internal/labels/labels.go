// Package labels is the single definition point for the forge labels Looper
// reads and writes.
//
// Labels are not decoration. Per ADR-0010 the label on an Issue or Pull
// Request is the durable Authority for work eligibility: a Role claims work
// because a label says it may, not because an agent decided it should. A label
// string is therefore part of Looper's protocol with the forge, and a typo or a
// drifted copy is a silent authority failure — the Role simply never claims.
//
// Before this package the same label was defined privately in two or three
// places at once (looper:worker-ready had three independent definitions), so
// this package exists to make that class of drift impossible rather than
// merely unlikely.
//
// Not in scope: HTML comment markers such as <!-- looper:stamp --> and
// <!-- looper:fixer-round … -->. Those share the "looper:" prefix but are a
// different mechanism with a different authority story — they live with the
// protocol that emits them (internal/disclosure, internal/forge, and the
// reviewer and fixer runners).
package labels

import "strings"

// Prefix is the namespace every Looper-owned label shares. A label carrying it
// is the marker of Looper's involvement with an item, which is why Prefix — not
// any single label — is what "did Looper touch this?" checks look for.
const Prefix = "looper:"

// IsLooperOwned reports whether label is in Looper's namespace.
func IsLooperOwned(label string) bool {
	return strings.HasPrefix(Normalize(label), Prefix)
}

// AnyLooperOwned reports whether any of labels is in Looper's namespace. This
// is half of Auto-merge scope: Looper may opt a Pull Request into auto-merge
// only when it carries a Looper label *and* links a tracked Issue. Both halves
// are required, so a caller that checks only this one is incomplete.
func AnyLooperOwned(labels []string) bool {
	for _, label := range labels {
		if IsLooperOwned(label) {
			return true
		}
	}
	return false
}

// Role trigger defaults.
//
// These are the *default* values for the user-configurable role triggers under
// roles.<role>.triggers.labels. A project may override them, so runtime
// discovery must read the configured value — never these constants — when
// deciding whether a Role may claim. They are defined here so that the config
// default, the label Looper provisions in a repository, and each Role's
// fallback discovery policy cannot disagree about what the default is.
const (
	DefaultPlanTrigger        = "looper:plan"
	DefaultWorkerReadyTrigger = "looper:worker-ready"
)

// Spec PR lifecycle. Looper-owned and not configurable: the Planner publishes
// a spec PR under SpecReviewing, a human promotes it to SpecReady, and the
// Worker claims only what carries SpecReady.
const (
	SpecReviewing = "looper:spec-reviewing"
	SpecReady     = "looper:spec-ready"
	NeedsHuman    = "looper:needs-human"
)

// AwaitingHuman marks work parked for human input. Looper-owned.
const AwaitingHuman = "looper:awaiting-human"

// Holds. A hold is a human veto: HoldGlobal stops every Role on the item, and
// the per-role holds stop exactly one. Looper applies none of these itself —
// they are read-only authority as far as Looper is concerned.
const (
	HoldGlobal   = "looper:hold"
	HoldWorker   = "looper:hold:worker"
	HoldFixer    = "looper:hold:fixer"
	HoldReviewer = "looper:hold:reviewer"
)

// Network target labels (looper:target:<node_name>) are deliberately NOT here.
// Constructing or parsing one requires validating the node name, which is a
// Network concept, so the whole family lives with that validation in
// internal/network/protocol: TargetLabelForNode, ParseTargetLabel,
// CollectTargetLabels, and CollectTargetLikeLabels. Use those rather than
// matching the prefix by hand.

// Normalize puts a label into the form comparisons use. Forge labels are
// case-insensitive and arrive with incidental whitespace from CLI output and
// config files, so every comparison in Looper goes through this.
func Normalize(label string) string {
	return strings.ToLower(strings.TrimSpace(label))
}

// Has reports whether labels contains target, comparing under Normalize.
func Has(labels []string, target string) bool {
	normalizedTarget := Normalize(target)
	for _, label := range labels {
		if Normalize(label) == normalizedTarget {
			return true
		}
	}
	return false
}
