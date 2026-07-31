package reviewsubmit

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

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
