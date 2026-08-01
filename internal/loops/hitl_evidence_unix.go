//go:build darwin || linux

package loops

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	hitlSentinelPath        = ".looper/ask.json"
	hitlPendingSentinelPath = ".looper/ask.pending"
)

var ErrUnsupportedHITLGateFile = errors.New("HITL gate path is not a regular file or symlink")

// StageHITLGateEvidence atomically moves ask.json to one fixed pending name on
// the same filesystem, then returns at most maxBytes without following any
// symlink component. The fixed name bounds retained evidence to one incident
// per worktree and remains discoverable after a crash before metadata persist.
func StageHITLGateEvidence(worktreeRoot string, maxBytes int64) ([]byte, *HITLGateEvidence, error) {
	if strings.TrimSpace(worktreeRoot) == "" {
		return nil, nil, nil
	}
	parentFD, err := openHITLDirectory(worktreeRoot)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	defer unix.Close(parentFD)

	var stat unix.Stat_t
	err = unix.Fstatat(parentFD, "ask.pending", &stat, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		if err = unix.Fstatat(parentFD, "ask.json", &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			if errors.Is(err, unix.ENOENT) {
				return nil, nil, nil
			}
			return nil, nil, fmt.Errorf("inspect HITL ask sentinel: %w", err)
		}
		if kind := hitlEvidenceKind(uint32(stat.Mode)); kind == "unsupported" {
			return nil, nil, fmt.Errorf("%w: mode %#o", ErrUnsupportedHITLGateFile, stat.Mode)
		}
		// Link first with no replacement, then remove the active name. A crash
		// between the two leaves two names for the same inode; the recovery path
		// below completes that staging instead of losing or overwriting evidence.
		if err := unix.Linkat(parentFD, "ask.json", parentFD, "ask.pending", 0); err != nil {
			return nil, nil, fmt.Errorf("stage HITL ask sentinel: %w", err)
		}
		if err := unix.Unlinkat(parentFD, "ask.json", 0); err != nil {
			return nil, nil, fmt.Errorf("remove active HITL ask name after staging: %w", err)
		}
		if err := unix.Fstatat(parentFD, "ask.pending", &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return nil, nil, fmt.Errorf("inspect staged HITL ask sentinel: %w", err)
		}
	} else if err != nil {
		return nil, nil, fmt.Errorf("inspect pending HITL ask sentinel: %w", err)
	} else {
		var active unix.Stat_t
		if activeErr := unix.Fstatat(parentFD, "ask.json", &active, unix.AT_SYMLINK_NOFOLLOW); activeErr == nil {
			if !sameHITLStatIdentity(stat, active) {
				return nil, nil, errors.New("active and pending HITL sentinels have different identities")
			}
			if err := unix.Unlinkat(parentFD, "ask.json", 0); err != nil {
				return nil, nil, fmt.Errorf("complete interrupted HITL sentinel staging: %w", err)
			}
		} else if !errors.Is(activeErr, unix.ENOENT) {
			return nil, nil, fmt.Errorf("inspect active HITL ask during staging recovery: %w", activeErr)
		}
	}

	evidence := evidenceFromStat(worktreeRoot, stat)
	if evidence.Kind == "symlink" {
		return nil, evidence, nil
	}
	if evidence.Kind != "regular" {
		return nil, nil, fmt.Errorf("%w: mode %#o", ErrUnsupportedHITLGateFile, stat.Mode)
	}
	file, err := openHITLPendingFile(parentFD)
	if err != nil {
		return nil, evidence, fmt.Errorf("open staged HITL ask sentinel: %w", err)
	}
	defer file.Close()
	if maxBytes < 0 {
		maxBytes = 0
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, evidence, fmt.Errorf("read staged HITL ask sentinel: %w", err)
	}
	if int64(len(raw)) > maxBytes {
		raw = raw[:maxBytes]
		evidence.Truncated = true
	}
	sum := sha256.Sum256(raw)
	evidence.PrefixSHA256 = hex.EncodeToString(sum[:])
	evidence.CapturedBytes = int64(len(raw))
	return raw, evidence, nil
}

