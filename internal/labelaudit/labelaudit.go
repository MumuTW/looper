// Package labelaudit finds places where a label's literal value is written
// outside the package that declares it.
//
// Having a constant is not enough. internal/infra/github once referenced
// specpr's label constants correctly and hardcoded "looper:plan" on the
// adjacent line of the same switch; internal/coordinator hand-rolled the
// "looper:target:" prefix twice while protocol.TargetLabelPrefix sat unused.
// Both compiled, both passed review, and neither would have failed a test,
// because a bypassed constant looks exactly like a used one.
//
// This is the other half of that fix, and it is a package rather than a test
// body because it is a static analysis with edge cases of its own — aliased
// imports, assembled constants, per-owner exemption — that need to be covered
// directly instead of through the repository it happens to scan.
//
// # What it does not detect
//
// The fold is syntactic. It resolves literals, concatenation, parentheses,
// file-scope and block-scope string constants, and selectors on a confirmed
// import of an owner package. It does not implement Go's constant evaluation,
// so all of these are out of scope:
//
//   - a constant declared in one file of a non-owner package and used in
//     another
//   - a chain of owner constants deeper than one level of derivation
//   - a local identifier that shadows an imported package name
//   - anything built at runtime, such as fmt.Sprintf
//
// The boundary is deliberate. Completeness needs go/types and the whole
// package graph, which would make a check that runs in `go test ./...`
// markedly slower to guard cases nobody has written; both regressions this
// exists to prevent were bare literals. A gap here is a missed report, never
// a false one — the failure mode is silence, not a wrong accusation — and the
// rule it enforces is a convention, not a security boundary. If a bypass in
// this list ever happens for real, that is the evidence to spend go/types on.
package labelaudit

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/MumuTW/looper/internal/labels"
	"github.com/MumuTW/looper/internal/network/protocol"
)

// Owner is a package permitted to write the label values it declares.
type Owner struct {
	// Qualifier is how the package is named in Go source, e.g. "labels".
	Qualifier string
	// ImportPath is matched against a file's imports so an aliased or dot
	// import still resolves to this owner.
	ImportPath string
	// Dir is the directory, relative to the scanned root, holding the package.
	Dir string
}

// DefaultOwners are the two packages that declare Looper's label values.
// labels owns the vocabulary; protocol owns the target-label family, because
// forming one requires validating a Node name.
func DefaultOwners() []Owner {
	return []Owner{
		{Qualifier: "labels", ImportPath: "github.com/MumuTW/looper/internal/labels", Dir: filepath.Join("internal", "labels")},
		{Qualifier: "protocol", ImportPath: "github.com/MumuTW/looper/internal/network/protocol", Dir: filepath.Join("internal", "network", "protocol")},
	}
}

// DefaultRoots are the directories holding production Go code. tools is
// included: a label hardcoded in a build tool is as much a second definition
// as one in internal.
func DefaultRoots() []string {
	return []string{"internal", "cmd", "pkg", "tools"}
}

// Violation is one literal or assembled label value written outside its owner.
type Violation struct {
	File  string
	Line  int
	Value string
	// Use names the constant the value should have been referenced as, or is
	// empty for a target label, which must be constructed rather than named.
	Use     string
	Message string
}

func (v Violation) String() string {
	return fmt.Sprintf("%s:%d: %s", v.File, v.Line, v.Message)
}

// Config controls a scan. The zero value is not usable; see Scan.
type Config struct {
	Root   string
	Roots  []string
	Owners []Owner
}

// Scan reports every protected label value written outside its owning package,
// sorted by file and line.
//
// Test files are scanned too. A label rename must fail at every fixture that
// constructs or asserts the protocol value, not only in production code. The
// owner package remains exempt because its wire-value test pins those values.
func Scan(root string) ([]Violation, error) {
	return ScanWith(Config{Root: root, Roots: DefaultRoots(), Owners: DefaultOwners()})
}

