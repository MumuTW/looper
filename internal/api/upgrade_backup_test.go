package api

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFilesystemDatabasePathNormalizesFileURI(t *testing.T) {
	t.Parallel()
	got, err := filesystemDatabasePath("file:/var/lib/looper/looper.sqlite?cache=shared")
	if err != nil {
		t.Fatalf("filesystemDatabasePath() error = %v", err)
	}
	if got != "/var/lib/looper/looper.sqlite" {
		t.Fatalf("filesystemDatabasePath() = %q", got)
	}
	if _, err := filesystemDatabasePath(":memory:"); err == nil {
		t.Fatal("filesystemDatabasePath(:memory:) error = nil")
	}
	if _, err := filesystemDatabasePath("file:memdb1?mode=memory&cache=shared"); err == nil {
		t.Fatal("filesystemDatabasePath(memory URI) error = nil")
	}
}

func TestFilesystemDatabasePathAcceptsPlainPath(t *testing.T) {
	t.Parallel()
	got, err := filesystemDatabasePath("/tmp/looper.sqlite")
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if got != "/tmp/looper.sqlite" {
		t.Fatalf("got %q", got)
	}
}

func TestPinExecutableContentsFreezesSource(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "current", "looper")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("verified-image"), 0o700); err != nil {
		t.Fatal(err)
	}
	pinned, cleanup, err := pinExecutableContents(path, "CLI binary")
	if err != nil {
		t.Fatalf("pinExecutableContents() error = %v", err)
	}
	defer cleanup()
	if err := os.WriteFile(path, []byte("candidate-after-pin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if string(pinned.contents) != "verified-image" {
		t.Fatalf("pinned contents = %q, want verified image", pinned.contents)
	}
	got, err := os.ReadFile(pinned.path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "verified-image" {
		t.Fatalf("pinned file = %q, want verified image", got)
	}
}
