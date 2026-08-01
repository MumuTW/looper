// Package convergence evaluates Reviewer/Fixer progress from explicit review
// item artifacts without reading forge prose or inferring missing transitions.
package convergence

import (
	"fmt"
	"sort"
	"strings"

	"github.com/MumuTW/looper/internal/reviewitem"
)

type SeverityFloor string

const (
	SeverityFloorBlocking    SeverityFloor = "blocking"
	SeverityFloorNonBlocking SeverityFloor = "non_blocking"
	SeverityFloorAll         SeverityFloor = "all"
)

type ItemStatus string

const (
	ItemStatusOpen       ItemStatus = "open"
	ItemStatusResolved   ItemStatus = "resolved"
	ItemStatusSuperseded ItemStatus = "superseded"
	ItemStatusDeferred   ItemStatus = "deferred"
)

type FixerResult string

const (
	FixerResultFixed    FixerResult = "fixed"
	FixerResultDeclined FixerResult = "declined"
	FixerResultDeferred FixerResult = "deferred"
)

type Action string

const (
	ActionContinue Action = "continue"
	ActionComplete Action = "complete"
	ActionEscalate Action = "escalate"
)

type Reason string

const (
	ReasonConverging           Reason = "converging"
	ReasonSeverityFloorReached Reason = "severity_floor_reached"
	ReasonStalled              Reason = "stalled"
	ReasonAbsoluteCeiling      Reason = "absolute_round_ceiling"
)

// Status is the human-facing lifecycle label persisted alongside the
// convergence record. It is distinct from ItemStatus, which tracks individual
// review items.
type Status string

const (
	StatusActive        Status = "active"
	StatusAwaitingHuman Status = "awaiting_human"
	StatusCompleted     Status = "completed"
)

// ValidStatus reports whether a persisted status string is a recognized value
// or empty. Empty is permitted because the record may predate the status field.
func ValidStatus(status string) bool {
	switch Status(status) {
	case "", StatusActive, StatusAwaitingHuman, StatusCompleted:
		return true
	default:
		return false
	}
}

type Policy struct {
	MaxConsecutiveUnproductive int           `json:"maxConsecutiveUnproductive"`
	MaxFixerAttemptsPerItem    int           `json:"maxFixerAttemptsPerItem"`
	MaxTotalRounds             int           `json:"maxTotalRounds"`
	SeverityFloor              SeverityFloor `json:"severityFloor"`
}

func DefaultPolicy() Policy {
	return Policy{
		MaxConsecutiveUnproductive: 3,
		MaxFixerAttemptsPerItem:    4,
		MaxTotalRounds:             40,
		SeverityFloor:              SeverityFloorNonBlocking,
	}
}

func (p Policy) Validate() error {
	if p.MaxConsecutiveUnproductive <= 0 {
		return fmt.Errorf("max consecutive unproductive rounds must be positive")
	}
	if p.MaxFixerAttemptsPerItem <= 0 {
		return fmt.Errorf("max fixer attempts per item must be positive")
	}
	if p.MaxTotalRounds <= 0 {
		return fmt.Errorf("max total rounds must be positive")
	}
	switch p.SeverityFloor {
	case SeverityFloorBlocking, SeverityFloorNonBlocking, SeverityFloorAll:
		return nil
	default:
		return fmt.Errorf("unsupported severity floor %q", p.SeverityFloor)
	}
}

// Valid reports whether an Action is a recognized value or empty. Empty is
// permitted because the persisted record omits action until the first round.
func (a Action) Valid() bool {
	switch a {
	case "", ActionContinue, ActionComplete, ActionEscalate:
		return true
	default:
		return false
	}
}

// Valid reports whether a Reason is a recognized value or empty.
func (r Reason) Valid() bool {
	switch r {
	case "", ReasonConverging, ReasonSeverityFloorReached, ReasonStalled, ReasonAbsoluteCeiling:
		return true
	default:
		return false
	}
}

// Validate reports whether an item's persisted fields are internally coherent.
// It mirrors normalizeItem but does not mutate, so a read-only projection can
// reject malformed persisted or client-supplied state without re-running the
// state machine.
func (i Item) Validate() error {
	if strings.TrimSpace(i.ID) == "" {
		return fmt.Errorf("review item id is required")
	}
	if _, err := reviewitem.ParseSeverity(string(i.Severity)); err != nil {
		return fmt.Errorf("review item %q: %w", i.ID, err)
	}
	switch i.Status {
	case ItemStatusOpen, ItemStatusResolved, ItemStatusSuperseded, ItemStatusDeferred:
	default:
		return fmt.Errorf("review item %q has unsupported status %q", i.ID, i.Status)
	}
	switch i.FixerResult {
	case "", FixerResultFixed, FixerResultDeclined, FixerResultDeferred:
	default:
		return fmt.Errorf("review item %q has unsupported fixer result %q", i.ID, i.FixerResult)
	}
	if i.FixerAttempts < 0 {
		return fmt.Errorf("review item %q has negative fixer attempts %d", i.ID, i.FixerAttempts)
	}
	return nil
}

