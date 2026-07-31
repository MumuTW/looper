package projects

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nexu-io/looper/internal/bootstrap"
	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/eventlog"
	"github.com/nexu-io/looper/internal/storage"
)

const legacyProjectIDPrefix = "legacy-id-"

var nonProjectIDPattern = regexp.MustCompile(`[^a-z0-9]+`)

type DetectedRepo struct {
	Repo     string
	Provider string
}

type DetectRepoFunc func(context.Context, string) (DetectedRepo, error)

type ListWorktreesFunc func(context.Context, string) ([]WorktreeListEntry, error)

type ListOpenPullRequestsFunc func(context.Context, ListOpenPullRequestsInput) ([]PullRequestSummary, error)

type CapturePullRequestSnapshotFunc func(context.Context, CapturePullRequestSnapshotInput) (storage.PullRequestSnapshotRecord, error)

type SnapshotMode string

const (
	SnapshotModeAsync SnapshotMode = "async"
	SnapshotModeFull  SnapshotMode = "full"
	SnapshotModeOff   SnapshotMode = "off"
	queueTypeSnapshot              = "snapshot"
)

type WorktreeListEntry struct {
	Path    string
	Branch  string
	HeadSHA string
	Bare    bool
}

type ListOpenPullRequestsInput struct {
	Repo    string
	CWD     string
	Limit   int
	Timeout time.Duration
}

type PullRequestSummary struct {
	Number  int64
	State   string
	IsDraft bool
}

type CapturePullRequestSnapshotInput struct {
	ProjectID  string
	Repo       string
	PRNumber   int64
	CWD        string
	CapturedAt string
}

type DiscoveryStatus string

const (
	DiscoveryStatusPending   DiscoveryStatus = "pending"
	DiscoveryStatusRunning   DiscoveryStatus = "running"
	DiscoveryStatusSucceeded DiscoveryStatus = "succeeded"
	DiscoveryStatusFailed    DiscoveryStatus = "failed"

	registrationDiscoveryMetadataKey = "registrationDiscovery"
)

// DiscoveryState is the observable post-commit worktree/PR discovery contract.
// It is stored on the Project metadata and never gates registration success.
type DiscoveryState struct {
	Status                 DiscoveryStatus `json:"status"`
	SnapshotMode           SnapshotMode    `json:"snapshotMode,omitempty"`
	UpdatedAt              string          `json:"updatedAt,omitempty"`
	Error                  string          `json:"error,omitempty"`
	DiscoveredPullRequests int             `json:"discoveredPullRequests,omitempty"`
	DiscoveredWorktrees    int             `json:"discoveredWorktrees,omitempty"`
	PendingSnapshots       int             `json:"pendingSnapshots,omitempty"`
	CapturedSnapshots      int             `json:"capturedSnapshots,omitempty"`
	Warnings               []string        `json:"warnings,omitempty"`
}

type Service struct {
	mutationMu     sync.Mutex
	projectLocksMu sync.Mutex
	projectLocks   map[string]*projectOperationLock
	DB             *sql.DB
	Repos          *storage.Repositories
	Logger         bootstrap.Logger
	Config         config.Config
	ConfigSource   ConfigSource
	// ConfigBoundary serializes project materialization/commit/publication with
	// global config validation/publication. It prevents a valid mutation on each
	// side from combining into an invalid catalog snapshot.
	ConfigBoundary             *sync.RWMutex
	Now                        func() time.Time
	DetectRepo                 DetectRepoFunc
	GetRepositorySettings      GetRepositorySettingsFunc
	GetBranchProtection        GetBranchProtectionFunc
	ListWorktrees              ListWorktreesFunc
	ListOpenPullRequests       ListOpenPullRequestsFunc
	CapturePullRequestSnapshot CapturePullRequestSnapshotFunc
	AsyncSnapshotQueueEnabled  func() bool
	PublishProjects            func([]config.ProjectRefConfig)
	// AfterPublishProjects runs after ConfigBoundary is released. Keep any
	// external reconciliation or other potentially blocking follow-up here;
	// PublishProjects itself is part of the atomic config/catalog publication.
	AfterPublishProjects func()
	// ScheduleDiscovery runs post-commit discovery under an explicit lifecycle
	// owner. Without it, registration leaves discovery pending for an explicit
	// DiscoverProject call rather than starting an unowned goroutine. Tests may
	// replace it to run inline or to suppress automatic discovery.
	ScheduleDiscovery func(func())
	// DiscoveryContext supplies the lifecycle context for post-commit work. The
	// default is context.Background for standalone Service users; runtimes inject
	// a cancelable context and own the corresponding goroutine drain.
	DiscoveryContext func() context.Context
}

type projectOperationLock struct {
	token chan struct{}
	refs  int
}

func newProjectOperationLock() *projectOperationLock {
	lock := &projectOperationLock{token: make(chan struct{}, 1)}
	lock.token <- struct{}{}
	return lock
}

// lockProjectOperations serializes discovery with mutations for the supplied
// Projects. The locks are process-local coordination only; SQLite remains the
// Project authority.
func (s *Service) lockProjectOperations(ctx context.Context, ids ...string) (func(), error) {
	unique := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			unique[id] = struct{}{}
		}
	}
	if len(unique) == 0 {
		return func() {}, nil
	}

	orderedIDs := make([]string, 0, len(unique))
	for id := range unique {
		orderedIDs = append(orderedIDs, id)
	}
	sort.Strings(orderedIDs)

	s.projectLocksMu.Lock()
	if s.projectLocks == nil {
		s.projectLocks = make(map[string]*projectOperationLock)
	}
	locks := make([]*projectOperationLock, 0, len(orderedIDs))
	for _, id := range orderedIDs {
		lock := s.projectLocks[id]
		if lock == nil {
			lock = newProjectOperationLock()
			s.projectLocks[id] = lock
		}
		lock.refs++
		locks = append(locks, lock)
	}
	s.projectLocksMu.Unlock()

	releaseReferences := func() {
		s.projectLocksMu.Lock()
		defer s.projectLocksMu.Unlock()
		for index, id := range orderedIDs {
			lock := locks[index]
			lock.refs--
			if lock.refs == 0 && s.projectLocks[id] == lock {
				delete(s.projectLocks, id)
			}
		}
	}

	for index, lock := range locks {
		select {
		case <-ctx.Done():
			for releaseIndex := index - 1; releaseIndex >= 0; releaseIndex-- {
				locks[releaseIndex].token <- struct{}{}
			}
			releaseReferences()
			return nil, ctx.Err()
		case <-lock.token:
		}
	}

	return func() {
		for index := len(locks) - 1; index >= 0; index-- {
			locks[index].token <- struct{}{}
		}
		releaseReferences()
	}, nil
}

type AddInput struct {
	ID           string
	Name         string
	RepoPath     string
	BaseBranch   string
	IDSource     string
	WorktreeRoot *string
	Repo         *string
	Provider     *string
	Validation   *config.ProjectValidationConfig
	SnapshotMode SnapshotMode
}

// UpdateStringField distinguishes an omitted patch field from an explicit JSON
// null. Set=false preserves the stored value; Set=true with Value=nil clears a
// nullable field. Keep this separate from AddInput because creation defaults
// are not repair defaults.
type UpdateStringField struct {
	Set   bool
	Value *string
}

type UpdateInput struct {
	Repo         UpdateStringField
	Name         UpdateStringField
	BaseBranch   UpdateStringField
	WorktreeRoot UpdateStringField
	Validation   *config.ProjectValidationConfig
}

type AddResult struct {
	Project  storage.ProjectRecord
	Repo     *string
	Provider *string
	// Discovery is pending when registration returns; progress is observable on
	// the Project record and via DiscoverProject retries.
	Discovery DiscoveryState
	Warnings  []string
}

type DiscoverInput struct {
	ProjectID    string
	SnapshotMode SnapshotMode
}

type DiscoverResult struct {
	Project   storage.ProjectRecord
	Discovery DiscoveryState
}

type ProjectIDCollisionError struct{ ProjectID string }

func (e ProjectIDCollisionError) Error() string {
	return fmt.Sprintf("Derived project id collides with an existing explicit project: %s", e.ProjectID)
}

type ProjectNotFoundError struct{ Identifier string }

func (e ProjectNotFoundError) Error() string {
	return fmt.Sprintf("project not found: %s", e.Identifier)
}

type AmbiguousProjectIdentifierError struct{ Identifier string }

