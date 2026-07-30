// Package convergence evaluates Reviewer/Fixer progress from explicit review
// item artifacts without reading forge prose or inferring missing transitions.
package convergence

import (
	"fmt"
	"sort"
	"strings"

	"github.com/nexu-io/looper/internal/reviewitem"
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

type Policy struct {
	MaxConsecutiveUnproductive int
	MaxFixerAttemptsPerItem    int
	MaxTotalRounds             int
	SeverityFloor              SeverityFloor
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

type Item struct {
	ID            string
	Severity      reviewitem.Severity
	Status        ItemStatus
	FixerResult   FixerResult
	FixerAttempts int
	Stuck         bool
}

type State struct {
	TotalRounds             int
	ConsecutiveUnproductive int
	Items                   map[string]Item
	History                 []RoundSummary
}

type Round struct {
	Items []Item
}

type RoundSummary struct {
	Number        int
	Productive    bool
	NewItemIDs    []string
	ClosedItemIDs []string
	StuckItemIDs  []string
	OpenItemIDs   []string
}

type Decision struct {
	State      State
	Action     Action
	Reason     Reason
	Productive bool
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
		if item.FixerResult != "" {
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
