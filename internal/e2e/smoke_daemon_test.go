package e2e

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/dashboard"
	"github.com/MumuTW/looper/internal/e2e/harness"
	pkgapi "github.com/MumuTW/looper/pkg/api"
)

func TestSmokeLooperdBootsWithDefaultConfig(t *testing.T) {
	bins := harness.MustBinaries(t)
	home := harness.NewTempHome(t)
	port := harness.MustFreePort(t)
	cfg := harness.DefaultConfig(t, home, harness.ConfigOptions{Port: port})
	harness.WriteConfig(t, home.ConfigPath, cfg, nil)
	proc := harness.StartLooperd(t, bins, home, home.ConfigPath, nil, cfg.Server.Host, cfg.Server.Port)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	status, err := proc.WaitForReady(ctx)
	if err != nil {
		t.Fatalf("wait for ready: %v", err)
	}
	service, _ := status["service"].(map[string]any)
	if service == nil {
		t.Fatalf("status.service missing: %#v", status)
	}
	if strings.TrimSpace(anyString(service["version"])) == "" {
		t.Fatalf("status.service.version missing: %#v", service)
	}
	if _, ok := service["healthy"]; !ok {
		t.Fatalf("status.service.healthy missing: %#v", service)
	}
	requirePathExists(t, home.DBPath)
	requirePathExists(t, home.LogDir)
	requirePathExists(t, home.BackupDir)
	requirePathExists(t, home.WorktreeRoot)
	requirePathExists(t, loopLogsPath(home))
	proc.Stop(context.Background())
}

func TestSmokeLooperdBootsWithRolesConfig(t *testing.T) {
	bins := harness.MustBinaries(t)
	home := harness.NewTempHome(t)
	port := harness.MustFreePort(t)
	cfg := harness.DefaultConfig(t, home, harness.ConfigOptions{Port: port})
	cfg.Roles.Worker.AutoDiscovery = false
	cfg.Roles.Fixer.AutoDiscovery = false
	harness.WriteConfig(t, home.ConfigPath, cfg, nil)
	proc := harness.StartLooperd(t, bins, home, home.ConfigPath, nil, cfg.Server.Host, cfg.Server.Port)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := proc.WaitForReady(ctx); err != nil {
		t.Fatalf("wait for ready: %v", err)
	}
	proc.Stop(context.Background())
}

func TestSmokeLooperdBootsWithUnknownConfigFields(t *testing.T) {
	bins := harness.MustBinaries(t)
	home := harness.NewTempHome(t)
	port := harness.MustFreePort(t)
	cfg := harness.DefaultConfig(t, home, harness.ConfigOptions{Port: port})
	harness.WriteConfig(t, home.ConfigPath, cfg, map[string]any{
		"legacyTopLevel": map[string]any{"enabled": true},
	})
	proc := harness.StartLooperd(t, bins, home, home.ConfigPath, nil, cfg.Server.Host, cfg.Server.Port)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := proc.WaitForReady(ctx); err != nil {
		t.Fatalf("wait for ready: %v", err)
	}
	proc.Stop(context.Background())
}

func TestSmokeLooperdBootsWithExplicitToolPaths(t *testing.T) {
	bins := harness.MustBinaries(t)
	home := harness.NewTempHome(t)
	repo := harness.CreateSeededRepo(t, "git")
	port := harness.MustFreePort(t)
	fakeGH := harness.NewFakeGH(t, bins, harness.GHSchema{JSONFieldAllowlist: map[string][]string{}})
	fakeAgent := harness.NewFakeAgent(t, bins)
	cfg := configWithFakeTools(t, bins, home, repo, fakeGH, fakeAgent, port)
	harness.WriteConfig(t, home.ConfigPath, cfg, nil)
	proc := harness.StartLooperd(t, bins, home, home.ConfigPath, fakeGH.EnvMap(), cfg.Server.Host, cfg.Server.Port)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	status, err := proc.WaitForReady(ctx)
	if err != nil {
		t.Fatalf("wait for ready: %v", err)
	}
	tools, _ := status["tools"].(map[string]any)
	if tools == nil || tools["gh"] != true || tools["git"] != true || tools["osascript"] != true {
		t.Fatalf("status.tools = %#v, want all explicit tools present", tools)
	}
	proc.Stop(context.Background())
}

func TestSmokeLooperdBootsWithoutOptionalConfigSections(t *testing.T) {
	bins := harness.MustBinaries(t)
	home := harness.NewTempHome(t)
	port := harness.MustFreePort(t)
	cfg := harness.DefaultConfig(t, home, harness.ConfigOptions{Port: port})
	payload, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(payload, &doc); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	for _, key := range []string{"notifications", "disclosure", "tools", "reviewer", "instructions", "projects", "roles"} {
		delete(doc, key)
	}
	overrides, ok := doc["daemon"].(map[string]any)
	if !ok {
		t.Fatalf("daemon section missing from config doc: %#v", doc)
	}
	overrides["workingDirectory"] = home.WorkingDir
	formatted, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("marshal optional-sections config: %v", err)
	}
	if err := os.WriteFile(home.ConfigPath, formatted, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	proc := harness.StartLooperd(t, bins, home, home.ConfigPath, nil, cfg.Server.Host, cfg.Server.Port)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := proc.WaitForReady(ctx); err != nil {
		t.Fatalf("wait for ready: %v", err)
	}
	proc.Stop(context.Background())
}