func (e AmbiguousProjectIdentifierError) Error() string {
	return fmt.Sprintf("project identifier matches multiple projects: %s", e.Identifier)
}

type ProjectValidationError struct{ Message string }

func (e ProjectValidationError) Error() string { return e.Message }

func (s *Service) AddProject(ctx context.Context, input AddInput) (AddResult, error) {
	if s.Repos == nil || s.Repos.Projects == nil {
		return AddResult{}, fmt.Errorf("projects repository is not configured")
	}
	s.mutationMu.Lock()
	unlockProjects, lockErr := s.lockProjectOperations(ctx, input.ID, normalizeProjectID(input))
	if lockErr != nil {
		s.mutationMu.Unlock()
		return AddResult{}, lockErr
	}
	result, discoverInput, err := s.addProjectLocked(ctx, input)
	unlockProjects()
	s.mutationMu.Unlock()
	if err != nil {
		return AddResult{}, err
	}
	if discoverInput != nil {
		// Schedule only after releasing the mutation and project locks so a
		// synchronous test runner can call DiscoverProject without deadlocking.
		s.scheduleDiscovery(*discoverInput)
	}
	return result, nil
}

func (s *Service) addProjectLocked(ctx context.Context, input AddInput) (AddResult, *DiscoverInput, error) {
	existing, err := s.Repos.Projects.GetByID(ctx, input.ID)
	if err != nil {
		return AddResult{}, nil, err
	}
	projectID := input.ID
	if existing == nil {
		projectID = normalizeProjectID(input)
	}
	if existing == nil && projectID != input.ID {
		normalizedExisting, err := s.Repos.Projects.GetByID(ctx, projectID)
		if err != nil {
			return AddResult{}, nil, err
		}
		if normalizedExisting != nil {
			metadata := parseMetadata(normalizedExisting.MetadataJSON)
			if normalized, _ := metadata["normalizedDerivedId"].(bool); !normalized {
				return AddResult{}, nil, ProjectIDCollisionError{ProjectID: projectID}
			}
			existing = normalizedExisting
		}
	}
	if existing != nil && metadataString(parseMetadata(existing.MetadataJSON), "source") == "config" {
		return AddResult{}, nil, ProjectValidationError{Message: fmt.Sprintf("project %s is managed by config and cannot be changed through the project API", existing.ID)}
	}
	if existing != nil && !existing.Archived {
		// Re-registration is the only non-destructive repair path for an API
		// project with missing repository metadata. The stable authority is the
		// existing project ID and its checkout, not whether the caller supplied
		// that ID or the API derived it from the path.
		if !sameProjectRepoPath(existing.RepoPath, input.RepoPath) {
			return AddResult{}, nil, ProjectIDCollisionError{ProjectID: projectID}
		}
	}

	if existing == nil {
		if err := assertValidProjectID(projectID); err != nil {
			return AddResult{}, nil, err
		}
	}

	cfg := s.currentConfig()
	repo := input.Repo
	provider := normalizeOptionalProvider(input.Provider)
	warnings := []string{}
	if (repo == nil || provider == nil) && s.DetectRepo != nil {
		detected, detectErr := s.DetectRepo(ctx, input.RepoPath)
		if detectErr != nil {
			warnings = append(warnings, fmt.Sprintf("Could not detect repository from git remote: %s", detectErr.Error()))
		} else {
			if repo == nil && strings.TrimSpace(detected.Repo) != "" {
				if provider != nil && strings.TrimSpace(detected.Provider) != strings.TrimSpace(*provider) {
					detectedProvider := strings.TrimSpace(detected.Provider)
					if detectedProvider == "" {
						detectedProvider = "the GitHub default"
					}
					return AddResult{}, nil, ProjectValidationError{Message: fmt.Sprintf("detected origin belongs to %s, not provider %q; pass --repo owner/name explicitly or use a checkout whose origin matches the provider", detectedProvider, strings.TrimSpace(*provider))}
				}
				value := strings.TrimSpace(detected.Repo)
				repo = &value
				if provider == nil && strings.TrimSpace(detected.Provider) != "" {
					return AddResult{}, nil, ProjectValidationError{Message: fmt.Sprintf("non-GitHub origin matches provider %q; rerun with --provider %s to confirm the binding", strings.TrimSpace(detected.Provider), strings.TrimSpace(detected.Provider))}
				}
			}
		}
	}
	if err := validateExplicitProvider(cfg, provider); err != nil {
		return AddResult{}, nil, err
	}
	if provider != nil && (repo == nil || strings.TrimSpace(*repo) == "") {
		return AddResult{}, nil, ProjectValidationError{Message: "provider is set but repo is missing; pass --repo owner/name or use a checkout with a detectable origin remote"}
	}
	if repo == nil || strings.TrimSpace(*repo) == "" {
		// Registering without a repository is allowed, but it produces a project
		// no role can act on: every discovery lane skips a project whose repo
		// metadata is empty. Say so at registration instead of leaving the
		// operator to infer it from a daemon log line on every tick.
		// PATCH is the repair, not remove-and-recreate or re-registration:
		// RemoveProject terminates every loop, while AddProject applies creation
		// defaults to omitted fields. The update contract preserves omitted state.
		warnings = append(warnings, fmt.Sprintf(`No repository is set for this project, so no automation will run for it. Set one with PATCH /api/v1/projects/%s and body {"repo":"owner/name"}; unlike creation defaults, omitted name, baseBranch, and snapshotMode are preserved, as are existing loops. The CLI cannot set a repository.`, projectID))
	}

	if err := s.validateReviewerAutoMergeForProject(ctx, projectID, repo, input.BaseBranch, cfg); err != nil {
		return AddResult{}, nil, err
	}

	nowISO := currentISO(s.Now)
	metadata := parseMetadata(nil)
	if existing != nil {
		metadata = parseMetadata(existing.MetadataJSON)
	}
	derivedProjectID := deriveProjectIDFromRepoPath(input.RepoPath)
	normalizedDerivedID := false
	if normalized, _ := metadata["normalizedDerivedId"].(bool); normalized {
		normalizedDerivedID = true
	}
	if input.IDSource == "derived" && strings.HasPrefix(derivedProjectID, legacyProjectIDPrefix) && input.ID == normalizeDerivedProjectID(derivedProjectID) {
		normalizedDerivedID = true
	}
	metadata["repo"] = nil
	if repo != nil {
		metadata["repo"] = *repo
	}
	if provider != nil {
		metadata["provider"] = *provider
	} else {
		delete(metadata, "provider")
	}
	if input.Validation != nil {
		metadata["validation"] = config.ProjectValidationConfig{
			Commands: append([]string(nil), input.Validation.Commands...),
			OptOut:   input.Validation.OptOut,
		}
	} else if existing == nil {
		delete(metadata, "validation")
	}
	delete(metadata, "roles")
	if input.WorktreeRoot != nil {
		metadata["worktreeRoot"] = *input.WorktreeRoot
	} else if _, ok := metadata["worktreeRoot"]; !ok {
		metadata["worktreeRoot"] = nil
	}
	if normalizedDerivedID {
		metadata["normalizedDerivedId"] = true
	}
	if existing != nil {
		if _, ok := metadata["source"]; !ok {
			metadata["source"] = "api"
		}
	} else {
		metadata["source"] = "api"
	}
	snapshotMode := snapshotModeOrDefault(input.SnapshotMode)
	discovery := DiscoveryState{
		Status:       DiscoveryStatusPending,
		SnapshotMode: snapshotMode,
		UpdatedAt:    nowISO,
	}
	metadata[registrationDiscoveryMetadataKey] = discoveryStateMap(discovery)
	metadataJSON, err := buildAddProjectMetadataJSON(metadata)
	if err != nil {
		return AddResult{}, nil, fmt.Errorf("marshal project metadata: %w", err)
	}

	record := storage.ProjectRecord{
		ID:           projectID,
		Name:         input.Name,
		RepoPath:     input.RepoPath,
		BaseBranch:   stringPointer(input.BaseBranch),
		Archived:     false,
		MetadataJSON: stringPointer(metadataJSON),
		CreatedAt:    nowISO,
		UpdatedAt:    nowISO,
	}
	if existing != nil {
		record.CreatedAt = existing.CreatedAt
	}
	var nextProjects []config.ProjectRefConfig
	publishedProjects := false
	err = s.withConfigBoundary(func() error {
		var materializeErr error
		nextProjects, materializeErr = s.materializeCandidate(ctx, &record, "")
		if materializeErr != nil {
			return ProjectValidationError{Message: materializeErr.Error()}
		}
		if upsertErr := s.Repos.Projects.Upsert(ctx, record); upsertErr != nil {
			return upsertErr
		}
		if s.PublishProjects != nil {
			s.PublishProjects(nextProjects)
			publishedProjects = true
		}
		return nil
	})
	if err != nil {
		return AddResult{}, nil, err
	}
	if publishedProjects && s.AfterPublishProjects != nil {
		s.AfterPublishProjects()
	}

	job := DiscoverInput{ProjectID: projectID, SnapshotMode: snapshotMode}
	return AddResult{
		Project:   record,
		Repo:      repo,
		Provider:  provider,
		Discovery: discovery,
		Warnings:  warnings,
	}, &job, nil
}

