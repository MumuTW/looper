package version

import (
	"bytes"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestDefaultBuildOverrides(t *testing.T) {
	overrides := DefaultBuildOverrides()

	if overrides.Version != defaultVersion {
		t.Fatalf("DefaultBuildOverrides().Version = %q, want %q", overrides.Version, defaultVersion)
	}

	if overrides.VersionSource != defaultVersionSource {
		t.Fatalf("DefaultBuildOverrides().VersionSource = %q, want %q", overrides.VersionSource, defaultVersionSource)
	}

	if overrides.Channel != defaultChannel {
		t.Fatalf("DefaultBuildOverrides().Channel = %q, want %q", overrides.Channel, defaultChannel)
	}

	if overrides.APIVersion != defaultAPIVersion {
		t.Fatalf("DefaultBuildOverrides().APIVersion = %q, want %q", overrides.APIVersion, defaultAPIVersion)
	}

	if overrides.GitCommitSHA != "" {
		t.Fatalf("DefaultBuildOverrides().GitCommitSHA = %q, want empty string", overrides.GitCommitSHA)
	}

	if overrides.BuildTimestamp != "" {
		t.Fatalf("DefaultBuildOverrides().BuildTimestamp = %q, want empty string", overrides.BuildTimestamp)
	}
	if overrides.Dirty != nil {
		t.Fatalf("DefaultBuildOverrides().Dirty = %v, want nil", overrides.Dirty)
	}
}

func TestBuildOverridesFromEnvUsesOptionalBuildMetadata(t *testing.T) {
	overrides := BuildOverridesFromEnv(func(name string) string {
		switch name {
		case buildVersionEnvVar:
			return "  1.2.3  "
		case buildVersionSourceEnvVar:
			return "  git-tag:v1.2.3  "
		case buildChannelEnvVar:
			return "  stable  "
		case buildAPIVersionEnvVar:
			return "  v1  "
		case buildGitSHAEnvVar:
			return "  abc123  "
		case buildTimestampEnvVar:
			return "  2026-04-17T00:00:00Z  "
		default:
			return ""
		}
	})

	if overrides.Version != "1.2.3" {
		t.Fatalf("BuildOverridesFromEnv(...).Version = %q, want %q", overrides.Version, "1.2.3")
	}

	if overrides.VersionSource != "git-tag:v1.2.3" {
		t.Fatalf("BuildOverridesFromEnv(...).VersionSource = %q, want %q", overrides.VersionSource, "git-tag:v1.2.3")
	}

	if overrides.Channel != "stable" {
		t.Fatalf("BuildOverridesFromEnv(...).Channel = %q, want %q", overrides.Channel, "stable")
	}

	if overrides.APIVersion != "v1" {
		t.Fatalf("BuildOverridesFromEnv(...).APIVersion = %q, want %q", overrides.APIVersion, "v1")
	}

	if overrides.GitCommitSHA != "abc123" {
		t.Fatalf("BuildOverridesFromEnv(...).GitCommitSHA = %q, want %q", overrides.GitCommitSHA, "abc123")
	}

	if overrides.BuildTimestamp != "2026-04-17T00:00:00Z" {
		t.Fatalf("BuildOverridesFromEnv(...).BuildTimestamp = %q, want %q", overrides.BuildTimestamp, "2026-04-17T00:00:00Z")
	}
}

func TestLDFlagsMatchesVersionVariables(t *testing.T) {
	dirty := false
	ldflags := LDFlags(BuildOverrides{
		Version:        "1.2.3",
		VersionSource:  "internal/version/version.go",
		Channel:        "stable",
		APIVersion:     "v1",
		GitCommitSHA:   "abc123",
		BuildTimestamp: "2026-04-17T00:00:00Z",
		Dirty:          &dirty,
	})

	const want = "-X github.com/MumuTW/looper/internal/version.Value=1.2.3 -X github.com/MumuTW/looper/internal/version.VersionSource=internal/version/version.go -X github.com/MumuTW/looper/internal/version.Channel=stable -X github.com/MumuTW/looper/internal/version.APIVersion=v1 -X github.com/MumuTW/looper/internal/version.GitCommitSHA=abc123 -X github.com/MumuTW/looper/internal/version.BuildTimestamp=2026-04-17T00:00:00Z -X github.com/MumuTW/looper/internal/version.BuildDirty=false"
	if ldflags != want {
		t.Fatalf("LDFlags(...) = %q, want %q", ldflags, want)
	}
}

func TestModulePathMigration(t *testing.T) {
	const (
		modulePath       = "github.com/MumuTW/looper"
		legacyModulePath = "github.com/nexu-io/looper"
	)

	root := repoRoot(t)
	goMod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	if !strings.HasPrefix(string(goMod), "module "+modulePath+"\n") {
		t.Fatalf("go.mod module declaration = %q, want %q", strings.SplitN(string(goMod), "\n", 2)[0], "module "+modulePath)
	}

	goFiles, err := trackedGoFiles(root)
	if err != nil {
		t.Fatalf("list tracked Go files: %v", err)
	}

	violations, err := legacyModulePathViolations(root, goFiles, legacyModulePath)
	if err != nil {
		t.Fatalf("scan tracked Go source for legacy module path: %v", err)
	}
	for _, v := range violations {
		t.Errorf("%s still imports legacy module path %q", v.Path, v.LegacyPath)
	}
}

// trackedGoFiles returns the repository's Git-tracked *.go files, relative to
// root. Using `git ls-files` as the scan authority keeps the migration guard
// focused on this checkout's own source: sibling worktrees (`.worktrees`,
// `.claude/worktrees`, ...) are separate checkouts that may legitimately sit
// at older revisions and are never listed by `git ls-files` run here. The test
// therefore does not walk the filesystem or mutate those worktrees.
func trackedGoFiles(root string) ([]string, error) {
	cmd := exec.Command("git", "-C", root, "ls-files", "-z", "--", "*.go")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %v: %s", err, stderr.String())
	}
	var files []string
	for _, name := range strings.Split(string(out), "\x00") {
		if name == "" {
			continue
		}
		files = append(files, name)
	}
	return files, nil
}

