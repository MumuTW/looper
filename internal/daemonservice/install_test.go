package daemonservice

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/config"
)

type fakeFS struct {
	files     map[string][]byte
	modes     map[string]os.FileMode
	dirs      []string
	statErr   error
	removeErr error
}

func newFakeFS() *fakeFS {
	return &fakeFS{files: map[string][]byte{}, modes: map[string]os.FileMode{}}
}

func (f *fakeFS) MkdirAll(path string, _ os.FileMode) error {
	f.dirs = append(f.dirs, path)
	return nil
}

func (f *fakeFS) CreateExclusive(path string, data []byte, perm os.FileMode) error {
	if _, exists := f.files[path]; exists {
		return os.ErrExist
	}
	f.files[path] = data
	f.modes[path] = perm
	return nil
}

func (f *fakeFS) Remove(path string) error {
	if f.removeErr != nil {
		return f.removeErr
	}
	if _, exists := f.files[path]; !exists {
		return os.ErrNotExist
	}
	delete(f.files, path)
	return nil
}

func (f *fakeFS) Stat(path string) (os.FileInfo, error) {
	if f.statErr != nil {
		return nil, f.statErr
	}
	if _, exists := f.files[path]; !exists {
		return nil, os.ErrNotExist
	}
	return fakeFileInfo{}, nil
}

type fakeFileInfo struct{}

