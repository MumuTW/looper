package validationcmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/MumuTW/looper/internal/infra/shell"
	"github.com/MumuTW/looper/internal/processcontainment"
	"github.com/MumuTW/looper/internal/processsandbox"
)

// Sandbox is the legacy Codex-specific disposable tool environment used by
// validation-gated Codex agents. Repository commands use processsandbox.Run.
type Sandbox struct {
	CWD         string
	ProfileName string
	TempRoot    string
	access      workspaceAccess
}

type workspaceAccess string

const (
	workspaceWritable workspaceAccess = "write"
	workspaceReadOnly workspaceAccess = "read"
)

// NewSandbox creates the disposable filesystem used by one sandboxed run.
// Call Cleanup after the owning process has terminated.
func NewSandbox(cwd, profileName, prefix string) (*Sandbox, error) {
	return newSandbox(cwd, profileName, prefix, workspaceWritable)
}

// NewAssessmentSandbox creates the Codex tool profile used before a human has
// authorized repository mutation. The worktree and linked Git metadata remain
// readable, while only the run-scoped temporary root is writable.
func NewAssessmentSandbox(cwd, profileName, prefix string) (*Sandbox, error) {
	return newSandbox(cwd, profileName, prefix, workspaceReadOnly)
}

func newSandbox(cwd, profileName, prefix string, access workspaceAccess) (*Sandbox, error) {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return nil, fmt.Errorf("validation sandbox: cwd is required")
	}
	profileName = strings.TrimSpace(profileName)
	if profileName == "" {
		return nil, fmt.Errorf("validation sandbox: profile name is required")
	}
	if strings.TrimSpace(prefix) == "" {
		prefix = "looper-sandbox-"
	}
	tempRoot, err := os.MkdirTemp("", prefix)
	if err != nil {
		return nil, fmt.Errorf("validation sandbox: create temporary root: %w", err)
	}
	if access != workspaceWritable && access != workspaceReadOnly {
		_ = os.RemoveAll(tempRoot)
		return nil, fmt.Errorf("validation sandbox: unsupported workspace access %q", access)
	}
	sandbox := &Sandbox{CWD: filepath.Clean(cwd), ProfileName: profileName, TempRoot: tempRoot, access: access}
	for _, dir := range []string{"home", "tmp", "xdg-config", "xdg-cache", "xdg-data", "go", "go-cache"} {
		if err := os.MkdirAll(filepath.Join(tempRoot, dir), 0o700); err != nil {
			sandbox.Cleanup()
			return nil, fmt.Errorf("validation sandbox: create %s: %w", dir, err)
		}
	}
	return sandbox, nil
}

func (s *Sandbox) Cleanup() {
	if s != nil && s.TempRoot != "" {
		_ = os.RemoveAll(s.TempRoot)
	}
}

// PermissionConfig returns the inline Codex permission profile definition.
func (s *Sandbox) PermissionConfig() string {
	return buildPermissionProfileForAccess(s.CWD, s.TempRoot, s.ProfileName, s.access)
}

// Environment returns the allowlisted environment exposed to sandboxed tools.
func (s *Sandbox) Environment() []string {
	return isolatedEnvironment(s.CWD, s.TempRoot)
}

// ShellEnvironmentConfig makes Codex tool subprocesses use Environment while
// the parent Codex process retains only the credentials it needs for model
// transport. Values are encoded as a TOML inline table.
func (s *Sandbox) ShellEnvironmentConfig() string {
	entries := make([]string, 0, len(s.Environment()))
	for _, entry := range s.Environment() {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			continue
		}
		entries = append(entries, fmt.Sprintf("%s=%s", strconv.Quote(key), strconv.Quote(value)))
	}
	return fmt.Sprintf("shell_environment_policy={ inherit = \"none\", set = { %s } }", strings.Join(entries, ", "))
}

// Options describes one daemon-owned validation command. The repository may
// control Command, so it must execute behind Looper's process boundary rather
// than in the daemon's credential-bearing environment.
type Options struct {
	CWD     string
	Command string
	Timeout time.Duration
	Tracker processcontainment.LiveTracker
}