type legacyModulePathViolation struct {
	Path       string
	LegacyPath string
}

// legacyModulePathViolations parses the given Go files (paths relative to root)
// and reports any that import legacyModulePath or one of its subpackages.
func legacyModulePathViolations(root string, relativeGoFiles []string, legacyModulePath string) ([]legacyModulePathViolation, error) {
	fset := token.NewFileSet()
	var violations []legacyModulePathViolation
	for _, rel := range relativeGoFiles {
		file, err := parser.ParseFile(fset, filepath.Join(root, rel), nil, parser.ImportsOnly)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", rel, err)
		}
		for _, imported := range file.Imports {
			importPath, err := strconv.Unquote(imported.Path.Value)
			if err != nil {
				return nil, fmt.Errorf("unquote import in %s: %w", rel, err)
			}
			if importPath == legacyModulePath || strings.HasPrefix(importPath, legacyModulePath+"/") {
				violations = append(violations, legacyModulePathViolation{Path: rel, LegacyPath: legacyModulePath})
			}
		}
	}
	return violations, nil
}

// TestLegacyModulePathViolationsDetectsTrackedImports verifies the scanner
// reports a legacy import in a tracked file by its relative path, while a
// legacy import that lives in a sibling worktree on disk is invisible because
// it is not part of the tracked file list handed to the scanner.
func TestLegacyModulePathViolationsDetectsTrackedImports(t *testing.T) {
	const legacyModulePath = "github.com/nexu-io/looper"
	root := t.TempDir()

	trackedRel := filepath.Join("internal", "version", "legacy.go")
	trackedPath := filepath.Join(root, trackedRel)
	if err := os.MkdirAll(filepath.Dir(trackedPath), 0o755); err != nil {
		t.Fatalf("mkdir tracked: %v", err)
	}
	if err := os.WriteFile(trackedPath, []byte("package version\n\nimport (\n\t\"github.com/nexu-io/looper/internal/foo\"\n)\n"), 0o644); err != nil {
		t.Fatalf("write tracked file: %v", err)
	}

	worktreeRel := filepath.Join(".worktrees", "stale", "cmd", "looperd", "main.go")
	worktreePath := filepath.Join(root, worktreeRel)
	if err := os.MkdirAll(filepath.Dir(worktreePath), 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	if err := os.WriteFile(worktreePath, []byte("package main\n\nimport (\n\t\"github.com/nexu-io/looper/internal/runtime\"\n)\n"), 0o644); err != nil {
		t.Fatalf("write worktree file: %v", err)
	}

	violations, err := legacyModulePathViolations(root, []string{trackedRel}, legacyModulePath)
	if err != nil {
		t.Fatalf("legacyModulePathViolations: %v", err)
	}
	if len(violations) != 1 || violations[0].Path != trackedRel {
		t.Fatalf("violations = %#v, want exactly one entry with Path %q", violations, trackedRel)
	}
}

// TestTrackedGoFilesExcludesWorktrees verifies the scan authority (git ls-files)
// lists only this checkout's tracked Go files and never files that live in a
// linked worktree, even when those files are tracked inside the worktree.
func TestTrackedGoFilesExcludesWorktrees(t *testing.T) {
	root := t.TempDir()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "test"},
	} {
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Skipf("git %v unavailable in this environment: %v %s", args, err, stderr.String())
		}
	}

	trackedRel := filepath.Join("cmd", "looperd", "main.go")
	trackedPath := filepath.Join(root, trackedRel)
	if err := os.MkdirAll(filepath.Dir(trackedPath), 0o755); err != nil {
		t.Fatalf("mkdir tracked: %v", err)
	}
	if err := os.WriteFile(trackedPath, []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("write tracked file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module github.com/MumuTW/looper\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	for _, args := range [][]string{
		{"add", trackedRel, "go.mod"},
		{"commit", "-m", "seed"},
	} {
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, stderr.String())
		}
	}

	worktreeRoot := filepath.Join(root, ".worktrees", "stale")
	worktreeAdd := exec.Command("git", "-C", root, "worktree", "add", worktreeRoot, "-b", "stale")
	var worktreeStderr bytes.Buffer
	worktreeAdd.Stderr = &worktreeStderr
	if err := worktreeAdd.Run(); err != nil {
		t.Skipf("git worktree add unavailable: %v %s", err, worktreeStderr.String())
	}

	worktreeGoRel := filepath.Join("internal", "runtime", "runtime.go")
	worktreeGoPath := filepath.Join(worktreeRoot, worktreeGoRel)
	if err := os.MkdirAll(filepath.Dir(worktreeGoPath), 0o755); err != nil {
		t.Fatalf("mkdir worktree file: %v", err)
	}
	if err := os.WriteFile(worktreeGoPath, []byte("package runtime\n\nimport (\n\t\"github.com/nexu-io/looper/internal/foo\"\n)\n"), 0o644); err != nil {
		t.Fatalf("write worktree file: %v", err)
	}
	for _, args := range [][]string{
		{"add", worktreeGoRel},
		{"commit", "-m", "stale legacy import"},
	} {
		cmd := exec.Command("git", append([]string{"-C", worktreeRoot}, args...)...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("git %v in worktree: %v %s", args, err, stderr.String())
		}
	}

	files, err := trackedGoFiles(root)
	if err != nil {
		t.Fatalf("trackedGoFiles: %v", err)
	}
	wantTracked := filepath.ToSlash(trackedRel)
	foundTracked := false
	for _, f := range files {
		if strings.Contains(filepath.ToSlash(f), ".worktrees") {
			t.Fatalf("trackedGoFiles listed worktree path %q; scan authority must be limited to this checkout", f)
		}
		if filepath.ToSlash(f) == wantTracked {
			foundTracked = true
		}
	}
	if !foundTracked {
		t.Fatalf("trackedGoFiles = %v, want %q listed", files, wantTracked)
	}
}

