package runtime

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/storage"
	"github.com/MumuTW/looper/internal/webhookforward"
)

func TestValidGitHubSignature(t *testing.T) {
	t.Parallel()

	secret := "top-secret"
	body := []byte(`{"repository":{"full_name":"acme/looper"}}`)
	signature := testGitHubSignature(secret, body)

	if !validGitHubSignature(secret, body, signature) {
		t.Fatal("validGitHubSignature() = false, want true for valid sha256 signature")
	}
	if validGitHubSignature(secret, []byte(`{"repository":{"full_name":"acme/other"}}`), signature) {
		t.Fatal("validGitHubSignature() = true, want false for tampered body")
	}
	if validGitHubSignature(secret, body, "") {
		t.Fatal("validGitHubSignature() = true, want false for missing signature")
	}
	if validGitHubSignature(secret, body, "sha1=deadbeef") {
		t.Fatal("validGitHubSignature() = true, want false for wrong signature algorithm")
	}
}

func TestRepoFromWebhookTunnelPath(t *testing.T) {
	t.Parallel()

	if got, ok := repoFromWebhookTunnelPath("/webhook/acme/looper"); !ok || got != "acme/looper" {
		t.Fatalf("repoFromWebhookTunnelPath() = (%q, %v), want (%q, true)", got, ok, "acme/looper")
	}
	if got, ok := repoFromWebhookTunnelPath("/webhook/host/github/acme/looper"); !ok || got != "github/acme/looper" {
		t.Fatalf("repoFromWebhookTunnelPath() = (%q, %v), want (%q, true)", got, ok, "github/acme/looper")
	}

	for _, path := range []string{"", "/webhook", "/webhook/acme", "/hook/acme/looper", "/webhook//looper", "/webhook/acme/", "/webhook/acme/looper/extra", "/webhook/github/acme/looper", "/webhook/host//acme/looper"} {
		if got, ok := repoFromWebhookTunnelPath(path); ok || got != "" {
			t.Fatalf("repoFromWebhookTunnelPath(%q) = (%q, %v), want (\"\", false)", path, got, ok)
		}
	}
}

func TestWebhookTunnelPayloadRepo(t *testing.T) {
	t.Parallel()

	if got := webhookTunnelPayloadRepo("code.example.test/acme/looper"); got != "acme/looper" {
		t.Fatalf("webhookTunnelPayloadRepo() = %q, want acme/looper", got)
	}
	if got := webhookTunnelPayloadRepo("acme/looper"); got != "acme/looper" {
		t.Fatalf("webhookTunnelPayloadRepo() = %q, want acme/looper", got)
	}
}

func TestWebhookTunnelManagedURLTrimsTrailingSlash(t *testing.T) {
	t.Parallel()

	cfg := config.Config{}
	cfg.Webhook.PublicBaseURL = " https://example.com/base// "

	if got := webhookTunnelManagedURL(cfg, "acme/looper"); got != "https://example.com/base/webhook/acme/looper" {
		t.Fatalf("webhookTunnelManagedURL() = %q, want %q", got, "https://example.com/base/webhook/acme/looper")
	}
	if got := webhookTunnelManagedURL(cfg, "github/acme/looper"); got != "https://example.com/base/webhook/host/github/acme/looper" {
		t.Fatalf("webhookTunnelManagedURL() = %q, want host-qualified route", got)
	}
}

func TestConfiguredWebhookTunnelReposUseProviderHostname(t *testing.T) {
	t.Parallel()

	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Providers = []config.ProviderConfig{{ID: "ghe", Kind: config.ProviderKindGitHub, BaseURL: "https://github/api/v3"}}
	cfg.Projects = []config.ProjectRefConfig{{ID: "project_1", Name: "Looper", Provider: "ghe", Repo: "acme/looper", RepoPath: t.TempDir(), Webhook: config.ProjectWebhookConfig{Mode: config.WebhookModeTunnel}}}

	if got := configuredWebhookReposForMode(cfg, config.WebhookModeTunnel); !slices.Equal(got, []string{"github/acme/looper"}) {
		t.Fatalf("configuredWebhookReposForMode() = %#v, want provider-qualified tunnel repo", got)
	}
	if got := configuredWebhookReposForMode(cfg, config.WebhookModeGHForward); len(got) != 0 {
		t.Fatalf("configuredWebhookReposForMode(gh-forward) = %#v, want no tunnel project", got)
	}
}

func TestWebhookRuntimeReconcileUsesProviderQualifiedTunnelRepo(t *testing.T) {
	t.Parallel()

	ctx, repos, cfg := setupWebhookTunnelTestRepos(t)
	cfg.Providers = []config.ProviderConfig{{ID: "ghe", Kind: config.ProviderKindGitHub, BaseURL: "https://github/api/v3"}}
	cfg.Projects = []config.ProjectRefConfig{{ID: "project_1", Name: "Looper", Provider: "ghe", Repo: "acme/looper", RepoPath: t.TempDir()}}
	client := &fakeWebhookTunnelGitHubClient{}
	runtime := newWebhookRuntime(cfg, &testLogger{}, time.Now)
	runtime.bootstrapDone = true
	runtime.tunnelClient = client
	defer runtime.stopTunnelServer()

	if err := runtime.Reconcile(repos); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if client.createCalls != 1 || client.lastCreate.repo != "github/acme/looper" || client.lastCreate.url != "/webhook/host/github/acme/looper" {
		t.Fatalf("CreateHook = %#v calls=%d, want provider-qualified tunnel hook", client.lastCreate, client.createCalls)
	}
	if _, ok, err := repos.WebhookTunnelHooks.Get(ctx, "github/acme/looper"); err != nil {
		t.Fatalf("WebhookTunnelHooks.Get() error = %v", err)
	} else if !ok {
		t.Fatal("WebhookTunnelHooks.Get() found = false, want provider-qualified record")
	}
}

