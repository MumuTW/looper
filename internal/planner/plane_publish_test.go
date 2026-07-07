package planner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nexu-io/looper/internal/infra/planedoc"
	"github.com/nexu-io/looper/internal/storage"
)

func TestReadPlannerSpecFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "specs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "specs", "s.md"), []byte("# Tech Spec\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readPlannerSpecFile(dir, "specs/s.md")
	if err != nil || got != "# Tech Spec\nbody" {
		t.Fatalf("readPlannerSpecFile = %q, %v", got, err)
	}
	// missing file → empty, no error
	if got, err := readPlannerSpecFile(dir, "specs/missing.md"); err != nil || got != "" {
		t.Fatalf("missing = %q, %v; want empty", got, err)
	}
	// empty path → empty
	if got, err := readPlannerSpecFile(dir, ""); err != nil || got != "" {
		t.Fatalf("empty path = %q, %v", got, err)
	}
}

func TestPublishTechSpecToPlaneWritesPageAndLinks(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "specs"), 0o755)
	os.WriteFile(filepath.Join(dir, "specs", "s.md"), []byte("# Tech Spec"), 0o644)

	// FindSpecLink(tech) empty → CreatePage → UpsertSpecLink list empty → link create
	gw, calls := scriptedGateway(`{"results":[]}`, `{"id":"pg-1","name":"Tech Spec: 登录"}`, `{"results":[]}`, `{"id":"l-new"}`)
	r := &Runner{planeDoc: func(string) (*planedoc.Gateway, string, bool) { return gw, "plane-proj", true }}
	in := stepInput{Project: storage.ProjectRecord{ID: "proj-1"}}
	issue := checkpointIssue{Title: "登录", URL: "https://plane.x/w/projects/pp/issues/wi-9", SpecPath: "specs/s.md"}
	wt := checkpointWorktree{Path: dir, SpecPath: "specs/s.md"}

	if err := r.publishTechSpecToPlane(context.Background(), in, issue, wt); err != nil {
		t.Fatalf("publishTechSpecToPlane error = %v", err)
	}
	if len(*calls) != 4 {
		t.Fatalf("calls = %d, want find + page create + upsert list + link create", len(*calls))
	}
	if (*calls)[1][1] != "page" || (*calls)[1][2] != "create" {
		t.Fatalf("2nd call = %v, want page create", (*calls)[1])
	}
}

func TestPublishTechSpecToPlaneIdempotentAndNonPlane(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "specs"), 0o755)
	os.WriteFile(filepath.Join(dir, "specs", "s.md"), []byte("# Tech Spec"), 0o644)
	issue := checkpointIssue{Title: "登录", URL: "https://plane.x/w/projects/pp/issues/wi-9", SpecPath: "specs/s.md"}
	wt := checkpointWorktree{Path: dir, SpecPath: "specs/s.md"}
	in := stepInput{Project: storage.ProjectRecord{ID: "proj-1"}}

	// already has a tech-spec link → no-op (only the find call)
	gw, calls := scriptedGateway(`{"results":[{"id":"l1","title":"looper:tech-spec","url":"https://plane.x/pages/p1"}]}`)
	r := &Runner{planeDoc: func(string) (*planedoc.Gateway, string, bool) { return gw, "plane-proj", true }}
	if err := r.publishTechSpecToPlane(context.Background(), in, issue, wt); err != nil {
		t.Fatalf("idempotent error = %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("idempotent calls = %d, want only the find (no page create)", len(*calls))
	}

	// github project (planeDoc false) → no-op
	rGH := &Runner{planeDoc: func(string) (*planedoc.Gateway, string, bool) { return nil, "", false }}
	if err := rGH.publishTechSpecToPlane(context.Background(), in, issue, wt); err != nil {
		t.Fatalf("github error = %v", err)
	}
}
