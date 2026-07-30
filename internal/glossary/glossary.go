package glossary

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// ExternalPackageQualifiers lists package names CONTEXT.md may reference that
// are not part of this repository. It is empty today and deliberately explicit:
// an unknown qualifier is a typo far more often than a new external reference,
// so the check denies by default and this is the visible escape hatch.
var ExternalPackageQualifiers = map[string]struct{}{}

// adrCitation matches an ADR citation in either spelling the repository uses:
// headings say "ADR 0005", prose says "ADR-0005".
//
// The number must begin with a digit so that ordinary prose — "ADR-related",
// or the word ADR ending a line before an unrelated word — is not read as a
// citation and failed for not being four digits. A check that fires on correct
// text gets suppressed rather than obeyed. The separator is a hyphen or a
// space rather than \s, for the same reason: it must not cross a line break.
var adrCitation = regexp.MustCompile(`ADR[- ]([0-9][0-9A-Za-z]*)`)

var exactlyFourDigits = regexp.MustCompile(`^[0-9]{4}$`)

// codePointer matches a backticked package-qualified exported identifier, such
// as `forge.ReviewItemID`. The shape is deliberately strict so that the many
// other backticked strings in CONTEXT.md — config keys (roles.planner.triggers),
// event names (triage.report), GitHub fields (blocked_by), label values — are
// not mistaken for code references. Anything not matching is simply not
// checked; this guards against rot, it does not require completeness.
var codePointer = regexp.MustCompile("`([a-z][a-z0-9_]*)\\.([A-Z][A-Za-z0-9_]*)`")

// repoReference matches a backticked path or root file name.
var repoReference = regexp.MustCompile("`([A-Za-z0-9_.-]+(?:/[^`]*)?)`")

// Finding is one reference in CONTEXT.md that does not resolve.
type Finding struct {
	Reference string
	Detail    string
}

func (f Finding) String() string {
	return fmt.Sprintf("CONTEXT.md references %q: %s", f.Reference, f.Detail)
}

func contextDoc(root string) (string, error) {
	body, err := os.ReadFile(filepath.Join(root, "CONTEXT.md"))
	if err != nil {
		return "", fmt.Errorf("read CONTEXT.md: %w", err)
	}
	return string(body), nil
}

// CheckADRReferences reports citations that resolve to no decision record, and
// numbering that is ambiguous or disagrees with its own heading. A citation
// that resolves to nothing sends a reader looking for a rationale that is not
// there, with no hint whether it was renumbered, superseded, or never written.
func CheckADRReferences(root string) ([]Finding, error) {
	doc, err := contextDoc(root)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(root, "docs", "adr"))
	if err != nil {
		return nil, fmt.Errorf("read docs/adr: %w", err)
	}

	findings := []Finding{}
	present := map[string]string{}
	for _, entry := range entries {
		// Only a regular .md file is a decision record. A directory or asset
		// carrying a numeric prefix would otherwise satisfy a citation that
		// has no rationale behind it.
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		number, _, found := strings.Cut(entry.Name(), "-")
		if !found || !exactlyFourDigits.MatchString(number) {
			continue
		}
		if existing, duplicate := present[number]; duplicate {
			findings = append(findings, Finding{
				Reference: "ADR-" + number,
				Detail:    fmt.Sprintf("used by two files, %s and %s", existing, entry.Name()),
			})
		}
		present[number] = entry.Name()

		if heading, ok := adrHeadingNumber(filepath.Join(root, "docs", "adr", entry.Name())); ok && heading != number {
			findings = append(findings, Finding{
				Reference: entry.Name(),
				Detail:    fmt.Sprintf("numbered %s in its filename but %s in its heading", number, heading),
			})
		}
	}

	for _, match := range adrCitation.FindAllStringSubmatch(doc, -1) {
		cited := match[1]
		if !exactlyFourDigits.MatchString(cited) {
			findings = append(findings, Finding{Reference: match[0], Detail: "not a four-digit ADR number"})
			continue
		}
		if _, ok := present[cited]; !ok {
			findings = append(findings, Finding{Reference: match[0], Detail: "docs/adr has no such ADR"})
		}
	}
	return findings, nil
}

// adrHeadingNumber reads a record's first Markdown heading and reports the
// number it claims. The whole token is captured rather than its first four
// digits, so "ADR-00160" does not agree with a file named 0016 by truncation.
func adrHeadingNumber(path string) (string, bool) {
	body, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	heading := regexp.MustCompile(`ADR[- ]([0-9A-Za-z]+)`)
	for _, line := range strings.Split(string(body), "\n") {
		if !strings.HasPrefix(line, "#") {
			continue
		}
		if m := heading.FindStringSubmatch(line); m != nil {
			return m[1], true
		}
		return "", false
	}
	return "", false
}

