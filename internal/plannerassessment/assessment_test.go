package plannerassessment

import (
	"testing"
)

func TestParseScopeAssessment_Valid(t *testing.T) {
	raw := []byte(`{
		"estimatedFilesTouched": 5,
		"estimatedPackagesTouched": 2,
		"filesEvidence": ["internal/foo.go", "internal/bar.go"],
		"packagesEvidence": ["internal", "pkg"],
		"publicSurfaceChange": false,
		"publicSurfaces": [],
		"publicSurfaceEvidence": "",
		"adrConflict": false,
		"conflictingAdr": "",
		"adrConflictEvidence": "",
		"unauthorizedDecision": false,
		"decisionRequired": "",
		"rationale": "small change"
	}`)
	got, err := ParseScopeAssessment(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.EstimatedFilesTouched != 5 {
		t.Errorf("files touched = %d, want 5", got.EstimatedFilesTouched)
	}
	if got.PublicSurfaceChange {
		t.Errorf("publicSurfaceChange should be false")
	}
}

func TestParseScopeAssessment_MalformedJSON(t *testing.T) {
	raw := []byte(`not json`)
	if _, err := ParseScopeAssessment(raw); err == nil {
		t.Fatal("expected error for malformed JSON, got nil")
	}
}

func TestParseScopeAssessment_ContradictoryFlags(t *testing.T) {
	raw := []byte(`{
		"estimatedFilesTouched": 0,
		"estimatedPackagesTouched": 0,
		"filesEvidence": [],
		"packagesEvidence": [],
		"publicSurfaceChange": false,
		"publicSurfaces": ["public_api"],
		"publicSurfaceEvidence": "foo",
		"adrConflict": false,
		"conflictingAdr": "",
		"adrConflictEvidence": "",
		"unauthorizedDecision": false,
		"decisionRequired": "",
		"rationale": ""
	}`)
	if _, err := ParseScopeAssessment(raw); err == nil {
		t.Fatal("expected error for contradictory flags, got nil")
	}
}

func TestParseScopeAssessment_NegativeBlastRadius(t *testing.T) {
	raw := []byte(`{
		"estimatedFilesTouched": -1,
		"estimatedPackagesTouched": 0,
		"filesEvidence": [],
		"packagesEvidence": [],
		"publicSurfaceChange": false,
		"publicSurfaces": [],
		"publicSurfaceEvidence": "",
		"adrConflict": false,
		"conflictingAdr": "",
		"adrConflictEvidence": "",
		"unauthorizedDecision": false,
		"decisionRequired": "",
		"rationale": ""
	}`)
	if _, err := ParseScopeAssessment(raw); err == nil {
		t.Fatal("expected error for negative blast radius, got nil")
	}
}

func TestParseScopeAssessment_ADRConflict(t *testing.T) {
	raw := []byte(`{
		"estimatedFilesTouched": 0,
		"estimatedPackagesTouched": 0,
		"filesEvidence": [],
		"packagesEvidence": [],
		"publicSurfaceChange": false,
		"publicSurfaces": [],
		"publicSurfaceEvidence": "",
		"adrConflict": true,
		"conflictingAdr": "0001-use-postgres.md",
		"adrConflictEvidence": "Issue proposes MySQL",
		"unauthorizedDecision": false,
		"decisionRequired": "",
		"rationale": "conflicts with recorded decision"
	}`)
	got, err := ParseScopeAssessment(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.ADRConflict || got.ConflictingADR != "0001-use-postgres.md" {
		t.Errorf("adrConflict not parsed correctly: %+v", got)
	}
}

func TestEvaluate_NoCriteriaFires(t *testing.T) {
	policy := DefaultEscalationPolicy()
	assessment := ScopeAssessment{
		EstimatedFilesTouched:    3,
		EstimatedPackagesTouched: 1,
	}
	result := Evaluate(policy, assessment, 1, "owner/repo", "hash123", "sha456")
	if result.Escalated {
		t.Error("expected no escalation")
	}
	if len(result.Criteria) != 0 {
		t.Errorf("expected 0 criteria, got %d", len(result.Criteria))
	}
}

func TestEvaluate_BlastRadiusFires(t *testing.T) {
	policy := DefaultEscalationPolicy()
	assessment := ScopeAssessment{
		EstimatedFilesTouched:    15,
		EstimatedPackagesTouched: 1,
		FilesEvidence:            []string{"a.go", "b.go", "c.go"},
	}
	result := Evaluate(policy, assessment, 2, "owner/repo", "hash123", "sha456")
	if !result.Escalated {
		t.Error("expected escalation")
	}
	if len(result.Criteria) != 1 {
		t.Errorf("expected 1 criterion, got %d", len(result.Criteria))
	}
	if result.Criteria[0].Name != "blast-radius-files" {
		t.Errorf("expected blast-radius-files, got %s", result.Criteria[0].Name)
	}
}

func TestEvaluate_PublicSurfaceFires(t *testing.T) {
	policy := DefaultEscalationPolicy()
	assessment := ScopeAssessment{
		PublicSurfaceChange:   true,
		PublicSurfaces:        []string{SurfacePublicAPI},
		PublicSurfaceEvidence: "adds new exported function",
	}
	result := Evaluate(policy, assessment, 3, "owner/repo", "hash123", "sha456")
	if !result.Escalated {
		t.Error("expected escalation")
	}
	found := false
	for _, c := range result.Criteria {
		if c.Name == "public-surface-change" {
			found = true
		}
	}
	if !found {
		t.Error("expected public-surface-change criterion")
	}
}

func TestEvaluate_DisabledCriterion(t *testing.T) {
	policy := EscalationPolicy{
		MaxFilesTouched:        0, // disabled
		OnPublicSurfaceChange:  false,
		OnADRConflict:          false,
		OnUnauthorizedDecision: false,
	}
	assessment := ScopeAssessment{
		EstimatedFilesTouched: 100, // would fire if enabled
	}
	result := Evaluate(policy, assessment, 4, "owner/repo", "hash123", "sha456")
	if result.Escalated {
		t.Error("expected no escalation with all criteria disabled")
	}
}

func TestEvaluate_RecordBound(t *testing.T) {
	policy := DefaultEscalationPolicy()
	assessment := ScopeAssessment{
		EstimatedFilesTouched: 20,
	}
	result := Evaluate(policy, assessment, 42, "owner/repo", "content-hash", "base-sha")
	if result.Record.IssueNumber != 42 {
		t.Errorf("issue number = %d, want 42", result.Record.IssueNumber)
	}
	if result.Record.Repo != "owner/repo" {
		t.Errorf("repo = %s, want owner/repo", result.Record.Repo)
	}
	if result.Record.IssueContentHash != "content-hash" {
		t.Errorf("content hash = %s, want content-hash", result.Record.IssueContentHash)
	}
	if result.Record.BaseSHA != "base-sha" {
		t.Errorf("base SHA = %s, want base-sha", result.Record.BaseSHA)
	}
}
