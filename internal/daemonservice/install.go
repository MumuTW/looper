package daemonservice

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Runner executes an activation command. Injected so installation can be tested
// without loading a real service into the running user's session.
type Runner func(ctx context.Context, name string, args ...string) (output string, err error)

// FS is the filesystem surface installation needs, injected for the same reason.
type FS interface {
	MkdirAll(path string, perm os.FileMode) error
	WriteFile(path string, data []byte, perm os.FileMode) error
	Remove(path string) error
	Stat(path string) (os.FileInfo, error)
}

// OSFS is the real filesystem.
type OSFS struct{}

func (OSFS) MkdirAll(path string, perm os.FileMode) error { return os.MkdirAll(path, perm) }
func (OSFS) WriteFile(path string, data []byte, perm os.FileMode) error {
	return os.WriteFile(path, data, perm)
}
func (OSFS) Remove(path string) error              { return os.Remove(path) }
func (OSFS) Stat(path string) (os.FileInfo, error) { return os.Stat(path) }

// Result reports what installation actually did, so the caller can print it
// rather than claim success generically.
type Result struct {
	UnitPath string
	Manager  Manager
	// Commands are the activation commands that ran, in order.
	Commands []string
}

// Install writes the unit and loads it. It is idempotent: reinstalling over an
// existing service replaces the unit and restarts it.
func Install(ctx context.Context, plan Plan, fs FS, run Runner) (Result, error) {
	if fs == nil {
		fs = OSFS{}
	}
	// The supervisor redirects output into LogDir and fails to start the daemon
	// when it does not exist, which surfaces as a service that flaps rather than
	// as an error anyone can read.
	if err := fs.MkdirAll(plan.LogDir, 0o700); err != nil {
		return Result{}, fmt.Errorf("create log directory %s: %w", plan.LogDir, err)
	}
	if err := fs.MkdirAll(filepath.Dir(plan.UnitPath), 0o700); err != nil {
		return Result{}, fmt.Errorf("create service directory %s: %w", filepath.Dir(plan.UnitPath), err)
	}
	if err := fs.WriteFile(plan.UnitPath, []byte(plan.Unit), os.FileMode(plan.FileMode)); err != nil {
		return Result{}, fmt.Errorf("write %s: %w", plan.UnitPath, err)
	}

	result := Result{UnitPath: plan.UnitPath, Manager: plan.Manager}
	for i, command := range plan.Activate {
		if len(command) == 0 {
			continue
		}
		_, err := run(ctx, command[0], command[1:]...)
		result.Commands = append(result.Commands, strings.Join(command, " "))
		if err == nil {
			continue
		}
		// The first launchd command is a bootout that clears any previously loaded
		// label. It fails when nothing is loaded, which is the ordinary
		// first-install case, so only later commands are treated as fatal.
		if plan.Manager == ManagerLaunchd && i == 0 {
			continue
		}
		return result, fmt.Errorf("%s: %w", strings.Join(command, " "), err)
	}
	return result, nil
}

// Uninstall unloads the service and removes the unit. A service that is already
// gone is not an error: the desired end state is what matters.
func Uninstall(ctx context.Context, plan Plan, fs FS, run Runner) (Result, error) {
	if fs == nil {
		fs = OSFS{}
	}
	result := Result{UnitPath: plan.UnitPath, Manager: plan.Manager}
	for _, command := range plan.Deactivate {
		if len(command) == 0 {
			continue
		}
		// Deactivation failures are expected when the service was never loaded, and
		// stopping here would leave the unit file behind — the opposite of what the
		// caller asked for.
		_, _ = run(ctx, command[0], command[1:]...)
		result.Commands = append(result.Commands, strings.Join(command, " "))
	}
	if err := fs.Remove(plan.UnitPath); err != nil && !os.IsNotExist(err) {
		return result, fmt.Errorf("remove %s: %w", plan.UnitPath, err)
	}
	return result, nil
}

// Installed reports whether the unit file exists. It deliberately does not ask
// the supervisor whether the service is running: that is what `looper status`
// and the daemon's own health endpoint answer.
func Installed(plan Plan, fs FS) bool {
	if fs == nil {
		fs = OSFS{}
	}
	_, err := fs.Stat(plan.UnitPath)
	return err == nil
}
