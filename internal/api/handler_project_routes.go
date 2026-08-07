package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/projects"
	"github.com/MumuTW/looper/internal/storage"
	pkgapi "github.com/MumuTW/looper/pkg/api"
)

func (h *Handler) buildProjectsRouteResponse(r *http.Request) (any, error) {
	service := h.projectsRouteService()
	if service == nil {
		return nil, apiError{
			code:    pkgapi.ErrorCodeProjectsUnavailable,
			status:  http.StatusInternalServerError,
			message: "Project management is not available in this runtime",
		}
	}

	switch r.Method {
	case http.MethodGet:
		items, err := service.List(r.Context())
		if err != nil {
			return nil, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
		}

		responseItems := make([]projectResponse, 0, len(items))
		for _, item := range items {
			responseItems = append(responseItems, serializeProject(item, h.context.Config, h.context.Config.Defaults.BaseBranch))
		}
		return projectsListResponse{Items: responseItems}, nil
	case http.MethodPost:
		return h.buildCreateProjectResponse(r, service)
	default:
		return nil, apiError{
			code:    pkgapi.ErrorCodeMethodNotAllowed,
			status:  http.StatusMethodNotAllowed,
			message: fmt.Sprintf("Unsupported method for %s", apiBasePath+"/projects"),
		}
	}
}

func (h *Handler) buildProjectRouteResponse(r *http.Request, path string) (any, error) {
	service := h.projectsRouteService()
	if service == nil {
		return nil, apiError{
			code:    pkgapi.ErrorCodeProjectsUnavailable,
			status:  http.StatusInternalServerError,
			message: "Project management is not available in this runtime",
		}
	}

	requestPath := normalizePath(r.URL.EscapedPath())
	discoverRoute := false
	trimmed := strings.TrimSuffix(requestPath, "/")
	projectPathPrefix := apiBasePath + "/projects/"
	projectPathSuffix := strings.TrimPrefix(trimmed, projectPathPrefix)
	if projectPathSuffix != trimmed {
		segments := strings.Split(projectPathSuffix, "/")
		if len(segments) == 2 && segments[1] == "discover" {
			discoverRoute = true
			requestPath = projectPathPrefix + segments[0]
		}
	}

	identifier, err := decodeProjectIdentifier(requestPath)
	if err != nil {
		return nil, err
	}

	if discoverRoute {
		return h.buildProjectDiscoverResponse(r, service, identifier)
	}
	if r.Method == http.MethodPatch {
		return h.buildUpdateProjectResponse(r, service, identifier)
	}

	if r.Method != http.MethodDelete {
		return nil, apiError{code: pkgapi.ErrorCodeMethodNotAllowed, status: http.StatusMethodNotAllowed, message: fmt.Sprintf("Unsupported method for %s", path)}
	}

	removed, err := service.RemoveProject(r.Context(), identifier)
	if err != nil {
		var notFound projects.ProjectNotFoundError
		var ambiguous projects.AmbiguousProjectIdentifierError
		var validation projects.ProjectValidationError
		switch {
		case errors.As(err, &notFound):
			return nil, apiError{code: pkgapi.ErrorCodeProjectNotFound, status: http.StatusNotFound, message: fmt.Sprintf("Project not found: %s", notFound.Identifier)}
		case errors.As(err, &ambiguous):
			return nil, apiError{code: pkgapi.ErrorCodeProjectAmbiguous, status: http.StatusConflict, message: err.Error()}
		case errors.As(err, &validation):
			return nil, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: err.Error()}
		default:
			return nil, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
		}
	}
	return serializeProject(removed, h.context.Config, h.context.Config.Defaults.BaseBranch), nil
}

func (h *Handler) projectsRouteService() projectService {
	if h.context.ProjectsService != nil {
		return h.context.ProjectsService
	}
	if h.context.Runtime != nil {
		runtimeProjects := h.context.Runtime.Services().Projects
		if runtimeProjects != nil {
			return runtimeProjects
		}
	}
	return nil
}

// buildProjectDiscoverResponse retries post-commit worktree/PR discovery for
// an already-registered project. Unlike registration this request is allowed
// to block: discovery is the work the caller explicitly asked to wait for.
func (h *Handler) buildProjectDiscoverResponse(r *http.Request, service projectService, identifier string) (any, error) {
	if r.Method != http.MethodPost {
		return nil, apiError{code: pkgapi.ErrorCodeMethodNotAllowed, status: http.StatusMethodNotAllowed, message: fmt.Sprintf("Unsupported method for %s", r.URL.EscapedPath())}
	}
	result, err := service.DiscoverProject(r.Context(), projects.DiscoverInput{ProjectID: identifier})
	if err != nil {
		if result.Discovery.Status == projects.DiscoveryStatusFailed {
			return projectDiscoverResponse(result, h.context.Config), nil
		}
		var notFound projects.ProjectNotFoundError
		var validation projects.ProjectValidationError
		switch {
		case errors.As(err, &notFound):
			return nil, apiError{code: pkgapi.ErrorCodeProjectNotFound, status: http.StatusNotFound, message: fmt.Sprintf("Project not found: %s", notFound.Identifier)}
		case errors.As(err, &validation):
			return nil, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: err.Error()}
		default:
			return nil, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
		}
	}
	return projectDiscoverResponse(result, h.context.Config), nil
}

