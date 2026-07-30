package runtime

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/intake/telegram"
	"github.com/MumuTW/looper/internal/outboundguard"
	"github.com/MumuTW/looper/internal/storage"
)

func intakeTickInput(t *testing.T, projects []storage.ProjectRecord, cfg *config.Config) defaultSchedulerTickInput {
	t.Helper()
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), filepath.Join(t.TempDir(), "looper.sqlite"), storage.SQLiteCoordinatorOptions{
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err := coordinator.MigrationRunner().RunPending(context.Background(), storage.RunPendingOptions{}); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}
	repos := storage.NewRepositories(coordinator.DB())
	for _, project := range projects {
		if err := repos.Projects.Upsert(context.Background(), project); err != nil {
			t.Fatalf("Projects.Upsert() error = %v", err)
		}
	}
	return defaultSchedulerTickInput{Repos: repos, Config: cfg}
}

func intakeProject(id, repo string, archived bool) storage.ProjectRecord {
	metadata := `{"repo":"` + repo + `"}`
	return storage.ProjectRecord{
		ID: id, Name: id, RepoPath: "/tmp/" + id, Archived: archived,
		MetadataJSON: &metadata,
		CreatedAt:    "2026-07-30T12:00:00.000Z", UpdatedAt: "2026-07-30T12:00:00.000Z",
	}
}

func TestResolveIntakeTargetReturnsTheProjectRepo(t *testing.T) {
	input := intakeTickInput(t, []storage.ProjectRecord{intakeProject("looper", "acme/looper", false)}, nil)

	target, err := resolveIntakeTarget(context.Background(), input, "looper")
	if err != nil {
		t.Fatalf("resolveIntakeTarget() error = %v", err)
	}
	if target.Unroutable != "" {
		t.Fatalf("target.Unroutable = %q, want routable", target.Unroutable)
	}
	if target.Repo != "acme/looper" || target.RepoPath != "/tmp/looper" {
		t.Fatalf("target = %+v", target)
	}
}

// Each of these is a permanent condition the sender is told about, as opposed to
// a lookup failure, which must surface as an error so the message is retried.
func TestResolveIntakeTargetReportsUnroutableProjects(t *testing.T) {
	cases := []struct {
		name      string
		projects  []storage.ProjectRecord
		projectID string
		want      string
	}{
		{name: "unknown", projects: nil, projectID: "ghost", want: "沒有這個 project"},
		{name: "archived", projects: []storage.ProjectRecord{intakeProject("old", "acme/old", true)}, projectID: "old", want: "已封存"},
		{name: "no repo binding", projects: []storage.ProjectRecord{{
			ID: "bare", Name: "bare", RepoPath: "/tmp/bare",
			CreatedAt: "2026-07-30T12:00:00.000Z", UpdatedAt: "2026-07-30T12:00:00.000Z",
		}}, projectID: "bare", want: "沒有繫結 repo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := intakeTickInput(t, tc.projects, nil)

			target, err := resolveIntakeTarget(context.Background(), input, tc.projectID)
			if err != nil {
				t.Fatalf("resolveIntakeTarget() error = %v", err)
			}
			if !strings.Contains(target.Unroutable, tc.want) {
				t.Fatalf("target.Unroutable = %q, want it to mention %q", target.Unroutable, tc.want)
			}
		})
	}
}

// A project that exists in storage but not in the current config is out of the
// captured catalog, which is the same authority the discovery lanes use. Intake
// must not file work into a repo the daemon would not then watch.
func TestResolveIntakeTargetRejectsProjectsOutsideTheCatalog(t *testing.T) {
	cfg := &config.Config{Projects: []config.ProjectRefConfig{{ID: "other", Repo: "acme/other"}}}
	input := intakeTickInput(t, []storage.ProjectRecord{intakeProject("looper", "acme/looper", false)}, cfg)

	target, err := resolveIntakeTarget(context.Background(), input, "looper")
	if err != nil {
		t.Fatalf("resolveIntakeTarget() error = %v", err)
	}
	if !strings.Contains(target.Unroutable, "設定檔") {
		t.Fatalf("target.Unroutable = %q, want a catalog explanation", target.Unroutable)
	}
}

// A message carrying a credential is rejected by the outbound guard on every
// attempt. Retrying it forever would wedge every later message behind it.
func TestClassifyIntakeCreateErrorMarksGuardRejectionsPermanent(t *testing.T) {
	t.Parallel()

	rejection := &outboundguard.Rejection{Field: "issue body", Reason: "contains a private key block"}
	classified := classifyIntakeCreateError(fmt.Errorf("create issue: %w", rejection))

	if !telegram.IsPermanent(classified) {
		t.Fatalf("classifyIntakeCreateError(guard rejection) is retryable; it would wedge the lane forever")
	}
	if !errors.Is(classified, error(rejection)) {
		t.Fatalf("classified error lost the underlying rejection: %v", classified)
	}
}

// Everything else — a rate limit, a 502, a dropped connection — succeeds on a
// later attempt and must not consume the sender's message.
func TestClassifyIntakeCreateErrorLeavesTransientFailuresRetryable(t *testing.T) {
	t.Parallel()

	classified := classifyIntakeCreateError(errors.New("gh: 502 bad gateway"))

	if telegram.IsPermanent(classified) {
		t.Fatal("a transient gateway failure was classified as permanent; the request would be dropped")
	}
}
