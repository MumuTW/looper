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
)

const repoRoot = "../.."

// Having a constant is not enough. Before this package existed, infra/github
// referenced specpr's label constants correctly and hardcoded "looper:plan" on
// the adjacent line of the same switch; coordinator hand-rolled the
// "looper:target:" prefix twice while protocol.TargetLabelPrefix sat unused.
// Both compiled, both passed review, and neither would have failed a test —
// a bypassed constant looks exactly like a used one.
//
// So the constant is only half the fix. This is the other half: the literal
// value of a label may not be written anywhere but here.
//
// The rule derives the forbidden set from this package's own declarations, so
// a new label constant is covered the moment it is added.
func TestLabelLiteralsAreConfinedToThisPackage(t *testing.T) {
	t.Parallel()

	forbidden := declaredLabelValues(t)
	if len(forbidden) == 0 {
		t.Fatal("found no label constants to protect; the derivation is broken")
	}

	labelsDir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("resolve package dir: %v", err)
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
			if dir, absErr := filepath.Abs(filepath.Dir(path)); absErr == nil && dir == labelsDir {
				return nil
			}
			for _, found := range stringLiterals(path) {
				if name, banned := forbidden[found.value]; banned {
					rel, _ := filepath.Rel(repoRoot, path)
					t.Errorf("%s:%d writes the label literal %q; use labels.%s instead", rel, found.line, found.value, name)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
}

// declaredLabelValues maps each label string this package declares to the
// constant that holds it. Prefix is excluded: it is a namespace test, not a
// label, and code legitimately matches on it via IsLooperOwned.
func declaredLabelValues(t *testing.T) map[string]string {
	t.Helper()

	file, err := parser.ParseFile(token.NewFileSet(), "labels.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse labels.go: %v", err)
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
				unquoted, err := strconv.Unquote(lit.Value)
				if err != nil || name.Name == "Prefix" {
					continue
				}
				out[unquoted] = name.Name
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
