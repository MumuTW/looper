// Package escalator derives operator-facing backlog digests from durable
// workflow state. It never starts, resumes, retries, or otherwise mutates work.
package escalator

import (
	"sort"
	"time"
)

type Kind string

const (
	KindWaiting Kind = "waiting_on_human"
	KindStuck   Kind = "stuck"
)

type Reason string

const (
	ReasonTriageConfirmation Reason = "triage_confirmation"
	ReasonPlannerEscalation  Reason = "planner_escalation"
	ReasonHITLQuestion       Reason = "hitl_question"
	ReasonReviewStall        Reason = "review_stall"
	ReasonEligibleAdvisePR   Reason = "eligible_advise_pr"
	ReasonCircuitBreaker     Reason = "circuit_breaker"
	ReasonQueueRetries       Reason = "queue_retries"
	ReasonTriageNotRouted    Reason = "triage_not_routed"
	ReasonStalePRHead        Reason = "stale_pr_head"
)

// Item is a read-only projection. ID is stable across digests; Fingerprint
// changes only when operator-relevant source state changes, not as AgeSeconds
// advances.
type Item struct {
	ID          string `json:"id"`
	Kind        Kind   `json:"kind"`
	Reason      Reason `json:"reason"`
	ProjectID   string `json:"projectId"`
	Stage       string `json:"stage"`
	Title       string `json:"title"`
	Detail      string `json:"detail,omitempty"`
	Link        string `json:"link"`
	AgeSeconds  int64  `json:"ageSeconds"`
	BlockedWork int    `json:"blockedWork"`
	Fingerprint string `json:"fingerprint"`
}

type StageBacklog struct {
	ProjectID        string `json:"projectId"`
	Stage            string `json:"stage"`
	Depth            int    `json:"depth"`
	OldestAgeSeconds int64  `json:"oldestAgeSeconds"`
}

type Snapshot struct {
	GeneratedAt string         `json:"generatedAt"`
	Items       []Item         `json:"items"`
	Backlog     []StageBacklog `json:"backlog"`
}

// Normalize pins deterministic ordering for rendering, persistence, and delta
// calculation. Blocked work outranks age; ties are stable by identity.
func (s *Snapshot) Normalize() {
	sort.Slice(s.Items, func(i, j int) bool {
		if s.Items[i].BlockedWork != s.Items[j].BlockedWork {
			return s.Items[i].BlockedWork > s.Items[j].BlockedWork
		}
		if s.Items[i].AgeSeconds != s.Items[j].AgeSeconds {
			return s.Items[i].AgeSeconds > s.Items[j].AgeSeconds
		}
		return s.Items[i].ID < s.Items[j].ID
	})
	sort.Slice(s.Backlog, func(i, j int) bool {
		if s.Backlog[i].ProjectID != s.Backlog[j].ProjectID {
			return s.Backlog[i].ProjectID < s.Backlog[j].ProjectID
		}
		return s.Backlog[i].Stage < s.Backlog[j].Stage
	})
}

func ageSeconds(now time.Time, raw string) (int64, error) {
	createdAt, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return 0, err
	}
	age := now.UTC().Sub(createdAt.UTC())
	if age < 0 {
		return 0, nil
	}
	return int64(age / time.Second), nil
}