// ScanWith is Scan with the roots and owners named explicitly, for testing the
// analysis against fixtures rather than against this repository.
func ScanWith(cfg Config) ([]Violation, error) {
	protected, authority, err := protectedValues(cfg)
	if err != nil {
		return nil, err
	}
	if len(protected) == 0 {
		return nil, fmt.Errorf("no label constants found under %v; the derivation is broken", cfg.Owners)
	}

	targetPrefix := labels.Normalize(protocol.TargetLabelPrefix)
	targetOwner := ""
	for _, owner := range cfg.Owners {
		if _, ok := protected[targetPrefix]; ok && protected[targetPrefix].ownerDir == absDir(cfg.Root, owner.Dir) {
			targetOwner = protected[targetPrefix].ownerDir
		}
	}

	violations := []Violation{}
	for _, name := range cfg.Roots {
		root := filepath.Join(cfg.Root, name)
		if _, statErr := os.Stat(root); statErr != nil {
			continue
		}
		walkErr := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				// testdata holds fixtures for this analysis and for others;
				// it is not production code and must not be scanned as such.
				if entry.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}
			dir, absErr := filepath.Abs(filepath.Dir(path))
			if absErr != nil {
				return absErr
			}
			rel, relErr := filepath.Rel(cfg.Root, path)
			if relErr != nil {
				rel = path
			}
			rel = filepath.ToSlash(rel)

			for _, found := range stringExpressions(path, authority, cfg.Owners) {
				normalized := labels.Normalize(found.value)
				if value, banned := protected[normalized]; banned {
					if dir != value.ownerDir {
						violations = append(violations, Violation{
							File: rel, Line: found.line, Value: found.value, Use: value.qualifier,
							Message: fmt.Sprintf("writes the label literal %q; use %s instead", found.value, value.qualifier),
						})
					}
					continue
				}
				// A concrete target label is as much a second definition as
				// the prefix: the family is constructed, never spelled out.
				if targetOwner != "" && normalized != targetPrefix && strings.HasPrefix(normalized, targetPrefix) && dir != targetOwner {
					violations = append(violations, Violation{
						File: rel, Line: found.line, Value: found.value,
						Message: fmt.Sprintf("writes the target label %q; build it with protocol.TargetLabelForNode", found.value),
					})
				}
			}
			return nil
		})
		if walkErr != nil {
			return nil, fmt.Errorf("walk %s: %w", name, walkErr)
		}
	}

	sort.Slice(violations, func(i, j int) bool {
		if violations[i].File != violations[j].File {
			return violations[i].File < violations[j].File
		}
		return violations[i].Line < violations[j].Line
	})
	return violations, nil
}

type protectedValue struct {
	qualifier string
	ownerDir  string
}

func absDir(root, dir string) string {
	abs, err := filepath.Abs(filepath.Join(root, dir))
	if err != nil {
		return filepath.Join(root, dir)
	}
	return abs
}

// protectedValues derives the protected set from the owners' own declarations,
// so a label constant added later is covered without anyone extending a list.
// It also returns an authority table, qualified constant name to value, used
// to fold expressions that assemble a label from one.
func protectedValues(cfg Config) (map[string]protectedValue, map[string]string, error) {
	protected := map[string]protectedValue{}
	authority := map[string]string{}

	for _, owner := range cfg.Owners {
		dir := absDir(cfg.Root, owner.Dir)
		entries, err := os.ReadDir(filepath.Join(cfg.Root, owner.Dir))
		if err != nil {
			return nil, nil, fmt.Errorf("read owner %s: %w", owner.Dir, err)
		}
		// Two passes: collect every constant first so that a definition
		// assembled from a sibling — const NewTrigger = Prefix + "new" — folds
		// rather than being skipped as a non-literal.
		local := map[string]string{}
		files := []string{}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
				continue
			}
			path := filepath.Join(cfg.Root, owner.Dir, entry.Name())
			files = append(files, path)
			for name, value := range constantStrings(path, nil, nil) {
				local[name] = value
			}
		}
		for _, path := range files {
			for name, value := range constantStrings(path, local, nil) {
				local[name] = value
			}
		}

		for name, value := range local {
			qualified := owner.Qualifier + "." + name
			authority[qualified] = value
			if name == "Prefix" || !strings.HasPrefix(strings.ToLower(value), labels.Prefix) {
				continue
			}
			normalized := labels.Normalize(value)
			if existing, clash := protected[normalized]; clash && existing.ownerDir != dir {
				return nil, nil, fmt.Errorf("label %q is declared by both %s and %s; ownership must be unambiguous", value, existing.qualifier, qualified)
			}
			protected[normalized] = protectedValue{qualifier: qualified, ownerDir: dir}
		}
	}
	return protected, authority, nil
}

