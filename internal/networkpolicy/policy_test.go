package networkpolicy

import (
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/labels"
	"github.com/nexu-io/looper/internal/network/protocol"
)

func TestEvaluateWorkerIgnoresTargetLabelsForLocalOnlyProjects(t *testing.T) {
	t.Parallel()
	decision := EvaluateWorker(ProjectPolicy{Mode: config.NetworkModeOff}, []string{labels.DefaultWorkerReadyTrigger, protocol.TargetLabelForNode("red")}, nil)
	if !decision.Allowed {
		t.Fatalf("decision = %#v, want allowed local-only worker claim", decision)
	}
}

func TestEvaluateWorkerRequiresExactOneMatchingTargetAndCoarseAuthority(t *testing.T) {
	t.Parallel()
	policy := ProjectPolicy{Mode: config.NetworkModeRouted, NodeName: "red", GitHubLogin: "worker", GitHubUserID: 42}
	decision := EvaluateWorker(policy, []string{labels.DefaultWorkerReadyTrigger, protocol.TargetLabelForNode("red")}, []GitHubUser{{Login: "worker", ID: 42}})
	if !decision.Allowed || decision.MatchMode != MatchModeNumeric {
		t.Fatalf("decision = %#v, want allowed numeric worker match", decision)
	}

	blocked := EvaluateWorker(policy, []string{labels.DefaultWorkerReadyTrigger, protocol.TargetLabelForNode("red"), protocol.TargetLabelForNode("blue")}, []GitHubUser{{Login: "worker", ID: 42}})
	if blocked.Allowed || blocked.Reason == "" {
		t.Fatalf("blocked = %#v, want multiple-target failure", blocked)
	}
}

func TestEvaluateWorkerUsesProjectNamespaceReadyLabel(t *testing.T) {
	projectPrefix := "team.looper:"
	cfg := config.Config{Network: config.NetworkConfig{NodeName: "red", GitHubLogin: "worker"}, Projects: []config.ProjectRefConfig{{ID: "project", LabelNamespace: projectPrefix, Network: config.ProjectNetworkConfig{Mode: config.ProjectNetworkModeRouted}}}}
	policy := ProjectPolicyForProject(cfg, "project")
	decision := EvaluateWorker(policy, []string{policy.WorkerReadyLabel, protocol.TargetLabelForNode("red")}, nil)
	if decision.Allowed || !strings.Contains(decision.Reason, "local GitHub identity") {
		t.Fatalf("decision = %#v, want target evaluation after namespaced ready label", decision)
	}
	if strings.Contains(decision.Reason, labels.DefaultWorkerReadyTrigger) {
		t.Fatalf("decision = %#v, unexpectedly required default worker-ready label", decision)
	}
}

func TestEvaluateReviewerFallsBackToLoginWhenNumericIDsUnavailable(t *testing.T) {
	t.Parallel()
	policy := ProjectPolicy{Mode: config.NetworkModeRouted, NodeName: "red", GitHubLogin: "reviewer", GitHubUserID: 42}
	decision := EvaluateReviewer(policy, []string{protocol.TargetLabelForNode("red")}, []GitHubUser{{Login: "reviewer"}})
	if !decision.Allowed || decision.MatchMode != MatchModeLoginFallback {
		t.Fatalf("decision = %#v, want allowed login fallback reviewer match", decision)
	}
}

func TestEvaluateTargetRejectsNonCanonicalTargetPrefix(t *testing.T) {
	t.Parallel()
	policy := ProjectPolicy{Mode: config.NetworkModeRouted, NodeName: "red", GitHubLogin: "worker"}
	decision := EvaluateWorker(policy, []string{labels.DefaultWorkerReadyTrigger, strings.ToUpper(protocol.TargetLabelForNode("red"))}, []GitHubUser{{Login: "worker"}})
	if decision.Allowed || decision.Reason == "" {
		t.Fatalf("decision = %#v, want mixed-case target label rejected", decision)
	}
}
