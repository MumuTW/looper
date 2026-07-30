package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/daemonservice"
)

// serviceDeps are the effects the service subcommand needs, injected so the
// command can be tested without writing to the user's LaunchAgents directory or
// loading anything into their session.
type serviceDeps struct {
	loadConfig func(args []string) (config.LoadedFileConfig, error)
	executable func() (string, error)
	homeDir    func() (string, error)
	uid        func() int
	goos       string
	fs         daemonservice.FS
	run        daemonservice.Runner
}

func defaultServiceDeps() serviceDeps {
	return serviceDeps{
		loadConfig: func(args []string) (config.LoadedFileConfig, error) {
			cwd, err := os.Getwd()
			if err != nil {
				return config.LoadedFileConfig{}, err
			}
			return config.LoadFile(config.LoadFileOptions{CWD: cwd, Args: args})
		},
		executable: os.Executable,
		homeDir:    os.UserHomeDir,
		uid:        os.Geteuid,
		goos:       runtime.GOOS,
		fs:         daemonservice.OSFS{},
		run: func(ctx context.Context, name string, args ...string) (string, error) {
			output, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
			return string(output), err
		},
	}
}

// runServiceCommand implements `looperd service <subcommand>`. It returns a
// process exit code.
func runServiceCommand(ctx context.Context, args []string, stdout, stderr io.Writer, deps serviceDeps) int {
	if len(args) == 0 {
		writeServiceUsage(stderr)
		return 2
	}
	subcommand, rest := args[0], args[1:]
	switch subcommand {
	case "help", "-h", "--help":
		writeServiceUsage(stdout)
		return 0
	case "install", "uninstall", "status", "print":
	default:
		_, _ = fmt.Fprintf(stderr, "looperd service: unknown subcommand %q\n", subcommand)
		writeServiceUsage(stderr)
		return 2
	}

	plan, configPath, err := buildServicePlan(rest, deps, subcommand == "install")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "looperd service: %v\n", err)
		return 1
	}

	switch subcommand {
	case "print":
		_, _ = fmt.Fprint(stdout, plan.Unit)
		return 0
	case "status":
		return reportServiceStatus(plan, configPath, stdout, deps)
	case "install":
		result, err := daemonservice.Install(ctx, plan, deps.fs, deps.run)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "looperd service install: %v\n", err)
			return 1
		}
		_, _ = fmt.Fprintf(stdout, "installed %s service\n  unit:   %s\n  config: %s\n  logs:   %s\n",
			result.Manager, result.UnitPath, configPath, plan.LogDir)
		writeLoginCaveat(stdout, plan.Manager)
		return 0
	default:
		result, err := daemonservice.Uninstall(ctx, plan, deps.fs, deps.run)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "looperd service uninstall: %v\n", err)
			return 1
		}
		_, _ = fmt.Fprintf(stdout, "removed %s service\n  unit: %s\n", result.Manager, result.UnitPath)
		return 0
	}
}

// buildServicePlan resolves the configuration and turns it into a plan.
// requireServiceMode is true only for install: printing or inspecting a plan is
// useful precisely when the mode is not yet switched over.
func buildServicePlan(args []string, deps serviceDeps, requireServiceMode bool) (daemonservice.Plan, string, error) {
	loaded, err := deps.loadConfig(args)
	if err != nil {
		return daemonservice.Plan{}, "", err
	}
	if requireServiceMode {
		if source, ok := nonPersistentServiceOverride(loaded.Metadata.FieldSources); ok {
			return daemonservice.Plan{}, "", fmt.Errorf("refuse to install with %s override for %s; write the effective value to %s so the supervised daemon receives the same configuration", source, sourcePath(loaded.Metadata.FieldSources, source), loaded.Metadata.ConfigPath)
		}
	}
	manager, supported := daemonservice.ForGOOS(deps.goos)
	if !supported {
		return daemonservice.Plan{}, "", fmt.Errorf("looper has no supervised-service support on %s; run looperd in the foreground under your own supervisor", deps.goos)
	}
	if requireServiceMode && !daemonModeMatches(loaded.Config.Daemon.Mode, manager) {
		// daemon.mode is the configured intent. Installing a service while the
		// configuration says "foreground" would leave the two disagreeing, and the
		// config file is the authority.
		return daemonservice.Plan{}, "", fmt.Errorf(
			"daemon.mode is %q; set it to %q in %s before installing the service",
			loaded.Config.Daemon.Mode, serviceModeFor(manager), loaded.Metadata.ConfigPath)
	}

	executablePath, err := deps.executable()
	if err != nil {
		return daemonservice.Plan{}, "", fmt.Errorf("resolve looperd path: %w", err)
	}
	if executableIsTemporary(executablePath) {
		return daemonservice.Plan{}, "", fmt.Errorf("refuse to install transient executable %s; build and install a durable looperd binary first", executablePath)
	}
	homeDir, err := deps.homeDir()
	if err != nil {
		return daemonservice.Plan{}, "", fmt.Errorf("resolve home directory: %w", err)
	}

	plan, err := daemonservice.Build(daemonservice.Input{
		Config:         loaded.Config.Daemon,
		ExecutablePath: executablePath,
		ConfigPath:     loaded.Metadata.ConfigPath,
		HomeDir:        homeDir,
		UID:            deps.uid(),
		GOOS:           deps.goos,
	})
	if err != nil {
		return daemonservice.Plan{}, "", err
	}
	return plan, loaded.Metadata.ConfigPath, nil
}