// DiscoverProject runs (or retries) post-commit worktree/PR discovery for an
// already-registered Project. It is idempotent for worktree upserts and
// snapshot queue dedupe, and never archives or unpublishes the Project.
func (s *Service) DiscoverProject(ctx context.Context, input DiscoverInput) (DiscoverResult, error) {
	if s.Repos == nil || s.Repos.Projects == nil {
		return DiscoverResult{}, fmt.Errorf("projects repository is not configured")
	}

	projectID := strings.TrimSpace(input.ProjectID)
	if projectID == "" {
		return DiscoverResult{}, ProjectValidationError{Message: "project id is required"}
	}
	unlockProject, lockErr := s.lockProjectOperations(ctx, projectID)
	if lockErr != nil {
		return DiscoverResult{}, lockErr
	}
	defer unlockProject()

	project, err := s.Repos.Projects.GetByID(ctx, projectID)
	if err != nil {
		return DiscoverResult{}, err
	}
	if project == nil || project.Archived {
		return DiscoverResult{}, ProjectNotFoundError{Identifier: projectID}
	}

	existingDiscovery := discoveryStateFromMetadata(parseMetadata(project.MetadataJSON))
	snapshotMode := existingDiscovery.SnapshotMode
	if input.SnapshotMode != "" {
		snapshotMode = input.SnapshotMode
	}
	snapshotMode = snapshotModeOrDefault(snapshotMode)

	nowISO := currentISO(s.Now)
	running := DiscoveryState{Status: DiscoveryStatusRunning, SnapshotMode: snapshotMode, UpdatedAt: nowISO}
	if err := s.writeDiscoveryState(ctx, project, running); err != nil {
		return DiscoverResult{}, err
	}

	warnings := []string{}
	discoveredWorktrees, worktreeErr := s.discoverWorktrees(ctx, *project, nowISO, &warnings)
	var discoveredPullRequests, pendingSnapshots, capturedSnapshots int
	var pullRequestErr error
	repo := stringMetadataPtr(project.MetadataJSON, "repo")
	discoveredPullRequests, pendingSnapshots, capturedSnapshots, pullRequestErr = s.discoverPullRequests(ctx, *project, repo, snapshotMode, &warnings)

	discovery := DiscoveryState{
		Status:                 DiscoveryStatusSucceeded,
		SnapshotMode:           snapshotMode,
		UpdatedAt:              currentISO(s.Now),
		DiscoveredPullRequests: discoveredPullRequests,
		DiscoveredWorktrees:    discoveredWorktrees,
		PendingSnapshots:       pendingSnapshots,
		CapturedSnapshots:      capturedSnapshots,
		Warnings:               append([]string{}, warnings...),
	}
	if worktreeErr != nil || pullRequestErr != nil {
		if discoveryCanceled(worktreeErr, pullRequestErr) {
			discovery.Status = DiscoveryStatusPending
		} else {
			discovery.Status = DiscoveryStatusFailed
			discovery.Error = firstErrorMessage(worktreeErr, pullRequestErr)
		}
		if writeErr := s.writeDiscoveryState(ctx, project, discovery); writeErr != nil {
			return DiscoverResult{}, writeErr
		}
		if worktreeErr != nil {
			return DiscoverResult{Project: *project, Discovery: discovery}, worktreeErr
		}
		return DiscoverResult{Project: *project, Discovery: discovery}, pullRequestErr
	}
	if err := s.writeDiscoveryState(ctx, project, discovery); err != nil {
		return DiscoverResult{}, err
	}
	return DiscoverResult{Project: *project, Discovery: discovery}, nil
}

func (s *Service) Get(ctx context.Context, id string) (*storage.ProjectRecord, error) {
	if s.Repos == nil || s.Repos.Projects == nil {
		return nil, fmt.Errorf("projects repository is not configured")
	}
	return s.Repos.Projects.GetByID(ctx, id)
}

func (s *Service) List(ctx context.Context) ([]storage.ProjectRecord, error) {
	if s.Repos == nil || s.Repos.Projects == nil {
		return nil, fmt.Errorf("projects repository is not configured")
	}
	items, err := s.Repos.Projects.List(ctx)
	if err != nil {
		return nil, err
	}
	active := make([]storage.ProjectRecord, 0, len(items))
	for _, item := range items {
		if !item.Archived {
			active = append(active, item)
		}
	}
	return active, nil
}

// ResumeIncompleteDiscoveries reschedules work whose persisted pending or running
// state outlived its process. The runtime calls this once after startup has
// materialized the project catalog and installed its discovery lifecycle.
func (s *Service) ResumeIncompleteDiscoveries(ctx context.Context) error {
	if s == nil || s.Repos == nil || s.Repos.Projects == nil {
		return fmt.Errorf("projects repository is not configured")
	}
	records, err := s.Repos.Projects.List(ctx)
	if err != nil {
		return err
	}
	inputs := make([]DiscoverInput, 0)
	for _, record := range records {
		if record.Archived {
			continue
		}
		discovery := DiscoveryStateFromRecord(record)
		if discovery.Status == DiscoveryStatusRunning || discovery.Status == DiscoveryStatusPending {
			inputs = append(inputs, DiscoverInput{ProjectID: record.ID, SnapshotMode: discovery.SnapshotMode})
		}
	}
	// Resume the persisted backlog through one lifecycle-owned job. This keeps
	// restart fan-out bounded without adding another worker pool or persisted
	// queue; newly registered projects retain their existing async behavior.
	s.scheduleDiscoveries(inputs)
	return nil
}

