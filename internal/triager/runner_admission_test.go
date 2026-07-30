package triager

import (
	"context"
	"testing"

	githubinfra "github.com/MumuTW/looper/internal/infra/github"
	"github.com/MumuTW/looper/internal/labels"
	"github.com/MumuTW/looper/internal/triager/admission"
)

func TestPersonalOwnerAutoAdmissionRoutesWithoutModel(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	fixture.github.detail.Author = "owner"
	fixture.github.detail.AuthorAssociation = "OWNER"
	fixture.runnerPolicy = ProjectPolicy{Admission: admission.Policy{Preset: admission.PresetPersonal, Classify: true}}
	unusedLLM := fixture.llm
	fixture.llm = nil

	result, err := fixture.runner().DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatal(err)
	}
	if unusedLLM.calls != 0 || result.DecisionsAttempted != 0 || result.Routed != 1 {
		t.Fatalf("result=%#v llmCalls=%d", result, unusedLLM.calls)
	}
	report := fixture.singleReport(t)
	if report.Version != 3 || report.Admission.Outcome != admission.OutcomeAuto || report.Admission.AuthorTier != admission.AuthorTierOwner || report.Admission.Visibility != "private" {
		t.Fatalf("report admission = %#v", report.Admission)
	}
	if report.Decision.Classification != "" || len(report.Decision.MissingInformation) != 0 || report.Policy.Action != ActionRoutePlanner {
		t.Fatalf("decision/policy = %#v / %#v", report.Decision, report.Policy)
	}
}

func TestAssessAdmissionClassifiesButCannotAutoRoute(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	fixture.github.detail.AuthorAssociation = "NONE"
	fixture.llm.responses = []string{eligibleDecisionJSON()}
	fixture.runnerPolicy = ProjectPolicy{Admission: admission.Policy{Preset: admission.PresetMaintainedOSS, Classify: true}}

	result, err := fixture.runner().DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatal(err)
	}
	if fixture.llm.calls != 1 || result.DecisionsAttempted != 1 || result.AwaitingConfirmation != 1 || result.Routed != 0 {
		t.Fatalf("result=%#v llmCalls=%d", result, fixture.llm.calls)
	}
	report := fixture.singleReport(t)
	if report.Admission.Outcome != admission.OutcomeAssess || report.Admission.AuthorTier != admission.AuthorTierUnaffiliated || report.Decision.Classification != ClassificationFeature {
		t.Fatalf("report = %#v", report)
	}
	if report.Policy.Action != ActionAwaitHuman || len(report.Policy.Reasons) != 1 || report.Policy.Reasons[0] != "admission_requires_confirmation" {
		t.Fatalf("policy = %#v", report.Policy)
	}
}

func TestConfiguredIgnoreIsDurableAndConsumesNoModelCall(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		authorType string
		labels     []string
	}{
		{name: "bot", authorType: "Bot"},
		{name: "hold", labels: []string{labels.HoldGlobal}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRunnerFixture(t)
			fixture.github.detail.Author = "automation[bot]"
			fixture.github.detail.AuthorType = test.authorType
			fixture.github.detail.Labels = test.labels
			fixture.runnerPolicy = ProjectPolicy{Admission: admission.Policy{Preset: admission.PresetCompany, Classify: true}}

			result, err := fixture.runner().DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
			if err != nil {
				t.Fatal(err)
			}
			if fixture.llm.calls != 0 || result.ReportsPersisted != 1 || result.Retired != 1 || result.Routed != 0 {
				t.Fatalf("result=%#v llmCalls=%d", result, fixture.llm.calls)
			}
			report := fixture.singleReport(t)
			if report.Admission.Outcome != admission.OutcomeIgnore || report.Policy.Action != ActionIgnore {
				t.Fatalf("report = %#v", report)
			}
		})
	}
}

func TestAssessWithoutClassificationWaitsWithoutModelCall(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	fixture.github.detail.AuthorAssociation = "MEMBER"
	fixture.runnerPolicy = ProjectPolicy{Admission: admission.Policy{Preset: admission.PresetCompany, Classify: false}}

	result, err := fixture.runner().DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatal(err)
	}
	if fixture.llm.calls != 0 || result.AwaitingConfirmation != 1 {
		t.Fatalf("result=%#v llmCalls=%d", result, fixture.llm.calls)
	}
	if report := fixture.singleReport(t); report.Admission.Classify || report.Decision.Classification != "" || len(report.Decision.MissingInformation) != 0 {
		t.Fatalf("report = %#v", report)
	}
}

func TestPersistedAdmissionReplaysWithoutSecondModelCall(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	fixture.github.detail.AuthorAssociation = "NONE"
	fixture.llm.responses = []string{eligibleDecisionJSON()}
	fixture.runnerPolicy = ProjectPolicy{Admission: admission.Policy{Preset: admission.PresetPersonal, Classify: true}}
	runner := fixture.runner()
	for range 2 {
		if _, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
			t.Fatal(err)
		}
	}
	if fixture.llm.calls != 1 {
		t.Fatalf("LLM calls = %d, want persisted admission/classification replay", fixture.llm.calls)
	}
}

func TestAdmissionReadsRepositoryVisibilityOncePerProjectTick(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	first := fixture.github.detail
	first.Number, first.AuthorAssociation = 23, "OWNER"
	second := first
	second.Number = 24
	second.Title = "Second issue"
	fixture.github.details = map[int64]githubinfra.IssueDetail{23: first, 24: second}
	fixture.runnerPolicy = ProjectPolicy{Admission: admission.Policy{Preset: admission.PresetPersonal, Classify: true}}

	result, err := fixture.runner().DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Routed != 2 || fixture.github.settingsCalls != 1 {
		t.Fatalf("result=%#v settingsCalls=%d, want two routes and one repo read", result, fixture.github.settingsCalls)
	}
}

func TestLegacyPolicyThresholdsAreConfigurable(t *testing.T) {
	t.Parallel()
	decision := Decision{Classification: ClassificationFeature, Scope: ScopeOutOfScope, Risk: RiskMedium, Confidence: 0.5, MissingInformation: []string{"detail"}, RecommendedNextRole: NextRoleHuman}
	policy := LegacyPolicy{AutoRouteConfidence: 0.5, MaxAutoRouteRisk: RiskMedium}
	if got := validateDecision(decision, policy); got.Action != ActionRoutePlanner {
		t.Fatalf("relaxed policy = %#v", got)
	}
}
