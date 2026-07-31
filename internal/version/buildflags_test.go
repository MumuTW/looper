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
// silently reusing a stale result. resolveGitIndex returns Git's effective
// index path (honoring GIT_INDEX_FILE), so the read observes the same index
// `git ls-files` reads rather than a hard-coded `<gitDir>/index`.
func trackedGoFiles(root string) ([]string, error) {
	indexPath, err := resolveGitIndex(root)
	if err != nil {
		return nil, err
	}
	if _, err := os.ReadFile(indexPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read git index: %w", err)
	}

	cmd := gitReadCommand(root, "ls-files", "-z")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %v: %s", err, stderr.String())
	}
	var files []string
	for _, entry := range strings.Split(string(out), "\x00") {
		if entry == "" || !strings.HasSuffix(entry, ".go") {
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

// resolveGitIndex returns the absolute path to Git's effective index file for
// root, or errNotAGitRepo when root is not inside a Git worktree (for example
// an exported source archive produced by `git archive`).
//
// Repository membership is decided in two stages so an exported archive can
// skip without requiring the git executable:
//   - root is checked for a `.git` entry first (a directory for a primary
//     worktree, a file for a linked worktree's gitdir pointer). When no entry
//     exists, an explicit GIT_DIR/GIT_WORK_TREE/GIT_COMMON_DIR environment can
//     still select an external checkout, so Git is consulted in that case;
//     otherwise errNotAGitRepo is returned without starting git. The latter
//     lets the guard skip in minimal build environments where the Go toolchain
//     is present but git is not installed.
//   - When repository metadata or an explicit Git environment is present, Git
//     is the authority for the effective index
//     path. `git rev-parse --git-path index` resolves GIT_INDEX_FILE (and the
//     other index-selection environment), so the cache-tied read observes the
//     same index `git ls-files` reads rather than a hard-coded `<gitDir>/index`.
//
// errNotAGitRepo is returned only for the actual nonrepository response: no
// `.git` metadata, or git ran and reported root is outside a worktree. Any
// other git failure — the executable cannot be started, or git started but
// failed for an unrelated reason such as a malformed GIT_CONFIG_GLOBAL — is
// preserved so the caller fails the guard loud instead of silently skipping.
func resolveGitIndex(root string) (string, error) {
	if !hasGitRepoMetadata(root) && !gitEnvironmentSelectsRepository() {
		return "", errNotAGitRepo
	}
	cmd := gitReadCommand(root, "rev-parse", "--git-path", "index")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		var execErr *exec.Error
		if errors.As(err, &execErr) {
			return "", fmt.Errorf("git rev-parse --git-path index: %w", err)
		}
		if isNotAGitRepoMessage(stderr.String()) {
			return "", errNotAGitRepo
		}
		return "", fmt.Errorf("git rev-parse --git-path index: %v: %s", err, stderr.String())
	}
	p := strings.TrimSpace(string(out))
	if p == "" {
		return "", errNotAGitRepo
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(root, p)
	}
	return filepath.Clean(p), nil
}

// hasGitRepoMetadata reports whether root carries repository metadata, i.e. a
// `.git` entry (a directory for a primary worktree, a file for a linked
// worktree's gitdir pointer) at root itself. root is always a repository root
// (see repoRoot), so an ancestor walk is unnecessary; an exported source
// archive has no `.git` and is detected here without starting git. An explicit
// external Git environment is handled separately by gitEnvironmentSelectsRepository.
func hasGitRepoMetadata(root string) bool {
	_, err := os.Stat(filepath.Join(root, ".git"))
	return err == nil
}

func gitEnvironmentSelectsRepository() bool {
	for _, name := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR"} {
		if strings.TrimSpace(os.Getenv(name)) != "" {
			return true
		}
	}
	return false
}

// gitReadCommand scopes Git's read-only repository access to this checkout.
// The command-line safe.directory setting avoids requiring a global config
// mutation when a supported test checkout is mounted with a different UID.
func gitReadCommand(root string, args ...string) *exec.Cmd {
	gitArgs := []string{"-c", "safe.directory=" + root, "-C", root}
	gitArgs = append(gitArgs, args...)
	return exec.Command("git", gitArgs...)
}

