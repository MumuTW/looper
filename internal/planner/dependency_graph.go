package planner

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	plannerDependencyGraphRelPath  = ".looper/planner-graph.json"
	maxPlannerDependencyGraphBytes = 64 << 10
	plannerDependencyGraphVersion  = 1
)

// PlannerDependencyGraph is an optional, durable proposal emitted by Planner
// for issues whose implementation can be decomposed into child work items.
// The file is committed with the planning spec so it is inspectable and
// replayable; Coordinator must still project accepted dependencies to the
// GitHub-native blocked_by authority before dispatching work.
type PlannerDependencyGraph struct {
	Version int                     `json:"version"`
	Nodes   []PlannerDependencyNode `json:"nodes"`
}

// PlannerDependencyNode is one proposed child implementation unit. Key is the
// stable identity used by dependency edges; it is deliberately not a loop ID,
// because loops are created only after a graph passes validation and is
// projected into the dispatch authority.
type PlannerDependencyNode struct {
	Key                string   `json:"key"`
	Goal               string   `json:"goal"`
	AcceptanceCriteria []string `json:"acceptanceCriteria"`
	Dependencies       []string `json:"dependencies"`
	PullRequestScope   string   `json:"pullRequestScope"`
}

// ParsePlannerDependencyGraph parses and validates a Planner-authored graph.
// Unknown JSON fields are rejected so a typo cannot silently weaken the graph
// contract used by a later projection step.
func ParsePlannerDependencyGraph(data []byte) (PlannerDependencyGraph, error) {
	var graph PlannerDependencyGraph
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&graph); err != nil {
		return PlannerDependencyGraph{}, fmt.Errorf("decode planner dependency graph: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return PlannerDependencyGraph{}, errors.New("planner dependency graph must contain one JSON value")
	} else if !errors.Is(err, io.EOF) {
		return PlannerDependencyGraph{}, fmt.Errorf("decode planner dependency graph tail: %w", err)
	}
	if err := graph.Validate(); err != nil {
		return PlannerDependencyGraph{}, err
	}
	return graph, nil
}

// Validate checks the graph shape before any queue or remote projection can
// observe it. It rejects duplicate/missing keys and cycles, and normalizes
// whitespace/order so replay produces the same ready set.
func (g *PlannerDependencyGraph) Validate() error {
	if g == nil {
		return errors.New("planner dependency graph is nil")
	}
	if g.Version != plannerDependencyGraphVersion {
		return fmt.Errorf("planner dependency graph version = %d, want %d", g.Version, plannerDependencyGraphVersion)
	}
	if len(g.Nodes) == 0 {
		return errors.New("planner dependency graph must contain at least one node")
	}
	keys := make(map[string]struct{}, len(g.Nodes))
	for index := range g.Nodes {
		node := &g.Nodes[index]
		node.Key = strings.TrimSpace(node.Key)
		node.Goal = strings.TrimSpace(node.Goal)
		node.PullRequestScope = strings.TrimSpace(node.PullRequestScope)
		if node.Key == "" {
			return fmt.Errorf("planner dependency graph node %d has empty key", index)
		}
		if node.Goal == "" {
			return fmt.Errorf("planner dependency graph node %q has empty goal", node.Key)
		}
		if node.PullRequestScope == "" {
			return fmt.Errorf("planner dependency graph node %q has empty pull request scope", node.Key)
		}
		if _, exists := keys[node.Key]; exists {
			return fmt.Errorf("planner dependency graph has duplicate node key %q", node.Key)
		}
		keys[node.Key] = struct{}{}
		node.AcceptanceCriteria = normalizeNonEmptyStrings(node.AcceptanceCriteria)
		if len(node.AcceptanceCriteria) == 0 {
			return fmt.Errorf("planner dependency graph node %q has no acceptance criteria", node.Key)
		}
		node.Dependencies = normalizeNonEmptyStrings(node.Dependencies)
	}
	for index := range g.Nodes {
		node := &g.Nodes[index]
		for _, dependency := range node.Dependencies {
			if dependency == node.Key {
				return fmt.Errorf("planner dependency graph node %q depends on itself", node.Key)
			}
			if _, exists := keys[dependency]; !exists {
				return fmt.Errorf("planner dependency graph node %q depends on missing node %q", node.Key, dependency)
			}
		}
	}
	if cycle := dependencyCycle(g.Nodes); len(cycle) > 0 {
		return fmt.Errorf("planner dependency graph contains cycle: %s", strings.Join(cycle, " -> "))
	}
	sort.SliceStable(g.Nodes, func(i, j int) bool { return g.Nodes[i].Key < g.Nodes[j].Key })
	return nil
}

// ReadyKeys returns nodes with no dependencies. It is a pure projection of the
// validated proposal; it does not authorize queue publication or mutate state.
func (g PlannerDependencyGraph) ReadyKeys() []string {
	keys := make([]string, 0)
	for _, node := range g.Nodes {
		if len(node.Dependencies) == 0 {
			keys = append(keys, node.Key)
		}
	}
	return keys
}

// loadPlannerDependencyGraph treats the graph as optional for backwards
// compatibility with one-step Planner work. A present graph must be a regular
// file within the Planner worktree and must pass the full structural contract.
func loadPlannerDependencyGraph(worktreePath string) (*PlannerDependencyGraph, error) {
	path := filepath.Join(worktreePath, plannerDependencyGraphRelPath)
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("inspect planner dependency graph: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("planner dependency graph must be a regular file")
	}
	if info.Size() > maxPlannerDependencyGraphBytes {
		return nil, fmt.Errorf("planner dependency graph exceeds %d bytes", maxPlannerDependencyGraphBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read planner dependency graph: %w", err)
	}
	graph, err := ParsePlannerDependencyGraph(data)
	if err != nil {
		return nil, err
	}
	return &graph, nil
}

func normalizeNonEmptyStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	normalized := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		normalized = append(normalized, value)
	}
	sort.Strings(normalized)
	return normalized
}

func dependencyCycle(nodes []PlannerDependencyNode) []string {
	edges := make(map[string][]string, len(nodes))
	for _, node := range nodes {
		edges[node.Key] = append([]string(nil), node.Dependencies...)
	}
	state := make(map[string]int, len(nodes))
	stack := make([]string, 0, len(nodes))
	stackIndex := make(map[string]int, len(nodes))
	var visit func(string) []string
	visit = func(key string) []string {
		state[key] = 1
		stackIndex[key] = len(stack)
		stack = append(stack, key)
		for _, dependency := range edges[key] {
			if index, exists := stackIndex[dependency]; exists {
				return append(append([]string(nil), stack[index:]...), dependency)
			}
			if state[dependency] == 0 {
				if cycle := visit(dependency); len(cycle) > 0 {
					return cycle
				}
			}
		}
		stack = stack[:len(stack)-1]
		delete(stackIndex, key)
		state[key] = 2
		return nil
	}
	keys := make([]string, 0, len(nodes))
	for _, node := range nodes {
		keys = append(keys, node.Key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if state[key] == 0 {
			if cycle := visit(key); len(cycle) > 0 {
				return cycle
			}
		}
	}
	return nil
}