// Run executes one validation command with no network, an isolated HOME/XDG
// tree, and write access only to the worktree and a disposable temporary root.
// Missing or unsupported sandbox runtime support fails closed.
func Run(ctx context.Context, options Options) (shell.Result, error) {
	cwd := strings.TrimSpace(options.CWD)
	if cwd == "" {
		return shell.Result{}, fmt.Errorf("validation sandbox: cwd is required")
	}
	command := strings.TrimSpace(options.Command)
	if command == "" {
		return shell.Result{}, fmt.Errorf("validation sandbox: command is required")
	}
	denyRead := []string{"/", filepath.Dir(cwd)}
	if home, homeErr := os.UserHomeDir(); homeErr == nil {
		denyRead = append(denyRead, home)
	}
	allowRead := validationReadRoots(cwd)
	environment, platformReadRoots := validationToolEnvironment()
	allowRead = append(allowRead, platformReadRoots...)
	return processsandbox.Run(ctx, processsandbox.Options{
		CWD:         cwd,
		Command:     "/bin/sh",
		Args:        []string{"-c", command},
		Environment: environment,
		Timeout:     options.Timeout,
		Tracker:     options.Tracker,
		Profile:     processsandbox.WritableWorkspaceProfile(allowRead, denyRead),
	})
}

func validationToolEnvironment() (processsandbox.ToolEnvironment, []string) {
	environment := processsandbox.ToolEnvironment{
		GoRoot:        resolvedGoRoot(),
		GoModuleCache: resolvedModuleCache(),
	}
	if runtime.GOOS != "darwin" {
		return environment, nil
	}
	output, err := exec.Command("xcode-select", "-p").Output()
	if err != nil {
		return environment, nil
	}
	developerDir := strings.TrimSpace(string(output))
	if developerDir == "" {
		return environment, nil
	}
	environment.PrependPath = []string{filepath.Join(developerDir, "usr", "bin")}
	return environment, []string{filepath.Dir(developerDir)}
}

func validationReadRoots(cwd string) []string {
	roots := map[string]struct{}{filepath.Clean(cwd): {}}
	for _, root := range resolvedGitToolRoots() {
		roots[root] = struct{}{}
	}
	if moduleCache := resolvedModuleCache(); moduleCache != "" {
		roots[filepath.Clean(moduleCache)] = struct{}{}
	}
	if goRoot := resolvedGoRoot(); goRoot != "" {
		roots[goRoot] = struct{}{}
	}
	for _, root := range linkedWorktreeReadRoots(cwd) {
		roots[root] = struct{}{}
	}
	result := make([]string, 0, len(roots))
	for root := range roots {
		result = append(result, root)
	}
	sort.Strings(result)
	return result
}

func resolvedModuleCache() string {
	if moduleCache := strings.TrimSpace(os.Getenv("GOMODCACHE")); moduleCache != "" {
		return moduleCache
	} else if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		return filepath.Join(home, "go", "pkg", "mod")
	}
	return ""
}

func buildPermissionProfileForAccess(cwd, tempRoot, profileName string, access workspaceAccess) string {
	readRoots := map[string]struct{}{}
	for _, root := range resolvedGitToolRoots() {
		readRoots[root] = struct{}{}
	}
	moduleCache := strings.TrimSpace(os.Getenv("GOMODCACHE"))
	if moduleCache == "" {
		if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
			moduleCache = filepath.Join(home, "go", "pkg", "mod")
		}
	}
	if moduleCache != "" {
		readRoots[filepath.Clean(moduleCache)] = struct{}{}
	}
	if goRoot := resolvedGoRoot(); goRoot != "" {
		readRoots[goRoot] = struct{}{}
	}
	for _, root := range linkedWorktreeReadRoots(cwd) {
		readRoots[root] = struct{}{}
	}
	delete(readRoots, filepath.Clean(cwd))
	delete(readRoots, filepath.Clean(tempRoot))

	paths := make([]string, 0, len(readRoots))
	for path := range readRoots {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	if access != workspaceWritable && access != workspaceReadOnly {
		return ""
	}
	var filesystem strings.Builder
	fmt.Fprintf(&filesystem, `":minimal" = "read", ":workspace_roots" = { "." = %q }`, string(access))
	fmt.Fprintf(&filesystem, ", %s = \"write\"", strconv.Quote(filepath.Clean(tempRoot)))
	for _, path := range paths {
		fmt.Fprintf(&filesystem, ", %s = \"read\"", strconv.Quote(path))
	}
	return fmt.Sprintf("permissions.%s={ filesystem = { %s }, network = { enabled = false } }", profileName, filesystem.String())
}

