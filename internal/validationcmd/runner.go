package validationcmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/infra/shell"
	"github.com/nexu-io/looper/internal/processcontainment"
)

const permissionProfile = "looper-validation"

// Sandbox is a disposable, credential-free tool environment shared by
// daemon-owned validation commands and validation-gated Codex agents.
type Sandbox struct {
	CWD         string
	ProfileName string
	TempRoot    string
}

// NewSandbox creates the disposable filesystem used by one sandboxed run.
// Call Cleanup after the owning process has terminated.
func NewSandbox(cwd, profileName, prefix string) (*Sandbox, error) {
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
	sandbox := &Sandbox{CWD: filepath.Clean(cwd), ProfileName: profileName, TempRoot: tempRoot}
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
	return buildPermissionProfile(s.CWD, s.TempRoot, s.ProfileName)
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
// control Command, so it must execute inside Codex's native OS sandbox rather
// than in the daemon's credential-bearing environment.
type Options struct {
	CWD          string
	Command      string
	Timeout      time.Duration
	Tracker      processcontainment.LiveTracker
	CodexCommand string
}

// Run executes one validation command with no network, an isolated HOME/XDG
// tree, and write access only to the worktree and a disposable temporary root.
// Missing or unsupported Codex sandbox support fails closed.
func Run(ctx context.Context, options Options) (shell.Result, error) {
	cwd := strings.TrimSpace(options.CWD)
	if cwd == "" {
		return shell.Result{}, fmt.Errorf("validation sandbox: cwd is required")
	}
	command := strings.TrimSpace(options.Command)
	if command == "" {
		return shell.Result{}, fmt.Errorf("validation sandbox: command is required")
	}
	codexCommand := strings.TrimSpace(options.CodexCommand)
	if codexCommand == "" {
		codexCommand = "codex"
	}

	sandbox, err := NewSandbox(cwd, permissionProfile, "looper-validation-")
	if err != nil {
		return shell.Result{}, err
	}
	defer sandbox.Cleanup()
	args := []string{
		"sandbox",
		"-c", sandbox.PermissionConfig(),
		"-P", permissionProfile,
		"--sandbox-state-disable-network",
		"-C", cwd,
		"/usr/bin/env", "-i",
	}
	args = append(args, sandbox.Environment()...)
	args = append(args, "/bin/sh", "-c", command)

	return shell.Run(ctx, shell.Options{
		Command: codexCommand,
		Args:    args,
		CWD:     cwd,
		Timeout: options.Timeout,
		Tracker: options.Tracker,
	})
}

func buildPermissionProfile(cwd, tempRoot, profileName string) string {
	readRoots := map[string]struct{}{}
	for _, entry := range filepath.SplitList(os.Getenv("PATH")) {
		entry = strings.TrimSpace(entry)
		if entry != "" {
			readRoots[filepath.Clean(entry)] = struct{}{}
		}
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

	var filesystem strings.Builder
	filesystem.WriteString(`":minimal" = "read", ":workspace_roots" = { "." = "write" }`)
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

func linkedWorktreeReadRoots(cwd string) []string {
	data, err := os.ReadFile(filepath.Join(cwd, ".git"))
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
	roots := []string{gitDir}
	if common, readErr := os.ReadFile(filepath.Join(gitDir, "commondir")); readErr == nil {
		commonDir := strings.TrimSpace(string(common))
		if commonDir != "" {
			if !filepath.IsAbs(commonDir) {
				commonDir = filepath.Join(gitDir, commonDir)
			}
			roots = append(roots, filepath.Clean(commonDir))
		}
	}
	return roots
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