// ConsumeHITLGateEvidence removes only the staged identity recorded in loop
// metadata. Missing evidence is idempotent only when no new ask.json exists;
// changed/replaced evidence fails closed and leaves the loop parked.
func ConsumeHITLGateEvidence(evidence *HITLGateEvidence) error {
	if evidence == nil {
		return nil
	}
	if evidence.PendingPath != hitlPendingSentinelPath {
		return fmt.Errorf("unsupported HITL pending path %q", evidence.PendingPath)
	}
	parentFD, err := openHITLDirectory(evidence.WorktreeRoot)
	if err != nil {
		return fmt.Errorf("open HITL evidence directory: %w", err)
	}
	defer unix.Close(parentFD)
	var stat unix.Stat_t
	if err := unix.Fstatat(parentFD, "ask.pending", &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			var active unix.Stat_t
			if activeErr := unix.Fstatat(parentFD, "ask.json", &active, unix.AT_SYMLINK_NOFOLLOW); errors.Is(activeErr, unix.ENOENT) {
				return nil
			} else if activeErr == nil {
				return errors.New("a new HITL ask sentinel exists; refusing to consume stale evidence")
			} else {
				return fmt.Errorf("inspect active HITL ask sentinel: %w", activeErr)
			}
		}
		return fmt.Errorf("inspect staged HITL evidence: %w", err)
	}
	var active unix.Stat_t
	if activeErr := unix.Fstatat(parentFD, "ask.json", &active, unix.AT_SYMLINK_NOFOLLOW); activeErr == nil {
		return errors.New("a new HITL ask sentinel exists; refusing to consume staged evidence")
	} else if !errors.Is(activeErr, unix.ENOENT) {
		return fmt.Errorf("inspect active HITL ask sentinel: %w", activeErr)
	}
	if !sameHITLEvidenceIdentity(evidence, stat) {
		return errors.New("staged HITL evidence identity changed; refusing to remove it")
	}
	if evidence.Kind == "regular" && evidence.PrefixSHA256 != "" {
		file, err := openHITLPendingFile(parentFD)
		if err != nil {
			return fmt.Errorf("open staged HITL evidence for verification: %w", err)
		}
		raw, readErr := io.ReadAll(io.LimitReader(file, evidence.CapturedBytes))
		closeErr := file.Close()
		if readErr != nil {
			return fmt.Errorf("verify staged HITL evidence: %w", readErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close staged HITL evidence: %w", closeErr)
		}
		sum := sha256.Sum256(raw)
		if hex.EncodeToString(sum[:]) != evidence.PrefixSHA256 {
			return errors.New("staged HITL evidence content changed; refusing to remove it")
		}
	}
	if err := unix.Unlinkat(parentFD, "ask.pending", 0); err != nil {
		return fmt.Errorf("consume staged HITL evidence: %w", err)
	}
	return nil
}

func openHITLDirectory(worktreeRoot string) (int, error) {
	rootFD, err := unix.Open(worktreeRoot, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, err
	}
	parentFD, err := unix.Openat(rootFD, ".looper", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	unix.Close(rootFD)
	return parentFD, err
}

func openHITLPendingFile(parentFD int) (*os.File, error) {
	fd, err := unix.Openat(parentFD, "ask.pending", unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), hitlPendingSentinelPath), nil
}

func evidenceFromStat(worktreeRoot string, stat unix.Stat_t) *HITLGateEvidence {
	return &HITLGateEvidence{
		WorktreeRoot: filepath.Clean(worktreeRoot), PendingPath: hitlPendingSentinelPath,
		Kind: hitlEvidenceKind(uint32(stat.Mode)), Device: uint64(stat.Dev), Inode: uint64(stat.Ino),
		Size: stat.Size, Mode: uint32(stat.Mode),
	}
}

func sameHITLEvidenceIdentity(evidence *HITLGateEvidence, stat unix.Stat_t) bool {
	return evidence.Kind == hitlEvidenceKind(uint32(stat.Mode)) && evidence.Device == uint64(stat.Dev) &&
		evidence.Inode == uint64(stat.Ino) && evidence.Size == stat.Size && evidence.Mode == uint32(stat.Mode)
}

func sameHITLStatIdentity(left, right unix.Stat_t) bool {
	return uint64(left.Dev) == uint64(right.Dev) && uint64(left.Ino) == uint64(right.Ino) &&
		left.Size == right.Size && uint32(left.Mode) == uint32(right.Mode)
}

func hitlEvidenceKind(mode uint32) string {
	switch mode & unix.S_IFMT {
	case unix.S_IFREG:
		return "regular"
	case unix.S_IFLNK:
		return "symlink"
	default:
		return "unsupported"
	}
}