func resolvedGoRoot() string {
	goBinary, err := exec.LookPath("go")
	if err != nil {
		return ""
	}
	resolved, err := filepath.EvalSymlinks(goBinary)
	if err != nil {
		return ""
	}
	// GOROOT is the parent of bin/go for both the standard distribution and
	// Homebrew's libexec/bin/go layout.
	return filepath.Dir(filepath.Dir(resolved))
}

// toolProbeTimeout bounds synchronous tool probes: they run before the
// sandboxed process starts, so agent and validation timeouts cannot interrupt
// a hung wrapper. On expiry the probe's grant is skipped, never the whole run.
var toolProbeTimeout = 5 * time.Second

// probeEnvironment drops environment overrides that would make git or
// xcode-select report operator-controlled paths (GIT_EXEC_PATH, DEVELOPER_DIR),
// so grants follow the resolved installation, not inherited environment.
func probeEnvironment() []string {
	env := os.Environ()
	filtered := make([]string, 0, len(env))
	for _, entry := range env {
		if strings.HasPrefix(entry, "GIT_EXEC_PATH=") || strings.HasPrefix(entry, "DEVELOPER_DIR=") {
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered
}

func probeOutput(command string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), toolProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Env = probeEnvironment()
	output, err := cmd.Output()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

// resolvedGitToolRoots grants only the git installation sandboxed repository
// inspection resolves to: the binary itself, its exec-path helper directory,
// and, for darwin's stock stub, the developer-directory binary the stub execs.
// Inherited daemon PATH directories (for example $HOME/bin) may hold operator
// secrets and are never granted wholesale.
func resolvedGitToolRoots() []string {
	binary, err := exec.LookPath("git")
	if err != nil {
		return nil
	}
	resolved, err := filepath.EvalSymlinks(binary)
	if err != nil {
		return nil
	}
	roots := []string{resolved}
	if output, execErr := probeOutput(resolved, "--exec-path"); execErr == nil {
		if execPath := resolvedToolPath(output); execPath != "" {
			if info, statErr := os.Stat(execPath); statErr == nil && info.IsDir() {
				roots = append(roots, execPath)
			}
		}
	}
	if runtime.GOOS == "darwin" && resolved == "/usr/bin/git" {
		if developerGit := developerDirGitBinary(); developerGit != "" {
			roots = append(roots, developerGit)
		}
	}
	sort.Strings(roots)
	return roots
}

func developerDirGitBinary() string {
	output, err := probeOutput("xcode-select", "-p")
	if err != nil {
		return ""
	}
	if output == "" {
		return ""
	}
	candidate := filepath.Join(output, "usr", "bin", "git")
	if info, statErr := os.Stat(candidate); statErr != nil || info.IsDir() {
		return ""
	}
	return resolvedToolPath(candidate)
}

func resolvedToolPath(path string) string {
	if path == "" {
		return ""
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		return resolved
	}
	return absolute
}

func linkedWorktreeReadRoots(cwd string) []string {
	worktreeGitFile := filepath.Join(filepath.Clean(cwd), ".git")
	data, err := os.ReadFile(worktreeGitFile)
	if err != nil {
		return nil
	}
	line := strings.TrimSpace(string(data))
	if !strings.HasPrefix(line, "gitdir:") {
		return nil
	}
	gitDir := strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(cwd, gitDir)
	}
	gitDir = filepath.Clean(gitDir)
	common, readErr := os.ReadFile(filepath.Join(gitDir, "commondir"))
	if readErr != nil {
		return nil
	}
	commonDir := strings.TrimSpace(string(common))
	if commonDir == "" {
		return nil
	}
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(gitDir, commonDir)
	}
	commonDir = filepath.Clean(commonDir)

	// The worktree's .git file is repository-controlled input, so it cannot by
	// itself authorize additional read roots. Git's reciprocal linked-worktree
	// records are the authority: the private git dir must be a direct child of
	// <common-dir>/worktrees and its gitdir record must point back to this
	// worktree's .git file.
	worktreesDir := filepath.Join(commonDir, "worktrees")
	if filepath.Dir(gitDir) != worktreesDir || !sameFilesystemPath(filepath.Join(gitDir, "gitdir"), worktreeGitFile) {
		return nil
	}
	// Grant only the Git metadata required for read-only inspection. The whole
	// commonDir would expose .git/config (remote URLs with embedded credentials,
	// credential.helper paths) to untrusted assessment prompts.
	return assessmentSafeGitReadRoots(gitDir, commonDir)
}

