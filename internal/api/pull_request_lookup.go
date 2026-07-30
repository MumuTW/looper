package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/forge"
	"github.com/nexu-io/looper/internal/infra/github"
)

// errPullRequestNotFound is the classified sentinel returned by the production
// lookup when the selected forge reports the pull request does not exist. The
// Handler translates only this into HTTP 404; every other lookup failure stays
// a retryable server error so a transient `gh`/Forgejo outage is not reported
// as a permanently missing target.
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
// The selected project's provider decides which forge answers. A Forgejo
// project is read through its Forgejo client — the same selection
// workerGitHubAdapter.ViewPullRequest applies — so a Forgejo PR is not silently
// queried against an unrelated same-slug GitHub repository. Projects the
// resolver does not bind to Forgejo fall through to the GitHub gateway.
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
	resolver := forge.NewResolver(cfg)
	return func(ctx context.Context, repo string, prNumber int64, cwd string) (PullRequestTarget, error) {
		if client, ok, err := resolver.ForgejoForLocation(repo, cwd); ok || err != nil {
			if err != nil {
				return PullRequestTarget{}, err
			}
			pr, viewErr := client.ViewPullRequest(ctx, prNumber)
			if viewErr != nil {
				return PullRequestTarget{}, classifyPullRequestLookupError(viewErr)
			}
			return PullRequestTarget{Number: pr.Number, State: pr.State}, nil
		}
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
	var httpErr *forge.ForgejoHTTPError
	if errors.As(err, &httpErr) && httpErr.HTTPStatusCode() == http.StatusNotFound {
		return fmt.Errorf("%w: %w", errPullRequestNotFound, err)
	}
	return err
}
