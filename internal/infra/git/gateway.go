package git

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/infra/shell"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/worktreesafety"
)

const javaScriptISOStringLayout = "2006-01-02T15:04:05.000Z"

// defaultStatusMaxCapturedBytes is large enough for ordinary dirty worktrees with
// thousands of long pathnames. The shell runner's generic 256 KiB default is too
// small for worker/planner/fixer InspectHead reconciliation (~1500 long paths
// already truncates), which would fail before fallback commit and again on retry.
// Match the path-diff budget used for complete local git authority elsewhere.
const defaultStatusMaxCapturedBytes = 32 * 1024 * 1024

var (
	missingWorktreeErrorPattern = regexp.MustCompile(`(?i)is not a working tree|does not exist|not found|no such file`)
	pushConflictErrorPattern    = regexp.MustCompile(`(?i)stale info|non-fast-forward|failed to push|rejected`)

	// statusMaxCapturedBytes is the porcelain status capture budget for dirt
	// classification. Tests may lower it to prove fail-closed truncation without
	// multi-MiB fixtures; production keeps defaultStatusMaxCapturedBytes.
	statusMaxCapturedBytes = defaultStatusMaxCapturedBytes
)

var fetchRefLockRetryDelays = []time.Duration{50 * time.Millisecond, 100 * time.Millisecond}

type CheckoutMode string

const (
	CheckoutModeBranch   CheckoutMode = "branch"
	CheckoutModeDetached CheckoutMode = "detached"
)

type Options struct {
	GitPath string
	Repos   *storage.Repositories
	Now     func() time.Time
}

type Gateway struct {
	gitPath string
	repos   *storage.Repositories
	now     func() time.Time
}

type CreateWorktreeInput struct {
	ProjectID         string
	RepoPath          string
	WorktreeRoot      string
	Branch            string
	BaseBranch        string
	PRNumber          int64
	ProtectedBranches []string
	CheckoutMode      CheckoutMode
}

type RestoreWorktreeInput struct {
	ProjectID            string
	RepoPath             string
	Branch               string
	WorktreeRoot         string
	CheckoutMode         CheckoutMode
	ExpectedWorktreePath string
}

type PrepareWorktreeInput struct {
	RepoPath        string
	WorktreeRoot    string
	WorktreePath    string
	Branch          string
	Ref             string
	ExpectedHeadSHA string
	Remote          string
}

type PrepareWorktreeResult struct {
	HeadSHA string
	Clean   bool
}

type InspectHeadInput struct {
	RepoPath     string
	WorktreeRoot string
	WorktreePath string
	BaseRef      string
}

type InspectHeadResult struct {
	HeadSHA               string
	NewCommitSHAs         []string
	HasUncommittedChanges bool
	ChangedFiles          []string
}

type CommitInput struct {
	RepoPath     string
	WorktreeRoot string
	WorktreePath string
	Message      string
}

type CommitResult struct {
	CommitSHA string
}

type CleanupWorktreeInput struct {
	ProjectID         string
	RepoPath          string
	WorktreeRoot      string
	WorktreePath      string
	Branch            string
	ProtectedBranches []string
	// AdmitStart, when set, wraps the destructive `git worktree remove`
	// process Start so callers can hold admission across Start only (not
	// Wait). Runtime cleanup uses this for R7 atomicity with MarkDegraded:
	// point-in-time AllowClaim leaves a window before cmd.Start.
	AdmitStart func(start func() error) error
}

// DiscardWorktreeChangesInput discards tracked and untracked local changes in a
// managed worktree, leaving HEAD and the worktree directory itself intact.
type DiscardWorktreeChangesInput struct {
	RepoPath     string
	WorktreeRoot string
	WorktreePath string
}

// DiscardWorktreeChangesResult reports whether discard mutated the worktree.
type DiscardWorktreeChangesResult struct {
	WorktreePath string
	WasDirty     bool
	NoOp         bool
}

type PushInput struct {
	RepoPath              string
	WorktreeRoot          string
	WorktreePath          string
	Branch                string
	Remote                string
	ExpectedRemoteHeadSHA string
	ProtectedBranches     []string
}

type WorktreeListEntry struct {
	Path    string
	Branch  string
	HeadSHA string
	Bare    bool
}

type ProtectedBranchError struct {
	Branch string
}

func (e *ProtectedBranchError) Error() string {
	return fmt.Sprintf("Refusing to modify protected branch: %s", e.Branch)
}

type RemoteHeadChangedError struct {
	Branch          string
	ExpectedHeadSHA string
	ActualHeadSHA   string
}

func (e *RemoteHeadChangedError) Error() string {
	expected := e.ExpectedHeadSHA
	if expected == "" {
		expected = "unknown"
	}
	actual := e.ActualHeadSHA
	if actual == "" {
		actual = "unknown"
	}

	return fmt.Sprintf("Remote head changed for %s: expected %s, got %s", e.Branch, expected, actual)
}

func New(options Options) *Gateway {
	gitPath := strings.TrimSpace(options.GitPath)
	if gitPath == "" {
		gitPath = "git"
	}

	now := options.Now
	if now == nil {
		now = time.Now
	}

	return &Gateway{
		gitPath: gitPath,
		repos:   options.Repos,
		now:     now,
	}
}

func (g *Gateway) CreateBranch(ctx context.Context, repoPath, branch, startPoint string, protectedBranches []string) error {
	if strings.TrimSpace(startPoint) == "" {
		return fmt.Errorf("startPoint is required")
	}
	protected := append([]string{}, protectedBranches...)
	if startPoint != "" {
		protected = append(protected, startPoint)
	}
	if err := g.AssertWritableBranch(branch, protected); err != nil {
		return err
	}
	return g.runGit(ctx, repoPath, nil, "branch", "--force", branch, startPoint)
}

func (g *Gateway) CreateWorktree(ctx context.Context, input CreateWorktreeInput) (storage.WorktreeRecord, error) {
	if err := g.AssertWritableBranch(input.Branch, append(append([]string{}, input.ProtectedBranches...), input.BaseBranch)); err != nil {
		return storage.WorktreeRecord{}, err
	}

	if err := os.MkdirAll(input.WorktreeRoot, 0o755); err != nil {
		return storage.WorktreeRecord{}, fmt.Errorf("create worktree root: %w", err)
	}

	worktreePath := filepath.Join(input.WorktreeRoot, buildWorktreeDirectoryName(input))
	if err := worktreesafety.Validate(worktreesafety.CheckInput{WorktreePath: worktreePath, RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot}); err != nil {
		return storage.WorktreeRecord{}, err
	}
	checkoutMode := normalizeCheckoutMode(input.CheckoutMode)

	restored, err := g.RestoreWorktree(ctx, RestoreWorktreeInput{
		ProjectID:            input.ProjectID,
		RepoPath:             input.RepoPath,
		Branch:               input.Branch,
		WorktreeRoot:         input.WorktreeRoot,
		CheckoutMode:         checkoutMode,
		ExpectedWorktreePath: worktreePath,
	})
	if err != nil {
		return storage.WorktreeRecord{}, err
	}
	if restored != nil {
		return *restored, nil
	}

	if checkoutMode == CheckoutModeDetached {
		startPoint, err := g.resolveDetachedStartPoint(ctx, input)
		if err != nil {
			return storage.WorktreeRecord{}, err
		}
		if err := g.runGit(ctx, input.RepoPath, nil, "worktree", "add", "--force", "--detach", worktreePath, startPoint); err != nil {
			return storage.WorktreeRecord{}, err
		}
	} else {
		branchExists, err := g.branchExists(ctx, input.RepoPath, input.Branch)
		if err != nil {
			return storage.WorktreeRecord{}, err
		}
		args := []string{"worktree", "add", "--force"}
		if branchExists {
			args = append(args, worktreePath, input.Branch)
		} else {
			startPoint, err := g.resolveAttachedStartPoint(ctx, input.RepoPath, input.Branch, input.BaseBranch)
			if err != nil {
				return storage.WorktreeRecord{}, err
			}
			args = append(args, "-b", input.Branch, worktreePath, startPoint)
		}
		if err := g.runGit(ctx, input.RepoPath, nil, args...); err != nil {
			return storage.WorktreeRecord{}, err
		}
	}

	// Keep common build artifacts (e.g. .pnpm-store/, node_modules/) out of every
	// loop commit via the worktree-local git exclude file. Best-effort.
	g.applyWorktreeArtifactExcludes(ctx, worktreePath)
	// Any CreateWorktree claim invalidates prior fixer ownership so path equality
	// alone cannot authorize dirty adopt after another runner used this directory.
	// Fail the claim if the marker cannot be revoked — stale authority is worse
	// than a retryable create error.
	if err := worktreesafety.ClearFixerOwnerToken(worktreePath); err != nil {
		return storage.WorktreeRecord{}, err
	}

	headSHA, err := g.getHeadSHA(ctx, worktreePath)
	if err != nil {
		return storage.WorktreeRecord{}, err
	}

	nowISO := g.now().UTC().Format(javaScriptISOStringLayout)
	var existingRecord *storage.WorktreeRecord
	if g.repos != nil {
		existingRecord, err = g.repos.Worktrees.GetByBranch(ctx, input.ProjectID, input.Branch)
		if err != nil {
			return storage.WorktreeRecord{}, fmt.Errorf("get existing worktree by branch: %w", err)
		}
	}

	baseBranch := input.BaseBranch
	record := storage.WorktreeRecord{
		ID:           valueOr(existingRecordID(existingRecord), mustRandomID()),
		ProjectID:    input.ProjectID,
		RepoPath:     input.RepoPath,
		WorktreePath: worktreePath,
		Branch:       input.Branch,
		BaseBranch:   stringPtr(baseBranch),
		Status:       "active",
		HeadSHA:      stringPtrIfNotEmpty(headSHA),
		MetadataJSON: stringPtr(`{"recovered":false}`),
		CreatedAt:    valueOr(existingRecordCreatedAt(existingRecord), nowISO),
		UpdatedAt:    nowISO,
		CleanedAt:    nil,
	}

	if g.repos != nil {
		if err := g.repos.Worktrees.Upsert(ctx, record); err != nil {
			return storage.WorktreeRecord{}, fmt.Errorf("upsert worktree record: %w", err)
		}
	}

	return record, nil

}

func (g *Gateway) ListWorktrees(ctx context.Context, repoPath string) ([]WorktreeListEntry, error) {
	result, err := g.runGitResult(ctx, repoPath, nil, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}

	return parseWorktreeList(result.Stdout), nil
}

func (g *Gateway) DetectGitHubRepo(ctx context.Context, repoPath string) (string, error) {
	remote, err := g.DetectOriginRemote(ctx, repoPath)
	if err != nil {
		return "", err
	}
	if !isGitHubRemoteHost(remote.Host) {
		return "", nil
	}
	return remote.Repo, nil
}

// OriginRemote is the parsed remote.origin URL for a local checkout.
type OriginRemote struct {
	URL  string
	Host string
	Repo string // owner/name
}

// DetectOriginRemote reads remote.origin.url and extracts host + owner/name when possible.
func (g *Gateway) DetectOriginRemote(ctx context.Context, repoPath string) (OriginRemote, error) {
	result, err := g.runGitResult(ctx, repoPath, nil, "config", "--get", "remote.origin.url")
	if err != nil {
		return OriginRemote{}, err
	}
	remoteURL := strings.TrimSpace(result.Stdout)
	host, repo := parseRemoteRepoFromURL(remoteURL)
	return OriginRemote{URL: remoteURL, Host: host, Repo: repo}, nil
}