// assessmentSafeGitReadRoots returns only the per-worktree metadata that
// read-only inspection needs, plus the object/ref metadata under the common
// git dir, deliberately omitting config, hooks, and other credential-bearing
// paths at either root. The private git dir itself is never granted wholesale:
// extensions.worktreeConfig can place credential helpers and embedded remotes
// in <gitDir>/config.worktree, and any other unlisted file there is unaudited.
func assessmentSafeGitReadRoots(gitDir, commonDir string) []string {
	roots := make([]string, 0, 8)
	for _, name := range []string{
		// Worktree identity and index state.
		"HEAD", "commondir", "gitdir", "index",
		// In-progress operation state: ref names, modes, and messages.
		"ORIG_HEAD", "MERGE_HEAD", "MERGE_MODE", "MERGE_MSG",
		"CHERRY_PICK_HEAD", "REVERT_HEAD", "REBASE_HEAD",
		"BISECT_LOG", "BISECT_EXPECTED_REV", "BISECT_ANCESTORS_OK",
		// Per-worktree reflogs, per-worktree refs (for example refs/bisect),
		// and sequencer state directories.
		"logs", "refs", "rebase-merge", "rebase-apply", "sequencer",
	} {
		path := filepath.Join(gitDir, name)
		if _, err := os.Stat(path); err == nil {
			roots = append(roots, path)
		}
	}
	// Split-index backing files referenced by the per-worktree index; hiding
	// them makes git status exit fatally in split-index worktrees.
	if shared, globErr := filepath.Glob(filepath.Join(gitDir, "sharedindex.*")); globErr == nil {
		roots = append(roots, shared...)
	}
	for _, name := range []string{"objects", "refs", "info", "logs", "packed-refs", "HEAD", "shallow"} {
		path := filepath.Join(commonDir, name)
		if _, err := os.Stat(path); err == nil {
			roots = append(roots, path)
		}
	}
	return roots
}

func sameFilesystemPath(pathFile, want string) bool {
	data, err := os.ReadFile(pathFile)
	if err != nil {
		return false
	}
	got := strings.TrimSpace(string(data))
	if got == "" {
		return false
	}
	if !filepath.IsAbs(got) {
		got = filepath.Join(filepath.Dir(pathFile), got)
	}
	return samePath(got, want)
}

func samePath(a, b string) bool {
	a = filepath.Clean(a)
	b = filepath.Clean(b)
	if a == b {
		return true
	}
	// macOS TempDir is often /var/... while Git records /private/var/...
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	if errA != nil || errB != nil {
		return false
	}
	return filepath.Clean(ra) == filepath.Clean(rb)
}

func isolatedEnvironment(cwd, tempRoot string) []string {
	pathValue := os.Getenv("PATH")
	if strings.TrimSpace(pathValue) == "" {
		pathValue = "/usr/bin:/bin"
	}
	env := []string{
		"HOME=" + filepath.Join(tempRoot, "home"),
		"TMPDIR=" + filepath.Join(tempRoot, "tmp"),
		"XDG_CONFIG_HOME=" + filepath.Join(tempRoot, "xdg-config"),
		"XDG_CACHE_HOME=" + filepath.Join(tempRoot, "xdg-cache"),
		"XDG_DATA_HOME=" + filepath.Join(tempRoot, "xdg-data"),
		"PATH=" + pathValue,
		"PWD=" + filepath.Clean(cwd),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=/usr/bin/false",
		"SSH_ASKPASS=/usr/bin/false",
		"GH_PROMPT_DISABLED=1",
		"GCM_INTERACTIVE=never",
		"GOCACHE=" + filepath.Join(tempRoot, "go-cache"),
		"GOPATH=" + filepath.Join(tempRoot, "go"),
	}
	if goRoot := resolvedGoRoot(); goRoot != "" {
		env = append(env, "GOROOT="+goRoot)
	}
	if moduleCache := strings.TrimSpace(os.Getenv("GOMODCACHE")); moduleCache != "" {
		env = append(env, "GOMODCACHE="+moduleCache)
	} else if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		env = append(env, "GOMODCACHE="+filepath.Join(home, "go", "pkg", "mod"))
	}
	for _, key := range []string{"LANG", "LC_ALL", "LC_CTYPE", "TERM"} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			env = append(env, key+"="+value)
		}
	}
	sort.Strings(env)
	return env
}
