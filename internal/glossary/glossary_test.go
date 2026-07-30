package glossary

import "testing"

const repoRoot = "../.."

func TestContextADRReferencesResolve(t *testing.T) {
	t.Parallel()
	findings, err := CheckADRReferences(repoRoot)
	if err != nil {
		t.Fatalf("CheckADRReferences() error = %v", err)
	}
	for _, finding := range findings {
		t.Errorf("%s", finding)
	}
}

func TestContextPathsResolve(t *testing.T) {
	t.Parallel()
	findings, err := CheckPaths(repoRoot)
	if err != nil {
		t.Fatalf("CheckPaths() error = %v", err)
	}
	for _, finding := range findings {
		t.Errorf("%s", finding)
	}
}

func TestContextCodePointersResolve(t *testing.T) {
	t.Parallel()
	findings, err := CheckCodePointers(repoRoot)
	if err != nil {
		t.Fatalf("CheckCodePointers() error = %v", err)
	}
	for _, finding := range findings {
		t.Errorf("%s", finding)
	}
}

// Ordinary prose must not be read as a citation. A check that fails CI on
// correct text gets suppressed rather than obeyed.
func TestADRCitationIgnoresProse(t *testing.T) {
	t.Parallel()
	for _, prose := range []string{"ADR-related", "ADR-driven design", "the ADR\nrationale"} {
		if match := adrCitation.FindStringSubmatch(prose); match != nil {
			t.Errorf("%q was read as a citation of %q", prose, match[1])
		}
	}
	for _, citation := range []string{"ADR-0001", "ADR 0005", "ADR-00160"} {
		if adrCitation.FindStringSubmatch(citation) == nil {
			t.Errorf("%q was not recognised as a citation", citation)
		}
	}
}
