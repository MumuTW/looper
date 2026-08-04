package trustmanifest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestBuildAndVerifyRecordedContentAgree(t *testing.T) {
	fixture := newFixture(t)

	sealed, err := Build(fixture.input())
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	current, err := Build(fixture.input())
	if err != nil {
		t.Fatalf("Build(current) error = %v", err)
	}
	if err := verifyRecordedContent(sealed); err != nil {
		t.Fatalf("verifyRecordedContent() error = %v", err)
	}
	if err := compareManifests(sealed, current); err != nil {
		t.Fatalf("compareManifests() error = %v", err)
	}
	if !reflect.DeepEqual(sealed, current) {
		t.Fatalf("shared resolver produced different manifests")
	}
}

func TestBuildSealsTransitivePackageTree(t *testing.T) {
	fixture := newFixture(t)
	dependency := filepath.Join(fixture.packageRoot, "commander", "index.js")
	if err := os.MkdirAll(filepath.Dir(dependency), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dependency, []byte("module.exports = {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	manifest, err := Build(fixture.input())
	if err != nil {
		t.Fatal(err)
	}
	dependency, err = filepath.EvalSymlinks(dependency)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range manifest.Entries {
		if entry.Path == dependency && entry.Kind == EntryTreeFile {
			return
		}
	}
	t.Fatalf("transitive package file %s was not sealed", dependency)
}

func TestVerifyRecordedContentRejectsSubstitution(t *testing.T) {
	fixture := newFixture(t)
	sealed, err := Build(fixture.input())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.roots["node"], []byte("substituted\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	err = verifyRecordedContent(sealed)
	if err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("verifyRecordedContent() error = %v, want digest mismatch", err)
	}
}

func TestCompareManifestsRejectsExecutableRootDrift(t *testing.T) {
	fixture := newFixture(t)
	sealed, err := Build(fixture.input())
	if err != nil {
		t.Fatal(err)
	}
	extra := fixture.writeScript("extra", fixture.interpreter)
	fixture.roots["extra"] = extra
	current, err := Build(fixture.input())
	if err != nil {
		t.Fatal(err)
	}

	err = compareManifests(sealed, current)
	if err == nil || !strings.Contains(err.Error(), "executable roots changed") {
		t.Fatalf("compareManifests() error = %v, want root drift", err)
	}
}

func TestVerifyInputRootsRejectsRetargetBeforeClosureResolution(t *testing.T) {
	fixture := newFixture(t)
	sealed, err := Build(fixture.input())
	if err != nil {
		t.Fatal(err)
	}
	fixture.roots["node"] = fixture.writeScript("replacement-node", fixture.interpreter)

	err = verifyInputRoots(sealed, fixture.input())
	if err == nil || !strings.Contains(err.Error(), "executable roots changed") {
		t.Fatalf("verifyInputRoots() error = %v, want root drift", err)
	}
}

func TestVerifyRecordedContentRejectsSymlinkRetarget(t *testing.T) {
	fixture := newFixture(t)
	first := filepath.Join(fixture.packageRoot, "first.js")
	second := filepath.Join(fixture.packageRoot, "second.js")
	if err := os.WriteFile(first, []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(fixture.packageRoot, "current.js")
	if err := os.Symlink("first.js", link); err != nil {
		t.Fatal(err)
	}
	sealed, err := Build(fixture.input())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("second.js", link); err != nil {
		t.Fatal(err)
	}

	err = verifyRecordedContent(sealed)
	if err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("verifyRecordedContent() error = %v, want symlink digest mismatch", err)
	}
}

func TestBuildRejectsPackageSymlinkEscape(t *testing.T) {
	fixture := newFixture(t)
	outside := filepath.Join(filepath.Dir(fixture.packageRoot), "outside")
	if err := os.WriteFile(outside, []byte("outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(fixture.packageRoot, "escape")); err != nil {
		t.Fatal(err)
	}

	_, err := Build(fixture.input())
	if err == nil || !strings.Contains(err.Error(), "resolves outside") {
		t.Fatalf("Build() error = %v, want symlink escape rejection", err)
	}
}

func TestLoadRootSealedRejectsNonRootOwner(t *testing.T) {
	fixture := newFixture(t)
	manifest, err := Build(fixture.input())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), ManifestFileName)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		t.Skip("test requires a non-root process")
	}

	_, err = loadRootSealed(path)
	if err == nil || !strings.Contains(err.Error(), "not owned by root") {
		t.Fatalf("loadRootSealed() error = %v, want owner rejection", err)
	}
}

func TestLoadRootSealedRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.json")
	if err := os.WriteFile(target, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, ManifestFileName)
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	if _, err := loadRootSealed(link); err == nil {
		t.Fatal("loadRootSealed() accepted a symlink")
	}
}

func TestManifestPathSitsOutsideSealedNodeModulesTree(t *testing.T) {
	moduleRoot := filepath.Join(string(filepath.Separator), "opt", "runtime", "lib", "node_modules")
	want := filepath.Join(string(filepath.Separator), "opt", "runtime", ManifestFileName)
	if got := ManifestPath(moduleRoot); got != want {
		t.Fatalf("ManifestPath() = %q, want %q", got, want)
	}
}

func TestSyncManifestDirectory(t *testing.T) {
	if err := syncManifestDirectory(t.TempDir()); err != nil {
		t.Fatalf("syncManifestDirectory() error = %v", err)
	}
}

func TestParseLDDOutputIncludesLibrariesAndInterpreter(t *testing.T) {
	root := t.TempDir()
	lib := filepath.Join(root, "libc.so.6")
	loader := filepath.Join(root, "ld-linux-x86-64.so.2")
	for _, path := range []string{lib, loader} {
		if err := os.WriteFile(path, []byte("elf fixture\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	output := "linux-vdso.so.1 (0x0000)\nlibc.so.6 => " + lib + " (0x0001)\n" + loader + " (0x0002)\n"

	got, err := parseLDDOutput(output)
	if err != nil {
		t.Fatalf("parseLDDOutput() error = %v", err)
	}
	resolvedLoader, err := filepath.EvalSymlinks(loader)
	if err != nil {
		t.Fatal(err)
	}
	resolvedLib, err := filepath.EvalSymlinks(lib)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{resolvedLoader, resolvedLib}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseLDDOutput() = %#v, want %#v", got, want)
	}
}

func TestParseLDDOutputRejectsMissingDependency(t *testing.T) {
	_, err := parseLDDOutput("libmissing.so => not found\n")
	if err == nil || !strings.Contains(err.Error(), "unresolved ELF dependency") {
		t.Fatalf("parseLDDOutput() error = %v, want missing dependency", err)
	}
}

type fixture struct {
	t           *testing.T
	root        string
	packageRoot string
	interpreter string
	roots       map[string]string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	packageRoot := filepath.Join(root, "node_modules")
	if err := os.MkdirAll(packageRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	interpreter := filepath.Join(root, "interpreter")
	if err := os.WriteFile(interpreter, []byte("fake interpreter\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, root: root, packageRoot: packageRoot, interpreter: interpreter, roots: make(map[string]string)}
	f.roots["srt"] = f.writeScript(filepath.Join("node_modules", "@anthropic-ai", "sandbox-runtime", "srt"), interpreter)
	for _, name := range []string{"node", "rg"} {
		f.roots[name] = f.writeScript(name, interpreter)
	}
	return f
}

func (f *fixture) input() Input {
	roots := make(map[string]string, len(f.roots))
	for name, path := range f.roots {
		roots[name] = path
	}
	return Input{PackageRoot: f.packageRoot, Roots: roots}
}

func (f *fixture) writeScript(relative, interpreter string) string {
	f.t.Helper()
	path := filepath.Join(f.root, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!"+interpreter+"\nexit 0\n"), 0o755); err != nil {
		f.t.Fatal(err)
	}
	return path
}
