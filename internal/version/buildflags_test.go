package version

import (
	"bytes"
	"errors"
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

// errNotAGitRepo is reported by trackedGoFiles when root is not inside a Git
// worktree (for example an exported source archive). The module path migration
// guard is repository hygiene rather than package behavior, so the test skips
// cleanly in that case instead of requiring VCS metadata.
var errNotAGitRepo = errors.New("not a git repository")

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
	if errors.Is(err, errNotAGitRepo) {
		t.Skipf("not a git repository; skipping module path migration guard: %v", err)
	}
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

// trackedGoFiles returns the repository's Git-tracked *.go files that are
// present in this checkout's working tree, relative to root. Using
// `git ls-files` as the scan authority keeps the migration guard focused on
// this checkout's own source: sibling worktrees (`.worktrees`,
// `.claude/worktrees`, ...) are separate checkouts that may legitimately sit
// at older revisions and are never listed by `git ls-files` run here. The test
// therefore does not walk the filesystem or mutate those worktrees.
//
// `git ls-files` lists index entries regardless of working-tree presence: an
// unstaged deletion keeps the cached entry, and a skip-worktree file may or
// may not be present. The status tags (H cached, S skip-worktree) are index
// flags, not presence indicators, so each candidate is statted to keep only
// files the parser can actually open rather than trusting the tags.
//
// The Git index is read via os.ReadFile so Go's test result cache observes it
// as an input. `git ls-files` runs in a child process, so without this read
// staging a new tracked Go file would not invalidate a cached pass; tying the
// cache key to the index contents keeps the documented test command from
// silently reusing a stale result.
func trackedGoFiles(root string) ([]string, error) {
	gitDir, err := resolveGitDir(root)
	if err != nil {
		return nil, err
	}
	if _, err := os.ReadFile(filepath.Join(gitDir, "index")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read git index: %w", err)
	}

	cmd := exec.Command("git", "-C", root, "ls-files", "-z", "--", "*.go")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %v: %s", err, stderr.String())
	}
	var files []string
	for _, entry := range strings.Split(string(out), "\x00") {
		if entry == "" {
			continue
		}
		// `git ls-files` lists cached paths even when the working-tree file is
		// gone (unstaged deletion) or carries the skip-worktree bit. Stat the
		// path so the scanner only hands parseable, on-disk files to the
		// parser; a missing file is skipped, any other stat error is surfaced.
		if _, err := os.Stat(filepath.Join(root, entry)); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("stat tracked go file %s: %w", entry, err)
		}
		files = append(files, entry)
	}
	return files, nil
}

