package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MumuTW/looper/internal/config"
	coordinatorrole "github.com/MumuTW/looper/internal/coordinator"
)

func TestBackfillRouteRequiresConfirmationForBroadScan(t *testing.T) {
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	called := false
	h := NewHandler(Context{Config: cfg, BackfillIssues: func(context.Context, coordinatorrole.BackfillInput) (coordinatorrole.BackfillResult, error) {
		called = true
		return coordinatorrole.BackfillResult{}, nil
	}})
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/backfill", strings.NewReader(`{"projectId":"demo","repo":"acme/looper"}`)))
	if recorder.Code != http.StatusBadRequest || called {
		t.Fatalf("status=%d called=%v body=%s; want confirmation rejection without callback", recorder.Code, called, recorder.Body.String())
	}
}

func TestBackfillRouteWiresExplicitSelectionAndReturnsResult(t *testing.T) {
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	var got coordinatorrole.BackfillInput
	h := NewHandler(Context{Config: cfg, BackfillIssues: func(_ context.Context, input coordinatorrole.BackfillInput) (coordinatorrole.BackfillResult, error) {
		got = input
		return coordinatorrole.BackfillResult{Considered: 1, Triaged: 1, ProcessedIssues: []int64{42}}, nil
	}})
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/backfill", strings.NewReader(`{"projectId":"demo","repo":"acme/looper","issueNumbers":[42],"labelFilter":" Backfill ","maxCount":1}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if got.ProjectID != "demo" || got.Repo != "acme/looper" || len(got.IssueNumbers) != 1 || got.IssueNumbers[0] != 42 || got.LabelFilter != "Backfill" {
		t.Fatalf("callback input = %#v", got)
	}
	var envelope struct {
		OK   bool `json:"ok"`
		Data struct {
			Triaged int `json:"triaged"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !envelope.OK || envelope.Data.Triaged != 1 {
		t.Fatalf("response = %#v", envelope)
	}
}

func TestBackfillRouteRequiresConfirmationForForceRetriage(t *testing.T) {
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	h := NewHandler(Context{Config: cfg, BackfillIssues: func(context.Context, coordinatorrole.BackfillInput) (coordinatorrole.BackfillResult, error) {
		t.Fatal("force-retriage callback should not run without confirmation")
		return coordinatorrole.BackfillResult{}, nil
	}})
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/backfill", strings.NewReader(`{"projectId":"demo","repo":"acme/looper","issueNumbers":[42],"forceRetriage":true}`)))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s; want confirmation rejection", recorder.Code, recorder.Body.String())
	}
}
