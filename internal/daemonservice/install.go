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

// Install writes a previously absent unit and loads it. An existing unit is
// refused rather than overwritten: the requested config authorizes creating
// Looper's service, not replacing an unknown service at the same path. Remove
// the existing unit with the explicit uninstall command before installing again.
func Install(ctx context.Context, plan Plan, fs FS, run Runner) (Result, error) {
	if fs == nil {
		fs = OSFS{}
	}
	if _, err := fs.Stat(plan.UnitPath); err == nil {
		return Result{}, fmt.Errorf("refuse to overwrite existing service unit %s; inspect it or uninstall it explicitly first", plan.UnitPath)
	} else if !os.IsNotExist(err) {
		return Result{}, fmt.Errorf("inspect existing service unit %s: %w", plan.UnitPath, err)
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
	for _, command := range plan.Activate {
		if len(command) == 0 {
			continue
		}
		_, err := run(ctx, command[0], command[1:]...)
		result.Commands = append(result.Commands, strings.Join(command, " "))
		if err == nil {
			continue
		}
		if removeErr := fs.Remove(plan.UnitPath); removeErr != nil && !os.IsNotExist(removeErr) {
			return result, fmt.Errorf("%s: %w (remove failed service unit: %v)", strings.Join(command, " "), err, removeErr)
		}
		return result, fmt.Errorf("%s: %w (removed newly written service unit)", strings.Join(command, " "), err)
	}
	return result, nil
}

// Uninstall unloads the service and then removes its unit. A missing unit is
// already uninstalled. For an existing unit, a deactivation failure is surfaced
// and the file is retained: reporting success after a supervisor refused to stop
// the daemon would leave an untracked process behind.
func Uninstall(ctx context.Context, plan Plan, fs FS, run Runner) (Result, error) {
	if fs == nil {
		fs = OSFS{}
	}
	result := Result{UnitPath: plan.UnitPath, Manager: plan.Manager}
	if _, err := fs.Stat(plan.UnitPath); err != nil {
		if os.IsNotExist(err) {
			return result, nil
		}
		return result, fmt.Errorf("inspect service unit %s: %w", plan.UnitPath, err)
	}
	for _, command := range plan.Deactivate {
		if len(command) == 0 {
			continue
		}
		_, err := run(ctx, command[0], command[1:]...)
		result.Commands = append(result.Commands, strings.Join(command, " "))
		if err != nil {
			return result, fmt.Errorf("%s: %w", strings.Join(command, " "), err)
		}
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
