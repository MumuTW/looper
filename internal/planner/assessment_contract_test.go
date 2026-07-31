package planner

import (
	"encoding/json"
	"strings"
	"testing"
)

func testAssessmentBinding(t *testing.T) AssessmentBinding {
	t.Helper()
	binding, err := NewAssessmentBinding("acme/looper", 42, "Add endpoint", "Please add it", "0123456789abcdef0123456789abcdef01234567")
	if err != nil {
		t.Fatal(err)
	}
	return binding
}

func assessmentJSON(t *testing.T, binding AssessmentBinding, recommendation AssessmentRecommendation, request string) []byte {
	t.Helper()
	raw, err := json.Marshal(Assessment{SchemaVersion: AssessmentSchemaVersion, Binding: binding, AffectedFiles: []string{"cmd/looper/main.go"}, Surfaces: []AssessmentSurface{AssessmentSurfaceCLI}, AuthorityQuestions: []string{}, Recommendation: recommendation, DecisionRequest: request})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestParseAssessmentRequiresCompleteStrictContract(t *testing.T) {
	binding := testAssessmentBinding(t)
	assessment, err := ParseAssessment(assessmentJSON(t, binding, AssessmentProceed, ""))
	if err != nil {
		t.Fatal(err)
	}
	if assessment.Binding != binding || assessment.Recommendation != AssessmentProceed {
		t.Fatalf("assessment = %#v", assessment)
	}
	for _, raw := range [][]byte{
		[]byte(`{"schemaVersion":"looper.assessment.v1"}`),
		[]byte(`{"schemaVersion":"looper.assessment.v1","binding":{},"affectedFiles":[],"surfaces":[],"authorityQuestions":[],"recommendation":"proceed","decisionRequest":"","extra":true}`),
		append(assessmentJSON(t, binding, AssessmentProceed, ""), []byte(` {}`)...),
	} {
		if _, err := ParseAssessment(raw); err == nil {
			t.Fatalf("ParseAssessment(%s) error = nil", raw)
		}
	}
}

func TestParseAssessmentRejectsDuplicateContractFields(t *testing.T) {
	binding := testAssessmentBinding(t)
	for _, raw := range [][]byte{
		[]byte(`{"schemaVersion":"looper.assessment.v1","schemaVersion":"looper.assessment.v1","binding":{"repo":"acme/looper","issueNumber":42,"issueDigest":"` + binding.IssueDigest + `","baseSha":"` + binding.BaseSHA + `"},"affectedFiles":[],"surfaces":[],"authorityQuestions":[],"recommendation":"proceed","decisionRequest":""}`),
		[]byte(`{"schemaVersion":"looper.assessment.v1","binding":{"repo":"acme/looper","issueNumber":42,"issueDigest":"` + binding.IssueDigest + `","baseSha":"` + binding.BaseSHA + `","baseSha":"ffffffffffffffffffffffffffffffffffffffff"},"affectedFiles":[],"surfaces":[],"authorityQuestions":[],"recommendation":"proceed","decisionRequest":""}`),
	} {
		if _, err := ParseAssessment(raw); err == nil || !strings.Contains(err.Error(), "repeats field") {
			t.Fatalf("ParseAssessment(%s) error = %v, want duplicate-field rejection", raw, err)
		}
	}
}

func TestParseAssessmentRejectsCaseFoldedContractFields(t *testing.T) {
	binding := testAssessmentBinding(t)
	for _, raw := range [][]byte{
		[]byte(`{"schemaVersion":"looper.assessment.v1","binding":{"repo":"acme/looper","issueNumber":42,"issueDigest":"` + binding.IssueDigest + `","baseSha":"` + binding.BaseSHA + `"},"affectedFiles":[],"surfaces":[],"authorityQuestions":[],"recommendation":"escalate","Recommendation":"proceed","decisionRequest":""}`),
		[]byte(`{"schemaVersion":"looper.assessment.v1","binding":{"repo":"acme/looper","issueNumber":42,"issueDigest":"` + binding.IssueDigest + `","baseSha":"` + binding.BaseSHA + `","BaseSha":"ffffffffffffffffffffffffffffffffffffffff"},"affectedFiles":[],"surfaces":[],"authorityQuestions":[],"recommendation":"proceed","decisionRequest":""}`),
		[]byte(`{"schemaVersion":"looper.assessment.v1","Binding":{"repo":"acme/looper","issueNumber":42,"issueDigest":"` + binding.IssueDigest + `","baseSha":"` + binding.BaseSHA + `"},"affectedFiles":[],"surfaces":[],"authorityQuestions":[],"recommendation":"proceed","decisionRequest":""}`),
	} {
		if _, err := ParseAssessment(raw); err == nil || !strings.Contains(err.Error(), "noncanonical") {
			t.Fatalf("ParseAssessment(%s) error = %v, want case-folded field rejection", raw, err)
		}
	}
}

func TestNewAssessmentBindingRequiresFullGitObjectID(t *testing.T) {
	for _, baseSHA := range []string{
		"main",
		"0123456789abcdef",
		"0123456789abcdef0123456789abcdef0123456g",
		"",
	} {
		if _, err := NewAssessmentBinding("acme/looper", 42, "Add endpoint", "Please add it", baseSHA); err == nil {
			t.Fatalf("NewAssessmentBinding(%q) error = nil", baseSHA)
		}
	}
	if _, err := NewAssessmentBinding("acme/looper", 42, "Add endpoint", "Please add it", strings.Repeat("a", 64)); err != nil {
		t.Fatalf("NewAssessmentBinding(SHA-256 object ID) error = %v", err)
	}
}

func TestParseAssessmentRejectsInvalidEvidence(t *testing.T) {
	binding := testAssessmentBinding(t)
	for _, mutate := range []func(*Assessment){
		func(a *Assessment) { a.AffectedFiles = []string{"../escape"} },
		func(a *Assessment) { a.Surfaces = []AssessmentSurface{"invented"} },
		func(a *Assessment) { a.AuthorityQuestions = []string{"same", "same"} },
	} {
		assessment := Assessment{SchemaVersion: AssessmentSchemaVersion, Binding: binding, AffectedFiles: []string{}, Surfaces: []AssessmentSurface{}, AuthorityQuestions: []string{}, Recommendation: AssessmentProceed, DecisionRequest: ""}
		mutate(&assessment)
		raw, err := json.Marshal(assessment)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ParseAssessment(raw); err == nil {
			t.Fatalf("ParseAssessment(%s) error = nil", raw)
		}
	}
}

func TestEvaluateAssessmentUsesCurrentBindingAndDeterministicPolicy(t *testing.T) {
	binding := testAssessmentBinding(t)
	proceed, err := ParseAssessment(assessmentJSON(t, binding, AssessmentProceed, ""))
	if err != nil {
		t.Fatal(err)
	}
	decision, err := EvaluateAssessment(binding, AssessmentPolicy{}, proceed)
	if err != nil || decision.Disposition != AssessmentDispositionProceed {
		t.Fatalf("decision = %#v, err = %v", decision, err)
	}
	escalate, err := ParseAssessment(assessmentJSON(t, binding, AssessmentEscalate, "Choose the CLI compatibility policy."))
	if err != nil {
		t.Fatal(err)
	}
	decision, err = EvaluateAssessment(binding, AssessmentPolicy{EscalateSurfaces: []AssessmentSurface{AssessmentSurfaceCLI}}, escalate)
	if err != nil || decision.Disposition != AssessmentDispositionEscalate || len(decision.Criteria) != 1 || decision.Criteria[0].Name != "surface_cli" {
		t.Fatalf("decision = %#v, err = %v", decision, err)
	}
	fileBound := escalate
	fileBound.AffectedFiles = []string{"cmd/looper/main.go", "internal/planner/runner.go"}
	decision, err = EvaluateAssessment(binding, AssessmentPolicy{MaxAffectedFiles: 1}, fileBound)
	if err != nil || len(decision.Criteria) != 1 || decision.Criteria[0].Name != "affected_files" {
		t.Fatalf("file-bound decision = %#v, err = %v", decision, err)
	}
	authorityBound := escalate
	authorityBound.Surfaces = nil
	authorityBound.AuthorityQuestions = []string{"Choose the backwards-compatibility boundary."}
	decision, err = EvaluateAssessment(binding, AssessmentPolicy{EscalateOnAuthorityQuestion: true}, authorityBound)
	if err != nil || len(decision.Criteria) != 1 || decision.Criteria[0].Name != "authority_question" {
		t.Fatalf("authority-bound decision = %#v, err = %v", decision, err)
	}
	if _, err := EvaluateAssessment(binding, AssessmentPolicy{EscalateSurfaces: []AssessmentSurface{AssessmentSurfaceCLI}}, proceed); err == nil || !strings.Contains(err.Error(), "contradicts") {
		t.Fatalf("proceed with fired rule error = %v", err)
	}
	changed := binding
	changed.BaseSHA = "fedcba9876543210fedcba9876543210fedcba98"
	if _, err := EvaluateAssessment(changed, AssessmentPolicy{}, proceed); err == nil || !strings.Contains(err.Error(), "no longer matches") {
		t.Fatalf("stale binding error = %v", err)
	}
	if _, err := EvaluateAssessment(binding, AssessmentPolicy{}, escalate); err == nil || !strings.Contains(err.Error(), "without a configured criterion") {
		t.Fatalf("unfired escalation error = %v", err)
	}
}
