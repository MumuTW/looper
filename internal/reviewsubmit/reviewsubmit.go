package reviewsubmit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/diffanchor"
	"github.com/nexu-io/looper/internal/disclosure"
	"github.com/nexu-io/looper/internal/domain"
	"github.com/nexu-io/looper/internal/forge"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/infra/shell"
	"github.com/nexu-io/looper/internal/outboundguard"
	"github.com/nexu-io/looper/internal/reviewitem"
	"github.com/nexu-io/looper/internal/storage"
)

type reviewSubmitPayload struct {
	Body     string                `json:"body"`
	Comments []reviewSubmitComment `json:"comments"`
}

type reviewSubmitComment struct {
	Body      string `json:"body"`
	Severity  string `json:"severity"`
	Path      string `json:"path"`
	Line      int64  `json:"line"`
	Side      string `json:"side"`
	StartLine int64  `json:"start_line"`
	StartSide string `json:"start_side"`
}

type reviewSubmitDiagnosticFields struct {
	Repo        string
	PRNumber    int64
	Event       string
	CommitID    string
	Payload     reviewSubmitPayload
	Error       string
	Extra       map[string]any
	RedactPaths bool
}

type reviewSubmitPullRequestViewer interface {
	ViewPullRequest(context.Context, githubinfra.ViewPullRequestInput) (githubinfra.PullRequestDetail, error)
}

type reviewSubmitGateway interface {
	reviewSubmitPullRequestViewer
	GetCurrentUserLoginForRepo(context.Context, string, string) (string, error)
	GetPullRequestDiff(context.Context, githubinfra.GetPullRequestDiffInput) (string, error)
	SubmitReview(context.Context, githubinfra.SubmitReviewInput) error
}

func reviewSubmitGatewayForConfig(cfg config.Config, repo, cwd string, diagnostic func(string, map[string]any)) (reviewSubmitGateway, error) {
	if cfg.Tools.GHPath == nil || strings.TrimSpace(*cfg.Tools.GHPath) == "" {
		return nil, fmt.Errorf("GitHub CLI (gh) not found; install gh or set --gh-path <path>")
	}
	gitPath := "git"
	if cfg.Tools.GitPath != nil && strings.TrimSpace(*cfg.Tools.GitPath) != "" {
		gitPath = strings.TrimSpace(*cfg.Tools.GitPath)
	}
	return githubinfra.New(githubinfra.Options{
		GHPath:                 *cfg.Tools.GHPath,
		Env:                    config.DaemonGitHubCredentialEnv(cfg),
		GitPath:                gitPath,
		CWD:                    cwd,
		GHRun:                  shell.Run,
		GitRun:                 shell.Run,
		ReviewSubmitDiagnostic: diagnostic,
	}), nil
}

