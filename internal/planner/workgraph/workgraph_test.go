package workgraph

import (
	"reflect"
	"strings"
	"testing"
)

func TestBuildRejectsMissingNodesAndCycles(t *testing.T) {
	t.Parallel()
	base := func(key string, dependencies ...string) Node {
		return Node{Key: key, Goal: key + " goal", AcceptanceCriteria: []string{"works"}, ExpectedPRScope: "one package", Dependencies: dependencies}
	}
	for _, tc := range []struct {
		name  string
		nodes []Node
		want  string
	}{
		{name: "missing dependency", nodes: []Node{base("api", "storage")}, want: "missing node"},
		{name: "cycle", nodes: []Node{base("api", "storage"), base("storage", "api")}, want: "cycle"},
		{name: "duplicate", nodes: []Node{base("api"), base("api")}, want: "duplicate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Build(tc.nodes)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Build() error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestBuildRejectsInvalidGitRefComponents(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"api.", "api..db"} {
		_, err := Build([]Node{{Key: key, Goal: "API", AcceptanceCriteria: []string{"endpoint"}, ExpectedPRScope: "api"}})
		if err == nil || !strings.Contains(err.Error(), "git ref component") {
			t.Fatalf("Build(%q) error = %v, want invalid git ref component", key, err)
		}
	}
}

func TestBuildRejectsMultipleDependencies(t *testing.T) {
	t.Parallel()
	_, err := Build([]Node{
		{Key: "api", Goal: "API", AcceptanceCriteria: []string{"endpoint"}, ExpectedPRScope: "api"},
		{Key: "storage", Goal: "Storage", AcceptanceCriteria: []string{"table"}, ExpectedPRScope: "storage"},
		{Key: "worker", Goal: "Worker", AcceptanceCriteria: []string{"handoff"}, ExpectedPRScope: "worker", Dependencies: []string{"api", "storage"}},
	})
	if err == nil || !strings.Contains(err.Error(), "multiple dependencies") {
		t.Fatalf("Build() error = %v, want multiple dependencies rejection", err)
	}
}

func TestEvaluateQueuesOnlyDependencyFreeNodes(t *testing.T) {
	t.Parallel()
	graph, err := Build([]Node{
		{Key: "api", Goal: "API", AcceptanceCriteria: []string{"endpoint"}, ExpectedPRScope: "api"},
		{Key: "storage", Goal: "Storage", AcceptanceCriteria: []string{"table"}, ExpectedPRScope: "storage"},
		{Key: "worker", Goal: "Worker", AcceptanceCriteria: []string{"handoff"}, ExpectedPRScope: "worker", Dependencies: []string{"storage"}},
	})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	ready, blocked := graph.Evaluate(map[string]State{"api": StateCompleted})
	if got, want := nodeKeys(ready), []string{"storage"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ready = %v, want %v", got, want)
	}
	if got, want := blocked, []Blocked{{NodeKey: "worker", BlockedBy: []string{"storage"}}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("blocked = %#v, want %#v", got, want)
	}

	ready, blocked = graph.Evaluate(map[string]State{"api": StateCompleted, "storage": StateCompleted})
	if got, want := nodeKeys(ready), []string{"worker"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ready after prerequisites = %v, want %v", got, want)
	}
	if len(blocked) != 0 {
		t.Fatalf("blocked after prerequisites = %#v, want none", blocked)
	}
}

func TestEvaluateRequestsReplanAfterTerminalDependencyFailure(t *testing.T) {
	t.Parallel()
	graph, err := Build([]Node{
		{Key: "root", Goal: "Root", AcceptanceCriteria: []string{"done"}, ExpectedPRScope: "root"},
		{Key: "child", Goal: "Child", AcceptanceCriteria: []string{"done"}, ExpectedPRScope: "child", Dependencies: []string{"root"}},
	})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	ready, blocked := graph.Evaluate(map[string]State{"root": StateFailed})
	if len(ready) != 0 {
		t.Fatalf("ready = %#v, want none", ready)
	}
	if got, want := blocked, []Blocked{{NodeKey: "child", BlockedBy: []string{"root"}, Replan: true}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("blocked = %#v, want %#v", got, want)
	}
}

func nodeKeys(nodes []Node) []string {
	keys := make([]string, len(nodes))
	for index, node := range nodes {
		keys[index] = node.Key
	}
	return keys
}