// resolveGitDir returns the absolute path to root's Git directory, or
// errNotAGitRepo when root is not inside a Git worktree (for example an
// exported source archive produced by `git archive`). errNotAGitRepo is
// returned only after git actually ran and reported root is outside a
// worktree; if the git executable cannot be started (absent from PATH, not
// executable, ...) the execution failure is preserved so the caller fails the
// guard loud instead of silently skipping it.
func resolveGitDir(root string) (string, error) {
	cmd := exec.Command("git", "-C", root, "rev-parse", "--absolute-git-dir")
	cmd.Stderr = nil
	out, err := cmd.Output()
	if err != nil {
		var execErr *exec.Error
		if errors.As(err, &execErr) {
			return "", fmt.Errorf("git rev-parse --absolute-git-dir: %w", err)
		}
		return "", errNotAGitRepo
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" {
		return "", errNotAGitRepo
	}
	return filepath.Clean(dir), nil
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
		{"commit", "--no-gpg-sign", "-m", "seed"},
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
		{"commit", "--no-gpg-sign", "-m", "stale legacy import"},
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

// TestTrackedGoFilesSkipsSparseCheckoutAbsent verifies that a tracked Go file
// omitted by a sparse checkout (skip-worktree bit set, absent from the working
// tree) is not listed, so the scanner never tries to parse a nonexistent path.
func TestTrackedGoFilesSkipsSparseCheckoutAbsent(t *testing.T) {
	root := t.TempDir()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "test"},
		{"config", "core.sparseCheckout", "true"},
	} {
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Skipf("git %v unavailable in this environment: %v %s", args, err, stderr.String())
		}
	}

	presentRel := filepath.Join("cmd", "looperd", "main.go")
	absentRel := filepath.Join("tools", "print-version-json", "main.go")
	for _, rel := range []string{presentRel, absentRel} {
		if err := os.MkdirAll(filepath.Join(root, filepath.Dir(rel)), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(filepath.Join(root, rel), []byte("package main\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module github.com/MumuTW/looper\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	for _, args := range [][]string{
		{"add", presentRel, absentRel, "go.mod"},
		{"commit", "--no-gpg-sign", "-m", "seed"},
	} {
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, stderr.String())
		}
	}

	// Restrict the working tree to cmd/ only; tools/ becomes skip-worktree and
	// its file is removed from the working tree.
	if err := os.WriteFile(filepath.Join(root, ".git", "info", "sparse-checkout"), []byte("/cmd/\n"), 0o644); err != nil {
		t.Fatalf("write sparse-checkout: %v", err)
	}
	read := exec.Command("git", "-C", root, "sparse-checkout", "reapply")
	var reapplyStderr bytes.Buffer
	read.Stderr = &reapplyStderr
	if err := read.Run(); err != nil {
		t.Skipf("git sparse-checkout reapply unavailable: %v %s", err, reapplyStderr.String())
	}
	if _, err := os.Stat(filepath.Join(root, absentRel)); !os.IsNotExist(err) {
		t.Skipf("sparse-checkout did not remove %s in this git version: %v", absentRel, err)
	}

	files, err := trackedGoFiles(root)
	if err != nil {
		t.Fatalf("trackedGoFiles: %v", err)
	}
	wantPresent := filepath.ToSlash(presentRel)
	wantAbsent := filepath.ToSlash(absentRel)
	foundPresent, foundAbsent := false, false
	for _, f := range files {
		slash := filepath.ToSlash(f)
		if slash == wantPresent {
			foundPresent = true
		}
		if slash == wantAbsent {
			foundAbsent = true
		}
	}
	if !foundPresent {
		t.Fatalf("trackedGoFiles = %v, want present sparse file %q listed", files, wantPresent)
	}
	if foundAbsent {
		t.Fatalf("trackedGoFiles = %v, want absent sparse file %q excluded (skip-worktree)", files, wantAbsent)
	}
}