// isNotAGitRepoMessage reports whether stderr is Git's canonical "not a git
// repository" diagnostic, the only response that means root is outside a
// worktree. Other failures (a malformed GIT_CONFIG_GLOBAL, ...) must not be
// collapsed into this sentinel.
func isNotAGitRepoMessage(stderr string) bool {
	const (
		canonical  = "fatal: not a git repository"
		withParent = canonical + " (or any of the parent directories): "
	)
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == canonical:
			return true
		case strings.HasPrefix(line, withParent) && strings.TrimSpace(strings.TrimPrefix(line, withParent)) != "":
			return true
		}
	}
	return false
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

func TestTrackedGoFilesIgnoresGitPathspecEnvironment(t *testing.T) {
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
	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, trackedRel)), 0o755); err != nil {
		t.Fatalf("mkdir tracked: %v", err)
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

	// These variables change the meaning of a pathspec. The scanner must not
	// pass a pathspec at all, so a tracked nested Go file remains visible.
	t.Setenv("GIT_LITERAL_PATHSPECS", "1")
	t.Setenv("GIT_GLOB_PATHSPECS", "1")
	files, err := trackedGoFiles(root)
	if err != nil {
		t.Fatalf("trackedGoFiles: %v", err)
	}
	want := filepath.ToSlash(trackedRel)
	for _, file := range files {
		if filepath.ToSlash(file) == want {
			return
		}
	}
	t.Fatalf("trackedGoFiles = %v, want %q with Git pathspec modes enabled", files, want)
}

func TestTrackedGoFilesRespectsExternalGitDirectory(t *testing.T) {
	root := t.TempDir()
	gitDir := t.TempDir()
	cleanEnv := withoutGitRepositoryEnvironment(os.Environ())
	initGit := exec.Command("git", "-C", gitDir, "init")
	initGit.Env = cleanEnv
	if output, err := initGit.CombinedOutput(); err != nil {
		t.Skipf("git init unavailable in this environment: %v\n%s", err, output)
	}
	for _, args := range [][]string{
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "test"},
	} {
		cmd := exec.Command("git", append([]string{"-C", gitDir}, args...)...)
		cmd.Env = cleanEnv
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
	}
	trackedRel := filepath.Join("cmd", "looperd", "main.go")
	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, trackedRel)), 0o755); err != nil {
		t.Fatalf("mkdir tracked: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, trackedRel), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("write tracked file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module github.com/MumuTW/looper\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	externalGitDir := filepath.Join(gitDir, ".git")
	env := append(cleanEnv, "GIT_DIR="+externalGitDir, "GIT_WORK_TREE="+root)
	for _, args := range [][]string{
		{"add", trackedRel, "go.mod"},
		{"commit", "--no-gpg-sign", "-m", "seed"},
	} {
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Env = env
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("external git %v: %v\n%s", args, err, output)
		}
	}
	t.Setenv("GIT_DIR", externalGitDir)
	t.Setenv("GIT_WORK_TREE", root)

	files, err := trackedGoFiles(root)
	if err != nil {
		t.Fatalf("trackedGoFiles with external Git directory: %v", err)
	}
	want := filepath.ToSlash(trackedRel)
	for _, file := range files {
		if filepath.ToSlash(file) == want {
			return
		}
	}
	t.Fatalf("trackedGoFiles = %v, want %q with external GIT_DIR/GIT_WORK_TREE", files, want)
}

func withoutGitRepositoryEnvironment(env []string) []string {
	var clean []string
	for _, entry := range env {
		name, _, ok := strings.Cut(entry, "=")
		switch name {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE":
			continue
		default:
			if ok {
				clean = append(clean, entry)
			}
		}
	}
	return clean
}

