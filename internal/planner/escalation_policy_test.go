package planner

import (
	"strings"
	"testing"
)

func fullEscalationPolicy() EscalationPolicy {
	return EscalationPolicy{
		Enabled:                true,
		MaxFilesTouched:        10,
		MaxPackagesTouched:     3,
		OnPublicSurfaceChange:  true,
		OnADRConflict:          true,
		OnUnauthorizedDecision: true,
	}
}

// benignAssessment trips nothing under fullEscalationPolicy.
func benignAssessment() ScopeAssessment {
	return ScopeAssessment{
		EstimatedFilesTouched:    2,
		EstimatedPackagesTouched: 1,
		FilesEvidence:            []string{"internal/planner/runner.go"},
		PackagesEvidence:         []string{"internal/planner"},
		Rationale:                "single-package change",
	}
}

func firedNames(criteria []FiredCriterion) []string {
	names := make([]string, 0, len(criteria))
	for _, criterion := range criteria {
		names = append(names, criterion.Criterion)
	}
	return names
}

func TestEvaluateEscalationPolicyFiresEachCriterionIndependently(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		mutate  func(*ScopeAssessment)
		want    string
		observe string
	}{
		{
			name:    "files over threshold",
			mutate:  func(a *ScopeAssessment) { a.EstimatedFilesTouched = 11 },
			want:    CriterionBlastRadiusFiles,
			observe: "11 files",
		},
		{
			name:    "packages over threshold",
			mutate:  func(a *ScopeAssessment) { a.EstimatedPackagesTouched = 4 },
			want:    CriterionBlastRadiusPackages,
			observe: "4 packages",
		},
		{
			name: "public surface change",
			mutate: func(a *ScopeAssessment) {
				a.PublicSurfaceChange = true
				a.PublicSurfaces = []string{SurfaceConfigSchema}
				a.PublicSurfaceEvidence = "adds roles.planner.escalation"
			},
			want:    CriterionPublicSurfaceChange,
			observe: SurfaceConfigSchema,
		},
		{
			name: "adr conflict",
			mutate: func(a *ScopeAssessment) {
				a.ADRConflict = true
				a.ConflictingADR = "docs/adr/0012-agent-bindings-are-global.md"
				a.ADRConflictEvidence = "requires per-project agent bindings"
			},
			want:    CriterionADRConflict,
			observe: "docs/adr/0012-agent-bindings-are-global.md",
		},
		{
			name: "unauthorized decision",
			mutate: func(a *ScopeAssessment) {
				a.UnauthorizedDecision = true
				a.DecisionRequired = "name a new Authority for spec approval"
			},
			want:    CriterionUnauthorizedDecision,
			observe: "name a new Authority for spec approval",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			assessment := benignAssessment()
			testCase.mutate(&assessment)
			fired := evaluateEscalationPolicy(fullEscalationPolicy(), assessment)
			if len(fired) != 1 || fired[0].Criterion != testCase.want {
				t.Fatalf("fired = %v, want exactly [%s]", firedNames(fired), testCase.want)
			}
			if fired[0].Observed != testCase.observe {
				t.Fatalf("observed = %q, want %q", fired[0].Observed, testCase.observe)
			}
			if len(fired[0].Evidence) == 0 {
				t.Fatalf("criterion %s fired with no supporting evidence", testCase.want)
			}
		})
	}
}

func TestEvaluateEscalationPolicyDoesNotFireOnBenignAssessment(t *testing.T) {
	t.Parallel()

	if fired := evaluateEscalationPolicy(fullEscalationPolicy(), benignAssessment()); len(fired) != 0 {
		t.Fatalf("fired = %v, want none", firedNames(fired))
	}
	// At the threshold exactly, not above it.
	atThreshold := benignAssessment()
	atThreshold.EstimatedFilesTouched = 10
	atThreshold.EstimatedPackagesTouched = 3
	if fired := evaluateEscalationPolicy(fullEscalationPolicy(), atThreshold); len(fired) != 0 {
		t.Fatalf("fired = %v at threshold, want none", firedNames(fired))
	}
}

func TestEvaluateEscalationPolicyRespectsDisabledCriteria(t *testing.T) {
	t.Parallel()

	assessment := benignAssessment()
	assessment.EstimatedFilesTouched = 500
	assessment.EstimatedPackagesTouched = 500
	assessment.PublicSurfaceChange = true
	assessment.PublicSurfaces = []string{SurfacePublicAPI}
	assessment.ADRConflict = true
	assessment.UnauthorizedDecision = true

	policy := EscalationPolicy{Enabled: true, MaxFilesTouched: 0, MaxPackagesTouched: -1}
	if fired := evaluateEscalationPolicy(policy, assessment); len(fired) != 0 {
		t.Fatalf("fired = %v with every criterion disabled, want none", firedNames(fired))
	}
	if fired := evaluateEscalationPolicy(EscalationPolicy{}, assessment); len(fired) != 0 {
		t.Fatalf("fired = %v with policy disabled, want none", firedNames(fired))
	}
}