// Options is one `looper review submit` invocation.
//
// The flags arrive already parsed rather than as a command object because the
// caller that matters is not a human: the daemon's trusted review proxy spawns
// this binary with an argv it rewrote itself (see
// forge.applyTrustedReviewProxyPolicy), and the shape of that argv is the
// contract this package has to keep.
type Options struct {
	// Argv is the process argv after the program name. It is forwarded
	// verbatim to the daemon-side proxy when one is configured, so the proxy
	// re-validates and re-policies exactly what the agent asked for rather
	// than a reconstruction of it.
	Argv []string
	// ConfigArgs are the global flags config.LoadFile parses. Separate from
	// Argv because the proxy rejects --config outright: a compromised agent
	// must not be able to redirect the daemon-injected provider token.
	ConfigArgs []string

	PRRef               string
	Event               string
	CommitID            string
	CleanReviewEvent    string
	BlockingReviewEvent string
	ReviewerManual      bool
	ReviewerRunID       string

	CWD    string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Run publishes a validated pull request review.
//
// It is the child half of the trusted review proxy: reviewer agents are told to
// invoke it through a daemon-written wrapper, and the daemon spawns this same
// binary with provider credentials injected and a config snapshot on an
// inherited descriptor. Everything the reviewer prompt promises about review
// publication — marker validation, head/base drift checks, anchor authority,
// hold gates, self-approval downgrade — is enforced here, because there is no
// other place between the agent and the forge that can enforce it.
func Run(ctx context.Context, opts Options) error {
	stdout, stderr := opts.Stdout, opts.Stderr

	// When a daemon-side trusted review proxy is configured, forward the full
	// invocation there so provider tokens stay out of the agent process and out
	// of any agent-visible credential path. The proxy child clears the socket env
	// and re-enters this command with tokens injected.
	if forge.TrustedReviewSockConfigured() {
		raw, err := io.ReadAll(opts.Stdin)
		if err != nil {
			return fmt.Errorf("read review payload from stdin: %w", err)
		}
		// Argv is forwarded whole. The proxy re-validates the shape and rebinds
		// the policy flags itself, so anything reconstructed here from the
		// parsed options would be a second, divergent account of what the agent
		// asked for.
		return forge.ProxyReviewSubmit(append([]string(nil), opts.Argv...), raw, opts.CWD)
	}

	repo, prNumber, err := parsePullRequestRef(opts.PRRef)
	if err != nil {
		return err
	}
	event, err := validateReviewSubmitEvent(opts.Event)
	if err != nil {
		return err
	}
	commitID := strings.TrimSpace(opts.CommitID)
	if commitID == "" {
		return fmt.Errorf("review submit requires --commit-id expected PR head SHA")
	}

	raw, err := io.ReadAll(opts.Stdin)
	if err != nil {
		return fmt.Errorf("read review payload from stdin: %w", err)
	}
	var payload reviewSubmitPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return fmt.Errorf("parse review payload JSON from stdin: %w", err)
	}

	loaded, err := loadConfig(opts)
	if err != nil {
		return err
	}
	policy, err := effectiveReviewSubmitPolicy(
		loaded.Config.Roles.Reviewer.Behavior.ReviewEvents,
		opts.CleanReviewEvent,
		opts.BlockingReviewEvent,
	)
	if err != nil {
		return err
	}
	if err := validateReviewSubmitEventAllowed(event, policy); err != nil {
		return err
	}
	cwd := opts.CWD

	diagnosticWriter := func(event string, fields map[string]any) {
		writeReviewSubmitDiagnosticEntry(stderr, event, fields)
	}
	gateway, err := reviewSubmitGatewayForConfig(loaded.Config, repo, cwd, diagnosticWriter)
	if err != nil {
		return err
	}
	detail, err := gateway.ViewPullRequest(ctx, githubinfra.ViewPullRequestInput{Repo: repo, PRNumber: prNumber, CWD: cwd})
	if err != nil {
		return fmt.Errorf("refresh pull request before review submit: %w", err)
	}
	if err := validateExpectedHeadCommit(commitID, detail.HeadSHA); err != nil {
		return err
	}
	if err := validateReviewerReviewSubmitHold(ctx, loaded.Config, repo, prNumber, opts.ReviewerManual, opts.ReviewerRunID, detail.Labels); err != nil {
		return err
	}
	if err := validateReviewSubmitBody(payload.Body, payload.Comments, commitID, event, policy, detail.Author); err != nil {
		// Always redact paths on pre-gate validation diagnostics: a malformed
		// marker or APPROVE-with-comments path never reaches SubmitReview's
		// content guard, and path may itself be secret-shaped.
		writeReviewSubmitDiagnostic(stderr, "github_review_submit_validation_failed", reviewSubmitDiagnosticFields{
			Repo: repo, PRNumber: prNumber, Event: event, CommitID: commitID, Payload: payload, Error: err.Error(), RedactPaths: true,
		})
		return err
	}
	submissionEvent, err := effectiveReviewSubmitEvent(ctx, stderr, gateway, repo, prNumber, event, detail.Author, cwd)
	if err != nil {
		return err
	}

	// Authority for whether an inline review comment is valid is the complete
	// diff between the exact PR base and submitted head SHAs — not a bounded
	// prefix of `gh pr diff` and not the agent-provided line alone.
	anchors, err := resolveReviewSubmitAnchors(ctx, gateway, repo, prNumber, cwd, detail, payload.Comments)
	if err != nil {
		if canSubmitWithoutAnchorValidation(err, payload.Comments) {
			// Body-only oversized/truncated fallback still must fail closed on base/head
			// drift: hold-only refresh is not enough when commit_id was captured earlier.
			if _, err := validateLatestReviewerReviewSubmitPublication(ctx, gateway, loaded.Config, repo, prNumber, commitID, detail.BaseSHA, opts.ReviewerManual, opts.ReviewerRunID, cwd); err != nil {
				return err
			}
			return submitReviewWithoutAnchorValidation(ctx, stdout, stderr, gateway, repo, prNumber, submissionEvent, payload, commitID, cwd, loaded.Config.Disclosure)
		}
		// Never reach SubmitReview's content guard on this path: redact paths and
		// never return path-bearing git/remote errors (path may be secret-shaped).
		writeReviewSubmitDiagnostic(stderr, "github_review_submit_validation_failed", reviewSubmitDiagnosticFields{
			Repo: repo, PRNumber: prNumber, Event: submissionEvent, CommitID: commitID, Payload: payload,
			Error: githubinfra.AnchorValidationUnavailableReason, RedactPaths: true,
			Extra: map[string]any{"reason": githubinfra.AnchorValidationUnavailableReason},
		})
		// Retryable sentinel only — authority errors can embed `git diff ... -- <path>` argv.
		return fmt.Errorf("resolve PR diff anchor authority for review submit: %w", githubinfra.ErrAnchorValidationUnavailable)
	}

	comments, err := buildReviewSubmitComments(payload.Comments)
	if err != nil {
		return err
	}
	// Fail closed on base/head drift between anchor resolution and mutation.
	if _, err := validateLatestReviewerReviewSubmitPublication(ctx, gateway, loaded.Config, repo, prNumber, commitID, detail.BaseSHA, opts.ReviewerManual, opts.ReviewerRunID, cwd); err != nil {
		return err
	}
	if err := gateway.SubmitReview(ctx, githubinfra.SubmitReviewInput{Repo: repo, PRNumber: prNumber, Event: submissionEvent, Body: payload.Body, CommitID: commitID, Comments: comments, Anchors: anchors, Disclosure: loaded.Config.Disclosure, CWD: cwd}); err != nil {
		return wrapReviewSubmitError(stderr, repo, prNumber, submissionEvent, commitID, payload, "submit validated PR review", err)
	}
	return writeJSON(stdout, map[string]any{"submitted": true})
}

