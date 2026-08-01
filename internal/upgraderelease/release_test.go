package upgraderelease

import (
	"os"
	"path/filepath"
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

func TestStageRejectsDuplicateReleaseAndNonExecutableInput(t *testing.T) {
	root := t.TempDir()
	sources := t.TempDir()
	cli := writeExecutable(t, sources, "looper", "cli")
	daemon := writeExecutable(t, sources, "looperd", "daemon")
	input := StageInput{RootDir: root, ReleaseID: "1.2.3-stable-aaaaaaa", CLIBinaryPath: cli, DaemonBinaryPath: daemon, Build: testBuild("1.2.3", "aaaaaaa"), Now: fixedNow}
	if _, err := Stage(input); err != nil {
		t.Fatal(err)
	}
	if _, err := Stage(input); err == nil {
		t.Fatal("duplicate Stage() error = nil")
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
