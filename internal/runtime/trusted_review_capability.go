package runtime

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nexu-io/looper/internal/bootstrap"
	"github.com/nexu-io/looper/internal/forge"
)

// trustedReviewCapabilityProbeTimeout bounds the probe because the reviewer
// tick path blocks on it. The verb loads no config and touches no network, so
// anything slower is already a broken binary.
const trustedReviewCapabilityProbeTimeout = 5 * time.Second

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
		return previous.capable, previous.reason, false
	}

	capable, reason = probeTrustedReviewCapability(identity)
	trustedReviewCapabilityCache.entries[configuredPath] = trustedReviewCapabilityEntry{identity: identity, capable: capable, reason: reason}
	return capable, reason, !hadPrevious || previous.capable != capable
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

func probeTrustedReviewCapability(identity trustedReviewCapabilityIdentity) (bool, string) {
	if identity.resolveError != "" {
		return false, fmt.Sprintf("resolve looper binary: %s", identity.resolveError)
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
		if detail == "" {
			return false, fmt.Sprintf("`looper review capability` failed: %v", err)
		}
		return false, fmt.Sprintf("`looper review capability` failed: %v: %s", err, detail)
	}

	token := strings.TrimSpace(stdout.String())
	if token != forge.TrustedReviewCapabilityToken {
		return false, fmt.Sprintf("`looper review capability` reported %q, want %q", trustedReviewCapabilityDiagnostic(token), forge.TrustedReviewCapabilityToken)
	}
	return true, ""
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
		logger.Info("trusted looper review-submit wrapper verified", map[string]any{"path": configuredPath})
		return
	}
	logger.Warn("reviewer publishing disabled: configured looper binary cannot serve `looper review submit`", map[string]any{"path": configuredPath, "reason": reason})
}