// resolveReviewSubmitAnchors establishes complete base/head anchor authority.
// For actionable inline comments it prefers path-targeted local diffs (GitHub
// gateway) and never treats a truncated remote capture as authoritative.
// Body-only reviews may still proceed when only GitHub oversized / local
// capture limits block a full remote diff.
func resolveReviewSubmitAnchors(ctx context.Context, gateway reviewSubmitGateway, repo string, prNumber int64, cwd string, detail githubinfra.PullRequestDetail, comments []reviewSubmitComment) (*diffanchor.Index, error) {
	if len(comments) == 0 {
		diff, err := gateway.GetPullRequestDiff(ctx, githubinfra.GetPullRequestDiffInput{Repo: repo, PRNumber: prNumber, CWD: cwd})
		if err != nil {
			return nil, err
		}
		parsed := diffanchor.Parse(diff)
		return &parsed, nil
	}

	// Prefer complete local base/head authority when the gateway supports it.
	if gh, ok := gateway.(*githubinfra.Gateway); ok {
		paths := make([]string, 0, len(comments))
		for _, comment := range comments {
			paths = append(paths, comment.Path)
		}
		anchors, _, err := gh.BuildReviewAnchorIndex(ctx, githubinfra.BuildReviewAnchorIndexInput{
			CWD:     cwd,
			BaseSHA: detail.BaseSHA,
			HeadSHA: detail.HeadSHA,
			Paths:   paths,
			RemoteDiff: func(ctx context.Context) (string, error) {
				return gh.GetPullRequestDiff(ctx, githubinfra.GetPullRequestDiffInput{Repo: repo, PRNumber: prNumber, CWD: cwd})
			},
		})
		if err != nil {
			return nil, err
		}
		return anchors, nil
	}

	// Non-GitHub-gateway callers: the remote PR diff is the complete authority.
	diff, err := gateway.GetPullRequestDiff(ctx, githubinfra.GetPullRequestDiffInput{Repo: repo, PRNumber: prNumber, CWD: cwd})
	if err != nil {
		return nil, err
	}
	parsed := diffanchor.Parse(diff)
	return &parsed, nil
}