func TestIsNotAGitRepoMessageMatchesOnlyCanonicalDiagnostics(t *testing.T) {
	for _, stderr := range []string{
		"fatal: not a git repository\n",
		"fatal: not a git repository (or any of the parent directories): .git\n",
	} {
		if !isNotAGitRepoMessage(stderr) {
			t.Errorf("isNotAGitRepoMessage(%q) = false, want true", stderr)
		}
	}
	for _, stderr := range []string{
		"fatal: bad config line 1 in file /tmp/not a git repository/config\n",
		"fatal: not a git repository: custom wrapper\n",
	} {
		if isNotAGitRepoMessage(stderr) {
			t.Errorf("isNotAGitRepoMessage(%q) = true, want false", stderr)
		}
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
// errNotAGitRepo when root is genuinely outside a Git worktree (no `.git`
// metadata), so the migration guard skips cleanly instead of requiring VCS
// metadata. Membership is decided from `.git` metadata, so this passes without
// requiring the git executable to be installed.
func TestTrackedGoFilesNotARepo(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module github.com/MumuTW/looper\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if _, err := trackedGoFiles(root); !errors.Is(err, errNotAGitRepo) {
		t.Fatalf("trackedGoFiles on non-repo = %v, want errNotAGitRepo", err)
	}
}

// TestTrackedGoFilesSkipsArchiveWithoutGit verifies the combined archive and
// missing-git scenario: an exported source archive (no `.git`) tested in a
// minimal build environment where the Go toolchain is present but git is not
// installed skips via errNotAGitRepo instead of failing, because membership is
// decided from `.git` metadata before git is required.
func TestTrackedGoFilesSkipsArchiveWithoutGit(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module github.com/MumuTW/looper\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	t.Setenv("PATH", "")
	if _, err := exec.LookPath("git"); err == nil {
		t.Skip("git still resolvable with empty PATH; cannot simulate archive without git")
	}
	if _, err := trackedGoFiles(root); !errors.Is(err, errNotAGitRepo) {
		t.Fatalf("trackedGoFiles on archive without git = %v, want errNotAGitRepo", err)
	}
}

// TestTrackedGoFilesFailsLoudOnGitConfigError verifies that when root is a real
// checkout but git starts and fails for an unrelated reason (a malformed
// GIT_CONFIG_GLOBAL), the failure is preserved instead of being collapsed into
// errNotAGitRepo, so the migration guard fails loud rather than silently
// skipping. Only the actual "not a git repository" response is the sentinel.
func TestTrackedGoFilesFailsLoudOnGitConfigError(t *testing.T) {
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

	// A malformed global config makes `git rev-parse` report a config error
	// (exit 128) rather than "not a git repository"; the guard must preserve it.
	badConfig := filepath.Join(t.TempDir(), "badconfig")
	if err := os.WriteFile(badConfig, []byte("this is not = valid\n[[[[\n"), 0o644); err != nil {
		t.Fatalf("write bad config: %v", err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", badConfig)
	_, err := trackedGoFiles(root)
	if errors.Is(err, errNotAGitRepo) {
		t.Fatalf("trackedGoFiles masked a git config error as errNotAGitRepo; unexpected git failures must be preserved: %v", err)
	}
	if err == nil {
		t.Fatalf("trackedGoFiles unexpectedly succeeded with malformed git config")
	}
}

// TestResolveGitIndexRespectsGitIndexFile verifies resolveGitIndex returns
// Git's effective index path, honoring GIT_INDEX_FILE, so the cache-tied read
// observes the same index `git ls-files` reads rather than a hard-coded
// `<gitDir>/index`.
func TestResolveGitIndexRespectsGitIndexFile(t *testing.T) {
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

	altIndex := filepath.Join(t.TempDir(), "alt-index")
	if err := os.WriteFile(altIndex, []byte("alternate index\n"), 0o644); err != nil {
		t.Fatalf("write alt index: %v", err)
	}
	t.Setenv("GIT_INDEX_FILE", altIndex)

	got, err := resolveGitIndex(root)
	if err != nil {
		t.Fatalf("resolveGitIndex: %v", err)
	}
	if want := filepath.Clean(altIndex); got != want {
		t.Fatalf("resolveGitIndex = %q, want effective index %q (GIT_INDEX_FILE)", got, want)
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