// Validate reports whether a persisted State is internally coherent: counters
// are non-negative and every recorded item passes Item.Validate. It does not
// re-derive productivity or action, so it cannot reject a logically stale but
// syntactically valid record — only malformed metadata that json.Unmarshal
// would otherwise accept silently.
func (s State) Validate() error {
	if s.TotalRounds < 0 {
		return fmt.Errorf("total rounds must be non-negative")
	}
	if s.ConsecutiveUnproductive < 0 {
		return fmt.Errorf("consecutive unproductive must be non-negative")
	}
	for _, item := range s.Items {
		if err := item.Validate(); err != nil {
			return err
		}
	}
	for _, round := range s.History {
		if round.Number < 0 {
			return fmt.Errorf("round number must be non-negative")
		}
	}
	return nil
}

type Item struct {
	ID              string              `json:"id"`
	Severity        reviewitem.Severity `json:"severity"`
	Status          ItemStatus          `json:"status"`
	FixerResult     FixerResult         `json:"fixerResult,omitempty"`
	FixerAttemptKey string              `json:"fixerAttemptKey,omitempty"`
	FixerAttempts   int                 `json:"fixerAttempts,omitempty"`
	Stuck           bool                `json:"stuck,omitempty"`
}

type State struct {
	TotalRounds             int             `json:"totalRounds"`
	ConsecutiveUnproductive int             `json:"consecutiveUnproductive"`
	Items                   map[string]Item `json:"items,omitempty"`
	History                 []RoundSummary  `json:"history,omitempty"`
}

type Round struct {
	Items []Item `json:"items"`
}

type RoundSummary struct {
	Number        int      `json:"number"`
	Productive    bool     `json:"productive"`
	NewItemIDs    []string `json:"newItemIds,omitempty"`
	ClosedItemIDs []string `json:"closedItemIds,omitempty"`
	StuckItemIDs  []string `json:"stuckItemIds,omitempty"`
	OpenItemIDs   []string `json:"openItemIds,omitempty"`
}

type Decision struct {
	State      State  `json:"state"`
	Action     Action `json:"action"`
	Reason     Reason `json:"reason"`
	Productive bool   `json:"productive"`
}

