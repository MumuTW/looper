//go:build darwin || linux

package loops

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestStageHITLGateEvidenceNeverFollowsSymlink(t *testing.T) {
	t.Parallel()
	worktree := t.TempDir()
	outside := filepath.Join(t.TempDir(), "daemon-secret")
	secret := []byte("daemon-readable secret must never be copied")
	if err := os.WriteFile(outside, secret, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(worktree, ".looper"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(worktree, hitlSentinelPath)); err != nil {
		t.Fatal(err)
	}

	raw, evidence, err := StageHITLGateEvidence(worktree, 1024)
	if err != nil || evidence == nil || evidence.Kind != "symlink" || len(raw) != 0 {
		t.Fatalf("StageHITLGateEvidence() = (%q, %#v, %v), want symlink metadata only", raw, evidence, err)
	}
	if _, err := os.Lstat(filepath.Join(worktree, hitlSentinelPath)); !os.IsNotExist(err) {
		t.Fatalf("active symlink still exists: %v", err)
	}
	if info, err := os.Lstat(filepath.Join(worktree, hitlPendingSentinelPath)); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("pending evidence = (%v, %v), want the link itself", info, err)
	}
	if got, err := os.ReadFile(outside); err != nil || !bytes.Equal(got, secret) {
		t.Fatalf("outside target = (%q, %v), want untouched", got, err)
	}
}

func TestStageHITLGateEvidenceIsBoundedAndCrashDiscoverable(t *testing.T) {
	t.Parallel()
	worktree := t.TempDir()
	if err := os.Mkdir(filepath.Join(worktree, ".looper"), 0o755); err != nil {
		t.Fatal(err)
	}
	content := bytes.Repeat([]byte("x"), 2<<20)
	if err := os.WriteFile(filepath.Join(worktree, hitlSentinelPath), content, 0o600); err != nil {
		t.Fatal(err)
	}

	raw, first, err := StageHITLGateEvidence(worktree, 4096)
	if err != nil || first == nil || len(raw) != 4096 || !first.Truncated || first.Size != int64(len(content)) {
		t.Fatalf("first stage = (%d, %#v, %v), want bounded prefix", len(raw), first, err)
	}
	raw, second, err := StageHITLGateEvidence(worktree, 4096)
	if err != nil || second == nil || second.Inode != first.Inode || !bytes.Equal(raw, content[:4096]) {
		t.Fatalf("restart stage = (%d, %#v, %v), want same pending identity", len(raw), second, err)
	}
	matches, err := filepath.Glob(filepath.Join(worktree, ".looper", "ask.*"))
	if err != nil || len(matches) != 1 || filepath.Base(matches[0]) != "ask.pending" {
		t.Fatalf("retained evidence = (%v, %v), want one fixed pending file", matches, err)
	}
}

func TestStageHITLGateEvidenceCompletesInterruptedLinkThenUnlink(t *testing.T) {
	t.Parallel()
	worktree := t.TempDir()
	dir := filepath.Join(worktree, ".looper")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	active, pending := filepath.Join(dir, "ask.json"), filepath.Join(dir, "ask.pending")
	if err := os.WriteFile(active, []byte("invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	// This is the only crash window in staging: pending already names the
	// same inode, while the active name has not yet been removed.
	if err := os.Link(active, pending); err != nil {
		t.Fatal(err)
	}
	_, evidence, err := StageHITLGateEvidence(worktree, 1024)
	if err != nil || evidence == nil {
		t.Fatalf("StageHITLGateEvidence() = (%#v, %v)", evidence, err)
	}
	if _, err := os.Lstat(active); !os.IsNotExist(err) {
		t.Fatalf("interrupted active name still exists: %v", err)
	}
	if _, err := os.Lstat(pending); err != nil {
		t.Fatalf("pending identity missing: %v", err)
	}
}

func TestStageHITLGateEvidenceRejectsDirectoryAndSymlinkAncestor(t *testing.T) {
	t.Parallel()
	t.Run("directory", func(t *testing.T) {
		worktree := t.TempDir()
		if err := os.MkdirAll(filepath.Join(worktree, hitlSentinelPath), 0o755); err != nil {
			t.Fatal(err)
		}
		_, evidence, err := StageHITLGateEvidence(worktree, 1024)
		if !errors.Is(err, ErrUnsupportedHITLGateFile) || evidence != nil {
			t.Fatalf("stage directory = (%#v, %v), want unsupported without evidence", evidence, err)
		}
	})
	t.Run("symlink ancestor", func(t *testing.T) {
		worktree := t.TempDir()
		outside := t.TempDir()
		if err := os.WriteFile(filepath.Join(outside, "ask.json"), []byte("secret"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(worktree, ".looper")); err != nil {
			t.Fatal(err)
		}
		_, evidence, err := StageHITLGateEvidence(worktree, 1024)
		if err == nil || evidence != nil {
			t.Fatalf("stage symlink ancestor = (%#v, %v), want ambiguous lookup error", evidence, err)
		}
		if got, err := os.ReadFile(filepath.Join(outside, "ask.json")); err != nil || string(got) != "secret" {
			t.Fatalf("outside ask = (%q, %v), want untouched", got, err)
		}
	})
}

func TestConsumeHITLGateEvidenceRequiresSameIdentity(t *testing.T) {
	t.Parallel()
	worktree := t.TempDir()
	if err := os.Mkdir(filepath.Join(worktree, ".looper"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, hitlSentinelPath), []byte("invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, evidence, err := StageHITLGateEvidence(worktree, 1024)
	if err != nil {
		t.Fatal(err)
	}
	pending := filepath.Join(worktree, hitlPendingSentinelPath)
	if err := os.Remove(pending); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pending, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ConsumeHITLGateEvidence(evidence); err == nil {
		t.Fatal("ConsumeHITLGateEvidence() error = nil, want changed identity rejection")
	}
	if got, err := os.ReadFile(pending); err != nil || string(got) != "replacement" {
		t.Fatalf("replacement = (%q, %v), want preserved", got, err)
	}
}
