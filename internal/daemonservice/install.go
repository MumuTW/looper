package daemonservice

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Runner executes an activation command and returns its combined output.
// Injected so installation is testable without loading a service into the
// running user's session.
type Runner func(ctx context.Context, name string, args ...string) (output string, err error)

// FS is the filesystem surface installation needs.
type FS interface {
	MkdirAll(path string, perm os.FileMode) error
	// CreateExclusive creates the file, failing if it already exists. Exclusive
	// creation is the whole of invariant I2: a Stat followed by a Write is two
	// operations, and another installer can act between them.
	CreateExclusive(path string, data []byte, perm os.FileMode) error
	Remove(path string) error
	Stat(path string) (os.FileInfo, error)
}

// OSFS is the real filesystem.
type OSFS struct{}

func (OSFS) MkdirAll(path string, perm os.FileMode) error { return os.MkdirAll(path, perm) }

func (OSFS) CreateExclusive(path string, data []byte, perm os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.Write(data)
	return err
}

func (OSFS) Remove(path string) error              { return os.Remove(path) }
func (OSFS) Stat(path string) (os.FileInfo, error) { return os.Stat(path) }

// ErrAlreadyInstalled reports that a unit is already present. Replacing one is
// uninstall then install, so an active service is never silently redefined.
var ErrAlreadyInstalled = errors.New("a service unit is already installed")

// Result reports what actually happened, so a caller states that rather than
// claiming success generically.
type Result struct {
	UnitPath string
	Manager  Manager
	Commands []string
}

// Install writes the unit and activates it.
//
// There is no rollback. If activation fails the unit is left in place and the
// failing step is reported: a rollback can itself fail, and the failure mode of
// a failed rollback is deleting the only unit while the supervisor still has the
// service loaded — strictly worse than leaving a unit that `uninstall` removes.
func Install(ctx context.Context, plan Plan, fs FS, run Runner) (Result, error) {
	if fs == nil {
		fs = OSFS{}
	}
	// The supervisor redirects output into LogDir and fails to start the daemon
	// when it is missing, which surfaces as a service that flaps rather than an
	// error anyone can read.
	if err := fs.MkdirAll(plan.LogDir, 0o700); err != nil {
		return Result{}, fmt.Errorf("create log directory %s: %w", plan.LogDir, err)
	}
	if err := fs.MkdirAll(filepath.Dir(plan.UnitPath), 0o700); err != nil {
		return Result{}, fmt.Errorf("create service directory %s: %w", filepath.Dir(plan.UnitPath), err)
	}
	if err := fs.CreateExclusive(plan.UnitPath, []byte(plan.Unit), 0o644); err != nil {
		if os.IsExist(err) {
			return Result{}, fmt.Errorf("%w at %s: uninstall it first to replace it", ErrAlreadyInstalled, plan.UnitPath)
		}
		return Result{}, fmt.Errorf("write %s: %w", plan.UnitPath, err)
	}

	result := Result{UnitPath: plan.UnitPath, Manager: plan.Manager}
	for _, command := range plan.Activate {
		if len(command) == 0 {
			continue
		}
		output, err := run(ctx, command[0], command[1:]...)
		result.Commands = append(result.Commands, strings.Join(command, " "))
		if err != nil {
			return result, fmt.Errorf("%s: %w%s", strings.Join(command, " "), err, formatOutput(output))
		}
	}
	return result, nil
}

// Uninstall deactivates the service and removes its unit.
//
// Deactivation is attempted whether or not the unit file exists (U1): the file
// being gone does not prove the supervisor has forgotten the service, and a
// service still loaded from a deleted unit is exactly the state nothing else can
// clean up. The file is removed only after deactivation succeeds (U2), so a
// loaded service never loses its definition.
func Uninstall(ctx context.Context, plan Plan, fs FS, run Runner) (Result, error) {
	if fs == nil {
		fs = OSFS{}
	}
	result := Result{UnitPath: plan.UnitPath, Manager: plan.Manager}
	for _, command := range plan.Deactivate {
		if len(command) == 0 {
			continue
		}
		// The unit file must be gone before systemd's final daemon-reload, so the
		// removal happens between the deactivation commands rather than after them.
		if plan.Manager == ManagerSystemd && command[len(command)-1] == "daemon-reload" {
			if err := removeUnit(fs, plan.UnitPath); err != nil {
				return result, err
			}
		}
		output, err := run(ctx, command[0], command[1:]...)
		result.Commands = append(result.Commands, strings.Join(command, " "))
		if err != nil {
			if isNotLoaded(output, err) {
				// Nothing was loaded. That is the desired end state for this step, not
				// a failure, and it is the ordinary case for a partial install.
				continue
			}
			return result, fmt.Errorf("%s: %w%s\nthe unit was left in place at %s", strings.Join(command, " "), err, formatOutput(output), plan.UnitPath)
		}
	}
	if err := removeUnit(fs, plan.UnitPath); err != nil {
		return result, err
	}
	return result, nil
}

func removeUnit(fs FS, path string) error {
	if err := fs.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}

// isNotLoaded reports whether a deactivation failed only because there was
// nothing loaded. Anything else means the supervisor still holds the service and
// its unit must stay.
func isNotLoaded(output string, err error) bool {
	haystack := strings.ToLower(output + " " + err.Error())
	for _, phrase := range []string{
		"no such process",        // launchctl bootout, nothing loaded
		"could not find service", // launchctl, unknown label
		"not loaded",             // systemctl disable
		"does not exist",         // systemctl, unknown unit
		"no such file or directory",
	} {
		if strings.Contains(haystack, phrase) {
			return true
		}
	}
	return false
}

// Installed reports whether a unit is present. A stat failure is returned rather
// than reported as "not installed": an unreadable service directory is not the
// same answer as an absent service, and conflating them makes uninstall look
// unnecessary when it is not.
func Installed(plan Plan, fs FS) (bool, error) {
	if fs == nil {
		fs = OSFS{}
	}
	if _, err := fs.Stat(plan.UnitPath); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("inspect %s: %w", plan.UnitPath, err)
	}
	return true, nil
}

// formatOutput appends a supervisor's own message. launchctl and systemctl
// explain refusals in their output, and an exit status alone is not actionable.
func formatOutput(output string) string {
	output = strings.TrimSpace(output)
	if output == "" {
		return ""
	}
	return "\n" + output
}
