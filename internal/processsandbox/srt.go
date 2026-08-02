// Package processsandbox runs an arbitrary process tree behind Looper's
// credential-isolated operating-system sandbox boundary.
package processsandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/MumuTW/looper/internal/infra/shell"
	"github.com/MumuTW/looper/internal/processcontainment"
	"github.com/MumuTW/looper/internal/processsandbox/trustmanifest"
)

type workspaceAccess string

const (
	workspaceReadOnly workspaceAccess = "read-only"
	workspaceWritable workspaceAccess = "writable"
)

// Available verifies the pinned runtime and its complete executable closure
// against the root-sealed content manifest written at installation time.
func Available() error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("process sandbox: resolve current directory: %w", err)
	}
	_, err = installedRuntime(cwd)
	return err
}

// Profile is an opaque daemon-owned capability policy.
type Profile struct {
	workspaceAccess workspaceAccess
	allowRead       []string
	denyRead        []string
	allowedDomains  []string
}

// ReadOnlyProfile is the restricted process substrate: host reads start
// denied, only explicit repository/tool roots are restored, and egress is
// limited to exact reviewed model domains. It does not inject model
// credentials; assessor wiring still requires a separate credential broker.
func ReadOnlyProfile(allowRead, allowedDomains []string) Profile {
	return Profile{
		workspaceAccess: workspaceReadOnly,
		allowRead:       append([]string(nil), allowRead...),
		allowedDomains:  append([]string(nil), allowedDomains...),
	}
}

// AssessmentProfile is the pre-authorization capability boundary. Assessment
// commands may inspect only their explicit repository and tool roots; they
// receive no network capability at all. Model transport belongs to the native
// agent runtime, never to its tools.
func AssessmentProfile(allowRead []string) Profile {
	return ReadOnlyProfile(allowRead, nil)
}

// WritableWorkspaceProfile is for credential-free repository validation. It
// can write the CWD, while callers identify host read roots that stay hidden.
func WritableWorkspaceProfile(allowRead, denyRead []string) Profile {
	return Profile{
		workspaceAccess: workspaceWritable,
		allowRead:       append([]string(nil), allowRead...),
		denyRead:        append([]string(nil), denyRead...),
	}
}

type Options struct {
	CWD            string
	Command        string
	Args           []string
	Environment    ToolEnvironment
	Timeout        time.Duration
	Tracker        processcontainment.LiveTracker
	Profile        Profile
	runtimeCommand string
}

// ToolEnvironment is the complete caller-controlled environment surface.
// It deliberately has no arbitrary key/value escape hatch or credentials.
type ToolEnvironment struct {
	PrependPath   []string
	GoRoot        string
	GoModuleCache string
}

type settings struct {
	Network networkSettings  `json:"network"`
	FS      fsSettings       `json:"filesystem"`
	Ripgrep *ripgrepSettings `json:"ripgrep,omitempty"`

	BwrapPath string `json:"bwrapPath,omitempty"`
	SocatPath string `json:"socatPath,omitempty"`

	EnableWeakerNestedSandbox    bool `json:"enableWeakerNestedSandbox"`
	EnableWeakerNetworkIsolation bool `json:"enableWeakerNetworkIsolation"`
	AllowAppleEvents             bool `json:"allowAppleEvents"`
}

type ripgrepSettings struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

type networkSettings struct {
	AllowedDomains    []string `json:"allowedDomains"`
	DeniedDomains     []string `json:"deniedDomains"`
	AllowUnixSockets  []string `json:"allowUnixSockets"`
	AllowLocalBinding bool     `json:"allowLocalBinding"`
}

type fsSettings struct {
	DenyRead   []string `json:"denyRead"`
	AllowRead  []string `json:"allowRead"`
	AllowWrite []string `json:"allowWrite"`
	DenyWrite  []string `json:"denyWrite"`
}

