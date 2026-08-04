package trustmanifest

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
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
		"relative launch PATH": {
			mutate: func(roots map[string]string) {},
			want:   "launch PATH entry",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			roots := fixture.input().Roots
			test.mutate(roots)
			input := fixture.input()
			input.Roots = roots
			if name == "relative launch PATH" {
				input.LaunchPath = []string{"relative-bin"}
			}
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
	resolved, _, ok, err := resolveScriptInterpreter(envScript, raw, []string{root})
	if err != nil || !ok || resolved != resolvedChain {
		t.Fatalf("resolveScriptInterpreter() = (%q, %v, %v), want chain path", resolved, ok, err)
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

func TestScriptInterpreterRejectsRelativeDirectShebang(t *testing.T) {
	path := filepath.Join(t.TempDir(), "script")
	resolved, _, ok, err := resolveScriptInterpreter(path, []byte("#!node\n"), nil)
	if err == nil || ok || !strings.Contains(err.Error(), "non-absolute direct interpreter") {
		t.Fatalf("resolveScriptInterpreter() = (%q, %v, %v), want direct relative rejection", resolved, ok, err)
	}
}

func TestScriptInterpreterRejectsNonCanonicalEnvPath(t *testing.T) {
	root := t.TempDir()
	envPath := filepath.Join(root, "env")
	if err := os.WriteFile(envPath, []byte("env\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	nodePath := filepath.Join(root, "node")
	if err := os.WriteFile(nodePath, []byte("node\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	resolved, _, ok, err := resolveScriptInterpreter(filepath.Join(root, "script"), []byte("#!"+envPath+" node\n"), []string{root})
	if err == nil || ok || !strings.Contains(err.Error(), "unsupported env interpreter path") {
		t.Fatalf("resolveScriptInterpreter(non-canonical env) = (%q, %v, %v), want path rejection", resolved, ok, err)
	}
}

func TestResolveScriptInterpreterReturnsCanonicalEnvClosure(t *testing.T) {
	root := t.TempDir()
	nodePath := filepath.Join(root, "node")
	if err := os.WriteFile(nodePath, []byte("node\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	envPath := "/usr/bin/env"
	if _, err := os.Stat(envPath); err != nil {
		t.Skip("canonical env is unavailable")
	}
	resolved, env, ok, err := resolveScriptInterpreter(filepath.Join(root, "script"), []byte("#!/usr/bin/env node\n"), []string{root})
	if err != nil || !ok {
		t.Fatalf("resolveScriptInterpreter() = (%q, %q, %v, %v), want canonical env resolution", resolved, env, ok, err)
	}
	wantNode, err := filepath.EvalSymlinks(nodePath)
	if err != nil {
		t.Fatal(err)
	}
	wantEnv, err := filepath.EvalSymlinks(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != wantNode || env != wantEnv {
		t.Fatalf("resolveScriptInterpreter() = (%q, %q), want (%q, %q)", resolved, env, wantNode, wantEnv)
	}
}

func TestVerifyRecordedContentRejectsModeDrift(t *testing.T) {
	fixture := newFixture(t)
	sealed, err := Build(fixture.input())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(fixture.roots["node"], 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyRecordedContent(sealed); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("verifyRecordedContent() error = %v, want mode mismatch", err)
	}
}

func TestDigestEntryRejectsSpecialFile(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("fixture requires a Unix FIFO")
	}
	path := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := digestEntryMetadata(path, EntryTreeFile, -1); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("digestEntryMetadata() error = %v, want special-file rejection", err)
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

func TestResolveMachOLibraryRejectsCWDRelativeNames(t *testing.T) {
	for _, library := range []string{"libfoo.dylib", "./libfoo.dylib", "../libfoo.dylib"} {
		t.Run(library, func(t *testing.T) {
			_, err := resolveMachOLibraryFrom("/opt/runtime/bin/tool", "/opt/runtime/bin/tool", library, nil)
			if err == nil || !strings.Contains(err.Error(), "unsupported relative library dependency") {
				t.Fatalf("resolveMachOLibraryFrom(%q) error = %v, want relative dependency rejection", library, err)
			}
		})
	}
}

func TestParseResolverCredentialRequiresUnprivilegedIdentity(t *testing.T) {
	if credential, ok := parseResolverCredential(0, "501", "20"); !ok || credential.Uid != 501 || credential.Gid != 20 {
		t.Fatalf("parseResolverCredential(valid) = (%#v, %t), want uid/gid 501/20", credential, ok)
	}
	for _, test := range []struct {
		name string
		uid  string
		gid  string
	}{
		{name: "missing", uid: "", gid: ""},
		{name: "root uid", uid: "0", gid: "20"},
		{name: "root gid", uid: "501", gid: "0"},
		{name: "malformed", uid: "daemon", gid: "20"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if credential, ok := parseResolverCredential(0, test.uid, test.gid); ok || credential != nil {
				t.Fatalf("parseResolverCredential(%q,%q) = (%#v, %t), want no credential", test.uid, test.gid, credential, ok)
			}
		})
	}
	if credential, ok := parseResolverCredential(1000, "501", "20"); ok || credential != nil {
		t.Fatalf("parseResolverCredential(non-root) = (%#v, %t), want no credential", credential, ok)
	}
}

func TestRequireUnprivilegedResolverFailsClosedForRoot(t *testing.T) {
	if err := requireUnprivilegedResolver(0, false); err == nil || !strings.Contains(err.Error(), "refusing to execute ELF interpreter as root") {
		t.Fatalf("requireUnprivilegedResolver(root, false) = %v, want root refusal", err)
	}
	if err := requireUnprivilegedResolver(0, true); err != nil {
		t.Fatalf("requireUnprivilegedResolver(root, true) = %v, want nil", err)
	}
	if err := requireUnprivilegedResolver(1000, false); err != nil {
		t.Fatalf("requireUnprivilegedResolver(non-root, false) = %v, want nil", err)
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

func TestMachODependenciesSupportsDarwinSystemLibraries(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("fixture requires a Darwin Mach-O executable")
	}
	if _, err := machoDependenciesForExecutable("/bin/sh", "/bin/sh"); err != nil {
		t.Fatalf("machoDependencies(/bin/sh) error = %v", err)
	}
}
