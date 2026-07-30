package github

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nexu-io/looper/internal/outboundguard"
)

// DeploymentEnvironment is the environment Looper records its deploys under. It
// is fixed rather than configurable because it is an identity, not a policy: a
// deploy Looper did not perform must never be mistaken for one it did.
const DeploymentEnvironment = "looper"

type BranchHeadInput struct {
	Repo   string
	Branch string
	CWD    string
}

// GetBranchHeadSHA returns the commit a branch currently points at.
func (g *Gateway) GetBranchHeadSHA(ctx context.Context, input BranchHeadInput) (string, error) {
	branch := strings.TrimSpace(input.Branch)
	if branch == "" {
		return "", fmt.Errorf("branch is required")
	}
	hostname, repo := splitRepoHostname(input.Repo)
	args := []string{"api", fmt.Sprintf("repos/%s/commits/%s", repo, branch), "--jq", ".sha"}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	result, err := g.runGh(ctx, input.CWD, "", args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(result.Stdout), nil
}

// DeploymentState is the outcome recorded against a deployment.
type DeploymentState string

const (
	DeploymentStateSuccess    DeploymentState = "success"
	DeploymentStateFailure    DeploymentState = "failure"
	DeploymentStateInProgress DeploymentState = "in_progress"
)

type DeploymentRecord struct {
	ID    int64
	SHA   string
	State DeploymentState
	// CreatedAt bounds how long an unfinished deployment holds its commit. Without
	// it an interrupted deploy would claim the commit forever.
	CreatedAt time.Time
}

type ListDeploymentsInput struct {
	Repo string
	SHA  string
	CWD  string
}

// LatestDeploymentForSHA returns Looper's most recent deployment of a commit,
// with its current state, or ok=false when the commit has never been deployed.
//
// This is the idempotency authority for deploying: GitHub already models "this
// commit was deployed", so Looper does not keep a private record that could
// disagree with it.
func (g *Gateway) LatestDeploymentForSHA(ctx context.Context, input ListDeploymentsInput) (DeploymentRecord, bool, error) {
	sha := strings.TrimSpace(input.SHA)
	if sha == "" {
		return DeploymentRecord{}, false, fmt.Errorf("sha is required")
	}
	hostname, repo := splitRepoHostname(input.Repo)
	endpoint := fmt.Sprintf("repos/%s/deployments?sha=%s&environment=%s&per_page=1", repo, sha, DeploymentEnvironment)
	args := []string{"api", endpoint}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	result, err := g.runGh(ctx, input.CWD, "", args...)
	if err != nil {
		return DeploymentRecord{}, false, err
	}
	rows, err := decodeJSONArray(result.Stdout)
	if err != nil {
		return DeploymentRecord{}, false, err
	}
	if len(rows) == 0 {
		return DeploymentRecord{}, false, nil
	}
	record := DeploymentRecord{
		ID: asInt64(rows[0]["id"]), SHA: asString(rows[0]["sha"]),
		CreatedAt: parseDeploymentTime(asString(rows[0]["created_at"])),
	}
	state, err := g.latestDeploymentState(ctx, repo, hostname, record.ID, input.CWD)
	if err != nil {
		return DeploymentRecord{}, false, err
	}
	record.State = state
	return record, true, nil
}

func parseDeploymentTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(value))
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func (g *Gateway) latestDeploymentState(ctx context.Context, repo, hostname string, deploymentID int64, cwd string) (DeploymentState, error) {
	args := []string{"api", fmt.Sprintf("repos/%s/deployments/%d/statuses?per_page=1", repo, deploymentID)}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	result, err := g.runGh(ctx, cwd, "", args...)
	if err != nil {
		return "", err
	}
	rows, err := decodeJSONArray(result.Stdout)
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", nil
	}
	return DeploymentState(asString(rows[0]["state"])), nil
}

