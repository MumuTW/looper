package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/daemonservice"
)

type serviceTestFS struct{ files map[string][]byte }

func (f *serviceTestFS) MkdirAll(string, os.FileMode) error { return nil }
func (f *serviceTestFS) CreateExclusive(path string, data []byte, _ os.FileMode) error {
	if _, exists := f.files[path]; exists {
		return os.ErrExist
	}
	f.files[path] = data
	return nil
}
func (f *serviceTestFS) Remove(path string) error {
	if _, exists := f.files[path]; !exists {
		return os.ErrNotExist
	}
	delete(f.files, path)
	return nil
}
func (f *serviceTestFS) Stat(path string) (os.FileInfo, error) {
	if _, exists := f.files[path]; !exists {
		return nil, os.ErrNotExist
	}
	return serviceTestFileInfo{}, nil
}

type serviceTestFileInfo struct{}

func (serviceTestFileInfo) Name() string       { return "unit" }
func (serviceTestFileInfo) Size() int64        { return 0 }
func (serviceTestFileInfo) Mode() os.FileMode  { return 0o644 }
func (serviceTestFileInfo) ModTime() time.Time { return time.Time{} }
func (serviceTestFileInfo) IsDir() bool        { return false }
func (serviceTestFileInfo) Sys() any           { return nil }

type serviceTestHarness struct {
	fs             *serviceTestFS
	commands       []string
	loadedArgs     []string
	configPath     string
	mode           config.DaemonMode
	toolDetection  map[string]config.ToolDetectionStatus
	executablePath string
	loadErr        error
}

func newServiceHarness() *serviceTestHarness {
	return &serviceTestHarness{
		fs:         &serviceTestFS{files: map[string][]byte{}},
		configPath: "/home/dev/.looper/config.toml",
		mode:       config.DaemonModeLaunchd,
		toolDetection: map[string]config.ToolDetectionStatus{
			"gitPath": config.ToolDetectionStatusConfigured,
			"ghPath":  config.ToolDetectionStatusConfigured,
		},
		executablePath: "/home/dev/.local/bin/looperd",
	}
}

func (h *serviceTestHarness) deps() serviceDeps {
	return serviceDeps{
		loadConfig: func(args []string) (config.LoadedFileConfig, error) {
			h.loadedArgs = append([]string(nil), args...)
			if h.loadErr != nil {
				return config.LoadedFileConfig{}, h.loadErr
			}
			cfg, err := config.DefaultConfig("/home/dev")
			if err != nil {
				return config.LoadedFileConfig{}, err
			}
			cfg.Daemon.Mode = h.mode
			cfg.Daemon.LogDir = "/home/dev/.looper/logs"
			cfg.Daemon.WorkingDirectory = "/home/dev"
			cfg.Daemon.Environment = map[string]string{}
			return config.LoadedFileConfig{
				Config: cfg,
				Metadata: config.LoadFileMetadata{
					ConfigPath: h.configPath, ToolDetection: h.toolDetection,
				},
			}, nil
		},
		executable:    func() (string, error) { return h.executablePath, nil },
		homeDir:       func() (string, error) { return "/home/dev", nil },
		xdgConfigHome: func() string { return "" },
		uid:           func() int { return 501 },
		goos:          "darwin",
		fs:            h.fs,
		run: func(_ context.Context, name string, args ...string) (string, error) {
			h.commands = append(h.commands, strings.TrimSpace(name+" "+strings.Join(args, " ")))
			return "", nil
		},
	}
}

func runService(t *testing.T, h *serviceTestHarness, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runServiceCommand(context.Background(), args, &stdout, &stderr, h.deps())
	return code, stdout.String(), stderr.String()
}

func TestServiceInstallWritesAndActivates(t *testing.T) {
	t.Parallel()
	h := newServiceHarness()

	code, stdout, stderr := runService(t, h, "install")

	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if len(h.fs.files) != 1 || len(h.commands) == 0 {
		t.Fatalf("files = %v commands = %v", h.fs.files, h.commands)
	}
	// "installed" reads as "always on", and for a per-user agent that is untrue.
	if !strings.Contains(stdout, "logout") {
		t.Fatalf("install output omits the login-session caveat:\n%s", stdout)
	}
}

