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

	legacyImports, err := trackedLegacyModuleImports(root, legacyModulePath)
	if err != nil {
		t.Fatalf("scan Go source for legacy module path: %v", err)
	}
	for _, relativePath := range legacyImports {
		t.Errorf("%s still imports legacy module path %q", relativePath, legacyModulePath)
	}
}

func trackedLegacyModuleImports(root, legacyModulePath string) ([]string, error) {
	trackedFiles, err := trackedGoFiles(root)
	if err != nil {
		return nil, err
	}

	legacyImports := make([]string, 0)
	for _, relativePath := range trackedFiles {
		path := filepath.Join(root, filepath.FromSlash(relativePath))
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return nil, err
		}
		for _, imported := range file.Imports {
			importPath, err := strconv.Unquote(imported.Path.Value)
			if err != nil {
				return nil, err
			}
			if importPath == legacyModulePath || strings.HasPrefix(importPath, legacyModulePath+"/") {
				legacyImports = append(legacyImports, relativePath)
				break
			}
		}
	}
	return legacyImports, nil
}

func trackedGoFiles(root string) ([]string, error) {
	command := exec.Command("git", "-C", root, "ls-files", "-z", "--", "*.go")
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("list tracked Go files: %w", err)
	}
	output = bytes.TrimSuffix(output, []byte{0})
	if len(output) == 0 {
		return nil, nil
	}
	files := strings.Split(string(output), "\x00")
	return files, nil
}

func TestTrackedModulePathScanIgnoresUntrackedWorktrees(t *testing.T) {
	t.Parallel()
	root := initModulePathTestRepo(t)
	writeModulePathTestFile(t, root, "tracked.go", "package tracked\n", true)
	writeModulePathTestFile(t, root, filepath.Join(".worktrees", "stale", "legacy.go"), "package stale\nimport _ \"github.com/nexu-io/looper/internal/version\"\n", false)

	legacyImports, err := trackedLegacyModuleImports(root, "github.com/nexu-io/looper")
	if err != nil {
		t.Fatal(err)
	}
	if len(legacyImports) != 0 {
		t.Fatalf("trackedLegacyModuleImports() = %v, want untracked worktree ignored", legacyImports)
	}
}

func TestTrackedModulePathScanFindsTrackedLegacyImport(t *testing.T) {
	t.Parallel()
	root := initModulePathTestRepo(t)
	writeModulePathTestFile(t, root, "tracked.go", "package tracked\nimport _ \"github.com/nexu-io/looper/internal/version\"\n", true)

	legacyImports, err := trackedLegacyModuleImports(root, "github.com/nexu-io/looper")
	if err != nil {
		t.Fatal(err)
	}
	if len(legacyImports) != 1 || legacyImports[0] != "tracked.go" {
		t.Fatalf("trackedLegacyModuleImports() = %v, want [tracked.go]", legacyImports)
	}
}

func initModulePathTestRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	command := exec.Command("git", "-C", root, "init", "--quiet")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, output)
	}
	return root
}

func writeModulePathTestFile(t *testing.T, root, relativePath, contents string, tracked bool) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relativePath))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", relativePath, err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", relativePath, err)
	}
	if !tracked {
		return
	}
	command := exec.Command("git", "-C", root, "add", "--", filepath.FromSlash(relativePath))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git add %s: %v\n%s", relativePath, err, output)
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
