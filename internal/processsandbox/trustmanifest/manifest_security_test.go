package trustmanifest

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNormalizeInputRejectsUnsafeExecutableRoots(t *testing.T) {
	fixture := newFixture(t)
	tests := map[string]struct {
		mutate func(map[string]string)
		want   string
	}{
		"relative path": {
			mutate: func(roots map[string]string) { roots["node"] = "relative-node" },
			want:   "must be absolute",
		},
		"non executable": {
			mutate: func(roots map[string]string) {
				path := filepath.Join(fixture.root, "not-executable")
				if err := os.WriteFile(path, []byte("not executable\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				roots["node"] = path
			},
			want: "not executable",
		},
		"reserved resolver name": {
			mutate: func(roots map[string]string) { roots[closureResolverRoot] = roots["node"] },
			want:   "reserved",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			roots := fixture.input().Roots
			test.mutate(roots)
			input := fixture.input()
			input.Roots = roots
			_, err := normalizeInput(input)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("normalizeInput() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestScriptInterpreterResolvesEnvAndChainedShebangs(t *testing.T) {
	root := t.TempDir()
	final := filepath.Join(root, "final")
	if err := os.WriteFile(final, []byte("fake interpreter\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	chain := filepath.Join(root, "chain")
	if err := os.WriteFile(chain, []byte("#!"+final+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	envScript := filepath.Join(root, "env-script")
	if err := os.WriteFile(envScript, []byte("#!/usr/bin/env chain\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	resolvedChain, err := filepath.EvalSymlinks(chain)
	if err != nil {
		t.Fatal(err)
	}
	resolvedFinal, err := filepath.EvalSymlinks(final)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(envScript)
	if err != nil {
		t.Fatal(err)
	}
	resolved, ok, err := scriptInterpreterBytes(envScript, raw, []string{root})
	if err != nil || !ok || resolved != resolvedChain {
		t.Fatalf("scriptInterpreterBytes() = (%q, %v, %v), want chain path", resolved, ok, err)
	}

	collector := closureCollector{
		entries:             make(map[string]Entry),
		visitedELF:          make(map[string]struct{}),
		visitedInterpreters: make(map[string]struct{}),
		interpreterPaths:    []string{root},
		visitedMachO:        make(map[string]struct{}),
	}
	if err := collector.addInterpreterClosure(resolved); err != nil {
		t.Fatalf("addInterpreterClosure() error = %v", err)
	}
	if _, ok := collector.entries[resolvedChain]; !ok {
		t.Fatalf("collector entries = %#v, want chained interpreter", collector.entries)
	}
	if _, ok := collector.entries[resolvedFinal]; !ok {
		t.Fatalf("collector entries = %#v, want final interpreter", collector.entries)
	}
}

func TestCompareManifestsRejectsClosureEntryDrift(t *testing.T) {
	fixture := newFixture(t)
	sealed, err := Build(fixture.input())
	if err != nil {
		t.Fatal(err)
	}
	current := sealed
	current.Entries = append([]Entry(nil), sealed.Entries...)
	current.Entries[0].SHA256 = strings.Repeat("0", 64)
	if err := compareManifests(sealed, current); err == nil || !strings.Contains(err.Error(), "executable closure changed") {
		t.Fatalf("compareManifests() error = %v, want closure entry drift", err)
	}
}

func TestParseLDDOutputRejectsUnresolvedRelativeCandidates(t *testing.T) {
	tests := map[string]string{
		"malformed mapping":   "libfoo.so =>\n",
		"relative resolution": "libfoo.so => ./libfoo.so (0x0001)\n",
	}
	for name, output := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := parseLDDOutput(output)
			if err == nil {
				t.Fatal("parseLDDOutput() accepted unsafe output")
			}
		})
	}
	if got, err := parseLDDOutput("statically linked\n"); err != nil || len(got) != 0 {
		t.Fatalf("parseLDDOutput(static) = (%#v, %v), want empty closure", got, err)
	}
}

func TestELFInterpreterReadsPTInterp(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("fixture requires a Linux ELF executable")
	}
	interpreter, ok, err := elfInterpreter("/bin/sh")
	if err != nil || !ok || !filepath.IsAbs(interpreter) {
		t.Fatalf("elfInterpreter(/bin/sh) = (%q, %v, %v), want absolute PT_INTERP", interpreter, ok, err)
	}
}