func reportServiceStatus(plan daemonservice.Plan, _ string, stdout io.Writer, deps serviceDeps) int {
	installed := daemonservice.Installed(plan, deps.fs)
	state := "not installed"
	if installed {
		state = "installed"
	}
	_, _ = fmt.Fprintf(stdout, "%s service: %s\n  unit:   %s\n", plan.Manager, state, plan.UnitPath)
	if !installed {
		return 1
	}
	// Whether the daemon is actually healthy is `looper status`'s question; this
	// reports only whether a supervisor has been told to run it.
	_, _ = fmt.Fprintln(stdout, "  (run `looper status` for daemon health)")
	return 0
}

func nonPersistentServiceOverride(sources map[string]config.ValueSource) (config.ValueSource, bool) {
	for _, source := range sources {
		if source == config.ValueSourceCLI || source == config.ValueSourceEnv {
			return source, true
		}
	}
	return "", false
}

func sourcePath(sources map[string]config.ValueSource, want config.ValueSource) string {
	for path, source := range sources {
		if source == want {
			return path
		}
	}
	return "configuration"
}

func executableIsTemporary(path string) bool {
	path = filepath.Clean(path)
	temp := filepath.Clean(os.TempDir())
	if pathWithin(temp, path) {
		return true
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	if resolved, err := filepath.EvalSymlinks(temp); err == nil {
		temp = resolved
	}
	rel, err := filepath.Rel(temp, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func pathWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// writeLoginCaveat states the limit of what a per-user service buys, because
// "installed" reads as "always on" and on macOS that is not true.
func writeLoginCaveat(stdout io.Writer, manager daemonservice.Manager) {
	switch manager {
	case daemonservice.ManagerLaunchd:
		_, _ = fmt.Fprintln(stdout, "\nThis is a per-user LaunchAgent: it starts at login and stops at logout.\nFor a machine that must run unattended, enable automatic login, or keep the\nuser session alive — a LaunchAgent cannot run for a logged-out user.")
	default:
		_, _ = fmt.Fprintln(stdout, "\nThis is a systemd user unit: it starts at login and stops at logout unless\nlingering is enabled. Run `loginctl enable-linger $USER` for a machine that\nmust run unattended.")
	}
}

func daemonModeMatches(mode config.DaemonMode, manager daemonservice.Manager) bool {
	return mode == serviceModeFor(manager)
}

func serviceModeFor(manager daemonservice.Manager) config.DaemonMode {
	if manager == daemonservice.ManagerSystemd {
		return config.DaemonModeSystemd
	}
	return config.DaemonModeLaunchd
}

func writeServiceUsage(w io.Writer) {
	_, _ = fmt.Fprint(w, strings.TrimLeft(`
looperd service

Usage:
	looperd service install     Write the service unit and load it
	looperd service uninstall   Unload the service and remove its unit
	looperd service status      Report whether the unit is installed
	looperd service print       Print the unit that would be installed

Installs a launchd agent (macOS) or systemd user unit (Linux) that runs looperd
with the same configuration this command resolved. Set daemon.mode in your
config before installing.
`, "\n"))
}
