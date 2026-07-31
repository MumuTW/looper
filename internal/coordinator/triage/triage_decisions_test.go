package triage

import (
	"context"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/labels"
)

type fixtureLLM struct{ raw string }

func (f fixtureLLM) Complete(context.Context, Request) (string, error) { return f.raw, nil }

func TestDecideValidDisposition(t *testing.T) {
	t.Parallel()
	decision := Decide(context.Background(), fixtureLLM{raw: `{"disposition":"valid","comment":"Looks actionable.","labels":{"kind":["kind/bug"],"area":["area/coordinator"],"complexity":["complexity/m"],"dispatch":["looper:dispatch:plan"]}}`}, Input{Issue: Issue{Title: "Coordinator bug", CreatedAt: time.Now().UTC().Format(time.RFC3339)}, Config: testConfig(), Now: time.Now().UTC()})
	if decision.NoOp {
		t.Fatal("Decide() returned no-op for valid output")
	}
	if got, want := decision.ApplyLabels, []string{"kind/bug", "area/coordinator", "complexity/m", "looper:dispatch:plan", "triaged"}; len(got) != len(want) {
		t.Fatalf("ApplyLabels len = %d, want %d", len(got), len(want))
	}
	if !decision.MarkTriaged {
		t.Fatal("MarkTriaged = false, want true")
	}
}

func TestDecideOutOfScopeDisposition(t *testing.T) {
	t.Parallel()
	decision := Decide(context.Background(), fixtureLLM{raw: `{"disposition":"out-of-scope","comment":"Not aligned.","labels":{}}`}, Input{Issue: Issue{Title: "Unfit", CreatedAt: time.Now().UTC().Format(time.RFC3339)}, Config: testConfig(), Now: time.Now().UTC()})
	if decision.NoOp {
		t.Fatal("Decide() returned no-op for out-of-scope output")
	}
	if len(decision.ApplyLabels) != 2 || decision.ApplyLabels[0] != "wontfix" || decision.ApplyLabels[1] != "triaged" {
		t.Fatalf("ApplyLabels = %v, want [wontfix triaged]", decision.ApplyLabels)
	}
}

func TestDecideUnclearDisposition(t *testing.T) {
	t.Parallel()
	decision := Decide(context.Background(), fixtureLLM{raw: `{"disposition":"unclear","comment":"Need repro steps.","labels":{}}`}, Input{Issue: Issue{Title: "Need info", CreatedAt: time.Now().UTC().Format(time.RFC3339)}, Config: testConfig(), Now: time.Now().UTC()})
	if decision.NoOp {
		t.Fatal("Decide() returned no-op for unclear output")
	}
	if len(decision.ApplyLabels) != 2 || decision.ApplyLabels[0] != "needs-info" || decision.ApplyLabels[1] != "triaged" {
		t.Fatalf("ApplyLabels = %v, want [needs-info triaged]", decision.ApplyLabels)
	}
}

func TestDecideCustomNamespaceDoesNotProjectClassificationByDefault(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	cfg.Namespace = labels.NewNamespace("team.looper:")
	decision := Decide(context.Background(), fixtureLLM{raw: `{"disposition":"valid","comment":"Looks actionable.","labels":{"kind":["kind/bug"],"area":["area/coordinator"],"complexity":["complexity/m"],"dispatch":["team.looper:dispatch:plan"]}}`}, Input{Issue: Issue{Title: "Coordinator bug", CreatedAt: time.Now().UTC().Format(time.RFC3339)}, Config: cfg, Now: time.Now().UTC()})
	if decision.NoOp {
		t.Fatalf("Decide() = %#v, want a valid decision", decision)
	}
	if got, want := decision.ApplyLabels, []string{"team.looper:dispatch:plan", "triaged"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("ApplyLabels = %v, want %v", got, want)
	}
	for _, pattern := range decision.ClearLabelPatterns {
		if pattern == "kind/*" || pattern == "area/*" || pattern == "complexity/*" {
			t.Fatalf("classification cleanup pattern %q leaked into custom namespace decision", pattern)
		}
	}
}

func testConfig() Config {
	return Config{TriagedLabel: "triaged", MaxIssueAgeDays: 7, MaxPerTick: 5, OutOfScopeLabel: "wontfix", UnclearLabel: "needs-info", ReTriageOnAuthorReply: true}
}