func TestSmokeLooperdFailsFastWithInvalidOsascriptPathWhenEnabled(t *testing.T) {
	bins := harness.MustBinaries(t)
	home := harness.NewTempHome(t)
	port := harness.MustFreePort(t)
	cfg := harness.DefaultConfig(t, home, harness.ConfigOptions{Port: port, EnableOsascript: true, ToolPaths: harness.TestToolPaths{Osascript: filepath.Join(home.Root, "missing-osascript")}})
	harness.WriteConfig(t, home.ConfigPath, cfg, nil)
	proc := harness.StartLooperd(t, bins, home, home.ConfigPath, nil, cfg.Server.Host, cfg.Server.Port)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := proc.WaitForReady(ctx)
	if err == nil {
		t.Fatal("expected startup failure for invalid osascript path")
	}
	stderr, readErr := os.ReadFile(filepath.Join(home.ArtifactsDir, "looperd.stderr.log"))
	if readErr != nil {
		t.Fatalf("read stderr: %v", readErr)
	}
	if !strings.Contains(string(stderr), "tools.osascriptPath") {
		t.Fatalf("stderr = %s, want tools.osascriptPath validation", string(stderr))
	}
}

func anyString(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}

// TestSmokeLooperdServesEmbeddedDashboard verifies that a release-style
// looperd binary — one built after `pnpm run build` has placed the SPA under
// internal/dashboard/assets — serves the built dashboard rather than the
// development placeholder, and that the dashboard's required API data is
// reachable from the same daemon.
//
// It skips when production assets are not embedded. That is the intentional
// development-mode exception: a plain `go run ./cmd/looperd` or `go build`
// without first building web/dashboard embeds only the fallback placeholder,
// so the dashboard UI is unavailable even though the API stays healthy. CI's
// verify job runs `pnpm run build` before `go test ./...`, so this smoke runs
// there and guards the release embed path. See docs/installation.md.
func TestSmokeLooperdServesEmbeddedDashboard(t *testing.T) {
	if !dashboard.HasProductionAssets() {
		t.Skip("production dashboard assets not embedded; run `pnpm run build` in web/dashboard before building looperd (see docs/installation.md)")
	}

	bins := harness.MustBinaries(t)
	home := harness.NewTempHome(t)
	port := harness.MustFreePort(t)
	cfg := harness.DefaultConfig(t, home, harness.ConfigOptions{Port: port})
	harness.WriteConfig(t, home.ConfigPath, cfg, nil)
	proc := harness.StartLooperd(t, bins, home, home.ConfigPath, nil, cfg.Server.Host, cfg.Server.Port)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := proc.WaitForReady(ctx); err != nil {
		t.Fatalf("wait for ready: %v", err)
	}
	defer proc.Stop(context.Background())

	client := &http.Client{Timeout: 5 * time.Second}
	base := proc.BaseURL()

	// /dashboard/ serves the built SPA, not the development placeholder.
	indexBody := dashboardGetBody(t, client, base+"/dashboard/")
	if !strings.Contains(indexBody, `<div id="root"></div>`) {
		t.Fatalf("/dashboard/ body missing #root mount point: %q", indexBody)
	}
	if strings.Contains(indexBody, "Production dashboard assets are not embedded") {
		t.Fatalf("/dashboard/ served the development placeholder: %q", indexBody)
	}

	// The built index references a content-hashed module under /dashboard/assets/.
	scriptRe := regexp.MustCompile(`<script type="module" crossorigin src="(/dashboard/assets/index-[A-Za-z0-9_-]+\.js)"></script>`)
	scriptMatch := scriptRe.FindStringSubmatch(indexBody)
	if scriptMatch == nil {
		t.Fatalf("/dashboard/ index missing hashed module script: %q", indexBody)
	}

	// The hashed asset is served with immutable caching so the SPA loads its bundle.
	assetResp := dashboardGet(t, client, base+scriptMatch[1])
	defer assetResp.Body.Close()
	if assetResp.StatusCode != http.StatusOK {
		t.Fatalf("asset %s status = %d, want 200", scriptMatch[1], assetResp.StatusCode)
	}
	if ct := assetResp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Fatalf("asset %s Content-Type = %q, want text/javascript*", scriptMatch[1], ct)
	}
	if cc := assetResp.Header.Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Fatalf("asset %s Cache-Control = %q, want immutable", scriptMatch[1], cc)
	}

	// SPA fallback for a navigation path still serves the index shell.
	spaBody := dashboardGetBody(t, client, base+"/dashboard/loops/42")
	if !strings.Contains(spaBody, `<div id="root"></div>`) {
		t.Fatalf("/dashboard/loops/42 SPA fallback missing #root mount point: %q", spaBody)
	}

	// The dashboard's required API data endpoint answers from the same daemon.
	healthBody := dashboardGetBody(t, client, base+"/api/v1/healthz")
	var envelope pkgapi.Envelope[map[string]any]
	if err := json.Unmarshal([]byte(healthBody), &envelope); err != nil {
		t.Fatalf("decode /api/v1/healthz envelope: %v\nbody: %s", err, healthBody)
	}
	if !envelope.OK || envelope.Data == nil {
		t.Fatalf("/api/v1/healthz envelope = %+v, want ok=true with data", envelope)
	}
	healthy, _ := (*envelope.Data)["healthy"].(bool)
	if !healthy {
		t.Fatalf("/api/v1/healthz data.healthy = %v, want true", (*envelope.Data)["healthy"])
	}
}

func dashboardGet(t *testing.T, client *http.Client, url string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request for %s: %v", url, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return resp
}

func dashboardGetBody(t *testing.T, client *http.Client, url string) string {
	t.Helper()
	resp := dashboardGet(t, client, url)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200", url, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s body: %v", url, err)
	}
	return string(body)
}