// Evaluate applies one explicit artifact observation. Missing items do not
// imply resolution: a resolved/superseded transition must be present in the
// round, so forge pagination or capture failure cannot manufacture progress.
func Evaluate(previous State, round Round, policy Policy) (Decision, error) {
	if err := policy.Validate(); err != nil {
		return Decision{}, err
	}
	next := cloneState(previous)
	next.TotalRounds++
	summary := RoundSummary{Number: next.TotalRounds}
	seen := make(map[string]struct{}, len(round.Items))

	for _, observed := range round.Items {
		item, err := normalizeItem(observed)
		if err != nil {
			return Decision{}, err
		}
		if _, duplicate := seen[item.ID]; duplicate {
			return Decision{}, fmt.Errorf("duplicate review item id %q", item.ID)
		}
		seen[item.ID] = struct{}{}

		prior, existed := next.Items[item.ID]
		if existed && prior.Severity != item.Severity {
			return Decision{}, fmt.Errorf("review item %q changed severity from %s to %s", item.ID, prior.Severity, item.Severity)
		}
		if existed {
			item.FixerAttempts = prior.FixerAttempts
			item.Stuck = prior.Stuck
			if prior.Stuck && prior.Status == ItemStatusDeferred && item.Status == ItemStatusOpen {
				if item.FixerResult != "" {
					return Decision{}, fmt.Errorf("stuck review item %q cannot receive another fixer attempt", item.ID)
				}
				item.Status = ItemStatusDeferred
			} else if prior.Status != ItemStatusOpen && item.Status == ItemStatusOpen {
				return Decision{}, fmt.Errorf("review item %q cannot transition from %s back to open", item.ID, prior.Status)
			}
		} else {
			item.FixerAttempts = 0
			item.Stuck = false
		}
		if item.FixerResult != "" && fixerAttemptObserved(prior, item, existed) {
			item.FixerAttempts++
		}
		if item.Status == ItemStatusOpen && item.FixerAttempts >= policy.MaxFixerAttemptsPerItem {
			item.Status = ItemStatusDeferred
			item.Stuck = true
			summary.StuckItemIDs = append(summary.StuckItemIDs, item.ID)
		}
		next.Items[item.ID] = item

		if !policy.Includes(item.Severity) {
			continue
		}
		if !existed {
			summary.NewItemIDs = append(summary.NewItemIDs, item.ID)
			summary.Productive = true
		}
		if existed && prior.Status == ItemStatusOpen && (item.Status == ItemStatusResolved || item.Status == ItemStatusSuperseded) {
			summary.ClosedItemIDs = append(summary.ClosedItemIDs, item.ID)
			summary.Productive = true
		}
	}

	for id, item := range next.Items {
		if policy.Includes(item.Severity) && item.Status == ItemStatusOpen {
			summary.OpenItemIDs = append(summary.OpenItemIDs, id)
		}
	}
	sort.Strings(summary.NewItemIDs)
	sort.Strings(summary.ClosedItemIDs)
	sort.Strings(summary.StuckItemIDs)
	sort.Strings(summary.OpenItemIDs)

	if summary.Productive {
		next.ConsecutiveUnproductive = 0
	} else {
		next.ConsecutiveUnproductive++
	}
	next.History = append(next.History, summary)
	decision := Decision{State: next, Productive: summary.Productive}

	if len(summary.OpenItemIDs) == 0 {
		decision.Action = ActionComplete
		decision.Reason = ReasonSeverityFloorReached
		return decision, nil
	}
	if next.TotalRounds >= policy.MaxTotalRounds {
		decision.Action = ActionEscalate
		decision.Reason = ReasonAbsoluteCeiling
		return decision, nil
	}
	if next.ConsecutiveUnproductive >= policy.MaxConsecutiveUnproductive {
		decision.Action = ActionEscalate
		decision.Reason = ReasonStalled
		return decision, nil
	}
	decision.Action = ActionContinue
	decision.Reason = ReasonConverging
	return decision, nil
}

func fixerAttemptObserved(previous Item, current Item, existed bool) bool {
	if !existed {
		return true
	}
	// A durable fixer marker can be observed repeatedly while a reviewer run
	// retries or the daemon restarts. Count only a new marker key; otherwise a
	// single declined/deferred result would exhaust the per-item budget merely
	// by being re-read. Legacy artifacts without a key count only the first
	// transition into a result state.
	if current.FixerAttemptKey != "" || previous.FixerAttemptKey != "" {
		return current.FixerAttemptKey != previous.FixerAttemptKey || current.FixerResult != previous.FixerResult
	}
	return previous.FixerResult == ""
}

func (p Policy) Includes(severity reviewitem.Severity) bool {
	switch p.SeverityFloor {
	case SeverityFloorBlocking:
		return severity == reviewitem.SeverityBlocking
	case SeverityFloorNonBlocking:
		return severity == reviewitem.SeverityBlocking || severity == reviewitem.SeverityNonBlocking
	case SeverityFloorAll:
		return severity == reviewitem.SeverityBlocking || severity == reviewitem.SeverityNonBlocking || severity == reviewitem.SeverityNit
	default:
		return false
	}
}

func normalizeItem(item Item) (Item, error) {
	item.ID = strings.TrimSpace(item.ID)
	if item.ID == "" {
		return Item{}, fmt.Errorf("review item id is required")
	}
	severity, err := reviewitem.ParseSeverity(string(item.Severity))
	if err != nil {
		return Item{}, fmt.Errorf("review item %q: %w", item.ID, err)
	}
	item.Severity = severity
	switch item.Status {
	case ItemStatusOpen, ItemStatusResolved, ItemStatusSuperseded, ItemStatusDeferred:
	default:
		return Item{}, fmt.Errorf("review item %q has unsupported status %q", item.ID, item.Status)
	}
	switch item.FixerResult {
	case "", FixerResultFixed, FixerResultDeclined, FixerResultDeferred:
	default:
		return Item{}, fmt.Errorf("review item %q has unsupported fixer result %q", item.ID, item.FixerResult)
	}
	return item, nil
}

func cloneState(source State) State {
	cloned := source
	cloned.Items = make(map[string]Item, len(source.Items))
	for id, item := range source.Items {
		cloned.Items[id] = item
	}
	cloned.History = append([]RoundSummary(nil), source.History...)
	return cloned
}