func (g *Gateway) RestoreWorktree(ctx context.Context, input RestoreWorktreeInput) (*storage.WorktreeRecord, error) {
	checkoutMode := normalizeCheckoutMode(input.CheckoutMode)

	if g.repos != nil {
		stored, err := g.repos.Worktrees.GetByBranch(ctx, input.ProjectID, input.Branch)
		if err != nil {
			return nil, fmt.Errorf("get stored worktree by branch: %w", err)
		}
		if stored != nil && stored.Status != "cleaned" && normalizeComparablePath(stored.RepoPath) == normalizeComparablePath(input.RepoPath) && worktreesafety.IsSafe(worktreesafety.CheckInput{WorktreePath: stored.WorktreePath, RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot}) {
			storedHealthy, err := g.isHealthyWorktree(ctx, stored.WorktreePath)
			if err != nil {
				return nil, err
			}
			if !storedHealthy {
				g.tryRemoveWorktree(ctx, input.RepoPath, stored.WorktreePath)
			} else {
				storedCheckoutMatches, err := g.matchesRestoreCheckoutMode(ctx, stored.WorktreePath, checkoutMode, input.Branch)
				if err != nil {
					return nil, err
				}
				if storedCheckoutMatches {
					headSHA, err := g.getHeadSHA(ctx, stored.WorktreePath)
					if err != nil {
						return nil, err
					}
					nowISO := g.now().UTC().Format(javaScriptISOStringLayout)
					restored := *stored
					restored.HeadSHA = stringPtrIfNotEmpty(headSHA)
					restored.Status = "active"
					restored.UpdatedAt = nowISO
					if err := g.repos.Worktrees.Upsert(ctx, restored); err != nil {
						return nil, fmt.Errorf("upsert restored worktree record: %w", err)
					}
					g.applyWorktreeArtifactExcludes(ctx, restored.WorktreePath)
					// Restoring for a new CreateWorktree claim drops prior fixer ownership.
					if err := worktreesafety.ClearFixerOwnerToken(restored.WorktreePath); err != nil {
						return nil, err
					}
					return &restored, nil
				}
				shouldReplace, err := g.shouldReplaceStoredWorktreeOnRestoreMismatch(ctx, stored.WorktreePath, checkoutMode, input.Branch, input.ExpectedWorktreePath)
				if err != nil {
					return nil, err
				}
				if shouldReplace {
					g.tryRemoveWorktree(ctx, input.RepoPath, stored.WorktreePath)
				}
			}
		}
	}

	worktrees, err := g.ListWorktrees(ctx, input.RepoPath)
	if err != nil {
		return nil, err
	}

	var match *WorktreeListEntry
	for i := range worktrees {
		candidate := worktrees[i]
		if checkoutMode == CheckoutModeDetached {
			expectedPath := candidate.Path
			if input.ExpectedWorktreePath != "" {
				expectedPath = input.ExpectedWorktreePath
			}
			if normalizeComparablePath(candidate.Path) != normalizeComparablePath(expectedPath) {
				continue
			}
		} else if candidate.Branch != input.Branch {
			continue
		}
		if !worktreesafety.IsSafe(worktreesafety.CheckInput{WorktreePath: candidate.Path, RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot}) {
			continue
		}
		match = &candidate
		break
	}

	if match == nil {
		return nil, nil
	}

	healthy, err := g.isHealthyWorktree(ctx, match.Path)
	if err != nil {
		return nil, err
	}
	if !healthy {
		g.tryRemoveWorktree(ctx, input.RepoPath, match.Path)
		return nil, nil
	}

	checkoutMatches, err := g.matchesRestoreCheckoutMode(ctx, match.Path, checkoutMode, input.Branch)
	if err != nil {
		return nil, err
	}
	if !checkoutMatches {
		g.tryRemoveWorktree(ctx, input.RepoPath, match.Path)
		return nil, nil
	}

	nowISO := g.now().UTC().Format(javaScriptISOStringLayout)
	record := storage.WorktreeRecord{
		ID:           mustRandomID(),
		ProjectID:    input.ProjectID,
		RepoPath:     input.RepoPath,
		WorktreePath: match.Path,
		Branch:       input.Branch,
		BaseBranch:   stringPtr(input.Branch),
		Status:       "active",
		HeadSHA:      stringPtrIfNotEmpty(match.HeadSHA),
		MetadataJSON: stringPtr(`{"recovered":true}`),
		CreatedAt:    nowISO,
		UpdatedAt:    nowISO,
		CleanedAt:    nil,
	}
	if g.repos != nil {
		existing, err := g.repos.Worktrees.GetByBranch(ctx, input.ProjectID, input.Branch)
		if err != nil {
			return nil, fmt.Errorf("get existing restored worktree by branch: %w", err)
		}
		if existing != nil {
			record = *existing
		}
		record.WorktreePath = match.Path
		record.HeadSHA = stringPtrIfNotEmpty(match.HeadSHA)
		record.Status = "active"
		record.UpdatedAt = nowISO
		if record.RepoPath == "" {
			record.RepoPath = input.RepoPath
		}
		if record.ProjectID == "" {
			record.ProjectID = input.ProjectID
		}
		if record.Branch == "" {
			record.Branch = input.Branch
		}
		if err := g.repos.Worktrees.Upsert(ctx, record); err != nil {
			return nil, fmt.Errorf("upsert discovered worktree record: %w", err)
		}
	}

	g.applyWorktreeArtifactExcludes(ctx, record.WorktreePath)
	if err := worktreesafety.ClearFixerOwnerToken(record.WorktreePath); err != nil {
		return nil, err
	}
	return &record, nil
}

func (g *Gateway) CleanupWorktree(ctx context.Context, input CleanupWorktreeInput) error {
	// Refuse before any filesystem mutation when the cleanup context was
	// canceled (MarkDegraded/BeginShutdown cancel under admission.mu).
	// When AdmitStart is set, Start is rechecked under that gate; this is the
	// pre-validation fast path only.
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := g.AssertWritableBranch(input.Branch, input.ProtectedBranches); err != nil {
		return err
	}
	if err := worktreesafety.Validate(worktreesafety.CheckInput{WorktreePath: input.WorktreePath, RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot}); err != nil {
		return err
	}
	// Capture this lifecycle's quarantine generation before git worktree remove
	// deletes the private git dir that holds the marker. Terminal cleanup must
	// delete only this generation — not prior abandoned generations that share
	// the same worktree basename under the retention window.
	generation, _ := readReservedReviewScratchGeneration(input.WorktreePath)
	if err := g.runGitWithStartGate(ctx, input.RepoPath, nil, input.AdmitStart, "worktree", "remove", "--force", input.WorktreePath); err != nil {
		if !missingWorktreeErrorPattern.MatchString(err.Error()) {
			return err
		}
		// The missing-worktree matcher is intentionally broad (Git phrasing
		// varies) and can also match git-start / CWD failures that contain
		// "not found" or "no such file" even when remove never ran. Only treat
		// the worktree as already gone when the path is independently absent.
		absent, confirmErr := worktreePathAbsent(input.WorktreePath)
		if confirmErr != nil {
			return fmt.Errorf("confirm worktree absence after remove: %w", confirmErr)
		}
		if !absent {
			return err
		}
	}

	// Authorized terminal deletion of this worktree's reserved-scratch
	// quarantine (see reservedReviewerScratchAuthority). Recovery holds only
	// while the worktree lifecycle is active; once remove succeeded (or the
	// path was independently confirmed missing), the quarantine may go.
	if err := removeReservedReviewScratchQuarantineForWorktree(input.WorktreePath, generation); err != nil {
		return err
	}

	if g.repos == nil {
		return nil
	}

	existing, err := g.repos.Worktrees.GetByBranch(ctx, input.ProjectID, input.Branch)
	if err != nil {
		return fmt.Errorf("get worktree before cleanup update: %w", err)
	}
	if existing == nil {
		return nil
	}

	nowISO := g.now().UTC().Format(javaScriptISOStringLayout)
	updated := *existing
	updated.Status = "cleaned"
	updated.CleanedAt = stringPtr(nowISO)
	updated.UpdatedAt = nowISO
	if err := g.repos.Worktrees.Upsert(ctx, updated); err != nil {
		return fmt.Errorf("update cleaned worktree record: %w", err)
	}

	return nil
}

func (g *Gateway) WorktreeClean(ctx context.Context, worktreePath string) (bool, error) {
	entries, err := g.readStatus(ctx, worktreePath)
	if err != nil {
		return false, err
	}
	return len(entries) == 0, nil
}

func (g *Gateway) IsWorktreeClean(ctx context.Context, worktreePath string) (bool, error) {
	return g.WorktreeClean(ctx, worktreePath)
}

// DiscardWorktreeChanges hard-resets tracked files and removes untracked files
// in a managed worktree. It never deletes the worktree directory, remote
// branches, or force-pushes.
func (g *Gateway) DiscardWorktreeChanges(ctx context.Context, input DiscardWorktreeChangesInput) (DiscardWorktreeChangesResult, error) {
	worktreePath := strings.TrimSpace(input.WorktreePath)
	if worktreePath == "" {
		return DiscardWorktreeChangesResult{}, fmt.Errorf("worktree path is required")
	}
	if err := g.validateMutationWorktree(worktreePath, input.RepoPath, input.WorktreeRoot); err != nil {
		return DiscardWorktreeChangesResult{}, err
	}
	if _, err := os.Stat(worktreePath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return DiscardWorktreeChangesResult{WorktreePath: worktreePath, NoOp: true}, nil
		}
		return DiscardWorktreeChangesResult{}, err
	}

	clean, err := g.WorktreeClean(ctx, worktreePath)
	if err != nil {
		return DiscardWorktreeChangesResult{}, err
	}
	if clean {
		return DiscardWorktreeChangesResult{WorktreePath: worktreePath, WasDirty: false, NoOp: true}, nil
	}

	// Recurse into submodules so a dirty tracked submodule is reset with the
	// superproject. Without --recurse-submodules, top-level tracked edits are
	// discarded while submodule dirt remains, and the post-clean check fails
	// after partial discard (not all-or-nothing for repos with submodules).
	if err := g.runGit(ctx, worktreePath, nil, "reset", "--hard", "--recurse-submodules", "HEAD"); err != nil {
		return DiscardWorktreeChangesResult{}, err
	}
	// Double -f is required so git clean also removes nested repositories
	// (untracked checkouts with their own .git). Single -f leaves those
	// behind, which fails the post-clean cleanliness check after tracked
	// edits were already discarded via reset --hard.
	if err := g.runGit(ctx, worktreePath, nil, "clean", "-ffd"); err != nil {
		return DiscardWorktreeChangesResult{}, err
	}
	// Top-level clean does not enter tracked submodules. Reset+clean each
	// submodule so untracked/modified files inside them are discarded too.
	// No-op when the worktree has no submodules.
	if err := g.runGit(ctx, worktreePath, nil, "submodule", "foreach", "--recursive", "git reset --hard && git clean -ffd"); err != nil {
		return DiscardWorktreeChangesResult{}, err
	}

	clean, err = g.WorktreeClean(ctx, worktreePath)
	if err != nil {
		return DiscardWorktreeChangesResult{}, err
	}
	if !clean {
		return DiscardWorktreeChangesResult{}, fmt.Errorf("worktree still dirty after discard at %s", worktreePath)
	}
	return DiscardWorktreeChangesResult{WorktreePath: worktreePath, WasDirty: true, NoOp: false}, nil
}

