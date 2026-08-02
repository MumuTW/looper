// Package labels is the single definition point for the forge labels Looper
// reads and writes, with two documented exceptions below: Network target
// labels and comment markers.
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

import (
	"fmt"
	"sort"
	"strings"
)

// Prefix is the namespace every Looper-owned label shares. A label carrying it
// is the marker of Looper's involvement with an item, which is why Prefix — not
// any single label — is what "did Looper touch this?" checks look for.
const Prefix = "looper:"

// maxNamespacePrefixLength leaves room for the longest standard suffix
// ("dispatch:implement") within GitHub's 50-character label-name limit.
const maxNamespacePrefixLength = 32

// Namespace is the project-scoped label authority. The default namespace is
// Prefix; a project may opt into another validated prefix so two Looper
// instances can coexist on one host repository without sharing control labels.
// Legacy bare dispatch labels are readable through IsDispatch* for one release,
// but are never emitted by the namespace.
type Namespace struct {
	Prefix string
}

func DefaultNamespace() Namespace { return Namespace{Prefix: Prefix} }

// ValidatePrefix accepts a label namespace such as "looper:" or
// "team.looper:". The colon is required so a namespace cannot accidentally
// match an ordinary host label prefix.
func ValidatePrefix(prefix string) error {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return nil
	}
	if len(prefix) < 2 || len(prefix) > maxNamespacePrefixLength || !strings.HasSuffix(prefix, ":") {
		return fmt.Errorf("label namespace must be 1-%d characters followed by ':'", maxNamespacePrefixLength-1)
	}
	for _, r := range prefix[:len(prefix)-1] {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			continue
		}
		return fmt.Errorf("label namespace contains unsupported character %q", r)
	}
	return nil
}

// NewNamespace normalizes a validated prefix. Invalid non-empty prefixes
// fall back to the safe default; configuration validation rejects them before
// runtime, while this fallback keeps legacy callers fail-closed and
// deterministic.
func NewNamespace(prefix string) Namespace {
	prefix = strings.ToLower(strings.TrimSpace(prefix))
	if prefix == "" {
		return DefaultNamespace()
	}
	if ValidatePrefix(prefix) != nil {
		return DefaultNamespace()
	}
	return Namespace{Prefix: prefix}
}

func (n Namespace) normalized() Namespace {
	if strings.TrimSpace(n.Prefix) == "" {
		return DefaultNamespace()
	}
	return NewNamespace(n.Prefix)
}

func (n Namespace) Label(suffix string) string {
	n = n.normalized()
	suffix = strings.TrimSpace(suffix)
	lower := strings.ToLower(suffix)
	for _, prefix := range []string{n.Prefix, Prefix} {
		if strings.HasPrefix(lower, prefix) {
			suffix = suffix[len(prefix):]
			break
		}
	}
	return n.Prefix + suffix
}

// Remap moves a label that belongs to the default Looper namespace into this
// project's namespace. Foreign labels are returned unchanged; this is what
// keeps configured host-repository labels read-only.
func (n Namespace) Remap(label string) string {
	if strings.HasPrefix(Normalize(label), Prefix) {
		return n.Label(label)
	}
	return strings.TrimSpace(label)
}

func (n Namespace) RemapAll(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	remapped := make([]string, 0, len(values))
	for _, value := range values {
		if value = n.Remap(value); value != "" {
			remapped = append(remapped, value)
		}
	}
	return remapped
}

func (n Namespace) PlanTrigger() string        { return n.Label("plan") }
func (n Namespace) WorkerReadyTrigger() string { return n.Label("worker-ready") }
func (n Namespace) SpecReviewing() string      { return n.Label("spec-reviewing") }
func (n Namespace) SpecReady() string          { return n.Label("spec-ready") }
func (n Namespace) NeedsHuman() string         { return n.Label("needs-human") }
func (n Namespace) AwaitingHuman() string      { return n.Label("awaiting-human") }
func (n Namespace) HoldGlobal() string         { return n.Label("hold") }
func (n Namespace) HoldWorker() string         { return n.Label("hold:worker") }
func (n Namespace) HoldFixer() string          { return n.Label("hold:fixer") }
func (n Namespace) HoldReviewer() string       { return n.Label("hold:reviewer") }