// UpdateProject repairs an API-managed project without applying creation
// defaults or disturbing its loops and queue items. The database record is the
// authority; the catalog is materialized and published from that replacement
// in the same config boundary used by registration.
func (s *Service) UpdateProject(ctx context.Context, identifier string, input UpdateInput) (storage.ProjectRecord, error) {
	if s.Repos == nil || s.Repos.Projects == nil {
		return storage.ProjectRecord{}, fmt.Errorf("projects repository is not configured")
	}
	if !input.Repo.Set && !input.Name.Set && !input.BaseBranch.Set && !input.WorktreeRoot.Set && input.Validation == nil {
		return storage.ProjectRecord{}, ProjectValidationError{Message: "at least one project field is required"}
	}

	s.mutationMu.Lock()
	defer s.mutationMu.Unlock()
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return storage.ProjectRecord{}, ProjectValidationError{Message: "project identifier is required"}
	}
	project, err := s.resolveActiveProjectForRemoval(ctx, identifier)
	if err != nil {
		return storage.ProjectRecord{}, err
	}
	if project == nil {
		return storage.ProjectRecord{}, ProjectNotFoundError{Identifier: identifier}
	}

	unlockProject, err := s.lockProjectOperations(ctx, project.ID)
	if err != nil {
		return storage.ProjectRecord{}, err
	}
	defer unlockProject()
	// Discovery may have committed metadata while this update waited for its
	// operation lock. Re-read before constructing a whole-record replacement.
	project, err = s.Repos.Projects.GetByID(ctx, project.ID)
	if err != nil {
		return storage.ProjectRecord{}, err
	}
	if project == nil || project.Archived {
		return storage.ProjectRecord{}, ProjectNotFoundError{Identifier: identifier}
	}
	metadata := parseMetadata(project.MetadataJSON)
	if metadataString(metadata, "source") == "config" {
		return storage.ProjectRecord{}, ProjectValidationError{Message: fmt.Sprintf("project %s is managed by config and cannot be changed through the project API", project.ID)}
	}

	updated := *project
	previousRepo := metadataString(metadata, "repo")
	repoChanged := false
	if input.Name.Set {
		if input.Name.Value == nil || strings.TrimSpace(*input.Name.Value) == "" {
			return storage.ProjectRecord{}, ProjectValidationError{Message: "name must not be empty when provided"}
		}
		updated.Name = strings.TrimSpace(*input.Name.Value)
	}
	if input.BaseBranch.Set {
		if input.BaseBranch.Value == nil {
			updated.BaseBranch = nil
		} else if branch := strings.TrimSpace(*input.BaseBranch.Value); branch == "" {
			return storage.ProjectRecord{}, ProjectValidationError{Message: "baseBranch must not be empty when provided"}
		} else {
			updated.BaseBranch = stringPointer(branch)
		}
	}
	if input.Repo.Set {
		if input.Repo.Value == nil {
			metadata["repo"] = nil
			repoChanged = previousRepo != ""
		} else {
			repo := strings.TrimSpace(*input.Repo.Value)
			parts := strings.Split(repo, "/")
			if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
				return storage.ProjectRecord{}, ProjectValidationError{Message: "repo must be owner/name"}
			}
			metadata["repo"] = repo
			repoChanged = previousRepo != repo
		}
	}
	if input.WorktreeRoot.Set {
		if input.WorktreeRoot.Value == nil || strings.TrimSpace(*input.WorktreeRoot.Value) == "" {
			metadata["worktreeRoot"] = nil
		} else {
			metadata["worktreeRoot"] = strings.TrimSpace(*input.WorktreeRoot.Value)
		}
	}
	if input.Validation != nil {
		metadata["validation"] = config.ProjectValidationConfig{
			Commands: append([]string(nil), input.Validation.Commands...),
			OptOut:   input.Validation.OptOut,
		}
	}

	var repo *string
	if value := metadataString(metadata, "repo"); value != "" {
		repo = stringPointer(value)
	}
	baseBranch := s.currentConfig().Defaults.BaseBranch
	if updated.BaseBranch != nil && strings.TrimSpace(*updated.BaseBranch) != "" {
		baseBranch = *updated.BaseBranch
	}
	if err := s.validateReviewerAutoMergeForProject(ctx, updated.ID, repo, baseBranch, s.currentConfig()); err != nil {
		return storage.ProjectRecord{}, err
	}
	if repoChanged {
		discovery := discoveryStateFromMetadata(metadata)
		discovery.Status = DiscoveryStatusPending
		discovery.Error = ""
		discovery.UpdatedAt = currentISO(s.Now)
		if discovery.SnapshotMode == "" {
			discovery.SnapshotMode = SnapshotModeAsync
		}
		metadata[registrationDiscoveryMetadataKey] = discoveryStateMap(discovery)
	}
	metadataJSON, err := buildAddProjectMetadataJSON(metadata)
	if err != nil {
		return storage.ProjectRecord{}, fmt.Errorf("marshal project metadata: %w", err)
	}
	updated.MetadataJSON = stringPointer(metadataJSON)
	updated.UpdatedAt = currentISO(s.Now)

	var published bool
	err = s.withConfigBoundary(func() error {
		next, materializeErr := s.materializeCandidate(ctx, &updated, "")
		if materializeErr != nil {
			return ProjectValidationError{Message: materializeErr.Error()}
		}
		if upsertErr := s.Repos.Projects.Upsert(ctx, updated); upsertErr != nil {
			return upsertErr
		}
		if s.PublishProjects != nil {
			s.PublishProjects(next)
			published = true
		}
		return nil
	})
	if err != nil {
		return storage.ProjectRecord{}, err
	}
	if published && s.AfterPublishProjects != nil {
		s.AfterPublishProjects()
	}
	if repoChanged {
		s.scheduleDiscovery(DiscoverInput{ProjectID: updated.ID, SnapshotMode: discoveryStateFromMetadata(metadata).SnapshotMode})
	}
	return updated, nil
}

func (s *Service) RemoveProject(ctx context.Context, identifier string) (storage.ProjectRecord, error) {
	if s.Repos == nil || s.Repos.Projects == nil {
		return storage.ProjectRecord{}, fmt.Errorf("projects repository is not configured")
	}
	s.mutationMu.Lock()
	defer s.mutationMu.Unlock()

	trimmed := strings.TrimSpace(identifier)
	if trimmed == "" {
		return storage.ProjectRecord{}, ProjectValidationError{Message: "project identifier is required"}
	}

	project, err := s.resolveActiveProjectForRemoval(ctx, trimmed)
	if err != nil {
		return storage.ProjectRecord{}, err
	}
	if project == nil {
		return storage.ProjectRecord{}, ProjectNotFoundError{Identifier: trimmed}
	}
	unlockProject, lockErr := s.lockProjectOperations(ctx, project.ID)
	if lockErr != nil {
		return storage.ProjectRecord{}, lockErr
	}
	defer unlockProject()
	if source, _ := parseMetadata(project.MetadataJSON)["source"].(string); source == "config" {
		return storage.ProjectRecord{}, ProjectValidationError{Message: fmt.Sprintf("project %s is managed by config and cannot be removed from the CLI", project.ID)}
	}

	nowISO := currentISO(s.Now)
	cancelReason := "project archived"
	var archived bool
	publishedProjects := false
	err = s.withConfigBoundary(func() error {
		nextProjects, materializeErr := s.materializeCandidate(ctx, nil, project.ID)
		if materializeErr != nil {
			return materializeErr
		}
		var transactionErr error
		archived, transactionErr = storage.WithTransactionValue(ctx, s.DB, nil, func(tx *sql.Tx) (bool, error) {
			repos := storage.NewRepositories(tx)
			archived, archiveErr := repos.Projects.Archive(ctx, project.ID, nowISO)
			if archiveErr != nil {
				return false, archiveErr
			}
			if !archived {
				return false, nil
			}
			if _, terminateErr := repos.Loops.TerminateByProject(ctx, project.ID, nowISO); terminateErr != nil {
				return false, terminateErr
			}
			if _, cancelErr := repos.Queue.CancelByProject(ctx, project.ID, nowISO, &cancelReason); cancelErr != nil {
				return false, cancelErr
			}
			return true, nil
		})
		if transactionErr != nil || !archived {
			return transactionErr
		}
		if s.PublishProjects != nil {
			s.PublishProjects(nextProjects)
			publishedProjects = true
		}
		return nil
	})
	if err != nil {
		return storage.ProjectRecord{}, err
	}
	if publishedProjects && s.AfterPublishProjects != nil {
		s.AfterPublishProjects()
	}
	if !archived {
		return storage.ProjectRecord{}, ProjectNotFoundError{Identifier: trimmed}
	}
	project.Archived = true
	project.UpdatedAt = nowISO
	return *project, nil
}

func (s *Service) withConfigBoundary(action func() error) error {
	if s.ConfigBoundary == nil {
		return action()
	}
	s.ConfigBoundary.Lock()
	defer s.ConfigBoundary.Unlock()
	return action()
}

func (s *Service) resolveActiveProjectForRemoval(ctx context.Context, identifier string) (*storage.ProjectRecord, error) {
	project, err := s.Repos.Projects.GetByID(ctx, identifier)
	if err != nil {
		return nil, err
	}
	if project != nil {
		if project.Archived {
			return nil, nil
		}
		return project, nil
	}

	items, err := s.Repos.Projects.List(ctx)
	if err != nil {
		return nil, err
	}

	var matched *storage.ProjectRecord
	for index := range items {
		if items[index].Archived || !strings.EqualFold(strings.TrimSpace(items[index].Name), identifier) {
			continue
		}
		if matched != nil {
			return nil, AmbiguousProjectIdentifierError{Identifier: identifier}
		}
		matched = &items[index]
	}
	return matched, nil
}