// wrapReviewSubmitError keeps content-safety rejections actionable for agents
// still in-session: surface the gate reason + recovery guidance, never the
// rejected payload, and record a validation diagnostic without raw paths.
func wrapReviewSubmitError(stderr io.Writer, repo string, prNumber int64, event string, commitID string, payload reviewSubmitPayload, prefix string, err error) error {
	if outboundguard.IsRejection(err) {
		writeReviewSubmitDiagnostic(stderr, "github_review_submit_validation_failed", reviewSubmitDiagnosticFields{
			Repo: repo, PRNumber: prNumber, Event: event, CommitID: commitID, Payload: payload, Error: err.Error(), RedactPaths: true,
		})
		return fmt.Errorf("%s blocked by content safety gate: %w", prefix, err)
	}
	return fmt.Errorf("%s: %w", prefix, err)
}

func effectiveReviewSubmitEvent(ctx context.Context, stderr io.Writer, gh reviewSubmitGateway, repo string, prNumber int64, event string, authorLogin string, cwd string) (string, error) {
	if !strings.EqualFold(strings.TrimSpace(event), "APPROVE") || strings.TrimSpace(authorLogin) == "" {
		return event, nil
	}
	currentLogin, err := gh.GetCurrentUserLoginForRepo(ctx, repo, cwd)
	if err != nil {
		return "", fmt.Errorf("determine authenticated GitHub user for self-approval check: %w", err)
	}
	if !sameGitHubLogin(currentLogin, authorLogin) {
		return event, nil
	}
	_, _ = fmt.Fprintf(stderr, "looper: downgrading APPROVE review to COMMENT for %s#%d because authenticated GitHub user %q authored the pull request and GitHub does not allow self-approval\n", repo, prNumber, strings.TrimSpace(currentLogin))
	return "COMMENT", nil
}

func sameGitHubLogin(a string, b string) bool {
	a = strings.TrimSpace(strings.TrimPrefix(a, "@"))
	b = strings.TrimSpace(strings.TrimPrefix(b, "@"))
	return a != "" && strings.EqualFold(a, b)
}

func validateReviewSubmitEvent(raw string) (string, error) {
	event := strings.ToUpper(strings.TrimSpace(raw))
	if event == "" {
		return "", fmt.Errorf("review submit requires --event COMMENT, APPROVE, or REQUEST_CHANGES")
	}
	if event != "COMMENT" && event != "APPROVE" && event != "REQUEST_CHANGES" {
		return "", fmt.Errorf("unsupported review event %q", event)
	}
	return event, nil
}

func validateReviewSubmitPolicy(policy config.ReviewerReviewEventsConfig) error {
	if policy.Clean != config.ReviewerReviewEventComment && policy.Clean != config.ReviewerReviewEventApprove {
		return fmt.Errorf("clean review event policy must be COMMENT or APPROVE")
	}
	if policy.Blocking != config.ReviewerReviewEventComment && policy.Blocking != config.ReviewerReviewEventRequestChanges {
		return fmt.Errorf("blocking review event policy must be COMMENT or REQUEST_CHANGES")
	}
	return nil
}

func effectiveReviewSubmitPolicy(base config.ReviewerReviewEventsConfig, cleanOverride string, blockingOverride string) (config.ReviewerReviewEventsConfig, error) {
	if err := validateReviewSubmitPolicy(base); err != nil {
		return config.ReviewerReviewEventsConfig{}, err
	}
	policy := base
	if value := strings.TrimSpace(cleanOverride); value != "" {
		policy.Clean = config.ReviewerReviewEvent(strings.ToUpper(value))
	}
	if value := strings.TrimSpace(blockingOverride); value != "" {
		policy.Blocking = config.ReviewerReviewEvent(strings.ToUpper(value))
	}
	if err := validateReviewSubmitPolicy(policy); err != nil {
		return config.ReviewerReviewEventsConfig{}, err
	}
	return policy, nil
}

func validateReviewSubmitEventAllowed(event string, policy config.ReviewerReviewEventsConfig) error {
	switch strings.ToUpper(strings.TrimSpace(event)) {
	case "APPROVE":
		if policy.Clean != config.ReviewerReviewEventApprove {
			return fmt.Errorf("review submit --event APPROVE requires roles.reviewer.behavior.reviewEvents.clean=APPROVE")
		}
	case "REQUEST_CHANGES":
		if policy.Blocking != config.ReviewerReviewEventRequestChanges {
			return fmt.Errorf("review submit --event REQUEST_CHANGES requires roles.reviewer.behavior.reviewEvents.blocking=REQUEST_CHANGES")
		}
	}
	return nil
}

