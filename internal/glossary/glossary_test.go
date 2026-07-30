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

// externalPackageQualifiers lists package names CONTEXT.md may reference that
// are not part of this repository. It is empty today and deliberately explicit:
// an unknown qualifier is a typo far more often than a new external reference,
// so the check denies by default and this is the visible escape hatch.
var externalPackageQualifiers = map[string]struct{}{}

// adrReference matches an ADR citation loosely so that a malformed one such as
// ADR-00010 is caught here rather than silently matching its first four digits.
// RE2 has no lookahead, so the token is captured whole and checked below.
var adrReference = regexp.MustCompile(`ADR-([0-9A-Za-z]+)`)

// exactlyFourDigits guards a captured ADR token and an ADR filename prefix.
var exactlyFourDigits = regexp.MustCompile(`^[0-9]{4}$`)

// codePointer matches a backticked package-qualified exported identifier, such
// as `forge.ReviewItemID`. The shape is deliberately strict so that the many
// other backticked strings in CONTEXT.md — config keys (roles.planner.triggers),
// event names (triage.report), GitHub fields (blocked_by), label values — are
// not mistaken for code references. Anything not matching is simply not
// checked; this test guards against rot, it does not require completeness.
var codePointer = regexp.MustCompile("`([a-z][a-z0-9_]*)\\.([A-Z][A-Za-z0-9_]*)`")

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
		// Only a regular .md file is a decision record. A directory or asset
		// carrying a numeric prefix would otherwise satisfy a citation that has
		// no rationale behind it.
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		number, _, found := strings.Cut(entry.Name(), "-")
		if !found || !exactlyFourDigits.MatchString(number) {
			continue
		}
		if existing, duplicate := present[number]; duplicate {
			t.Errorf("ADR number %s is used by two files: %s and %s", number, existing, entry.Name())
		}
		present[number] = entry.Name()
	}

	for _, match := range adrReference.FindAllStringSubmatch(contextDoc(t), -1) {
		cited := match[1]
		if !exactlyFourDigits.MatchString(cited) {
			t.Errorf("CONTEXT.md cites %s, which is not a four-digit ADR number", match[0])
			continue
		}
		if _, ok := present[cited]; !ok {
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

// The load-bearing one. Entries say "defined by forge.ReviewItemID" instead of
// restating the definition, so a rename that misses CONTEXT.md turns the entry
// into a confident lie. This makes that a build failure.
func TestContextCodePointersResolve(t *testing.T) {
	t.Parallel()

	byName := packagesByName(t)
	for _, match := range codePointer.FindAllStringSubmatch(contextDoc(t), -1) {
		pkg, ident := match[1], match[2]
		if _, external := externalPackageQualifiers[pkg]; external {
			continue
		}
		candidates := byName[pkg]
		switch len(candidates) {
		case 0:
			// Denying by default is the point. A mistyped qualifier such as
			// `protcol.ParseTargetLabel` is exactly the stale pointer this test
			// exists to catch; skipping it would leave the primary guarantee
			// unenforced.
			t.Errorf("CONTEXT.md references %s.%s, but no package in this repository is named %q; fix the reference or add %q to externalPackageQualifiers", pkg, ident, pkg, pkg)
		case 1:
			if _, ok := candidates[0].idents[ident]; !ok {
				t.Errorf("CONTEXT.md references %s.%s, but %s declares no such identifier", pkg, ident, candidates[0].path)
			}
		default:
			paths := make([]string, 0, len(candidates))
			for _, candidate := range candidates {
				paths = append(paths, candidate.path)
			}
			sort.Strings(paths)
			t.Errorf("CONTEXT.md references %s.%s, but %q is ambiguous across %s; name the path instead of the bare package", pkg, ident, pkg, strings.Join(paths, " and "))
		}
	}
}

type declaredPackage struct {
	path   string
	idents map[string]struct{}
}

// packagesByName groups packages by declared name while keeping their paths
// distinct. Pooling identifiers by name alone would let a pointer resolve
// against the wrong package — internal/api and pkg/api are both named api — so
// an ambiguous qualifier is reported rather than quietly satisfied by either.
func packagesByName(t *testing.T) map[string][]declaredPackage {
	t.Helper()

	byDir := map[string]*declaredPackage{}
	nameByDir := map[string]string{}
	for _, root := range []string{"internal", "cmd", "pkg"} {
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
			dir := filepath.Dir(path)
			if byDir[dir] == nil {
				rel, relErr := filepath.Rel(repoRoot, dir)
				if relErr != nil {
					rel = dir
				}
				byDir[dir] = &declaredPackage{path: filepath.ToSlash(rel), idents: map[string]struct{}{}}
				nameByDir[dir] = file.Name.Name
			}
			for _, name := range topLevelNames(file) {
				byDir[dir].idents[name] = struct{}{}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}

	out := map[string][]declaredPackage{}
	for dir, pkg := range byDir {
		name := nameByDir[dir]
		out[name] = append(out[name], *pkg)
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
	return names
}