func (n Namespace) DispatchPlan() string      { return n.Label("dispatch:plan") }
func (n Namespace) DispatchImplement() string { return n.Label("dispatch:implement") }

func (n Namespace) DispatchLabels() []string {
	return []string{n.DispatchPlan(), n.DispatchImplement()}
}

func (n Namespace) IsOwned(label string) bool {
	return strings.HasPrefix(Normalize(label), n.normalized().Prefix)
}

func (n Namespace) AnyOwned(labels []string) bool {
	for _, label := range labels {
		if n.IsOwned(label) {
			return true
		}
	}
	return false
}

func (n Namespace) IsDispatch(label string) bool {
	return n.IsConfiguredDispatch(label) || (n.acceptsLegacyDispatch() && (Normalize(label) == "dispatch/plan" || Normalize(label) == "dispatch/implement"))
}

func (n Namespace) IsConfiguredDispatch(label string) bool {
	switch Normalize(label) {
	case Normalize(n.DispatchPlan()), Normalize(n.DispatchImplement()):
		return true
	default:
		return false
	}
}

func (n Namespace) IsDispatchPlan(label string) bool {
	return n.IsConfiguredDispatchPlan(label) || (n.acceptsLegacyDispatch() && Normalize(label) == "dispatch/plan")
}

func (n Namespace) IsConfiguredDispatchPlan(label string) bool {
	return Normalize(label) == Normalize(n.DispatchPlan())
}

func (n Namespace) IsDispatchPlanForLabels(values []string) bool {
	for _, value := range values {
		if n.IsDispatchPlan(value) {
			return true
		}
	}
	return false
}

func (n Namespace) IsDispatchImplement(label string) bool {
	return n.IsConfiguredDispatchImplement(label) || (n.acceptsLegacyDispatch() && Normalize(label) == "dispatch/implement")
}

func (n Namespace) acceptsLegacyDispatch() bool {
	return n.normalized().Prefix == Prefix
}

func (n Namespace) IsConfiguredDispatchImplement(label string) bool {
	return Normalize(label) == Normalize(n.DispatchImplement())
}

func (n Namespace) Standard() []Definition {
	return []Definition{
		{Name: n.PlanTrigger(), Color: "5319e7", Description: "Picked up automatically by planner"},
		{Name: n.WorkerReadyTrigger(), Color: "0e8a16", Description: "Ready for Looper worker implementation"},
		{Name: n.DispatchPlan(), Color: "5319e7", Description: "Coordinator dispatches this issue to the planner"},
		{Name: n.DispatchImplement(), Color: "0e8a16", Description: "Coordinator dispatches this issue to the worker"},
		{Name: n.SpecReviewing(), Color: "1d76db", Description: "Spec PR is under review"},
		{Name: n.SpecReady(), Color: "0e8a16", Description: "Spec PR is ready for implementation"},
		{Name: n.NeedsHuman(), Color: "d93f0b", Description: "Looper requires manual intervention"},
		{Name: n.AwaitingHuman(), Color: "fbca04", Description: "Waiting on a human response before Looper continues"},
		{Name: n.HoldGlobal(), Color: "b60205", Description: "Pause all automatic Looper work"},
		{Name: n.HoldWorker(), Color: "b60205", Description: "Pause automatic worker work"},
		{Name: n.HoldFixer(), Color: "b60205", Description: "Pause automatic fixer work"},
		{Name: n.HoldReviewer(), Color: "b60205", Description: "Pause automatic reviewer work"},
	}
}

