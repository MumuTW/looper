package planner

import (
	"context"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/infra/planedoc"
	"github.com/nexu-io/looper/internal/infra/shell"
	"github.com/nexu-io/looper/internal/storage"
)

func TestRequestProductSpecInThreadMatchesStrictPlaneLinkGate(t *testing.T) {
	var got string
	cfg := config.Config{Projects: []config.ProjectRefConfig{{ID: "project", ProductOwner: &config.ProductOwnerConfig{FeishuOpenID: "ou_product"}}}}
	runner := &Runner{projectRoleConfig: &cfg, postThreadNote: func(_ context.Context, _ string, text string, mentions []string) error {
		got = text
		if len(mentions) != 1 || mentions[0] != "ou_product" {
			t.Fatalf("mentions = %#v", mentions)
		}
		return nil
	}}
	runner.requestProductSpecInThread(context.Background(), stepInput{Project: storage.ProjectRecord{ID: "project"}, Loop: storage.LoopRecord{ID: "loop"}}, "跨页面导出")
	for _, want := range []string{"looper:product-spec", "Links", "非空", "普通评论", "飞书回复", "不会解除"} {
		if !strings.Contains(got, want) {
			t.Fatalf("thread note missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "发在本 thread") || strings.Contains(got, "自动关联") {
		t.Fatalf("thread note promises unsupported association: %s", got)
	}
}

// scriptedGateway builds a planedoc gateway whose plane CLI returns the given
// stdouts in order, and records the invocations.
func scriptedGateway(stdouts ...string) (*planedoc.Gateway, *[][]string) {
	calls := &[][]string{}
	i := 0
	gw := planedoc.New(planedoc.Options{
		APIKey: "k", Workspace: "w", APIBaseURL: "https://plane.x/api/v1",
		Run: func(_ context.Context, o shell.Options) (shell.Result, error) {
			*calls = append(*calls, o.Args)
			out := ""
			if i < len(stdouts) {
				out = stdouts[i]
			}
			i++
			return shell.Result{Stdout: out}, nil
		},
	})
	return gw, calls
}

func gateInput(labels []string) (stepInput, plannerCheckpoint) {
	url := "https://plane.x/w/projects/pp/issues/wi-9"
	cp := plannerCheckpoint{Issue: &checkpointIssue{Repo: "owner/repo", IssueNumber: 9, Title: "登录", URL: url, Labels: labels}}
	in := stepInput{Project: storage.ProjectRecord{ID: "proj-1"}, Checkpoint: cp}
	return in, cp
}

func TestProductSpecGateHoldsFeatureWithoutSpec(t *testing.T) {
	// link list (no product-spec) → then comment create (RequestProductSpec)
	gw, calls := scriptedGateway(`{"results":[]}`, `{"id":"c1"}`)
	r := &Runner{planeDoc: func(string) (*planedoc.Gateway, string, bool) { return gw, "plane-proj-uuid", true }}
	in, cp := gateInput([]string{"kind/feature", "looper:plan"})

	gateErr := r.productSpecGate(context.Background(), in, cp)
	if gateErr == nil || gateErr.kind != FailureManualIntervention {
		t.Fatalf("gate = %v, want a manual-intervention hold", gateErr)
	}
	if len(*calls) != 2 {
		t.Fatalf("calls = %d, want link list + comment create", len(*calls))
	}
	// asked product on the work item
	comment := (*calls)[1]
	if comment[1] != "comment" || comment[2] != "create" {
		t.Fatalf("second call = %v, want comment create", comment)
	}
}

func TestProductSpecGateProceedsWhenSpecPresent(t *testing.T) {
	gw, calls := scriptedGateway(`{"results":[{"id":"l1","title":"looper:product-spec","url":"https://plane.x/w/projects/pp/pages/pg1"}]}`, "# Product Spec")
	r := &Runner{planeDoc: func(string) (*planedoc.Gateway, string, bool) { return gw, "plane-proj-uuid", true }}
	in, cp := gateInput([]string{"kind/feature", "looper:plan"})
	if gateErr := r.productSpecGate(context.Background(), in, cp); gateErr != nil {
		t.Fatalf("gate = %v, want nil (has product spec → proceed)", gateErr)
	}
	if len(*calls) != 2 {
		t.Fatalf("calls = %d, want link list + non-empty Plane page read (no comment)", len(*calls))
	}
}

func TestProductSpecGateSkipsNonFeatureAndNonPlane(t *testing.T) {
	gw, calls := scriptedGateway(`{"results":[]}`)
	// a bug → no gate (bugs don't need a product spec)
	r := &Runner{planeDoc: func(string) (*planedoc.Gateway, string, bool) { return gw, "pp", true }}
	inBug, cpBug := gateInput([]string{"kind/bug", "looper:plan"})
	if gateErr := r.productSpecGate(context.Background(), inBug, cpBug); gateErr != nil {
		t.Fatalf("bug gate = %v, want nil", gateErr)
	}
	if len(*calls) != 0 {
		t.Fatalf("bug made %d plane calls, want 0", len(*calls))
	}
	// a github project (planeDoc → false) → no gate
	rGH := &Runner{planeDoc: func(string) (*planedoc.Gateway, string, bool) { return nil, "", false }}
	inF, cpF := gateInput([]string{"kind/feature"})
	if gateErr := rGH.productSpecGate(context.Background(), inF, cpF); gateErr != nil {
		t.Fatalf("github gate = %v, want nil", gateErr)
	}
	// no planeDoc resolver at all → no gate
	rNil := &Runner{}
	if gateErr := rNil.productSpecGate(context.Background(), inF, cpF); gateErr != nil {
		t.Fatalf("nil resolver gate = %v, want nil", gateErr)
	}
}
