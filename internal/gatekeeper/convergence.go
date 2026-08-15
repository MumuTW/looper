package gatekeeper

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/MumuTW/looper/internal/domain"
	"github.com/MumuTW/looper/internal/reviewer/convergence"
	"github.com/MumuTW/looper/internal/reviewitem"
	"github.com/MumuTW/looper/internal/storage"
)

// ReviewerConvergenceEvidence is the read-only convergence state that Gatekeeper
// used for this evaluation. The Reviewer loop metadata is the authority; this is
// an audit projection, not a second lifecycle record.
type ReviewerConvergenceEvidence struct {
	Policy                  convergence.Policy `json:"policy"`
	Status                  string             `json:"status,omitempty"`
	Action                  convergence.Action `json:"action,omitempty"`
	Reason                  convergence.Reason `json:"reason,omitempty"`
	TotalRounds             int                `json:"totalRounds"`
	ConsecutiveUnproductive int                `json:"consecutiveUnproductive"`
	OpenItemIDs             []string           `json:"openItemIds"`
	// ReviewedHeadSHA is the head the Reviewer loop last recorded a completed
	// review round against (loop.lastReviewedHeadSha). The convergence state is
	// authoritative only for that head; a different observed head means the
	// persisted decision belongs to a commit the gate is no longer evaluating.
	ReviewedHeadSHA string `json:"reviewedHeadSha,omitempty"`
	// UpdatedAt is the convergence metadata revision stamp. The unchanged path
	// compares it locally to detect Reviewer progress without re-polling forge.
	UpdatedAt string `json:"updatedAt,omitempty"`
	// HeadStale is true when ReviewedHeadSHA does not match the head Gatekeeper
	// observed for this evaluation, so the persisted decision is not
	// authoritative for the current head and the gate fails closed.
	HeadStale bool `json:"headStale,omitempty"`
	// PendingStart is synthesized (not read from loop metadata) when the newest
	// Reviewer loop has not yet written convergence metadata but may still do
	// so — for example a loop queued for this PR that has not started running.
	// The review is pending, so the gate blocks until a decision lands.
	PendingStart bool `json:"pendingStart,omitempty"`
}

type reviewerConvergenceMetadata struct {
	Policy    convergence.Policy `json:"policy"`
	State     convergence.State  `json:"state"`
	Action    convergence.Action `json:"action,omitempty"`
	Reason    convergence.Reason `json:"reason,omitempty"`
	Status    string             `json:"status,omitempty"`
	UpdatedAt string             `json:"updatedAt,omitempty"`
}

// latestReviewerConvergence reads only the newest Reviewer loop for this
// project/repository/pull request. A newer loop that can no longer produce a
// convergence decision (terminal without metadata) deliberately hides older
// metadata rather than letting stale state block a new review. A newer loop
// that has not written metadata yet but may still converge — queued, idle,
// waiting, or mid-run — is pending, not absent: Gatekeeper blocks until the
// Reviewer records its decision, so auto trust cannot merge a PR before its
// pending Reviewer executes. Missing or malformed legacy metadata is not
// inferred.
//
// observedHeadSHA binds the persisted convergence decision to the head
// Gatekeeper is evaluating: the Reviewer's state is authoritative only for the
// head it last reviewed, so a mismatch fails closed instead of letting a clean
// decision for a prior head authorize the current one.
func latestReviewerConvergence(ctx context.Context, repos *storage.Repositories, projectID, repo string, prNumber int64, observedHeadSHA string) (ReviewerConvergenceEvidence, bool, error) {
	if repos == nil || repos.Loops == nil {
		return ReviewerConvergenceEvidence{}, false, nil
	}
	loops, err := repos.Loops.ListByRepoAndPR(ctx, repo, prNumber)
	if err != nil {
		return ReviewerConvergenceEvidence{}, false, fmt.Errorf("list reviewer convergence loops: %w", err)
	}
	for _, loop := range loops {
		if loop.ProjectID != projectID || loop.Type != string(domain.LoopTypeReviewer) {
			continue
		}
		evidence, ok := reviewerConvergenceFromLoop(loop)
		if !ok {
			if reviewerLoopMayStillConverge(loop.Status) {
				return ReviewerConvergenceEvidence{PendingStart: true}, true, nil
			}
			return ReviewerConvergenceEvidence{}, false, nil
		}
		evidence.HeadStale = evidence.ReviewedHeadSHA != observedHeadSHA
		return evidence, true, nil
	}
	return ReviewerConvergenceEvidence{}, false, nil
}

