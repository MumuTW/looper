package e2e

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

const fakeLooperAsset = "fake looper release asset\n"

type uninstallSandbox struct {
	looperHome string
	cliPath    string
	dataPaths  []string
	profiles   []string
	env        []string
}

func TestInstallThenUninstallHonorsDataApprovalAndPreservesProfileContent(t *testing.T) {
	t.Run("unsafe state root is rejected", func(t *testing.T) {
		home := t.TempDir()
		output, err := runShellScriptResult(t, filepath.Join(repositoryRoot(t), "scripts", "uninstall.sh"), append(os.Environ(),
			"HOME="+home,
			"LOOPER_HOME=/",
			"LOOPER_INSTALL_PATH=",
		))
		if err == nil {
			t.Fatalf("uninstall with LOOPER_HOME=/ succeeded:\n%s", output)
		}
		if !strings.Contains(output, "refusing unsafe LOOPER_HOME") {
			t.Fatalf("uninstall output = %q, want unsafe-root refusal", output)
		}
	})

	t.Run("declined data remains", func(t *testing.T) {
		sandbox := newUninstallSandbox(t)
		output := sandbox.runUninstall(t, false)

		assertUninstallPromptScope(t, output, sandbox.dataPaths)
		for _, path := range sandbox.dataPaths {
			requireSandboxPath(t, path, true)
		}
		sandbox.assertInstallerFilesRemoved(t)
		sandbox.assertUnrelatedContentPreserved(t)
	})

	t.Run("approved data is removed", func(t *testing.T) {
		sandbox := newUninstallSandbox(t)
		output := sandbox.runUninstall(t, true)

		assertUninstallPromptScope(t, output, sandbox.dataPaths)
		for _, path := range sandbox.dataPaths {
			requireSandboxPath(t, path, false)
		}
		sandbox.assertInstallerFilesRemoved(t)
		sandbox.assertUnrelatedContentPreserved(t)
	})
}

