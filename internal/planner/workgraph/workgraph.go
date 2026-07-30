// Package workgraph validates and evaluates Planner-produced implementation
// graphs. It contains no forge, queue, or storage access: the structured graph
// is the authority for dependency order, while callers own persistence and
// delivery.
package workgraph

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

var keyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// Node is one independently implementable child item emitted by Planner.
// Key is stable within a graph revision and dependencies name other keys in
// that revision.
type Node struct {
	Key                string
	Goal               string
	AcceptanceCriteria []string
	Dependencies       []string
	ExpectedPRScope    string
}

type State string

const (
	StatePending   State = "pending"
	StateRunning   State = "running"
	StateCompleted State = "completed"
	StateFailed    State = "failed"
	StateClosed    State = "closed"
	StateInvalid   State = "invalid"
)

// Blocked describes why a pending node must not be queued. Replan is true
// only when a prerequisite reached a terminal non-success state.
type Blocked struct {
	NodeKey   string
	BlockedBy []string
	Replan    bool
}

type Graph struct {
	nodes map[string]Node
	keys  []string
}

// Build validates a complete Planner graph. It rejects malformed node keys,
// missing required planning content, duplicate or unknown dependencies, and
// cycles before a caller can persist or queue anything.
func Build(nodes []Node) (Graph, error) {
	if len(nodes) == 0 {
		return Graph{}, fmt.Errorf("work graph must contain at least one node")
	}
	graph := Graph{nodes: make(map[string]Node, len(nodes))}
	for _, raw := range nodes {
		node := normalizeNode(raw)
		if !keyPattern.MatchString(node.Key) {
			return Graph{}, fmt.Errorf("invalid node key %q", raw.Key)
		}
		if node.Goal == "" {
			return Graph{}, fmt.Errorf("node %q goal is required", node.Key)
		}
		if len(node.AcceptanceCriteria) == 0 {
			return Graph{}, fmt.Errorf("node %q acceptance criteria are required", node.Key)
		}
		if node.ExpectedPRScope == "" {
			return Graph{}, fmt.Errorf("node %q expected pull-request scope is required", node.Key)
		}
		if _, exists := graph.nodes[node.Key]; exists {
			return Graph{}, fmt.Errorf("duplicate node key %q", node.Key)
		}
		graph.nodes[node.Key] = node
		graph.keys = append(graph.keys, node.Key)
	}
	sort.Strings(graph.keys)
	for _, key := range graph.keys {
		for _, dependency := range graph.nodes[key].Dependencies {
			if dependency == key {
				return Graph{}, fmt.Errorf("node %q cannot depend on itself", key)
			}
			if _, exists := graph.nodes[dependency]; !exists {
				return Graph{}, fmt.Errorf("node %q depends on missing node %q", key, dependency)
			}
		}
	}
	if cycle := graph.cycle(); len(cycle) > 0 {
		return Graph{}, fmt.Errorf("work graph contains dependency cycle: %s", strings.Join(cycle, " -> "))
	}
	return graph, nil
}

// Nodes returns a detached, stable-key-ordered graph snapshot.
func (g Graph) Nodes() []Node {
	result := make([]Node, 0, len(g.keys))
	for _, key := range g.keys {
		result = append(result, cloneNode(g.nodes[key]))
	}
	return result
}

// Evaluate returns pending nodes whose dependencies are all completed and the
// remaining pending nodes with their durable blockers. A failed, closed, or
// invalid dependency requests re-planning instead of later automatic queueing.
func (g Graph) Evaluate(states map[string]State) (ready []Node, blocked []Blocked) {
	for _, key := range g.keys {
		if stateFor(states, key) != StatePending {
			continue
		}
		node := g.nodes[key]
		block := Blocked{NodeKey: key}
		for _, dependency := range node.Dependencies {
			state := stateFor(states, dependency)
			if state == StateCompleted {
				continue
			}
			block.BlockedBy = append(block.BlockedBy, dependency)
			if state == StateFailed || state == StateClosed || state == StateInvalid {
				block.Replan = true
			}
		}
		if len(block.BlockedBy) == 0 {
			ready = append(ready, cloneNode(node))
			continue
		}
		blocked = append(blocked, block)
	}
	return ready, blocked
}

func normalizeNode(node Node) Node {
	node.Key = strings.TrimSpace(node.Key)
	node.Goal = strings.TrimSpace(node.Goal)
	node.ExpectedPRScope = strings.TrimSpace(node.ExpectedPRScope)
	criteria := make([]string, 0, len(node.AcceptanceCriteria))
	for _, criterion := range node.AcceptanceCriteria {
		if criterion = strings.TrimSpace(criterion); criterion != "" {
			criteria = append(criteria, criterion)
		}
	}
	node.AcceptanceCriteria = criteria
	dependencies := make([]string, 0, len(node.Dependencies))
	seen := map[string]struct{}{}
	for _, dependency := range node.Dependencies {
		dependency = strings.TrimSpace(dependency)
		if dependency == "" {
			continue
		}
		if _, duplicate := seen[dependency]; duplicate {
			continue
		}
		seen[dependency] = struct{}{}
		dependencies = append(dependencies, dependency)
	}
	sort.Strings(dependencies)
	node.Dependencies = dependencies
	return node
}

func (g Graph) cycle() []string {
	state := make(map[string]int, len(g.keys))
	stack := make([]string, 0, len(g.keys))
	index := map[string]int{}
	var found []string
	var visit func(string)
	visit = func(key string) {
		if len(found) > 0 {
			return
		}
		state[key] = 1
		index[key] = len(stack)
		stack = append(stack, key)
		for _, next := range g.nodes[key].Dependencies {
			if start, active := index[next]; active {
				found = append(append([]string(nil), stack[start:]...), next)
				return
			}
			if state[next] == 0 {
				visit(next)
			}
		}
		stack = stack[:len(stack)-1]
		delete(index, key)
		state[key] = 2
	}
	for _, key := range g.keys {
		if state[key] == 0 {
			visit(key)
		}
	}
	return found
}

func stateFor(states map[string]State, key string) State {
	if state := states[key]; state != "" {
		return state
	}
	return StatePending
}

func cloneNode(node Node) Node {
	node.AcceptanceCriteria = append([]string(nil), node.AcceptanceCriteria...)
	node.Dependencies = append([]string(nil), node.Dependencies...)
	return node
}