func (g *Gateway) Push(ctx context.Context, input PushInput) error {
	if err := g.AssertWritableBranch(input.Branch, input.ProtectedBranches); err != nil {
		return err
	}
	if err := g.validateMutationWorktree(input.WorktreePath, input.RepoPath, input.WorktreeRoot); err != nil {
		return err
	}
	remote := input.Remote
	if strings.TrimSpace(remote) == "" {
		remote = "origin"
	}

	if input.ExpectedRemoteHeadSHA != "" {
		isExpectedAncestor, err := g.isAncestor(ctx, input.WorktreePath, input.ExpectedRemoteHeadSHA, "HEAD")
		if err != nil {
			return err
		}
		if !isExpectedAncestor {
			return fmt.Errorf("Refusing fixer push for %s because local HEAD no longer descends from %s", input.Branch, input.ExpectedRemoteHeadSHA)
		}

		err = g.runGit(ctx, input.WorktreePath, nil,
			"push",
			"--porcelain",
			fmt.Sprintf("--force-with-lease=refs/heads/%s:%s", input.Branch, input.ExpectedRemoteHeadSHA),
			"-u",
			remote,
			fmt.Sprintf("HEAD:refs/heads/%s", input.Branch),
		)
		if err != nil {
			if pushConflictErrorPattern.MatchString(err.Error()) {
				actualHeadSHA, lookupErr := g.getRemoteHeadSHA(ctx, input.WorktreePath, remote, input.Branch)
				if lookupErr != nil {
					return lookupErr
				}
				return &RemoteHeadChangedError{Branch: input.Branch, ExpectedHeadSHA: input.ExpectedRemoteHeadSHA, ActualHeadSHA: actualHeadSHA}
			}
			return err
		}
		return nil
	}

	return g.runGit(ctx, input.WorktreePath, nil, "push", "-u", remote, fmt.Sprintf("HEAD:refs/heads/%s", input.Branch))
}

func (g *Gateway) PrepareWorktree(ctx context.Context, input PrepareWorktreeInput) (PrepareWorktreeResult, error) {
	if err := g.validateMutationWorktree(input.WorktreePath, input.RepoPath, input.WorktreeRoot); err != nil {
		return PrepareWorktreeResult{}, err
	}
	remote := input.Remote
	if strings.TrimSpace(remote) == "" {
		remote = "origin"
	}
	targetSpec := strings.TrimSpace(input.Ref)
	resetRef := "FETCH_HEAD"
	errorRef := targetSpec
	if targetSpec == "" {
		targetSpec = strings.TrimSpace(input.Branch)
		resetRef = remote + "/" + targetSpec
		errorRef = targetSpec
	}
	if targetSpec == "" {
		return PrepareWorktreeResult{}, fmt.Errorf("branch or ref is required")
	}
	if err := g.runGit(ctx, input.WorktreePath, nil, "fetch", remote, targetSpec); err != nil {
		return PrepareWorktreeResult{}, err
	}

	remoteHeadSHA, err := g.getRevision(ctx, input.WorktreePath, resetRef)
	if err != nil {
		return PrepareWorktreeResult{}, err
	}
	if input.ExpectedHeadSHA != "" && remoteHeadSHA != input.ExpectedHeadSHA {
		return PrepareWorktreeResult{}, &RemoteHeadChangedError{Branch: errorRef, ExpectedHeadSHA: input.ExpectedHeadSHA, ActualHeadSHA: remoteHeadSHA}
	}

	// Reconcile managed excludes before cleanliness so upgraded daemons heal
	// existing worktrees (reserved reviewer scratch, build artifacts) without MI.
	g.applyWorktreeArtifactExcludes(ctx, input.WorktreePath)
	// A prior agent may have staged reserved scratch under a .gitignore
	// negation (git add -A). Status would report "A " (not "??"), so the
	// untracked-only classifier alone leaves retry in MI. Unstage additions
	// without deleting files so readStatus can ignore them as reserved dirt.
	if err := g.unstageReservedReviewerScratchAdditions(ctx, input.WorktreePath); err != nil {
		return PrepareWorktreeResult{}, err
	}

	statusBeforeReset, err := g.readStatus(ctx, input.WorktreePath)
	if err != nil {
		// Truncated status fails closed here; leave on-disk payloads untouched so
		// operators can inspect incomplete classification (never delete).
		return PrepareWorktreeResult{}, err
	}
	// After a complete dirt classification, move reserved scratch outside the
	// worktree. Reporting clean while leaving the payload in place lets a later
	// fixer agent run raw `git add -A && git commit` (bypassing Gateway.Commit)
	// and publish leftover review JSON. Relocate preserves bytes (rename, never
	// delete) and runs even when ordinary dirt forces Clean=false so the next
	// prepare after dirt is cleared cannot re-expose the same payload.
	if err := g.relocateReservedReviewerScratch(ctx, input.WorktreePath); err != nil {
		return PrepareWorktreeResult{}, err
	}
	if len(statusBeforeReset) > 0 {
		return PrepareWorktreeResult{HeadSHA: remoteHeadSHA, Clean: false}, nil
	}

	localHeadSHA, err := g.getHeadSHA(ctx, input.WorktreePath)
	if err != nil {
		return PrepareWorktreeResult{}, err
	}
	if remoteHeadSHA != "" && localHeadSHA != remoteHeadSHA {
		if err := g.runGit(ctx, input.WorktreePath, nil, "reset", "--hard", resetRef); err != nil {
			return PrepareWorktreeResult{}, err
		}
	}

	statusAfterReset, err := g.readStatus(ctx, input.WorktreePath)
	if err != nil {
		return PrepareWorktreeResult{}, err
	}
	// Untracked files survive reset --hard; relocate again in case any reserved
	// scratch reappeared or was missed before the reset path.
	if err := g.relocateReservedReviewerScratch(ctx, input.WorktreePath); err != nil {
		return PrepareWorktreeResult{}, err
	}
	headSHA, err := g.getHeadSHA(ctx, input.WorktreePath)
	if err != nil {
		return PrepareWorktreeResult{}, err
	}

	return PrepareWorktreeResult{HeadSHA: headSHA, Clean: len(statusAfterReset) == 0}, nil
}

func (g *Gateway) InspectHead(ctx context.Context, input InspectHeadInput) (InspectHeadResult, error) {
	if err := g.validateMutationWorktree(input.WorktreePath, input.RepoPath, input.WorktreeRoot); err != nil {
		return InspectHeadResult{}, err
	}
	headSHA, err := g.getHeadSHA(ctx, input.WorktreePath)
	if err != nil {
		return InspectHeadResult{}, err
	}

	newCommitSHAs := []string{}
	if input.BaseRef != "" {
		newCommitSHAs, err = g.listCommitsSince(ctx, input.WorktreePath, input.BaseRef)
		if err != nil {
			return InspectHeadResult{}, err
		}
	}

	status, err := g.readStatus(ctx, input.WorktreePath)
	if err != nil {
		return InspectHeadResult{}, err
	}

	changedFiles := make([]string, 0, len(status))
	for _, entry := range status {
		changedFiles = append(changedFiles, entry.Path)
	}

	return InspectHeadResult{
		HeadSHA:               headSHA,
		NewCommitSHAs:         newCommitSHAs,
		HasUncommittedChanges: len(status) > 0,
		ChangedFiles:          changedFiles,
	}, nil
}

func (g *Gateway) Commit(ctx context.Context, input CommitInput) (CommitResult, error) {
	if err := g.validateMutationWorktree(input.WorktreePath, input.RepoPath, input.WorktreeRoot); err != nil {
		return CommitResult{}, err
	}
	// Stage normally so tracked paths that happen to match the reserved name
	// (e.g. a fixture) still commit. info/exclude alone loses to higher-
	// precedence .gitignore negations, so after add we unstage only newly-added
	// root reserved scratch (including any pre-staged by the agent).
	if err := g.runGit(ctx, input.WorktreePath, nil, "add", "-A"); err != nil {
		return CommitResult{}, err
	}
	if err := g.unstageReservedReviewerScratchAdditions(ctx, input.WorktreePath); err != nil {
		return CommitResult{}, err
	}
	if err := g.runGit(ctx, input.WorktreePath, nil, "commit", "-m", input.Message); err != nil {
		return CommitResult{}, err
	}

	headSHA, err := g.getHeadSHA(ctx, input.WorktreePath)
	if err != nil {
		return CommitResult{}, err
	}
	if headSHA == "" {
		return CommitResult{}, fmt.Errorf("commitSHA is required")
	}

	return CommitResult{CommitSHA: headSHA}, nil
}

// unstageReservedReviewerScratchAdditions drops index entries for root-level
// reserved reviewer scratch that are newly added (not present in HEAD). That
// covers untracked and already-staged ephemeral submit payloads without
// blocking legitimate modifications or deletions of tracked files that share
// the same basename pattern.
//
// Dual classification (with isIgnorableReservedReviewerScratch in readStatus):
// PrepareWorktree and Commit both call this unstage first so prestaged reserved
// paths become untracked; status then drops only "??" / intent-to-add root
// reserved names; Commit still unstages after git add -A so scratch never
// lands via Gateway.Commit. PrepareWorktree additionally relocates untracked
// reserved payloads outside the worktree so agent-authored `git add -A &&
// git commit` (which bypasses Gateway.Commit) cannot publish leftovers after
// a clean prepare. Both classifiers use isReservedReviewerScratchPath so Git
// glob, empty suffix, root-only, and core.ignoreCase rules stay identical.
// Costs: keep Prepare/Commit unstage + status filter + prepare relocate in
// sync; namespace collisions if a tracked root file uses the reserved pattern
// (mitigated by unstaging only --diff-filter=A additions, not M/D, and by
// never relocating index-tracked paths); Git ignore precedence where tracked
// .gitignore !/.looper-review-*.json outranks info/exclude (exclude alone is
// insufficient — that is why classifiers exist); shell capture is still
// bounded (256 KiB), so this query is pathspec-scoped to the reserved root
// namespace — ordinary large commits never fail the gate; only a truncated
// reserved-namespace listing fails closed (pathologically many simultaneous
// scratch files). Simpler alternatives rejected: exclude-only (broken under
// negation); delete-on-sight during prepare (destroys mid-flight recovery;
// terminal/orphan deletion is separately authorized — see
// reservedReviewerScratchAuthority); leave-in-place after clean prepare
// (agent-authored commits publish scratch); blanket pathspec exclude on commit
// (blocks tracked same-name fixtures); unscoped full-addition listing
// (rejects unrelated large worker/planner/fixer commits).
//
// --no-renames is required: when a tracked file is deleted and an identical
// reserved scratch is staged, default rename detection reports R100 so
// --diff-filter=A would miss the destination. --ita-visible-in-index is
// required so intent-to-add (git add -N) entries appear in the cached addition
// listing; without it, ITA stays as porcelain " A" and the ??-only status
// filter cannot clear PrepareWorktree. -z avoids core.quotePath escaping of
// non-ASCII reserved basenames. The listing pathspec uses
// :(glob).looper-review-*.json (with ,icase when core.ignoreCase is true) so
// only reserved-root candidates are captured (* does not cross /). Paths are
// then passed as :(literal) pathspecs so basenames with \, *, ?, [, ] are
// unstaged as themselves rather than as glob/escape metacharacters
// (git reset -- <pathspec>...).
func (g *Gateway) unstageReservedReviewerScratchAdditions(ctx context.Context, worktreePath string) error {
	ignoreCase := g.coreIgnoreCase(ctx, worktreePath)
	// Scope capture to the reserved root namespace so shell's 256 KiB stdout
	// bound cannot reject ordinary large commits that never touch scratch.
	// Honor core.ignoreCase: Git's installed exclude pattern folds case when
	// the option is true; a case-sensitive pathspec would miss uppercase
	// reserved names under .gitignore negation and let Commit land them.
	pathspec := ":(glob)" + reservedReviewerScratchPrefix + "*" + reservedReviewerScratchSuffix
	if ignoreCase {
		pathspec = ":(glob,icase)" + reservedReviewerScratchPrefix + "*" + reservedReviewerScratchSuffix
	}
	result, err := g.runGitResult(ctx, worktreePath, nil,
		"diff", "--cached", "--name-only", "--diff-filter=A", "--no-renames",
		"--ita-visible-in-index", "-z",
		"--", pathspec,
	)
	if err != nil {
		return err
	}
	// Fail closed only for the reserved-namespace listing: a truncated capture
	// may omit scratch beyond the prefix and would otherwise let Commit land it.
	if result.StdoutTruncated {
		return fmt.Errorf("reserved reviewer scratch addition list truncated after %d bytes; refuse incomplete classification", len(result.Stdout))
	}
	paths := make([]string, 0)
	for _, path := range splitNUL(result.Stdout) {
		// Defense in depth: pathspec * is root-only (* does not cross /), but
		// keep the shared classifier so empty-middle / suffix / case rules stay one place.
		if isReservedReviewerScratchPath(path, ignoreCase) {
			// git reset treats args as pathspecs; force literal matching so
			// reserved basenames with \ * ? [ ] cannot escape or expand.
			paths = append(paths, ":(literal)"+path)
		}
	}
	if len(paths) == 0 {
		return nil
	}
	args := append([]string{"reset", "HEAD", "--"}, paths...)
	return g.runGit(ctx, worktreePath, nil, args...)
}

