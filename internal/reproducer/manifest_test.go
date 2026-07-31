package reproducer

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseNormalizesAndRejectsUnknownFields(t *testing.T) {
	testHash := strings.Repeat("a", 64)
	manifest, err := Parse([]byte(`{"version":1,"testPath":"internal/foo_test.go","testName":"TestBug","testCommand":"go test ./internal -run '^TestBug$'","testSha256":"` + strings.ToUpper(testHash) + `"}`))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if manifest.TestSHA256 != testHash || manifest.TestName != "TestBug" {
		t.Fatalf("Parse() = %#v, want normalized values", manifest)
	}
	if _, err := Parse([]byte(`{"version":1,"testPath":"internal/foo_test.go","testName":"TestBug","testCommand":"go test","testSha256":"` + testHash + `","extra":true}`)); err == nil {
		t.Fatal("Parse() accepted unknown field")
	}
}

func TestLoadOptionalAndVerifyDetectsManifestAndTestTampering(t *testing.T) {
	root := t.TempDir()
	testPath := filepath.Join(root, "internal", "foo_test.go")
	if err := os.MkdirAll(filepath.Dir(testPath), 0o755); err != nil {
		t.Fatal(err)
	}
	testContent := []byte("func TestBug() {}\n")
	if err := os.WriteFile(testPath, testContent, 0o644); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(testContent)
	manifestJSON := `{"version":1,"testPath":"internal/foo_test.go","testName":"TestBug","testCommand":"go test ./internal -run '^TestBug$'","testSha256":"` + hex.EncodeToString(hash[:]) + `"}`
	if err := os.Mkdir(filepath.Join(root, ".looper"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ManifestPath), []byte(manifestJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest, err := Load(root)
	if err != nil || manifest == nil {
		t.Fatalf("Load() = %#v, %v", manifest, err)
	}
	if err := manifest.Verify(root); err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if err := os.WriteFile(testPath, []byte("func TestBug() { t.Skip() }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := manifest.Verify(root); err == nil || !strings.Contains(err.Error(), "modified") {
		t.Fatalf("Verify() after test tamper = %v, want modified error", err)
	}
	if err := os.WriteFile(testPath, testContent, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ManifestPath), []byte(strings.Replace(manifestJSON, "TestBug", "TestOther", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := manifest.Verify(root); err == nil || !strings.Contains(err.Error(), "manifest changed") {
		t.Fatalf("Verify() after manifest tamper = %v, want manifest changed error", err)
	}
}

func TestLoadRejectsSymlinkedManifestAndTestPathTraversal(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".looper"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(outside, []byte(`{"version":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, ManifestPath)); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	if _, err := Load(root); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("Load() error = %v, want symlink rejection", err)
	}
	if _, err := Parse([]byte(`{"version":1,"testPath":"../secret.go","testName":"TestBug","testCommand":"go test","testSha256":"` + strings.Repeat("a", 64) + `"}`)); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("Parse() error = %v, want traversal rejection", err)
	}
}

func TestLoadMissingManifestIsBackwardCompatible(t *testing.T) {
	manifest, err := Load(t.TempDir())
	if err != nil || manifest != nil {
		t.Fatalf("Load() = %#v, %v, want nil manifest without error", manifest, err)
	}
}