// DefinitionFor returns the standard presentation for a namespaced built-in
// label. It recognizes both the default and a validated custom prefix so the
// GitHub gateway can provision the same presentation without knowing the
// project's configuration type.
func DefinitionFor(label string) (Definition, bool) {
	normalized := Normalize(label)
	definitions := DefaultNamespace().Standard()
	sort.SliceStable(definitions, func(i, j int) bool {
		iSuffix := strings.TrimPrefix(Normalize(definitions[i].Name), Prefix)
		jSuffix := strings.TrimPrefix(Normalize(definitions[j].Name), Prefix)
		return len(iSuffix) > len(jSuffix)
	})
	for _, definition := range definitions {
		suffix := strings.TrimPrefix(Normalize(definition.Name), Prefix)
		if !strings.HasSuffix(normalized, suffix) {
			continue
		}
		prefix := strings.TrimSuffix(normalized, suffix)
		if err := ValidatePrefix(prefix); err != nil {
			continue
		}
		namespace := NewNamespace(prefix)
		for _, candidate := range namespace.Standard() {
			if Normalize(candidate.Name) == normalized {
				return candidate, true
			}
		}
	}
	return Definition{}, false
}

// IsLooperOwned reports whether label is in Looper's namespace.
func IsLooperOwned(label string) bool {
	return DefaultNamespace().IsOwned(label)
}

// AnyLooperOwned reports whether any of labels is in Looper's namespace. This
// is half of Auto-merge scope: Looper may opt a Pull Request into auto-merge
// only when it carries a Looper label *and* links a tracked Issue. Both halves
// are required, so a caller that checks only this one is incomplete.
func AnyLooperOwned(labels []string) bool {
	return DefaultNamespace().AnyOwned(labels)
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
	DispatchPlan              = Prefix + "dispatch:plan"
	DispatchImplement         = Prefix + "dispatch:implement"
)

// DispatchLabels returns the complete set of Coordinator-owned dispatch
// labels. It is deliberately exact, rather than a prefix pattern: a host
// repository's `dispatch/*` taxonomy is foreign state and must never be read
// as authority or removed by Looper.
func DispatchLabels() []string {
	return DefaultNamespace().DispatchLabels()
}

// IsDispatch reports whether label is one of the Coordinator's exact dispatch
// labels. It does not accept legacy bare `dispatch/*` labels.
func IsDispatch(label string) bool {
	return Has([]string{label}, DispatchPlan) || Has([]string{label}, DispatchImplement)
}

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

// DoNotMerge is the widely-used community label that is not in Looper's
// namespace and that Looper therefore never creates, never removes, and never
// lists in Standard() — it is read-only authority in exactly the way the holds
// above are. It lives here anyway because the alternative is each reader
// spelling the string itself, which is the drift this package exists to
// prevent.
const DoNotMerge = "do-not-merge"

// Network target labels (looper:target:<node_name>) are deliberately NOT here.
// Constructing or parsing one requires validating the node name, which is a
// Network concept, so the whole family lives with that validation in
// internal/network/protocol: TargetLabelForNode, ParseTargetLabel,
// CollectTargetLabels, and CollectTargetLikeLabels. Use those rather than
// matching the prefix by hand.

// Definition is a label together with the presentation a repository should
// carry for it. Identity and presentation live together so that adding a label
// forces a decision about how it reads in the forge UI, rather than leaving it
// to a default that says nothing.
type Definition struct {
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

// Standard returns every label Looper provisions into a managed repository.
//
// Provisioning creates what is missing and never edits what is already there.
// Colors and descriptions here therefore describe the intended default for a
// fresh repository, not an authority over an existing one: a maintainer who
// has reworded a label in the forge keeps their wording.
//
// Where a label already existed in a repository Looper manages, that live
// value is recorded here rather than a fresh invention, so that the two agree
// from the start instead of the table quietly describing a different label
// than the one in use.
func Standard() []Definition {
	return DefaultNamespace().Standard()
}

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
		normalized := Normalize(label)
		if normalizedTarget == DoNotMerge {
			normalized = normalizeDoNotMerge(label)
		}
		if normalized == normalizedTarget {
			return true
		}
	}
	return false
}

func normalizeDoNotMerge(label string) string {
	parts := strings.Fields(strings.NewReplacer("_", " ", "-", " ").Replace(label))
	return strings.ToLower(strings.Join(parts, "-"))
}