func (fakeFileInfo) Name() string       { return "unit" }
func (fakeFileInfo) Size() int64        { return 0 }
func (fakeFileInfo) Mode() os.FileMode  { return 0o644 }
func (fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (fakeFileInfo) IsDir() bool        { return false }
func (fakeFileInfo) Sys() any           { return nil }

type recordingRunner struct {
	commands []string
	fail     map[string]error
	output   map[string]string
	// fileAt records the unit's presence when each command ran, which is how the
	// ordering invariants are asserted.
	fileAt map[string]bool
	fs     *fakeFS
	path   string
}

func (r *recordingRunner) run(_ context.Context, name string, args ...string) (string, error) {
	command := strings.TrimSpace(name + " " + strings.Join(args, " "))
	r.commands = append(r.commands, command)
	if r.fileAt != nil && r.fs != nil {
		_, exists := r.fs.files[r.path]
		r.fileAt[command] = exists
	}
	for prefix, err := range r.fail {
		if strings.HasPrefix(command, prefix) {
			return r.output[prefix], err
		}
	}
	return "", nil
}

func planFor(t *testing.T, goos string) Plan {
	t.Helper()
	plan, err := Build(testInput(goos, nil))
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	return plan
}

func TestInstallWritesTheUnitAndActivatesIt(t *testing.T) {
	t.Parallel()
	plan := planFor(t, "darwin")
	fs := newFakeFS()
	runner := &recordingRunner{}

	result, err := Install(context.Background(), plan, fs, runner.run)
	if err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	if string(fs.files[plan.UnitPath]) != plan.Unit {
		t.Fatal("Install() did not write the planned unit")
	}
	if len(runner.commands) != len(plan.Activate) {
		t.Fatalf("commands = %v", runner.commands)
	}
	if result.UnitPath != plan.UnitPath {
		t.Fatalf("result = %+v", result)
	}
}

// I2: install never modifies or deletes an existing unit. Exclusive creation is
// what makes this hold against another installer racing between check and write.
func TestInstallRefusesAnExistingUnitWithoutTouchingIt(t *testing.T) {
	t.Parallel()
	plan := planFor(t, "darwin")
	fs := newFakeFS()
	fs.files[plan.UnitPath] = []byte("a unit somebody else installed")
	runner := &recordingRunner{}

	_, err := Install(context.Background(), plan, fs, runner.run)

	if !errors.Is(err, ErrAlreadyInstalled) {
		t.Fatalf("Install() error = %v, want ErrAlreadyInstalled", err)
	}
	if string(fs.files[plan.UnitPath]) != "a unit somebody else installed" {
		t.Fatal("Install() overwrote an existing unit")
	}
	if len(runner.commands) != 0 {
		t.Fatalf("Install() activated over an existing unit: %v", runner.commands)
	}
}

// I1: a failed activation reports which step failed. There is deliberately no
// rollback — a rollback that itself fails deletes the only unit while the
// supervisor still holds the service.
func TestInstallLeavesTheUnitWhenActivationFails(t *testing.T) {
	t.Parallel()
	plan := planFor(t, "darwin")
	fs := newFakeFS()
	runner := &recordingRunner{
		fail:   map[string]error{"launchctl bootstrap": errors.New("exit status 5")},
		output: map[string]string{"launchctl bootstrap": "Load failed: 5: Input/output error"},
	}

	_, err := Install(context.Background(), plan, fs, runner.run)

	if err == nil {
		t.Fatal("Install() reported success after activation failed")
	}
	if !strings.Contains(err.Error(), "launchctl bootstrap") {
		t.Fatalf("error does not name the failing step: %v", err)
	}
	// The supervisor's own message is the only actionable part of the failure.
	if !strings.Contains(err.Error(), "Input/output error") {
		t.Fatalf("error discards the supervisor's output: %v", err)
	}
	if _, exists := fs.files[plan.UnitPath]; !exists {
		t.Fatal("Install() removed the unit after a failed activation")
	}
}

// U1: a missing unit file does not prove the supervisor has forgotten the
// service, so deactivation is attempted regardless.
func TestUninstallDeactivatesEvenWithNoUnitFile(t *testing.T) {
	t.Parallel()
	plan := planFor(t, "darwin")
	runner := &recordingRunner{}

	if _, err := Uninstall(context.Background(), plan, newFakeFS(), runner.run); err != nil {
		t.Fatalf("Uninstall() error = %v", err)
	}
	if len(runner.commands) == 0 {
		t.Fatal("Uninstall() skipped deactivation because the file was absent")
	}
}

// U2: a service that could not be deactivated keeps its unit. Removing it would
// leave a loaded service with no definition, which nothing can then clean up.
func TestUninstallKeepsTheUnitWhenDeactivationFails(t *testing.T) {
	t.Parallel()
	plan := planFor(t, "darwin")
	fs := newFakeFS()
	fs.files[plan.UnitPath] = []byte(plan.Unit)
	runner := &recordingRunner{
		fail:   map[string]error{"launchctl bootout": errors.New("exit status 1")},
		output: map[string]string{"launchctl bootout": "Boot-out failed: 36: Operation now in progress"},
	}

	_, err := Uninstall(context.Background(), plan, fs, runner.run)

	if err == nil {
		t.Fatal("Uninstall() reported success after deactivation failed")
	}
	if _, exists := fs.files[plan.UnitPath]; !exists {
		t.Fatal("Uninstall() removed the unit of a service it could not deactivate")
	}
	if !strings.Contains(err.Error(), "left in place") {
		t.Fatalf("error does not say the unit was kept: %v", err)
	}
}

// U3, and the ordinary case after a partial install: nothing loaded is the
// desired end state, not a failure.
func TestUninstallSucceedsWhenNothingWasLoaded(t *testing.T) {
	t.Parallel()
	plan := planFor(t, "darwin")
	fs := newFakeFS()
	fs.files[plan.UnitPath] = []byte(plan.Unit)
	runner := &recordingRunner{
		fail:   map[string]error{"launchctl bootout": errors.New("exit status 3")},
		output: map[string]string{"launchctl bootout": "Boot-out failed: 3: No such process"},
	}

	if _, err := Uninstall(context.Background(), plan, fs, runner.run); err != nil {
		t.Fatalf("Uninstall() error = %v, want a not-loaded refusal tolerated", err)
	}
	if _, exists := fs.files[plan.UnitPath]; exists {
		t.Fatal("Uninstall() left the unit behind")
	}
}

func TestUninstallIsIdempotent(t *testing.T) {
	t.Parallel()
	plan := planFor(t, "darwin")

	if _, err := Uninstall(context.Background(), plan, newFakeFS(), (&recordingRunner{}).run); err != nil {
		t.Fatalf("Uninstall() on a missing unit error = %v", err)
	}
}

// systemd re-reads unit files on daemon-reload, so the file has to be gone
// before the final reload or the removal is not observed.
func TestSystemdUninstallRemovesTheUnitBeforeTheFinalReload(t *testing.T) {
	t.Parallel()
	plan := planFor(t, "linux")
	fs := newFakeFS()
	fs.files[plan.UnitPath] = []byte(plan.Unit)
	runner := &recordingRunner{fileAt: map[string]bool{}, fs: fs, path: plan.UnitPath}

	if _, err := Uninstall(context.Background(), plan, fs, runner.run); err != nil {
		t.Fatalf("Uninstall() error = %v", err)
	}

	if present, ok := runner.fileAt["systemctl --user disable --now looperd.service"]; !ok || !present {
		t.Fatalf("disable ran without the unit present (fileAt=%v)", runner.fileAt)
	}
	if present, ok := runner.fileAt["systemctl --user daemon-reload"]; !ok || present {
		t.Fatalf("daemon-reload ran while the unit was still on disk (fileAt=%v)", runner.fileAt)
	}
}

// An unreadable service directory is not the same answer as an absent service:
// conflating them makes uninstall look unnecessary when it is not.
func TestInstalledPropagatesInspectionFailures(t *testing.T) {
	t.Parallel()
	plan := planFor(t, "darwin")
	fs := newFakeFS()
	fs.statErr = errors.New("permission denied")

	installed, err := Installed(plan, fs)

	if err == nil {
		t.Fatal("Installed() swallowed a stat failure")
	}
	if installed {
		t.Fatal("Installed() = true on a failed inspection")
	}
}

func TestInstalledReportsPresence(t *testing.T) {
	t.Parallel()
	plan := planFor(t, "darwin")
	fs := newFakeFS()

	if installed, err := Installed(plan, fs); err != nil || installed {
		t.Fatalf("Installed() = (%v, %v) before install", installed, err)
	}
	if _, err := Install(context.Background(), plan, fs, (&recordingRunner{}).run); err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	if installed, err := Installed(plan, fs); err != nil || !installed {
		t.Fatalf("Installed() = (%v, %v) after install", installed, err)
	}
}

var _ = config.DaemonModeLaunchd