// reviewerLoopMayStillConverge reports whether a Reviewer loop in this status
// may still write convergence metadata. Only terminal statuses are excluded;
// an unknown future status fails closed as pending rather than authorizing a
// merge on an unreviewed PR.
func reviewerLoopMayStillConverge(status string) bool {
	switch domain.LoopStatus(strings.TrimSpace(status)) {
	case domain.LoopStatusStopped, domain.LoopStatusTerminated, domain.LoopStatusCompleted,
		domain.LoopStatusFailed, domain.LoopStatusInterrupted:
		return false
	default:
		return true
	}
}

func reviewerConvergenceFromLoop(loop storage.LoopRecord) (ReviewerConvergenceEvidence, bool) {
	if loop.MetadataJSON == nil || strings.TrimSpace(*loop.MetadataJSON) == "" {
		return ReviewerConvergenceEvidence{}, false
	}
	var metadata map[string]any
	if err := json.Unmarshal([]byte(*loop.MetadataJSON), &metadata); err != nil {
		return ReviewerConvergenceEvidence{}, false
	}
	raw, ok := metadata["convergence"]
	if !ok || raw == nil {
		return ReviewerConvergenceEvidence{}, false
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return ReviewerConvergenceEvidence{}, false
	}
	var persisted reviewerConvergenceMetadata
	if err := json.Unmarshal(encoded, &persisted); err != nil || persisted.Policy.Validate() != nil || !validConvergenceStatus(persisted.Status) || !validConvergenceState(persisted.State) {
		return ReviewerConvergenceEvidence{}, false
	}

	openItemIDs := make([]string, 0)
	for id, item := range persisted.State.Items {
		if persisted.Policy.Includes(item.Severity) && item.Status == convergence.ItemStatusOpen {
			openItemIDs = append(openItemIDs, id)
		}
	}
	sort.Strings(openItemIDs)
	return ReviewerConvergenceEvidence{
		Policy:                  persisted.Policy,
		Status:                  persisted.Status,
		Action:                  persisted.Action,
		Reason:                  persisted.Reason,
		TotalRounds:             persisted.State.TotalRounds,
		ConsecutiveUnproductive: persisted.State.ConsecutiveUnproductive,
		OpenItemIDs:             openItemIDs,
		ReviewedHeadSHA:         reviewerLoopReviewedHead(metadata),
		UpdatedAt:               persisted.UpdatedAt,
	}, true
}

// reviewerLoopReviewedHead reads the head the Reviewer loop last recorded a
// completed review round against (loop.lastReviewedHeadSha). It is the
// authoritative binding between a persisted convergence decision and the commit
// it was reached on; convergence state retained across a head push does not
// transfer to the new head.
func reviewerLoopReviewedHead(metadata map[string]any) string {
	loopRaw, ok := metadata["loop"]
	if !ok || loopRaw == nil {
		return ""
	}
	loopMap, ok := loopRaw.(map[string]any)
	if !ok {
		return ""
	}
	head, _ := loopMap["lastReviewedHeadSha"].(string)
	return strings.TrimSpace(head)
}

func validConvergenceStatus(status string) bool {
	switch status {
	case "", "active", "awaiting_human", "completed":
		return true
	default:
		return false
	}
}

func validConvergenceState(state convergence.State) bool {
	for id, item := range state.Items {
		if strings.TrimSpace(id) == "" || strings.TrimSpace(item.ID) == "" || id != item.ID {
			return false
		}
		if _, err := reviewitem.ParseSeverity(string(item.Severity)); err != nil {
			return false
		}
		switch item.Status {
		case convergence.ItemStatusOpen, convergence.ItemStatusResolved, convergence.ItemStatusSuperseded, convergence.ItemStatusDeferred:
		default:
			return false
		}
	}
	return true
}