func projectDiscoverResponse(result projects.DiscoverResult, cfg config.Config) createProjectResponse {
	return createProjectResponse{
		projectResponse:        serializeProject(result.Project, cfg, cfg.Defaults.BaseBranch),
		Discovery:              serializeDiscovery(result.Discovery),
		DiscoveredPullRequests: result.Discovery.DiscoveredPullRequests,
		DiscoveredWorktrees:    result.Discovery.DiscoveredWorktrees,
		PendingSnapshots:       result.Discovery.PendingSnapshots,
		CapturedSnapshots:      result.Discovery.CapturedSnapshots,
		Warnings:               append([]string{}, result.Discovery.Warnings...),
	}
}

func (h *Handler) buildUpdateProjectResponse(r *http.Request, service projectService, identifier string) (any, error) {
	body := updateProjectRequest{}
	if aerr := decodeJSONMutationBody(r, &body, true); aerr != nil {
		return nil, *aerr
	}
	updated, err := service.UpdateProject(r.Context(), identifier, projects.UpdateInput{
		Repo: updateProjectField(body.Repo), Name: updateProjectField(body.Name),
		BaseBranch: updateProjectField(body.BaseBranch), WorktreeRoot: updateProjectField(body.WorktreeRoot),
		Validation: body.Validation, GatekeeperTrust: updateProjectField(body.GatekeeperTrust),
	})
	if err != nil {
		var notFound projects.ProjectNotFoundError
		var ambiguous projects.AmbiguousProjectIdentifierError
		var validation projects.ProjectValidationError
		switch {
		case errors.As(err, &notFound):
			return nil, apiError{code: pkgapi.ErrorCodeProjectNotFound, status: http.StatusNotFound, message: fmt.Sprintf("Project not found: %s", notFound.Identifier)}
		case errors.As(err, &ambiguous):
			return nil, apiError{code: pkgapi.ErrorCodeProjectAmbiguous, status: http.StatusConflict, message: err.Error()}
		case errors.As(err, &validation):
			return nil, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: err.Error()}
		default:
			return nil, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
		}
	}
	return serializeProject(updated, h.context.Config, h.context.Config.Defaults.BaseBranch), nil
}

func (h *Handler) buildCreateProjectResponse(r *http.Request, service projectService) (createProjectResponse, error) {
	body := createProjectRequest{}
	if aerr := decodeJSONMutationBody(r, &body, true); aerr != nil {
		return createProjectResponse{}, *aerr
	}

	repoPath := strings.TrimSpace(derefString(body.RepoPath))
	if repoPath == "" {
		return createProjectResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "repoPath is required"}
	}

	providedID := strings.TrimSpace(derefString(body.ID))
	idSource := "derived"
	projectID := providedID
	if projectID == "" {
		projectID = deriveProjectIDFromRepoPath(repoPath)
	} else {
		idSource = "explicit"
	}

	name := strings.TrimSpace(derefString(body.Name))
	if name == "" {
		name = projectID
	}

	baseBranch := strings.TrimSpace(derefString(body.BaseBranch))
	if baseBranch == "" {
		baseBranch = h.context.Config.Defaults.BaseBranch
	}
	snapshotMode := projects.SnapshotMode(strings.TrimSpace(derefString(body.SnapshotMode)))
	if snapshotMode == "" {
		snapshotMode = projects.SnapshotMode(h.context.Config.Defaults.AddSnapshotMode)
	}
	if snapshotMode != "" && snapshotMode != projects.SnapshotModeAsync && snapshotMode != projects.SnapshotModeFull && snapshotMode != projects.SnapshotModeOff {
		return createProjectResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "snapshotMode must be one of: async, full, off"}
	}

	result, err := service.AddProject(r.Context(), projects.AddInput{
		ID:           projectID,
		Name:         name,
		RepoPath:     repoPath,
		BaseBranch:   baseBranch,
		IDSource:     idSource,
		WorktreeRoot: normalizeOptionalString(body.WorktreeRoot),
		Repo:         normalizeOptionalString(body.Repo),
		Validation:   body.Validation,
		SnapshotMode: snapshotMode,
	})
	if err != nil {
		var collision projects.ProjectIDCollisionError
		var validation projects.ProjectValidationError
		switch {
		case errors.As(err, &collision):
			return createProjectResponse{}, apiError{code: pkgapi.ErrorCodeProjectIDConflict, status: http.StatusConflict, message: err.Error()}
		case errors.As(err, &validation):
			return createProjectResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: err.Error()}
		case strings.HasPrefix(err.Error(), "invalid project id"):
			message := strings.Replace(err.Error(), "invalid project id", "Invalid project id", 1)
			return createProjectResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: message}
		default:
			return createProjectResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
		}
	}
	return createProjectResponse{
		projectResponse:        serializeProject(result.Project, h.context.Config, h.context.Config.Defaults.BaseBranch),
		Discovery:              serializeDiscovery(result.Discovery),
		DiscoveredPullRequests: result.Discovery.DiscoveredPullRequests,
		DiscoveredWorktrees:    result.Discovery.DiscoveredWorktrees,
		PendingSnapshots:       result.Discovery.PendingSnapshots,
		CapturedSnapshots:      result.Discovery.CapturedSnapshots,
		Warnings:               append([]string{}, result.Warnings...),
	}, nil
}

