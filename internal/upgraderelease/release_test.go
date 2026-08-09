package upgraderelease

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/version"
)

func TestStageVerifyAndActivateSwitchesOneCurrentPointer(t *testing.T) {
	root := t.TempDir()
	sources := t.TempDir()
	cli := writeExecutable(t, sources, "looper-source", "cli-v1")
	daemon := writeExecutable(t, sources, "looperd-source", "daemon-v1")
	first, err := Stage(StageInput{RootDir: root, ReleaseID: "1.2.3-stable-aaaaaaa", CLIBinaryPath: cli, DaemonBinaryPath: daemon, Build: testBuild("1.2.3", "aaaaaaa"), Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(root, first.ReleaseID); err != nil {
		t.Fatalf("Verify first release: %v", err)
	}
	if err := os.WriteFile(cli, []byte("changed-after-stage"), 0o755); err != nil {
		t.Fatal(err)
	}
	stagedCLI, err := os.ReadFile(filepath.Join(first.Directory, "looper"))
	if err != nil {
		t.Fatal(err)
	}
	if string(stagedCLI) != "cli-v1" {
		t.Fatalf("staged CLI = %q", stagedCLI)
	}
	activated, err := Activate(root, first.ReleaseID)
	if err != nil {
		t.Fatal(err)
	}
	if activated.PreviousReleaseID != "" || activated.CurrentReleaseID != first.ReleaseID {
		t.Fatalf("first activation = %#v", activated)
	}
	assertCurrentTarget(t, root, first.ReleaseID)
	if current, err := CurrentReleaseID(root); err != nil || current != first.ReleaseID {
		t.Fatalf("CurrentReleaseID() = (%q, %v)", current, err)
	}

	secondCLI := writeExecutable(t, sources, "looper-source-v2", "cli-v2")
	secondDaemon := writeExecutable(t, sources, "looperd-source-v2", "daemon-v2")
	second, err := Stage(StageInput{RootDir: root, ReleaseID: "1.2.4-stable-bbbbbbb", CLIBinaryPath: secondCLI, DaemonBinaryPath: secondDaemon, Build: testBuild("1.2.4", "bbbbbbb"), Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	activated, err = Activate(root, second.ReleaseID)
	if err != nil {
		t.Fatal(err)
	}
	if activated.PreviousReleaseID != first.ReleaseID || activated.CurrentReleaseID != second.ReleaseID {
		t.Fatalf("second activation = %#v", activated)
	}
	assertCurrentTarget(t, root, second.ReleaseID)
	if current, err := CurrentReleaseID(root); err != nil || current != second.ReleaseID {
		t.Fatalf("CurrentReleaseID() = (%q, %v)", current, err)
	}
	ids, err := ReleaseIDs(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != first.ReleaseID || ids[1] != second.ReleaseID {
		t.Fatalf("release IDs = %v", ids)
	}
}

func TestStageReusesIdenticalExistingRelease(t *testing.T) {
	root := t.TempDir()
	sources := t.TempDir()
	cli := writeExecutable(t, sources, "looper-source", "cli-v1")
	daemon := writeExecutable(t, sources, "looperd-source", "daemon-v1")
	build := testBuild("1.2.3", "aaaaaaa")
	first, err := Stage(StageInput{RootDir: root, ReleaseID: "1.2.3-stable-aaaaaaa", CLIBinaryPath: cli, DaemonBinaryPath: daemon, Build: build, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	// Later cutover re-stages the live pair (same id + bytes).
	again, err := Stage(StageInput{RootDir: root, ReleaseID: first.ReleaseID, CLIBinaryPath: cli, DaemonBinaryPath: daemon, Build: build, Now: fixedNow})
	if err != nil {
		t.Fatalf("idempotent Stage() error = %v", err)
	}
	if again.Directory != first.Directory || again.ReleaseID != first.ReleaseID {
		t.Fatalf("reuse = %#v, want %#v", again, first)
	}
	// Different bytes must still fail.
	changed := writeExecutable(t, sources, "looper-changed", "changed")
	if _, err := Stage(StageInput{RootDir: root, ReleaseID: first.ReleaseID, CLIBinaryPath: changed, DaemonBinaryPath: daemon, Build: build, Now: fixedNow}); err == nil {
		t.Fatal("Stage() error = nil for conflicting existing release")
	}
}

func TestStageRetriesDurabilitySyncWhenReusingPublishedRelease(t *testing.T) {
	root := t.TempDir()
	sources := t.TempDir()
	cli := writeExecutable(t, sources, "looper-source", "cli-v1")
	daemon := writeExecutable(t, sources, "looperd-source", "daemon-v1")
	input := StageInput{RootDir: root, ReleaseID: "1.2.3-stable-aaaaaaa", CLIBinaryPath: cli, DaemonBinaryPath: daemon, Build: testBuild("1.2.3", "aaaaaaa"), Now: fixedNow}
	originalSync := stageSyncDirectory
	t.Cleanup(func() { stageSyncDirectory = originalSync })
	calls := 0
	stageSyncDirectory = func(path string) error {
		calls++
		if calls == 1 {
			return errors.New("injected releases directory sync failure")
		}
		return originalSync(path)
	}
	if _, err := Stage(input); err == nil || !strings.Contains(err.Error(), "injected releases directory sync failure") {
		t.Fatalf("first Stage() error = %v, want publish sync failure", err)
	}
	result, err := Stage(input)
	if err != nil {
		t.Fatalf("retry Stage() error = %v", err)
	}
	if result.ReleaseID != input.ReleaseID {
		t.Fatalf("retry release ID = %q, want %q", result.ReleaseID, input.ReleaseID)
	}
	if calls != 2 {
		t.Fatalf("stage sync calls = %d, want initial failure plus retry", calls)
	}
}

func TestActivateRestoresPreviousPointerAtomicallyWhenSyncFails(t *testing.T) {
	root := t.TempDir()
	sources := t.TempDir()
	first, err := Stage(StageInput{RootDir: root, ReleaseID: "1.2.3-stable-aaaaaaa", CLIBinaryPath: writeExecutable(t, sources, "looper", "cli-v1"), DaemonBinaryPath: writeExecutable(t, sources, "looperd", "daemon-v1"), Build: testBuild("1.2.3", "aaaaaaa"), Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Activate(root, first.ReleaseID); err != nil {
		t.Fatal(err)
	}
	second, err := Stage(StageInput{RootDir: root, ReleaseID: "1.2.4-stable-bbbbbbb", CLIBinaryPath: writeExecutable(t, sources, "looper-v2", "cli-v2"), DaemonBinaryPath: writeExecutable(t, sources, "looperd-v2", "daemon-v2"), Build: testBuild("1.2.4", "bbbbbbb"), Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	originalSync := activateSyncDirectory
	t.Cleanup(func() { activateSyncDirectory = originalSync })
	calls := 0
	activateSyncDirectory = func(string) error {
		calls++
		if calls == 1 {
			return errors.New("injected current sync failure")
		}
		return nil
	}
	if _, err := Activate(root, second.ReleaseID); err == nil || !strings.Contains(err.Error(), "injected current sync failure") {
		t.Fatalf("Activate() error = %v, want sync failure", err)
	}
	if current, err := CurrentReleaseID(root); err != nil || current != first.ReleaseID {
		t.Fatalf("CurrentReleaseID() after failed activate = (%q, %v), want previous %q", current, err, first.ReleaseID)
	}
	if entries, err := os.ReadDir(root); err != nil {
		t.Fatal(err)
	} else {
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".current-") {
				t.Fatalf("temporary current pointer remains: %s", entry.Name())
			}
		}
	}
}

func TestStageRejectsSymlinkedExistingReleaseDirectory(t *testing.T) {
	root := t.TempDir()
	sources := t.TempDir()
	input := StageInput{
		RootDir:          root,
		ReleaseID:        "1.2.3-stable-aaaaaaa",
		CLIBinaryPath:    writeExecutable(t, sources, "looper", "cli"),
		DaemonBinaryPath: writeExecutable(t, sources, "looperd", "daemon"),
		Build:            testBuild("1.2.3", "aaaaaaa"),
		Now:              fixedNow,
	}
	if err := os.MkdirAll(filepath.Join(root, "releases"), 0o755); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "release")
	if err := os.MkdirAll(external, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(root, "releases", input.ReleaseID)); err != nil {
		t.Fatal(err)
	}
	if _, err := Stage(input); err == nil || !strings.Contains(err.Error(), "real directory") {
		t.Fatalf("Stage() error = %v, want symlink destination rejection", err)
	}
}

func TestVerifyAndActivateRejectSymlinkedReleaseDirectory(t *testing.T) {
	root := t.TempDir()
	sources := t.TempDir()
	input := StageInput{
		RootDir:          root,
		ReleaseID:        "1.2.3-stable-aaaaaaa",
		CLIBinaryPath:    writeExecutable(t, sources, "looper", "cli"),
		DaemonBinaryPath: writeExecutable(t, sources, "looperd", "daemon"),
		Build:            testBuild("1.2.3", "aaaaaaa"),
		Now:              fixedNow,
	}
	staged, err := Stage(input)
	if err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "release")
	if err := os.Rename(staged.Directory, external); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, staged.Directory); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(root, input.ReleaseID); err == nil || !strings.Contains(err.Error(), "real directory") {
		t.Fatalf("Verify() error = %v, want real-directory refusal", err)
	}
	if _, err := Activate(root, input.ReleaseID); err == nil || !strings.Contains(err.Error(), "real directory") {
		t.Fatalf("Activate() error = %v, want real-directory refusal", err)
	}
}

func TestVerifyRejectsChangedOrUnexpectedReleaseContents(t *testing.T) {
	root := t.TempDir()
	sources := t.TempDir()
	result, err := Stage(StageInput{RootDir: root, ReleaseID: "1.2.3-stable-aaaaaaa", CLIBinaryPath: writeExecutable(t, sources, "looper", "cli"), DaemonBinaryPath: writeExecutable(t, sources, "looperd", "daemon"), Build: testBuild("1.2.3", "aaaaaaa"), Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(result.Directory, "looper"), []byte("changed"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(root, result.ReleaseID); err == nil {
		t.Fatal("Verify changed release error = nil")
	}
	if err := os.WriteFile(filepath.Join(result.Directory, "looper"), []byte("cli"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(result.Directory, "extra"), []byte("extra"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(root, result.ReleaseID); err == nil {
		t.Fatal("Verify release with extra file error = nil")
	}
}

func TestStageRejectsConflictingExistingReleaseAndNonExecutableInput(t *testing.T) {
	root := t.TempDir()
	sources := t.TempDir()
	cli := writeExecutable(t, sources, "looper", "cli")
	daemon := writeExecutable(t, sources, "looperd", "daemon")
	input := StageInput{RootDir: root, ReleaseID: "1.2.3-stable-aaaaaaa", CLIBinaryPath: cli, DaemonBinaryPath: daemon, Build: testBuild("1.2.3", "aaaaaaa"), Now: fixedNow}
	if _, err := Stage(input); err != nil {
		t.Fatal(err)
	}
	// Identical re-stage is allowed (later cutovers). Conflicting bytes are not.
	if _, err := Stage(input); err != nil {
		t.Fatalf("identical restage error = %v", err)
	}
	otherCLI := writeExecutable(t, sources, "looper-other", "other")
	conflict := input
	conflict.CLIBinaryPath = otherCLI
	if _, err := Stage(conflict); err == nil {
		t.Fatal("conflicting Stage() error = nil")
	}
	notExecutable := filepath.Join(sources, "not-executable")
	if err := os.WriteFile(notExecutable, []byte("plain"), 0o600); err != nil {
		t.Fatal(err)
	}
	input.ReleaseID, input.CLIBinaryPath = "1.2.4-stable-bbbbbbb", notExecutable
	if _, err := Stage(input); err == nil {
		t.Fatal("non-executable Stage() error = nil")
	}
}

func TestRemoveDiscardsOnlyTheNamedRelease(t *testing.T) {
	root := t.TempDir()
	sources := t.TempDir()
	first, err := Stage(StageInput{RootDir: root, ReleaseID: "1.2.3-stable-aaaaaaa", CLIBinaryPath: writeExecutable(t, sources, "looper", "cli"), DaemonBinaryPath: writeExecutable(t, sources, "looperd", "daemon"), Build: testBuild("1.2.3", "aaaaaaa"), Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Stage(StageInput{RootDir: root, ReleaseID: "1.2.4-stable-bbbbbbb", CLIBinaryPath: writeExecutable(t, sources, "looper-2", "cli2"), DaemonBinaryPath: writeExecutable(t, sources, "looperd-2", "daemon2"), Build: testBuild("1.2.4", "bbbbbbb"), Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	if err := Remove(root, first.ReleaseID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(first.Directory); !os.IsNotExist(err) {
		t.Fatalf("removed release stat = %v, want not exist", err)
	}
	if _, err := os.Stat(second.Directory); err != nil {
		t.Fatalf("sibling release removed: %v", err)
	}
}

func writeExecutable(t *testing.T, directory, name, contents string) string {
	t.Helper()
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertCurrentTarget(t *testing.T, root, releaseID string) {
	t.Helper()
	target, err := os.Readlink(filepath.Join(root, "current"))
	if err != nil {
		t.Fatal(err)
	}
	if target != filepath.Join("releases", releaseID) {
		t.Fatalf("current target = %q", target)
	}
}

func testBuild(value, commit string) version.Info {
	return version.Info{Version: value, Metadata: version.BuildMetadata{VersionSource: "git-tag:v" + value, Channel: "stable", APIVersion: "v1", GitCommitSHA: &commit}}
}

func fixedNow() time.Time {
	return time.Date(2026, time.July, 31, 3, 4, 5, 0, time.UTC)
}
