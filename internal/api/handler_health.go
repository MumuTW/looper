package api

import (
	"context"
)

type healthResponse struct {
	Healthy   bool          `json:"healthy"`
	StartedAt *string       `json:"startedAt,omitempty"`
	Storage   storageHealth `json:"storage"`
}

type storageHealth struct {
	OK          bool            `json:"ok"`
	Mode        string          `json:"mode"`
	DBPath      string          `json:"dbPath"`
	LastUpdated string          `json:"lastUpdatedAt"`
	Details     *string         `json:"details,omitempty"`
	Migration   migrationHealth `json:"migration"`
}

type migrationHealth struct {
	LatestAvailableID string `json:"latestAvailableId,omitempty"`
	LatestAppliedID   string `json:"latestAppliedId,omitempty"`
	PendingCount      int    `json:"pendingCount"`
}

func (h *Handler) buildHealthResponse(ctx context.Context) (healthResponse, error) {
	state, err := h.loadStorageState(ctx)
	if err != nil {
		details := err.Error()
		state = storageState{
			Details: &details,
		}
	}

	startedAt := h.startedAtISO()

	return healthResponse{
		// /healthz is the liveness/storage contract. Admission readiness is
		// projected by /status and must not cause a live daemon to be evicted.
		Healthy:   state.OK,
		StartedAt: startedAt,
		Storage: storageHealth{
			OK:          state.OK,
			Mode:        h.context.Config.Storage.Mode,
			DBPath:      h.context.Config.Storage.DBPath,
			LastUpdated: h.now().UTC().Format(javaScriptISOString),
			Details:     state.Details,
			Migration: migrationHealth{
				LatestAvailableID: state.LatestAvailableID,
				LatestAppliedID:   state.LatestAppliedID,
				PendingCount:      len(state.PendingMigrationIDs),
			},
		},
	}, nil
}
