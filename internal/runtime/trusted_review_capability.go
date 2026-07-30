package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nexu-io/looper/internal/bootstrap"
	"github.com/nexu-io/looper/internal/forge"
)

// trustedReviewCapabilityProbeTimeout bounds the probe because the reviewer
// tick path blocks on it. The verb loads no config and touches no network, so
// anything slower is a broken binary or a machine under enough load that the
// answer would not be about the binary either way — which is why blowing this
// deadline counts as transient rather than as a verdict.
//
// It is a var only so tests can shorten it; nothing reassigns it at runtime.
var trustedReviewCapabilityProbeTimeout = 5 * time.Second

// trustedReviewCapabilityRetryDelay prevents one persistently hung configured
// binary from consuming a full probe timeout on every scheduler tick. A binary
// identity change bypasses this delay, so replacement with a working binary
// recovers immediately.
//
// It is a var only so tests can shorten it; nothing reassigns it at runtime.
var trustedReviewCapabilityRetryDelay = 30 * time.Second

// trustedReviewCapabilityDiagnosticLimit caps how much probe output is kept for
// the operator-facing reason; probe output is agent-adjacent and unbounded.
const trustedReviewCapabilityDiagnosticLimit = 200

// trustedReviewCapabilityIdentity identifies the probed binary. Modtime and
// size are part of the identity so an upgraded looper is re-probed rather than
// inheriting the previous verdict, while steady-state reviewer ticks only pay
// for a stat.
type trustedReviewCapabilityIdentity struct {
	resolvedPath    string
	modTimeUnixNano int64
	size            int64
	resolveError    string
}

type trustedReviewCapabilityEntry struct {
	identity trustedReviewCapabilityIdentity
	capable  bool
	reason   string
	// retryAfter is nonzero only for a transient probe failure. It is
	// deliberately in-memory: this cooldown controls scheduler load, not the
	// durable authority for whether the binary supports review submission.
	retryAfter time.Time
}

// trustedReviewCapabilityCache is keyed by configured path so an upgrade
// replaces the entry instead of accumulating one per binary revision. The
// scheduler resolves the trusted path from concurrent tick and claim paths.
var trustedReviewCapabilityCache = struct {
	mu      sync.Mutex
	entries map[string]trustedReviewCapabilityEntry
}{entries: make(map[string]trustedReviewCapabilityEntry)}

// trustedReviewCapability reports whether configuredPath can serve as the
// trusted `looper review submit` wrapper. changed is true only when the verdict
// was first computed or flipped, so callers can log the verdict without
// spamming once per tick.
func trustedReviewCapability(configuredPath string) (capable bool, reason string, changed bool) {
	configuredPath = strings.TrimSpace(configuredPath)
	if configuredPath == "" {
		return false, "trusted looper path is not configured", false
	}

	identity := trustedReviewCapabilityIdentityFor(configuredPath)

	trustedReviewCapabilityCache.mu.Lock()
	// The probe runs under the lock so concurrent reviewer ticks collapse onto
	// a single subprocess instead of racing to spawn their own.
	defer trustedReviewCapabilityCache.mu.Unlock()

	previous, hadPrevious := trustedReviewCapabilityCache.entries[configuredPath]
	if hadPrevious && previous.identity == identity {
		if previous.retryAfter.IsZero() || time.Now().Before(previous.retryAfter) {
			return previous.capable, previous.reason, false
		}
	}

	capable, reason, transient := probeTrustedReviewCapability(identity)
	// A transient failure is not a verdict about the binary, and caching one
	// as a durable verdict would be permanent: the cache key is the binary's
	// own identity, which does not change because the machine was briefly out
	// of process slots. One fork/exec EAGAIN, or one probe that lost its 5s
	// race under load, would otherwise leave reviewer publishing fail-closed
	// for the rest of the daemon's life.
	// A short in-memory cooldown avoids spending the full probe timeout on every
	// scheduler tick while still re-probing the unchanged binary automatically.
	// Retrying in place would hold this mutex, which every concurrent reviewer
	// tick is queued behind, across the backoff.
	entry := trustedReviewCapabilityEntry{identity: identity, capable: capable, reason: reason}
	if transient {
		entry.retryAfter = time.Now().Add(trustedReviewCapabilityRetryDelay)
	}
	trustedReviewCapabilityCache.entries[configuredPath] = entry
	return capable, reason, !hadPrevious || previous.capable != capable
}

