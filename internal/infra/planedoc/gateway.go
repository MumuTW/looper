// Package planedoc reads and writes Plane spec documents (Pages) and associates
// them with work items via native Plane work-item Links, by shelling out to the
// `plane` CLI — the same way the github package shells out to `gh`. This is how
// looper's tech-spec pipeline (plan §8.2/§8.4) stores specs in Plane instead of as
// repo files, and how it answers "which page is this work item's product spec"
// (the page↔work-item convention: a native link tagged looper:product-spec /
// looper:tech-spec, spike-verified 2026-07-07).
package planedoc

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/infra/shell"
)

const defaultPlaneCommandTimeout = 45 * time.Second

// Machine-parseable link titles marking which Plane page is a work item's spec.
// The reverse lookup filters a work item's links by these exact titles.
const (
	ProductSpecLinkTitle = "looper:product-spec"
	TechSpecLinkTitle    = "looper:tech-spec"
)

// RunFunc runs a `plane` invocation; injectable so tests fake the CLI.
type RunFunc func(context.Context, shell.Options) (shell.Result, error)

type Options struct {
	// PlanePath is the `plane` binary; defaults to "plane" (resolved on PATH).
	PlanePath string
	// APIBaseURL / APIKey / Workspace are passed explicitly to every call so looper
	// drives Plane deterministically with the teammate's own key, not plane.toml.
	APIBaseURL string
	APIKey     string
	Workspace  string
	Run        RunFunc
}

type Gateway struct {
	planePath  string
	apiBaseURL string
	apiKey     string
	workspace  string
	run        RunFunc
}

func New(o Options) *Gateway {
	planePath := strings.TrimSpace(o.PlanePath)
	if planePath == "" {
		planePath = "plane"
	}
	run := o.Run
	if run == nil {
		run = shell.Run
	}
	return &Gateway{
		planePath:  planePath,
		apiBaseURL: strings.TrimSpace(o.APIBaseURL),
		apiKey:     strings.TrimSpace(o.APIKey),
		workspace:  strings.TrimSpace(o.Workspace),
		run:        run,
	}
}

// Page is a Plane spec document.
type Page struct {
	ID   string
	Name string
	// URL is the human-clickable page URL (constructed; Plane's page API returns no
	// URL). The page id is embedded, so a link back to it is reverse-parseable.
	URL string
}

// Link is a native Plane work-item link.
type Link struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	URL   string `json:"url"`
}

// globalArgs are the auth/coordinate flags every call carries. Only non-empty ones
// are passed, so an unset field falls through to the CLI's plane.toml / env.
func (g *Gateway) globalArgs() []string {
	args := make([]string, 0, 6)
	if g.apiBaseURL != "" {
		args = append(args, "--api-base-url", g.apiBaseURL)
	}
	if g.apiKey != "" {
		args = append(args, "--api-key", g.apiKey)
	}
	if g.workspace != "" {
		args = append(args, "--workspace", g.workspace)
	}
	return args
}

func (g *Gateway) runPlane(ctx context.Context, stdin string, args ...string) (shell.Result, error) {
	return g.run(ctx, shell.Options{
		Command: g.planePath,
		Args:    args,
		Stdin:   stdin,
		Timeout: defaultPlaneCommandTimeout,
	})
}

// CreatePage creates a Plane page whose body is `bodyMarkdown` (converted to HTML
// server-side) and returns it with a constructed web URL.
func (g *Gateway) CreatePage(ctx context.Context, projectID, name, bodyMarkdown string) (Page, error) {
	if strings.TrimSpace(projectID) == "" || strings.TrimSpace(name) == "" {
		return Page{}, fmt.Errorf("planedoc: CreatePage requires project id and name")
	}
	args := []string{"api", "page", "create", "--project", projectID, "--name", name, "--body", bodyMarkdown, "--format", "md", "--json"}
	args = append(args, g.globalArgs()...)
	result, err := g.runPlane(ctx, "", args...)
	if err != nil {
		return Page{}, fmt.Errorf("planedoc: create page: %w", err)
	}
	var payload struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(result.Stdout), &payload); err != nil {
		return Page{}, fmt.Errorf("planedoc: decode created page: %w", err)
	}
	if strings.TrimSpace(payload.ID) == "" {
		return Page{}, fmt.Errorf("planedoc: created page has no id")
	}
	return Page{ID: payload.ID, Name: payload.Name, URL: g.pageWebURL(projectID, payload.ID)}, nil
}

// PageContent returns a page's body as HTML.
func (g *Gateway) PageContent(ctx context.Context, projectID, pageID string) (string, error) {
	if strings.TrimSpace(projectID) == "" || strings.TrimSpace(pageID) == "" {
		return "", fmt.Errorf("planedoc: PageContent requires project id and page id")
	}
	args := []string{"api", "page", "get", "--project", projectID, pageID, "--content"}
	args = append(args, g.globalArgs()...)
	result, err := g.runPlane(ctx, "", args...)
	if err != nil {
		return "", fmt.Errorf("planedoc: get page content: %w", err)
	}
	return strings.TrimRight(result.Stdout, "\n"), nil
}