// reservedReviewerScratchAuthority is the managed-reviewer contract for
// top-level /.looper-review-*.json ephemeral submit scratch.
//
// Authority (what Looper may do to this namespace):
//  1. Ignore: clone-local info/exclude and dual status/index classifiers may
//     treat untracked reserved root names as non-dirt so Prepare stays clean
//     and Gateway.Commit never lands the payload.
//  2. Relocate during prepare: untracked reserved root payloads may be moved
//     to a sibling quarantine under the worktree parent. Prepare never
//     deletes the payload bytes (rename/copy-then-remove-source only after the
//     destination is durable). Tracked same-name fixtures are never relocated.
//  3. Terminal quarantine deletion: after CleanupWorktree successfully removes
//     the worktree (or independently confirms the path is already absent),
//     removeReservedReviewScratchQuarantineForWorktree may delete that
//     worktree lifecycle's generation-keyed quarantine subdirectory. Operator
//     recovery is mid-flight only — while the worktree lifecycle is still
//     active. Prior generations that share the worktree basename stay until
//     orphan retention (rule 4).
//  4. Orphan retention deletion: pruneExpiredReservedReviewScratchQuarantine
//     may delete quarantine subdirs that are not the active generation for a
//     still-present worktree path (or whose worktree path is gone) and whose
//     container age exceeds reservedReviewScratchQuarantineRetention. Active
//     worktrees are never pruned by payload mtime (rename preserves source mtime).
//
// What authority still forbids: delete-on-sight during prepare; treating
// arbitrary non-reserved dirt as clean; deleting quarantine while the matching
// worktree path still exists; following symlinked quarantine roots on delete.
//
// Costs: sibling quarantine layout, per-lifecycle generation marker, cleanup
// coupling, orphan-prune edge cases (clock skew, operators wanting copies
// longer than retention).
// Simpler alternatives rejected: leave-in-place after clean prepare (agent
// git add -A && commit publishes leftovers); delete on prepare (destroys
// recovery); unbounded quarantine (disk growth); mtime-only prune of active
// quarantine (races double-relocate); path-only quarantine keys (recreated
// worktree paths steal prior generation retention).
const reservedReviewerScratchAuthority = "reserved-reviewer-scratch-v1"

// reservedReviewScratchQuarantineDirName is a sibling directory under the
// worktree parent (typically the managed worktree root) that holds payloads
// relocated out of a prepared worktree. It is never a git worktree path.
// Deletion of contents is authorized only under reservedReviewerScratchAuthority
// rules 3 (terminal CleanupWorktree) and 4 (orphan retention), never during
// prepare relocate (rule 2). Must be a real directory (Lstat); symlinked roots
// are refused for create and delete so RemoveAll cannot escape quarantine.
const reservedReviewScratchQuarantineDirName = ".looper-reserved-review-scratch"

// reservedReviewScratchQuarantineRetention bounds growth for orphaned quarantine
// entries (abandoned worktrees whose path is gone, or prior generations under a
// recreated path). Active worktree quarantine is retained until CleanupWorktree,
// regardless of payload mtime. Deletion of expired orphans is authorized by
// reservedReviewerScratchAuthority rule 4.
const reservedReviewScratchQuarantineRetention = 7 * 24 * time.Hour

// reservedReviewScratchGenerationFile is stored under the worktree-private git
// dir so each CreateWorktree lifecycle gets a distinct quarantine key without
// appearing as worktree dirt.
const reservedReviewScratchGenerationFile = "looper-reserved-scratch-generation"

// reservedReviewScratchQuarantineGenSep joins worktree basename and generation
// in quarantine entry names: {basename}--gen-{16hex}.
const reservedReviewScratchQuarantineGenSep = "--gen-"

// ReservedReviewScratchQuarantineDir returns the operator-visible directory that
// holds relocated reserved reviewer payloads for the active lifecycle of
// worktreePath. Empty when the worktree path is blank or no generation marker
// is available yet (pre-relocate). After ensure/relocate, the path is
// generation-keyed under reservedReviewScratchQuarantineDirName.
func ReservedReviewScratchQuarantineDir(worktreePath string) string {
	worktreePath = strings.TrimSpace(worktreePath)
	if worktreePath == "" {
		return ""
	}
	gen, err := readReservedReviewScratchGeneration(worktreePath)
	if err != nil || gen == "" {
		// Pre-generation or unreadable marker: expose the legacy path-only key
		// so operators can still find pre-upgrade quarantines.
		return filepath.Join(filepath.Dir(worktreePath), reservedReviewScratchQuarantineDirName, filepath.Base(worktreePath))
	}
	return quarantineDirForGeneration(worktreePath, gen)
}

// relocateReservedReviewerScratch moves untracked root-level reserved reviewer
// scratch files out of the worktree under reservedReviewerScratchAuthority
// rule 2: prepare may ignore and relocate this namespace, and never deletes
// payload bytes mid-flight (rename or durable copy then remove source). Tracked
// same-name fixtures (present in the index/HEAD) are left untouched.
//
// Opportunistic orphan prune (rule 4) runs first and is independent of this
// worktree's relocate. Call only after a complete readStatus classification so
// truncated large listings still fail closed with files left in place.
func (g *Gateway) relocateReservedReviewerScratch(ctx context.Context, worktreePath string) error {
	if strings.TrimSpace(worktreePath) == "" {
		return nil
	}
	// Authorized orphan retention deletion (rule 4); best-effort so prune
	// failures must not block prepare relocation of the current worktree.
	g.pruneExpiredReservedReviewScratchQuarantine(filepath.Dir(worktreePath))

	ignoreCase := g.coreIgnoreCase(ctx, worktreePath)
	entries, err := os.ReadDir(worktreePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("list worktree for reserved scratch relocate: %w", err)
	}
	generation, err := ensureReservedReviewScratchGeneration(worktreePath)
	if err != nil {
		return fmt.Errorf("ensure reserved scratch quarantine generation: %w", err)
	}
	if err := migrateLegacyReservedReviewScratchQuarantine(worktreePath, generation); err != nil {
		return err
	}
	quarantineRoot := quarantineDirForGeneration(worktreePath, generation)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !isReservedReviewerScratchPath(name, ignoreCase) {
			continue
		}
		// Never relocate tracked fixtures or other index-present paths that
		// share the reserved basename pattern. Operational probe failures
		// (cancel, corrupt index, git start) must fail before any rename so a
		// tracked matching file is never treated as scratch. When
		// core.ignoreCase is true, match the on-disk spelling case-insensitively
		// against the index so a case-only spelling drift is not quarantined.
		present, err := g.isIndexPathPresent(ctx, worktreePath, name, ignoreCase)
		if err != nil {
			return fmt.Errorf("probe index for reserved scratch %q: %w", name, err)
		}
		if present {
			continue
		}
		src := filepath.Join(worktreePath, name)
		if err := ensureRealQuarantineTree(quarantineRoot); err != nil {
			return fmt.Errorf("create reserved scratch quarantine: %w", err)
		}
		dest, err := reserveQuarantineDestination(quarantineRoot, name, g.now)
		if err != nil {
			return fmt.Errorf("reserve reserved scratch quarantine dest for %q: %w", name, err)
		}
		if err := os.Rename(src, dest); err != nil {
			// Cross-device fallback: write bytes onto the reserved destination,
			// then remove source only after the destination is durable.
			if copyErr := copyFileOnto(src, dest); copyErr != nil {
				_ = os.Remove(dest) // free empty/partial reservation; keep src
				return fmt.Errorf("relocate reserved scratch %q: rename: %v; copy: %w", name, err, copyErr)
			}
			if rmErr := os.Remove(src); rmErr != nil {
				// Destination holds the payload; surface source cleanup failure.
				return fmt.Errorf("relocate reserved scratch %q: remove source after copy: %w", name, rmErr)
			}
		}
	}
	return nil
}

// isIndexPathPresent reports whether path is known to the index (tracked or
// staged). Used to avoid relocating tracked reserved-name fixtures.
//
// git ls-files --error-unmatch exits 1 when the path is absent from the index;
// that expected miss is returned as (false, nil). Context cancel, corrupt
// index, and other operational failures are propagated so callers do not
// mutate the filesystem under an unknown index state.
//
// When ignoreCase is true (core.ignoreCase), the pathspec uses icase so an
// on-disk reserved basename that differs from the index only by case still
// counts as tracked and is never relocated as scratch.
func (g *Gateway) isIndexPathPresent(ctx context.Context, worktreePath, path string, ignoreCase bool) (bool, error) {
	pathspec := ":(literal)" + path
	if ignoreCase {
		pathspec = ":(icase,literal)" + path
	}
	err := g.runGit(ctx, worktreePath, nil, "ls-files", "--error-unmatch", "-z", "--", pathspec)
	if err == nil {
		return true, nil
	}
	var commandErr *shell.CommandExecutionError
	if errors.As(err, &commandErr) && commandErr.Result.ExitCode == 1 {
		return false, nil
	}
	return false, err
}