func TestServiceInstallRefusesWhenDaemonModeIsForeground(t *testing.T) {
	t.Parallel()
	h := newServiceHarness()
	h.mode = config.DaemonModeForeground

	code, _, stderr := runService(t, h, "install")

	if code == 0 || !strings.Contains(stderr, "daemon.mode") {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if len(h.fs.files) != 0 || len(h.commands) != 0 {
		t.Fatal("a refused install still touched the system")
	}
}

// `go run` builds into a temporary directory that is removed when it exits, so
// the recorded unit would point at nothing.
func TestServiceInstallRefusesATransientExecutable(t *testing.T) {
	t.Parallel()
	h := newServiceHarness()
	h.executablePath = filepath.Join(os.TempDir(), "go-build123", "looperd")

	code, _, stderr := runService(t, h, "install")

	if code == 0 || !strings.Contains(stderr, "temporary directory") {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if len(h.fs.files) != 0 {
		t.Fatal("a refused install wrote a unit")
	}
}

// print is for inspecting what would be installed, which is legitimate under
// `go run` — the check belongs only to the command that records the path.
func TestServicePrintAllowsATransientExecutable(t *testing.T) {
	t.Parallel()
	h := newServiceHarness()
	h.executablePath = filepath.Join(os.TempDir(), "go-build123", "looperd")

	code, stdout, stderr := runService(t, h, "print")

	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(stdout, daemonservice.Label) {
		t.Fatalf("print did not emit the unit:\n%s", stdout)
	}
	if len(h.fs.files) != 0 || len(h.commands) != 0 {
		t.Fatal("print touched the system")
	}
}

// Uninstall must work when the configuration does not load — which is often
// exactly why someone is uninstalling.
func TestServiceUninstallReadsNoConfiguration(t *testing.T) {
	t.Parallel()
	h := newServiceHarness()
	h.loadErr = os.ErrPermission

	code, _, stderr := runService(t, h, "install")
	if code == 0 {
		t.Fatal("install succeeded with an unloadable config")
	}
	h.loadedArgs = nil

	code, _, stderr = runService(t, h, "uninstall")
	if code != 0 {
		t.Fatalf("uninstall exit = %d, stderr = %s", code, stderr)
	}
	if h.loadedArgs != nil {
		t.Fatal("uninstall loaded configuration")
	}
	if len(h.commands) == 0 {
		t.Fatal("uninstall did not attempt deactivation")
	}
}

// Status reports what is on the machine. The current invocation's configuration
// is not persisted state and must not be presented as though it were.
func TestServiceStatusReportsOnlyTheUnit(t *testing.T) {
	t.Parallel()
	h := newServiceHarness()

	code, stdout, _ := runService(t, h, "status")
	if code == 0 {
		t.Fatal("status reported success before install")
	}
	if strings.Contains(stdout, h.configPath) {
		t.Fatalf("status presented the invocation's config path as installed state:\n%s", stdout)
	}

	if code, _, stderr := runService(t, h, "install"); code != 0 {
		t.Fatalf("install exit = %d, stderr = %s", code, stderr)
	}
	code, stdout, _ = runService(t, h, "status")
	if code != 0 || !strings.Contains(stdout, "installed") {
		t.Fatalf("exit = %d, stdout = %s", code, stdout)
	}
}

// These commands read no configuration, so a flag would be silently ignored
// rather than doing what the caller expects.
func TestServiceTeardownCommandsRejectArguments(t *testing.T) {
	t.Parallel()
	for _, subcommand := range []string{"uninstall", "status"} {
		h := newServiceHarness()
		code, _, stderr := runService(t, h, subcommand, "--config", "/tmp/other.toml")
		if code == 0 {
			t.Fatalf("%s accepted a config flag", subcommand)
		}
		if !strings.Contains(stderr, "takes no arguments") {
			t.Fatalf("%s stderr = %s", subcommand, stderr)
		}
	}
}

// Configuration flags follow the subcommand and are passed through untouched.
func TestServiceInstallForwardsConfigFlags(t *testing.T) {
	t.Parallel()
	h := newServiceHarness()

	if code, _, stderr := runService(t, h, "install", "--config", "/tmp/custom.toml"); code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if strings.Join(h.loadedArgs, " ") != "--config /tmp/custom.toml" {
		t.Fatalf("loaded args = %v", h.loadedArgs)
	}
}

func TestServiceRejectsUnknownSubcommand(t *testing.T) {
	t.Parallel()
	h := newServiceHarness()

	code, _, _ := runService(t, h, "reticulate")

	if code == 0 {
		t.Fatal("unknown subcommand exited 0")
	}
	if len(h.fs.files) != 0 {
		t.Fatal("unknown subcommand touched the system")
	}
}

// Replacing a unit is uninstall then install, so an active service is never
// silently left running an old definition.
func TestServiceInstallRefusesToReplaceAnExistingUnit(t *testing.T) {
	t.Parallel()
	h := newServiceHarness()

	if code, _, stderr := runService(t, h, "install"); code != 0 {
		t.Fatalf("first install exit = %d, stderr = %s", code, stderr)
	}
	code, _, stderr := runService(t, h, "install")

	if code == 0 {
		t.Fatal("a second install replaced the unit")
	}
	if !strings.Contains(stderr, "uninstall it first") {
		t.Fatalf("stderr = %s", stderr)
	}
}
