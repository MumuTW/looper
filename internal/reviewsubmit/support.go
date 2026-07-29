package reviewsubmit

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/forge"
)

// loadConfig resolves the configuration this submission is judged against.
//
// A trusted proxy child gets its configuration from a one-shot descriptor the
// daemon materialized when it captured the run, not from the file the agent can
// see: the daemon already resolved file, environment, and CLI precedence, and
// re-resolving here would let the agent's environment rewrite the review-event
// policy the daemon bound. The descriptor is consumed exactly once, so this
// must stay the only config load on the trusted path.
func loadConfig(opts Options) (config.LoadedFileConfig, error) {
	if loaded, configured, err := forge.LoadTrustedReviewConfigSnapshot(); configured {
		if err != nil {
			return config.LoadedFileConfig{}, err
		}
		return loaded, nil
	}
	return config.LoadFile(config.LoadFileOptions{
		CWD:  opts.CWD,
		Args: append([]string(nil), opts.ConfigArgs...),
	})
}

// parsePullRequestRef splits the "<repo>#<number>" target. The proxy validates
// the same shape before spawning this process and binds it to the run's PR, so
// a mismatch here means the two parsers disagree, not that the operator typed
// something unusual.
func parsePullRequestRef(value string) (string, int64, error) {
	parts := strings.Split(strings.TrimSpace(value), "#")
	if len(parts) != 2 {
		return "", 0, fmt.Errorf("pull request reference must be <repo>#<number>")
	}
	repo := strings.TrimSpace(parts[0])
	if repo == "" {
		return "", 0, fmt.Errorf("pull request reference must be <repo>#<number>")
	}
	prNumber, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
	if err != nil || prNumber <= 0 {
		return "", 0, fmt.Errorf("pull request reference must be <repo>#<number>")
	}
	return repo, prNumber, nil
}

// writeJSON emits the machine-readable result the reviewer agent reads back.
func writeJSON(w io.Writer, payload any) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(payload)
}
