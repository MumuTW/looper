package workgraphdispatch

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/planner/workgraph"
	"github.com/MumuTW/looper/internal/storage"
)

func TestCreateAndCompleteQueuesOnlyReadyNodesOnce(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t)
	graph := mustGraph(t,
		workgraph.Node{Key: "storage", Goal: "Persist records", AcceptanceCriteria: []string{"migration"}, ExpectedPRScope: "storage"},
		workgraph.Node{Key: "api", Goal: "Expose records", AcceptanceCriteria: []string{"route"}, ExpectedPRScope: "api", Dependencies: []string{"storage"}},
	)
	created, err := fixture.service.Create(fixture.ctx, CreateInput{ProjectID: fixture.projectID, ParentRepo: "acme/looper", ParentIssueNumber: 339, PlannerLoopID: fixture.plannerLoopID, BaseBranch: "main", Graph: graph})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if got, want := created.QueuedNodeKeys, []string{"storage"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Create().QueuedNodeKeys = %v, want %v", got, want)
	}
	nodes, err := fixture.repos.PlannerWorkGraphs.ListNodes(fixture.ctx, created.GraphID)
	if err != nil {
		t.Fatalf("ListNodes() error = %v", err)
	}
	storageNode := nodeByKey(t, nodes, "storage")
	apiNode := nodeByKey(t, nodes, "api")
	if apiNode.State != "pending" || apiNode.BlockedReason == nil || *apiNode.BlockedReason != "blocked by storage" {
		t.Fatalf("api node = %#v, want visible pending blocker", apiNode)
	}
	if claimed, err := fixture.service.ClaimWorkerNode(fixture.ctx, storageNode.WorkerLoopID); err != nil || !claimed {
		t.Fatalf("ClaimWorkerNode() = (%t, %v), want (true, nil)", claimed, err)
	}
	if claimed, err := fixture.service.ClaimWorkerNode(fixture.ctx, storageNode.WorkerLoopID); err != nil || claimed {
		t.Fatalf("duplicate ClaimWorkerNode() = (%t, %v), want (false, nil)", claimed, err)
	}
	if err := fixture.service.ReleaseWorkerNode(fixture.ctx, storageNode.WorkerLoopID); err != nil {
		t.Fatalf("ReleaseWorkerNode() error = %v", err)
	}
	if claimed, err := fixture.service.ClaimWorkerNode(fixture.ctx, storageNode.WorkerLoopID); err != nil || !claimed {
		t.Fatalf("retry ClaimWorkerNode() = (%t, %v), want (true, nil)", claimed, err)
	}
	queued, err := fixture.service.CompleteWorkerNode(fixture.ctx, storageNode.WorkerLoopID)
	if err != nil {
		t.Fatalf("CompleteWorkerNode() error = %v", err)
	}
	if got, want := queued, []string{"api"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("CompleteWorkerNode() queued = %v, want %v", got, want)
	}
	if queued, err := fixture.service.CompleteWorkerNode(fixture.ctx, storageNode.WorkerLoopID); err != nil || len(queued) != 0 {
		t.Fatalf("duplicate CompleteWorkerNode() = (%v, %v), want no duplicate queue", queued, err)
	}
	apiLoop, err := fixture.repos.Loops.GetByID(fixture.ctx, apiNode.WorkerLoopID)
	if err != nil || apiLoop == nil || apiLoop.Status != "queued" {
		t.Fatalf("api loop = %#v, %v, want queued", apiLoop, err)
	}
	var metadata struct {
		Worker struct {
			BaseBranch string `json:"baseBranch"`
		} `json:"worker"`
	}
	if err := json.Unmarshal([]byte(*apiLoop.MetadataJSON), &metadata); err != nil {
		t.Fatalf("decode api worker metadata: %v", err)
	}
	if metadata.Worker.BaseBranch != storageNode.Branch {
		t.Fatalf("api base branch = %q, want prerequisite branch %q", metadata.Worker.BaseBranch, storageNode.Branch)
	}
	allQueueItems, err := fixture.repos.Queue.List(fixture.ctx)
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	if got := countQueueItemsForLoop(allQueueItems, apiNode.WorkerLoopID); got != 1 {
		t.Fatalf("api queue items = %d, want exactly one after duplicate completion", got)
	}
}

