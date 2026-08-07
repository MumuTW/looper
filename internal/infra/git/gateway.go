package git

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/MumuTW/looper/internal/infra/shell"
	"github.com/MumuTW/looper/internal/storage"
	"github.com/MumuTW/looper/internal/worktreesafety"
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
	ProjectID    string
	RepoPath     string
	WorktreeRoot string
	Branch       string
	BaseBranch   string
	// BaseSHA pins creation of a new branch to the caller's observed base
	// commit. When empty, creation retains the ordinary live-base behavior.
	BaseSHA           string
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
	HeadSHA string
	// Branch is the attached branch name, or "HEAD" when detached.
	Branch                string
	NewCommitSHAs         []string
	HasUncommittedChanges bool
	ChangedFiles          []string
	// StagedFiles lists paths with non-space index status (including renames
	// as the destination path only).
	StagedFiles []string
	// UntrackedFiles lists paths with ?? status.
	UntrackedFiles []string
	// DiffFingerprint fingerprints porcelain status codes and paths only (not
	// file contents), so content-only edits of untracked files stay stable.
	DiffFingerprint string
}

type VerifyWorktreeIdentityInput struct {
	RepoPath        string
	WorktreeRoot    string
	WorktreePath    string
	ExpectedBranch  string
	ExpectedHeadSHA string
	CheckoutMode    CheckoutMode
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

type RevertCommitInput struct {
	RepoPath     string
	WorktreeRoot string
	WorktreePath string
	CommitSHA    string
}

// VerifyRevertCommitInput identifies an existing single commit that may have
// been produced by a prior revert attempt. Verification compares the exact
// resulting tree with the inverse of RevertedCommitSHA applied to BaseSHA.
type VerifyRevertCommitInput struct {
	RepoPath          string
	WorktreeRoot      string
	WorktreePath      string
	BaseSHA           string
	ExistingCommitSHA string
	RevertedCommitSHA string
}

var commitSHAPattern = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)