func Run(ctx context.Context, options Options) (shell.Result, error) {
	cwd := strings.TrimSpace(options.CWD)
	if cwd == "" {
		return shell.Result{}, fmt.Errorf("process sandbox: cwd is required")
	}
	cwd, err := filepath.Abs(cwd)
	if err != nil {
		return shell.Result{}, fmt.Errorf("process sandbox: resolve cwd: %w", err)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(cwd); resolveErr == nil {
		cwd = resolved
	}
	if isFilesystemRoot(cwd) {
		return shell.Result{}, fmt.Errorf("process sandbox: cwd cannot be a filesystem root")
	}
	runtimePaths := runtimePaths{command: strings.TrimSpace(options.runtimeCommand)}
	if runtimePaths.command == "" {
		var runtimeErr error
		runtimePaths, runtimeErr = installedRuntime(cwd)
		if runtimeErr != nil {
			return shell.Result{}, runtimeErr
		}
	}
	command := strings.TrimSpace(options.Command)
	if command == "" {
		return shell.Result{}, fmt.Errorf("process sandbox: command is required")
	}
	if options.Profile.workspaceAccess != workspaceReadOnly && options.Profile.workspaceAccess != workspaceWritable {
		return shell.Result{}, fmt.Errorf("process sandbox: workspace access must be read-only or writable")
	}

	for _, domain := range options.Profile.allowedDomains {
		if strings.Contains(domain, "*") {
			return shell.Result{}, fmt.Errorf("process sandbox: network domain %q must be exact", domain)
		}
		if forbiddenNetworkDomain(domain) {
			return shell.Result{}, fmt.Errorf("process sandbox: network domain %q is forbidden", domain)
		}
	}
	for _, path := range options.Profile.allowRead {
		if unsafeAllowReadRoot(path) {
			return shell.Result{}, fmt.Errorf("process sandbox: read root %q is too broad", path)
		}
	}

	tempRoot, err := os.MkdirTemp("", "looper-process-sandbox-")
	if err != nil {
		return shell.Result{}, fmt.Errorf("process sandbox: create temporary root: %w", err)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(tempRoot); resolveErr == nil {
		tempRoot = resolved
	}
	defer os.RemoveAll(tempRoot)
	for _, dir := range []string{"home", "tmp", "xdg-config", "xdg-cache", "xdg-data", "go", "go-cache"} {
		if err := os.MkdirAll(filepath.Join(tempRoot, dir), 0o700); err != nil {
			return shell.Result{}, fmt.Errorf("process sandbox: create %s: %w", dir, err)
		}
	}

	allowWrite := []string{tempRoot}
	denyWrite := append(srtHostDefaultDenyWritePaths(), cwd)
	denyRead := append([]string(nil), options.Profile.denyRead...)
	if options.Profile.workspaceAccess == workspaceReadOnly {
		// Assessment-style profiles start from no host reads. SRT's minimal
		// system-tool roots plus the explicit allowlist remain available.
		denyRead = append(denyRead, "/")
	}
	if options.Profile.workspaceAccess == workspaceWritable {
		allowWrite = append(allowWrite, cwd)
		denyWrite = srtHostDefaultDenyWritePaths()
	}
	cfg := settings{
		Network: networkSettings{
			AllowedDomains:    compact(options.Profile.allowedDomains),
			DeniedDomains:     []string{},
			AllowUnixSockets:  []string{},
			AllowLocalBinding: false,
		},
		FS: fsSettings{
			DenyRead: compact(denyRead),
			AllowRead: compact(append(
				options.Profile.allowRead,
				append(append(runtimePaths.directories(), platformSystemReadRoots()...), cwd, tempRoot)...,
			)),
			AllowWrite: compact(allowWrite),
			DenyWrite:  compact(denyWrite),
		},
	}
	if runtimePaths.ripgrep != "" {
		cfg.Ripgrep = &ripgrepSettings{Command: runtimePaths.ripgrep, Args: []string{}}
	}
	cfg.BwrapPath = runtimePaths.bwrap
	cfg.SocatPath = runtimePaths.socat
	raw, err := json.Marshal(cfg)
	if err != nil {
		return shell.Result{}, fmt.Errorf("process sandbox: encode settings: %w", err)
	}
	settingsPath := filepath.Join(tempRoot, "settings.json")
	if err := os.WriteFile(settingsPath, raw, 0o600); err != nil {
		return shell.Result{}, fmt.Errorf("process sandbox: write settings: %w", err)
	}

	args := []string{"--settings", settingsPath, "--", command}
	args = append(args, options.Args...)
	return shell.Run(ctx, shell.Options{
		Command: runtimePaths.command,
		Args:    args,
		CWD:     cwd,
		Env:     isolatedEnvironment(tempRoot, options.Environment, runtimePaths.executableDirectories()),
		Timeout: options.Timeout,
		Tracker: options.Tracker,
	})
}

const (
	supportedRuntimePackage = "@anthropic-ai/sandbox-runtime"
	supportedRuntimeVersion = "0.0.67"
)

type runtimePaths struct {
	command string
	node    string
	ripgrep string
	bwrap   string
	socat   string
}

func (p runtimePaths) directories() []string {
	result := make([]string, 0, 5)
	for _, path := range []string{p.command, p.node, p.ripgrep, p.bwrap, p.socat} {
		if path != "" {
			result = append(result, filepath.Dir(path))
		}
	}
	return compact(result)
}

// executableDirectories preserves the sealed Node directory as the first PATH
// entry. SRT and package scripts commonly use #!/usr/bin/env node; sorting all
// support directories would let a different node binary win lookup.
func (p runtimePaths) executableDirectories() []string {
	ordered := []string{p.node, p.command, p.ripgrep, p.bwrap, p.socat}
	seen := make(map[string]struct{})
	result := make([]string, 0, len(ordered))
	for _, path := range ordered {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		dir := filepath.Dir(path)
		if _, ok := seen[dir]; ok {
			continue
		}
		seen[dir] = struct{}{}
		result = append(result, dir)
	}
	return result
}

func installedRuntime(cwd string) (runtimePaths, error) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return runtimePaths{}, fmt.Errorf("process sandbox: unsupported platform %s", runtime.GOOS)
	}
	path, err := exec.LookPath("srt")
	if err != nil {
		return runtimePaths{}, fmt.Errorf("process sandbox: required srt runtime not found: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return runtimePaths{}, fmt.Errorf("process sandbox: resolve srt runtime: %w", err)
	}
	var metadata struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	dir := filepath.Dir(resolved)
	var packageRoot string
	found := false
	for range 5 {
		raw, readErr := os.ReadFile(filepath.Join(dir, "package.json"))
		if readErr == nil && json.Unmarshal(raw, &metadata) == nil && metadata.Name == supportedRuntimePackage {
			found = true
			packageRoot = dir
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	if !found || metadata.Version != supportedRuntimeVersion {
		got := metadata.Version
		if !found {
			got = "unknown"
		}
		return runtimePaths{}, fmt.Errorf("process sandbox: srt package version %s is unsupported; require %s", got, supportedRuntimeVersion)
	}
	moduleRoot := packageRoot
	for filepath.Base(moduleRoot) != "node_modules" {
		parent := filepath.Dir(moduleRoot)
		if parent == moduleRoot {
			return runtimePaths{}, fmt.Errorf("process sandbox: srt package is not inside node_modules")
		}
		moduleRoot = parent
	}
	paths := runtimePaths{command: resolved}
	resolvedRoots := map[string]string{"srt": resolved}
	required := []struct {
		name   string
		target *string
	}{
		{name: "node", target: &paths.node},
		{name: "rg", target: &paths.ripgrep},
	}
	if runtime.GOOS == "linux" {
		required = append(required,
			struct {
				name   string
				target *string
			}{name: "bwrap", target: &paths.bwrap},
			struct {
				name   string
				target *string
			}{name: "socat", target: &paths.socat},
		)
	}
	for _, requiredCommand := range required {
		resolvedCommand, lookErr := exec.LookPath(requiredCommand.name)
		if lookErr != nil {
			return runtimePaths{}, fmt.Errorf("process sandbox: required support tool %s not found: %w", requiredCommand.name, lookErr)
		}
		resolvedCommand, resolveErr := filepath.EvalSymlinks(resolvedCommand)
		if resolveErr != nil {
			return runtimePaths{}, fmt.Errorf("process sandbox: resolve support tool %s: %w", requiredCommand.name, resolveErr)
		}
		if pathContains(cwd, resolvedCommand) {
			return runtimePaths{}, fmt.Errorf("process sandbox: support tool %s resolves inside cwd", requiredCommand.name)
		}
		*requiredCommand.target = resolvedCommand
		resolvedRoots[requiredCommand.name] = resolvedCommand
	}
	if pathContains(cwd, resolved) {
		return runtimePaths{}, fmt.Errorf("process sandbox: srt runtime resolves inside cwd")
	}
	if err := trustmanifest.Verify(trustmanifest.ManifestPath(moduleRoot), trustmanifest.Input{PackageRoot: moduleRoot, Roots: resolvedRoots}); err != nil {
		return runtimePaths{}, fmt.Errorf("process sandbox: untrusted srt installation: %w", err)
	}
	return paths, nil
}

func platformSystemReadRoots() []string {
	if runtime.GOOS == "darwin" {
		return []string{
			"/Library/Apple/System/Library",
			"/System/Library",
			"/etc/ssl/cert.pem",
			"/etc/ssl/certs",
			"/etc/ssl/openssl.cnf",
			"/private/etc/ssl/cert.pem",
			"/private/etc/ssl/certs",
			"/private/etc/ssl/openssl.cnf",
			"/usr/lib",
		}
	}
	return []string{
		"/bin",
		"/etc/alternatives",
		"/etc/ld.so.cache",
		"/etc/ssl/certs",
		"/lib",
		"/lib64",
		"/usr/bin",
		"/usr/lib",
		"/usr/lib64",
		"/usr/local/bin",
		"/usr/local/lib",
	}
}

func srtHostDefaultDenyWritePaths() []string {
	// SRT adds these host paths to every profile. Looper supplies its own
	// isolated TMPDIR and does not grant terminal/device side channels.
	return []string{
		"/dev/autofs_nowait",
		"/dev/dtracehelper",
		"/dev/tty",
		"/private/tmp/claude",
		"/tmp/claude",
	}
}

func isolatedEnvironment(tempRoot string, tools ToolEnvironment, supportPath []string) map[string]string {
	env := map[string]string{
		"HOME":            filepath.Join(tempRoot, "home"),
		"TMPDIR":          filepath.Join(tempRoot, "tmp"),
		"XDG_CONFIG_HOME": filepath.Join(tempRoot, "xdg-config"),
		"XDG_CACHE_HOME":  filepath.Join(tempRoot, "xdg-cache"),
		"XDG_DATA_HOME":   filepath.Join(tempRoot, "xdg-data"),
		"GOCACHE":         filepath.Join(tempRoot, "go-cache"),
		"GOPATH":          filepath.Join(tempRoot, "go"),
		// SRT otherwise rewrites TMPDIR to /tmp/claude, which may not exist.
		// Keep its implementation-specific temporary path inside our root.
		"CLAUDE_CODE_TMPDIR":  filepath.Join(tempRoot, "tmp"),
		"PATH":                executablePath(append(supportPath, tools.PrependPath...)),
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_CONFIG_GLOBAL":   "/dev/null",
		"GIT_TERMINAL_PROMPT": "0",
		"GIT_ASKPASS":         "/usr/bin/false",
		"SSH_ASKPASS":         "/usr/bin/false",
		"GH_PROMPT_DISABLED":  "1",
		"GCM_INTERACTIVE":     "never",
	}
	for _, key := range []string{"LANG", "LC_ALL", "LC_CTYPE", "TERM"} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			env[key] = value
		}
	}
	if value := strings.TrimSpace(tools.GoRoot); value != "" {
		env["GOROOT"] = value
	}
	if value := strings.TrimSpace(tools.GoModuleCache); value != "" {
		env["GOMODCACHE"] = value
	}
	return env
}

func compact(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func executablePath(prepend []string) string {
	entries := append([]string(nil), prepend...)
	entries = append(entries, filepath.SplitList(os.Getenv("PATH"))...)
	entries = append(entries, "/usr/bin", "/bin")
	seen := make(map[string]struct{}, len(entries))
	ordered := make([]string, 0, len(entries))
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if _, ok := seen[entry]; ok {
			continue
		}
		seen[entry] = struct{}{}
		ordered = append(ordered, entry)
	}
	return strings.Join(ordered, string(os.PathListSeparator))
}

func forbiddenNetworkDomain(domain string) bool {
	domain = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(domain)), "*.")
	return domain == "github.com" ||
		strings.HasSuffix(domain, ".github.com") ||
		domain == "githubusercontent.com" ||
		strings.HasSuffix(domain, ".githubusercontent.com")
}

