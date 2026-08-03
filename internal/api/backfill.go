package api

import (
	"fmt"
	"net/http"
	"strings"

	coordinatorrole "github.com/MumuTW/looper/internal/coordinator"
	pkgapi "github.com/MumuTW/looper/pkg/api"
)

type backfillRequest struct {
	ProjectID     string  `json:"projectId"`
	Repo          string  `json:"repo"`
	IssueNumbers  []int64 `json:"issueNumbers,omitempty"`
	LabelFilter   string  `json:"labelFilter,omitempty"`
	MaxAgeDays    int     `json:"maxAgeDays,omitempty"`
	MaxCount      int     `json:"maxCount,omitempty"`
	SkipTriaged   bool    `json:"skipTriaged,omitempty"`
	ForceRetriage bool    `json:"forceRetriage,omitempty"`
	Confirm       bool    `json:"confirm"`
}

func (h *Handler) buildBackfillResponse(r *http.Request) (coordinatorrole.BackfillResult, error) {
	if r.Method != http.MethodPost {
		return coordinatorrole.BackfillResult{}, apiError{code: pkgapi.ErrorCodeMethodNotAllowed, status: http.StatusMethodNotAllowed, message: fmt.Sprintf("Unsupported method for %s", r.URL.EscapedPath())}
	}
	if h.context.BackfillIssues == nil {
		return coordinatorrole.BackfillResult{}, apiError{code: pkgapi.ErrorCodeServiceUnavailable, status: http.StatusServiceUnavailable, message: "Coordinator backfill is unavailable"}
	}
	var body backfillRequest
	if aerr := decodeJSONMutationBody(r, &body, true); aerr != nil {
		return coordinatorrole.BackfillResult{}, *aerr
	}
	body.ProjectID = strings.TrimSpace(body.ProjectID)
	body.Repo = strings.TrimSpace(body.Repo)
	body.LabelFilter = strings.TrimSpace(body.LabelFilter)
	if body.ProjectID == "" {
		return coordinatorrole.BackfillResult{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "projectId is required"}
	}
	if body.Repo == "" {
		return coordinatorrole.BackfillResult{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "repo is required"}
	}
	if body.MaxAgeDays < 0 || body.MaxAgeDays > 365 {
		return coordinatorrole.BackfillResult{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "maxAgeDays must be between 1 and 365 when provided"}
	}
	if body.MaxCount < 0 || body.MaxCount > 200 {
		return coordinatorrole.BackfillResult{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "maxCount must be between 1 and 200 when provided"}
	}
	for _, number := range body.IssueNumbers {
		if number <= 0 {
			return coordinatorrole.BackfillResult{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "issueNumbers must contain only positive integers"}
		}
	}
	// A request without explicit issue numbers or a label is a broad historical
	// scan. Force-retriage is also destructive to the normal triage invariant.
	// Both require an affirmative confirmation so a dashboard retry cannot widen
	// scope accidentally.
	if (len(body.IssueNumbers) == 0 && body.LabelFilter == "") || body.ForceRetriage {
		if !body.Confirm {
			return coordinatorrole.BackfillResult{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "confirm=true is required for broad or force-retriage backfill"}
		}
	}
	result, err := h.context.BackfillIssues(r.Context(), coordinatorrole.BackfillInput{
		ProjectID:     body.ProjectID,
		Repo:          body.Repo,
		IssueNumbers:  append([]int64(nil), body.IssueNumbers...),
		LabelFilter:   body.LabelFilter,
		MaxAgeDays:    body.MaxAgeDays,
		MaxCount:      body.MaxCount,
		SkipTriaged:   body.SkipTriaged,
		ForceRetriage: body.ForceRetriage,
	})
	if err != nil {
		return coordinatorrole.BackfillResult{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: err.Error()}
	}
	return result, nil
}
