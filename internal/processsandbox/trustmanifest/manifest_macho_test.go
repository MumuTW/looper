package trustmanifest

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestMachOClosurePropagatesExecutableRunpaths(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("fixture requires Darwin Mach-O tooling")
	}
	clang, err := exec.LookPath("clang")
	if err != nil {
		t.Skipf("clang is unavailable: %v", err)
	}
	root := t.TempDir()
	deps := filepath.Join(root, "deps")
	if err := os.MkdirAll(deps, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(path, source string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(source), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, "child.c"), "int child(void) { return 0; }\n")
	write(filepath.Join(root, "parent.c"), "extern int child(void); int parent(void) { return child(); }\n")
	write(filepath.Join(root, "main.c"), "extern int parent(void); int main(void) { return parent(); }\n")
	child := filepath.Join(deps, "libchild.dylib")
	parent := filepath.Join(root, "libparent.dylib")
	main := filepath.Join(root, "main")
	run := func(args ...string) {
		t.Helper()
		command := exec.Command(clang, args...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("%s %s: %v\n%s", clang, strings.Join(args, " "), err, output)
		}
	}
	run("-dynamiclib", filepath.Join(root, "child.c"), "-Wl,-install_name,@rpath/libchild.dylib", "-o", child)
	run("-dynamiclib", filepath.Join(root, "parent.c"), "-L"+deps, "-lchild", "-Wl,-install_name,@rpath/libparent.dylib", "-o", parent)
	run(filepath.Join(root, "main.c"), "-L"+root, "-lparent", "-Wl,-rpath,@executable_path/", "-Wl,-rpath,@executable_path/deps/", "-o", main)

	resolvedChild, err := resolveExistingPath(child)
	if err != nil {
		t.Fatal(err)
	}
	collector := closureCollector{
		entries:      make(map[string]Entry),
		visitedMachO: make(map[string]struct{}),
	}
	if err := collector.addMachOClosureFrom(main, main); err != nil {
		t.Fatalf("addMachOClosureFrom() error = %v", err)
	}
	if _, ok := collector.entries[resolvedChild]; !ok {
		t.Fatalf("entries = %#v, want inherited executable rpath to resolve %s", collector.entries, resolvedChild)
	}
}
