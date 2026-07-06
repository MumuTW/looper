package planedoc

import (
	"context"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/infra/shell"
)

// fakeRun records each invocation's args and returns a scripted stdout per call.
type fakeRun struct {
	calls   [][]string
	stdouts []string
}

func (f *fakeRun) run(_ context.Context, o shell.Options) (shell.Result, error) {
	f.calls = append(f.calls, o.Args)
	out := ""
	if len(f.stdouts) >= len(f.calls) {
		out = f.stdouts[len(f.calls)-1]
	}
	return shell.Result{Stdout: out}, nil
}

func newGateway(f *fakeRun) *Gateway {
	return New(Options{
		PlanePath:  "plane",
		APIBaseURL: "https://plane.powerformer.net/api/v1",
		APIKey:     "secret-key",
		Workspace:  "open-design",
		Run:        f.run,
	})
}

func argsContain(args []string, sub ...string) bool {
	joined := strings.Join(args, "\x00")
	for _, s := range sub {
		if !strings.Contains(joined, s) {
			return false
		}
	}
	return true
}

// argPairPresent checks that flag is immediately followed by value.
func argPairPresent(args []string, flag, value string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

func TestCreatePageBuildsArgsAndParsesID(t *testing.T) {
	f := &fakeRun{stdouts: []string{`{"id":"pg-1","name":"Tech Spec"}`}}
	g := newGateway(f)
	page, err := g.CreatePage(context.Background(), "proj-1", "Tech Spec", "# Body\n- x")
	if err != nil {
		t.Fatalf("CreatePage error = %v", err)
	}
	if page.ID != "pg-1" || page.Name != "Tech Spec" {
		t.Fatalf("page = %+v, want id pg-1", page)
	}
	if !strings.Contains(page.URL, "pg-1") || !strings.Contains(page.URL, "open-design") {
		t.Fatalf("page URL = %q, want it to embed the page id + workspace", page.URL)
	}
	args := f.calls[0]
	if !argsContain(args, "api", "page", "create", "--json") ||
		!argPairPresent(args, "--project", "proj-1") ||
		!argPairPresent(args, "--name", "Tech Spec") ||
		!argPairPresent(args, "--body", "# Body\n- x") ||
		!argPairPresent(args, "--api-key", "secret-key") ||
		!argPairPresent(args, "--workspace", "open-design") {
		t.Fatalf("create args = %v, missing expected flags", args)
	}
}

func TestPageContentReturnsBody(t *testing.T) {
	f := &fakeRun{stdouts: []string{"<h1>Spec</h1>\n"}}
	g := newGateway(f)
	html, err := g.PageContent(context.Background(), "proj-1", "pg-1")
	if err != nil {
		t.Fatalf("PageContent error = %v", err)
	}
	if html != "<h1>Spec</h1>" {
		t.Fatalf("content = %q, want trimmed html", html)
	}
	if !argsContain(f.calls[0], "api", "page", "get", "--content") || !argsContain(f.calls[0], "pg-1") {
		t.Fatalf("get args = %v", f.calls[0])
	}
}

func TestFindSpecLinkFiltersByTitle(t *testing.T) {
	f := &fakeRun{stdouts: []string{`{"results":[{"id":"l1","title":"looper:product-spec","url":"https://x/pages/p9"},{"id":"l2","title":"other","url":"https://y"}]}`}}
	g := newGateway(f)
	url, found, err := g.FindSpecLink(context.Background(), "proj-1", "wi-1", ProductSpecLinkTitle)
	if err != nil || !found || url != "https://x/pages/p9" {
		t.Fatalf("FindSpecLink = %q, %v, %v; want the product-spec url", url, found, err)
	}
	// A tag with no matching link returns not-found, no error.
	if _, found, err := g.FindSpecLink(context.Background(), "proj-1", "wi-1", TechSpecLinkTitle); err != nil || found {
		t.Fatalf("FindSpecLink(tech) = found %v, err %v; want not found", found, err)
	}
}

func TestUpsertSpecLinkCreatesWhenAbsent(t *testing.T) {
	f := &fakeRun{stdouts: []string{`{"results":[]}`, `{"id":"l-new"}`}}
	g := newGateway(f)
	if err := g.UpsertSpecLink(context.Background(), "proj-1", "wi-1", TechSpecLinkTitle, "https://x/pages/p1"); err != nil {
		t.Fatalf("UpsertSpecLink error = %v", err)
	}
	if len(f.calls) != 2 {
		t.Fatalf("calls = %d, want list + create", len(f.calls))
	}
	create := f.calls[1]
	if !argsContain(create, "api", "link", "create") || !argPairPresent(create, "--work-item", "wi-1") {
		t.Fatalf("create args = %v", create)
	}
	// The --data payload carries both url and title.
	if !argsContain(create, `"url":"https://x/pages/p1"`, `"title":"looper:tech-spec"`) {
		t.Fatalf("create --data missing url/title: %v", create)
	}
}

func TestUpsertSpecLinkNoopWhenSameURL(t *testing.T) {
	f := &fakeRun{stdouts: []string{`{"results":[{"id":"l1","title":"looper:tech-spec","url":"https://x/pages/p1"}]}`}}
	g := newGateway(f)
	if err := g.UpsertSpecLink(context.Background(), "proj-1", "wi-1", TechSpecLinkTitle, "https://x/pages/p1"); err != nil {
		t.Fatalf("UpsertSpecLink error = %v", err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("calls = %d, want only the list (no-op on identical link)", len(f.calls))
	}
}

func TestUpsertSpecLinkUpdatesStaleURL(t *testing.T) {
	f := &fakeRun{stdouts: []string{`{"results":[{"id":"l1","title":"looper:tech-spec","url":"https://x/OLD"}]}`, `{}`}}
	g := newGateway(f)
	if err := g.UpsertSpecLink(context.Background(), "proj-1", "wi-1", TechSpecLinkTitle, "https://x/NEW"); err != nil {
		t.Fatalf("UpsertSpecLink error = %v", err)
	}
	if len(f.calls) != 2 {
		t.Fatalf("calls = %d, want list + update", len(f.calls))
	}
	update := f.calls[1]
	if !argsContain(update, "api", "link", "update", "l1", `"url":"https://x/NEW"`) {
		t.Fatalf("update args = %v", update)
	}
}

func TestDecodeLinksToleratesBareArray(t *testing.T) {
	links, err := decodeLinks(`[{"id":"l1","title":"t","url":"u"}]`)
	if err != nil || len(links) != 1 || links[0].ID != "l1" {
		t.Fatalf("decodeLinks(bare) = %v, %v", links, err)
	}
	if links, err := decodeLinks("  "); err != nil || links != nil {
		t.Fatalf("decodeLinks(empty) = %v, %v", links, err)
	}
}