func (s *Service) SyncConfigured(ctx context.Context, cfg config.Config, now time.Time) error {
	if s.Repos == nil || s.Repos.Projects == nil {
		return fmt.Errorf("projects repository is not configured")
	}
	s.mutationMu.Lock()
	defer s.mutationMu.Unlock()

	nowISO := currentISO(func() time.Time { return now })
	cancelReason := "project archived"
	initialProjects, err := s.Repos.Projects.List(ctx)
	if err != nil {
		return err
	}
	projectIDs := make([]string, 0, len(initialProjects)+len(cfg.Projects))
	for _, project := range initialProjects {
		projectIDs = append(projectIDs, project.ID)
	}
	for _, project := range cfg.Projects {
		projectIDs = append(projectIDs, project.ID)
	}
	unlockProjects, lockErr := s.lockProjectOperations(ctx, projectIDs...)
	if lockErr != nil {
		return lockErr
	}
	defer unlockProjects()
	// Discovery does not take mutationMu, so it may have updated metadata while
	// SyncConfigured waited for the keyed locks. Re-read after acquiring them;
	// this second snapshot is the authoritative input for the replacement rows.
	existingProjects, err := s.Repos.Projects.List(ctx)
	if err != nil {
		return err
	}
	desiredIDs := make(map[string]struct{}, len(cfg.Projects))
	existingByID := make(map[string]*storage.ProjectRecord, len(existingProjects))
	for index := range existingProjects {
		existingByID[existingProjects[index].ID] = &existingProjects[index]
	}
	desiredRecords := make([]storage.ProjectRecord, 0, len(cfg.Projects))
	for _, project := range cfg.Projects {
		desiredIDs[project.ID] = struct{}{}
		if existing := existingByID[project.ID]; existing != nil {
			if source, _ := parseMetadata(existing.MetadataJSON)["source"].(string); source == "api" {
				return ProjectValidationError{Message: fmt.Sprintf("configured project %s conflicts with an API-managed project", project.ID)}
			}
		}
	}

	for _, project := range cfg.Projects {
		existing := existingByID[project.ID]

		repo, err := s.detectConfiguredProjectRepo(ctx, existing, project)
		if err != nil {
			return fmt.Errorf("detect repo for %s: %w", project.ID, err)
		}

		metadataJSONValue, err := buildProjectMetadataJSON(existing, project, repo)
		if err != nil {
			return fmt.Errorf("build project metadata for %s: %w", project.ID, err)
		}

		baseBranch := cfg.Defaults.BaseBranch
		if project.BaseBranch != nil {
			baseBranch = *project.BaseBranch
		}
		if err := s.validateReviewerAutoMergeForProject(ctx, project.ID, repo, baseBranch, cfg); err != nil {
			return err
		}

		createdAt := nowISO
		if existing != nil {
			createdAt = existing.CreatedAt
		}

		record := storage.ProjectRecord{
			ID:           project.ID,
			Name:         project.Name,
			RepoPath:     project.RepoPath,
			BaseBranch:   &baseBranch,
			Archived:     false,
			MetadataJSON: &metadataJSONValue,
			CreatedAt:    createdAt,
			UpdatedAt:    nowISO,
		}
		desiredRecords = append(desiredRecords, record)
	}

	applyImport := func(repos *storage.Repositories) error {
		for _, record := range desiredRecords {
			if err := repos.Projects.Upsert(ctx, record); err != nil {
				return err
			}
		}
		for index := range existingProjects {
			existing := existingProjects[index]
			if existing.Archived {
				continue
			}
			if source, _ := parseMetadata(existing.MetadataJSON)["source"].(string); source != "config" {
				continue
			}
			if _, configured := desiredIDs[existing.ID]; configured {
				continue
			}
			if _, err := repos.Loops.TerminateByProject(ctx, existing.ID, nowISO); err != nil {
				return err
			}
			if _, err := repos.Queue.CancelByProject(ctx, existing.ID, nowISO, &cancelReason); err != nil {
				return err
			}
			if _, err := repos.Projects.Archive(ctx, existing.ID, nowISO); err != nil {
				return err
			}
		}
		return nil
	}
	if s.DB == nil {
		return applyImport(s.Repos)
	}
	_, err = storage.WithTransactionValue(ctx, s.DB, nil, func(tx *sql.Tx) (struct{}, error) {
		if err := applyImport(storage.NewRepositories(tx)); err != nil {
			return struct{}{}, err
		}
		return struct{}{}, nil
	})
	return err
}

func (s *Service) detectConfiguredProjectRepo(ctx context.Context, existing *storage.ProjectRecord, project config.ProjectRefConfig) (*string, error) {
	if repo := strings.TrimSpace(project.Repo); repo != "" {
		return &repo, nil
	}
	if config.ResolvedProjectProviderKind(s.currentConfig(), project) != config.ProviderKindGitHub {
		if existing != nil && existing.RepoPath == project.RepoPath {
			return stringMetadataPtr(existing.MetadataJSON, "repo"), nil
		}
		return nil, nil
	}
	if s.DetectRepo != nil {
		detected, err := s.DetectRepo(ctx, project.RepoPath)
		if err != nil {
			if existing != nil && existing.RepoPath == project.RepoPath {
				if repo := stringMetadataPtr(existing.MetadataJSON, "repo"); repo != nil {
					if s.Logger != nil {
						s.Logger.Warn("preserving existing project repo metadata after detection failure", map[string]any{"projectId": project.ID, "repoPath": project.RepoPath, "error": err.Error()})
					}
					return repo, nil
				}
			}
			if s.Logger != nil {
				s.Logger.Warn("skipping configured project repo detection after failure", map[string]any{"projectId": project.ID, "repoPath": project.RepoPath, "error": err.Error()})
			}
			return nil, nil
		}
		if strings.TrimSpace(detected.Provider) != "" {
			return nil, nil
		}
		detectedRepo := strings.TrimSpace(detected.Repo)
		if detectedRepo == "" {
			if existing != nil && existing.RepoPath == project.RepoPath {
				if repo := stringMetadataPtr(existing.MetadataJSON, "repo"); repo != nil {
					return repo, nil
				}
			}
			return nil, nil
		}
		return &detectedRepo, nil
	}

	if existing != nil && existing.RepoPath == project.RepoPath {
		return stringMetadataPtr(existing.MetadataJSON, "repo"), nil
	}
	return nil, nil
}

func normalizeProjectID(input AddInput) string {
	if input.IDSource != "derived" {
		return input.ID
	}
	if input.ID != deriveProjectIDFromRepoPath(input.RepoPath) {
		return input.ID
	}
	if !strings.HasPrefix(input.ID, legacyProjectIDPrefix) {
		return input.ID
	}
	return normalizeDerivedProjectID(input.ID)
}

func normalizeDerivedProjectID(projectID string) string {
	if !strings.HasPrefix(projectID, legacyProjectIDPrefix) {
		return projectID
	}
	return "project_" + projectID
}

func sameProjectRepoPath(left string, right string) bool {
	return filepath.Clean(strings.TrimSpace(left)) == filepath.Clean(strings.TrimSpace(right))
}

func deriveProjectIDFromRepoPath(repoPath string) string {
	segments := strings.FieldsFunc(repoPath, func(r rune) bool { return r == '/' || r == '\\' })
	lastSegment := "project"
	if len(segments) > 0 {
		lastSegment = segments[len(segments)-1]
	}
	normalized := strings.Trim(nonProjectIDPattern.ReplaceAllString(strings.ToLower(lastSegment), "-"), "-")
	if normalized == "" {
		return "project"
	}
	return normalized
}

func assertValidProjectID(projectID string) error {
	if projectID == "" || projectID == "." || projectID == ".." || strings.HasPrefix(projectID, legacyProjectIDPrefix) || containsProjectPathSeparator(projectID) || filepath.IsAbs(projectID) || isWindowsAbsolute(projectID) {
		return fmt.Errorf("invalid project id %q: must not contain path separators, dot segments, be an absolute path, or start with legacy-id-", projectID)
	}
	return nil
}

