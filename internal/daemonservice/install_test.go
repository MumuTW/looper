package daemonservice

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

type fakeFS struct {
	files    map[string][]byte
	modes    map[string]os.FileMode
	dirs     []string
	removed  []string
	writeErr error
}

func newFakeFS() *fakeFS {
	return &fakeFS{files: map[string][]byte{}, modes: map[string]os.FileMode{}}
}

func (f *fakeFS) MkdirAll(path string, _ os.FileMode) error {
	f.dirs = append(f.dirs, path)
	return nil
}

func (f *fakeFS) WriteFile(path string, data []byte, perm os.FileMode) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	f.files[path] = data
	f.modes[path] = perm
	return nil
}

func (f *fakeFS) Remove(path string) error {
	if _, ok := f.files[path]; !ok {
		return os.ErrNotExist
	}
	delete(f.files, path)
	f.removed = append(f.removed, path)
	return nil
}

func (f *fakeFS) Stat(path string) (os.FileInfo, error) {
	if _, ok := f.files[path]; !ok {
		return nil, os.ErrNotExist
	}
	return fakeFileInfo{name: path}, nil
}

type fakeFileInfo struct{ name string }

func (f fakeFileInfo) Name() string     { return f.name }
func (fakeFileInfo) Size() int64        { return 0 }
func (fakeFileInfo) Mode() os.FileMode  { return 0o600 }
func (fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (fakeFileInfo) IsDir() bool        { return false }
func (fakeFileInfo) Sys() any           { return nil }

type recordingRunner struct {
	commands []string
	failOn   map[string]error
}

func (r *recordingRunner) run(_ context.Context, name string, args ...string) (string, error) {
	command := strings.TrimSpace(name + " " + strings.Join(args, " "))
	r.commands = append(r.commands, command)
	for prefix, err := range r.failOn {
		if strings.HasPrefix(command, prefix) {
			return "", err
		}
	}
	return "", nil
}

func launchdPlan(t *testing.T) Plan {
	t.Helper()
	plan, err := Build(testInput("darwin", nil))
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	return plan
}

func TestInstallWritesTheUnitAndActivatesIt(t *testing.T) {
	t.Parallel()
	plan := launchdPlan(t)
	fs := newFakeFS()
	runner := &recordingRunner{}

	result, err := Install(context.Background(), plan, fs, runner.run)
	if err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	if string(fs.files[plan.UnitPath]) != plan.Unit {
		t.Fatal("Install() did not write the planned unit")
	}
	if fs.modes[plan.UnitPath] != 0o600 {
		t.Fatalf("unit mode = %#o, want 0600", fs.modes[plan.UnitPath])
	}
	if len(runner.commands) != len(plan.Activate) {
		t.Fatalf("commands = %v, want all activation steps", runner.commands)
	}
	if result.UnitPath != plan.UnitPath {
		t.Fatalf("result = %+v", result)
	}
}

// The log directory must exist before the supervisor starts the daemon: both
// launchd and systemd fail to redirect output into a missing directory, which
// surfaces as a service that flaps rather than an error anyone can read.
func TestInstallCreatesTheLogDirectory(t *testing.T) {
	t.Parallel()
	plan := launchdPlan(t)
	fs := newFakeFS()

	if _, err := Install(context.Background(), plan, fs, (&recordingRunner{}).run); err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	found := false
	for _, dir := range fs.dirs {
		if dir == plan.LogDir {
			found = true
		}
	}
	if !found {
		t.Fatalf("log directory was not created; dirs = %v", fs.dirs)
	}
}

// bootout fails when nothing is loaded, which is the ordinary first install.
// Treating that as fatal would make installing impossible on a clean machine.
func TestInstallToleratesBootoutFailingOnAFreshMachine(t *testing.T) {
	t.Parallel()
	plan := launchdPlan(t)
	runner := &recordingRunner{failOn: map[string]error{"launchctl bootout": errors.New("no such process")}}

	if _, err := Install(context.Background(), plan, newFakeFS(), runner.run); err != nil {
		t.Fatalf("Install() error = %v, want the initial bootout tolerated", err)
	}
	if len(runner.commands) != len(plan.Activate) {
		t.Fatalf("install stopped early: %v", runner.commands)
	}
}

// A failed bootstrap means the service is not running. Reporting success would
// leave the operator believing the daemon survives reboot when it does not.
func TestInstallFailsWhenActivationFails(t *testing.T) {
	t.Parallel()
	plan := launchdPlan(t)
	runner := &recordingRunner{failOn: map[string]error{"launchctl bootstrap": errors.New("Load failed: 5: Input/output error")}}

	if _, err := Install(context.Background(), plan, newFakeFS(), runner.run); err == nil {
		t.Fatal("Install() reported success after activation failed")
	}
}

func TestInstallFailsWhenTheUnitCannotBeWritten(t *testing.T) {
	t.Parallel()
	plan := launchdPlan(t)
	fs := newFakeFS()
	fs.writeErr = errors.New("permission denied")
	runner := &recordingRunner{}

	if _, err := Install(context.Background(), plan, fs, runner.run); err == nil {
		t.Fatal("Install() reported success after the unit could not be written")
	}
	if len(runner.commands) != 0 {
		t.Fatalf("activation ran despite no unit on disk: %v", runner.commands)
	}
}

// Uninstalling something that was never loaded must still remove the unit: the
// caller asked for it gone, and the deactivation error is expected noise.
func TestUninstallRemovesTheUnitEvenWhenDeactivationFails(t *testing.T) {
	t.Parallel()
	plan := launchdPlan(t)
	fs := newFakeFS()
	fs.files[plan.UnitPath] = []byte(plan.Unit)
	runner := &recordingRunner{failOn: map[string]error{"launchctl bootout": errors.New("no such process")}}

	if _, err := Uninstall(context.Background(), plan, fs, runner.run); err != nil {
		t.Fatalf("Uninstall() error = %v", err)
	}
	if _, exists := fs.files[plan.UnitPath]; exists {
		t.Fatal("Uninstall() left the unit on disk")
	}
}

func TestUninstallIsIdempotent(t *testing.T) {
	t.Parallel()
	plan := launchdPlan(t)

	if _, err := Uninstall(context.Background(), plan, newFakeFS(), (&recordingRunner{}).run); err != nil {
		t.Fatalf("Uninstall() on a missing unit error = %v, want success", err)
	}
}

func TestInstalledReportsUnitPresence(t *testing.T) {
	t.Parallel()
	plan := launchdPlan(t)
	fs := newFakeFS()

	if Installed(plan, fs) {
		t.Fatal("Installed() = true before install")
	}
	if _, err := Install(context.Background(), plan, fs, (&recordingRunner{}).run); err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	if !Installed(plan, fs) {
		t.Fatal("Installed() = false after install")
	}
}
