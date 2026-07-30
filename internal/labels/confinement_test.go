package labels

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/network/protocol"
)

const repoRoot = "../.."

// ownerDirs are the packages allowed to write a protected label literal: the
// package that declares each value.
var ownerDirs = []string{
	filepath.Join(repoRoot, "internal", "labels"),
	filepath.Join(repoRoot, "internal", "network", "protocol"),
}

// Having a constant is not enough. Before this package existed, infra/github
// referenced specpr's label constants correctly and hardcoded "looper:plan" on
// the adjacent line of the same switch; coordinator hand-rolled the
// "looper:target:" prefix twice while protocol.TargetLabelPrefix sat unused.
// Both compiled, both passed review, and neither would have failed a test —
// a bypassed constant looks exactly like a used one.
//
// So the constant is only half the fix. This is the other half: the literal
// value of a label may not be written anywhere but the package that owns it.
//
// The protected set is derived from the declarations themselves — every file
// of this package, plus protocol's target-label prefix — so a new label
// constant is covered the moment it is added, in whichever of the two owning
// packages it lands.
func TestLabelLiteralsAreConfinedToTheirOwningPackage(t *testing.T) {
	t.Parallel()

	forbidden := protectedLabelValues(t)
	if len(forbidden) == 0 {
		t.Fatal("found no label constants to protect; the derivation is broken")
	}
	if _, ok := forbidden[Normalize(protocol.TargetLabelPrefix)]; !ok {
		t.Fatalf("target-label prefix %q is not protected; the coordinator regression this test cites would go unenforced", protocol.TargetLabelPrefix)
	}

	owners := map[string]struct{}{}
	for _, dir := range ownerDirs {
		abs, err := filepath.Abs(dir)
		if err != nil {
			t.Fatalf("resolve owner dir %s: %v", dir, err)
		}
		owners[abs] = struct{}{}
	}

	for _, root := range []string{"internal", "cmd", "pkg"} {
		err := filepath.WalkDir(filepath.Join(repoRoot, root), func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			// Tests may legitimately pin a wire value — TestLabelWireValues
			// does exactly that — so the rule covers production code only.
			if strings.HasSuffix(path, "_test.go") {
				return nil
			}
			if dir, absErr := filepath.Abs(filepath.Dir(path)); absErr == nil {
				if _, owned := owners[dir]; owned {
					return nil
				}
			}
			for _, found := range stringLiterals(path) {
				// Compare normalized: forge labels are case-insensitive and
				// this package's own comparisons fold case, so "LOOPER:PLAN"
				// is the same protocol label and the same bypass.
				if name, banned := forbidden[Normalize(found.value)]; banned {
					rel, _ := filepath.Rel(repoRoot, path)
					t.Errorf("%s:%d writes the label literal %q; use %s instead", rel, found.line, found.value, name)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
}

// protectedLabelValues maps each normalized protected value to the qualified
// constant that holds it.
//
// Prefix is excluded: it is a namespace test rather than a label, and code
// legitimately matches on it through IsLooperOwned.
//
// Not detected: a value assembled from fragments, such as `Prefix + "plan"`.
// Catching that needs constant folding over arbitrary expressions, which is a
// large amount of machinery for a bypass nobody has written; the confinement
// rule targets the copy-paste that has actually happened twice.
func protectedLabelValues(t *testing.T) map[string]string {
	t.Helper()

	out := map[string]string{}
	collect := func(dir, qualifier string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
				continue
			}
			for name, value := range stringConsts(t, filepath.Join(dir, entry.Name())) {
				if name == "Prefix" || !strings.HasPrefix(strings.ToLower(value), Prefix) {
					continue
				}
				out[Normalize(value)] = qualifier + "." + name
			}
		}
	}
	// Every file of this package, so a constant added in a future file is
	// protected without anyone remembering to extend a list.
	collect(filepath.Join(repoRoot, "internal", "labels"), "labels")
	collect(filepath.Join(repoRoot, "internal", "network", "protocol"), "protocol")
	return out
}

func stringConsts(t *testing.T, path string) map[string]string {
	t.Helper()

	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	out := map[string]string{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok || len(value.Names) != len(value.Values) {
				continue
			}
			for i, name := range value.Names {
				lit, ok := value.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				if unquoted, err := strconv.Unquote(lit.Value); err == nil {
					out[name.Name] = unquoted
				}
			}
		}
	}
	return out
}

type literal struct {
	value string
	line  int
}

func stringLiterals(path string) []literal {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil
	}
	out := []literal{}
	ast.Inspect(file, func(node ast.Node) bool {
		lit, ok := node.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		if unquoted, err := strconv.Unquote(lit.Value); err == nil {
			out = append(out, literal{value: unquoted, line: fset.Position(lit.Pos()).Line})
		}
		return true
	})
	return out
}
