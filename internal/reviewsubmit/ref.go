package reviewsubmit

import (
	"fmt"
	"strconv"
	"strings"
)

// ParsePullRequestRef splits the `<owner>/<repo>#<number>` target the trusted
// review proxy binds a run to. Every failure returns the same message: the
// caller is an agent argv, and a more specific error would only describe input
// the caller already controls.
func ParsePullRequestRef(value string) (string, int64, error) {
	trimmed := strings.TrimSpace(value)
	parts := strings.Split(trimmed, "#")
	if len(parts) != 2 {
		return "", 0, fmt.Errorf("pull request reference must be <repo>#<number>")
	}

	repo := strings.TrimSpace(parts[0])
	if repo == "" {
		return "", 0, fmt.Errorf("pull request reference must be <repo>#<number>")
	}

	prNumber, err := parsePositiveInt(strings.TrimSpace(parts[1]), "pull request number")
	if err != nil {
		return "", 0, fmt.Errorf("pull request reference must be <repo>#<number>")
	}

	return repo, prNumber, nil
}

func parsePositiveInt(value string, flag string) (int64, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, fmt.Errorf("%s must be a positive integer", flag)
	}

	parsed, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", flag)
	}

	return parsed, nil
}