func newUninstallSandbox(t *testing.T) *uninstallSandbox {
	t.Helper()
	home := t.TempDir()
	looperHome := filepath.Join(home, ".looper")
	installDir := filepath.Join(home, ".local", "bin")
	fakeBin := filepath.Join(home, "fake-bin")
	for _, dir := range []string{installDir, fakeBin, looperHome} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", dir, err)
		}
	}

	assetHash := fmt.Sprintf("%x", sha256.Sum256([]byte(fakeLooperAsset)))
	fakeCurl := fmt.Sprintf(`#!/bin/sh
set -eu
output=
url=
write_status=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) shift; output="$1" ;;
    -w) shift; write_status=1 ;;
    http://*|https://*) url="$1" ;;
  esac
  shift
done
if [ "$write_status" -eq 1 ]; then
  printf '404'
  exit 0
fi
case "$url" in
  *.sha256) printf '%s  looper\n' >"$output" ;;
  *) printf '%s' >"$output" ;;
esac
`, assetHash, fakeLooperAsset)
	if err := os.WriteFile(filepath.Join(fakeBin, "curl"), []byte(fakeCurl), 0o755); err != nil {
		t.Fatalf("write fake curl: %v", err)
	}

	repoRoot := repositoryRoot(t)
	baseEnv := append(os.Environ(),
		"HOME="+home,
		"SHELL=/bin/zsh",
		"LOOPER_HOME="+looperHome,
		"LOOPER_INSTALL_DIR="+installDir,
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	runShellScript(t, filepath.Join(repoRoot, "scripts", "install.sh"), baseEnv)
	cliPath := filepath.Join(installDir, "looper")
	requireSandboxPath(t, cliPath, true)

	profiles := []string{
		filepath.Join(home, ".zprofile"),
		filepath.Join(home, ".bash_profile"),
		filepath.Join(home, ".profile"),
	}
	for index, profile := range profiles {
		content := fmt.Sprintf("export KEEP_%d=yes\n# Added by looper installer\nexport PATH=\"/unrelated/bin:$PATH\"\n\n# Added by looper installer\nexport PATH=\"$HOME/.local/bin:$PATH\"\nalias keep_%d=true\n", index, index)
		if err := os.WriteFile(profile, []byte(content), 0o640); err != nil {
			t.Fatalf("write profile %s: %v", profile, err)
		}
	}

	for _, path := range []string{
		filepath.Join(looperHome, "config.toml"),
		filepath.Join(looperHome, "config.json"),
		filepath.Join(looperHome, "config.yaml"),
		filepath.Join(looperHome, "config.yml"),
	} {
		if err := os.WriteFile(path, []byte("test config\n"), 0o600); err != nil {
			t.Fatalf("write config %s: %v", path, err)
		}
	}

	dbPath := filepath.Join(looperHome, "looper.sqlite")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open SQLite database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; CREATE TABLE uninstall_test (id INTEGER PRIMARY KEY); INSERT INTO uninstall_test DEFAULT VALUES;`); err != nil {
		t.Fatalf("create WAL database: %v", err)
	}
	walPath := dbPath + "-wal"
	shmPath := dbPath + "-shm"
	requireSandboxPath(t, walPath, true)
	requireSandboxPath(t, shmPath, true)

	for _, dir := range []string{"backups", "logs", "worktrees", "state", filepath.Join("run", "nested")} {
		path := filepath.Join(looperHome, dir)
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", path, err)
		}
		if err := os.WriteFile(filepath.Join(path, "sentinel"), []byte("remove me\n"), 0o600); err != nil {
			t.Fatalf("write sentinel in %s: %v", path, err)
		}
	}
	for _, path := range []string{
		filepath.Join(looperHome, "bin", "looperd"),
		filepath.Join(looperHome, "bin", "looperd.prev"),
		filepath.Join(looperHome, "run", "upgrade.lock"),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte("installer managed\n"), 0o700); err != nil {
			t.Fatalf("write installer file %s: %v", path, err)
		}
	}
	if err := os.WriteFile(filepath.Join(looperHome, "keep.txt"), []byte("user owned\n"), 0o600); err != nil {
		t.Fatalf("write unrelated data: %v", err)
	}

	dataPaths := []string{
		filepath.Join(looperHome, "config.toml"),
		filepath.Join(looperHome, "config.json"),
		filepath.Join(looperHome, "config.yaml"),
		filepath.Join(looperHome, "config.yml"),
		dbPath,
		walPath,
		shmPath,
		filepath.Join(looperHome, "backups"),
		filepath.Join(looperHome, "logs"),
		filepath.Join(looperHome, "worktrees"),
	}
	return &uninstallSandbox{
		looperHome: looperHome, cliPath: cliPath,
		dataPaths: dataPaths, profiles: profiles,
		env: append(baseEnv,
			"LOOPER_INSTALL_PATH="+cliPath,
			"PATH="+installDir+string(os.PathListSeparator)+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		),
	}
}

func (s *uninstallSandbox) runUninstall(t *testing.T, approve bool) string {
	t.Helper()
	env := append([]string(nil), s.env...)
	if approve {
		env = append(env, "LOOPER_UNINSTALL_YES=1")
	}
	return runShellScript(t, filepath.Join(repositoryRoot(t), "scripts", "uninstall.sh"), env)
}

func (s *uninstallSandbox) assertInstallerFilesRemoved(t *testing.T) {
	t.Helper()
	for _, path := range []string{
		s.cliPath,
		filepath.Join(s.looperHome, "bin", "looperd"),
		filepath.Join(s.looperHome, "bin", "looperd.prev"),
		filepath.Join(s.looperHome, "state"),
		filepath.Join(s.looperHome, "run", "upgrade.lock"),
	} {
		requireSandboxPath(t, path, false)
	}
}

func (s *uninstallSandbox) assertUnrelatedContentPreserved(t *testing.T) {
	t.Helper()
	requireSandboxPath(t, filepath.Join(s.looperHome, "keep.txt"), true)
	for index, profile := range s.profiles {
		contents, err := os.ReadFile(profile)
		if err != nil {
			t.Fatalf("read profile %s: %v", profile, err)
		}
		text := string(contents)
		for _, want := range []string{
			fmt.Sprintf("export KEEP_%d=yes", index),
			"# Added by looper installer\nexport PATH=\"/unrelated/bin:$PATH\"",
			fmt.Sprintf("alias keep_%d=true", index),
		} {
			if !strings.Contains(text, want) {
				t.Fatalf("profile %s = %q, want unrelated content %q preserved", profile, text, want)
			}
		}
		if strings.Contains(text, `export PATH="$HOME/.local/bin:$PATH"`) {
			t.Fatalf("profile %s still contains installer-managed PATH stanza: %q", profile, text)
		}
	}
}

func assertUninstallPromptScope(t *testing.T, output string, paths []string) {
	t.Helper()
	for _, path := range paths {
		if !strings.Contains(output, path) {
			t.Fatalf("uninstall output omitted data path %s:\n%s", path, output)
		}
	}
	if strings.Contains(output, "keep.txt") {
		t.Fatalf("uninstall output included unrelated data:\n%s", output)
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller could not locate repository")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
}

func runShellScript(t *testing.T, path string, env []string) string {
	t.Helper()
	output, err := runShellScriptResult(t, path, env)
	if err != nil {
		t.Fatalf("run %s: %v\n%s", path, err, output)
	}
	return output
}

func runShellScriptResult(t *testing.T, path string, env []string) (string, error) {
	t.Helper()
	cmd := exec.Command("/bin/sh", path)
	cmd.Env = env
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	err := cmd.Run()
	return output.String(), err
}

func requireSandboxPath(t *testing.T, path string, wantExists bool) {
	t.Helper()
	_, err := os.Lstat(path)
	if wantExists && err != nil {
		t.Fatalf("expected %s to exist: %v", path, err)
	}
	if !wantExists && !os.IsNotExist(err) {
		t.Fatalf("expected %s to be absent, got err=%v", path, err)
	}
}
