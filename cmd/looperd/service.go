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

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/daemonservice"
)

// serviceDeps are the effects the subcommand needs, injected so it is testable
// without writing to the user's LaunchAgents directory or loading anything into
// their session.
type serviceDeps struct {
	loadConfig    func(args []string) (config.LoadedFileConfig, error)
	executable    func() (string, error)
	homeDir       func() (string, error)
	xdgConfigHome func() string
	uid           func() int
	goos          string
	fs            daemonservice.FS
	run           daemonservice.Runner
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
		executable:    os.Executable,
		homeDir:       os.UserHomeDir,
		xdgConfigHome: func() string { return os.Getenv("XDG_CONFIG_HOME") },
		uid:           os.Getuid,
		goos:          runtime.GOOS,
		fs:            daemonservice.OSFS{},
		run: func(ctx context.Context, name string, args ...string) (string, error) {
			output, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
			return string(output), err
		},
	}
}

// runServiceCommand implements `looperd service <subcommand>` and returns a
// process exit code.
//
// Only install and print read configuration. uninstall and status address the
// canonical location directly, so they work when the configuration is broken —
// which is often exactly why someone is uninstalling.
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
	case "install", "print":
		return runServicePlanCommand(ctx, subcommand, rest, stdout, stderr, deps)
	case "uninstall", "status":
		return runServiceTeardownCommand(ctx, subcommand, rest, stdout, stderr, deps)
	default:
		_, _ = fmt.Fprintf(stderr, "looperd service: unknown subcommand %q\n", subcommand)
		writeServiceUsage(stderr)
		return 2
	}
}

func runServicePlanCommand(ctx context.Context, subcommand string, args []string, stdout, stderr io.Writer, deps serviceDeps) int {
	loaded, err := deps.loadConfig(args)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "looperd service %s: %v\n", subcommand, err)
		return 1
	}
	executablePath, err := deps.executable()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "looperd service %s: resolve looperd path: %v\n", subcommand, err)
		return 1
	}
	homeDir, err := deps.homeDir()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "looperd service %s: resolve home directory: %v\n", subcommand, err)
		return 1
	}

	plan, err := daemonservice.Build(daemonservice.Input{
		Config:         loaded.Config.Daemon,
		ToolDetection:  loaded.Metadata.ToolDetection,
		ExecutablePath: executablePath,
		ConfigPath:     loaded.Metadata.ConfigPath,
		HomeDir:        homeDir,
		XDGConfigHome:  deps.xdgConfigHome(),
		UID:            deps.uid(),
		GOOS:           deps.goos,
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "looperd service %s: %v\n", subcommand, err)
		return 1
	}

	if subcommand == "print" {
		_, _ = fmt.Fprint(stdout, plan.Unit)
		return 0
	}

	// A transient executable is checked only here, the one command that records a
	// path for a supervisor to run later. Printing or inspecting under `go run` is
	// legitimate.
	if err := rejectTransientExecutable(executablePath); err != nil {
		_, _ = fmt.Fprintf(stderr, "looperd service install: %v\n", err)
		return 1
	}
	if !daemonModeMatches(loaded.Config.Daemon.Mode, plan.Manager) {
		_, _ = fmt.Fprintf(stderr, "looperd service install: daemon.mode is %q; set it to %q in %s before installing\n",
			loaded.Config.Daemon.Mode, serviceModeFor(plan.Manager), loaded.Metadata.ConfigPath)
		return 1
	}

	result, err := daemonservice.Install(ctx, plan, deps.fs, deps.run)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "looperd service install: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "installed %s service\n  unit:   %s\n  config: %s\n  logs:   %s\n",
		result.Manager, result.UnitPath, loaded.Metadata.ConfigPath, plan.LogDir)
	writeLoginCaveat(stdout, result.Manager)
	return 0
}

func runServiceTeardownCommand(ctx context.Context, subcommand string, args []string, stdout, stderr io.Writer, deps serviceDeps) int {
	if len(args) > 0 {
		// These commands read no configuration, so a flag here would be silently
		// ignored rather than doing what the caller expects.
		_, _ = fmt.Fprintf(stderr, "looperd service %s takes no arguments (it addresses the canonical service directly); got %v\n", subcommand, args)
		return 2
	}
	homeDir, err := deps.homeDir()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "looperd service %s: resolve home directory: %v\n", subcommand, err)
		return 1
	}
	plan, err := daemonservice.BuildTeardown(daemonservice.TeardownInput{
		HomeDir: homeDir, XDGConfigHome: deps.xdgConfigHome(), UID: deps.uid(), GOOS: deps.goos,
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "looperd service %s: %v\n", subcommand, err)
		return 1
	}

	if subcommand == "status" {
		installed, err := daemonservice.Installed(plan, deps.fs)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "looperd service status: %v\n", err)
			return 1
		}
		state := "not installed"
		if installed {
			state = "installed"
		}
		// The unit path is the whole of what is known here. The current invocation's
		// configuration is not persisted state and must not be presented as such.
		_, _ = fmt.Fprintf(stdout, "%s service: %s\n  unit: %s\n", plan.Manager, state, plan.UnitPath)
		if !installed {
			return 1
		}
		_, _ = fmt.Fprintln(stdout, "  (run `looper status` for daemon health)")
		return 0
	}

	result, err := daemonservice.Uninstall(ctx, plan, deps.fs, deps.run)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "looperd service uninstall: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "removed %s service\n  unit: %s\n", result.Manager, result.UnitPath)
	return 0
}

// rejectTransientExecutable refuses to record a path that will not exist later.
// `go run` builds into a temporary directory that is deleted when it exits, so
// the resulting unit would point at nothing.
func rejectTransientExecutable(path string) error {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		resolved = path
	}
	tempRoot, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		tempRoot = os.TempDir()
	}
	for _, root := range []string{tempRoot, os.TempDir()} {
		root = strings.TrimSuffix(root, string(os.PathSeparator))
		if root == "" {
			continue
		}
		if resolved == root || strings.HasPrefix(resolved, root+string(os.PathSeparator)) {
			return fmt.Errorf("refusing to install %s: it is under the temporary directory and will not exist when the supervisor next starts it (install a built binary, not `go run`)", path)
		}
	}
	return nil
}

// writeLoginCaveat states the limit of a per-user service, because "installed"
// reads as "always on" and that is not what either supervisor provides.
func writeLoginCaveat(stdout io.Writer, manager daemonservice.Manager) {
	if manager == daemonservice.ManagerLaunchd {
		_, _ = fmt.Fprintln(stdout, "\nThis is a per-user LaunchAgent: it starts at login and stops at logout.\nFor a machine that must run unattended, enable automatic login — a LaunchAgent\ncannot run for a logged-out user.")
		return
	}
	_, _ = fmt.Fprintln(stdout, "\nThis is a systemd user unit: it starts at login and stops at logout unless\nlingering is enabled. Run `loginctl enable-linger $USER` for a machine that\nmust run unattended.")
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
	looperd service install [config flags]   Write the service unit and load it
	looperd service print   [config flags]   Print the unit that would be installed
	looperd service uninstall                Unload the service and remove its unit
	looperd service status                   Report whether the unit is installed

Installs a launchd agent (macOS) or systemd user unit (Linux) from your
configuration. Set daemon.mode before installing.

"service" must be the first argument, and configuration flags follow the
subcommand. uninstall and status read no configuration: they address the
canonical service location directly, so they work when the configuration does
not load.
`, "\n"))
}
