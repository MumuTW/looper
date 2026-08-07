// Package workgraphdispatch persists Planner decompositions and projects only
// dependency-ready nodes into ordinary Worker loops. It deliberately reuses
// the Worker queue contract instead of creating a second execution lane.
package workgraphdispatch

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/MumuTW/looper/internal/eventlog"
	"github.com/MumuTW/looper/internal/planner/workgraph"
	"github.com/MumuTW/looper/internal/storage"
)

type Options struct {
	DB               *sql.DB
	Repositories     *storage.Repositories
	Now              func() time.Time
	RetryMaxAttempts int64
	OnEnqueued       func()
	// StopLoop drains live sibling executions before graph supersede retires
	// their queue rows. Nil skips in-process stop (unit tests only).
	StopLoop func(ctx context.Context, loopID, reason string) error
}

type Service struct {
	db               *sql.DB
	repos            *storage.Repositories
	now              func() time.Time
	retryMaxAttempts int64
	onEnqueued       func()
	stopLoop         func(ctx context.Context, loopID, reason string) error
}

func New(options Options) *Service {
	now := options.Now
	if now == nil {
		now = time.Now
	}
	retries := options.RetryMaxAttempts
	if retries == 0 {
		retries = 3
	}
	return &Service{db: options.DB, repos: options.Repositories, now: now, retryMaxAttempts: retries, onEnqueued: options.OnEnqueued, stopLoop: options.StopLoop}
}

type CreateInput struct {
	ProjectID         string
	ParentRepo        string
	ParentIssueNumber int64
	PlannerLoopID     string
	BaseBranch        string
	Graph             workgraph.Graph
}

type CreateResult struct {
	GraphID        string
	QueuedNodeKeys []string
	Existing       bool
	Activated      bool
}