type literal struct {
	value string
	line  int
}

// stringExpressions returns every literal or assembled constant string in the
// file. A bare reference to a label constant is the intended usage and is not
// reported; only a value spelled out or rebuilt is a second definition.
func stringExpressions(path string, authority map[string]string, owners []Owner) []literal {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil
	}
	local := fileConstants(file)
	qualifiers := authorityQualifiers(file, owners)

	out := []literal{}
	ast.Inspect(file, func(node ast.Node) bool {
		expr, ok := node.(ast.Expr)
		if !ok {
			return true
		}
		switch expr.(type) {
		case *ast.BasicLit, *ast.BinaryExpr:
		default:
			return true
		}
		if value, folded := fold(expr, local, authority, qualifiers); folded {
			out = append(out, literal{value: value, line: fset.Position(expr.Pos()).Line})
		}
		return true
	})
	return out
}

// authorityQualifiers maps the name a file actually uses for each owner
// package to that owner's qualifier. Import aliases are ordinary Go, and a dot
// import puts the constants in file scope, so both are resolved here rather
// than assuming every file spells the package the same way.
func authorityQualifiers(file *ast.File, owners []Owner) map[string]string {
	canonical := map[string]string{}
	for _, owner := range owners {
		canonical[owner.ImportPath] = owner.Qualifier
	}
	out := map[string]string{}
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		qualifier, known := canonical[path]
		if !known {
			continue
		}
		local := qualifier
		if spec.Name != nil {
			local = spec.Name.Name
		}
		out[local] = qualifier
	}
	return out
}

// fileConstants collects constant strings declared anywhere in the file,
// including inside function bodies: a block-scoped const is as good a way to
// reassemble a label as a package-level one.
func fileConstants(file *ast.File) map[string]string {
	out := map[string]string{}
	ast.Inspect(file, func(node ast.Node) bool {
		decl, ok := node.(*ast.GenDecl)
		if !ok || decl.Tok != token.CONST {
			return true
		}
		for _, spec := range decl.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok || len(value.Names) != len(value.Values) {
				continue
			}
			for i, name := range value.Names {
				if lit, ok := value.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
					if unquoted, err := strconv.Unquote(lit.Value); err == nil {
						out[name.Name] = unquoted
					}
				}
			}
		}
		return true
	})
	return out
}

// constantStrings parses one file for constant strings, optionally folding
// against constants already known.
func constantStrings(path string, local, authority map[string]string) map[string]string {
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil
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
				if folded, ok := fold(value.Values[i], local, authority, nil); ok {
					out[name.Name] = folded
				}
			}
		}
	}
	return out
}

// fold evaluates expr as a compile-time constant string. It is bounded to
// literals, concatenation, parentheses, plain identifiers and selectors on an
// owner package — not arbitrary expressions. Runtime construction such as
// fmt.Sprintf is out of scope: the rule targets copy-paste and constant
// reassembly, which is what has actually happened.
func fold(expr ast.Expr, local, authority, qualifiers map[string]string) (string, bool) {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return "", false
		}
		unquoted, err := strconv.Unquote(e.Value)
		return unquoted, err == nil
	case *ast.ParenExpr:
		return fold(e.X, local, authority, qualifiers)
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return "", false
		}
		left, ok := fold(e.X, local, authority, qualifiers)
		if !ok {
			return "", false
		}
		right, ok := fold(e.Y, local, authority, qualifiers)
		if !ok {
			return "", false
		}
		return left + right, true
	case *ast.Ident:
		if value, ok := local[e.Name]; ok {
			return value, true
		}
		// A dot-imported owner constant is an unqualified identifier.
		for localName, qualifier := range qualifiers {
			if localName == "." {
				if value, ok := authority[qualifier+"."+e.Name]; ok {
					return value, true
				}
			}
		}
		return "", false
	case *ast.SelectorExpr:
		pkg, ok := e.X.(*ast.Ident)
		if !ok {
			return "", false
		}
		// Only a confirmed import of an owner package counts. A local
		// variable that happens to be named labels must not be read as one.
		qualifier, imported := qualifiers[pkg.Name]
		if !imported {
			return "", false
		}
		value, ok := authority[qualifier+"."+e.Sel.Name]
		return value, ok
	default:
		return "", false
	}
}