// LatestSuccessfulDeploymentSHA returns the commit of Looper's most recent
// successful deployment, or "" when there has never been one. It exists only to
// build a "what changed since last time" comparison for a human.
func (g *Gateway) LatestSuccessfulDeploymentSHA(ctx context.Context, input ListDeploymentsInput) (string, error) {
	hostname, repo := splitRepoHostname(input.Repo)
	// Deployments come back newest first; scan a bounded page rather than paging
	// through history, since a comparison link older than this is not useful.
	args := []string{"api", fmt.Sprintf("repos/%s/deployments?environment=%s&per_page=30", repo, DeploymentEnvironment)}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	result, err := g.runGh(ctx, input.CWD, "", args...)
	if err != nil {
		return "", err
	}
	rows, err := decodeJSONArray(result.Stdout)
	if err != nil {
		return "", err
	}
	for _, row := range rows {
		id := asInt64(row["id"])
		if id == 0 {
			continue
		}
		state, err := g.latestDeploymentState(ctx, repo, hostname, id, input.CWD)
		if err != nil {
			return "", err
		}
		if state == DeploymentStateSuccess {
			return asString(row["sha"]), nil
		}
	}
	return "", nil
}

type CreateDeploymentInput struct {
	Repo        string
	SHA         string
	Description string
	CWD         string
}

// CreateDeployment records the intent to deploy a commit and returns its id.
//
// auto_merge is forced off: GitHub's default is true, which makes this endpoint
// merge the base branch into the ref and return 202 without creating anything —
// a silent no-op that also writes a merge commit nobody asked for.
// required_contexts is forced empty for the same reason: the default refuses to
// create a deployment while any check is pending, and Looper already gates on
// branch protection before merging.
func (g *Gateway) CreateDeployment(ctx context.Context, input CreateDeploymentInput) (int64, error) {
	sha := strings.TrimSpace(input.SHA)
	if sha == "" {
		return 0, fmt.Errorf("sha is required")
	}
	if err := outboundguard.Validate(outboundguard.Field{Name: "deployment description", Text: input.Description}); err != nil {
		return 0, err
	}
	hostname, repo := splitRepoHostname(input.Repo)
	args := []string{
		"api", fmt.Sprintf("repos/%s/deployments", repo), "--method", "POST",
		"-f", "ref=" + sha,
		"-f", "environment=" + DeploymentEnvironment,
		"-f", "description=" + input.Description,
		"-F", "auto_merge=false",
		"-f", "required_contexts[]=",
	}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	result, err := g.runGh(ctx, input.CWD, "", args...)
	if err != nil {
		return 0, err
	}
	row, err := decodeJSONObject(result.Stdout)
	if err != nil {
		return 0, err
	}
	id := asInt64(row["id"])
	if id == 0 {
		// A 202 with no id is the auto_merge path above. Treat it as a failure
		// rather than reporting a deploy that was never recorded.
		return 0, fmt.Errorf("github did not create a deployment for %s", sha)
	}
	return id, nil
}

type DeploymentStatusInput struct {
	Repo         string
	DeploymentID int64
	State        DeploymentState
	Description  string
	LogURL       string
	CWD          string
}

// SetDeploymentStatus records the outcome of a deployment.
func (g *Gateway) SetDeploymentStatus(ctx context.Context, input DeploymentStatusInput) error {
	if input.DeploymentID == 0 {
		return fmt.Errorf("deployment id is required")
	}
	if err := outboundguard.Validate(outboundguard.Field{Name: "deployment status description", Text: input.Description}); err != nil {
		return err
	}
	hostname, repo := splitRepoHostname(input.Repo)
	// GitHub rejects a description longer than 140 characters. Truncate on a rune
	// boundary: a byte slice can split a multi-byte character and produce invalid
	// UTF-8, which the API rejects for a different and much less obvious reason.
	description := input.Description
	if utf8.RuneCountInString(description) > 140 {
		description = string([]rune(description)[:139]) + "…"
	}
	args := []string{
		"api", fmt.Sprintf("repos/%s/deployments/%d/statuses", repo, input.DeploymentID), "--method", "POST",
		"-f", "state=" + string(input.State),
		"-f", "description=" + description,
	}
	if strings.TrimSpace(input.LogURL) != "" {
		args = append(args, "-f", "log_url="+strings.TrimSpace(input.LogURL))
	}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	_, err := g.runGh(ctx, input.CWD, "", args...)
	return err
}
