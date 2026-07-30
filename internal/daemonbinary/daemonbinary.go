// Package daemonbinary records the identity of the executable file a process
// started from and reports when that file stops holding the image the process
// is running.
//
// looperd runs agents against looper itself. An agent that rebuilds and
// installs looperd overwrites the exact file the live daemon was launched
// from. Nothing fails at that moment — the process keeps executing the image
// it already loaded — so the swap is invisible until something restarts the
// daemon and drops every in-flight run with it. This package makes the
// divergence observable while it is still only a file difference, so an
// operator does not have to infer it afterwards from backup filenames.
//
// It detects; it cannot prevent. Preventing the write requires owning the
// writer, which looper does not for an unsandboxed agent shell.
package daemonbinary

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// SwappedDegradedReason is the /status degraded reason emitted when the
// daemon's on-disk executable no longer matches the running image.
const SwappedDegradedReason = "daemon_binary_swapped"

// Identity is the recorded fingerprint of an executable file. Size and
// ModTimeUnixNano exist so a repeat check costs one stat: only when the cheap
// fields move does the file get hashed again.
type Identity struct {
	Path            string
	Size            int64
	ModTimeUnixNano int64
	SHA256          string
}

// Known reports whether the identity was recorded successfully. An unrecorded
// identity proves nothing either way, so it is never reported as a swap.
func (i Identity) Known() bool {
	return i.Path != "" && i.SHA256 != ""
}

// Status is the operator-facing comparison between the running image and the
// file on disk.
type Status struct {
	// Known is false when the identity could not be recorded at startup. The
	// daemon then cannot tell whether its binary changed, which is itself
	// reported rather than silently treated as "unchanged".
	Known bool   `json:"known"`
	Path  string `json:"path,omitempty"`
	// RunningSHA256 is the digest of the file as it was when this process
	// started, i.e. the image it is executing.
	RunningSHA256 string `json:"runningSha256,omitempty"`
	// OnDiskSHA256 is the digest of that path now. Empty when it could not be
	// read.
	OnDiskSHA256 string `json:"onDiskSha256,omitempty"`
	// Swapped is true when the path no longer holds the running image, or can
	// no longer be shown to hold it. Reason distinguishes the cases.
	Swapped bool   `json:"swapped"`
	Reason  string `json:"reason,omitempty"`
}

// Self records the identity of the executable of the current process.
//
// The result is memoized: which file this process loaded is fixed for its
// lifetime, and hashing it again would only re-read a file that may already
// have been replaced. The first call must therefore happen at startup.
func Self() (Identity, error) {
	selfOnce.Do(func() {
		executablePath, err := os.Executable()
		if err != nil {
			selfErr = fmt.Errorf("resolve own executable: %w", err)
			return
		}

		selfIdentity, selfErr = Capture(executablePath)
	})

	return selfIdentity, selfErr
}

var (
	selfOnce     sync.Once
	selfIdentity Identity
	selfErr      error
)

// Capture records the identity of the file at path, resolving symlinks so a
// later comparison is against the same file the loader used.
func Capture(path string) (Identity, error) {
	resolvedPath := resolvePath(path)
	if resolvedPath == "" {
		return Identity{}, fmt.Errorf("executable path is empty")
	}

	info, err := os.Stat(resolvedPath)
	if err != nil {
		return Identity{}, fmt.Errorf("stat executable %s: %w", resolvedPath, err)
	}

	digest, err := fileSHA256(resolvedPath)
	if err != nil {
		return Identity{}, err
	}

	return Identity{
		Path:            resolvedPath,
		Size:            info.Size(),
		ModTimeUnixNano: info.ModTime().UnixNano(),
		SHA256:          digest,
	}, nil
}

// Verify compares recorded against the file on disk now.
//
// The cheap stat path is authoritative for "unchanged": if size and mtime both
// still match, the content does. A moved stat re-hashes rather than concluding
// a swap, so `touch` alone does not raise a false alarm.
func Verify(recorded Identity) Status {
	if !recorded.Known() {
		return Status{
			Reason: "daemon executable identity was not recorded at startup; a binary swap cannot be detected",
		}
	}

	status := Status{
		Known:         true,
		Path:          recorded.Path,
		RunningSHA256: recorded.SHA256,
	}

	info, err := os.Stat(recorded.Path)
	if err != nil {
		status.Swapped = true
		status.Reason = fmt.Sprintf("daemon executable %s can no longer be read: %v", recorded.Path, err)
		return status
	}

	if info.Size() == recorded.Size && info.ModTime().UnixNano() == recorded.ModTimeUnixNano {
		status.OnDiskSHA256 = recorded.SHA256
		return status
	}

	digest, err := fileSHA256(recorded.Path)
	if err != nil {
		status.Swapped = true
		status.Reason = fmt.Sprintf("daemon executable %s can no longer be read: %v", recorded.Path, err)
		return status
	}

	status.OnDiskSHA256 = digest
	if digest == recorded.SHA256 {
		return status
	}

	status.Swapped = true
	status.Reason = fmt.Sprintf(
		"daemon executable %s was replaced while running (running image %s, on disk %s); this process is still executing the old image and the next restart will silently switch builds",
		recorded.Path, shortDigest(recorded.SHA256), shortDigest(digest),
	)
	return status
}

func resolvePath(path string) string {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return ""
	}

	resolved, err := filepath.EvalSymlinks(trimmed)
	if err != nil {
		return trimmed
	}

	resolved = strings.TrimSpace(resolved)
	if resolved == "" {
		return trimmed
	}

	return resolved
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open executable %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()

	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", fmt.Errorf("read executable %s: %w", path, err)
	}

	return hex.EncodeToString(digest.Sum(nil)), nil
}

func shortDigest(digest string) string {
	if len(digest) <= 12 {
		return digest
	}

	return digest[:12]
}