// trustedReviewCapabilityCached returns only a verdict that a scheduler-path
// probe has already recorded. In particular, it does not resolve, stat, or
// execute configuredPath: status requests must remain read-only and must not
// make a configured binary part of the request's latency or failure surface.
func trustedReviewCapabilityCached(configuredPath string) (capable bool, reason string, known bool) {
	configuredPath = strings.TrimSpace(configuredPath)
	if configuredPath == "" {
		return false, "trusted looper path is not configured", true
	}

	trustedReviewCapabilityCache.mu.Lock()
	defer trustedReviewCapabilityCache.mu.Unlock()
	entry, ok := trustedReviewCapabilityCache.entries[configuredPath]
	if !ok || !entry.retryAfter.IsZero() {
		return false, "", false
	}
	return entry.capable, entry.reason, true
}

func trustedReviewCapabilityIdentityFor(configuredPath string) trustedReviewCapabilityIdentity {
	// LookPath first so a bare command name resolves the same way the trusted
	// review proxy resolves it at mint time.
	resolved, err := exec.LookPath(configuredPath)
	if err != nil {
		return trustedReviewCapabilityIdentity{resolveError: err.Error()}
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return trustedReviewCapabilityIdentity{resolveError: err.Error()}
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return trustedReviewCapabilityIdentity{resolvedPath: resolved, resolveError: err.Error()}
	}
	return trustedReviewCapabilityIdentity{resolvedPath: resolved, modTimeUnixNano: info.ModTime().UnixNano(), size: info.Size()}
}

// probeTrustedReviewCapability runs the verb once. transient reports that the
// probe never got a verdict out of the binary — see isTransientProbeFailure.
func probeTrustedReviewCapability(identity trustedReviewCapabilityIdentity) (capable bool, reason string, transient bool) {
	if identity.resolveError != "" {
		// A resolve error is safe to cache: the identity it produces changes as
		// soon as the path resolves, so the entry cannot outlive the condition.
		return false, fmt.Sprintf("resolve looper binary: %s", identity.resolveError), false
	}

	ctx, cancel := context.WithTimeout(context.Background(), trustedReviewCapabilityProbeTimeout)
	defer cancel()

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, identity.resolvedPath, "review", "capability")
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		detail := trustedReviewCapabilityDiagnostic(stderr.String())
		if detail == "" {
			detail = trustedReviewCapabilityDiagnostic(stdout.String())
		}
		transient := isTransientProbeFailure(ctx, err)
		if detail == "" {
			return false, fmt.Sprintf("`looper review capability` failed: %v", err), transient
		}
		return false, fmt.Sprintf("`looper review capability` failed: %v: %s", err, detail), transient
	}

	token := strings.TrimSpace(stdout.String())
	if token != forge.TrustedReviewCapabilityToken {
		return false, fmt.Sprintf("`looper review capability` reported %q, want %q", trustedReviewCapabilityDiagnostic(token), forge.TrustedReviewCapabilityToken), false
	}
	return true, "", false
}

// isTransientProbeFailure reports the failures that say nothing about the
// binary, so the verdict must not be cached against its identity.
//
// The list is deliberately short. A binary that is not executable, is the wrong
// architecture, or exits nonzero has answered the question; re-probing it every
// reviewer tick would spend a subprocess to be told the same thing. What is
// left is the machine being briefly unable to start any process, the binary
// being mid-write — the case a `go build` into the configured path produces —
// and the probe losing its own deadline under load, which CommandContext
// reports as a killed process rather than as an error wrapping ctx.Err().
func isTransientProbeFailure(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return true
	}
	return errors.Is(err, syscall.EAGAIN) ||
		errors.Is(err, syscall.ENOMEM) ||
		errors.Is(err, syscall.ETXTBSY)
}

func trustedReviewCapabilityDiagnostic(output string) string {
	output = strings.TrimSpace(output)
	if output == "" {
		return ""
	}
	if index := strings.IndexByte(output, '\n'); index >= 0 {
		output = strings.TrimSpace(output[:index])
	}
	if len(output) > trustedReviewCapabilityDiagnosticLimit {
		output = output[:trustedReviewCapabilityDiagnosticLimit] + "…"
	}
	return output
}

func logTrustedReviewCapabilityVerdict(logger bootstrap.Logger, configuredPath string, capable bool, reason string) {
	if logger == nil {
		return
	}
	if capable {
		logger.Info("trusted looper review-submit capability verified", map[string]any{"path": configuredPath})
		return
	}
	logger.Warn("reviewer publishing disabled: configured looper binary cannot serve `looper review submit`", map[string]any{"path": configuredPath, "reason": reason})
}
