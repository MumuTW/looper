package daemonbinary

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeExecutable(t *testing.T, path string, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// The whole point of the package: a file that was replaced under a running
// process must report as swapped, with both digests so an operator can tell
// which build is which.
func TestVerifyReportsReplacedExecutable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "looperd")
	writeExecutable(t, path, "running image")

	recorded, err := Capture(path)
	if err != nil {
		t.Fatalf("Capture() error = %v", err)
	}

	writeExecutable(t, path, "agent-built replacement")
	// Same-second writes can land on the recorded mtime; force the cheap
	// fields to move so the test exercises the digest comparison either way.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatalf("Chtimes() error = %v", err)
	}

	status := Verify(recorded)
	if !status.Swapped {
		t.Fatalf("Verify() = %#v, want Swapped for a replaced executable", status)
	}
	if status.RunningSHA256 == status.OnDiskSHA256 {
		t.Fatalf("Verify() digests both %q, want the running image and the on-disk file to differ", status.RunningSHA256)
	}
	if !strings.Contains(status.Reason, path) {
		t.Fatalf("Reason = %q, want it to name %q", status.Reason, path)
	}
}

func TestVerifyAcceptsUntouchedExecutable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "looperd")
	writeExecutable(t, path, "running image")

	recorded, err := Capture(path)
	if err != nil {
		t.Fatalf("Capture() error = %v", err)
	}

	status := Verify(recorded)
	if status.Swapped || !status.Known {
		t.Fatalf("Verify() = %#v, want a known, unswapped status", status)
	}
	if status.OnDiskSHA256 != recorded.SHA256 {
		t.Fatalf("OnDiskSHA256 = %q, want the recorded digest %q", status.OnDiskSHA256, recorded.SHA256)
	}
}

// Rewriting identical bytes (a rebuild that produced the same binary, or a
// bare touch) moves mtime but changes nothing that matters. Reporting that as
// a swap would train operators to ignore the signal.
func TestVerifyIgnoresTimestampOnlyChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "looperd")
	writeExecutable(t, path, "running image")

	recorded, err := Capture(path)
	if err != nil {
		t.Fatalf("Capture() error = %v", err)
	}

	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatalf("Chtimes() error = %v", err)
	}

	if status := Verify(recorded); status.Swapped {
		t.Fatalf("Verify() = %#v, want no swap when only the timestamp moved", status)
	}
}

// A same-size replacement is what a rebuild of the same source tree tends to
// produce, so size alone must not be the test.
func TestVerifyDetectsSameSizeReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "looperd")
	writeExecutable(t, path, "aaaa")

	recorded, err := Capture(path)
	if err != nil {
		t.Fatalf("Capture() error = %v", err)
	}

	writeExecutable(t, path, "bbbb")
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatalf("Chtimes() error = %v", err)
	}

	if status := Verify(recorded); !status.Swapped {
		t.Fatalf("Verify() = %#v, want Swapped for a same-size replacement", status)
	}
}

func TestVerifyReportsRemovedExecutable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "looperd")
	writeExecutable(t, path, "running image")

	recorded, err := Capture(path)
	if err != nil {
		t.Fatalf("Capture() error = %v", err)
	}

	if err := os.Remove(path); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}

	status := Verify(recorded)
	if !status.Swapped {
		t.Fatalf("Verify() = %#v, want Swapped when the executable is gone", status)
	}
	if status.OnDiskSHA256 != "" {
		t.Fatalf("OnDiskSHA256 = %q, want empty when the file cannot be read", status.OnDiskSHA256)
	}
}

// An unrecorded identity means the daemon cannot see a swap. It must say so
// rather than report the reassuring answer.
func TestVerifyWithoutRecordedIdentityIsUnknownNotClean(t *testing.T) {
	status := Verify(Identity{})
	if status.Known {
		t.Fatalf("Verify(Identity{}) = %#v, want Known false", status)
	}
	if status.Reason == "" {
		t.Fatal("Reason is empty, want an explanation that detection is unavailable")
	}
}

func TestSelfRecordsTheRunningTestBinary(t *testing.T) {
	identity, err := Self()
	if err != nil {
		t.Fatalf("Self() error = %v", err)
	}
	if !identity.Known() {
		t.Fatalf("Self() = %#v, want a known identity", identity)
	}

	// Memoized: the executable a process loaded cannot change mid-life, and a
	// second read could pick up a replacement instead of the running image.
	again, err := Self()
	if err != nil {
		t.Fatalf("Self() second call error = %v", err)
	}
	if again != identity {
		t.Fatalf("Self() = %#v on the second call, want the memoized %#v", again, identity)
	}

	if status := Verify(identity); status.Swapped {
		t.Fatalf("Verify(Self()) = %#v, want no swap for an untouched test binary", status)
	}
}