func serializeDiscovery(state projects.DiscoveryState) discoveryResponse {
	return discoveryResponse{
		Status:                 string(state.Status),
		SnapshotMode:           string(state.SnapshotMode),
		UpdatedAt:              state.UpdatedAt,
		Error:                  state.Error,
		DiscoveredPullRequests: state.DiscoveredPullRequests,
		DiscoveredWorktrees:    state.DiscoveredWorktrees,
		PendingSnapshots:       state.PendingSnapshots,
		CapturedSnapshots:      state.CapturedSnapshots,
		Warnings:               append([]string{}, state.Warnings...),
	}
}

func serializeProject(project storage.ProjectRecord, cfg config.Config, defaultBaseBranch string) projectResponse {
	metadata := parseProjectMetadata(project.MetadataJSON)

	baseBranch := defaultBaseBranch
	if project.BaseBranch != nil && strings.TrimSpace(*project.BaseBranch) != "" {
		baseBranch = *project.BaseBranch
	}

	response := projectResponse{
		ID:              project.ID,
		Name:            project.Name,
		RepoPath:        project.RepoPath,
		BaseBranch:      baseBranch,
		Archived:        project.Archived,
		Provider:        resolveProjectProviderKind(cfg, project.ID, metadata),
		Repo:            stringMetadataPtr(metadata, "repo"),
		WorktreeRoot:    stringMetadataPtr(metadata, "worktreeRoot"),
		GatekeeperTrust: resolveProjectGatekeeperTrust(cfg, project.ID, metadata),
		Validation:      serializeProjectValidation(metadata, cfg),
		CreatedAt:       project.CreatedAt,
		UpdatedAt:       project.UpdatedAt,
	}
	if state := projects.DiscoveryStateFromRecord(project); state.Status != "" {
		serialized := serializeDiscovery(state)
		response.Discovery = &serialized
	}
	return response
}

func resolveProjectGatekeeperTrust(cfg config.Config, projectID string, metadata map[string]any) string {
	if roles, ok := metadata["roles"].(map[string]any); ok {
		if gatekeeper, ok := roles["gatekeeper"].(map[string]any); ok {
			if trust, ok := gatekeeper["trust"].(string); ok {
				return serializeGatekeeperTrust(trust)
			}
		}
	}
	return serializeGatekeeperTrust(string(config.ProjectRoleConfigs(cfg, projectID).Gatekeeper.Trust))
}

func serializeGatekeeperTrust(trust string) string {
	switch config.GatekeeperTrustLevel(strings.ToLower(strings.TrimSpace(trust))) {
	case config.GatekeeperTrustAdvise, config.GatekeeperTrustAuto:
		return strings.ToLower(strings.TrimSpace(trust))
	default:
		return ""
	}
}

func serializeProjectValidation(metadata map[string]any, cfg config.Config) *projectValidationResponse {
	if raw, ok := metadata["validation"]; ok && raw != nil {
		encoded, err := json.Marshal(raw)
		if err == nil {
			var policy config.ProjectValidationConfig
			if json.Unmarshal(encoded, &policy) == nil {
				return &projectValidationResponse{Commands: append([]string(nil), policy.Commands...), OptOut: policy.OptOut, Source: "project"}
			}
		}
	}
	if commands := config.ResolveValidationCommands(cfg); len(commands) > 0 {
		return &projectValidationResponse{Commands: commands, Source: "defaults"}
	}
	return nil
}

// resolveProjectProviderKind returns the display provider kind for a project:
// config binding first, then API metadata provider id, else github.
func resolveProjectProviderKind(cfg config.Config, projectID string, metadata map[string]any) string {
	for _, configured := range cfg.Projects {
		if configured.ID != projectID {
			continue
		}
		kind := config.ResolvedProjectProviderKind(cfg, configured)
		if kind != "" {
			return string(kind)
		}
		break
	}
	if providerID := strings.TrimSpace(stringMetadataValue(metadata, "provider")); providerID != "" {
		for _, provider := range cfg.Providers {
			if provider.ID == providerID && provider.Kind != "" {
				return string(provider.Kind)
			}
		}
	}
	return string(config.ProviderKindGitHub)
}

func stringMetadataValue(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	value, _ := metadata[key].(string)
	return value
}
