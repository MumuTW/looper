package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	looperdruntime "github.com/MumuTW/looper/internal/runtime"
	pkgapi "github.com/MumuTW/looper/pkg/api"
)

func TestUpgradeDrainClosesAdmissionAndReportsSupervisorSnapshot(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	handler := NewHandler(Context{Config: cfg, Runtime: rt})

	post := httptest.NewRecorder()
	handler.ServeHTTP(post, httptest.NewRequest(http.MethodPost, "/api/v1/upgrade/drain", nil))
	if post.Code != http.StatusOK {
		t.Fatalf("POST status = %d body=%s", post.Code, post.Body.String())
	}
	var started pkgapi.Envelope[upgradeDrainResponse]
	if err := json.Unmarshal(post.Body.Bytes(), &started); err != nil {
		t.Fatalf("decode POST response: %v", err)
	}
	if started.Data.AdmissionState != string(looperdruntime.AdmissionDraining) || !started.Data.Drained {
		t.Fatalf("POST response = %#v", started.Data)
	}

	blocked := httptest.NewRecorder()
	handler.ServeHTTP(blocked, httptest.NewRequest(http.MethodPost, "/api/v1/loops", nil))
	if blocked.Code != http.StatusServiceUnavailable {
		t.Fatalf("ordinary POST after drain = %d body=%s, want 503", blocked.Code, blocked.Body.String())
	}

	// The drain control route itself stays available while draining: the CLI
	// resends this POST when its first response was lost past the deadline,
	// and BeginDrain is idempotent.
	repeat := httptest.NewRecorder()
	handler.ServeHTTP(repeat, httptest.NewRequest(http.MethodPost, "/api/v1/upgrade/drain", nil))
	if repeat.Code != http.StatusOK {
		t.Fatalf("repeated POST after drain = %d body=%s, want 200", repeat.Code, repeat.Body.String())
	}

	get := httptest.NewRecorder()
	handler.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/api/v1/upgrade/drain", nil))
	if get.Code != http.StatusOK {
		t.Fatalf("GET status = %d body=%s", get.Code, get.Body.String())
	}
	var observed pkgapi.Envelope[upgradeDrainResponse]
	if err := json.Unmarshal(get.Body.Bytes(), &observed); err != nil {
		t.Fatalf("decode GET response: %v", err)
	}
	if observed.Data.AdmissionState != string(looperdruntime.AdmissionDraining) || !observed.Data.Drained {
		t.Fatalf("GET response = %#v", observed.Data)
	}
}
