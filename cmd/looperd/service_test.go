package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/daemonservice"
)

type serviceTestFS struct {
	files map[string][]byte
}

func (f *serviceTestFS) MkdirAll(string, os.FileMode) error { return nil }
func (f *serviceTestFS) WriteFile(path string, data []byte, _ os.FileMode) error {
	f.files[path] = data
	return nil
}
func (f *serviceTestFS) Remove(path string) error {
	if _, ok := f.files[path]; !ok {
		return os.ErrNotExist
	}
	delete(f.files, path)
	return nil
}
func (f *serviceTestFS) Stat(path string) (os.FileInfo, error) {
	if _, ok := f.files[path]; !ok {
		return nil, os.ErrNotExist
	}
	return serviceTestFileInfo{}, nil
}

type serviceTestFileInfo struct{}

func (serviceTestFileInfo) Name() string       { return "unit" }
func (serviceTestFileInfo) Size() int64        { return 0 }
func (serviceTestFileInfo) Mode() os.FileMode  { return 0o600 }
func (serviceTestFileInfo) ModTime() time.Time { return time.Time{} }
func (serviceTestFileInfo) IsDir() bool        { return false }
func (serviceTestFileInfo) Sys() any           { return nil }

func testServiceDeps(mode config.DaemonMode, fs *serviceTestFS, commands *[]string) serviceDeps {
	return serviceDeps{
		loadConfig: func([]string) (config.LoadedFileConfig, error) {
			cfg, err := config.DefaultConfig("/home/dev")
			if err != nil {
				return config.LoadedFileConfig{}, err
			}
			cfg.Daemon.Mode = mode
			cfg.Daemon.LogDir = "/home/dev/.looper/logs"
			cfg.Daemon.WorkingDirectory = "/home/dev"
			return config.LoadedFileConfig{
				Config:   cfg,
				Metadata: config.LoadFileMetadata{ConfigPath: "/home/dev/.looper/config.toml"},
			}, nil
		},
		executable: func() (string, error) { return "/home/dev/.local/bin/looperd", nil },
		homeDir:    func() (string, error) { return "/home/dev", nil },
		uid:        func() int { return 501 },
		goos:       "darwin",
		fs:         fs,
		run: func(_ context.Context, name string, args ...string) (string, error) {
			*commands = append(*commands, strings.TrimSpace(name+" "+strings.Join(args, " ")))
			return "", nil
		},
	}
}

// daemon.mode is the configured intent. Installing while it says "foreground"
// would leave the config and the machine disagreeing, with the config — the
// stated authority — losing.
func TestServiceInstallRefusesWhenDaemonModeIsForeground(t *testing.T) {
	t.Parallel()
	fs := &serviceTestFS{files: map[string][]byte{}}
	var commands []string
	var stdout, stderr bytes.Buffer

	code := runServiceCommand(context.Background(), []string{"install"}, &stdout, &stderr,
		testServiceDeps(config.DaemonModeForeground, fs, &commands))

	if code == 0 {
		t.Fatal("install succeeded while daemon.mode was foreground")
	}
	if !strings.Contains(stderr.String(), "daemon.mode") {
		t.Fatalf("stderr does not explain the mode requirement: %s", stderr.String())
	}
	if len(fs.files) != 0 || len(commands) != 0 {
		t.Fatalf("refused install still touched the system: files=%v commands=%v", fs.files, commands)
	}
}

func TestServiceInstallWritesAndActivates(t *testing.T) {
	t.Parallel()
	fs := &serviceTestFS{files: map[string][]byte{}}
	var commands []string
	var stdout, stderr bytes.Buffer

	code := runServiceCommand(context.Background(), []string{"install"}, &stdout, &stderr,
		testServiceDeps(config.DaemonModeLaunchd, fs, &commands))

	if code != 0 {
		t.Fatalf("install exit = %d, stderr = %s", code, stderr.String())
	}
	if len(fs.files) != 1 {
		t.Fatalf("files = %v, want one unit", fs.files)
	}
	if len(commands) == 0 || !strings.HasPrefix(commands[0], "launchctl") {
		t.Fatalf("commands = %v", commands)
	}
	// "installed" reads as "always on", and for a per-user agent that is untrue.
	if !strings.Contains(stdout.String(), "logout") {
		t.Fatalf("install output omits the login-session caveat:\n%s", stdout.String())
	}
}

// print is for inspecting what would be installed, which is most useful before
// the mode has been switched — so it must not require the mode.
func TestServicePrintWorksBeforeSwitchingMode(t *testing.T) {
	t.Parallel()
	fs := &serviceTestFS{files: map[string][]byte{}}
	var commands []string
	var stdout, stderr bytes.Buffer

	code := runServiceCommand(context.Background(), []string{"print"}, &stdout, &stderr,
		testServiceDeps(config.DaemonModeForeground, fs, &commands))

	if code != 0 {
		t.Fatalf("print exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), daemonservice.Label) {
		t.Fatalf("print did not emit the unit:\n%s", stdout.String())
	}
	if len(fs.files) != 0 || len(commands) != 0 {
		t.Fatal("print wrote to the system")
	}
}

func TestServiceStatusReportsInstalledState(t *testing.T) {
	t.Parallel()
	fs := &serviceTestFS{files: map[string][]byte{}}
	var commands []string
	var stdout, stderr bytes.Buffer
	deps := testServiceDeps(config.DaemonModeLaunchd, fs, &commands)

	if code := runServiceCommand(context.Background(), []string{"status"}, &stdout, &stderr, deps); code == 0 {
		t.Fatal("status reported success before install")
	}
	stdout.Reset()
	if code := runServiceCommand(context.Background(), []string{"install"}, &stdout, &stderr, deps); code != 0 {
		t.Fatalf("install exit = %d, stderr = %s", code, stderr.String())
	}
	stdout.Reset()
	if code := runServiceCommand(context.Background(), []string{"status"}, &stdout, &stderr, deps); code != 0 {
		t.Fatalf("status exit = %d after install", code)
	}
	if !strings.Contains(stdout.String(), "installed") {
		t.Fatalf("status output = %s", stdout.String())
	}
}

func TestServiceUninstallRemovesTheUnit(t *testing.T) {
	t.Parallel()
	fs := &serviceTestFS{files: map[string][]byte{}}
	var commands []string
	var stdout, stderr bytes.Buffer
	deps := testServiceDeps(config.DaemonModeLaunchd, fs, &commands)

	if code := runServiceCommand(context.Background(), []string{"install"}, &stdout, &stderr, deps); code != 0 {
		t.Fatalf("install exit = %d", code)
	}
	if code := runServiceCommand(context.Background(), []string{"uninstall"}, &stdout, &stderr, deps); code != 0 {
		t.Fatalf("uninstall exit = %d, stderr = %s", code, stderr.String())
	}
	if len(fs.files) != 0 {
		t.Fatalf("uninstall left files: %v", fs.files)
	}
}

func TestServiceRejectsUnknownSubcommand(t *testing.T) {
	t.Parallel()
	fs := &serviceTestFS{files: map[string][]byte{}}
	var commands []string
	var stdout, stderr bytes.Buffer

	code := runServiceCommand(context.Background(), []string{"reticulate"}, &stdout, &stderr,
		testServiceDeps(config.DaemonModeLaunchd, fs, &commands))

	if code == 0 {
		t.Fatal("unknown subcommand exited 0")
	}
	if len(fs.files) != 0 {
		t.Fatal("unknown subcommand wrote to the system")
	}
}
