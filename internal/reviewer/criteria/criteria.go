package criteria

import (
	"fmt"
	"strings"
)

type AcceptanceCriterion string

type Verdict string

const (
	VerdictPass         Verdict = "pass"
	VerdictFail         Verdict = "fail"
	VerdictUnverifiable Verdict = "unverifiable"
)

type AggregateDisposition string

const (
	DispositionPass         AggregateDisposition = "pass"
	DispositionFail         AggregateDisposition = "fail"
	DispositionUnverifiable AggregateDisposition = "unverifiable"
)

type Evidence struct {
	FilePath  string
	StartLine int
	EndLine   int
}

type PRDiff struct {
	Files []DiffFile
}

type DiffFile struct {
	Path  string
	Patch string
}

type CriterionAssessment struct {
	Verdict       Verdict
	Justification string
	Evidence      []Evidence
}

type CriterionResult struct {
	Criterion     AcceptanceCriterion
	Verdict       Verdict
	Justification string
	Evidence      []Evidence
}

type VerificationResult struct {
	Disposition AggregateDisposition
	Criteria    []CriterionResult
}

type Verifier interface {
	VerifyCriterion(criterion AcceptanceCriterion, diff PRDiff) (CriterionAssessment, error)
}

func Extract(issueBody string) []AcceptanceCriterion {
	lines := strings.Split(issueBody, "\n")
	inSection := false
	criteria := make([]AcceptanceCriterion, 0)

	for _, rawLine := range lines {
		trimmed := strings.TrimSpace(rawLine)

		if strings.HasPrefix(trimmed, "## ") {
			if isAcceptanceCriteriaHeading(trimmed) {
				inSection = true
				continue
			}
			if inSection {
				break
			}
		}
		if !inSection || trimmed == "" {
			continue
		}
		if criterion, ok := parseCriterionLine(trimmed); ok {
			criteria = append(criteria, AcceptanceCriterion(criterion))
		}
	}

	return criteria
}

func Verify(criteria []AcceptanceCriterion, diff PRDiff, verifier Verifier) (VerificationResult, error) {
	if verifier == nil && len(criteria) > 0 {
		return VerificationResult{}, fmt.Errorf("criteria verifier is required")
	}

	results := make([]CriterionResult, 0, len(criteria))
	disposition := DispositionPass

	for _, criterion := range criteria {
		assessment, err := verifier.VerifyCriterion(criterion, diff)
		if err != nil {
			return VerificationResult{}, err
		}
		if err := validateAssessment(criterion, assessment, diff); err != nil {
			return VerificationResult{}, err
		}
		results = append(results, CriterionResult{
			Criterion:     criterion,
			Verdict:       assessment.Verdict,
			Justification: assessment.Justification,
			Evidence:      append([]Evidence(nil), assessment.Evidence...),
		})
		switch assessment.Verdict {
		case VerdictFail:
			disposition = DispositionFail
		case VerdictUnverifiable:
			if disposition != DispositionFail {
				disposition = DispositionUnverifiable
			}
		}
	}

	return VerificationResult{Disposition: disposition, Criteria: results}, nil
}

func validateAssessment(criterion AcceptanceCriterion, assessment CriterionAssessment, diff PRDiff) error {
	if assessment.Verdict != VerdictPass && assessment.Verdict != VerdictFail && assessment.Verdict != VerdictUnverifiable {
		return fmt.Errorf("criterion %q returned unsupported verdict %q", criterion, assessment.Verdict)
	}
	if strings.TrimSpace(assessment.Justification) == "" {
		return fmt.Errorf("criterion %q returned empty justification", criterion)
	}
	if assessment.Verdict != VerdictPass {
		return nil
	}
	if len(assessment.Evidence) == 0 {
		return fmt.Errorf("criterion %q returned pass without evidence", criterion)
	}
	for _, evidence := range assessment.Evidence {
		if strings.TrimSpace(evidence.FilePath) == "" || evidence.StartLine < 1 || evidence.EndLine < evidence.StartLine {
			return fmt.Errorf("criterion %q returned invalid evidence", criterion)
		}
		if !diffContainsFile(diff, evidence.FilePath) {
			return fmt.Errorf("criterion %q returned pass evidence outside the diff", criterion)
		}
	}
	return nil
}

func diffContainsFile(diff PRDiff, filePath string) bool {
	for _, file := range diff.Files {
		if file.Path == filePath {
			return true
		}
	}
	return false
}

func parseCriterionLine(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return "", false
	}
	if strings.HasPrefix(trimmed, "-") || strings.HasPrefix(trimmed, "*") {
		trimmed = strings.TrimSpace(trimmed[1:])
	}
	trimmed = strings.TrimPrefix(trimmed, "[ ]")
	trimmed = strings.TrimPrefix(trimmed, "[x]")
	trimmed = strings.TrimPrefix(trimmed, "[X]")
	trimmed = strings.TrimSpace(trimmed)
	if strings.HasPrefix(trimmed, "[") {
		if idx := strings.Index(trimmed, "]"); idx >= 0 && idx+1 < len(trimmed) {
			trimmed = strings.TrimSpace(trimmed[idx+1:])
		}
	}
	if trimmed == "" {
		return "", false
	}
	return trimmed, true
}

func isAcceptanceCriteriaHeading(line string) bool {
	heading := strings.TrimSpace(strings.TrimPrefix(line, "## "))
	heading = strings.TrimSpace(strings.TrimRight(heading, ":;.!?"))
	return strings.EqualFold(heading, "acceptance criteria")
}
