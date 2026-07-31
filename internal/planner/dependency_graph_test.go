package planner

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParsePlannerDependencyGraphNormalizesAndProjectsReadyNodes(t *testing.T) {
	graph, err := ParsePlannerDependencyGraph([]byte(`{
		"version": 1,
		"nodes": [
			{"key":"child-b","goal":" Implement B ","acceptanceCriteria":[" tests pass ","tests pass"],"dependencies":[" child-a ","child-a"],"pullRequestScope":" b "},
			{"key":"child-a","goal":"Implement A","acceptanceCriteria":["A works"],"dependencies":[],"pullRequestScope":"a"}
		]
	}`))
	if err != nil {
		t.Fatalf("ParsePlannerDependencyGraph() error = %v", err)
	}
	if got := []string{graph.Nodes[0].Key, graph.Nodes[1].Key}; !reflect.DeepEqual(got, []string{"child-a", "child-b"}) {
		t.Fatalf("node order = %v, want [child-a child-b]", got)
	}
	if got := graph.ReadyKeys(); !reflect.DeepEqual(got, []string{"child-a"}) {
		t.Fatalf("ReadyKeys() = %v, want [child-a]", got)
	}
	if got := graph.Nodes[1].AcceptanceCriteria; !reflect.DeepEqual(got, []string{"tests pass"}) {
		t.Fatalf("normalized acceptance criteria = %v, want [tests pass]", got)
	}
}

func TestParsePlannerDependencyGraphRejectsInvalidStructure(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		wantErr string
	}{
		{name: "wrong version", payload: `{"version":2,"nodes":[{"key":"a","goal":"A","acceptanceCriteria":["done"],"pullRequestScope":"a"}]}`, wantErr: "version"},
		{name: "duplicate key", payload: `{"version":1,"nodes":[{"key":"a","goal":"A","acceptanceCriteria":["done"],"pullRequestScope":"a"},{"key":"a","goal":"B","acceptanceCriteria":["done"],"pullRequestScope":"b"}]}`, wantErr: "duplicate"},
		{name: "missing dependency", payload: `{"version":1,"nodes":[{"key":"a","goal":"A","acceptanceCriteria":["done"],"dependencies":["missing"],"pullRequestScope":"a"}]}`, wantErr: "missing node"},
		{name: "cycle", payload: `{"version":1,"nodes":[{"key":"a","goal":"A","acceptanceCriteria":["done"],"dependencies":["b"],"pullRequestScope":"a"},{"key":"b","goal":"B","acceptanceCriteria":["done"],"dependencies":["a"],"pullRequestScope":"b"}]}`, wantErr: "cycle"},
		{name: "unknown field", payload: `{"version":1,"nodes":[{"key":"a","goal":"A","acceptanceCriteria":["done"],"unexpected":true}]}`, wantErr: "unknown field"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParsePlannerDependencyGraph([]byte(tt.payload))
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tt.wantErr)) {
				t.Fatalf("error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadPlannerDependencyGraphIsOptionalAndRejectsSymlink(t *testing.T) {
	worktree := t.TempDir()
	graph, err := loadPlannerDependencyGraph(worktree)
	if err != nil || graph != nil {
		t.Fatalf("missing graph = (%#v, %v), want (nil, nil)", graph, err)
	}
	graphDir := filepath.Join(worktree, ".looper")
	if err := os.Mkdir(graphDir, 0o755); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}
	path := filepath.Join(graphDir, "planner-graph.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"nodes":[{"key":"a","goal":"A","acceptanceCriteria":["done"],"pullRequestScope":"a"}]}`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	graph, err = loadPlannerDependencyGraph(worktree)
	if err != nil || graph == nil {
		t.Fatalf("valid graph = (%#v, %v), want graph", graph, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	target := filepath.Join(worktree, "target.json")
	if err := os.WriteFile(target, []byte(`{"version":1,"nodes":[{"key":"a","goal":"A","acceptanceCriteria":["done"],"pullRequestScope":"a"}]}`), 0o600); err != nil {
		t.Fatalf("WriteFile(target) error = %v", err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if _, err := loadPlannerDependencyGraph(worktree); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("symlink load error = %v, want regular-file rejection", err)
	}
}
