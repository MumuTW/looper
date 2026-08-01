package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/infra/github"
	"github.com/MumuTW/looper/internal/storage"
	pkgapi "github.com/MumuTW/looper/pkg/api"
)

// errIssueNotFound is the classified sentinel returned by the production
// issue-occupancy lookup when the forge reports the issue does not exist. The
// Handler translates only this into HTTP 404; every other lookup failure stays
// a retryable server error so a transient `gh` outage is not reported as a
// permanently missing target.
var errIssueNotFound = errors.New("issue not found")

// IsIssueLookupNotFound reports whether an issue-occupancy lookup error is the
// classified "the issue does not exist" result, as opposed to an operational
// failure the caller should retry.
func IsIssueLookupNotFound(err error) bool {
	return errors.Is(err, errIssueNotFound)
}

// IssueOccupancy is the forge-side answer to "is someone already doing this
// issue": whether the issue is still open, and the open pull requests that
// reference (close) it. A closed issue or a non-empty OpenPullRequests list
// means dispatching a worker would duplicate work the forge already knows about.
type IssueOccupancy struct {
	Repo             string
	IssueNumber      int64
	State            string
	IsPullRequest    bool
	OpenPullRequests []IssueOccupantPullRequest
}

// IssueOccupantPullRequest is a pull request the forge links to the issue. Only
// the fields needed to name the occupant in a refusal message are carried.
type IssueOccupantPullRequest struct {
	Number int64
	State  string
	Repo   string
	URL    string
}

// Occupied reports whether the forge signals the issue is already handled: the
// issue is closed, or at least one open pull request references it.
func (o IssueOccupancy) Occupied() bool {
	if o.IsPullRequest {
		return true
	}
	if strings.TrimSpace(o.State) != "" && !strings.EqualFold(strings.TrimSpace(o.State), "open") {
		return true
	}
	return len(o.OpenPullRequests) > 0
}

// NewGatewayIssueOccupancyLookup builds the production Context.LookupIssueOccupancy:
// a direct forge read of the issue's state plus the pull requests GitHub links to
// it via closing references. Mirrors NewGatewayPullRequestLookup: wired explicitly
// so a process with no forge access (tests, embeddings) keeps the prior behavior
// instead of silently shelling out.
func NewGatewayIssueOccupancyLookup(cfg config.Config, now func() time.Time) func(context.Context, string, int64, string) (IssueOccupancy, error) {
	ghPath := ""
	if cfg.Tools.GHPath != nil {
		ghPath = strings.TrimSpace(*cfg.Tools.GHPath)
	}
	gateway := github.New(github.Options{GHPath: ghPath, Env: config.DaemonGitHubCredentialEnv(cfg), Now: now})
	return newGatewayIssueOccupancyLookup(gateway)
}

func newGatewayIssueOccupancyLookup(gateway *github.Gateway) func(context.Context, string, int64, string) (IssueOccupancy, error) {
	return func(ctx context.Context, repo string, issueNumber int64, cwd string) (IssueOccupancy, error) {
		// ViewIssue is the same call the worker's prepare-work step makes, so the
		// dispatch check reuses the established authority rather than introducing a
		// second forge shape. It also distinguishes issue-vs-PR (a number that
		// resolves to a PR is rejected upstream by the worker).
		issue, err := gateway.ViewIssue(ctx, github.ViewIssueInput{Repo: repo, IssueNumber: issueNumber, CWD: cwd})
		if err != nil {
			return IssueOccupancy{}, classifyIssueLookupError(err)
		}
		occupancy := IssueOccupancy{Repo: repo, IssueNumber: issueNumber, State: issue.State, IsPullRequest: issue.IsPullRequest}
		// Only fetch linked PRs for a real issue — a PR target is rejected upstream
		// by the worker, and asking closedByPullRequestsReferences on a PR errors.
		if issue.IsPullRequest {
			return occupancy, nil
		}
		linked, linkErr := gateway.ListLinkedPullRequests(ctx, github.LinkedPullRequestsInput{Repo: repo, IssueNumber: issueNumber, CWD: cwd})
		if linkErr != nil {
			// Linked-PR read failing is not fatal: the issue-state check alone still
			// catches closed issues. An open-PR occupant could be missed, but a
			// transient forge error must not block dispatch with a 500.
			if occupancy.Occupied() || github.IsTransientError(linkErr) {
				return occupancy, nil
			}
			return IssueOccupancy{}, classifyIssueLookupError(linkErr)
		}
		for _, pr := range linked {
			if strings.EqualFold(strings.TrimSpace(pr.State), "OPEN") {
				occupancy.OpenPullRequests = append(occupancy.OpenPullRequests, IssueOccupantPullRequest{Number: pr.Number, State: pr.State, Repo: pr.Repo, URL: pr.URL})
			}
		}
		return occupancy, nil
	}
}

// classifyIssueLookupError wraps a forge error with errIssueNotFound only when
// the forge classified it as "the issue does not exist" (HTTP 404). Operational
// failures (timeouts, unavailable executables, 5xx) pass through unmodified so
// the Handler can surface them as retryable server errors instead of 404.
func classifyIssueLookupError(err error) error {
	if err == nil {
		return nil
	}
	if github.IsNotFoundError(err) {
		return fmt.Errorf("%w: %w", errIssueNotFound, err)
	}
	return err
}

// issueCollisionError translates a storage issue-claim conflict into the HTTP
// 409 loop-conflict response shared by create and retry mutations. Keeping the
// mapping here lets every issue-claim admission path report the same occupant
// payload instead of each route handler reinventing the envelope.
func issueCollisionError(issueNumber int64, loopID, loopType string) apiError {
	return apiError{
		code:    pkgapi.ErrorCodeLoopConflict,
		status:  http.StatusConflict,
		message: fmt.Sprintf("Issue #%d is occupied by active %s loop %s", issueNumber, loopType, loopID),
		details: map[string]any{"occupiedBy": map[string]any{"loopId": loopID, "loopType": loopType}},
	}
}

// mapIssueClaimAdmissionError converts a storage-layer issue-claim conflict
// into the shared 409 apiError, passing through any non-conflict error so
// operational failures surface as retryable server errors.
func mapIssueClaimAdmissionError(err error) error {
	if conflict, ok := storage.IsIssueClaimConflictError(err); ok {
		return issueCollisionError(conflict.IssueNumber, conflict.LoopID, conflict.LoopType)
	}
	return err
}

// assertIssueClaimAdmission runs the storage issue-claim admission check for a
// candidate loop and maps a conflict into the shared 409 apiError. force
// overrides the claim check the same way as upsertLoopAfterIssueClaimAdmission.
func assertIssueClaimAdmission(ctx context.Context, repos *storage.Repositories, candidate storage.LoopRecord, force bool) error {
	err := repos.Loops.AssertIssueClaimAdmission(ctx, candidate, force)
	if conflict, ok := storage.IsIssueClaimConflictError(err); ok {
		return issueCollisionError(conflict.IssueNumber, conflict.LoopID, conflict.LoopType)
	}
	return err
}

// upsertLoopAfterIssueClaimAdmission persists the loop record, selecting the
// force-override upsert path when force is set so a forced create/retry can
// displace an existing issue claim. Any conflict surfaces as the shared 409.
func upsertLoopAfterIssueClaimAdmission(ctx context.Context, loops *storage.LoopsRepository, record storage.LoopRecord, force bool) error {
	var err error
	if force {
		err = loops.UpsertForcingIssueClaimAdmission(ctx, record)
	} else {
		err = loops.Upsert(ctx, record)
	}
	return mapIssueClaimAdmissionError(err)
}
