package git

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/infra/shell"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/worktreesafety"
)

const javaScriptISOStringLayout = "2006-01-02T15:04:05.000Z"

var (
	missingWorktreeErrorPattern = regexp.MustCompile(`(?i)is not a working tree|does not exist|not found|no such file`)
	pushConflictErrorPattern    = regexp.MustCompile(`(?i)stale info|non-fast-forward|failed to push|rejected`)
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
	// Generation is the claim generation the caller already resolved. Zero
	// means "resolve it from storage" (direct RestoreWorktree callers).
	Generation int64
	// CheckoutKey is the checkout identity the caller already resolved. Empty
	// means "derive it from ExpectedWorktreePath".
	CheckoutKey string
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
	// WorktreeID names the exact generation being reclaimed. Callers that read
	// a row to decide on cleanup must set it: branch is ambiguous once a
	// retired generation and its live successor share one.
	WorktreeID        string
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
	// LocalHeadSHA pins the source commit. When set, Push publishes this exact
	// object instead of resolving HEAD again after validation.
	LocalHeadSHA      string
	ProtectedBranches []string
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

	// Generation is resolved before the path is formed: a retired generation is
	// invisible to the live-checkout lookup, so this claim allocates the next
	// one and lands in a directory the previous writer cannot reach.
	checkoutKey := checkoutKeyForInput(input)
	generation, err := g.LiveGenerationForCheckout(ctx, input.ProjectID, checkoutKey)
	if err != nil {
		return storage.WorktreeRecord{}, err
	}
	worktreePath := filepath.Join(input.WorktreeRoot, buildWorktreeDirectoryName(input, generation))
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
		Generation:           generation,
		CheckoutKey:          checkoutKey,
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
		// A generation past the first checks out its own ref, so a retired
		// generation that is still committing cannot advance the ref this one
		// resolves HEAD through.
		localBranch := attachedBranchRefForGeneration(input.Branch, generation)
		branchExists, err := g.branchExists(ctx, input.RepoPath, localBranch)
		if err != nil {
			return storage.WorktreeRecord{}, err
		}
		args := []string{"worktree", "add", "--force"}
		if branchExists {
			args = append(args, worktreePath, localBranch)
		} else {
			startPoint, err := g.resolveAttachedStartPoint(ctx, input.RepoPath, input.Branch, input.BaseBranch)
			if err != nil {
				return storage.WorktreeRecord{}, err
			}
			args = append(args, "-b", localBranch, worktreePath, startPoint)
		}
		if err := g.runGit(ctx, input.RepoPath, nil, args...); err != nil {
			return storage.WorktreeRecord{}, err
		}
	}

	// Protect agent-authored git commands (agents run their own `git add -A`
	// per the repo prompts) with a WORKTREE-SCOPED excludes file: the file and
	// the config live in the worktree's private git dir, so the main checkout
	// and sibling worktrees are untouched. Best-effort. Also removes the
	// legacy managed block an earlier looper wrote into the shared
	// .git/info/exclude.
	g.applyWorktreeScopedArtifactExcludes(ctx, worktreePath)
	g.removeLegacySharedArtifactExcludes(ctx, worktreePath)
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
		existingRecord, err = g.repos.Worktrees.GetLiveByCheckout(ctx, input.ProjectID, checkoutKey)
		if err != nil {
			return storage.WorktreeRecord{}, fmt.Errorf("get existing worktree by checkout: %w", err)
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
		Generation:   generation,
		CheckoutKey:  checkoutKey,
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

	checkoutKey := strings.TrimSpace(input.CheckoutKey)
	if checkoutKey == "" {
		checkoutKey = storage.CheckoutKeyFromPath(input.ExpectedWorktreePath)
	}
	generation := input.Generation
	if generation < 1 {
		resolved, err := g.LiveGenerationForCheckout(ctx, input.ProjectID, checkoutKey)
		if err != nil {
			return nil, err
		}
		generation = resolved
	}
	attachedBranch := attachedBranchRefForGeneration(input.Branch, generation)
	// Directories belonging to retired generations are never adopted. Path
	// equality is exactly what retirement revokes, so `git worktree list`
	// discovery must not walk back into one.
	retiredPaths, err := g.retiredWorktreePaths(ctx)
	if err != nil {
		return nil, err
	}

	if g.repos != nil {
		stored, err := g.liveWorktreeRecord(ctx, input.ProjectID, checkoutKey, input.Branch)
		if err != nil {
			return nil, err
		}
		if stored != nil && stored.Status != "cleaned" && normalizeComparablePath(stored.RepoPath) == normalizeComparablePath(input.RepoPath) && worktreesafety.IsSafe(worktreesafety.CheckInput{WorktreePath: stored.WorktreePath, RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot}) {
			storedHealthy, err := g.isHealthyWorktree(ctx, stored.WorktreePath)
			if err != nil {
				return nil, err
			}
			if !storedHealthy {
				g.tryRemoveWorktree(ctx, input.RepoPath, stored.WorktreePath)
			} else {
				storedCheckoutMatches, err := g.matchesRestoreCheckoutMode(ctx, stored.WorktreePath, checkoutMode, attachedBranchRefForGeneration(input.Branch, stored.Generation))
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
					// Restoring for a new CreateWorktree claim drops prior fixer ownership.
					if err := worktreesafety.ClearFixerOwnerToken(restored.WorktreePath); err != nil {
						return nil, err
					}
					return &restored, nil
				}
				shouldReplace, err := g.shouldReplaceStoredWorktreeOnRestoreMismatch(ctx, stored.WorktreePath, checkoutMode, attachedBranchRefForGeneration(input.Branch, stored.Generation), input.ExpectedWorktreePath)
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
		if _, retired := retiredPaths[normalizeComparablePath(candidate.Path)]; retired {
			continue
		}
		if checkoutMode == CheckoutModeDetached {
			expectedPath := candidate.Path
			if input.ExpectedWorktreePath != "" {
				expectedPath = input.ExpectedWorktreePath
			}
			if normalizeComparablePath(candidate.Path) != normalizeComparablePath(expectedPath) {
				continue
			}
		} else if candidate.Branch != attachedBranch {
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

	checkoutMatches, err := g.matchesRestoreCheckoutMode(ctx, match.Path, checkoutMode, attachedBranch)
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
		Generation:   generation,
		CheckoutKey:  checkoutKey,
	}
	if g.repos != nil {
		existing, err := g.liveWorktreeRecord(ctx, input.ProjectID, checkoutKey, input.Branch)
		if err != nil {
			return nil, err
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
		if record.CheckoutKey == "" {
			record.CheckoutKey = storage.CheckoutKeyFromPath(match.Path)
		}
		if err := g.repos.Worktrees.Upsert(ctx, record); err != nil {
			return nil, fmt.Errorf("upsert discovered worktree record: %w", err)
		}
	}

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

	// Provenance check: refuse to remove a worktree that looper did not create.
	// This prevents destructive cleanup of external (non-looper) worktrees that
	// happen to share the same repo (#128).
	if g.repos == nil {
		return fmt.Errorf("refusing to remove worktree %q branch %q: looper provenance repository is unavailable", input.WorktreePath, input.Branch)
	}
	// Resolve by checkout, not by branch: a retired generation shares its branch
	// with the live successor, and a live branch lookup deliberately returns the
	// successor — which would fail the path check below and leave the retired
	// directory unreclaimable. One lookup serves both the provenance check and
	// the row marked cleaned, so the two cannot disagree about which row this is.
	existing, err := g.cleanupTargetRecord(ctx, input)
	if err != nil {
		return fmt.Errorf("provenance check: %w", err)
	}
	if existing == nil {
		return fmt.Errorf("refusing to remove worktree %q branch %q: no looper worktree record found for this project — worktree was not created by looper", input.WorktreePath, input.Branch)
	}
	if existing.Status != "active" {
		return fmt.Errorf("refusing to remove worktree %q branch %q: looper worktree record is %q, not active", input.WorktreePath, input.Branch, existing.Status)
	}
	if normalizeComparablePath(existing.RepoPath) != normalizeComparablePath(input.RepoPath) {
		return fmt.Errorf("refusing to remove worktree %q branch %q: looper record belongs to a different repository", input.WorktreePath, input.Branch)
	}
	if normalizeComparablePath(existing.WorktreePath) != normalizeComparablePath(input.WorktreePath) {
		return fmt.Errorf("refusing to remove worktree %q branch %q: looper record has a different path %q — refusing to remove a path that may belong to a different checkout", input.WorktreePath, input.Branch, existing.WorktreePath)
	}

	if err := g.runGitWithStartGate(ctx, input.RepoPath, nil, input.AdmitStart, "worktree", "remove", "--force", input.WorktreePath); err != nil {
		if !missingWorktreeErrorPattern.MatchString(err.Error()) {
			return err
		}
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

func (g *Gateway) cleanupTargetRecord(ctx context.Context, input CleanupWorktreeInput) (*storage.WorktreeRecord, error) {
	if id := strings.TrimSpace(input.WorktreeID); id != "" {
		record, err := g.repos.Worktrees.GetByID(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("get worktree by id before cleanup update: %w", err)
		}
		return record, nil
	}
	record, err := g.liveWorktreeRecord(ctx, input.ProjectID, storage.CheckoutKeyFromPath(input.WorktreePath), input.Branch)
	if err != nil {
		return nil, fmt.Errorf("get worktree before cleanup update: %w", err)
	}
	return record, nil
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
	sourceRef := "HEAD"
	if requested := strings.TrimSpace(input.LocalHeadSHA); requested != "" {
		resolved, err := g.getRevision(ctx, input.WorktreePath, requested+"^{commit}")
		if err != nil {
			return fmt.Errorf("resolve pinned push commit %s: %w", requested, err)
		}
		if resolved != requested {
			return fmt.Errorf("pinned push commit resolved to %s, want %s", resolved, requested)
		}
		sourceRef = requested
	}

	// Every push that updates an existing remote branch carries a lease. When
	// the caller has no durable prior observation, the lease is the remote head
	// resolved immediately before the push. Either way the branch can only move
	// forward from a head this daemon actually saw, so a stale agent that pushed
	// behind our back surfaces as a typed *RemoteHeadChangedError instead of an
	// opaque git rejection or an unnoticed divergence.
	expectedRemoteHeadSHA := strings.TrimSpace(input.ExpectedRemoteHeadSHA)
	derivedLease := false
	if expectedRemoteHeadSHA == "" {
		// Asked of the remote, not of the local tracking refs: a branch another
		// actor created since our last fetch has no refs/remotes/ entry here,
		// and treating that as "does not exist remotely" is what would drop us
		// onto the unleased creation path against a branch that does exist.
		// An empty answer means the branch is genuinely absent.
		resolved, err := g.getRemoteHeadSHA(ctx, input.WorktreePath, remote, input.Branch)
		if err != nil {
			return err
		}
		if strings.TrimSpace(resolved) != "" {
			expectedRemoteHeadSHA = strings.TrimSpace(resolved)
			derivedLease = true
			// A remote head we do not even have locally is a commit we cannot
			// be descended from — someone pushed it while we were not looking.
			if !g.hasLocalObject(ctx, input.WorktreePath, expectedRemoteHeadSHA) {
				return &RemoteHeadChangedError{Branch: input.Branch, ExpectedHeadSHA: "", ActualHeadSHA: expectedRemoteHeadSHA}
			}
		}
	}

	if expectedRemoteHeadSHA != "" {
		isExpectedAncestor, err := g.isAncestor(ctx, input.WorktreePath, expectedRemoteHeadSHA, sourceRef)
		if err != nil {
			return err
		}
		if !isExpectedAncestor {
			if derivedLease {
				// The remote moved to a commit we never built on. Report it in
				// the recoverable shape callers already handle.
				return &RemoteHeadChangedError{Branch: input.Branch, ExpectedHeadSHA: "", ActualHeadSHA: expectedRemoteHeadSHA}
			}
			return fmt.Errorf("Refusing fixer push for %s because local HEAD no longer descends from %s", input.Branch, expectedRemoteHeadSHA)
		}

		err = g.runGit(ctx, input.WorktreePath, nil,
			"push",
			"--porcelain",
			fmt.Sprintf("--force-with-lease=refs/heads/%s:%s", input.Branch, expectedRemoteHeadSHA),
			"-u",
			remote,
			fmt.Sprintf("%s:refs/heads/%s", sourceRef, input.Branch),
		)
		if err != nil {
			if pushConflictErrorPattern.MatchString(err.Error()) {
				actualHeadSHA, lookupErr := g.getRemoteHeadSHA(ctx, input.WorktreePath, remote, input.Branch)
				if lookupErr != nil {
					return lookupErr
				}
				return &RemoteHeadChangedError{Branch: input.Branch, ExpectedHeadSHA: expectedRemoteHeadSHA, ActualHeadSHA: actualHeadSHA}
			}
			return err
		}
		return g.setPinnedPushUpstream(ctx, input.WorktreePath, remote, input.Branch, sourceRef)
	}

	// Reached only when the branch does not exist remotely: creating a ref has
	// nothing to lease against.
	if err := g.runGit(ctx, input.WorktreePath, nil, "push", "-u", remote, fmt.Sprintf("%s:refs/heads/%s", sourceRef, input.Branch)); err != nil {
		return err
	}
	return g.setPinnedPushUpstream(ctx, input.WorktreePath, remote, input.Branch, sourceRef)
}

// hasLocalObject reports whether the commit is present in this checkout's
// object database.
func (g *Gateway) hasLocalObject(ctx context.Context, worktreePath, sha string) bool {
	if strings.TrimSpace(sha) == "" {
		return false
	}
	return g.runGit(ctx, worktreePath, nil, "cat-file", "-e", sha+"^{commit}") == nil
}

func (g *Gateway) setPinnedPushUpstream(ctx context.Context, worktreePath, remote, destinationBranch, sourceRef string) error {
	if sourceRef == "HEAD" {
		return nil
	}
	currentBranch, err := g.getCurrentBranch(ctx, worktreePath)
	if err != nil {
		return fmt.Errorf("resolve attached branch after pinned push: %w", err)
	}
	if currentBranch == "" {
		return nil
	}
	if err := g.runGit(ctx, worktreePath, nil, "branch", "--set-upstream-to="+remote+"/"+destinationBranch, currentBranch); err != nil {
		return fmt.Errorf("set upstream for pinned push branch %s: %w", currentBranch, err)
	}
	return nil
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

	statusBeforeReset, err := g.readStatus(ctx, input.WorktreePath)
	if err != nil {
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
	// Ensure the worktree-scoped artifact excludes exist before staging
	// (idempotent): worktrees created before this mechanism landed would
	// otherwise stage untracked artifacts. The ignore layer has exactly
	// gitignore semantics — tracked files, including intentionally tracked
	// dist/ etc., always stage.
	g.applyWorktreeScopedArtifactExcludes(ctx, input.WorktreePath)
	if err := g.runGit(ctx, input.WorktreePath, nil, "add", "-A"); err != nil {
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

// worktreeArtifactExcludePatterns are common build-artifact directories/files
// that must never be committed by the agent or by looper's fallback commit. On
// pnpm/node repos whose .gitignore misses one of these (e.g. .pnpm-store/, a
// 100MB+ content store), an `git add -A` would otherwise stage it and the push
// would be rejected. The list is intentionally conservative — only universal
// build output, never source.
var worktreeArtifactExcludePatterns = []string{
	".pnpm-store/",
	"node_modules/",
	".turbo/",
	"dist/",
	".next/",
	".cache/",
	"*.log",
}

const worktreeExcludeManagedHeader = "# looper: build-artifact excludes (managed; safe to remove)"

// applyWorktreeScopedArtifactExcludes writes looper's artifact patterns into
// the worktree's PRIVATE git dir ($GIT_DIR/info/looper-exclude — per-worktree
// for linked worktrees) and points worktree-scoped configuration at it via
// extensions.worktreeConfig. Any git invocation inside this worktree — the
// agent's own `git add -A` included — then ignores untracked artifacts, while
// the main checkout and sibling worktrees see none of it.
func (g *Gateway) applyWorktreeScopedArtifactExcludes(ctx context.Context, worktreePath string) {
	if strings.TrimSpace(worktreePath) == "" {
		return
	}
	result, err := g.runGitResult(ctx, worktreePath, nil, "rev-parse", "--git-dir")
	if err != nil {
		return
	}
	gitDir := strings.TrimSpace(result.Stdout)
	if gitDir == "" {
		return
	}
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(worktreePath, gitDir)
	}
	excludePath := filepath.Join(gitDir, "info", "looper-exclude")
	if err := os.MkdirAll(filepath.Dir(excludePath), 0o755); err != nil {
		return
	}
	var builder strings.Builder
	builder.WriteString(worktreeExcludeManagedHeader)
	builder.WriteByte('\n')
	for _, pattern := range worktreeArtifactExcludePatterns {
		builder.WriteString(pattern)
		builder.WriteByte('\n')
	}
	if err := os.WriteFile(excludePath, []byte(builder.String()), 0o644); err != nil {
		return
	}
	// extensions.worktreeConfig is additive and standard; without it,
	// --worktree config would land in the shared config file.
	if err := g.runGit(ctx, worktreePath, nil, "config", "extensions.worktreeConfig", "true"); err != nil {
		return
	}
	_ = g.runGit(ctx, worktreePath, nil, "config", "--worktree", "core.excludesFile", excludePath)
}

// removeLegacySharedArtifactExcludes strips the managed block an earlier
// looper version appended to the repository's COMMON .git/info/exclude, which
// leaked ignore policy to the main checkout and every sibling worktree (#71).
// Only the managed header and looper's own patterns are removed; all other
// content is preserved byte-for-byte. Best-effort.
func (g *Gateway) removeLegacySharedArtifactExcludes(ctx context.Context, worktreePath string) {
	result, err := g.runGitResult(ctx, worktreePath, nil, "rev-parse", "--git-common-dir")
	if err != nil {
		return
	}
	commonDir := strings.TrimSpace(result.Stdout)
	if commonDir == "" {
		return
	}
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(worktreePath, commonDir)
	}
	excludePath := filepath.Join(commonDir, "info", "exclude")
	raw, err := os.ReadFile(excludePath)
	if err != nil {
		return
	}
	if !strings.Contains(string(raw), worktreeExcludeManagedHeader) {
		return
	}
	managed := map[string]struct{}{worktreeExcludeManagedHeader: {}}
	for _, pattern := range worktreeArtifactExcludePatterns {
		managed[pattern] = struct{}{}
	}
	kept := make([]string, 0)
	for _, line := range strings.Split(string(raw), "\n") {
		if _, drop := managed[strings.TrimSpace(line)]; drop {
			continue
		}
		kept = append(kept, line)
	}
	_ = os.WriteFile(excludePath, []byte(strings.Join(kept, "\n")), 0o644)
}

// isArtifactExcludedUntrackedPath reports whether an UNTRACKED status path
// matches looper's artifact patterns. Only untracked entries are filtered —
// tracked files under these paths keep appearing in status, exactly like a
// gitignore would behave.
func isArtifactExcludedUntrackedPath(path string) bool {
	normalized := strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(path)), "./")
	for _, pattern := range worktreeArtifactExcludePatterns {
		if strings.HasSuffix(pattern, "/") {
			name := strings.TrimSuffix(pattern, "/")
			if normalized == name || strings.HasPrefix(normalized, name+"/") || strings.Contains(normalized, "/"+name+"/") {
				return true
			}
			continue
		}
		if matched, _ := filepath.Match(pattern, filepath.Base(normalized)); matched {
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
	result, err := g.runGitResult(ctx, repoPath, nil, "status", "--porcelain", "--untracked-files=all", "--ignored=no")
	if err != nil {
		return nil, err
	}

	entries := []statusEntry{}
	for _, line := range strings.Split(result.Stdout, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if len(line) < 3 {
			continue
		}
		if line[:2] == "!!" {
			continue
		}
		code, path := line[:2], strings.TrimSpace(line[3:])
		if code == "??" && isArtifactExcludedUntrackedPath(path) {
			// Untracked build artifacts are invisible to looper's dirt
			// decisions, mirroring the staging exclusion in Commit.
			continue
		}
		entries = append(entries, statusEntry{Code: code, Path: path})
	}
	return entries, nil
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
	var result shell.Result
	var err error
	for attempt := 0; ; attempt++ {
		result, err = g.runGitResultOnce(ctx, cwd, env, args...)
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
	result, err := shell.Run(ctx, shell.Options{Command: g.gitPath, Args: args, CWD: cwd, Env: env, StartGate: startGate})
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

// buildWorktreeDirectoryName renders the on-disk name for one generation of a
// managed checkout. The generation is part of the path, which is what makes
// containment enforceable without proving anything about a process: a retired
// generation and its successor simply address different directories, and the
// stale writer's open file handles keep pointing at the old one.
//
// Generation 1 keeps the historical name so no existing checkout moves on disk.
func buildWorktreeDirectoryName(input CreateWorktreeInput, generation int64) string {
	base := checkoutKeyForInput(input)
	if generation > 1 {
		return base + storage.WorktreeGenerationSuffix + strconv.FormatInt(generation, 10)
	}
	return base
}

// checkoutKeyForInput is the generation-1 directory name: the identity of the
// managed checkout itself, independent of which generation currently owns it.
// It is what generations are allocated against and what the live-row unique
// index is keyed by, because a branch is not a checkout — one project can hold
// an attached planner checkout and a detached PR checkout of the same branch.
func checkoutKeyForInput(input CreateWorktreeInput) string {
	if input.PRNumber != 0 {
		if normalizeCheckoutMode(input.CheckoutMode) == CheckoutModeDetached {
			return fmt.Sprintf("looper-fix-%s-pr-%d-detached", sanitizeBranchName(input.ProjectID), input.PRNumber)
		}
		return fmt.Sprintf("looper-fix-%s-pr-%d", sanitizeBranchName(input.ProjectID), input.PRNumber)
	}
	return sanitizeBranchName(input.Branch)
}

// attachedBranchRefForGeneration is the local branch an attached checkout of
// this generation is on.
//
// Distinct directories are not enough for an attached checkout. `git worktree
// add --force` lets two worktrees hold the same branch, so a retired generation
// that is still alive keeps committing to the SHARED ref — and the live
// generation, whose HEAD is a symref to that same ref, would silently resolve to
// the stale commit and could publish it. Path divergence contains the files; ref
// divergence is what contains the history.
//
// Generation 1 stays on the branch itself, so nothing about today's checkouts
// changes. The stored record's Branch is always the logical branch: this ref is
// local plumbing, and Push still writes to refs/heads/<branch> on the remote.
func attachedBranchRefForGeneration(branch string, generation int64) string {
	if generation <= 1 {
		return branch
	}
	return fmt.Sprintf("looper-gen/%d/%s", generation, branch)
}

// DetachedPRWorktreePath returns the managed shared detached worktree path that
// CreateWorktree claims for a PR at a given generation
// (looper-fix-<project>-pr-<N>-detached[-g<G>]). Callers that must read
// ownership markers before CreateWorktree revokes them should probe this
// candidate path even when no checkpoint worktree exists.
func DetachedPRWorktreePath(worktreeRoot, projectID string, prNumber int64, generation int64) string {
	worktreeRoot = strings.TrimSpace(worktreeRoot)
	projectID = strings.TrimSpace(projectID)
	if worktreeRoot == "" || projectID == "" || prNumber == 0 {
		return ""
	}
	return filepath.Join(worktreeRoot, buildWorktreeDirectoryName(CreateWorktreeInput{
		ProjectID:    projectID,
		PRNumber:     prNumber,
		CheckoutMode: CheckoutModeDetached,
	}, generation))
}

// retiredWorktreePaths indexes the directories of retired generations so that
// discovery and cleanup can refuse to touch them.
//
// Deliberately not filtered by project. Retirement revokes the filesystem path
// itself, and `git worktree list` is repository-wide: two projects can share a
// repo or a worktree root, and an attached checkout without a PR is named from
// the branch alone. Whichever project row recorded the retirement, the stale
// writer is in that directory.
func (g *Gateway) retiredWorktreePaths(ctx context.Context) (map[string]struct{}, error) {
	if g.repos == nil || g.repos.Worktrees == nil {
		return nil, nil
	}
	retired, err := g.repos.Worktrees.ListRetired(ctx)
	if err != nil {
		return nil, fmt.Errorf("list retired worktrees: %w", err)
	}
	paths := make(map[string]struct{}, len(retired))
	for _, record := range retired {
		paths[normalizeComparablePath(record.WorktreePath)] = struct{}{}
	}
	return paths, nil
}

// liveWorktreeRecord resolves the live row for one managed checkout. It falls
// back to the branch when the caller could not name a checkout — direct
// RestoreWorktree callers that pass no expected path — which is the historical
// behaviour for the single-checkout case.
func (g *Gateway) liveWorktreeRecord(ctx context.Context, projectID, checkoutKey, branch string) (*storage.WorktreeRecord, error) {
	if g.repos == nil || g.repos.Worktrees == nil {
		return nil, nil
	}
	if strings.TrimSpace(checkoutKey) != "" {
		record, err := g.repos.Worktrees.GetLiveByCheckout(ctx, projectID, checkoutKey)
		if err != nil {
			return nil, fmt.Errorf("get live worktree by checkout: %w", err)
		}
		return record, nil
	}
	record, err := g.repos.Worktrees.GetByBranch(ctx, projectID, branch)
	if err != nil {
		return nil, fmt.Errorf("get live worktree by branch: %w", err)
	}
	return record, nil
}

// DetachedPRWorktreePathCandidates returns every generation directory that
// currently exists on disk for a PR's shared detached checkout, highest
// generation first. Marker probes use this because the generation CreateWorktree
// will claim is not knowable from the filesystem alone, and a marker left by an
// earlier generation is still the provenance the caller needs.
func DetachedPRWorktreePathCandidates(worktreeRoot, projectID string, prNumber int64) []string {
	base := DetachedPRWorktreePath(worktreeRoot, projectID, prNumber, 1)
	if base == "" {
		return nil
	}
	prefix := filepath.Base(base) + storage.WorktreeGenerationSuffix
	type candidate struct {
		path       string
		generation int64
	}
	found := []candidate{{path: base, generation: 1}}
	entries, err := os.ReadDir(worktreeRoot)
	if err == nil {
		for _, entry := range entries {
			name := entry.Name()
			if !strings.HasPrefix(name, prefix) {
				continue
			}
			generation, convErr := strconv.ParseInt(strings.TrimPrefix(name, prefix), 10, 64)
			if convErr != nil || generation < 2 {
				continue
			}
			found = append(found, candidate{path: filepath.Join(worktreeRoot, name), generation: generation})
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].generation > found[j].generation })
	paths := make([]string, 0, len(found))
	for _, entry := range found {
		paths = append(paths, entry.path)
	}
	return paths
}

// LiveGenerationForCheckout resolves the generation the next claim of this
// checkout must use. A live (non-retired) row keeps its own generation;
// otherwise the next claim allocates one past the highest generation ever
// recorded for that checkout. Gateways without storage always report generation
// 1, which is the historical path.
func (g *Gateway) LiveGenerationForCheckout(ctx context.Context, projectID, checkoutKey string) (int64, error) {
	if g.repos == nil || g.repos.Worktrees == nil || strings.TrimSpace(checkoutKey) == "" {
		return 1, nil
	}
	live, err := g.repos.Worktrees.GetLiveByCheckout(ctx, projectID, checkoutKey)
	if err != nil {
		return 0, err
	}
	if live != nil {
		if live.Generation < 1 {
			return 1, nil
		}
		return live.Generation, nil
	}
	return g.repos.Worktrees.NextGenerationForCheckout(ctx, projectID, checkoutKey)
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