var reviewSubmitMarkerRE = regexp.MustCompile(`<!--\s*looper:review\s+([^>]*)-->`)
var markdownHTMLCommentRE = regexp.MustCompile(`(?s)<!--.*?-->`)
var markdownReferenceDefinitionRE = regexp.MustCompile(`(?m)^\s{0,3}\[[^\]\n]+\]:[^\n]*(?:\n[ \t]+[^\n]*)*`)

func validateReviewSubmitBody(body string, comments []reviewSubmitComment, commitID string, event string, policy config.ReviewerReviewEventsConfig, authorLogin string) error {
	matches := reviewSubmitMarkerRE.FindAllStringSubmatch(body, -1)
	if len(matches) != 1 {
		return fmt.Errorf("review body must contain exactly one well-formed looper review marker")
	}
	fields := parseReviewSubmitMarkerFields(matches[0][1])
	outcome := fields["outcome"]
	if fields["id"] == "" || fields["head"] == "" || !isValidReviewSubmitOutcome(outcome) {
		return fmt.Errorf("review body must contain exactly one well-formed looper review marker")
	}
	if !strings.EqualFold(fields["head"], strings.TrimSpace(commitID)) {
		return fmt.Errorf("review marker head=%s does not match --commit-id %s", fields["head"], strings.TrimSpace(commitID))
	}
	switch event {
	case "APPROVE":
		if outcome != "clean" {
			return fmt.Errorf("review marker outcome=%s does not match APPROVE event", outcome)
		}
		if len(comments) > 0 {
			return fmt.Errorf("APPROVE reviews require clean outcome without inline comments")
		}
		if err := validateCleanApproveBody(body, authorLogin); err != nil {
			return err
		}
	case "REQUEST_CHANGES":
		if outcome != "blocking" {
			return fmt.Errorf("review marker outcome=%s does not match REQUEST_CHANGES event", outcome)
		}
	case "COMMENT":
		if outcome == "clean" && policy.Clean == config.ReviewerReviewEventApprove {
			return fmt.Errorf("review marker outcome=clean requires APPROVE under effective policy")
		}
		if outcome == "blocking" && policy.Blocking == config.ReviewerReviewEventRequestChanges {
			return fmt.Errorf("review marker outcome=blocking requires REQUEST_CHANGES under effective policy")
		}
	}
	if err := validateReviewItemSeverities(outcome, comments); err != nil {
		return err
	}
	return nil
}

func validateReviewItemSeverities(outcome string, comments []reviewSubmitComment) error {
	for index, comment := range comments {
		severity, err := reviewitem.ParseSeverity(comment.Severity)
		if err != nil {
			return fmt.Errorf("review comment %d: %w", index+1, err)
		}
		if severity == reviewitem.SeverityBlocking && outcome != "blocking" && outcome != "actionable" {
			return fmt.Errorf("review comment %d severity=blocking exceeds review outcome=%s", index+1, outcome)
		}
		if _, err := reviewitem.AttachMarker(comment.Body, severity); err != nil {
			return fmt.Errorf("review comment %d: %w", index+1, err)
		}
	}
	return nil
}

func buildReviewSubmitComments(input []reviewSubmitComment) ([]githubinfra.ReviewComment, error) {
	comments := make([]githubinfra.ReviewComment, 0, len(input))
	for index, comment := range input {
		severity, err := reviewitem.ParseSeverity(comment.Severity)
		if err != nil {
			return nil, fmt.Errorf("review comment %d: %w", index+1, err)
		}
		body, err := reviewitem.AttachMarker(comment.Body, severity)
		if err != nil {
			return nil, fmt.Errorf("review comment %d: %w", index+1, err)
		}
		comments = append(comments, githubinfra.ReviewComment{Body: body, Path: comment.Path, Line: comment.Line, Side: comment.Side, StartLine: comment.StartLine, StartSide: comment.StartSide})
	}
	return comments, nil
}

