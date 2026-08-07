package trustmanifest

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestELFSharedObjectUsesLoadingExecutableContext(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("fixture requires a Linux ELF loader")
	}
	if os.Geteuid() == 0 && os.Getenv("SUDO_UID") == "" {
		t.Skip("closure resolver intentionally refuses root without an explicit daemon identity")
	}

	executable := "/bin/sh"
	if _, err := os.Stat(executable); err != nil {
		t.Skipf("Linux loader fixture is unavailable: %v", err)
	}
	executable, err := resolveExistingPath(executable)
	if err != nil {
		t.Skipf("resolve Linux loader fixture: %v", err)
	}
	if _, ok, err := elfInterpreter(executable); err != nil || !ok {
		t.Skipf("Linux executable has no usable PT_INTERP: %v", err)
	}

	shared := findELFSharedObject(t)
	if shared == "" {
		t.Skip("no ELF shared object with DT_NEEDED found in standard library paths")
	}
	dependencies, err := lddDependencies(shared, executable)
	if err != nil {
		t.Fatalf("lddDependencies(%s, %s) error = %v", shared, executable, err)
	}
	if len(dependencies) == 0 {
		t.Fatalf("lddDependencies(%s, %s) returned an empty closure", shared, executable)
	}
	if _, err := lddDependencies(shared); err == nil {
		t.Fatalf("lddDependencies(%s) accepted a shared object without loading context", shared)
	}

	collector := closureCollector{
		entries:             make(map[string]Entry),
		visitedELF:          make(map[string]struct{}),
		visitedInterpreters: make(map[string]struct{}),
		elfContextPath:      executable,
		visitedMachO:        make(map[string]struct{}),
	}
	if err := collector.addELFClosure(shared); err != nil {
		t.Fatalf("addELFClosure(%s) error = %v", shared, err)
	}
	for _, dependency := range dependencies {
		if _, ok := collector.entries[dependency]; !ok {
			t.Fatalf("shared-object dependency %s was not sealed; entries = %#v", dependency, collector.entries)
		}
	}
	interpreter, _, err := elfInterpreter(executable)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := collector.entries[interpreter]; !ok {
		t.Fatalf("loading executable interpreter %s was not sealed", interpreter)
	}
}

func findELFSharedObject(t *testing.T) string {
	t.Helper()
	for _, root := range []string{"/lib", "/lib64", "/usr/lib", "/usr/lib64"} {
		found := ""
		_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil || found != "" {
				return nil
			}
			if !entry.Type().IsRegular() {
				return nil
			}
			info, err := entry.Info()
			if err != nil || !info.Mode().IsRegular() {
				return nil
			}
			raw, err := os.ReadFile(path)
			if err != nil || !isELFBytes(raw) {
				return nil
			}
			interpreter, ok, err := elfInterpreter(path)
			if err != nil || ok || interpreter != "" {
				return nil
			}
			needed, err := elfNeededLibraries(path)
			if err == nil && len(needed) > 0 {
				found, _ = resolveExistingPath(path)
			}
			return nil
		})
		if found != "" {
			return found
		}
	}
	return ""
}