func TestEvaluateEscalationPolicyReportsEveryFiredCriterion(t *testing.T) {
	t.Parallel()

	assessment := benignAssessment()
	assessment.EstimatedFilesTouched = 40
	assessment.EstimatedPackagesTouched = 9
	assessment.PublicSurfaceChange = true
	assessment.PublicSurfaces = []string{SurfaceWireFormat, SurfacePublicAPI}
	assessment.PublicSurfaceEvidence = "changes the queue payload"
	assessment.ADRConflict = true
	assessment.ConflictingADR = "docs/adr/0001-coordinator-is-stateless.md"
	assessment.ADRConflictEvidence = "adds coordinator state"
	assessment.UnauthorizedDecision = true
	assessment.DecisionRequired = "choose between two incompatible designs"

	fired := evaluateEscalationPolicy(fullEscalationPolicy(), assessment)
	want := []string{CriterionBlastRadiusFiles, CriterionBlastRadiusPackages, CriterionPublicSurfaceChange, CriterionADRConflict, CriterionUnauthorizedDecision}
	got := firedNames(fired)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("fired = %v, want %v", got, want)
	}
	question := buildEscalationQuestion("acme/looper", 114, fired)
	for _, name := range want {
		if !strings.Contains(question, name) {
			t.Fatalf("question omits fired criterion %s:\n%s", name, question)
		}
	}
	if !strings.Contains(question, "acme/looper#114") {
		t.Fatalf("question omits the issue it is about:\n%s", question)
	}
}

func TestDecodeScopeAssessmentRejectsMalformedAgentOutput(t *testing.T) {
	t.Parallel()

	valid := `{"estimatedFilesTouched":3,"estimatedPackagesTouched":1,"filesEvidence":["a.go"],"packagesEvidence":["internal/a"],"publicSurfaceChange":false,"publicSurfaces":[],"publicSurfaceEvidence":"","adrConflict":false,"conflictingAdr":"","adrConflictEvidence":"","unauthorizedDecision":false,"decisionRequired":"","rationale":"read three files"}`
	assessment, err := decodeScopeAssessment(valid)
	if err != nil {
		t.Fatalf("decodeScopeAssessment(valid) error = %v", err)
	}
	if assessment.EstimatedFilesTouched != 3 || assessment.Rationale != "read three files" {
		t.Fatalf("assessment = %#v", assessment)
	}

	for name, raw := range map[string]string{
		"prose":             "Sure! Here is the assessment.",
		"unknown field":     `{"estimatedFilesTouched":1,"rationale":"x","verdict":"escalate"}`,
		"trailing content":  `{"estimatedFilesTouched":1,"rationale":"x"} extra`,
		"missing rationale": `{"estimatedFilesTouched":1}`,
		"negative estimate": `{"estimatedFilesTouched":-1,"rationale":"x"}`,
		"unknown surface":   `{"publicSurfaceChange":true,"publicSurfaces":["everything"],"rationale":"x"}`,
	} {
		if _, err := decodeScopeAssessment(raw); err == nil {
			t.Fatalf("decodeScopeAssessment(%s) error = nil, want rejection", name)
		}
	}
}

func TestClassifyEscalationAnswerSeparatesAuthorizationFromRejection(t *testing.T) {
	t.Parallel()

	for _, answer := range []string{"stop", "STOP", "no", "reject this", "close — out of scope", "Decline.", EscalationOptionStop} {
		if classifyEscalationAnswer(answer) {
			t.Fatalf("classifyEscalationAnswer(%q) = authorized, want rejected", answer)
		}
	}
	for _, answer := range []string{"proceed", "yes", EscalationOptionProceed, "go ahead but keep it to internal/planner", "ok"} {
		if !classifyEscalationAnswer(answer) {
			t.Fatalf("classifyEscalationAnswer(%q) = rejected, want authorized", answer)
		}
	}
	if classifyEscalationAnswer("   ") {
		t.Fatal("classifyEscalationAnswer(blank) = authorized, want rejected")
	}
}

func TestAppendEscalationAuthorizationCarriesHumanGuidanceIntoSpecPrompt(t *testing.T) {
	t.Parallel()

	base := "write a spec"
	if got := appendEscalationAuthorization(base, nil); got != base {
		t.Fatalf("prompt changed with no escalation: %q", got)
	}
	if got := appendEscalationAuthorization(base, &checkpointScope{HumanAuthorized: false, AuthorizedGuidance: "x"}); got != base {
		t.Fatalf("prompt changed without authorization: %q", got)
	}
	got := appendEscalationAuthorization(base, &checkpointScope{HumanAuthorized: true, AuthorizedGuidance: "keep it to internal/planner"})
	if !strings.Contains(got, "keep it to internal/planner") || !strings.Contains(got, "HUMAN AUTHORIZATION") {
		t.Fatalf("prompt missing human authorization:\n%s", got)
	}
}