func TestLDFlagsInjectIntoBuiltBinary(t *testing.T) {
	dirty := false
	tempDir := t.TempDir()
	outputPath := filepath.Join(tempDir, "print-version")

	command := exec.Command(
		"go",
		"build",
		"-o",
		outputPath,
		"-ldflags",
		LDFlags(BuildOverrides{
			Version:        "9.9.9",
			VersionSource:  "internal/version/version.go",
			Channel:        "stable",
			APIVersion:     "v1",
			GitCommitSHA:   "deadbeef",
			BuildTimestamp: "2026-04-17T12:34:56Z",
			Dirty:          &dirty,
		}),
		"./tools/print-version-json",
	)
	command.Dir = repoRoot(t)
	command.Env = os.Environ()
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("go build test helper failed: %v\n%s", err, output)
	}

	runCommand := exec.Command(outputPath)
	runCommand.Env = os.Environ()
	runOutput, err := runCommand.CombinedOutput()
	if err != nil {
		t.Fatalf("running built helper failed: %v\n%s", err, runOutput)
	}

	got := strings.TrimSpace(string(runOutput))
	const want = `{"version":"9.9.9","metadata":{"versionSource":"internal/version/version.go","channel":"stable","apiVersion":"v1","gitCommitSha":"deadbeef","buildTimestamp":"2026-04-17T12:34:56Z","dirty":false}}`
	if got != want {
		t.Fatalf("built helper output = %s, want %s", got, want)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()

	workingDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd() error = %v", err)
	}

	return filepath.Clean(filepath.Join(workingDir, "..", ".."))
}