func reviewerConvergenceReasonSubject(evidence ReviewerConvergenceEvidence) string {
	if evidence.PendingStart {
		return "loop_pending_start"
	}
	parts := []string{"floor=" + string(evidence.Policy.SeverityFloor)}
	if evidence.Status != "" {
		parts = append(parts, "status="+evidence.Status)
	}
	if evidence.Reason != "" {
		parts = append(parts, "reason="+string(evidence.Reason))
	}
	if len(evidence.OpenItemIDs) > 0 {
		parts = append(parts, "open="+strings.Join(evidence.OpenItemIDs, ","))
	}
	return strings.Join(parts, ";")
}

func reviewerConvergenceBlocks(evidence ReviewerConvergenceEvidence) bool {
	// The newest Reviewer loop has not written convergence metadata yet but may
	// still do so: the review is pending, not complete.
	if evidence.PendingStart {
		return true
	}
	// Awaiting human is itself a durable decision boundary. Normal evaluator
	// output always carries floor-qualified open items here, but retaining the
	// status guard keeps a partially written escalation fail-closed.
	if evidence.Status == "awaiting_human" || len(evidence.OpenItemIDs) > 0 {
		return true
	}
	// A loop that has persisted a convergence policy but not yet recorded its
	// first round carries an empty state with no action. Treat the absence of
	// an authoritative ActionComplete decision as pending so auto mode cannot
	// merge while the Reviewer agent is still running.
	if evidence.Action != convergence.ActionComplete {
		return true
	}
	// The persisted decision is authoritative only for the head the Reviewer
	// last reviewed. A different observed head means a clean decision for a
	// prior commit must not authorize the current one.
	return evidence.HeadStale
}

// convergenceRevision summarises the persisted convergence state Gatekeeper
// relied on, so the unchanged path can detect Reviewer progress with a local
// SQLite read instead of re-polling the forge every tick. It is a revision of
// the durable state, not a content hash of the pull request.
func convergenceRevision(evidence ReviewerConvergenceEvidence) string {
	pending := ""
	if evidence.PendingStart {
		pending = "pending"
	}
	return strings.Join([]string{
		pending,
		evidence.UpdatedAt,
		evidence.ReviewedHeadSHA,
		string(evidence.Action),
		fmt.Sprintf("%d", evidence.TotalRounds),
		strings.Join(evidence.OpenItemIDs, ","),
	}, "\x1f")
}

// latestConvergenceRevisions returns the current convergence revision per pull
// request for one project in a single local query, so the unchanged path can
// detect Reviewer progress without re-polling the forge every tick. A pull
// request whose newest reviewer loop can no longer produce a decision is
// absent from the map, matching latestReviewerConvergence's hide-stale rule;
// one whose newest loop is pending start maps to a pending revision so an
// unchanged pending report is reused and the first persisted decision
// invalidates it.
func latestConvergenceRevisions(ctx context.Context, repos *storage.Repositories, projectID string) (map[string]string, error) {
	if repos == nil || repos.Loops == nil {
		return nil, nil
	}
	loops, err := repos.Loops.ListFiltered(ctx, storage.ListLoopsOptions{ProjectID: projectID})
	if err != nil {
		return nil, fmt.Errorf("list convergence revisions: %w", err)
	}
	revisions := make(map[string]string)
	seen := make(map[string]bool)
	// ListFiltered orders by updated_at DESC, seq DESC, so the first reviewer
	// loop for an entity is its newest; a newer loop without valid convergence
	// metadata hides older metadata rather than falling back to it.
	for _, loop := range loops {
		if loop.Type != string(domain.LoopTypeReviewer) || loop.Repo == nil || loop.PRNumber == nil {
			continue
		}
		entityID := fmt.Sprintf("%s#%d", *loop.Repo, *loop.PRNumber)
		if seen[entityID] {
			continue
		}
		seen[entityID] = true
		evidence, ok := reviewerConvergenceFromLoop(loop)
		if !ok {
			if reviewerLoopMayStillConverge(loop.Status) {
				revisions[entityID] = convergenceRevision(ReviewerConvergenceEvidence{PendingStart: true})
			}
			continue
		}
		revisions[entityID] = convergenceRevision(evidence)
	}
	return revisions, nil
}
