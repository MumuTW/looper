package api

import (
	"context"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/infra/github"
)

// NewGatewayPullRequestLookup builds the production Context.LookupPullRequest: a
// direct forge read for pull requests no snapshot covers yet.
//
// This is wired explicitly rather than defaulted inside the Handler so that a
// process which never configures it — tests, and any embedding that has no forge
// access — keeps the snapshot-only behavior instead of silently shelling out.
func NewGatewayPullRequestLookup(cfg config.Config, now func() time.Time) func(context.Context, string, int64, string) (PullRequestTarget, error) {
	ghPath := ""
	if cfg.Tools.GHPath != nil {
		ghPath = strings.TrimSpace(*cfg.Tools.GHPath)
	}
	gateway := github.New(github.Options{GHPath: ghPath, Env: github.AuthEnv(cfg), Now: now})
	return func(ctx context.Context, repo string, prNumber int64, cwd string) (PullRequestTarget, error) {
		detail, err := gateway.ViewPullRequest(ctx, github.ViewPullRequestInput{Repo: repo, PRNumber: prNumber, CWD: cwd})
		if err != nil {
			return PullRequestTarget{}, err
		}
		return PullRequestTarget{
			Number: detail.Number,
			State:  detail.State,
			Merged: strings.TrimSpace(detail.MergedAt) != "",
		}, nil
	}
}