func validateCleanApproveBody(body string, authorLogin string) error {
	visible := cleanReviewHumanBody(body)
	mention := authorMention(authorLogin)
	if mention == "" {
		return fmt.Errorf("APPROVE clean review body requires the PR author login for @mention validation")
	}
	fields := strings.Fields(visible)
	if len(fields) == 0 || !strings.EqualFold(fields[0], mention) {
		return fmt.Errorf("APPROVE clean review body must start with an @mention of the PR author")
	}
	if len(fields) < 12 {
		return fmt.Errorf("APPROVE clean review body must include a short human summary and friendly acknowledgement, not only markers or disclosure")
	}
	return nil
}

func cleanReviewHumanBody(body string) string {
	cleaned := reviewSubmitMarkerRE.ReplaceAllString(body, "")
	cleaned = disclosure.StripMarkdownStamp(cleaned)
	cleaned = markdownHTMLCommentRE.ReplaceAllString(cleaned, "")
	cleaned = markdownReferenceDefinitionRE.ReplaceAllString(cleaned, "")
	return strings.TrimSpace(cleaned)
}

func authorMention(login string) string {
	login = strings.TrimSpace(strings.TrimPrefix(login, "@"))
	if login == "" {
		return ""
	}
	return "@" + login
}

func isValidReviewSubmitOutcome(outcome string) bool {
	switch outcome {
	case "clean", "non_blocking", "blocking", "actionable":
		return true
	default:
		return false
	}
}

func parseReviewSubmitMarkerFields(segment string) map[string]string {
	fields := map[string]string{}
	for _, field := range strings.Fields(segment) {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		fields[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
	}
	return fields
}

func validateExpectedHeadCommit(expected string, actual string) error {
	expected = strings.TrimSpace(expected)
	actual = strings.TrimSpace(actual)
	if expected == "" {
		return fmt.Errorf("review submit requires --commit-id expected PR head SHA")
	}
	if actual == "" {
		return fmt.Errorf("validate expected PR head commit: PR head SHA is empty")
	}
	if !strings.EqualFold(expected, actual) {
		return fmt.Errorf("review submit expected head commit %s but PR head is %s; refresh the review before submitting", expected, actual)
	}
	return nil
}

func validateReviewerReviewSubmitHold(ctx context.Context, cfg config.Config, repo string, prNumber int64, manual bool, runID string, labels []string) error {
	if !domain.IsAutoLaneHeld(domain.LoopTypeReviewer, labels) {
		return nil
	}
	if manual {
		dbPath := strings.TrimSpace(cfg.Storage.DBPath)
		if dbPath == "" {
			return fmt.Errorf("reviewer review submit blocked because %s#%d is currently held", repo, prNumber)
		}
		filePath, isFile, err := storage.SQLiteFilesystemPath(dbPath)
		if err != nil {
			return fmt.Errorf("validate held manual reviewer run database path: %w", err)
		}
		if isFile {
			if _, err := os.Stat(filePath); err != nil {
				if os.IsNotExist(err) {
					return fmt.Errorf("reviewer review submit blocked because %s#%d is currently held", repo, prNumber)
				}
				return fmt.Errorf("validate held manual reviewer run: stat storage database: %w", err)
			}
		}
		db, err := storage.OpenSQLiteDBWithCompatibilityCheck(ctx, dbPath)
		if err != nil {
			return fmt.Errorf("validate held manual reviewer run: %w", err)
		}
		defer func() { _ = db.Close() }()
		trusted, err := trustedManualReviewerRun(ctx, storage.NewRepositories(db.DB), repo, prNumber, runID)
		if err != nil {
			return err
		}
		if trusted {
			return nil
		}
	}
	return fmt.Errorf("reviewer review submit blocked because %s#%d is currently held", repo, prNumber)
}

// validateLatestReviewerReviewSubmitPublication re-reads the PR before mutation
// and fails closed on head/base drift plus hold labels. Refreshed labels are
// returned so publish authority uses the latest snapshot.
func validateLatestReviewerReviewSubmitPublication(ctx context.Context, gh reviewSubmitPullRequestViewer, cfg config.Config, repo string, prNumber int64, commitID string, expectedBaseSHA string, manual bool, runID string, cwd string) ([]string, error) {
	detail, err := gh.ViewPullRequest(ctx, githubinfra.ViewPullRequestInput{Repo: repo, PRNumber: prNumber, CWD: cwd})
	if err != nil {
		return nil, fmt.Errorf("refresh pull request before review publish: %w", err)
	}
	if err := validateExpectedHeadCommit(commitID, detail.HeadSHA); err != nil {
		return nil, err
	}
	if err := validateExpectedBaseCommit(expectedBaseSHA, detail.BaseSHA); err != nil {
		return nil, err
	}
	if err := validateReviewerReviewSubmitHold(ctx, cfg, repo, prNumber, manual, runID, detail.Labels); err != nil {
		return nil, err
	}
	return detail.Labels, nil
}

func validateExpectedBaseCommit(expected string, actual string) error {
	expected = strings.TrimSpace(expected)
	actual = strings.TrimSpace(actual)
	if expected == "" || actual == "" {
		// Some fixtures omit base; only enforce when both sides are known.
		return nil
	}
	if !strings.EqualFold(expected, actual) {
		return fmt.Errorf("review submit expected base commit %s but PR base is %s; refresh the review before submitting", expected, actual)
	}
	return nil
}

func trustedManualReviewerRun(ctx context.Context, repos *storage.Repositories, repo string, prNumber int64, runID string) (bool, error) {
	loop, err := trustedCurrentReviewerLoopForRun(ctx, repos, repo, prNumber, runID)
	if err != nil {
		return false, err
	}
	if loop == nil {
		return false, nil
	}
	manualValue, _ := parseReviewSubmitJSONObject(loop.MetadataJSON)["manual"].(bool)
	return manualValue, nil
}

// trustedCurrentReviewerLoopForRun returns the loop for runID only when that
// run is the current running reviewer run for the given repo/PR.
func trustedCurrentReviewerLoopForRun(ctx context.Context, repos *storage.Repositories, repo string, prNumber int64, runID string) (*storage.LoopRecord, error) {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return nil, nil
	}
	if repos == nil || repos.Runs == nil || repos.Loops == nil {
		return nil, fmt.Errorf("validate held manual reviewer run: storage is not configured")
	}
	run, err := repos.Runs.GetByID(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("validate held manual reviewer run: %w", err)
	}
	if run == nil {
		return nil, nil
	}
	if run.Status != string(domain.RunStatusRunning) {
		return nil, nil
	}
	loop, err := repos.Loops.GetByID(ctx, run.LoopID)
	if err != nil {
		return nil, fmt.Errorf("validate held manual reviewer loop: %w", err)
	}
	loopRepo := ""
	if loop != nil && loop.Repo != nil {
		loopRepo = *loop.Repo
	}
	if loop == nil || loop.Type != string(domain.LoopTypeReviewer) || !strings.EqualFold(strings.TrimSpace(loopRepo), strings.TrimSpace(repo)) || loop.PRNumber == nil || *loop.PRNumber != prNumber {
		return nil, nil
	}
	if loop.Status != string(domain.LoopStatusRunning) {
		return nil, nil
	}
	currentRun, err := currentRunningReviewerRun(ctx, repos, repo, prNumber)
	if err != nil {
		return nil, err
	}
	if currentRun == nil || currentRun.ID != run.ID {
		return nil, nil
	}
	return loop, nil
}