// reserveQuarantineDestination exclusively creates an empty destination file so
// concurrent/retried prepares cannot clobber a prior payload via os.Rename
// replace semantics on Unix.
//
// Layout is a short random subdirectory containing the original basename:
//
//	{quarantineRoot}/{16-hex}/{basename}
//
// Hex-encoding the full basename would double a near-NAME_MAX scratch name past
// the filesystem component limit (~255) and break Prepare with ENAMETOOLONG.
// Keeping the original basename under a bounded random dir stays within NAME_MAX
// whenever the source name was valid in the worktree.
//
// now is accepted for call-site stability; destination uniqueness is random-only.
func reserveQuarantineDestination(quarantineRoot, basename string, now func() time.Time) (string, error) {
	_ = now
	base := filepath.Base(basename)
	if base == "" || base == "." || base == ".." {
		return "", fmt.Errorf("invalid quarantine basename %q", basename)
	}
	var lastErr error
	for attempt := 0; attempt < 16; attempt++ {
		var randBytes [8]byte
		if _, err := rand.Read(randBytes[:]); err != nil {
			return "", fmt.Errorf("random quarantine suffix: %w", err)
		}
		destDir := filepath.Join(quarantineRoot, hex.EncodeToString(randBytes[:]))
		if err := os.Mkdir(destDir, 0o700); err != nil {
			lastErr = err
			if os.IsExist(err) {
				continue
			}
			return "", err
		}
		dest := filepath.Join(destDir, base)
		f, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			_ = os.Remove(destDir)
			lastErr = err
			if os.IsExist(err) {
				continue
			}
			return "", err
		}
		if err := f.Close(); err != nil {
			_ = os.Remove(dest)
			_ = os.Remove(destDir)
			return "", err
		}
		return dest, nil
	}
	return "", fmt.Errorf("exclusive quarantine dest after retries: %w", lastErr)
}