// CheckPaths reports backticked repository references that name nothing.
func CheckPaths(root string) ([]Finding, error) {
	doc, err := contextDoc(root)
	if err != nil {
		return nil, err
	}
	names, extensions, err := rootEntries(root)
	if err != nil {
		return nil, err
	}

	findings := []Finding{}
	for _, match := range repoReference.FindAllStringSubmatch(doc, -1) {
		reference := match[1]
		top, hasSlash := reference, strings.Contains(reference, "/")
		if hasSlash {
			top, _, _ = strings.Cut(reference, "/")
		}
		if !hasSlash {
			// A root file is referenced with no slash, which makes it look
			// exactly like the config keys and event names CONTEXT.md also
			// backticks (triage.report, network.mode). Distinguish by
			// extension, taken from the repository's own root files, so a typo
			// in AGENTS.md is caught while triage.report is not mistaken for
			// a file.
			if _, isRootExtension := extensions[path.Ext(reference)]; !isRootExtension {
				continue
			}
		} else if _, isRepoReference := names[top]; !isRepoReference {
			continue
		}
		// A reference is a claim about repository content, so it has to stay
		// inside the repository: docs/../../../etc/passwd names a real file on
		// some machines and repository content on none.
		if reference != path.Clean(reference) || strings.HasPrefix(path.Clean(reference), "..") {
			findings = append(findings, Finding{Reference: reference, Detail: "does not name repository content"})
			continue
		}
		if _, statErr := os.Stat(filepath.Join(root, filepath.FromSlash(reference))); statErr != nil {
			findings = append(findings, Finding{Reference: reference, Detail: "does not exist"})
		}
	}
	return findings, nil
}

// rootEntries returns the repository's top-level names and the extensions of
// its root files, read from the repository rather than listed here so a new
// entry needs no edit to be covered.
//
// Hidden entries are included: .github holds tracked workflows that CONTEXT.md
// and AGENTS.md both cite, and skipping them would silently exempt exactly the
// paths most likely to be renamed.
func rootEntries(root string) (map[string]struct{}, map[string]struct{}, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, nil, fmt.Errorf("read repository root: %w", err)
	}
	names := map[string]struct{}{}
	extensions := map[string]struct{}{}
	for _, entry := range entries {
		if entry.Name() == ".git" {
			continue
		}
		names[entry.Name()] = struct{}{}
		if !entry.IsDir() {
			if extension := path.Ext(entry.Name()); extension != "" {
				extensions[extension] = struct{}{}
			}
		}
	}
	return names, extensions, nil
}

// CheckCodePointers reports glossary entries that point at code which is not
// there. Entries say "defined by forge.ReviewItemID" instead of restating the
// definition, so a rename that misses CONTEXT.md turns the entry into a
// confident lie.
func CheckCodePointers(root string) ([]Finding, error) {
	doc, err := contextDoc(root)
	if err != nil {
		return nil, err
	}
	byName, err := packagesByName(root)
	if err != nil {
		return nil, err
	}

	findings := []Finding{}
	for _, match := range codePointer.FindAllStringSubmatch(doc, -1) {
		pkg, ident := match[1], match[2]
		if _, external := ExternalPackageQualifiers[pkg]; external {
			continue
		}
		candidates := byName[pkg]
		switch len(candidates) {
		case 0:
			// Denying by default is the point. A mistyped qualifier such as
			// protcol.ParseTargetLabel is exactly the stale pointer this
			// exists to catch; skipping it would leave the guarantee unmet.
			findings = append(findings, Finding{
				Reference: pkg + "." + ident,
				Detail:    fmt.Sprintf("no package in this repository is named %q; fix the reference or add it to ExternalPackageQualifiers", pkg),
			})
		case 1:
			if _, ok := candidates[0].idents[ident]; !ok {
				findings = append(findings, Finding{
					Reference: pkg + "." + ident,
					Detail:    fmt.Sprintf("%s declares no such identifier", candidates[0].path),
				})
			}
		default:
			paths := make([]string, 0, len(candidates))
			for _, candidate := range candidates {
				paths = append(paths, candidate.path)
			}
			sort.Strings(paths)
			findings = append(findings, Finding{
				Reference: pkg + "." + ident,
				Detail:    fmt.Sprintf("%q is ambiguous across %s; name the path instead of the bare package", pkg, strings.Join(paths, " and ")),
			})
		}
	}
	return findings, nil
}

type declaredPackage struct {
	path   string
	idents map[string]struct{}
}

// packagesByName groups packages by declared name while keeping their paths
// distinct. Pooling identifiers by name alone would let a pointer resolve
// against the wrong package — internal/api and pkg/api are both named api —
// so an ambiguous qualifier is reported rather than quietly satisfied.
func packagesByName(root string) (map[string][]declaredPackage, error) {
	byDir := map[string]*declaredPackage{}
	nameByDir := map[string]string{}

	for _, top := range []string{"internal", "cmd", "pkg", "tools"} {
		start := filepath.Join(root, top)
		if _, err := os.Stat(start); err != nil {
			continue
		}
		err := filepath.WalkDir(start, func(p string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				// testdata holds fixtures, including deliberately fake
				// packages. Treating one as production authority would let a
				// typo in CONTEXT.md resolve against a fixture and pass.
				if entry.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			file, parseErr := parser.ParseFile(token.NewFileSet(), p, nil, parser.SkipObjectResolution)
			if parseErr != nil {
				return nil
			}
			dir := filepath.Dir(p)
			if byDir[dir] == nil {
				rel, relErr := filepath.Rel(root, dir)
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
			return nil, fmt.Errorf("walk %s: %w", top, err)
		}
	}

	out := map[string][]declaredPackage{}
	for dir, pkg := range byDir {
		out[nameByDir[dir]] = append(out[nameByDir[dir]], *pkg)
	}
	return out, nil
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
