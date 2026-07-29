package validationcmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/infra/shell"
	"github.com/nexu-io/looper/internal/processcontainment"
)

const permissionProfile = "looper-validation"

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

	tempRoot, err := os.MkdirTemp("", "looper-validation-")
	if err != nil {
		return shell.Result{}, fmt.Errorf("validation sandbox: create temporary root: %w", err)
	}
	defer os.RemoveAll(tempRoot)
	for _, dir := range []string{"home", "tmp", "xdg-config", "xdg-cache", "xdg-data", "go", "go-cache"} {
		if err := os.MkdirAll(filepath.Join(tempRoot, dir), 0o700); err != nil {
			return shell.Result{}, fmt.Errorf("validation sandbox: create %s: %w", dir, err)
		}
	}

	profile := buildPermissionProfile(cwd, tempRoot)
	args := []string{
		"sandbox",
		"-c", profile,
		"-P", permissionProfile,
		"--sandbox-state-disable-network",
		"-C", cwd,
		"/usr/bin/env", "-i",
	}
	args = append(args, isolatedEnvironment(cwd, tempRoot)...)
	args = append(args, "/bin/sh", "-c", command)

	return shell.Run(ctx, shell.Options{
		Command: codexCommand,
		Args:    args,
		CWD:     cwd,
		Timeout: options.Timeout,
		Tracker: options.Tracker,
	})
}

func buildPermissionProfile(cwd, tempRoot string) string {
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
	return fmt.Sprintf("permissions.%s={ filesystem = { %s }, network = { enabled = false } }", permissionProfile, filesystem.String())
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