func containsProjectPathSeparator(projectID string) bool {
	return strings.Contains(projectID, "/") || strings.Contains(projectID, `\`)
}

func isWindowsAbsolute(projectID string) bool {
	if len(projectID) >= 3 {
		drive := projectID[0]
		sep := projectID[2]
		if ((drive >= 'a' && drive <= 'z') || (drive >= 'A' && drive <= 'Z')) && projectID[1] == ':' && (sep == '/' || sep == '\\') {
			return true
		}
	}
	if len(projectID) >= 2 && strings.HasPrefix(projectID, `\\`) {
		return true
	}
	return false
}

func parseMetadata(metadataJSON *string) map[string]any {
	if metadataJSON == nil || *metadataJSON == "" {
		return map[string]any{}
	}
	var metadata map[string]any
	if err := json.Unmarshal([]byte(*metadataJSON), &metadata); err != nil || metadata == nil {
		return map[string]any{}
	}
	return metadata
}

func buildProjectMetadataJSON(existing *storage.ProjectRecord, project config.ProjectRefConfig, repo *string) (string, error) {
	extras := map[string]json.RawMessage{}
	repoRaw := json.RawMessage("null")

	if existing != nil {
		existingMetadata := parseMetadata(existing.MetadataJSON)
		for key, value := range existingMetadata {
			switch key {
			case "repo":
				continue
			case "worktreeRoot", "source":
				continue
			default:
				encoded, err := json.Marshal(value)
				if err != nil {
					return "", err
				}
				extras[key] = encoded
			}
		}
	}
	if repo != nil && strings.TrimSpace(*repo) != "" {
		encoded, err := json.Marshal(strings.TrimSpace(*repo))
		if err != nil {
			return "", err
		}
		repoRaw = encoded
	}
	setProjectMetadata := func(key string, value any, keep bool) error {
		if !keep {
			delete(extras, key)
			return nil
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		extras[key] = encoded
		return nil
	}
	if err := setProjectMetadata("provider", strings.TrimSpace(project.Provider), strings.TrimSpace(project.Provider) != ""); err != nil {
		return "", err
	}
	if err := setProjectMetadata("path", strings.TrimSpace(project.Path), strings.TrimSpace(project.Path) != ""); err != nil {
		return "", err
	}
	if err := setProjectMetadata("network", project.Network, project.Network.Mode != ""); err != nil {
		return "", err
	}
	if err := setProjectMetadata("webhook", project.Webhook, project.Webhook.Mode != ""); err != nil {
		return "", err
	}
	if err := setProjectMetadata("validation", project.Validation, project.Validation != nil); err != nil {
		return "", err
	}
	if err := setProjectMetadata("roles", project.Roles, project.Roles != nil); err != nil {
		return "", err
	}

	entries := make([]orderedJSONEntry, 0, len(extras)+3)
	extraKeys := make([]string, 0, len(extras))
	for key := range extras {
		extraKeys = append(extraKeys, key)
	}
	sort.Strings(extraKeys)
	for _, key := range extraKeys {
		entries = append(entries, orderedJSONEntry{Key: key, Raw: extras[key]})
	}
	entries = append(entries, orderedJSONEntry{Key: "repo", Raw: repoRaw})
	if project.WorktreeRoot != nil {
		encoded, err := json.Marshal(*project.WorktreeRoot)
		if err != nil {
			return "", err
		}
		entries = append(entries, orderedJSONEntry{Key: "worktreeRoot", Raw: encoded})
	} else {
		entries = append(entries, orderedJSONEntry{Key: "worktreeRoot", Raw: json.RawMessage("null")})
	}
	entries = append(entries, orderedJSONEntry{Key: "source", Raw: json.RawMessage(`"config"`)})

	return marshalOrderedJSONObject(entries)
}

func buildAddProjectMetadataJSON(metadata map[string]any) (string, error) {
	entries := make([]orderedJSONEntry, 0, len(metadata))
	extraKeys := make([]string, 0, len(metadata))
	for key := range metadata {
		switch key {
		case "normalizedDerivedId", "provider", "repo", "worktreeRoot", "source":
			continue
		default:
			extraKeys = append(extraKeys, key)
		}
	}
	sort.Strings(extraKeys)
	for _, key := range extraKeys {
		encoded, err := json.Marshal(metadata[key])
		if err != nil {
			return "", err
		}
		entries = append(entries, orderedJSONEntry{Key: key, Raw: encoded})
	}
	if value, ok := metadata["normalizedDerivedId"]; ok {
		encoded, err := json.Marshal(value)
		if err != nil {
			return "", err
		}
		entries = append(entries, orderedJSONEntry{Key: "normalizedDerivedId", Raw: encoded})
	}
	if value, ok := metadata["provider"]; ok {
		encoded, err := json.Marshal(value)
		if err != nil {
			return "", err
		}
		entries = append(entries, orderedJSONEntry{Key: "provider", Raw: encoded})
	}
	repoEncoded, err := json.Marshal(metadata["repo"])
	if err != nil {
		return "", err
	}
	entries = append(entries, orderedJSONEntry{Key: "repo", Raw: repoEncoded})
	worktreeRootEncoded, err := json.Marshal(metadata["worktreeRoot"])
	if err != nil {
		return "", err
	}
	entries = append(entries, orderedJSONEntry{Key: "worktreeRoot", Raw: worktreeRootEncoded})
	sourceEncoded, err := json.Marshal(metadata["source"])
	if err != nil {
		return "", err
	}
	entries = append(entries, orderedJSONEntry{Key: "source", Raw: sourceEncoded})
	return marshalOrderedJSONObject(entries)
}

func normalizeOptionalProvider(value *string) *string {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

// validateExplicitProvider rejects provider bindings on `project add`. The only
// remaining provider kind is GitHub, which projects get by default, so an
// explicit binding has nothing left to express; it belongs in [[projects]].
func validateExplicitProvider(cfg config.Config, provider *string) error {
	if provider == nil {
		return nil
	}
	providerID := strings.TrimSpace(*provider)
	for _, configured := range cfg.Providers {
		if configured.ID == providerID {
			return ProjectValidationError{Message: fmt.Sprintf("provider %q cannot be bound by project add; define the project under [[projects]] instead", providerID)}
		}
	}
	return ProjectValidationError{Message: fmt.Sprintf("unknown provider id %q; configure it under [[providers]] first", providerID)}
}

type orderedJSONEntry struct {
	Key string
	Raw json.RawMessage
}

func marshalOrderedJSONObject(entries []orderedJSONEntry) (string, error) {
	buffer := &bytes.Buffer{}
	buffer.WriteByte('{')
	for index, entry := range entries {
		if index > 0 {
			buffer.WriteByte(',')
		}
		keyJSON, err := json.Marshal(entry.Key)
		if err != nil {
			return "", err
		}
		buffer.Write(keyJSON)
		buffer.WriteByte(':')
		buffer.Write(entry.Raw)
	}
	buffer.WriteByte('}')
	return buffer.String(), nil
}

func currentISO(now func() time.Time) string {
	if now == nil {
		now = time.Now
	}
	return eventlog.FormatJavaScriptISOString(now())
}

func stringMetadataPtr(metadataJSON *string, key string) *string {
	metadata := parseMetadata(metadataJSON)
	value, _ := metadata[key].(string)
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}

func stringPointer(value string) *string {
	return &value
}

func (s *Service) discoverWorktrees(ctx context.Context, project storage.ProjectRecord, nowISO string, warnings *[]string) (int, error) {
	if s.ListWorktrees == nil || s.Repos == nil || s.Repos.Worktrees == nil {
		return 0, nil
	}

	worktrees, err := s.ListWorktrees(ctx, project.RepoPath)
	if err != nil {
		message := err.Error()
		if s.Logger != nil {
			s.Logger.Warn("failed to discover worktrees for project", map[string]any{"projectId": project.ID, "repoPath": project.RepoPath, "message": message})
		}
		*warnings = append(*warnings, fmt.Sprintf("Could not discover worktrees: %s", message))
		return 0, err
	}

	discovered := 0
	for _, worktree := range worktrees {
		if worktree.Bare || strings.TrimSpace(worktree.Branch) == "" {
			continue
		}

		existingByPath, err := s.Repos.Worktrees.GetByPath(ctx, worktree.Path)
		if err != nil {
			return discovered, err
		}
		if existingByPath != nil && existingByPath.ProjectID != project.ID {
			return discovered, fmt.Errorf("worktree path %q belongs to project %q, not %q", worktree.Path, existingByPath.ProjectID, project.ID)
		}
		existingByBranch, err := s.Repos.Worktrees.GetByBranch(ctx, project.ID, worktree.Branch)
		if err != nil {
			return discovered, err
		}
		existing := existingByPath
		if existing == nil {
			existing = existingByBranch
		}

		baseBranch := stringPointer(worktree.Branch)
		if project.BaseBranch != nil && strings.TrimSpace(*project.BaseBranch) != "" {
			baseBranch = project.BaseBranch
		}
		if existing != nil && existing.BaseBranch != nil && strings.TrimSpace(*existing.BaseBranch) != "" {
			baseBranch = existing.BaseBranch
		}

		headSHA := existingHeadSHA(existing)
		if strings.TrimSpace(worktree.HeadSHA) != "" {
			headSHA = stringPointer(worktree.HeadSHA)
		}

		metadataJSON := `{"discovered":true}`
		record := storage.WorktreeRecord{
			ID:           worktreeID(existing, s.Now),
			ProjectID:    project.ID,
			RepoPath:     project.RepoPath,
			WorktreePath: worktree.Path,
			Branch:       worktree.Branch,
			BaseBranch:   baseBranch,
			Status:       "active",
			HeadSHA:      headSHA,
			MetadataJSON: &metadataJSON,
			CreatedAt:    worktreeCreatedAt(existing, nowISO),
			UpdatedAt:    nowISO,
			CleanedAt:    nil,
		}
		// A path row owns the physical checkout. If its new branch is already
		// represented elsewhere, adopt it transactionally so the other checkout
		// keeps cleanup provenance while this path takes the current branch.
		if existingByPath != nil && existingByBranch != nil && existingByPath.ID != existingByBranch.ID {
			err = s.Repos.Worktrees.AdoptPath(ctx, record)
		} else {
			err = s.Repos.Worktrees.Upsert(ctx, record)
		}
		if err != nil {
			return discovered, err
		}
		discovered++
	}

	return discovered, nil
}

func (s *Service) discoverPullRequests(ctx context.Context, project storage.ProjectRecord, repo *string, mode SnapshotMode, warnings *[]string) (int, int, int, error) {
	if mode == SnapshotModeOff || repo == nil || strings.TrimSpace(*repo) == "" || s.ListOpenPullRequests == nil {
		return 0, 0, 0, nil
	}
	if mode == SnapshotModeAsync && !s.asyncSnapshotQueueEnabled() {
		mode = SnapshotModeFull
		*warnings = append(*warnings, "Async snapshot mode requires the scheduler; capturing snapshots synchronously instead.")
	}

	listCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	pullRequests, err := s.ListOpenPullRequests(listCtx, ListOpenPullRequestsInput{Repo: *repo, CWD: project.RepoPath, Limit: 1000, Timeout: 15 * time.Second})
	if err != nil {
		message := err.Error()
		if s.Logger != nil {
			s.Logger.Warn("failed to discover pull requests for project", map[string]any{"projectId": project.ID, "repo": *repo, "message": message})
		}
		*warnings = append(*warnings, fmt.Sprintf("Could not discover pull requests: %s", message))
		return 0, 0, 0, err
	}

	discovered := 0
	pending := 0
	captured := 0
	for _, pullRequest := range pullRequests {
		if pullRequest.IsDraft || normalizePRState(pullRequest.State) != "open" {
			continue
		}
		discovered++
		if mode == SnapshotModeAsync {
			queued, err := s.enqueuePullRequestSnapshot(ctx, project, *repo, pullRequest.Number)
			if err != nil {
				return discovered, pending, captured, err
			}
			if queued {
				pending++
			}
			continue
		}
		if s.CapturePullRequestSnapshot == nil || s.Repos == nil || s.Repos.PullRequestSnapshots == nil {
			continue
		}

		snapshot, err := s.CapturePullRequestSnapshot(ctx, CapturePullRequestSnapshotInput{
			ProjectID:  project.ID,
			Repo:       *repo,
			PRNumber:   pullRequest.Number,
			CWD:        project.RepoPath,
			CapturedAt: currentISO(s.Now),
		})
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return discovered, pending, captured, err
			}
			if ctxErr := ctx.Err(); errors.Is(ctxErr, context.Canceled) || errors.Is(ctxErr, context.DeadlineExceeded) {
				return discovered, pending, captured, errors.Join(err, ctxErr)
			}
			message := err.Error()
			if s.Logger != nil {
				s.Logger.Warn("failed to snapshot pull request for project", map[string]any{"projectId": project.ID, "repo": *repo, "pullRequestNumber": pullRequest.Number, "message": message})
			}
			*warnings = append(*warnings, fmt.Sprintf("Could not snapshot pull request #%d: %s", pullRequest.Number, message))
			continue
		}
		if err := s.Repos.PullRequestSnapshots.Upsert(ctx, snapshot); err != nil {
			return discovered, pending, captured, err
		}
		captured++
	}

	return discovered, pending, captured, nil
}

func (s *Service) asyncSnapshotQueueEnabled() bool {
	if s.AsyncSnapshotQueueEnabled == nil {
		return true
	}
	return s.AsyncSnapshotQueueEnabled()
}

func (s *Service) enqueuePullRequestSnapshot(ctx context.Context, project storage.ProjectRecord, repo string, prNumber int64) (bool, error) {
	if s.Repos == nil || s.Repos.Queue == nil {
		return false, nil
	}
	dedupeKey := fmt.Sprintf("snapshot:%s:%s:%d", project.ID, repo, prNumber)
	existing, err := s.Repos.Queue.FindActiveByDedupe(ctx, dedupeKey)
	if err != nil {
		return false, err
	}
	if existing != nil {
		return true, nil
	}
	nowISO := currentISO(s.Now)
	payload, err := json.Marshal(map[string]any{"cwd": project.RepoPath})
	if err != nil {
		return false, err
	}
	record := storage.QueueItemRecord{
		ID:          fmt.Sprintf("snapshot_%s_%d_%d", project.ID, prNumber, currentTime(s.Now).UnixNano()),
		ProjectID:   &project.ID,
		Type:        queueTypeSnapshot,
		TargetType:  "pull_request_snapshot",
		TargetID:    fmt.Sprintf("%s#%d", repo, prNumber),
		Repo:        &repo,
		PRNumber:    &prNumber,
		DedupeKey:   dedupeKey,
		Priority:    storage.QueuePrioritySnapshot,
		Status:      "queued",
		AvailableAt: nowISO,
		MaxAttempts: s.snapshotRetryMaxAttempts(),
		PayloadJSON: stringPointer(string(payload)),
		CreatedAt:   nowISO,
		UpdatedAt:   nowISO,
	}
	_, _, err = s.Repos.Queue.CreateOrGetActiveByDedupe(ctx, record)
	return true, err
}

func (s *Service) snapshotRetryMaxAttempts() int64 {
	cfg := s.currentConfig()
	if cfg.Scheduler.RetryMaxAttempts == 0 {
		return -1
	}
	return int64(cfg.Scheduler.RetryMaxAttempts)
}

func (s *Service) currentConfig() config.Config {
	if s != nil && s.ConfigSource != nil {
		return s.ConfigSource.Snapshot()
	}
	if s == nil {
		return config.Config{}
	}
	return s.Config
}

func (s *Service) materializeCandidate(ctx context.Context, replacement *storage.ProjectRecord, archiveID string) ([]config.ProjectRefConfig, error) {
	if s == nil || s.Repos == nil || s.Repos.Projects == nil {
		return nil, fmt.Errorf("projects repository is not configured")
	}
	records, err := s.Repos.Projects.List(ctx)
	if err != nil {
		return nil, err
	}
	originalLegacyInert := legacyInertProjectIDs(records)
	replaced := false
	for index := range records {
		if replacement != nil && records[index].ID == replacement.ID {
			records[index] = *replacement
			replaced = true
		}
		if records[index].ID == archiveID {
			records[index].Archived = true
		}
	}
	if replacement != nil && !replaced {
		records = append(records, *replacement)
	}
	current := s.currentConfig()
	materialized, err := MaterializeCatalog(current, records)
	if err != nil {
		return nil, err
	}
	if s.ConfigSource != nil {
		// Only a record that was already an inert pre-validation API project
		// may remain unstanced while it is repaired. New registrations and a
		// PATCH that adds repository metadata still validate every project.
		legacyInert := intersectProjectIDs(legacyInertProjectIDs(records), originalLegacyInert)
		if err := validateCatalogValidationPolicies(current, materialized, legacyInert); err != nil {
			return nil, err
		}
	}
	return materialized, nil
}

func legacyInertProjectIDs(records []storage.ProjectRecord) map[string]struct{} {
	ids := map[string]struct{}{}
	for _, record := range records {
		if record.Archived {
			continue
		}
		metadata := parseMetadata(record.MetadataJSON)
		if metadataString(metadata, "source") != "api" || metadataString(metadata, "repo") != "" {
			continue
		}
		if _, hasValidation := metadata["validation"]; !hasValidation {
			ids[record.ID] = struct{}{}
		}
	}
	return ids
}

func intersectProjectIDs(left, right map[string]struct{}) map[string]struct{} {
	intersection := map[string]struct{}{}
	for id := range left {
		if _, ok := right[id]; ok {
			intersection[id] = struct{}{}
		}
	}
	return intersection
}

func validateCatalogValidationPolicies(global config.Config, materialized []config.ProjectRefConfig, allowedMissingPolicy map[string]struct{}) error {
	candidate := config.CloneConfig(global)
	candidate.Projects = make([]config.ProjectRefConfig, 0, len(materialized))
	for _, project := range materialized {
		if _, allowed := allowedMissingPolicy[project.ID]; allowed {
			continue
		}
		candidate.Projects = append(candidate.Projects, project)
	}
	return config.ValidateProjectValidationPolicies(candidate)
}

// ValidateStoredCatalogValidationPolicies preserves startup repair for only
// API projects that predate project validation and cannot run without repo
// metadata. Their persisted record is the authority for that exception.
func ValidateStoredCatalogValidationPolicies(global config.Config, records []storage.ProjectRecord, materialized []config.ProjectRefConfig) error {
	return validateCatalogValidationPolicies(global, materialized, legacyInertProjectIDs(records))
}

func snapshotModeOrDefault(mode SnapshotMode) SnapshotMode {
	switch mode {
	case SnapshotModeFull, SnapshotModeOff:
		return mode
	default:
		return SnapshotModeAsync
	}
}

func normalizePRState(state string) string {
	trimmed := strings.TrimSpace(strings.ToLower(state))
	if trimmed == "" {
		return "open"
	}
	return trimmed
}

func worktreeID(existing *storage.WorktreeRecord, now func() time.Time) string {
	if existing != nil && strings.TrimSpace(existing.ID) != "" {
		return existing.ID
	}

	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Sprintf("worktree_%d", currentTime(now).UnixNano())
	}
	return "worktree_" + hex.EncodeToString(raw)
}

func currentTime(now func() time.Time) time.Time {
	if now == nil {
		return time.Now()
	}
	return now()
}

func existingHeadSHA(existing *storage.WorktreeRecord) *string {
	if existing == nil {
		return nil
	}
	return existing.HeadSHA
}

func worktreeCreatedAt(existing *storage.WorktreeRecord, nowISO string) string {
	if existing != nil && strings.TrimSpace(existing.CreatedAt) != "" {
		return existing.CreatedAt
	}
	return nowISO
}

func (s *Service) scheduleDiscovery(input DiscoverInput) {
	s.scheduleDiscoveries([]DiscoverInput{input})
}

func (s *Service) scheduleDiscoveries(inputs []DiscoverInput) {
	if len(inputs) == 0 {
		return
	}
	if s == nil || s.ScheduleDiscovery == nil {
		if s != nil && s.Logger != nil {
			s.Logger.Warn("post-commit project discovery left pending without a lifecycle owner", map[string]any{
				"projectId": inputs[0].ProjectID,
			})
		}
		return
	}
	ctx := context.Background()
	if s.DiscoveryContext != nil {
		if candidate := s.DiscoveryContext(); candidate != nil {
			ctx = candidate
		}
	}
	run := func() {
		for _, input := range inputs {
			if ctx.Err() != nil {
				return
			}
			_, err := s.DiscoverProject(ctx, input)
			if err == nil || s.Logger == nil {
				continue
			}
			s.Logger.Warn("post-commit project discovery failed", map[string]any{
				"projectId": input.ProjectID,
				"message":   err.Error(),
			})
		}
	}
	s.ScheduleDiscovery(run)
}

func (s *Service) writeDiscoveryState(ctx context.Context, project *storage.ProjectRecord, discovery DiscoveryState) error {
	if project == nil || s.Repos == nil || s.Repos.Projects == nil {
		return fmt.Errorf("projects repository is not configured")
	}
	// DiscoverProject holds the keyed project-operation lock. Re-read the
	// authoritative row before each whole-record upsert so metadata committed
	// before the lock was acquired is not replaced by the caller's stale copy.
	current, err := s.Repos.Projects.GetByID(context.Background(), project.ID)
	if err != nil {
		return err
	}
	if current == nil {
		return ProjectNotFoundError{Identifier: project.ID}
	}
	*project = *current
	metadata := parseMetadata(current.MetadataJSON)
	metadata[registrationDiscoveryMetadataKey] = discoveryStateMap(discovery)
	metadataJSON, err := buildAddProjectMetadataJSON(metadata)
	if err != nil {
		return fmt.Errorf("marshal project metadata: %w", err)
	}
	project.MetadataJSON = stringPointer(metadataJSON)
	project.UpdatedAt = discovery.UpdatedAt
	if discovery.UpdatedAt == "" {
		project.UpdatedAt = currentISO(s.Now)
	}
	// Persist discovery status independently of the caller's context so a
	// canceled discovery request cannot leave status stuck at running.
	return s.Repos.Projects.Upsert(context.Background(), *project)
}

// DiscoveryStateFromRecord returns the persisted post-commit discovery state.
func DiscoveryStateFromRecord(project storage.ProjectRecord) DiscoveryState {
	return discoveryStateFromMetadata(parseMetadata(project.MetadataJSON))
}

func discoveryStateFromMetadata(metadata map[string]any) DiscoveryState {
	raw, _ := metadata[registrationDiscoveryMetadataKey].(map[string]any)
	if raw == nil {
		return DiscoveryState{}
	}
	status, _ := raw["status"].(string)
	status = strings.TrimSpace(status)
	if status == "" {
		return DiscoveryState{}
	}
	snapshotMode, _ := raw["snapshotMode"].(string)
	updatedAt, _ := raw["updatedAt"].(string)
	errText, _ := raw["error"].(string)
	state := DiscoveryState{
		Status:                 DiscoveryStatus(status),
		SnapshotMode:           SnapshotMode(strings.TrimSpace(snapshotMode)),
		UpdatedAt:              strings.TrimSpace(updatedAt),
		Error:                  strings.TrimSpace(errText),
		DiscoveredPullRequests: intFromAny(raw["discoveredPullRequests"]),
		DiscoveredWorktrees:    intFromAny(raw["discoveredWorktrees"]),
		PendingSnapshots:       intFromAny(raw["pendingSnapshots"]),
		CapturedSnapshots:      intFromAny(raw["capturedSnapshots"]),
	}
	if warnings, ok := raw["warnings"].([]any); ok {
		for _, warning := range warnings {
			text, _ := warning.(string)
			text = strings.TrimSpace(text)
			if text != "" {
				state.Warnings = append(state.Warnings, text)
			}
		}
	}
	return state
}

func discoveryStateMap(discovery DiscoveryState) map[string]any {
	out := map[string]any{
		"status":    string(discovery.Status),
		"updatedAt": discovery.UpdatedAt,
	}
	if discovery.SnapshotMode != "" {
		out["snapshotMode"] = string(discovery.SnapshotMode)
	}
	if discovery.Error != "" {
		out["error"] = discovery.Error
	}
	if discovery.DiscoveredPullRequests != 0 {
		out["discoveredPullRequests"] = discovery.DiscoveredPullRequests
	}
	if discovery.DiscoveredWorktrees != 0 {
		out["discoveredWorktrees"] = discovery.DiscoveredWorktrees
	}
	if discovery.PendingSnapshots != 0 {
		out["pendingSnapshots"] = discovery.PendingSnapshots
	}
	if discovery.CapturedSnapshots != 0 {
		out["capturedSnapshots"] = discovery.CapturedSnapshots
	}
	if len(discovery.Warnings) > 0 {
		out["warnings"] = append([]string{}, discovery.Warnings...)
	}
	return out
}

func intFromAny(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case json.Number:
		parsed, err := typed.Int64()
		if err != nil {
			return 0
		}
		return int(parsed)
	default:
		return 0
	}
}

func firstErrorMessage(errs ...error) string {
	for _, err := range errs {
		if err != nil {
			return err.Error()
		}
	}
	return ""
}

func discoveryCanceled(errs ...error) bool {
	canceled := false
	for _, err := range errs {
		if err == nil {
			continue
		}
		if !onlyContextCanceled(err) {
			return false
		}
		canceled = true
	}
	return canceled
}

func onlyContextCanceled(err error) bool {
	if err == context.Canceled {
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		unwrapped := joined.Unwrap()
		if len(unwrapped) == 0 {
			return false
		}
		for _, nested := range unwrapped {
			if !onlyContextCanceled(nested) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return onlyContextCanceled(wrapped.Unwrap())
	}
	return false
}
