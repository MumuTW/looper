package fixer

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MumuTW/looper/internal/reproducer"
)

func writeFixerReproductionFixture(t *testing.T) (string, *reproducer.Manifest) {
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

func TestFixerReproductionCapturePersistsAndDetectsTampering(t *testing.T) {
	root, expected := writeFixerReproductionFixture(t)
	checkpoint := fixerCheckpoint{}
	if err := captureFixerReproduction(&checkpoint, root); err != nil {
		t.Fatalf("captureFixerReproduction() error = %v", err)
	}
	if checkpoint.Reproduction == nil || !checkpoint.Reproduction.Equal(*expected) {
		t.Fatalf("captured reproduction = %#v, want %#v", checkpoint.Reproduction, expected)
	}
	if err := verifyFixerReproduction(checkpoint, root); err != nil {
		t.Fatalf("verifyFixerReproduction() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, expected.TestPath), []byte("func TestBug(t *testing.T) { t.Skip() }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := verifyFixerReproduction(checkpoint, root)
	if err == nil || !strings.Contains(err.Error(), "integrity check failed") || !strings.Contains(err.Error(), "modified") {
		t.Fatalf("verifyFixerReproduction() after tamper = %v, want integrity failure", err)
	}
	if got := err.(*loopError).kind; got != FailureManualIntervention {
		t.Fatalf("failure kind = %q, want %q", got, FailureManualIntervention)
	}
}

func TestFixerValidationCommandsIncludeReproductionCommand(t *testing.T) {
	_, manifest := writeFixerReproductionFixture(t)
	commands := fixerValidationCommands([]string{"go test ./..."}, fixerCheckpoint{Reproduction: manifest})
	if len(commands) != 2 || commands[1] != manifest.TestCommand {
		t.Fatalf("fixerValidationCommands() = %#v, want configured plus reproduction command", commands)
	}
	commands = fixerValidationCommands(commands, fixerCheckpoint{Reproduction: manifest})
	if len(commands) != 2 {
		t.Fatalf("fixerValidationCommands() duplicated command: %#v", commands)
	}
}

func TestFixerReproductionCaptureVerifiesOnFirstAdopt(t *testing.T) {
	root, expected := writeFixerReproductionFixture(t)
	// Stale hash in the committed manifest: first capture must refuse before the
	// daemon can execute testCommand.
	bad := []byte(`{"version":1,"testPath":"internal/bug_test.go","testName":"TestBug","testCommand":"go test ./internal -run '^TestBug$'","testSha256":"` + strings.Repeat("0", 64) + `"}`)
	if err := os.WriteFile(filepath.Join(root, reproducer.ManifestPath), bad, 0o644); err != nil {
		t.Fatal(err)
	}
	checkpoint := fixerCheckpoint{}
	err := captureFixerReproduction(&checkpoint, root)
	if err == nil || !strings.Contains(err.Error(), "integrity check failed") {
		t.Fatalf("captureFixerReproduction() = %v, want hash verify failure", err)
	}
	if checkpoint.Reproduction != nil {
		t.Fatalf("Reproduction = %#v, want nil after failed first capture", checkpoint.Reproduction)
	}
	// Restore a valid manifest and confirm Verify-on-capture succeeds.
	good := []byte(`{"version":1,"testPath":"internal/bug_test.go","testName":"TestBug","testCommand":"go test ./internal -run '^TestBug$'","testSha256":"` + expected.TestSHA256 + `"}`)
	if err := os.WriteFile(filepath.Join(root, reproducer.ManifestPath), good, 0o644); err != nil {
		t.Fatal(err)
	}
	checkpoint = fixerCheckpoint{}
	if err := captureFixerReproduction(&checkpoint, root); err != nil {
		t.Fatalf("captureFixerReproduction() error = %v", err)
	}
	if checkpoint.Reproduction == nil || !checkpoint.Reproduction.Equal(*expected) {
		t.Fatalf("captured = %#v, want %#v", checkpoint.Reproduction, expected)
	}
}

func TestFixerReproductionRefusesAgentAuthoredManifestAfterAbsentStart(t *testing.T) {
	root := t.TempDir()
	checkpoint := fixerCheckpoint{}
	if err := captureFixerReproduction(&checkpoint, root); err != nil {
		t.Fatalf("capture with no manifest error = %v", err)
	}
	if !checkpoint.ReproductionAbsent || checkpoint.Reproduction != nil {
		t.Fatalf("checkpoint = %#v, want ReproductionAbsent and nil Reproduction", checkpoint)
	}
	// Agent later writes a manifest; Fixer must not adopt it as the contract.
	if err := os.MkdirAll(filepath.Join(root, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	testContent := []byte("func TestBug(t *testing.T) {}\n")
	if err := os.WriteFile(filepath.Join(root, "internal", "bug_test.go"), testContent, 0o644); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(testContent)
	if err := os.MkdirAll(filepath.Join(root, ".looper"), 0o755); err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"version":1,"testPath":"internal/bug_test.go","testName":"TestBug","testCommand":"go test ./internal -run '^TestBug$'","testSha256":"` + hex.EncodeToString(hash[:]) + `"}`)
	if err := os.WriteFile(filepath.Join(root, reproducer.ManifestPath), data, 0o644); err != nil {
		t.Fatal(err)
	}
	err := captureFixerReproduction(&checkpoint, root)
	if err == nil || !strings.Contains(err.Error(), "appeared after run start") {
		t.Fatalf("capture after agent-authored manifest = %v, want refusal", err)
	}
}