func unsafeAllowReadRoot(path string) bool {
	path = strings.TrimSpace(path)
	if path == "" {
		return false
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return true
	}
	if resolved, resolveErr := filepath.EvalSymlinks(absolute); resolveErr == nil {
		absolute = resolved
	}
	if isFilesystemRoot(absolute) {
		return true
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	if resolved, resolveErr := filepath.EvalSymlinks(home); resolveErr == nil {
		home = resolved
	}
	protected := []string{
		home,
		filepath.Join(home, ".ssh"),
		filepath.Join(home, ".aws"),
		filepath.Join(home, ".azure"),
		filepath.Join(home, ".config", "gh"),
		filepath.Join(home, ".config", "gcloud"),
		filepath.Join(home, ".config", "opencode"),
		filepath.Join(home, ".claude"),
		filepath.Join(home, ".codex"),
		filepath.Join(home, ".cursor"),
		filepath.Join(home, ".docker"),
		filepath.Join(home, ".gemini"),
		filepath.Join(home, ".kube"),
		filepath.Join(home, ".looper"),
		filepath.Join(home, ".gitconfig"),
		filepath.Join(home, ".netrc"),
	}
	for _, secretRoot := range protected {
		if pathContains(absolute, secretRoot) {
			return true
		}
	}
	return false
}

func isFilesystemRoot(path string) bool {
	clean := filepath.Clean(path)
	return clean == filepath.Clean(string(filepath.Separator)) ||
		(filepath.VolumeName(clean) != "" && clean == filepath.VolumeName(clean)+string(filepath.Separator))
}

func pathContains(root, path string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