// TestTrackedGoFilesExcludesUnstagedDeletion verifies that a tracked Go file
// deleted from the working tree but still in the index (an unstaged deletion,
// which `git ls-files` still lists as cached) is excluded, so the scanner does
// not hand a nonexistent path to the parser.
func TestTrackedGoFilesExcludesUnstagedDeletion(t *testing.T) {
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

	presentRel := filepath.Join("cmd", "looperd", "main.go")
	deletedRel := filepath.Join("tools", "print-version-json", "main.go")
	for _, rel := range []string{presentRel, deletedRel} {
		if err := os.MkdirAll(filepath.Join(root, filepath.Dir(rel)), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(filepath.Join(root, rel), []byte("package main\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module github.com/MumuTW/looper\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	for _, args := range [][]string{
		{"add", presentRel, deletedRel, "go.mod"},
		{"commit", "--no-gpg-sign", "-m", "seed"},
	} {
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, stderr.String())
		}
	}

	// Unstaged deletion: the file is gone from the working tree but still
	// cached in the index, so `git ls-files` would list it without a presence
	// check.
	if err := os.Remove(filepath.Join(root, deletedRel)); err != nil {
		t.Fatalf("remove %s: %v", deletedRel, err)
	}

	files, err := trackedGoFiles(root)
	if err != nil {
		t.Fatalf("trackedGoFiles: %v", err)
	}
	wantPresent := filepath.ToSlash(presentRel)
	wantDeleted := filepath.ToSlash(deletedRel)
	foundPresent, foundDeleted := false, false
	for _, f := range files {
		slash := filepath.ToSlash(f)
		if slash == wantPresent {
			foundPresent = true
		}
		if slash == wantDeleted {
			foundDeleted = true
		}
	}
	if !foundPresent {
		t.Fatalf("trackedGoFiles = %v, want present file %q listed", files, wantPresent)
	}
	if foundDeleted {
		t.Fatalf("trackedGoFiles = %v, want unstaged-deleted file %q excluded", files, wantDeleted)
	}
}

// TestTrackedGoFilesIncludesPresentSkipWorktree verifies that a tracked Go
// file carrying the skip-worktree bit but still present on disk is listed, so
// the scanner does not silently omit a real source file based on the index
// flag alone.
func TestTrackedGoFilesIncludesPresentSkipWorktree(t *testing.T) {
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

	normalRel := filepath.Join("cmd", "looperd", "main.go")
	skipRel := filepath.Join("tools", "print-version-json", "main.go")
	for _, rel := range []string{normalRel, skipRel} {
		if err := os.MkdirAll(filepath.Join(root, filepath.Dir(rel)), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(filepath.Join(root, rel), []byte("package main\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module github.com/MumuTW/looper\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	for _, args := range [][]string{
		{"add", normalRel, skipRel, "go.mod"},
		{"commit", "--no-gpg-sign", "-m", "seed"},
	} {
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, stderr.String())
		}
	}

	// Mark skipRel skip-worktree but leave the file on disk; `git ls-files -t`
	// would tag it S, but it is a real, parseable source file.
	skip := exec.Command("git", "-C", root, "update-index", "--skip-worktree", filepath.ToSlash(skipRel))
	var skipStderr bytes.Buffer
	skip.Stderr = &skipStderr
	if err := skip.Run(); err != nil {
		t.Skipf("git update-index --skip-worktree unavailable: %v %s", err, skipStderr.String())
	}
	if _, err := os.Stat(filepath.Join(root, skipRel)); err != nil {
		t.Fatalf("skip-worktree file %s must remain present: %v", skipRel, err)
	}

	files, err := trackedGoFiles(root)
	if err != nil {
		t.Fatalf("trackedGoFiles: %v", err)
	}
	wantNormal := filepath.ToSlash(normalRel)
	wantSkip := filepath.ToSlash(skipRel)
	foundNormal, foundSkip := false, false
	for _, f := range files {
		slash := filepath.ToSlash(f)
		if slash == wantNormal {
			foundNormal = true
		}
		if slash == wantSkip {
			foundSkip = true
		}
	}
	if !foundNormal {
		t.Fatalf("trackedGoFiles = %v, want normal file %q listed", files, wantNormal)
	}
	if !foundSkip {
		t.Fatalf("trackedGoFiles = %v, want present skip-worktree file %q listed", files, wantSkip)
	}
}

// TestTrackedGoFilesPreservesGitExecFailure verifies that when root is a real
// checkout but the git executable cannot be started, the execution failure is
// preserved instead of being masked as errNotAGitRepo, so the migration guard
// fails loud rather than silently skipping.
func TestTrackedGoFilesPreservesGitExecFailure(t *testing.T) {
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
	if err := os.MkdirAll(filepath.Join(root, filepath.Dir(trackedRel)), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, trackedRel), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("write tracked file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module github.com/MumuTW/looper\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	for _, args := range [][]string{
		{"add", trackedRel, "go.mod"},
		{"commit", "--no-gpg-sign", "-m", "seed"},
	} {
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, stderr.String())
		}
	}

	// Make git unstartable for child processes and confirm the guard does not
	// collapse the execution failure into errNotAGitRepo.
	t.Setenv("PATH", "")
	if _, err := exec.LookPath("git"); err == nil {
		t.Skip("git still resolvable with empty PATH; cannot simulate exec failure")
	}
	_, err := trackedGoFiles(root)
	if errors.Is(err, errNotAGitRepo) {
		t.Fatalf("trackedGoFiles masked a git exec failure as errNotAGitRepo; execution failures must be preserved: %v", err)
	}
	if err == nil {
		t.Fatalf("trackedGoFiles unexpectedly succeeded with git unavailable")
	}
}

// TestTrackedGoFilesNotARepo verifies the scan authority fails closed with
// errNotAGitRepo when root is genuinely outside a Git worktree (git ran and
// reported as much), so the migration guard skips cleanly instead of requiring
// VCS metadata. If git itself is unavailable the case is undecidable and skipped.
func TestTrackedGoFilesNotARepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git unavailable; cannot determine non-repo behavior: %v", err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module github.com/MumuTW/looper\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if _, err := trackedGoFiles(root); !errors.Is(err, errNotAGitRepo) {
		t.Fatalf("trackedGoFiles on non-repo = %v, want errNotAGitRepo", err)
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