func currentRunningReviewerRun(ctx context.Context, repos *storage.Repositories, repo string, prNumber int64) (*storage.RunRecord, error) {
	loops, err := repos.Loops.ListByStatuses(ctx, []string{string(domain.LoopStatusRunning)})
	if err != nil {
		return nil, fmt.Errorf("validate held manual reviewer loops: %w", err)
	}
	loopIDs := make([]string, 0, len(loops))
	for _, loop := range loops {
		loopRepo := ""
		if loop.Repo != nil {
			loopRepo = *loop.Repo
		}
		if loop.Type == string(domain.LoopTypeReviewer) && strings.EqualFold(strings.TrimSpace(loopRepo), strings.TrimSpace(repo)) && loop.PRNumber != nil && *loop.PRNumber == prNumber {
			loopIDs = append(loopIDs, loop.ID)
		}
	}
	if len(loopIDs) == 0 {
		return nil, nil
	}
	runs, err := repos.Runs.ListLatestByLoopIDs(ctx, loopIDs)
	if err != nil {
		return nil, fmt.Errorf("validate held manual reviewer runs: %w", err)
	}
	var current *storage.RunRecord
	for i := range runs {
		if runs[i].Status != string(domain.RunStatusRunning) {
			continue
		}
		if current == nil || storage.RunNewer(runs[i], *current) {
			run := runs[i]
			current = &run
		}
	}
	return current, nil
}

