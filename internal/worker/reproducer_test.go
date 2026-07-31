package worker

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MumuTW/looper/internal/reproducer"
)

func writeWorkerReproductionFixture(t *testing.T) (string, *reproducer.Manifest) {
	t.Helper()
	root := t.TempDir()
	testPath := filepath.Join(root, "internal", "bug_test.go")
	if err := os.MkdirAll(filepath.Dir(testPath), 0o755); err != nil {
		t.Fatal(err)
	}
	testContent := []byte("func TestBug(t *testing.T) {}\n")
	if err := os.WriteFile(testPath, testContent, 0o644); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(testContent)
	manifest := &reproducer.Manifest{Version: 1, TestPath: "internal/bug_test.go", TestName: "TestBug", TestCommand: "go test ./internal -run '^TestBug$'", TestSHA256: hex.EncodeToString(hash[:])}
	if err := os.Mkdir(filepath.Join(root, ".looper"), 0o755); err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"version":1,"testPath":"internal/bug_test.go","testName":"TestBug","testCommand":"go test ./internal -run '^TestBug$'","testSha256":"` + manifest.TestSHA256 + `"}`)
	if err := os.WriteFile(filepath.Join(root, reproducer.ManifestPath), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return root, manifest
}

func TestWorkerReproductionCapturePersistsAndDetectsTampering(t *testing.T) {
	root, expected := writeWorkerReproductionFixture(t)
	checkpoint := workerCheckpoint{Work: &workerInput{}}
	if err := captureWorkerReproduction(&checkpoint, root); err != nil {
		t.Fatalf("captureWorkerReproduction() error = %v", err)
	}
	if checkpoint.Work.Reproduction == nil || !checkpoint.Work.Reproduction.Equal(*expected) {
		t.Fatalf("captured reproduction = %#v, want %#v", checkpoint.Work.Reproduction, expected)
	}
	if err := verifyWorkerReproduction(checkpoint, root); err != nil {
		t.Fatalf("verifyWorkerReproduction() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, expected.TestPath), []byte("func TestBug(t *testing.T) { t.Skip() }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := verifyWorkerReproduction(checkpoint, root)
	if err == nil || !strings.Contains(err.Error(), "integrity check failed") || !strings.Contains(err.Error(), "modified") {
		t.Fatalf("verifyWorkerReproduction() after tamper = %v, want integrity failure", err)
	}
	if got := err.(*loopError).kind; got != FailureManualIntervention {
		t.Fatalf("failure kind = %q, want %q", got, FailureManualIntervention)
	}
}

func TestWorkerValidationCommandsIncludeReproductionCommand(t *testing.T) {
	_, manifest := writeWorkerReproductionFixture(t)
	commands := workerValidationCommands([]string{"go test ./..."}, workerInput{Reproduction: manifest})
	if len(commands) != 2 || commands[1] != manifest.TestCommand {
		t.Fatalf("workerValidationCommands() = %#v, want configured plus reproduction command", commands)
	}
	commands = workerValidationCommands(commands, workerInput{Reproduction: manifest})
	if len(commands) != 2 {
		t.Fatalf("workerValidationCommands() duplicated command: %#v", commands)
	}
}