// ListWorkItemLinks returns a work item's native links.
func (g *Gateway) ListWorkItemLinks(ctx context.Context, projectID, workItemID string) ([]Link, error) {
	if strings.TrimSpace(projectID) == "" || strings.TrimSpace(workItemID) == "" {
		return nil, fmt.Errorf("planedoc: ListWorkItemLinks requires project id and work item id")
	}
	args := []string{"api", "link", "list", "--project", projectID, "--work-item", workItemID, "--all", "--json"}
	args = append(args, g.globalArgs()...)
	result, err := g.runPlane(ctx, "", args...)
	if err != nil {
		return nil, fmt.Errorf("planedoc: list links: %w", err)
	}
	return decodeLinks(result.Stdout)
}

// FindSpecLink returns the URL of the work item's link tagged with `title`
// (e.g. ProductSpecLinkTitle), and whether one exists. This is the reverse lookup
// "which page is this work item's spec".
func (g *Gateway) FindSpecLink(ctx context.Context, projectID, workItemID, title string) (string, bool, error) {
	links, err := g.ListWorkItemLinks(ctx, projectID, workItemID)
	if err != nil {
		return "", false, err
	}
	for _, link := range links {
		if strings.EqualFold(strings.TrimSpace(link.Title), strings.TrimSpace(title)) {
			return link.URL, true, nil
		}
	}
	return "", false, nil
}

// UpsertSpecLink attaches (or re-points) the work item's spec link for `title` to
// `url`. Idempotent: a matching link with the same URL is left alone; a stale one
// is updated in place; otherwise a new link is created. This is how looper
// associates a spec page with its work item — including on the human's behalf when
// they just drop a spec in the thread (§8.3).
func (g *Gateway) UpsertSpecLink(ctx context.Context, projectID, workItemID, title, url string) error {
	if strings.TrimSpace(url) == "" || strings.TrimSpace(title) == "" {
		return fmt.Errorf("planedoc: UpsertSpecLink requires title and url")
	}
	links, err := g.ListWorkItemLinks(ctx, projectID, workItemID)
	if err != nil {
		return err
	}
	data := fmt.Sprintf(`{"url":%s,"title":%s}`, jsonString(url), jsonString(title))
	for _, link := range links {
		if !strings.EqualFold(strings.TrimSpace(link.Title), strings.TrimSpace(title)) {
			continue
		}
		if strings.TrimSpace(link.URL) == strings.TrimSpace(url) {
			return nil // already associated
		}
		args := []string{"api", "link", "update", "--project", projectID, "--work-item", workItemID, link.ID, "--data", data}
		args = append(args, g.globalArgs()...)
		if _, err := g.runPlane(ctx, "", args...); err != nil {
			return fmt.Errorf("planedoc: update spec link: %w", err)
		}
		return nil
	}
	args := []string{"api", "link", "create", "--project", projectID, "--work-item", workItemID, "--data", data, "--json"}
	args = append(args, g.globalArgs()...)
	if _, err := g.runPlane(ctx, "", args...); err != nil {
		return fmt.Errorf("planedoc: create spec link: %w", err)
	}
	return nil
}

// pageWebURL constructs a page's human URL from the API base (Plane's page API
// returns none). Best-effort; the page id is embedded so it round-trips.
func (g *Gateway) pageWebURL(projectID, pageID string) string {
	base := strings.TrimSuffix(strings.TrimRight(g.apiBaseURL, "/"), "/api/v1")
	if base == "" {
		base = "https://plane.powerformer.net"
	}
	ws := g.workspace
	if ws == "" {
		return fmt.Sprintf("%s/pages/%s", base, pageID)
	}
	return fmt.Sprintf("%s/%s/projects/%s/pages/%s", base, ws, projectID, pageID)
}

// decodeLinks parses a `link list` response, tolerating both a bare array and the
// paginated {results:[...]} envelope the CLI emits.
func decodeLinks(stdout string) ([]Link, error) {
	stdout = strings.TrimSpace(stdout)
	if stdout == "" {
		return nil, nil
	}
	var envelope struct {
		Results []Link `json:"results"`
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err == nil && envelope.Results != nil {
		return envelope.Results, nil
	}
	var bare []Link
	if err := json.Unmarshal([]byte(stdout), &bare); err != nil {
		return nil, fmt.Errorf("planedoc: decode links: %w", err)
	}
	return bare, nil
}

// jsonString safely JSON-encodes a string for embedding in a --data body.
func jsonString(s string) string {
	encoded, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(encoded)
}
