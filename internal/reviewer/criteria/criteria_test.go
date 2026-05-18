package criteria

import "testing"

func TestExtract(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		issueBody string
		want      []AcceptanceCriterion
	}{
		{
			name:      "standard acceptance criteria section",
			issueBody: "## Acceptance criteria\n- [ ] first criterion\n- [x] second criterion\n\n## Notes\nignored",
			want:      []AcceptanceCriterion{"first criterion", "second criterion"},
		},
		{
			name:      "acceptance criteria heading allows trailing punctuation",
			issueBody: "## Acceptance Criteria:\n- [ ] first criterion\n\n## Notes\nignored",
			want:      []AcceptanceCriterion{"first criterion"},
		},
		{
			name:      "acceptance criteria heading allows nested heading levels",
			issueBody: "### Acceptance Criteria\n- [ ] first criterion\n\n### Notes\nignored",
			want:      []AcceptanceCriterion{"first criterion"},
		},
		{
			name:      "acceptance criteria heading allows closing atx markers",
			issueBody: "## Acceptance Criteria ##\n- [ ] first criterion\n\n## Notes\nignored",
			want:      []AcceptanceCriterion{"first criterion"},
		},
		{
			name:      "nested headings inside acceptance criteria do not end parsing",
			issueBody: "## Acceptance Criteria\n- [ ] api returns ok\n\n### Backend\n- [ ] background worker retries\n\n## Notes\nignored",
			want:      []AcceptanceCriterion{"api returns ok", "background worker retries"},
		},
		{
			name:      "missing section returns empty list",
			issueBody: "## Summary\n- [ ] not here",
			want:      []AcceptanceCriterion{},
		},
		{
			name:      "malformed lines are tolerated",
			issueBody: "## Acceptance criteria\nPlease convert this list before merging.\n- [] missing space\n* [x] valid\n- criterion without checkbox",
			want:      []AcceptanceCriterion{"missing space", "valid", "criterion without checkbox"},
		},
		{
			name:      "preserves leading markdown links in criteria",
			issueBody: "## Acceptance criteria\n- [ ] [Spec](https://example.com) is updated",
			want:      []AcceptanceCriterion{"[Spec](https://example.com) is updated"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Extract(tt.issueBody)
			if len(got) != len(tt.want) {
				t.Fatalf("len(Extract()) = %d, want %d", len(got), len(tt.want))
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("Extract()[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestVerify(t *testing.T) {
	t.Parallel()

	diff := PRDiff{Files: []DiffFile{{
		Path:  "internal/reviewer/automerge/decision.go",
		Patch: "@@ -10,2 +10,3 @@\n context\n-old decision\n+new decision\n+follow up\n",
	}}}
	passEvidence := []Evidence{{FilePath: "internal/reviewer/automerge/decision.go", StartLine: 10, EndLine: 12}}

	tests := []struct {
		name         string
		criteria     []AcceptanceCriterion
		responses    map[AcceptanceCriterion]CriterionAssessment
		wantVerdicts []Verdict
		wantOverall  AggregateDisposition
	}{
		{
			name:         "all pass",
			criteria:     []AcceptanceCriterion{"criterion 1", "criterion 2"},
			wantVerdicts: []Verdict{VerdictPass, VerdictPass},
			wantOverall:  DispositionPass,
			responses: map[AcceptanceCriterion]CriterionAssessment{
				"criterion 1": {Verdict: VerdictPass, Justification: "matched diff", Evidence: passEvidence},
				"criterion 2": {Verdict: VerdictPass, Justification: "matched diff", Evidence: passEvidence},
			},
		},
		{
			name:         "missing evidence fails",
			criteria:     []AcceptanceCriterion{"criterion 1", "criterion 2"},
			wantVerdicts: []Verdict{VerdictPass, VerdictFail},
			wantOverall:  DispositionFail,
			responses: map[AcceptanceCriterion]CriterionAssessment{
				"criterion 1": {Verdict: VerdictPass, Justification: "matched diff", Evidence: passEvidence},
				"criterion 2": {Verdict: VerdictFail, Justification: "no evidence in diff"},
			},
		},
		{
			name:         "unverifiable distinguished from fail",
			criteria:     []AcceptanceCriterion{"criterion 1", "criterion 2"},
			wantVerdicts: []Verdict{VerdictPass, VerdictUnverifiable},
			wantOverall:  DispositionUnverifiable,
			responses: map[AcceptanceCriterion]CriterionAssessment{
				"criterion 1": {Verdict: VerdictPass, Justification: "matched diff", Evidence: passEvidence},
				"criterion 2": {Verdict: VerdictUnverifiable, Justification: "non-functional criterion"},
			},
		},
		{
			name:         "fail dominates unverifiable",
			criteria:     []AcceptanceCriterion{"criterion 1", "criterion 2", "criterion 3"},
			wantVerdicts: []Verdict{VerdictPass, VerdictUnverifiable, VerdictFail},
			wantOverall:  DispositionFail,
			responses: map[AcceptanceCriterion]CriterionAssessment{
				"criterion 1": {Verdict: VerdictPass, Justification: "matched diff", Evidence: passEvidence},
				"criterion 2": {Verdict: VerdictUnverifiable, Justification: "non-functional criterion"},
				"criterion 3": {Verdict: VerdictFail, Justification: "contradicted by diff"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result, err := Verify(tt.criteria, diff, fixtureVerifier{responses: tt.responses})
			if err != nil {
				t.Fatalf("Verify() error = %v", err)
			}
			if result.Disposition != tt.wantOverall {
				t.Fatalf("Verify().Disposition = %q, want %q", result.Disposition, tt.wantOverall)
			}
			for i, want := range tt.wantVerdicts {
				if result.Criteria[i].Verdict != want {
					t.Fatalf("Verify().Criteria[%d].Verdict = %q, want %q", i, result.Criteria[i].Verdict, want)
				}
			}
		})
	}
}

func TestVerifyRejectsInvalidVerifierOutput(t *testing.T) {
	t.Parallel()

	_, err := Verify([]AcceptanceCriterion{"criterion 1"}, PRDiff{}, fixtureVerifier{responses: map[AcceptanceCriterion]CriterionAssessment{
		"criterion 1": {Verdict: VerdictPass, Justification: "matched diff"},
	}})
	if err == nil || err.Error() != `criterion "criterion 1" returned pass without evidence` {
		t.Fatalf("Verify() error = %v, want pass-without-evidence failure", err)
	}

	_, err = Verify([]AcceptanceCriterion{"criterion 2"}, PRDiff{Files: []DiffFile{{Path: "changed.go", Patch: "@@"}}}, fixtureVerifier{responses: map[AcceptanceCriterion]CriterionAssessment{
		"criterion 2": {Verdict: VerdictPass, Justification: "matched diff", Evidence: []Evidence{{FilePath: "other.go", StartLine: 1, EndLine: 2}}},
	}})
	if err == nil || err.Error() != `criterion "criterion 2" returned pass evidence outside the diff` {
		t.Fatalf("Verify() error = %v, want evidence-outside-diff failure", err)
	}

	_, err = Verify([]AcceptanceCriterion{"criterion 3"}, PRDiff{Files: []DiffFile{{
		Path:  "changed.go",
		Patch: "@@ -10,2 +10,2 @@\n-old\n+new\n keep\n",
	}}}, fixtureVerifier{responses: map[AcceptanceCriterion]CriterionAssessment{
		"criterion 3": {Verdict: VerdictPass, Justification: "matched diff", Evidence: []Evidence{{FilePath: "changed.go", StartLine: 50, EndLine: 51}}},
	}})
	if err == nil || err.Error() != `criterion "criterion 3" returned pass evidence outside the diff` {
		t.Fatalf("Verify() error = %v, want evidence-outside-hunks failure", err)
	}
}

type fixtureVerifier struct {
	responses map[AcceptanceCriterion]CriterionAssessment
}

func (f fixtureVerifier) VerifyCriterion(criterion AcceptanceCriterion, _ PRDiff) (CriterionAssessment, error) {
	return f.responses[criterion], nil
}
