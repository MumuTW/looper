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

	forbidden, authority := protectedLabelValues(t)
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
			for _, found := range stringExpressions(path, authority) {
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
// constant that holds it, and returns an authority table (qualified constant
// name → value) for folding assembled expressions such as `labels.Prefix +
// "plan"`.
//
// Prefix is excluded from the protected set: it is a namespace marker rather
// than a label, and code legitimately matches on it through IsLooperOwned. It
// remains in the authority table so a concatenation that assembles a full label
// from it folds to the label value and is caught.
//
// Assembled values are detected when they are constant: literal concatenation
// (`"looper:" + "plan"`) and references to label-authority constants
// (`labels.Prefix + "plan"`). Arbitrary runtime expressions such as
// fmt.Sprintf are not folded; the confinement rule targets the copy-paste and
// constant-reassembly that has actually happened, not every conceivable
// construction.
func protectedLabelValues(t *testing.T) (map[string]string, map[string]string) {
	t.Helper()

	forbidden := map[string]string{}
	authority := map[string]string{}
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
				authority[qualifier+"."+name] = value
				if name == "Prefix" || !strings.HasPrefix(strings.ToLower(value), Prefix) {
					continue
				}
				forbidden[Normalize(value)] = qualifier + "." + name
			}
		}
	}
	// Every file of this package, so a constant added in a future file is
	// protected without anyone remembering to extend a list.
	collect(filepath.Join(repoRoot, "internal", "labels"), "labels")
	collect(filepath.Join(repoRoot, "internal", "network", "protocol"), "protocol")
	return forbidden, authority
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

// stringExpressions returns the compile-time constant string expressions in a
// file: bare string literals and assembled values such as "looper:" + "plan"
// or labels.Prefix + "plan". A bare reference to a label constant
// (labels.DefaultPlanTrigger) is the intended usage and is not reported; only
// literal or assembled expressions restate a value, so only those are
// collected. Each top-level constant expression is reported once rather than
// re-reporting its fragments.
func stringExpressions(path string, authority map[string]string) []literal {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil
	}
	local := fileStringConstants(file)
	out := []literal{}
	ast.Inspect(file, func(node ast.Node) bool {
		expr, ok := node.(ast.Expr)
		if !ok {
			return true
		}
		// Only literal or assembled expressions can restate a value; a bare
		// reference to the constant is the intended usage.
		switch expr.(type) {
		case *ast.BasicLit, *ast.BinaryExpr:
		default:
			return true
		}
		value, ok := foldString(expr, local, authority)
		if !ok {
			return true
		}
		out = append(out, literal{value: value, line: fset.Position(expr.Pos()).Line})
		return false // handled this expression; do not re-report its fragments
	})
	return out
}

// fileStringConstants returns this file's own top-level string constants
// (local name → value) so a concatenation using a local constant folds.
func fileStringConstants(file *ast.File) map[string]string {
	consts := map[string]string{}
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
					consts[name.Name] = unquoted
				}
			}
		}
	}
	return consts
}

// foldString evaluates expr as a compile-time constant string, resolving
// references to label-authority constants (labels.Prefix,
// protocol.TargetLabelPrefix) and local file constants. It returns the folded
// value and true when expr is a constant string expression.
func foldString(expr ast.Expr, local, authority map[string]string) (string, bool) {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return "", false
		}
		v, err := strconv.Unquote(e.Value)
		if err != nil {
			return "", false
		}
		return v, true
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return "", false
		}
		left, ok := foldString(e.X, local, authority)
		if !ok {
			return "", false
		}
		right, ok := foldString(e.Y, local, authority)
		if !ok {
			return "", false
		}
		return left + right, true
	case *ast.ParenExpr:
		return foldString(e.X, local, authority)
	case *ast.Ident:
		v, ok := local[e.Name]
		return v, ok
	case *ast.SelectorExpr:
		pkg, ok := e.X.(*ast.Ident)
		if !ok {
			return "", false
		}
		v, ok := authority[pkg.Name+"."+e.Sel.Name]
		return v, ok
	default:
		return "", false
	}
}