// Create persists a validated graph and its worker loops without dispatching
// roots. Call Activate after the owning Planner run reaches its publish
// boundary. A Planner replay returns the graph already bound to its loop;
// graphs marked replan_required are superseded before a replacement is stored.
func (s *Service) Create(ctx context.Context, input CreateInput) (CreateResult, error) {
	if s.db == nil || s.repos == nil {
		return CreateResult{}, fmt.Errorf("work graph storage is not configured")
	}
	if strings.TrimSpace(input.ProjectID) == "" || strings.TrimSpace(input.ParentRepo) == "" || input.ParentIssueNumber <= 0 || strings.TrimSpace(input.PlannerLoopID) == "" || strings.TrimSpace(input.BaseBranch) == "" {
		return CreateResult{}, fmt.Errorf("work graph parent, planner loop, and base branch are required")
	}
	nodes := input.Graph.Nodes()
	if len(nodes) == 0 {
		return CreateResult{}, fmt.Errorf("work graph must contain at least one node")
	}
	result := CreateResult{}
	err := storage.WithTransaction(ctx, s.db, nil, func(tx *sql.Tx) error {
		repos := storage.NewRepositories(tx)
		existing, err := repos.PlannerWorkGraphs.GetByPlannerLoopID(ctx, input.PlannerLoopID)
		if err != nil {
			return err
		}
		if existing != nil {
			if existing.Status != "replan_required" {
				result = CreateResult{GraphID: existing.ID, Existing: true}
				return nil
			}
			if err := s.retireGraph(ctx, repos, existing.ID, s.nowISO()); err != nil {
				return err
			}
		}
		nowISO := s.nowISO()
		graphID := eventlog.NewEventID("workgraph")
		if err := repos.PlannerWorkGraphs.Create(ctx, storage.PlannerWorkGraphRecord{ID: graphID, ProjectID: input.ProjectID, ParentRepo: input.ParentRepo, ParentIssueNumber: input.ParentIssueNumber, PlannerLoopID: input.PlannerLoopID, BaseBranch: input.BaseBranch, Status: "active", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
			return err
		}

		branches := make(map[string]string, len(nodes))
		loopIDs := make(map[string]string, len(nodes))
		for _, node := range nodes {
			loopIDs[node.Key] = eventlog.NewEventID("loop")
			branches[node.Key] = graphBranch(graphID, node.Key)
		}
		for _, node := range nodes {
			baseBranch := input.BaseBranch
			state := "pending"
			loopStatus := "paused"
			var blockedReason *string
			if len(node.Dependencies) == 1 {
				baseBranch = branches[node.Dependencies[0]]
				reason := "blocked by " + node.Dependencies[0]
				blockedReason = &reason
			} else {
				reason := "awaiting planner publish"
				blockedReason = &reason
			}
			criteria, err := json.Marshal(node.AcceptanceCriteria)
			if err != nil {
				return fmt.Errorf("encode node %q acceptance criteria: %w", node.Key, err)
			}
			seq, err := repos.Loops.AllocateSeq(ctx)
			if err != nil {
				return err
			}
			workerPayload := workerPayload(graphID, node, input.ParentRepo, baseBranch, branches[node.Key])
			metadata, err := json.Marshal(map[string]any{"worker": workerPayload})
			if err != nil {
				return fmt.Errorf("encode node %q worker metadata: %w", node.Key, err)
			}
			targetID := "project:" + input.ProjectID
			loop := storage.LoopRecord{ID: loopIDs[node.Key], Seq: seq, ProjectID: input.ProjectID, Type: "worker", TargetType: "project", TargetID: &targetID, Repo: &input.ParentRepo, Status: loopStatus, MetadataJSON: stringPtr(string(metadata)), CreatedAt: nowISO, UpdatedAt: nowISO}
			if err := repos.Loops.Upsert(ctx, loop); err != nil {
				return err
			}
			if err := repos.PlannerWorkGraphs.CreateNode(ctx, storage.PlannerWorkGraphNodeRecord{GraphID: graphID, NodeKey: node.Key, Goal: node.Goal, AcceptanceCriteriaJSON: string(criteria), ExpectedPRScope: node.ExpectedPRScope, WorkerLoopID: loop.ID, Branch: branches[node.Key], State: state, BlockedReason: blockedReason, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
				return err
			}
		}
		// Insert dependency edges only after every referenced node exists. SQLite
		// enforces both composite foreign keys, including edges whose prerequisite
		// sorts after its dependent key.
		for _, node := range nodes {
			for _, dependency := range node.Dependencies {
				if err := repos.PlannerWorkGraphs.CreateDependency(ctx, graphID, node.Key, dependency); err != nil {
					return err
				}
			}
		}
		result.GraphID = graphID
		return nil
	})
	if err != nil {
		return CreateResult{}, err
	}
	if len(result.QueuedNodeKeys) > 0 && s.onEnqueued != nil {
		s.onEnqueued()
	}
	return result, nil
}

// Activate queues dependency-ready root nodes after the owning Planner run has
// reached its publish boundary. Replays are idempotent.
func (s *Service) Activate(ctx context.Context, graphID string) ([]string, error) {
	if s.db == nil || strings.TrimSpace(graphID) == "" {
		return nil, nil
	}
	queued := []string{}
	err := storage.WithTransaction(ctx, s.db, nil, func(tx *sql.Tx) error {
		var err error
		queued, err = s.activateGraph(ctx, storage.NewRepositories(tx), graphID)
		return err
	})
	if err != nil {
		return nil, err
	}
	if len(queued) > 0 && s.onEnqueued != nil {
		s.onEnqueued()
	}
	return queued, nil
}

func (s *Service) activateGraph(ctx context.Context, repos *storage.Repositories, graphID string) ([]string, error) {
	graph, err := repos.PlannerWorkGraphs.GetByID(ctx, graphID)
	if err != nil || graph == nil {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("work graph not found: %s", graphID)
	}
	if graph.Status != "active" {
		return nil, fmt.Errorf("work graph %s is %s and cannot be activated", graphID, graph.Status)
	}
	nodes, err := repos.PlannerWorkGraphs.ListNodes(ctx, graphID)
	if err != nil {
		return nil, err
	}
	queued := []string{}
	for _, node := range nodes {
		if node.State != "pending" {
			continue
		}
		deps, err := repos.PlannerWorkGraphs.ListDependencies(ctx, node.GraphID, node.NodeKey)
		if err != nil {
			return nil, err
		}
		if len(deps) > 0 {
			continue
		}
		if added, err := s.queuePendingNode(ctx, repos, graph, node); err != nil {
			return nil, err
		} else if added {
			queued = append(queued, node.NodeKey)
		}
	}
	return queued, nil
}

func (s *Service) retireGraph(ctx context.Context, repos *storage.Repositories, graphID, nowISO string) error {
	nodes, err := repos.PlannerWorkGraphs.ListNodes(ctx, graphID)
	if err != nil {
		return err
	}
	reason := "work graph superseded"
	for _, node := range nodes {
		loop, err := repos.Loops.GetByID(ctx, node.WorkerLoopID)
		if err != nil {
			return err
		}
		if loop != nil && loop.Status == "running" && s.stopLoop != nil {
			if err := s.stopLoop(ctx, node.WorkerLoopID, reason); err != nil {
				return fmt.Errorf("stop live graph worker %s: %w", node.WorkerLoopID, err)
			}
		}
		if _, err := repos.Queue.CancelByLoop(ctx, node.WorkerLoopID, nowISO, &reason); err != nil {
			return err
		}
		if loop == nil {
			continue
		}
		switch loop.Status {
		case "completed", "terminated":
			continue
		}
		loop.Status = "terminated"
		loop.NextRunAt = nil
		loop.UpdatedAt = nowISO
		if err := repos.Loops.Upsert(ctx, *loop); err != nil {
			return err
		}
	}
	return repos.PlannerWorkGraphs.DeleteByID(ctx, graphID)
}

// ClaimWorkerNode is the graph's second, shared claim fence. Queue claiming
// selects one row atomically; this transition prevents a duplicate or stale
// queue delivery from executing the same graph node again.
func (s *Service) ClaimWorkerNode(ctx context.Context, workerLoopID string) (bool, error) {
	if s.db == nil || strings.TrimSpace(workerLoopID) == "" {
		return false, nil
	}
	claimed := false
	err := storage.WithTransaction(ctx, s.db, nil, func(tx *sql.Tx) error {
		repos := storage.NewRepositories(tx)
		node, err := repos.PlannerWorkGraphs.GetNodeByWorkerLoopID(ctx, workerLoopID)
		if err != nil || node == nil {
			return err
		}
		claimed, err = repos.PlannerWorkGraphs.TransitionNode(ctx, node.GraphID, node.NodeKey, "queued", "running", nil, s.nowISO())
		return err
	})
	return claimed, err
}

// ReclaimWorkerNode allows a recovered queue delivery to resume a graph node
// whose queue was requeued while the node remained running.
func (s *Service) ReclaimWorkerNode(ctx context.Context, workerLoopID string) (bool, error) {
	if s.db == nil || strings.TrimSpace(workerLoopID) == "" {
		return false, nil
	}
	reclaimed := false
	err := storage.WithTransaction(ctx, s.db, nil, func(tx *sql.Tx) error {
		repos := storage.NewRepositories(tx)
		node, err := repos.PlannerWorkGraphs.GetNodeByWorkerLoopID(ctx, workerLoopID)
		if err != nil || node == nil {
			return err
		}
		reclaimed = node.State == "running"
		return nil
	})
	return reclaimed, err
}

// ReleaseWorkerNode makes the same logical node available for a normal queue
// retry after its Worker attempt did not reach a terminal result. It does not
// settle the node, so a later retry still has to pass the shared claim fence.
func (s *Service) ReleaseWorkerNode(ctx context.Context, workerLoopID string) error {
	if s.db == nil || strings.TrimSpace(workerLoopID) == "" {
		return nil
	}
	return storage.WithTransaction(ctx, s.db, nil, func(tx *sql.Tx) error {
		return s.releaseWorkerNode(ctx, storage.NewRepositories(tx), workerLoopID)
	})
}

// ReleaseWorkerNodeInTransaction is the storage callback counterpart of
// ReleaseWorkerNode. Its caller owns the surrounding transaction.
func (s *Service) ReleaseWorkerNodeInTransaction(ctx context.Context, repos *storage.Repositories, workerLoopID string) error {
	if repos == nil || strings.TrimSpace(workerLoopID) == "" {
		return nil
	}
	return s.releaseWorkerNode(ctx, repos, workerLoopID)
}

func (s *Service) releaseWorkerNode(ctx context.Context, repos *storage.Repositories, workerLoopID string) error {
	node, err := repos.PlannerWorkGraphs.GetNodeByWorkerLoopID(ctx, workerLoopID)
	if err != nil || node == nil {
		return err
	}
	_, err = repos.PlannerWorkGraphs.TransitionNode(ctx, node.GraphID, node.NodeKey, "running", "queued", nil, s.nowISO())
	return err
}

// CompleteWorkerNode settles one running node and queues exactly the children
// whose sole prerequisite is now completed. Replaying a completion is a no-op.
func (s *Service) CompleteWorkerNode(ctx context.Context, workerLoopID string) ([]string, error) {
	if s.db == nil || strings.TrimSpace(workerLoopID) == "" {
		return nil, nil
	}
	queued := []string{}
	err := storage.WithTransaction(ctx, s.db, nil, func(tx *sql.Tx) error {
		var err error
		queued, err = s.completeWorkerNode(ctx, storage.NewRepositories(tx), workerLoopID, "")
		return err
	})
	if err != nil {
		return nil, err
	}
	if len(queued) > 0 && s.onEnqueued != nil {
		s.onEnqueued()
	}
	return queued, nil
}

// CompleteWorkerNodeInTransaction is the storage callback counterpart of
// CompleteWorkerNode. Its caller owns the surrounding transaction and must
// wake the scheduler only after that transaction commits. completedBranch, when
// non-empty, records the prerequisite branch children should stack on.
func (s *Service) CompleteWorkerNodeInTransaction(ctx context.Context, repos *storage.Repositories, workerLoopID, completedBranch string) ([]string, error) {
	if repos == nil || strings.TrimSpace(workerLoopID) == "" {
		return nil, nil
	}
	return s.completeWorkerNode(ctx, repos, workerLoopID, completedBranch)
}

func (s *Service) Wake() {
	if s.onEnqueued != nil {
		s.onEnqueued()
	}
}

func (s *Service) completeWorkerNode(ctx context.Context, repos *storage.Repositories, workerLoopID, completedBranch string) ([]string, error) {
	queued := []string{}
	node, err := repos.PlannerWorkGraphs.GetNodeByWorkerLoopID(ctx, workerLoopID)
	if err != nil || node == nil {
		return nil, err
	}
	if completedBranch = strings.TrimSpace(completedBranch); completedBranch != "" && !strings.EqualFold(completedBranch, node.Branch) {
		if err := repos.PlannerWorkGraphs.UpdateNodeBranch(ctx, node.GraphID, node.NodeKey, completedBranch, s.nowISO()); err != nil {
			return nil, err
		}
		node.Branch = completedBranch
	}
	if node.State == "completed" {
		return queued, nil
	}
	changed, err := repos.PlannerWorkGraphs.TransitionNode(ctx, node.GraphID, node.NodeKey, "running", "completed", nil, s.nowISO())
	if err != nil {
		return nil, err
	}
	if !changed {
		return queued, nil
	}
	node.State = "completed"
	graph, err := repos.PlannerWorkGraphs.GetByID(ctx, node.GraphID)
	if err != nil || graph == nil {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("work graph not found: %s", node.GraphID)
	}
	nodes, err := repos.PlannerWorkGraphs.ListNodes(ctx, node.GraphID)
	if err != nil {
		return nil, err
	}
	byKey := make(map[string]storage.PlannerWorkGraphNodeRecord, len(nodes))
	for _, candidate := range nodes {
		byKey[candidate.NodeKey] = candidate
	}
	byKey[node.NodeKey] = *node
	for _, candidate := range nodes {
		if candidate.State != "pending" {
			continue
		}
		deps, err := repos.PlannerWorkGraphs.ListDependencies(ctx, candidate.GraphID, candidate.NodeKey)
		if err != nil {
			return nil, err
		}
		if len(deps) != 1 || deps[0] != node.NodeKey || byKey[deps[0]].State != "completed" {
			continue
		}
		prereq := byKey[deps[0]]
		loop, err := repos.Loops.GetByID(ctx, candidate.WorkerLoopID)
		if err != nil || loop == nil {
			if err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("worker loop not found: %s", candidate.WorkerLoopID)
		}
		nowISO := s.nowISO()
		if loop.Status == "terminated" {
			reason := "worker loop terminated"
			if _, err := repos.PlannerWorkGraphs.TransitionNode(ctx, candidate.GraphID, candidate.NodeKey, "pending", "closed", stringPtr(reason), nowISO); err != nil {
				return nil, err
			}
			continue
		}
		changed, err := repos.PlannerWorkGraphs.TransitionNode(ctx, candidate.GraphID, candidate.NodeKey, "pending", "queued", nil, nowISO)
		if err != nil || !changed {
			if err != nil {
				return nil, err
			}
			continue
		}
		loop.Status, loop.NextRunAt, loop.UpdatedAt = "queued", &nowISO, nowISO
		if err := repos.Loops.Upsert(ctx, *loop); err != nil {
			return nil, err
		}
		payload := parseWorkerPayload(loop.MetadataJSON)
		payload["baseBranch"] = prereq.Branch
		targetID := "project:" + graph.ProjectID
		if err := enqueueNode(ctx, repos, graph.ProjectID, graph.ParentRepo, graph.ID, candidate.NodeKey, candidate.WorkerLoopID, targetID, payload, s.retryMaxAttempts, nowISO); err != nil {
			return nil, err
		}
		queued = append(queued, candidate.NodeKey)
	}
	allCompleted := true
	for _, candidate := range nodes {
		if candidate.NodeKey != node.NodeKey && candidate.State != "completed" {
			allCompleted = false
			break
		}
	}
	if allCompleted {
		return queued, repos.PlannerWorkGraphs.UpdateStatus(ctx, graph.ID, "completed", nil, s.nowISO())
	}
	return queued, nil
}

// FailWorkerNode prevents dependent work from being dispatched and records a
// durable re-plan action on the original Planner loop.
func (s *Service) FailWorkerNode(ctx context.Context, workerLoopID, reason string) (bool, error) {
	if s.db == nil || strings.TrimSpace(workerLoopID) == "" {
		return false, nil
	}
	replanned := false
	err := storage.WithTransaction(ctx, s.db, nil, func(tx *sql.Tx) error {
		var err error
		replanned, err = s.failWorkerNode(ctx, storage.NewRepositories(tx), workerLoopID, reason, true)
		return err
	})
	if err != nil {
		return false, err
	}
	if replanned && s.onEnqueued != nil {
		s.onEnqueued()
	}
	return replanned, nil
}

// FailWorkerNodeInTransaction is the storage callback counterpart of
// FailWorkerNode. Its caller owns the surrounding transaction.
func (s *Service) FailWorkerNodeInTransaction(ctx context.Context, repos *storage.Repositories, workerLoopID, reason string) (bool, error) {
	if repos == nil || strings.TrimSpace(workerLoopID) == "" {
		return false, nil
	}
	return s.failWorkerNode(ctx, repos, workerLoopID, reason, true)
}

// SettleSkippedWorkerNode records a non-success Worker outcome and routes the
// graph out of active state so manual or replan recovery can proceed.
func (s *Service) SettleSkippedWorkerNode(ctx context.Context, workerLoopID, reason string) error {
	if s.db == nil || strings.TrimSpace(workerLoopID) == "" {
		return nil
	}
	replanned := false
	err := storage.WithTransaction(ctx, s.db, nil, func(tx *sql.Tx) error {
		var err error
		replanned, err = s.failWorkerNode(ctx, storage.NewRepositories(tx), workerLoopID, reason, true)
		return err
	})
	if err != nil {
		return err
	}
	if replanned && s.onEnqueued != nil {
		s.onEnqueued()
	}
	return nil
}

func (s *Service) failWorkerNode(ctx context.Context, repos *storage.Repositories, workerLoopID, reason string, replan bool) (bool, error) {
	node, err := repos.PlannerWorkGraphs.GetNodeByWorkerLoopID(ctx, workerLoopID)
	if err != nil || node == nil {
		return false, err
	}
	if node.State == "failed" || node.State == "closed" {
		return false, nil
	}
	changed, err := repos.PlannerWorkGraphs.TransitionNode(ctx, node.GraphID, node.NodeKey, "running", "failed", stringPtr(reason), s.nowISO())
	if err != nil {
		return false, err
	}
	if !changed {
		return false, nil
	}
	graph, err := repos.PlannerWorkGraphs.GetByID(ctx, node.GraphID)
	if err != nil || graph == nil {
		if err != nil {
			return false, err
		}
		return false, fmt.Errorf("work graph not found: %s", node.GraphID)
	}
	blockReason := "blocked by failed prerequisite " + node.NodeKey
	if err := repos.PlannerWorkGraphs.BlockPendingChildrenAfterFailure(ctx, graph.ID, node.NodeKey, blockReason, s.nowISO()); err != nil {
		return false, err
	}
	if !replan {
		return false, nil
	}
	dedupeKey := "planner:workgraph-replan:" + graph.ID
	existing, err := repos.Queue.FindActiveByDedupe(ctx, dedupeKey)
	if err != nil {
		return false, err
	}
	if existing != nil {
		if graph.Status != "replan_required" {
			if err := repos.PlannerWorkGraphs.UpdateStatus(ctx, graph.ID, "replan_required", stringPtr(reason), s.nowISO()); err != nil {
				return false, err
			}
		}
		return false, nil
	}
	if err := repos.PlannerWorkGraphs.UpdateStatus(ctx, graph.ID, "replan_required", stringPtr(reason), s.nowISO()); err != nil {
		return false, err
	}
	plannerLoop, err := repos.Loops.GetByID(ctx, graph.PlannerLoopID)
	if err != nil || plannerLoop == nil {
		if err != nil {
			return false, err
		}
		return false, fmt.Errorf("planner loop not found: %s", graph.PlannerLoopID)
	}
	nowISO := s.nowISO()
	plannerLoop.Status, plannerLoop.NextRunAt, plannerLoop.UpdatedAt = "queued", &nowISO, nowISO
	if err := repos.Loops.Upsert(ctx, *plannerLoop); err != nil {
		return false, err
	}
	payload, _ := json.Marshal(map[string]any{"issueNumber": graph.ParentIssueNumber, "replanReason": reason, "workGraphID": graph.ID, "failedNodeKey": node.NodeKey})
	targetID := fmt.Sprintf("issue:%s:%d", graph.ParentRepo, graph.ParentIssueNumber)
	lockKey := storage.IssueLockKey(graph.ProjectID, graph.ParentRepo, graph.ParentIssueNumber)
	queue := storage.QueueItemRecord{ID: eventlog.NewEventID("queue"), ProjectID: &graph.ProjectID, LoopID: &graph.PlannerLoopID, Type: "planner", TargetType: "issue", TargetID: targetID, Repo: &graph.ParentRepo, DedupeKey: "planner:workgraph-replan:" + graph.ID, Priority: storage.QueuePriorityPlanner, Status: "queued", AvailableAt: nowISO, MaxAttempts: s.retryMaxAttempts, LockKey: &lockKey, PayloadJSON: stringPtr(string(payload)), CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := repos.Queue.Upsert(ctx, queue); err != nil {
		return false, err
	}
	return true, nil
}

func enqueueNode(ctx context.Context, repos *storage.Repositories, projectID, repo, graphID, nodeKey, loopID, targetID string, payload map[string]any, maxAttempts int64, nowISO string) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode graph worker queue payload: %w", err)
	}
	lockKey := "workgraph:" + graphID + ":" + nodeKey
	queue := storage.QueueItemRecord{ID: eventlog.NewEventID("queue"), ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: targetID, Repo: &repo, DedupeKey: "workgraph:" + graphID + ":" + nodeKey, Priority: storage.QueuePriorityWorker, Status: "queued", AvailableAt: nowISO, MaxAttempts: maxAttempts, LockKey: &lockKey, PayloadJSON: stringPtr(string(encoded)), CreatedAt: nowISO, UpdatedAt: nowISO}
	return repos.Queue.Upsert(ctx, queue)
}

func (s *Service) queuePendingNode(ctx context.Context, repos *storage.Repositories, graph *storage.PlannerWorkGraphRecord, node storage.PlannerWorkGraphNodeRecord) (bool, error) {
	if node.State != "pending" {
		return false, nil
	}
	loop, err := repos.Loops.GetByID(ctx, node.WorkerLoopID)
	if err != nil || loop == nil {
		if err != nil {
			return false, err
		}
		return false, fmt.Errorf("worker loop not found: %s", node.WorkerLoopID)
	}
	if loop.Status == "terminated" {
		nowISO := s.nowISO()
		_, err := repos.PlannerWorkGraphs.TransitionNode(ctx, node.GraphID, node.NodeKey, "pending", "closed", stringPtr("worker loop terminated"), nowISO)
		return false, err
	}
	nowISO := s.nowISO()
	changed, err := repos.PlannerWorkGraphs.TransitionNode(ctx, node.GraphID, node.NodeKey, "pending", "queued", nil, nowISO)
	if err != nil || !changed {
		return false, err
	}
	loop.Status, loop.NextRunAt, loop.UpdatedAt = "queued", &nowISO, nowISO
	if err := repos.Loops.Upsert(ctx, *loop); err != nil {
		return false, err
	}
	payload := parseWorkerPayload(loop.MetadataJSON)
	targetID := "project:" + graph.ProjectID
	if err := enqueueNode(ctx, repos, graph.ProjectID, graph.ParentRepo, graph.ID, node.NodeKey, node.WorkerLoopID, targetID, payload, s.retryMaxAttempts, nowISO); err != nil {
		return false, err
	}
	return true, nil
}

func workerPayload(graphID string, node workgraph.Node, repo, baseBranch, branch string) map[string]any {
	criteria := make([]string, 0, len(node.AcceptanceCriteria))
	for _, criterion := range node.AcceptanceCriteria {
		criteria = append(criteria, "- "+criterion)
	}
	prompt := strings.Join([]string{"Goal: " + node.Goal, "Acceptance criteria:", strings.Join(criteria, "\n"), "Expected pull-request scope: " + node.ExpectedPRScope}, "\n")
	return map[string]any{"title": node.Goal, "prompt": prompt, "repo": repo, "baseBranch": baseBranch, "branch": branch, "executionMode": "create-pr", "autoDiscovered": true, "workGraphID": graphID, "workGraphNodeKey": node.Key}
}

func parseWorkerPayload(metadataJSON *string) map[string]any {
	if metadataJSON == nil {
		return map[string]any{}
	}
	var metadata struct {
		Worker map[string]any `json:"worker"`
	}
	if err := json.Unmarshal([]byte(*metadataJSON), &metadata); err != nil || metadata.Worker == nil {
		return map[string]any{}
	}
	return metadata.Worker
}

func graphBranch(graphID, nodeKey string) string { return "looper/graph/" + graphID + "/" + nodeKey }
func (s *Service) nowISO() string                { return eventlog.FormatJavaScriptISOString(s.now()) }
func stringPtr(value string) *string             { return &value }
