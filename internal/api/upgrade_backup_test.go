package api

import "testing"

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