func copyFileOnto(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	// dest is the exclusively reserved empty file from reserveQuarantineDestination.
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// removeReservedReviewScratchQuarantineForWorktree deletes one worktree
// lifecycle's generation-keyed quarantine subdirectory under
// reservedReviewerScratchAuthority rule 3. generation must be captured before
// worktree remove destroys the private git-dir marker; empty generation falls
// back to the legacy path-only key for pre-upgrade quarantines. Call only after
// CleanupWorktree has removed the worktree or independently confirmed the path
// is already absent — never while the worktree still exists.
//
// Refuse when the quarantine root or target entry is a symlink (Lstat): RemoveAll
// resolves intermediate symlinks and would otherwise delete outside quarantine.
func removeReservedReviewScratchQuarantineForWorktree(worktreePath, generation string) error {
	worktreePath = strings.TrimSpace(worktreePath)
	if worktreePath == "" {
		return nil
	}
	var dir string
	if generation != "" {
		dir = quarantineDirForGeneration(worktreePath, generation)
	} else {
		dir = filepath.Join(filepath.Dir(worktreePath), reservedReviewScratchQuarantineDirName, filepath.Base(worktreePath))
	}
	if err := removeQuarantineDirNoFollow(dir); err != nil {
		return fmt.Errorf("remove reserved scratch quarantine for %q: %w", filepath.Base(worktreePath), err)
	}
	// Best-effort: drop empty parent quarantine root when it is a real directory.
	parent := filepath.Dir(dir)
	if err := requireRealDirNoFollow(parent); err != nil {
		return nil
	}
	if entries, err := os.ReadDir(parent); err == nil && len(entries) == 0 {
		_ = os.Remove(parent)
	}
	return nil
}

// pruneExpiredReservedReviewScratchQuarantine deletes orphan quarantine
// subdirectories older than reservedReviewScratchQuarantineRetention under
// worktreeRoot's sibling quarantine tree (reservedReviewerScratchAuthority
// rule 4). A quarantine entry is orphan when it is not the active generation
// for a still-present worktree (including prior generations under a recreated
// path, and path-only legacy keys after a generation marker exists), or when
// the worktree path is gone — never while it is the active lifecycle, regardless
// of payload mtime. Best-effort; bounds growth for abandoned worktrees.
// Symlinked quarantine roots are skipped (never followed for deletion).
func (g *Gateway) pruneExpiredReservedReviewScratchQuarantine(worktreeRoot string) {
	worktreeRoot = strings.TrimSpace(worktreeRoot)
	if worktreeRoot == "" {
		return
	}
	root := filepath.Join(worktreeRoot, reservedReviewScratchQuarantineDirName)
	if err := requireRealDirNoFollow(root); err != nil {
		return
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	cutoff := g.now().Add(-reservedReviewScratchQuarantineRetention)
	for _, entry := range entries {
		name := entry.Name()
		qPath := filepath.Join(root, name)
		if err := requireRealDirNoFollow(qPath); err != nil {
			// Symlink or non-dir entry: never RemoveAll through it.
			continue
		}
		if quarantineEntryIsActive(worktreeRoot, name) {
			continue
		}
		if !orphanQuarantineExpired(qPath, cutoff) {
			continue
		}
		_ = removeQuarantineDirNoFollow(qPath)
	}
	// Best-effort: drop empty quarantine root.
	if remaining, readErr := os.ReadDir(root); readErr == nil && len(remaining) == 0 {
		_ = os.Remove(root)
	}
}

// quarantineDirForGeneration returns the generation-keyed quarantine path for
// worktreePath. generation must be a valid marker value.
func quarantineDirForGeneration(worktreePath, generation string) string {
	return filepath.Join(
		filepath.Dir(worktreePath),
		reservedReviewScratchQuarantineDirName,
		quarantineEntryName(filepath.Base(worktreePath), generation),
	)
}

func quarantineEntryName(worktreeBase, generation string) string {
	return worktreeBase + reservedReviewScratchQuarantineGenSep + generation
}

// parseQuarantineEntryName splits {basename}--gen-{16hex}. Legacy path-only
// entries return ok=false with basename set to the whole entry name.
func parseQuarantineEntryName(entry string) (basename, generation string, ok bool) {
	idx := strings.LastIndex(entry, reservedReviewScratchQuarantineGenSep)
	if idx <= 0 {
		return entry, "", false
	}
	basename = entry[:idx]
	generation = entry[idx+len(reservedReviewScratchQuarantineGenSep):]
	if basename == "" || !isValidReservedReviewScratchGeneration(generation) {
		return entry, "", false
	}
	return basename, generation, true
}

func isValidReservedReviewScratchGeneration(generation string) bool {
	if len(generation) != 16 {
		return false
	}
	for i := 0; i < len(generation); i++ {
		c := generation[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// ensureReservedReviewScratchGeneration returns the lifecycle generation for
// worktreePath, creating a new 16-hex marker in the private git dir when absent.
func ensureReservedReviewScratchGeneration(worktreePath string) (string, error) {
	if gen, err := readReservedReviewScratchGeneration(worktreePath); err != nil {
		return "", err
	} else if gen != "" {
		return gen, nil
	}
	gitDir, err := worktreesafety.ResolveWorktreePrivateGitDir(worktreePath, true)
	if err != nil {
		return "", err
	}
	var randBytes [8]byte
	if _, err := rand.Read(randBytes[:]); err != nil {
		return "", fmt.Errorf("random quarantine generation: %w", err)
	}
	gen := hex.EncodeToString(randBytes[:])
	path := filepath.Join(gitDir, reservedReviewScratchGenerationFile)
	if err := os.WriteFile(path, []byte(gen+"\n"), 0o644); err != nil {
		return "", err
	}
	return gen, nil
}

// readReservedReviewScratchGeneration returns the generation marker when present.
// Empty string with nil error means absent/unreadable-as-missing.
func readReservedReviewScratchGeneration(worktreePath string) (string, error) {
	worktreePath = strings.TrimSpace(worktreePath)
	if worktreePath == "" {
		return "", nil
	}
	gitDir, err := worktreesafety.ResolveWorktreePrivateGitDir(worktreePath, false)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	data, err := os.ReadFile(filepath.Join(gitDir, reservedReviewScratchGenerationFile))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	gen := strings.TrimSpace(string(data))
	if !isValidReservedReviewScratchGeneration(gen) {
		return "", nil
	}
	return gen, nil
}

// migrateLegacyReservedReviewScratchQuarantine renames a pre-generation
// path-only quarantine dir for this worktree basename into the generation-keyed
// layout when the destination does not yet exist. Same-lifecycle upgrade only;
// if both exist, the legacy dir is left for orphan retention (prior lifecycle).
func migrateLegacyReservedReviewScratchQuarantine(worktreePath, generation string) error {
	legacy := filepath.Join(filepath.Dir(worktreePath), reservedReviewScratchQuarantineDirName, filepath.Base(worktreePath))
	if err := requireRealDirNoFollow(legacy); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		// Symlink/non-dir at the legacy key: refuse rather than follow.
		return fmt.Errorf("legacy reserved scratch quarantine unsafe at %q: %w", legacy, err)
	}
	dest := quarantineDirForGeneration(worktreePath, generation)
	if _, err := os.Lstat(dest); err == nil {
		// Active generation already has a dir; leave legacy as a separate orphan.
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := ensureRealQuarantineRoot(filepath.Dir(dest)); err != nil {
		return err
	}
	if err := os.Rename(legacy, dest); err != nil {
		return fmt.Errorf("migrate legacy reserved scratch quarantine: %w", err)
	}
	return nil
}

// quarantineEntryIsActive reports whether entry under the quarantine root is the
// live generation for a still-present worktree. Fail closed (active) on marker
// I/O errors so prune never deletes under uncertainty.
func quarantineEntryIsActive(worktreeRoot, entryName string) bool {
	basename, generation, keyed := parseQuarantineEntryName(entryName)
	wtPath := filepath.Join(worktreeRoot, basename)
	absent, err := worktreePathAbsent(wtPath)
	if err != nil {
		return true
	}
	if absent {
		return false
	}
	activeGen, genErr := readReservedReviewScratchGeneration(wtPath)
	if genErr != nil {
		return true
	}
	if keyed {
		return activeGen != "" && activeGen == generation
	}
	// Legacy path-only key: active only while the worktree has no generation
	// marker yet (pre-upgrade lifecycle). Once a generation exists, the path-only
	// dir is a prior lifecycle orphan.
	return activeGen == ""
}

// ensureRealQuarantineTree ensures parent quarantine root and leaf dir exist as
// real (non-symlink) directories. Intermediate symlink roots are refused so
// payloads cannot be written outside Looper's quarantine tree.
func ensureRealQuarantineTree(quarantineLeaf string) error {
	root := filepath.Dir(quarantineLeaf)
	if filepath.Base(root) != reservedReviewScratchQuarantineDirName {
		return fmt.Errorf("quarantine leaf %q not under %s", quarantineLeaf, reservedReviewScratchQuarantineDirName)
	}
	if err := ensureRealQuarantineRoot(root); err != nil {
		return err
	}
	info, err := os.Lstat(quarantineLeaf)
	if err != nil {
		if os.IsNotExist(err) {
			if mkErr := os.Mkdir(quarantineLeaf, 0o755); mkErr != nil {
				return mkErr
			}
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("quarantine path %q is a symlink", quarantineLeaf)
	}
	if !info.IsDir() {
		return fmt.Errorf("quarantine path %q is not a directory", quarantineLeaf)
	}
	return nil
}

func ensureRealQuarantineRoot(root string) error {
	info, err := os.Lstat(root)
	if err != nil {
		if os.IsNotExist(err) {
			if mkErr := os.Mkdir(root, 0o755); mkErr != nil {
				return mkErr
			}
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("quarantine root %q is a symlink", root)
	}
	if !info.IsDir() {
		return fmt.Errorf("quarantine root %q is not a directory", root)
	}
	return nil
}

// requireRealDirNoFollow Lstats path and requires a non-symlink directory.
// Returns the original NotExist error when missing so callers can branch.
func requireRealDirNoFollow(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("path %q is a symlink", path)
	}
	if !info.IsDir() {
		return fmt.Errorf("path %q is not a directory", path)
	}
	return nil
}

// removeQuarantineDirNoFollow deletes dir only when the quarantine root and dir
// itself are real directories (Lstat). Missing paths are success. Symlinks are
// refused so os.RemoveAll cannot follow an intermediate symlink out of quarantine.
func removeQuarantineDirNoFollow(dir string) error {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil
	}
	root := filepath.Dir(dir)
	if filepath.Base(root) != reservedReviewScratchQuarantineDirName {
		return fmt.Errorf("refuse quarantine delete outside %s: %q", reservedReviewScratchQuarantineDirName, dir)
	}
	if err := requireRealDirNoFollow(root); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := requireRealDirNoFollow(dir); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return os.RemoveAll(dir)
}

// orphanQuarantineExpired reports whether the orphan quarantine container is
// older than cutoff. Retention is based on quarantine creation age (the
// worktree-scoped dir and its relocate-created child entries), never on nested
// payload file mtimes: os.Rename preserves source mtime, so aging scratch
// quarantined today would otherwise expire as soon as its worktree becomes an
// orphan outside CleanupWorktree. Any fresh or unreadable container entry keeps
// the orphan for another retention cycle.
func orphanQuarantineExpired(qPath string, cutoff time.Time) bool {
	newest, ok := quarantineContainerNewestModTime(qPath)
	if !ok {
		return false
	}
	return newest.Before(cutoff)
}

// quarantineContainerNewestModTime returns the newest mtime among qPath and its
// direct children (random destination subdirs created at relocate time). Nested
// payload files are intentionally ignored so rename-preserved source mtimes
// cannot collapse the recovery window.
func quarantineContainerNewestModTime(qPath string) (time.Time, bool) {
	info, err := os.Stat(qPath)
	if err != nil {
		return time.Time{}, false
	}
	newest := info.ModTime()
	entries, err := os.ReadDir(qPath)
	if err != nil {
		return time.Time{}, false
	}
	for _, entry := range entries {
		ei, infoErr := entry.Info()
		if infoErr != nil {
			return time.Time{}, false
		}
		if ei.ModTime().After(newest) {
			newest = ei.ModTime()
		}
	}
	return newest, true
}

// worktreePathAbsent reports whether path does not exist. Non-existence is the
// only affirmative absence signal; permission and other Stat errors are returned
// so callers fail closed rather than treating an unreadable path as removed.
func worktreePathAbsent(path string) (bool, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return true, nil
	}
	_, err := os.Stat(path)
	if err == nil {
		return false, nil
	}
	if os.IsNotExist(err) {
		return true, nil
	}
	return false, err
}

// coreIgnoreCase reports the repository's effective core.ignoreCase setting.
// Unset or unreadable config is treated as false (Git's documented default when
// init/clone did not probe a case-insensitive filesystem).
func (g *Gateway) coreIgnoreCase(ctx context.Context, repoPath string) bool {
	result, err := g.runGitResult(ctx, repoPath, nil, "config", "--bool", "--get", "core.ignoreCase")
	if err != nil {
		return false
	}
	return strings.TrimSpace(result.Stdout) == "true"
}

func (g *Gateway) validateMutationWorktree(worktreePath, repoPath, worktreeRoot string) error {
	if repoPath == "" && worktreeRoot == "" && g.repos != nil {
		// Legacy callers may not provide safety context. Keep them working while
		// adapter callers pass repo/root context for the hard safety check.
		return nil
	}
	return worktreesafety.Validate(worktreesafety.CheckInput{WorktreePath: worktreePath, RepoPath: repoPath, WorktreeRoot: worktreeRoot})
}

func (g *Gateway) AssertWritableBranch(branch string, protectedBranches []string) error {
	for _, protectedBranch := range protectedBranches {
		if protectedBranch == branch {
			return &ProtectedBranchError{Branch: branch}
		}
	}
	return nil
}

func (g *Gateway) getHeadSHA(ctx context.Context, repoPath string) (string, error) {
	result, err := g.runGitResult(ctx, repoPath, nil, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(result.Stdout), nil
}

func (g *Gateway) branchExists(ctx context.Context, repoPath, branch string) (bool, error) {
	_, err := g.runGitResult(ctx, repoPath, nil, "show-ref", "--quiet", "--verify", "refs/heads/"+branch)
	if err == nil {
		return true, nil
	}
	var commandErr *shell.CommandExecutionError
	if errors.As(err, &commandErr) && commandErr.Result.ExitCode == 1 {
		return false, nil
	}
	return false, err
}

func (g *Gateway) remoteBranchExists(ctx context.Context, repoPath, remote, branch string) (bool, error) {
	_, err := g.runGitResult(ctx, repoPath, nil, "show-ref", "--quiet", "--verify", "refs/remotes/"+remote+"/"+branch)
	if err == nil {
		return true, nil
	}
	var commandErr *shell.CommandExecutionError
	if errors.As(err, &commandErr) && commandErr.Result.ExitCode == 1 {
		return false, nil
	}
	return false, err
}

func (g *Gateway) resolveDetachedStartPoint(ctx context.Context, input CreateWorktreeInput) (string, error) {
	startPoint, ok, err := g.resolveDetachedStartPointRef(ctx, input.RepoPath, input.Branch)
	if err != nil {
		return "", err
	}
	if ok {
		return startPoint, nil
	}

	startPoint, ok, err = g.resolveDetachedStartPointRef(ctx, input.RepoPath, input.BaseBranch)
	if err != nil {
		return "", err
	}
	if ok {
		return startPoint, nil
	}

	return "", fmt.Errorf("resolve detached start point: no local or remote ref found for branch %q or base branch %q", input.Branch, input.BaseBranch)
}

func (g *Gateway) resolveAttachedStartPoint(ctx context.Context, repoPath, branch, baseBranch string) (string, error) {
	startPoint, ok, err := g.resolveDetachedStartPointRef(ctx, repoPath, branch)
	if err != nil {
		return "", err
	}
	if ok {
		return startPoint, nil
	}

	startPoint, ok, err = g.resolveDetachedStartPointRef(ctx, repoPath, baseBranch)
	if err != nil {
		branchExists, branchErr := g.branchExists(ctx, repoPath, baseBranch)
		if branchErr == nil && branchExists {
			return baseBranch, nil
		}
		return "", err
	}
	if ok {
		return startPoint, nil
	}
	return "", fmt.Errorf("resolve attached start point: no local or remote ref found for base branch %q", baseBranch)
}

func (g *Gateway) resolveDetachedStartPointRef(ctx context.Context, repoPath, branch string) (string, bool, error) {
	remote := "origin"
	hasRemote, err := g.hasRemote(ctx, repoPath, remote)
	if err != nil {
		return "", false, err
	}
	if hasRemote {
		remoteHeadSHA, err := g.getRemoteHeadSHA(ctx, repoPath, remote, branch)
		if err != nil {
			return "", false, err
		}
		if remoteHeadSHA != "" {
			remoteRef := fmt.Sprintf("refs/remotes/%s/%s", remote, branch)
			if err := g.runGit(ctx, repoPath, nil, "fetch", remote, fmt.Sprintf("+refs/heads/%s:%s", branch, remoteRef)); err != nil {
				return "", false, err
			}

			remoteBranchExists, err := g.remoteBranchExists(ctx, repoPath, remote, branch)
			if err != nil {
				return "", false, err
			}
			if remoteBranchExists {
				return remote + "/" + branch, true, nil
			}
		}
	}

	branchExists, err := g.branchExists(ctx, repoPath, branch)
	if err != nil {
		return "", false, err
	}
	if branchExists {
		return branch, true, nil
	}

	return "", false, nil
}

func (g *Gateway) hasRemote(ctx context.Context, repoPath, remote string) (bool, error) {
	_, err := g.runGitResult(ctx, repoPath, nil, "config", "--get", "remote."+remote+".url")
	if err == nil {
		return true, nil
	}
	var commandErr *shell.CommandExecutionError
	if errors.As(err, &commandErr) && commandErr.Result.ExitCode == 1 {
		return false, nil
	}
	return false, err
}

func (g *Gateway) IsAncestor(ctx context.Context, repoPath, ancestor, descendant string) (bool, error) {
	return g.isAncestor(ctx, repoPath, ancestor, descendant)
}

func (g *Gateway) FetchBranch(ctx context.Context, repoPath, remote, branch string) error {
	remote = strings.TrimSpace(remote)
	branch = strings.TrimSpace(branch)
	if remote == "" {
		remote = "origin"
	}
	if branch == "" {
		return fmt.Errorf("branch is required")
	}
	return g.runGit(ctx, repoPath, nil, "fetch", remote, branch)
}

// MergeBaseInput asks to merge a remote base branch into a worktree.
type MergeBaseInput struct {
	WorktreePath string
	Remote       string
	BaseBranch   string
}

// MergeBaseResult reports the outcome of a base merge.
type MergeBaseResult struct {
	AlreadyUpToDate bool
	// Conflicted is true when the merge left conflict markers in the worktree for
	// an agent to resolve (the merge is intentionally NOT aborted in that case).
	Conflicted bool
}

// MergeBaseIntoWorktree fetches the base branch and merges it into the worktree.
// On a clean merge it returns AlreadyUpToDate as appropriate; on conflicts it
// leaves the conflict markers in place and returns Conflicted=true so the caller
// can hand the worktree to an agent; on any other merge failure it aborts to keep
// the worktree clean and returns the error.
func (g *Gateway) MergeBaseIntoWorktree(ctx context.Context, input MergeBaseInput) (MergeBaseResult, error) {
	remote := strings.TrimSpace(input.Remote)
	if remote == "" {
		remote = "origin"
	}
	base := strings.TrimSpace(input.BaseBranch)
	if base == "" {
		return MergeBaseResult{}, fmt.Errorf("base branch is required")
	}
	if err := g.FetchBranch(ctx, input.WorktreePath, remote, base); err != nil {
		return MergeBaseResult{}, err
	}
	res, err := g.runGitResult(ctx, input.WorktreePath, nil, "merge", "--no-edit", remote+"/"+base)
	out := res.Stdout + res.Stderr
	if err == nil {
		return MergeBaseResult{AlreadyUpToDate: strings.Contains(out, "Already up to date")}, nil
	}
	if strings.Contains(out, "CONFLICT") || strings.Contains(out, "Automatic merge failed") {
		return MergeBaseResult{Conflicted: true}, nil
	}
	// Some other merge failure — abort so the worktree isn't left half-merged.
	_, _ = g.runGitResult(ctx, input.WorktreePath, nil, "merge", "--abort")
	return MergeBaseResult{}, err
}

func (g *Gateway) isAncestor(ctx context.Context, repoPath, ancestor, descendant string) (bool, error) {
	_, err := g.runGitResult(ctx, repoPath, nil, "merge-base", "--is-ancestor", ancestor, descendant)
	if err == nil {
		return true, nil
	}
	var commandErr *shell.CommandExecutionError
	if errors.As(err, &commandErr) && commandErr.Result.ExitCode == 1 {
		return false, nil
	}
	return false, err
}

func (g *Gateway) isHealthyWorktree(ctx context.Context, worktreePath string) (bool, error) {
	if _, err := os.Stat(worktreePath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}

	_, err := g.runGitResult(ctx, worktreePath, nil, "status", "--porcelain", "--untracked-files=all")
	if err == nil {
		return true, nil
	}
	return false, err
}

func (g *Gateway) isDetachedWorktree(ctx context.Context, worktreePath string) (bool, error) {
	result, err := g.runGitResult(ctx, worktreePath, nil, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(result.Stdout) == "HEAD", nil
}

func (g *Gateway) getCurrentBranch(ctx context.Context, worktreePath string) (string, error) {
	result, err := g.runGitResult(ctx, worktreePath, nil, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", err
	}
	branch := strings.TrimSpace(result.Stdout)
	if branch == "HEAD" {
		return "", nil
	}
	return branch, nil
}

func (g *Gateway) matchesRestoreCheckoutMode(ctx context.Context, worktreePath string, checkoutMode CheckoutMode, branch string) (bool, error) {
	if checkoutMode == CheckoutModeDetached {
		return g.isDetachedWorktree(ctx, worktreePath)
	}

	currentBranch, err := g.getCurrentBranch(ctx, worktreePath)
	if err != nil {
		return false, err
	}
	return currentBranch == branch, nil
}

func (g *Gateway) shouldReplaceStoredWorktreeOnRestoreMismatch(ctx context.Context, worktreePath string, checkoutMode CheckoutMode, branch, expectedWorktreePath string) (bool, error) {
	if checkoutMode == CheckoutModeDetached {
		return false, nil
	}

	currentBranch, err := g.getCurrentBranch(ctx, worktreePath)
	if err != nil {
		return false, err
	}
	if currentBranch == "" {
		return normalizeComparablePath(worktreePath) == normalizeComparablePath(expectedWorktreePath), nil
	}
	return currentBranch != branch, nil
}

func (g *Gateway) tryRemoveWorktree(ctx context.Context, repoPath, worktreePath string) {
	if err := g.runGit(ctx, repoPath, nil, "worktree", "remove", "--force", worktreePath); err != nil {
		return
	}
}

// worktreeArtifactExcludePatterns are clone-local ignore patterns installed into
// each managed worktree's info/exclude. They cover:
//   - universal build output that must never be committed by the agent or by
//     looper's fallback commit (e.g. .pnpm-store/ on repos whose .gitignore
//     misses it)
//   - reserved top-level reviewer submission scratch (/.looper-review-*.json)
//     as a best-effort defense when no higher-precedence .gitignore negation
//     re-includes the path (tracked .gitignore outranks info/exclude)
//
// The list is intentionally conservative — never broad source globs. Patterns
// only hide untracked matches; tracked modifications remain visible to status.
// When exclude is overridden, PrepareWorktree/Commit unstage newly-added
// reserved scratch, PrepareWorktree relocates untracked reserved payloads
// outside the worktree (never deletes), readStatus ignores only untracked root
// reserved scratch for InspectHead races, and tracked same-name paths still commit.
var worktreeArtifactExcludePatterns = []string{
	".pnpm-store/",
	"node_modules/",
	".turbo/",
	"dist/",
	".next/",
	".cache/",
	"*.log",
	// Root-anchored reviewer scratch namespace (ephemeral submit payload only).
	"/.looper-review-*.json",
}

const worktreeExcludeManagedHeader = "# looper: build-artifact excludes (managed; safe to remove)"

// applyWorktreeArtifactExcludes appends looper's managed ignore patterns to the
// worktree's git exclude file (info/exclude). That file is local to this clone
// and is never committed, so it cannot alter the repo's tracked files or its
// .gitignore. The write is idempotent (patterns already present are skipped)
// and best-effort: any failure is swallowed so it never blocks a loop worktree.
func (g *Gateway) applyWorktreeArtifactExcludes(ctx context.Context, worktreePath string) {
	if strings.TrimSpace(worktreePath) == "" {
		return
	}
	result, err := g.runGitResult(ctx, worktreePath, nil, "rev-parse", "--git-path", "info/exclude")
	if err != nil {
		return
	}
	excludePath := strings.TrimSpace(result.Stdout)
	if excludePath == "" {
		return
	}
	if !filepath.IsAbs(excludePath) {
		excludePath = filepath.Join(worktreePath, excludePath)
	}

	existing, err := os.ReadFile(excludePath)
	if err != nil && !os.IsNotExist(err) {
		return
	}
	existingText := string(existing)

	missing := make([]string, 0, len(worktreeArtifactExcludePatterns))
	for _, pattern := range worktreeArtifactExcludePatterns {
		if !worktreeExcludeContainsPattern(existingText, pattern) {
			missing = append(missing, pattern)
		}
	}
	if len(missing) == 0 {
		return
	}

	if err := os.MkdirAll(filepath.Dir(excludePath), 0o755); err != nil {
		return
	}
	var builder strings.Builder
	builder.WriteString(existingText)
	if existingText != "" && !strings.HasSuffix(existingText, "\n") {
		builder.WriteByte('\n')
	}
	if !strings.Contains(existingText, worktreeExcludeManagedHeader) {
		builder.WriteString(worktreeExcludeManagedHeader)
		builder.WriteByte('\n')
	}
	for _, pattern := range missing {
		builder.WriteString(pattern)
		builder.WriteByte('\n')
	}
	_ = os.WriteFile(excludePath, []byte(builder.String()), 0o644)
}

// worktreeExcludeContainsPattern reports whether pattern already appears as its
// own line in the exclude file (ignoring surrounding whitespace).
func worktreeExcludeContainsPattern(content, pattern string) bool {
	for _, line := range strings.Split(content, "\n") {
		if strings.TrimSpace(line) == pattern {
			return true
		}
	}
	return false
}

func (g *Gateway) getRemoteHeadSHA(ctx context.Context, repoPath, remote, branch string) (string, error) {
	result, err := g.runGitResult(ctx, repoPath, nil, "ls-remote", "--heads", remote, branch)
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(strings.Split(result.Stdout, "\n")[0])
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return "", nil
	}
	return fields[0], nil
}

func (g *Gateway) getRevision(ctx context.Context, repoPath, ref string) (string, error) {
	result, err := g.runGitResult(ctx, repoPath, nil, "rev-parse", ref)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(result.Stdout), nil
}

func (g *Gateway) listCommitsSince(ctx context.Context, repoPath, baseRef string) ([]string, error) {
	result, err := g.runGitResult(ctx, repoPath, nil, "rev-list", "--reverse", baseRef+"..HEAD")
	if err != nil {
		return nil, err
	}

	lines := strings.Split(result.Stdout, "\n")
	commits := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			commits = append(commits, trimmed)
		}
	}
	return commits, nil
}

type statusEntry struct {
	Code string
	Path string
}

func (g *Gateway) readStatus(ctx context.Context, repoPath string) ([]statusEntry, error) {
	// -z: NUL-terminated, unquoted paths so non-ASCII reserved basenames
	// (e.g. .looper-review-é.json) are not core.quotePath-escaped with
	// backslashes that would fail the root-only separator check.
	// Capture budget is statusMaxCapturedBytes (32 MiB by default), not the
	// shell runner's 256 KiB generic default: status is shared by InspectHead,
	// PrepareWorktree, and WorktreeClean for every role. Ordinary large dirt
	// must return a complete classification so worker/planner/fixer can fall
	// back to commit instead of hard-failing before any publish attempt.
	result, err := g.runGitResultWithMaxCapture(ctx, repoPath, nil, statusMaxCapturedBytes,
		"status", "--porcelain", "-z", "--untracked-files=all", "--ignored=no")
	if err != nil {
		return nil, err
	}
	// Fail closed even with the raised budget: if truncation still drops paths
	// after a reserved-only prefix, filtering would report clean and a later
	// fallback commit could publish the omitted ordinary dirt.
	if result.StdoutTruncated {
		return nil, fmt.Errorf("git status output truncated after %d bytes; refuse incomplete dirt classification", len(result.Stdout))
	}

	ignoreCase := g.coreIgnoreCase(ctx, repoPath)
	entries := []statusEntry{}
	for _, entry := range parsePorcelainStatusZ(result.Stdout) {
		// info/exclude is lower precedence than tracked .gitignore. When a repo
		// re-includes reserved scratch via negation, git still reports it as
		// untracked; drop only those root-level untracked/ITA entries for dirt.
		if isIgnorableReservedReviewerScratch(entry, ignoreCase) {
			continue
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// parsePorcelainStatusZ parses git status --porcelain -z output. Rename/copy
// entries emit destination then origin as consecutive NUL fields.
func parsePorcelainStatusZ(stdout string) []statusEntry {
	parts := strings.Split(stdout, "\x00")
	entries := make([]statusEntry, 0)
	for i := 0; i < len(parts); {
		record := parts[i]
		i++
		if record == "" {
			continue
		}
		if len(record) < 3 {
			continue
		}
		code := record[:2]
		if code == "!!" {
			continue
		}
		// Preserve every byte after "XY " through the NUL terminator. -z
		// pathnames are literal (including leading/trailing whitespace); trimming
		// would rewrite names like " .looper-review-x.json" into reserved ones.
		path := record[3:]
		if isRenameOrCopyStatus(code) {
			// Consume the origin path field that follows destination.
			if i < len(parts) {
				i++
			}
		}
		entries = append(entries, statusEntry{Code: code, Path: path})
	}
	return entries
}

func isRenameOrCopyStatus(code string) bool {
	if len(code) != 2 {
		return false
	}
	return code[0] == 'R' || code[0] == 'C' || code[1] == 'R' || code[1] == 'C'
}

func splitNUL(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, "\x00")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		// Preserve every byte between NULs (git -z pathnames are literal).
		if part == "" {
			continue
		}
		out = append(out, part)
	}
	return out
}

// isIgnorableReservedReviewerScratch reports root-level files in the reserved
// /.looper-review-*.json namespace that are safe to treat as non-dirt for
// inspection. Nested lookalikes and tracked M/D (or fully staged non-ITA
// changes) remain visible dirt. Accepted porcelain codes:
//   - "??" untracked (typical after exclude or after unstage)
//   - " A" intent-to-add (git add -N): Prepare unstages ITA with
//     --ita-visible-in-index, but status still accepts " A" so dirt
//     classification stays consistent if ITA is observed mid-lifecycle
//
// PrepareWorktree unstages reserved additions first so prestaged/ITA scratch
// becomes "??" before hard reset; Commit reuses the same unstage after
// git add -A. Both share isReservedReviewerScratchPath — see that function
// and the unstage comment for trade-offs and failure modes.
func isIgnorableReservedReviewerScratch(entry statusEntry, ignoreCase bool) bool {
	if entry.Code != "??" && entry.Code != " A" {
		return false
	}
	return isReservedReviewerScratchPath(entry.Path, ignoreCase)
}

// reservedReviewerScratchPrefix/Suffix implement Git's root-only
// /.looper-review-*.json wildcard as literal prefix/suffix checks so the
// middle * matches any bytes (including empty and newline). Production
// callers always pass git -z pathnames: every byte is literal — no
// core.quotePath dequoting.
const (
	reservedReviewerScratchPrefix = ".looper-review-"
	reservedReviewerScratchSuffix = ".json"
)

// isReservedReviewerScratchPath reports whether path is a root-level reserved
// reviewer scratch basename. When ignoreCase is true (repo core.ignoreCase),
// matching folds ASCII case the same way Git's installed
// /.looper-review-*.json exclude and :(glob,icase) pathspecs do.
func isReservedReviewerScratchPath(path string, ignoreCase bool) bool {
	// Do not TrimSpace: leading/trailing whitespace is part of a literal
	// pathname from git -z and is outside the reserved root basename namespace.
	if path == "" {
		return false
	}
	// Root-anchored only: '/' is the repository path separator in git -z
	// output. On Unix a literal backslash is a valid basename byte (not a
	// separator) and Git's /.looper-review-*.json wildcard matches it.
	// Callers that need non-ASCII basenames must pass -z so paths are not
	// core.quotePath-escaped.
	if strings.Contains(path, "/") {
		return false
	}
	if ignoreCase {
		if !hasPrefixFold(path, reservedReviewerScratchPrefix) ||
			!hasSuffixFold(path, reservedReviewerScratchSuffix) {
			return false
		}
	} else if !strings.HasPrefix(path, reservedReviewerScratchPrefix) ||
		!strings.HasSuffix(path, reservedReviewerScratchSuffix) {
		return false
	}
	// Prefix and suffix must not overlap; empty middle matches Git * (e.g.
	// .looper-review-.json).
	return len(path) >= len(reservedReviewerScratchPrefix)+len(reservedReviewerScratchSuffix)
}

func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

func hasSuffixFold(s, suffix string) bool {
	return len(s) >= len(suffix) && strings.EqualFold(s[len(s)-len(suffix):], suffix)
}

func (g *Gateway) runGit(ctx context.Context, cwd string, env map[string]string, args ...string) error {
	_, err := g.runGitResult(ctx, cwd, env, args...)
	return err
}

func (g *Gateway) runGitWithStartGate(ctx context.Context, cwd string, env map[string]string, startGate func(start func() error) error, args ...string) error {
	_, err := g.runGitResultOnceWithStartGate(ctx, cwd, env, startGate, args...)
	return err
}

func (g *Gateway) runGitResult(ctx context.Context, cwd string, env map[string]string, args ...string) (shell.Result, error) {
	return g.runGitResultWithMaxCapture(ctx, cwd, env, 0, args...)
}

// runGitResultWithMaxCapture runs git with an explicit shell stdout/stderr
// capture budget. maxCapturedBytes <= 0 leaves the shell package default (256
// KiB). Status classification uses a higher budget so ordinary large dirt is
// complete; other git calls keep the generic default.
func (g *Gateway) runGitResultWithMaxCapture(ctx context.Context, cwd string, env map[string]string, maxCapturedBytes int, args ...string) (shell.Result, error) {
	var result shell.Result
	var err error
	for attempt := 0; ; attempt++ {
		result, err = g.runGitResultOnceWithStartGateCapture(ctx, cwd, env, nil, maxCapturedBytes, args...)
		if err == nil || !isRetryableFetchRefLockRace(args, err) || attempt >= len(fetchRefLockRetryDelays) {
			return result, err
		}

		timer := time.NewTimer(fetchRefLockRetryDelays[attempt])
		select {
		case <-ctx.Done():
			timer.Stop()
			return result, ctx.Err()
		case <-timer.C:
		}
	}
}

func (g *Gateway) runGitResultOnce(ctx context.Context, cwd string, env map[string]string, args ...string) (shell.Result, error) {
	return g.runGitResultOnceWithStartGate(ctx, cwd, env, nil, args...)
}

func (g *Gateway) runGitResultOnceWithStartGate(ctx context.Context, cwd string, env map[string]string, startGate func(start func() error) error, args ...string) (shell.Result, error) {
	return g.runGitResultOnceWithStartGateCapture(ctx, cwd, env, startGate, 0, args...)
}

func (g *Gateway) runGitResultOnceWithStartGateCapture(ctx context.Context, cwd string, env map[string]string, startGate func(start func() error) error, maxCapturedBytes int, args ...string) (shell.Result, error) {
	result, err := shell.Run(ctx, shell.Options{
		Command:          g.gitPath,
		Args:             args,
		CWD:              cwd,
		Env:              env,
		StartGate:        startGate,
		MaxCapturedBytes: maxCapturedBytes,
	})
	if err == nil {
		return result, nil
	}

	var commandErr *shell.CommandExecutionError
	if errors.As(err, &commandErr) {
		message := strings.TrimSpace(commandErr.Result.Stderr)
		if message == "" {
			message = strings.TrimSpace(commandErr.Result.Stdout)
		}
		if message == "" {
			message = commandErr.Error()
		}
		formatted := *commandErr
		formatted.Message = message
		return formatted.Result, fmt.Errorf("git %s: %w", strings.Join(args, " "), &formatted)
	}

	return result, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
}

func isRetryableFetchRefLockRace(args []string, err error) bool {
	if len(args) == 0 || args[0] != "fetch" || err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "cannot lock ref") && strings.Contains(message, " but expected ")
}

func buildWorktreeDirectoryName(input CreateWorktreeInput) string {
	if input.PRNumber != 0 {
		if normalizeCheckoutMode(input.CheckoutMode) == CheckoutModeDetached {
			return fmt.Sprintf("looper-fix-%s-pr-%d-detached", sanitizeBranchName(input.ProjectID), input.PRNumber)
		}
		return fmt.Sprintf("looper-fix-%s-pr-%d", sanitizeBranchName(input.ProjectID), input.PRNumber)
	}

	return sanitizeBranchName(input.Branch)
}

// DetachedPRWorktreePath returns the managed shared detached worktree path that
// CreateWorktree claims for a PR (looper-fix-<project>-pr-<N>-detached).
// Callers that must read ownership markers before CreateWorktree revokes them
// should probe this candidate path even when no checkpoint worktree exists.
func DetachedPRWorktreePath(worktreeRoot, projectID string, prNumber int64) string {
	worktreeRoot = strings.TrimSpace(worktreeRoot)
	projectID = strings.TrimSpace(projectID)
	if worktreeRoot == "" || projectID == "" || prNumber == 0 {
		return ""
	}
	return filepath.Join(worktreeRoot, buildWorktreeDirectoryName(CreateWorktreeInput{
		ProjectID:    projectID,
		PRNumber:     prNumber,
		CheckoutMode: CheckoutModeDetached,
	}))
}

func parseGitHubRepoFromRemoteURL(remoteURL string) string {
	host, repo := parseRemoteRepoFromURL(remoteURL)
	if !isGitHubRemoteHost(host) {
		return ""
	}
	return repo
}

// parseRemoteRepoFromURL extracts host and owner/name from common git remote URL forms:
//   - user@host:owner/repo.git
//   - user@[ipv6]:owner/repo.git
//   - ssh://git@host/owner/repo.git
//   - https://host/owner/repo.git
//   - http://host/owner/repo.git
func parseRemoteRepoFromURL(remoteURL string) (host, repo string) {
	remoteURL = strings.TrimSpace(remoteURL)
	if remoteURL == "" {
		return "", ""
	}

	if parsed, err := url.Parse(remoteURL); err == nil && parsed.Scheme != "" {
		switch strings.ToLower(parsed.Scheme) {
		case "ssh", "http", "https":
			host = strings.ToLower(strings.TrimSpace(parsed.Hostname()))
			repo = strings.TrimSuffix(strings.Trim(strings.TrimSpace(parsed.Path), "/"), ".git")
			if host != "" && isOwnerNameRepo(repo) {
				return host, repo
			}
		}
		return "", ""
	}

	pattern := regexp.MustCompile(`^(?:[^@/:]+@)?(?P<host>\[[^]]+\]|[^/:]+):(?P<repo>[^/]+/[^/]+?)(?:\.git)?/?$`)
	match := pattern.FindStringSubmatch(remoteURL)
	if match == nil {
		return "", ""
	}
	host = strings.ToLower(strings.Trim(strings.TrimSpace(match[pattern.SubexpIndex("host")]), "[]"))
	repo = strings.TrimSuffix(strings.TrimSpace(match[pattern.SubexpIndex("repo")]), ".git")
	if host != "" && isOwnerNameRepo(repo) {
		return host, repo
	}
	return "", ""
}

func isGitHubRemoteHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	return host == "github.com" || strings.HasSuffix(host, ".github.com")
}

func isOwnerNameRepo(repo string) bool {
	parts := strings.Split(repo, "/")
	if len(parts) != 2 {
		return false
	}
	return strings.TrimSpace(parts[0]) != "" && strings.TrimSpace(parts[1]) != ""
}

func sanitizeBranchName(branch string) string {
	var builder strings.Builder
	for _, r := range branch {
		if ('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z') || ('0' <= r && r <= '9') || r == '.' || r == '_' || r == '-' {
			builder.WriteRune(r)
			continue
		}
		builder.WriteRune('-')
	}
	return builder.String()
}

func isWithinRoot(path, root string) bool {
	resolvedPath := normalizeComparablePath(path)
	resolvedRoot := normalizeComparablePath(root)
	return resolvedPath == resolvedRoot || strings.HasPrefix(resolvedPath, resolvedRoot+"/")
}

func normalizeComparablePath(path string) string {
	if path == "" {
		return ""
	}
	resolved, err := filepath.Abs(path)
	if err != nil {
		resolved = path
	}
	resolved = filepath.ToSlash(filepath.Clean(resolved))
	if strings.HasPrefix(resolved, "/private/") {
		return strings.TrimPrefix(resolved, "/private")
	}
	return resolved
}

func parseWorktreeList(output string) []WorktreeListEntry {
	entries := []WorktreeListEntry{}
	current := WorktreeListEntry{}

	flush := func() {
		if current.Path == "" {
			return
		}
		entries = append(entries, current)
		current = WorktreeListEntry{}
	}

	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}

		switch {
		case strings.HasPrefix(line, "worktree "):
			current.Path = strings.TrimPrefix(line, "worktree ")
		case strings.HasPrefix(line, "branch refs/heads/"):
			current.Branch = strings.TrimPrefix(line, "branch refs/heads/")
		case strings.HasPrefix(line, "HEAD "):
			current.HeadSHA = strings.TrimPrefix(line, "HEAD ")
		case line == "bare":
			current.Bare = true
		}
	}

	flush()
	return entries
}

func normalizeCheckoutMode(mode CheckoutMode) CheckoutMode {
	if mode == CheckoutModeDetached {
		return CheckoutModeDetached
	}
	return CheckoutModeBranch
}

func mustRandomID() string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		panic(err)
	}
	return hex.EncodeToString(raw)
}

func stringPtr(value string) *string {
	return &value
}

func stringPtrIfNotEmpty(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func valueOr(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

func existingRecordID(record *storage.WorktreeRecord) string {
	if record == nil {
		return ""
	}
	return record.ID
}

func existingRecordCreatedAt(record *storage.WorktreeRecord) string {
	if record == nil {
		return ""
	}
	return record.CreatedAt
}