func TestWebhookTunnelRequestPathHonorsPublicBaseURLPath(t *testing.T) {
	t.Parallel()

	cfg := config.Config{}
	cfg.Webhook.PublicBaseURL = " https://example.com/base// "

	if got, ok := webhookTunnelRequestPath(cfg, "/base/webhook/acme/looper"); !ok || got != "/webhook/acme/looper" {
		t.Fatalf("webhookTunnelRequestPath() = (%q, %v), want (%q, true)", got, ok, "/webhook/acme/looper")
	}
	if got, ok := webhookTunnelRequestPath(cfg, "/webhook/acme/looper"); ok || got != "" {
		t.Fatalf("webhookTunnelRequestPath() = (%q, %v), want (\"\", false)", got, ok)
	}
	if got, ok := webhookTunnelRequestPath(config.Config{}, "/webhook/acme/looper"); !ok || got != "/webhook/acme/looper" {
		t.Fatalf("webhookTunnelRequestPath() without prefix = (%q, %v), want (%q, true)", got, ok, "/webhook/acme/looper")
	}
}

func setupWebhookTunnelTestRepos(t *testing.T) (context.Context, *storage.Repositories, config.Config) {
	t.Helper()

	tempDir := t.TempDir()
	cfg, err := config.DefaultConfig(tempDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(tempDir, "looper.sqlite")
	cfg.Webhook.Enabled = true
	cfg.Webhook.Mode = config.WebhookModeTunnel
	cfg.Webhook.ListenPort = 0
	ghPath := "/usr/bin/gh"
	cfg.Tools.GHPath = &ghPath
	coordinator := openMigratedCoordinator(t, cfg.Storage.DBPath, filepath.Join(tempDir, "backups"))
	t.Cleanup(func() {
		if err := coordinator.Close(); err != nil {
			t.Fatalf("coordinator.Close() error = %v", err)
		}
	})
	return context.Background(), storage.NewRepositories(coordinator.DB()), cfg
}

type fakeWebhookTunnelGitHubClient struct {
	getHook        webhookTunnelGitHubHook
	getFound       bool
	createHook     webhookTunnelGitHubHook
	createErr      error
	updateHook     webhookTunnelGitHubHook
	updateErr      error
	deleteErr      error
	getDeadline    bool
	createDeadline bool
	updateDeadline bool
	getCalls       int
	createCalls    int
	updateCalls    int
	deleteCalls    int
	lastUpdate     fakeWebhookTunnelUpdateCall
	lastCreate     fakeWebhookTunnelCreateCall
	deletedHooks   []int64
}

type fakeWebhookTunnelUpdateCall struct {
	repo   string
	id     int64
	url    string
	secret string
	active bool
}

type fakeWebhookTunnelCreateCall struct {
	repo string
	url  string
}

func (f *fakeWebhookTunnelGitHubClient) GetHook(ctx context.Context, _ string, _ int64) (webhookTunnelGitHubHook, bool, error) {
	f.getCalls++
	_, f.getDeadline = ctx.Deadline()
	return f.getHook, f.getFound, nil
}

func (f *fakeWebhookTunnelGitHubClient) CreateHook(ctx context.Context, repo string, url string, _ string, _ []string) (webhookTunnelGitHubHook, error) {
	f.createCalls++
	f.lastCreate = fakeWebhookTunnelCreateCall{repo: repo, url: url}
	_, f.createDeadline = ctx.Deadline()
	if f.createHook.ID == 0 {
		f.createHook.ID = 999
	}
	return f.createHook, f.createErr
}

func (f *fakeWebhookTunnelGitHubClient) UpdateHook(ctx context.Context, repo string, id int64, url string, secret string, _ []string, active bool) (webhookTunnelGitHubHook, error) {
	f.updateCalls++
	_, f.updateDeadline = ctx.Deadline()
	f.lastUpdate = fakeWebhookTunnelUpdateCall{repo: repo, id: id, url: url, secret: secret, active: active}
	return f.updateHook, f.updateErr
}

func (f *fakeWebhookTunnelGitHubClient) DeleteHook(_ context.Context, _ string, id int64) error {
	f.deleteCalls++
	f.deletedHooks = append(f.deletedHooks, id)
	return f.deleteErr
}

type testTunnelForwarder struct {
	result      webhookforward.ForwardResult
	err         error
	calls       int
	lastRequest webhookforward.DeliveryRequest
}

func (f *testTunnelForwarder) Forward(_ context.Context, req webhookforward.DeliveryRequest) (webhookforward.ForwardResult, error) {
	f.calls++
	f.lastRequest = req
	if f.err != nil {
		return webhookforward.ForwardResult{}, f.err
	}
	return f.result, nil
}

func (f *testTunnelForwarder) Stats() webhookforward.Stats { return webhookforward.Stats{} }

func (f *testTunnelForwarder) CancelExecute() {}

func (f *testTunnelForwarder) Close() {}

func (f *testTunnelForwarder) reset() {
	f.calls = 0
	f.lastRequest = webhookforward.DeliveryRequest{}
	f.err = nil
}

func testGitHubSignature(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