func TestCompleteWorkerNodeHonorsTerminatedChildLoop(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t)
	graph := mustGraph(t,
		workgraph.Node{Key: "storage", Goal: "Persist records", AcceptanceCriteria: []string{"migration"}, ExpectedPRScope: "storage"},
		workgraph.Node{Key: "api", Goal: "Expose records", AcceptanceCriteria: []string{"route"}, ExpectedPRScope: "api", Dependencies: []string{"storage"}},
	)
	created, err := fixture.service.Create(fixture.ctx, CreateInput{ProjectID: fixture.projectID, ParentRepo: "acme/looper", ParentIssueNumber: 339, PlannerLoopID: fixture.plannerLoopID, BaseBranch: "main", Graph: graph})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	nodes, _ := fixture.repos.PlannerWorkGraphs.ListNodes(fixture.ctx, created.GraphID)
	storageNode := nodeByKey(t, nodes, "storage")
	apiNode := nodeByKey(t, nodes, "api")
	nowISO := "2026-07-31T08:00:00.000Z"
	apiLoop, err := fixture.repos.Loops.GetByID(fixture.ctx, apiNode.WorkerLoopID)
	if err != nil || apiLoop == nil {
		t.Fatalf("GetByID(api loop) = %#v, %v", apiLoop, err)
	}
	apiLoop.Status = "terminated"
	apiLoop.UpdatedAt = nowISO
	if err := fixture.repos.Loops.Upsert(fixture.ctx, *apiLoop); err != nil {
		t.Fatalf("terminate api loop: %v", err)
	}
	if claimed, err := fixture.service.ClaimWorkerNode(fixture.ctx, storageNode.WorkerLoopID); err != nil || !claimed {
		t.Fatalf("ClaimWorkerNode() = (%t, %v)", claimed, err)
	}
	if _, err := fixture.service.CompleteWorkerNode(fixture.ctx, storageNode.WorkerLoopID); err != nil {
		t.Fatalf("CompleteWorkerNode() error = %v", err)
	}
	nodes, _ = fixture.repos.PlannerWorkGraphs.ListNodes(fixture.ctx, created.GraphID)
	apiNode = nodeByKey(t, nodes, "api")
	if apiNode.State != "closed" {
		t.Fatalf("terminated child node = %#v, want closed without queueing", apiNode)
	}
	queued, err := fixture.repos.Queue.ListQueued(fixture.ctx, 10)
	if err != nil {
		t.Fatalf("ListQueued() error = %v", err)
	}
	for _, item := range queued {
		if item.LoopID != nil && *item.LoopID == apiNode.WorkerLoopID {
			t.Fatalf("terminated child was queued: %#v", item)
		}
	}
}