func parseReviewSubmitJSONObject(value *string) map[string]any {
	if value == nil || strings.TrimSpace(*value) == "" {
		return map[string]any{}
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(*value), &parsed); err != nil || parsed == nil {
		return map[string]any{}
	}
	return parsed
}

func canSubmitWithoutAnchorValidation(err error, comments []reviewSubmitComment) bool {
	if len(comments) != 0 {
		// Actionable inline comments must not silently become body-only when
		// anchor authority is unavailable; fail closed for retry instead.
		return false
	}
	return errors.Is(err, githubinfra.ErrDiffTooLarge) || errors.Is(err, githubinfra.ErrLocalCaptureTruncated)
}

func submitReviewWithoutAnchorValidation(ctx context.Context, stdout, stderr io.Writer, gh reviewSubmitGateway, repo string, prNumber int64, event string, payload reviewSubmitPayload, commitID string, cwd string, disclosureCfg config.DisclosureConfig) error {
	if err := gh.SubmitReview(ctx, githubinfra.SubmitReviewInput{Repo: repo, PRNumber: prNumber, Event: event, Body: payload.Body, CommitID: commitID, Disclosure: disclosureCfg, CWD: cwd}); err != nil {
		return wrapReviewSubmitError(stderr, repo, prNumber, event, commitID, payload, "submit PR review without anchor validation", err)
	}
	return writeJSON(stdout, map[string]any{"submitted": true})
}

func writeReviewSubmitDiagnostic(w io.Writer, event string, fields reviewSubmitDiagnosticFields) {
	entry := map[string]any{
		"repo":         fields.Repo,
		"pr_number":    fields.PRNumber,
		"event":        event,
		"review_event": fields.Event,
		"commit_id":    strings.TrimSpace(fields.CommitID),
		"method":       "POST",
		"endpoint":     fmt.Sprintf("repos/%s/pulls/%d/reviews", fields.Repo, fields.PRNumber),
		"payload": map[string]any{
			"body_marker": reviewSubmitPayloadBodyMarker(fields.Payload.Body),
			"comments":    reviewSubmitPayloadComments(fields.Payload.Comments, fields.RedactPaths),
		},
	}
	if strings.TrimSpace(fields.Error) != "" {
		entry["error"] = strings.TrimSpace(fields.Error)
	}
	for key, value := range fields.Extra {
		entry[key] = value
	}
	writeReviewSubmitDiagnosticEntry(w, event, entry)
}

func writeReviewSubmitDiagnosticEntry(w io.Writer, event string, fields map[string]any) {
	if w == nil {
		return
	}
	entry := map[string]any{"event": event}
	for key, value := range fields {
		entry[key] = value
	}
	encoded, err := json.Marshal(entry)
	if err != nil {
		return
	}
	_, _ = io.Copy(w, bytes.NewReader(append(encoded, '\n')))
}

func reviewSubmitPayloadBodyMarker(body string) map[string]any {
	matches := reviewSubmitMarkerRE.FindAllStringSubmatch(body, -1)
	if len(matches) == 0 {
		return map[string]any{}
	}
	fields := parseReviewSubmitMarkerFields(matches[0][1])
	return map[string]any{"id": fields["id"], "head": fields["head"], "outcome": fields["outcome"]}
}

func reviewSubmitPayloadComments(comments []reviewSubmitComment, redactPaths bool) []map[string]any {
	summary := make([]map[string]any, 0, len(comments))
	for idx, comment := range comments {
		entry := map[string]any{"index": idx}
		if severity, err := reviewitem.ParseSeverity(comment.Severity); err == nil {
			entry["severity"] = string(severity)
		} else if strings.TrimSpace(comment.Severity) != "" {
			entry["severity_present"] = true
		}
		if comment.Path != "" {
			if redactPaths {
				entry["path_present"] = true
			} else {
				entry["path"] = comment.Path
			}
		}
		if comment.Line > 0 {
			entry["line"] = comment.Line
		}
		if comment.Side != "" {
			entry["side"] = strings.ToUpper(strings.TrimSpace(comment.Side))
		}
		if comment.StartLine > 0 {
			entry["start_line"] = comment.StartLine
		}
		if comment.StartSide != "" {
			entry["start_side"] = strings.ToUpper(strings.TrimSpace(comment.StartSide))
		}
		summary = append(summary, entry)
	}
	return summary
}
