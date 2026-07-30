package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/infra/github"
)

// errPullRequestNotFound is the classified sentinel returned by the production
// lookup when the selected forge reports the pull request does not exist. The
// Handler translates only this into HTTP 404; every other lookup failure stays
// a retryable server error so a transient `gh` outage is not reported as a
// permanently missing target.
var errPullRequestNotFound = errors.New("pull request not found")

// IsPullRequestLookupNotFound reports whether a LookupPullRequest error is the
// classified "the PR does not exist" result, as opposed to an operational
// failure the caller should retry.
func IsPullRequestLookupNotFound(err error) bool {
	return errors.Is(err, errPullRequestNotFound)
}

// NewGatewayPullRequestLookup builds the production Context.LookupPullRequest: a
// direct forge read for pull requests no snapshot covers yet, and the freshness
// check for ones a snapshot does cover.
//
// This is wired explicitly rather than defaulted inside the Handler so that a
// process which never configures it — tests, and any embedding that has no forge
// access — keeps the snapshot-only behavior instead of silently shelling out.
func NewGatewayPullRequestLookup(cfg config.Config, now func() time.Time) func(context.Context, string, int64, string) (PullRequestTarget, error) {
	ghPath := ""
	if cfg.Tools.GHPath != nil {
		ghPath = strings.TrimSpace(*cfg.Tools.GHPath)
	}
	gateway := github.New(github.Options{GHPath: ghPath, Now: now})
	return func(ctx context.Context, repo string, prNumber int64, cwd string) (PullRequestTarget, error) {
		detail, err := gateway.ViewPullRequest(ctx, github.ViewPullRequestInput{Repo: repo, PRNumber: prNumber, CWD: cwd})
		if err != nil {
			return PullRequestTarget{}, classifyPullRequestLookupError(err)
		}
		return PullRequestTarget{
			Number: detail.Number,
			State:  detail.State,
			Merged: strings.TrimSpace(detail.MergedAt) != "",
		}, nil
	}
}

// classifyPullRequestLookupError wraps a forge error with errPullRequestNotFound
// only when the forge classified it as "the PR does not exist". Operational
// failures (timeouts, unavailable executables, 5xx) pass through unmodified so
// the Handler can surface them as retryable server errors instead of 404.
func classifyPullRequestLookupError(err error) error {
	if err == nil {
		return nil
	}
	if github.IsPullRequestNotFoundError(err) {
		return fmt.Errorf("%w: %w", errPullRequestNotFound, err)
	}
	return err
}