func revertCommitArgs(parentLine string) []string {
	fields := strings.Fields(parentLine)
	args := []string{"revert", "--no-edit", "--no-gpg-sign"}
	if len(fields) > 2 {
		args = append(args, "-m", "1")
	}
	if len(fields) > 0 {
		args = append(args, fields[0])
	}
	return args
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
	RepoPath        string
	WorktreeRoot    string
	WorktreePath    string
	ExpectedBranch  string
	ExpectedHeadSHA string
	CheckoutMode    CheckoutMode
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
			startPoint, err := g.resolveAttachedStartPoint(ctx, input.RepoPath, input.Branch, input.BaseBranch, input.BaseSHA)
			if err != nil {
				return storage.WorktreeRecord{}, err
			}
			args = append(args, "-b", input.Branch, worktreePath, startPoint)
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
		existingRecord, err = g.resolveWorktreeIdentity(ctx, input.ProjectID, input.Branch, worktreePath)
		if err != nil {
			return storage.WorktreeRecord{}, err
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
		MetadataJSON: worktreeRecordMetadata(nil, checkoutMode, false),
		CreatedAt:    valueOr(existingRecordCreatedAt(existingRecord), nowISO),
		UpdatedAt:    nowISO,
		CleanedAt:    nil,
	}
	record.MetadataJSON = worktreeRecordMetadata(record.MetadataJSON, checkoutMode, false)

	if g.repos != nil {
		if err := g.repos.Worktrees.AdoptPath(ctx, record); err != nil {
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
	hasStoredIdentity := false

	if g.repos != nil {
		var stored *storage.WorktreeRecord
		var err error
		nowISO := g.now().UTC().Format(javaScriptISOStringLayout)
		if input.ExpectedWorktreePath != "" {
			stored, err = g.resolveWorktreeIdentity(ctx, input.ProjectID, input.Branch, input.ExpectedWorktreePath)
		}
		if stored == nil && err == nil {
			stored, err = g.resolveWorktreeIdentity(ctx, input.ProjectID, input.Branch, "")
		}
		if err != nil {
			return nil, err
		}
		hasStoredIdentity = stored != nil
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
					restored := *stored
					restored.ProjectID = input.ProjectID
					restored.Branch = input.Branch
					restored.HeadSHA = stringPtrIfNotEmpty(headSHA)
					restored.Status = "active"
					restored.UpdatedAt = nowISO
					restored.CleanedAt = nil
					// Restoring for a new CreateWorktree claim drops prior fixer ownership.
					if err := worktreesafety.ClearFixerOwnerToken(restored.WorktreePath); err != nil {
						return nil, err
					}
					if err := g.repos.Worktrees.AdoptPath(ctx, restored); err != nil {
						return nil, fmt.Errorf("upsert restored worktree record: %w", err)
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
	// Check health before consulting retirement provenance. A vanished path can
	// be pruned and recreated, while a healthy retired checkout may contain
	// uncommitted work from the previous owner and must never be adopted. A
	// healthy checkout with no provenance is instead an interrupted create and
	// is safely recovered below.
	if g.repos != nil && !hasStoredIdentity {
		owner, err := g.repos.Worktrees.GetByPath(ctx, match.Path)
		if err != nil {
			return nil, fmt.Errorf("get worktree retirement provenance: %w", err)
		}
		if owner != nil && owner.Status == "retired" {
			return nil, fmt.Errorf("refusing to adopt retired worktree %q for project %q; remove or register the checkout before retrying", match.Path, input.ProjectID)
		}
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
		MetadataJSON: worktreeRecordMetadata(nil, checkoutMode, true),
		CreatedAt:    nowISO,
		UpdatedAt:    nowISO,
		CleanedAt:    nil,
	}
	if g.repos != nil {
		existing, err := g.resolveWorktreeIdentity(ctx, input.ProjectID, input.Branch, match.Path)
		if err != nil {
			return nil, err
		}
		if existing != nil {
			record = *existing
		}
		record.ProjectID = input.ProjectID
		record.RepoPath = input.RepoPath
		record.WorktreePath = match.Path
		record.Branch = input.Branch
		record.HeadSHA = stringPtrIfNotEmpty(match.HeadSHA)
		record.Status = "active"
		record.UpdatedAt = nowISO
		record.CleanedAt = nil
		if record.CreatedAt == "" {
			record.CreatedAt = nowISO
		}
	}
	record.MetadataJSON = worktreeRecordMetadata(record.MetadataJSON, checkoutMode, true)

	if err := worktreesafety.ClearFixerOwnerToken(record.WorktreePath); err != nil {
		return nil, err
	}
	if g.repos != nil {
		if err := g.repos.Worktrees.AdoptPath(ctx, record); err != nil {
			return nil, fmt.Errorf("upsert discovered worktree record: %w", err)
		}
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
	existing, err := g.repos.Worktrees.GetByPath(ctx, input.WorktreePath)
	if err != nil {
		return fmt.Errorf("provenance check (get worktree by path): %w", err)
	}
	if existing == nil {
		return fmt.Errorf("refusing to remove worktree %q branch %q: no looper worktree record found for this path — worktree was not created by looper", input.WorktreePath, input.Branch)
	}
	if existing.ProjectID != input.ProjectID {
		return fmt.Errorf("refusing to remove worktree %q branch %q: looper record belongs to project %q, not %q", input.WorktreePath, input.Branch, existing.ProjectID, input.ProjectID)
	}
	if existing.Status != "active" {
		return fmt.Errorf("refusing to remove worktree %q branch %q: looper worktree record is %q, not active", input.WorktreePath, input.Branch, existing.Status)
	}
	if existing.Branch != input.Branch {
		return fmt.Errorf("refusing to remove worktree %q branch %q: looper record is currently claimed by branch %q", input.WorktreePath, input.Branch, existing.Branch)
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
	if err := g.VerifyWorktreeIdentity(ctx, VerifyWorktreeIdentityInput{
		RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot, WorktreePath: worktreePath,
		ExpectedBranch: input.ExpectedBranch, ExpectedHeadSHA: input.ExpectedHeadSHA, CheckoutMode: input.CheckoutMode,
	}); err != nil {
		return DiscardWorktreeChangesResult{}, fmt.Errorf("verify worktree identity before discard: %w", err)
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

	if input.ExpectedRemoteHeadSHA != "" {
		isExpectedAncestor, err := g.isAncestor(ctx, input.WorktreePath, input.ExpectedRemoteHeadSHA, sourceRef)
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
			fmt.Sprintf("%s:refs/heads/%s", sourceRef, input.Branch),
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
		return g.setPinnedPushUpstream(ctx, input.WorktreePath, remote, input.Branch, sourceRef)
	}

	if err := g.runGit(ctx, input.WorktreePath, nil, "push", "-u", remote, fmt.Sprintf("%s:refs/heads/%s", sourceRef, input.Branch)); err != nil {
		return err
	}
	return g.setPinnedPushUpstream(ctx, input.WorktreePath, remote, input.Branch, sourceRef)
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
	branch, err := g.getCurrentBranch(ctx, input.WorktreePath)
	if err != nil {
		return InspectHeadResult{}, err
	}
	if branch == "" {
		branch = "HEAD"
	}

	newCommitSHAs := []string{}
	if input.BaseRef != "" {
		newCommitSHAs, err = g.listCommitsSince(ctx, input.WorktreePath, input.BaseRef)
		if err != nil {
			return InspectHeadResult{}, err
		}
	}

	statusResult, err := g.runGitResult(ctx, input.WorktreePath, nil, "status", "--porcelain", "--untracked-files=all", "--ignored=no")
	if err != nil {
		return InspectHeadResult{}, err
	}
	status, err := parseStatusResult(statusResult)
	if err != nil {
		return InspectHeadResult{}, err
	}

	changedFiles := make([]string, 0, len(status))
	stagedFiles := make([]string, 0)
	untrackedFiles := make([]string, 0)
	fingerprintParts := make([]string, 0, len(status))
	for _, entry := range status {
		changedFiles = append(changedFiles, entry.Path)
		fingerprintParts = append(fingerprintParts, entry.Code+"\t"+entry.Path)
		if entry.Code == "??" {
			untrackedFiles = append(untrackedFiles, entry.Path)
			continue
		}
		// Index column non-space means staged (including rename/copy).
		if len(entry.Code) > 0 && entry.Code[0] != ' ' && entry.Code[0] != '?' {
			stagedFiles = append(stagedFiles, entry.Path)
		}
	}
	diffFingerprint := ""
	if len(fingerprintParts) > 0 {
		sum := sha256.Sum256([]byte(strings.Join(fingerprintParts, "\n")))
		diffFingerprint = hex.EncodeToString(sum[:])
	}

	return InspectHeadResult{
		HeadSHA:               headSHA,
		Branch:                branch,
		NewCommitSHAs:         newCommitSHAs,
		HasUncommittedChanges: len(status) > 0,
		ChangedFiles:          changedFiles,
		StagedFiles:           stagedFiles,
		UntrackedFiles:        untrackedFiles,
		DiffFingerprint:       diffFingerprint,
	}, nil
}

// VerifyWorktreeIdentity compares the checkpoint's branch authority with the
// physical checkout. Worktree rows are mutable discovery state and may point
// at a replacement checkout, so they are not sufficient resume authority.
func (g *Gateway) VerifyWorktreeIdentity(ctx context.Context, input VerifyWorktreeIdentityInput) error {
	if err := g.validateMutationWorktree(input.WorktreePath, input.RepoPath, input.WorktreeRoot); err != nil {
		return err
	}
	repoCommon, err := g.gitCommonDir(ctx, input.RepoPath)
	if err != nil {
		return fmt.Errorf("inspect project repository identity: %w", err)
	}
	worktreeCommon, err := g.gitCommonDir(ctx, input.WorktreePath)
	if err != nil {
		return fmt.Errorf("inspect worktree repository identity: %w", err)
	}
	if normalizeComparablePath(repoCommon) != normalizeComparablePath(worktreeCommon) {
		return fmt.Errorf("worktree does not belong to project repository")
	}
	if normalizeCheckoutMode(input.CheckoutMode) == CheckoutModeDetached {
		expectedHeadSHA := strings.TrimSpace(input.ExpectedHeadSHA)
		if expectedHeadSHA == "" {
			return fmt.Errorf("expected detached worktree head is required")
		}
		actualBranch, err := g.getCurrentBranch(ctx, input.WorktreePath)
		if err != nil {
			return fmt.Errorf("inspect current worktree branch: %w", err)
		}
		if actualBranch != "" {
			return fmt.Errorf("worktree branch is %q, expected detached checkout", actualBranch)
		}
		actualHeadSHA, err := g.getHeadSHA(ctx, input.WorktreePath)
		if err != nil {
			return fmt.Errorf("inspect detached worktree head: %w", err)
		}
		if actualHeadSHA != expectedHeadSHA {
			return fmt.Errorf("worktree head is %q, expected %q", actualHeadSHA, expectedHeadSHA)
		}
		return nil
	}
	expectedBranch := strings.TrimSpace(input.ExpectedBranch)
	if expectedBranch == "" {
		return fmt.Errorf("expected worktree branch is required")
	}
	actualBranch, err := g.getCurrentBranch(ctx, input.WorktreePath)
	if err != nil {
		return fmt.Errorf("inspect current worktree branch: %w", err)
	}
	if actualBranch != expectedBranch {
		return fmt.Errorf("worktree branch is %q, expected %q", actualBranch, expectedBranch)
	}
	return nil
}

func worktreeRecordMetadata(raw *string, checkoutMode CheckoutMode, recovered bool) *string {
	metadata := map[string]any{}
	if raw != nil && strings.TrimSpace(*raw) != "" {
		_ = json.Unmarshal([]byte(*raw), &metadata)
	}
	metadata["recovered"] = recovered
	metadata["checkoutMode"] = string(normalizeCheckoutMode(checkoutMode))
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return raw
	}
	return stringPtr(string(encoded))
}

func (g *Gateway) gitCommonDir(ctx context.Context, path string) (string, error) {
	// --path-format was added in Git 2.31. git-common-dir itself is enough for
	// this comparison as long as a relative result is made absolute from the
	// directory in which rev-parse ran.
	result, err := g.runGitResult(ctx, path, nil, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", err
	}
	commonDir := strings.TrimSpace(result.Stdout)
	if commonDir == "" {
		return "", fmt.Errorf("git common directory is empty")
	}
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(path, commonDir)
	}
	abs, err := filepath.Abs(commonDir)
	if err != nil {
		return "", fmt.Errorf("make git common directory absolute: %w", err)
	}
	// Resolve symlinks so a project.RepoPath that is a symlink compares equal
	// to worktrees whose --git-common-dir is the physical path.
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		// The path may not exist yet during prepare; fall back to the absolute form.
		return filepath.Clean(abs), nil
	}
	return filepath.Clean(resolved), nil
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
	if err := g.runGit(ctx, input.WorktreePath, nil, "commit", "--no-gpg-sign", "-m", input.Message); err != nil {
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

// RevertCommit creates one inverse commit in a managed, non-protected
// worktree. A failed revert is aborted before returning so retry never adopts
// conflict markers or an incomplete index.
func (g *Gateway) RevertCommit(ctx context.Context, input RevertCommitInput) (CommitResult, error) {
	if err := g.validateMutationWorktree(input.WorktreePath, input.RepoPath, input.WorktreeRoot); err != nil {
		return CommitResult{}, err
	}
	commitSHA := strings.TrimSpace(input.CommitSHA)
	if !commitSHAPattern.MatchString(commitSHA) {
		return CommitResult{}, fmt.Errorf("revert commit requires a commit SHA")
	}
	parents, err := g.runGitResult(ctx, input.WorktreePath, nil, "rev-list", "--parents", "-n", "1", commitSHA)
	if err != nil {
		return CommitResult{}, err
	}
	// A merge commit needs the default-branch parent selected explicitly;
	// otherwise git refuses before creating the inverse commit.
	revertArgs := revertCommitArgs(strings.TrimSpace(parents.Stdout))
	if err := g.runGit(ctx, input.WorktreePath, nil, revertArgs...); err != nil {
		_ = g.runGit(ctx, input.WorktreePath, nil, "revert", "--abort")
		return CommitResult{}, err
	}
	headSHA, err := g.getHeadSHA(ctx, input.WorktreePath)
	if err != nil {
		return CommitResult{}, err
	}
	if headSHA == "" {
		return CommitResult{}, fmt.Errorf("revert commit did not produce a commit SHA")
	}
	return CommitResult{CommitSHA: headSHA}, nil
}

// VerifyRevertCommit proves that an existing proposal commit is exactly the
// inverse that RevertCommit would create from the frozen base. It uses a
// temporary index, so retry verification never mutates the retained proposal
// worktree or its branch.
func (g *Gateway) VerifyRevertCommit(ctx context.Context, input VerifyRevertCommitInput) error {
	if err := g.validateMutationWorktree(input.WorktreePath, input.RepoPath, input.WorktreeRoot); err != nil {
		return err
	}
	baseSHA := strings.TrimSpace(input.BaseSHA)
	existingSHA := strings.TrimSpace(input.ExistingCommitSHA)
	revertedSHA := strings.TrimSpace(input.RevertedCommitSHA)
	for name, value := range map[string]string{"base SHA": baseSHA, "existing commit SHA": existingSHA, "reverted commit SHA": revertedSHA} {
		if !commitSHAPattern.MatchString(value) {
			return fmt.Errorf("verify revert commit requires a valid %s", name)
		}
	}

	existingParents, err := g.runGitResult(ctx, input.WorktreePath, nil, "rev-list", "--parents", "-n", "1", existingSHA)
	if err != nil {
		return err
	}
	existingFields := strings.Fields(existingParents.Stdout)
	if len(existingFields) != 2 || existingFields[1] != baseSHA {
		return fmt.Errorf("existing revert commit %s is not based directly on frozen base %s", existingSHA, baseSHA)
	}

	revertedParents, err := g.runGitResult(ctx, input.WorktreePath, nil, "rev-list", "--parents", "-n", "1", revertedSHA)
	if err != nil {
		return err
	}
	revertedFields := strings.Fields(revertedParents.Stdout)
	if len(revertedFields) < 2 {
		return fmt.Errorf("reverted commit %s has no parent", revertedSHA)
	}
	diff, err := g.runGitResult(ctx, input.WorktreePath, nil, "diff", "--binary", "--full-index", revertedFields[1], revertedSHA)
	if err != nil {
		return err
	}
	if diff.StdoutTruncated {
		return fmt.Errorf("verify revert commit diff exceeded capture limit")
	}

	verifyRoot, err := os.MkdirTemp(filepath.Dir(input.WorktreePath), ".looper-revert-verify-")
	if err != nil {
		return fmt.Errorf("create revert verification directory: %w", err)
	}
	defer os.RemoveAll(verifyRoot)
	patchPath := filepath.Join(verifyRoot, "inverse.patch")
	if err := os.WriteFile(patchPath, []byte(diff.Stdout), 0o600); err != nil {
		return fmt.Errorf("write revert verification patch: %w", err)
	}
	indexPath := filepath.Join(verifyRoot, "index")
	env := map[string]string{"GIT_INDEX_FILE": indexPath}
	if err := g.runGit(ctx, input.WorktreePath, env, "read-tree", baseSHA); err != nil {
		return fmt.Errorf("load frozen base for revert verification: %w", err)
	}
	if strings.TrimSpace(diff.Stdout) != "" {
		if err := g.runGit(ctx, input.WorktreePath, env, "apply", "--cached", "--reverse", patchPath); err != nil {
			return fmt.Errorf("apply expected inverse for revert verification: %w", err)
		}
	}
	expectedTree, err := g.runGitResult(ctx, input.WorktreePath, env, "write-tree")
	if err != nil {
		return fmt.Errorf("write expected revert tree: %w", err)
	}
	actualTree, err := g.runGitResult(ctx, input.WorktreePath, nil, "rev-parse", existingSHA+"^{tree}")
	if err != nil {
		return err
	}
	if strings.TrimSpace(expectedTree.Stdout) != strings.TrimSpace(actualTree.Stdout) {
		return fmt.Errorf("existing revert commit %s does not match the exact inverse of %s from base %s", existingSHA, revertedSHA, baseSHA)
	}
	return nil
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

func (g *Gateway) resolveAttachedStartPoint(ctx context.Context, repoPath, branch, baseBranch, baseSHA string) (string, error) {
	startPoint, ok, err := g.resolveDetachedStartPointRef(ctx, repoPath, branch)
	if err != nil {
		return "", err
	}
	if ok {
		return startPoint, nil
	}
	if baseSHA = strings.TrimSpace(baseSHA); baseSHA != "" {
		if !commitSHAPattern.MatchString(baseSHA) {
			return "", fmt.Errorf("resolve attached start point: invalid base SHA %q", baseSHA)
		}
		if err := g.ensureRevisionAvailable(ctx, repoPath, baseBranch, baseSHA); err != nil {
			return "", err
		}
		return baseSHA, nil
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

func (g *Gateway) ensureRevisionAvailable(ctx context.Context, repoPath, baseBranch, baseSHA string) error {
	if _, err := g.getRevision(ctx, repoPath, baseSHA+"^{commit}"); err == nil {
		return nil
	}
	if strings.TrimSpace(baseBranch) == "" {
		return fmt.Errorf("resolve attached start point: base SHA %s is not available locally and base branch is empty", baseSHA)
	}
	if err := g.runGit(ctx, repoPath, nil, "fetch", "origin", fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", baseBranch, baseBranch)); err != nil {
		return fmt.Errorf("fetch base branch for pinned SHA %s: %w", baseSHA, err)
	}
	if _, err := g.getRevision(ctx, repoPath, baseSHA+"^{commit}"); err != nil {
		return fmt.Errorf("resolve attached start point: pinned base SHA %s is unavailable after fetching %s: %w", baseSHA, baseBranch, err)
	}
	return nil
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

// resolveWorktreeIdentity picks the durable worktree row that Create/Restore
// should reuse, enforcing path ownership. Path adoption itself is deferred to
// Worktrees.AdoptPath so branch retirement and the adopting upsert are atomic.
//
// Path is the filesystem authority: a row already on this path is preferred so
// reviewer/fixer can share one detached checkout under different logical
// branch labels.
func (g *Gateway) resolveWorktreeIdentity(ctx context.Context, projectID, branch, worktreePath string) (*storage.WorktreeRecord, error) {
	if g.repos == nil {
		return nil, nil
	}

	var (
		pathRecord   *storage.WorktreeRecord
		branchRecord *storage.WorktreeRecord
		err          error
	)
	if worktreePath != "" {
		pathRecord, err = g.repos.Worktrees.GetByPath(ctx, worktreePath)
		if err != nil {
			return nil, fmt.Errorf("get existing worktree by path: %w", err)
		}
		if pathRecord != nil && pathRecord.ProjectID != "" && pathRecord.ProjectID != projectID {
			return nil, fmt.Errorf("refusing worktree path %q: looper record belongs to project %q, not %q", worktreePath, pathRecord.ProjectID, projectID)
		}
		if pathRecord != nil && pathRecord.Status == "retired" {
			pathRecord = nil
		}
	}
	if projectID != "" && branch != "" {
		branchRecord, err = g.repos.Worktrees.GetByBranch(ctx, projectID, branch)
		if err != nil {
			return nil, fmt.Errorf("get existing worktree by branch: %w", err)
		}
		if branchRecord != nil && branchRecord.Status == "retired" {
			branchRecord = nil
		}
	}

	// Prefer the physical path identity. A branch collision is resolved together
	// with the final durable adoption, after all restore checks have succeeded.
	if pathRecord != nil {
		return pathRecord, nil
	}
	return branchRecord, nil
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
	return parseStatusResult(result)
}

// parseStatusResult parses porcelain status output. Truncated captures fail
// closed so timeout-progress fingerprints cannot silently drop dirt.
func parseStatusResult(result shell.Result) ([]statusEntry, error) {
	if result.StdoutTruncated {
		return nil, fmt.Errorf("git status output exceeded capture limit")
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
		// Renames/copies: "R  old -> new" — keep the destination path only.
		if idx := strings.Index(path, " -> "); idx >= 0 {
			path = strings.TrimSpace(path[idx+4:])
		}
		// Quoted paths from git for special characters.
		if len(path) >= 2 && path[0] == '"' && path[len(path)-1] == '"' {
			if unquoted, err := strconv.Unquote(path); err == nil {
				path = unquoted
			}
		}
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

func normalizeComparablePath(path string) string {
	return normalizeComparablePathForOS(path, runtime.GOOS)
}

func normalizeComparablePathForOS(path, goos string) string {
	if path == "" {
		return ""
	}
	resolved, err := filepath.Abs(path)
	if err != nil {
		resolved = path
	}
	resolved = filepath.ToSlash(filepath.Clean(resolved))
	if goos == "darwin" && strings.HasPrefix(resolved, "/private/") {
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