func TestFailedNodeBlocksChildAndQueuesPlannerReplan(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t)
	graph := mustGraph(t,
		workgraph.Node{Key: "storage", Goal: "Persist records", AcceptanceCriteria: []string{"migration"}, ExpectedPRScope: "storage"},
		workgraph.Node{Key: "api", Goal: "Expose records", AcceptanceCriteria: []string{"route"}, ExpectedPRScope: "api", Dependencies: []string{"storage"}},
	)
	created, err := fixture.service.Create(fixture.ctx, CreateInput{ProjectID: fixture.projectID, ParentRepo: "acme/looper", ParentIssueNumber: 339, PlannerLoopID: fixture.plannerLoopID, BaseBranch: "main", Graph: graph})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	nodes, _ := fixture.repos.PlannerWorkGraphs.ListNodes(fixture.ctx, created.GraphID)
	storageNode := nodeByKey(t, nodes, "storage")
	if claimed, err := fixture.service.ClaimWorkerNode(fixture.ctx, storageNode.WorkerLoopID); err != nil || !claimed {
		t.Fatalf("ClaimWorkerNode() = (%t, %v)", claimed, err)
	}
	replanned, err := fixture.service.FailWorkerNode(fixture.ctx, storageNode.WorkerLoopID, "validation failed")
	if err != nil || !replanned {
		t.Fatalf("FailWorkerNode() = (%t, %v), want replan", replanned, err)
	}
	nodes, _ = fixture.repos.PlannerWorkGraphs.ListNodes(fixture.ctx, created.GraphID)
	if child := nodeByKey(t, nodes, "api"); child.State != "pending" || child.BlockedReason == nil || *child.BlockedReason != "blocked by failed prerequisite storage" {
		t.Fatalf("blocked child = %#v", child)
	}
	graphRecord, _ := fixture.repos.PlannerWorkGraphs.GetByID(fixture.ctx, created.GraphID)
	if graphRecord.Status != "replan_required" || graphRecord.ReplanReason == nil {
		t.Fatalf("graph = %#v, want replan required", graphRecord)
	}
	queued, err := fixture.repos.Queue.ListQueued(fixture.ctx, 10)
	if err != nil {
		t.Fatalf("ListQueued() error = %v", err)
	}
	if len(queued) != 2 || queued[0].Type != "planner" && queued[1].Type != "planner" {
		t.Fatalf("queued = %#v, want root worker plus planner replan", queued)
	}
}

type fixture struct {
	ctx           context.Context
	repos         *storage.Repositories
	service       *Service
	projectID     string
	plannerLoopID string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	ctx := context.Background()
	db, err := storage.OpenSQLiteDB(ctx, filepath.Join(t.TempDir(), "looper.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLiteDB() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := storage.NewMigrationRunner(db, storage.MigrationRunnerOptions{}).RunPending(ctx); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}
	repos := storage.NewRepositories(db)
	now := time.Date(2026, time.July, 31, 8, 0, 0, 0, time.UTC)
	nowISO := "2026-07-31T08:00:00.000Z"
	projectID := "project_1"
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: t.TempDir(), BaseBranch: stringPointer("main"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Upsert project: %v", err)
	}
	seq, err := repos.Loops.AllocateSeq(ctx)
	if err != nil {
		t.Fatalf("AllocateSeq() error = %v", err)
	}
	loopID := "loop_planner_1"
	target := "issue:acme/looper:339"
	repo := "acme/looper"
	if err := repos.Loops.Upsert(ctx, storage.LoopRecord{ID: loopID, Seq: seq, ProjectID: projectID, Type: "planner", TargetType: "issue", TargetID: &target, Repo: &repo, Status: "completed", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Upsert planner loop: %v", err)
	}
	return fixture{ctx: ctx, repos: repos, service: New(Options{DB: db, Repositories: repos, Now: func() time.Time { return now }}), projectID: projectID, plannerLoopID: loopID}
}

func mustGraph(t *testing.T, nodes ...workgraph.Node) workgraph.Graph {
	t.Helper()
	graph, err := workgraph.Build(nodes)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	return graph
}

func nodeByKey(t *testing.T, nodes []storage.PlannerWorkGraphNodeRecord, key string) storage.PlannerWorkGraphNodeRecord {
	t.Helper()
	for _, node := range nodes {
		if node.NodeKey == key {
			return node
		}
	}
	t.Fatalf("node %q not found", key)
	return storage.PlannerWorkGraphNodeRecord{}
}

func stringPointer(value string) *string { return &value }

func countQueueItemsForLoop(items []storage.QueueItemRecord, loopID string) int {
	count := 0
	for _, item := range items {
		if item.LoopID != nil && *item.LoopID == loopID {
			count++
		}
	}
	return count
}
