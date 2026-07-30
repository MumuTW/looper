package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/projects"
)

func TestHandlerProjectsCreateRejectsProviderFieldByPresence(t *testing.T) {
	tests := []string{
		`{"repoPath":"/tmp/repo","provider":"forgejo"}`,
		`{"repoPath":"/tmp/repo","provider":""}`,
		`{"repoPath":"/tmp/repo","provider":"   "}`,
		`{"repoPath":"/tmp/repo","provider":null}`,
	}
	for _, body := range tests {
		body := body
		t.Run(body, func(t *testing.T) {
			fixture := newTestFixture(t)
			called := false
			handler := NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime, ProjectsService: fakeProjectService{
				addProject: func(context.Context, projects.AddInput) (projects.AddResult, error) {
					called = true
					return projects.AddResult{}, nil
				},
			}})
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/projects", bytes.NewBufferString(body)))

			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", recorder.Code, recorder.Body.String())
			}
			if called {
				t.Fatal("project service called for prohibited provider field")
			}
			if !strings.Contains(recorder.Body.String(), `unknown field \"provider\"`) {
				t.Fatalf("body = %s, want strict unknown-field error", recorder.Body.String())
			}
		})
	}
}
