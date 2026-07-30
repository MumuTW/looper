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

// ownerDirs are the packages that declare protected label values. Exemption is
// per value, not per directory: labels may not restate a value protocol owns
// and protocol may not restate one labels owns, because labels.go assigns the
// target-label family to protocol precisely so there is one place to look.
var ownerDirs = map[string]string{
	"labels":   filepath.Join(repoRoot, "internal", "labels"),
	"protocol": filepath.Join(repoRoot, "internal", "network", "protocol"),
}

// protectedValue is a label value and the one directory allowed to write it.
type protectedValue struct {
	qualifier string
	ownerDir  string
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

	targetPrefix := Normalize(protocol.TargetLabelPrefix)
	protocolDir, err := filepath.Abs(ownerDirs["protocol"])
	if err != nil {
		t.Fatalf("resolve protocol dir: %v", err)
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
			dir, absErr := filepath.Abs(filepath.Dir(path))
			if absErr != nil {
				return absErr
			}
			for _, found := range stringExpressions(path, authority) {
				// Compare normalized: forge labels are case-insensitive and
				// this package's own comparisons fold case, so "LOOPER:PLAN"
				// is the same protocol label and the same bypass.
				normalized := Normalize(found.value)
				rel, _ := filepath.Rel(repoRoot, path)
				if value, banned := forbidden[normalized]; banned {
					if dir != value.ownerDir {
						t.Errorf("%s:%d writes the label literal %q; use %s instead", rel, found.line, found.value, value.qualifier)
					}
					continue
				}
				// A concrete target label is as much a bypass as the prefix:
				// labels.go assigns the whole family to protocol, so nothing
				// else may spell one out.
				if normalized != targetPrefix && strings.HasPrefix(normalized, targetPrefix) && dir != protocolDir {
					t.Errorf("%s:%d writes the target label %q; build it with protocol.TargetLabelForNode", rel, found.line, found.value)
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
func protectedLabelValues(t *testing.T) (map[string]protectedValue, map[string]string) {
	t.Helper()

	forbidden := map[string]protectedValue{}
	authority := map[string]string{}
	collect := func(dir, qualifier string) {
		ownerDir, absErr := filepath.Abs(dir)
		if absErr != nil {
			t.Fatalf("resolve owner dir %s: %v", dir, absErr)
		}
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
				forbidden[Normalize(value)] = protectedValue{qualifier: qualifier + "." + name, ownerDir: ownerDir}
			}
		}
	}
	// Every file of this package, so a constant added in a future file is
	// protected without anyone remembering to extend a list.
	collect(ownerDirs["labels"], "labels")
	collect(ownerDirs["protocol"], "protocol")
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
	qualifiers := authorityQualifiers(file)
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
		value, ok := foldString(expr, local, authority, qualifiers)
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
// authorityQualifiers maps the name a file actually uses for each authority
// package to its canonical name. An import alias is ordinary Go, and resolving
// it is what keeps `l.Prefix + "plan"` from folding to nothing and slipping
// past a check whose whole purpose is to be unbypassable.
func authorityQualifiers(file *ast.File) map[string]string {
	canonical := map[string]string{
		"github.com/nexu-io/looper/internal/labels":           "labels",
		"github.com/nexu-io/looper/internal/network/protocol": "protocol",
	}
	out := map[string]string{}
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		name, known := canonical[path]
		if !known {
			continue
		}
		local := name
		if spec.Name != nil {
			local = spec.Name.Name
		}
		out[local] = name
	}
	return out
}

func foldString(expr ast.Expr, local, authority, qualifiers map[string]string) (string, bool) {
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
		left, ok := foldString(e.X, local, authority, qualifiers)
		if !ok {
			return "", false
		}
		right, ok := foldString(e.Y, local, authority, qualifiers)
		if !ok {
			return "", false
		}
		return left + right, true
	case *ast.ParenExpr:
		return foldString(e.X, local, authority, qualifiers)
	case *ast.Ident:
		v, ok := local[e.Name]
		return v, ok
	case *ast.SelectorExpr:
		pkg, ok := e.X.(*ast.Ident)
		if !ok {
			return "", false
		}
		qualifier, aliased := qualifiers[pkg.Name]
		if !aliased {
			qualifier = pkg.Name
		}
		v, ok := authority[qualifier+"."+e.Sel.Name]
		return v, ok
	default:
		return "", false
	}
}
