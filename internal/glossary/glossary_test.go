package glossary

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

func TestCheckPathsReportsStaleTopLevelDirectory(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeGlossaryFixture(t, root, "CONTEXT.md", "`docs/adr/0017.md`")

	findings, err := CheckPaths(root)
	if err != nil {
		t.Fatalf("CheckPaths() error = %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("CheckPaths() findings = %v, want one stale path", findings)
	}
	if findings[0].Reference != "docs/adr/0017.md" || !strings.Contains(findings[0].Detail, "does not exist") {
		t.Errorf("CheckPaths() finding = %v, want missing docs/adr/0017.md", findings[0])
	}
}

func TestCheckCodePointersSupportsPathQualifiedPointers(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeGlossaryFixture(t, root, "CONTEXT.md", "`internal/labels.DefaultPlanTrigger`\n`internal/labels.Missing`")
	writeGlossaryFixture(t, root, "internal/labels/labels.go", "package labels\n\nconst DefaultPlanTrigger = \"looper:plan\"\n")

	findings, err := CheckCodePointers(root)
	if err != nil {
		t.Fatalf("CheckCodePointers() error = %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("CheckCodePointers() findings = %v, want one missing identifier", findings)
	}
	if findings[0].Reference != "internal/labels.Missing" || !strings.Contains(findings[0].Detail, "declares no such identifier") {
		t.Errorf("CheckCodePointers() finding = %v, want missing path-qualified identifier", findings[0])
	}

	pathFindings, err := CheckPaths(root)
	if err != nil {
		t.Fatalf("CheckPaths() error = %v", err)
	}
	if len(pathFindings) != 0 {
		t.Errorf("CheckPaths() findings = %v, want code pointers excluded from path checks", pathFindings)
	}
}

func writeGlossaryFixture(t *testing.T, root, name, body string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}
