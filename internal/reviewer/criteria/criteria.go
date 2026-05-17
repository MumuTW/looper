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

		if isAcceptanceCriteriaHeading(trimmed) {
			inSection = true
			continue
		}
		if isMarkdownHeading(trimmed) {
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
	trimmed = trimCheckboxPrefix(trimmed)
	if trimmed == "" {
		return "", false
	}
	return trimmed, true
}

func isAcceptanceCriteriaHeading(line string) bool {
	heading, ok := markdownHeadingText(line)
	if !ok {
		return false
	}
	heading = strings.TrimSpace(strings.TrimRight(heading, ":;.!?"))
	return strings.EqualFold(heading, "acceptance criteria")
}

func isMarkdownHeading(line string) bool {
	_, ok := markdownHeadingText(line)
	return ok
}

func markdownHeadingText(line string) (string, bool) {
	if line == "" || line[0] != '#' {
		return "", false
	}
	level := 0
	for level < len(line) && line[level] == '#' {
		level++
	}
	if level == 0 || level > 6 || level >= len(line) || line[level] != ' ' {
		return "", false
	}
	return strings.TrimSpace(line[level+1:]), true
}

func trimCheckboxPrefix(line string) string {
	for _, prefix := range []string{"[ ]", "[x]", "[X]", "[]"} {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(line[len(prefix):])
		}
	}
	return strings.TrimSpace(line)
}
