package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/projects"
	pkgapi "github.com/nexu-io/looper/pkg/api"
)

func (h *Handler) buildProjectsRouteResponse(r *http.Request) (any, error) {
	service := h.context.ProjectsService
	if service == nil {
		runtimeProjects := h.context.Runtime.Services().Projects
		if runtimeProjects != nil {
			service = runtimeProjects
		}
	}
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
	service := h.context.ProjectsService
	if service == nil {
		runtimeProjects := h.context.Runtime.Services().Projects
		if runtimeProjects != nil {
			service = runtimeProjects
		}
	}
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
