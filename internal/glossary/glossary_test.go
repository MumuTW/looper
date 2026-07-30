package glossary

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// repoRoot is two levels up from internal/glossary.
const repoRoot = "../.."

// adrReference matches "ADR-0003" style citations.
var adrReference = regexp.MustCompile(`ADR-(\d{4})`)

// codePointer matches a backticked package-qualified exported identifier, such
// as `forge.ReviewItemID`. The shape is deliberately strict so that the many
// other backticked strings in CONTEXT.md — config keys (roles.planner.triggers),
// event names (triage.report), GitHub fields (blocked_by), label values — are
// not mistaken for code references. Anything not matching is simply not
// checked; this test is a guard against rot, not a completeness requirement.
var codePointer = regexp.MustCompile("`([a-z][a-z0-9]*)\\.([A-Z][A-Za-z0-9]*)`")

// packagePath matches a backticked repository path, such as `internal/labels`
// or `internal/disclosure/disclosure.go`.
var packagePath = regexp.MustCompile("`((?:internal|cmd|pkg)/[A-Za-z0-9_./-]+)`")

func contextDoc(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(repoRoot, "CONTEXT.md"))
	if err != nil {
		t.Fatalf("read CONTEXT.md: %v", err)
	}
	return string(body)
}

// An ADR citation that resolves to no file sends a reader looking for a
// rationale that is not there, and gives no hint whether the decision was
// renumbered, superseded, or never written down.
func TestContextADRReferencesResolve(t *testing.T) {
	t.Parallel()

	entries, err := os.ReadDir(filepath.Join(repoRoot, "docs", "adr"))
	if err != nil {
		t.Fatalf("read docs/adr: %v", err)
	}
	present := map[string]string{}
	for _, entry := range entries {
		if number, _, found := strings.Cut(entry.Name(), "-"); found {
			if existing, duplicate := present[number]; duplicate {
				t.Errorf("ADR number %s is used by two files: %s and %s", number, existing, entry.Name())
			}
			present[number] = entry.Name()
		}
	}

	for _, match := range adrReference.FindAllStringSubmatch(contextDoc(t), -1) {
		if _, ok := present[match[1]]; !ok {
			t.Errorf("CONTEXT.md cites %s, but docs/adr has no such ADR", match[0])
		}
	}
}

// A glossary entry that points at a repository path is only as good as the
// path.
func TestContextPackagePathsResolve(t *testing.T) {
	t.Parallel()

	for _, match := range packagePath.FindAllStringSubmatch(contextDoc(t), -1) {
		if _, err := os.Stat(filepath.Join(repoRoot, match[1])); err != nil {
			t.Errorf("CONTEXT.md references %q, which does not exist", match[1])
		}
	}
}

// The load-bearing one. Entries now say "defined by forge.ReviewItemID"
// instead of restating the definition, so a rename that misses CONTEXT.md
// turns the entry into a confident lie. This makes that a build failure.
func TestContextCodePointersResolve(t *testing.T) {
	t.Parallel()

	declared := declaredIdentifiers(t)
	for _, match := range codePointer.FindAllStringSubmatch(contextDoc(t), -1) {
		pkg, ident := match[1], match[2]
		names, ok := declared[pkg]
		if !ok {
			// Not a package in this repository — CONTEXT.md also backticks
			// things like `json.Marshal`-shaped names from elsewhere.
			continue
		}
		if _, ok := names[ident]; !ok {
			t.Errorf("CONTEXT.md references %s.%s, but package %s declares no such identifier", pkg, ident, pkg)
		}
	}
}

// declaredIdentifiers maps package name to the set of top-level identifiers it
// declares. Package name rather than import path is enough here: CONTEXT.md
// writes `forge.ReviewItemID`, the way the code reads.
func declaredIdentifiers(t *testing.T) map[string]map[string]struct{} {
	t.Helper()

	out := map[string]map[string]struct{}{}
	roots := []string{"internal", "cmd", "pkg"}
	for _, root := range roots {
		err := filepath.WalkDir(filepath.Join(repoRoot, root), func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
			if parseErr != nil {
				return nil
			}
			pkg := file.Name.Name
			if out[pkg] == nil {
				out[pkg] = map[string]struct{}{}
			}
			for _, name := range topLevelNames(file) {
				out[pkg][name] = struct{}{}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	return out
}

func topLevelNames(file *ast.File) []string {
	names := []string{}
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Recv == nil {
				names = append(names, d.Name.Name)
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					names = append(names, s.Name.Name)
				case *ast.ValueSpec:
					for _, name := range s.Names {
						names = append(names, name.Name)
					}
				}
			}
		}
	}
	sort.Strings(names)
	return names
}
