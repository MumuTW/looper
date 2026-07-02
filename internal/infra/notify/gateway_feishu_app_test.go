package notify

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

type capturedFeishuCall struct {
	method  string
	url     string
	headers map[string]string
	body    []byte
}

func newFeishuAppGateway(t *testing.T, cfg config.WebhookNotificationConfig, calls *[]capturedFeishuCall) *Gateway {
	t.Helper()

	rootDir := t.TempDir()
	coordinator := openNotifyCoordinator(t, rootDir)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 11, 12, 0, 0, 0, time.UTC)

	return NewGateway(Options{
		Config: config.NotificationConfig{
			InApp:     false,
			Osascript: config.OsascriptNotificationConfig{Enabled: false, ThrottleWindowSeconds: 60},
			Webhook:   cfg,
		},
		Repositories: repos,
		Now:          func() time.Time { return now },
		FeishuAppHTTP: func(_ context.Context, method, url string, headers map[string]string, body []byte) (int, []byte, error) {
			*calls = append(*calls, capturedFeishuCall{method: method, url: url, headers: headers, body: append([]byte(nil), body...)})
			if strings.Contains(url, "/auth/v3/tenant_access_token/internal") {
				return 200, []byte(`{"code":0,"msg":"ok","tenant_access_token":"t-abc123","expire":7200}`), nil
			}
			return 200, []byte(`{"code":0,"msg":"success"}`), nil
		},
	})
}

func appModeConfig() config.WebhookNotificationConfig {
	return config.WebhookNotificationConfig{
		Enabled:               true,
		Format:                "feishu",
		Mode:                  "app",
		AppIDEnv:              "LOOPER_TEST_FEISHU_APP_ID",
		AppSecretEnv:          "LOOPER_TEST_FEISHU_APP_SECRET",
		ChatID:                "oc_group_chat_123",
		ThrottleWindowSeconds: 60,
	}
}

func TestGatewayFeishuAppChannel(t *testing.T) {
	ctx := context.Background()

	actionRequired := SystemNotificationPayload{
		Level:      "action_required",
		Title:      "Looper Worker Needs Attention",
		Subtitle:   "task_1",
		Body:       "A worker paused for human input",
		EntityType: "task",
		EntityID:   "task_1",
		DedupeKey:  "worker.attention:task:task_1",
	}

	t.Run("app mode fetches token then posts interactive card", func(t *testing.T) {
		t.Setenv("LOOPER_TEST_FEISHU_APP_ID", "cli_app_id")
		t.Setenv("LOOPER_TEST_FEISHU_APP_SECRET", "app_secret_value")

		var calls []capturedFeishuCall
		gateway := newFeishuAppGateway(t, appModeConfig(), &calls)

		records := gateway.Notify(ctx, actionRequired)

		if len(calls) != 2 {
			t.Fatalf("feishu calls = %d, want 2 (token + message)", len(calls))
		}

		// First call: tenant_access_token with app id/secret from env.
		token := calls[0]
		if !strings.Contains(token.url, "/open-apis/auth/v3/tenant_access_token/internal") {
			t.Fatalf("first call url = %q, want token endpoint", token.url)
		}
		var tokenBody map[string]string
		if err := json.Unmarshal(token.body, &tokenBody); err != nil {
			t.Fatalf("token body not JSON: %v", err)
		}
		if tokenBody["app_id"] != "cli_app_id" || tokenBody["app_secret"] != "app_secret_value" {
			t.Fatalf("token body = %#v, want app id/secret from env", tokenBody)
		}

		// Second call: interactive message to the chat, bearer token attached.
		msg := calls[1]
		if !strings.Contains(msg.url, "/open-apis/im/v1/messages?receive_id_type=chat_id") {
			t.Fatalf("second call url = %q, want im messages endpoint", msg.url)
		}
		if msg.headers["Authorization"] != "Bearer t-abc123" {
			t.Fatalf("message Authorization = %q, want Bearer t-abc123", msg.headers["Authorization"])
		}
		var envelope struct {
			ReceiveID string `json:"receive_id"`
			MsgType   string `json:"msg_type"`
			Content   string `json:"content"`
		}
		if err := json.Unmarshal(msg.body, &envelope); err != nil {
			t.Fatalf("message body not JSON: %v", err)
		}
		if envelope.ReceiveID != "oc_group_chat_123" {
			t.Fatalf("receive_id = %q, want oc_group_chat_123", envelope.ReceiveID)
		}
		if envelope.MsgType != "interactive" {
			t.Fatalf("msg_type = %q, want interactive", envelope.MsgType)
		}
		// content is a JSON string containing the card; assert title/body + orange header.
		if !strings.Contains(envelope.Content, "Looper Worker Needs Attention") {
			t.Fatalf("card content missing title: %s", envelope.Content)
		}
		if !strings.Contains(envelope.Content, "A worker paused for human input") {
			t.Fatalf("card content missing body: %s", envelope.Content)
		}
		if !strings.Contains(envelope.Content, `"template":"orange"`) {
			t.Fatalf("card content missing orange header for action_required: %s", envelope.Content)
		}

		if got := notificationStatus(records, "feishu_app"); got != "success" {
			t.Fatalf("feishu_app status = %q, want success", got)
		}
		if notificationStatus(records, "webhook") != "" {
			t.Fatal("webhook channel should not be recorded in app mode")
		}
	})

	t.Run("failure level uses red header", func(t *testing.T) {
		t.Setenv("LOOPER_TEST_FEISHU_APP_ID", "cli_app_id")
		t.Setenv("LOOPER_TEST_FEISHU_APP_SECRET", "app_secret_value")

		var calls []capturedFeishuCall
		gateway := newFeishuAppGateway(t, appModeConfig(), &calls)

		gateway.Notify(ctx, SystemNotificationPayload{Level: "failure", Title: "Run failed", Body: "boom"})
		if len(calls) != 2 {
			t.Fatalf("feishu calls = %d, want 2", len(calls))
		}
		var envelope struct {
			Content string `json:"content"`
		}
		if err := json.Unmarshal(calls[1].body, &envelope); err != nil {
			t.Fatalf("message body not JSON: %v", err)
		}
		if !strings.Contains(envelope.Content, `"template":"red"`) {
			t.Fatalf("failure card missing red header: %s", envelope.Content)
		}
	})

	t.Run("disabled records skipped without any call", func(t *testing.T) {
		cfg := appModeConfig()
		cfg.Enabled = false
		var calls []capturedFeishuCall
		gateway := newFeishuAppGateway(t, cfg, &calls)

		records := gateway.Notify(ctx, actionRequired)
		if len(calls) != 0 {
			t.Fatalf("feishu calls = %d, want 0", len(calls))
		}
		if got := notificationStatus(records, "feishu_app"); got != "skipped" {
			t.Fatalf("feishu_app status = %q, want skipped", got)
		}
		if got := notificationError(records, "feishu_app"); got != "disabled" {
			t.Fatalf("feishu_app error = %q, want disabled", got)
		}
	})

	t.Run("missing credentials records skipped", func(t *testing.T) {
		// Env vars intentionally unset.
		var calls []capturedFeishuCall
		gateway := newFeishuAppGateway(t, appModeConfig(), &calls)

		records := gateway.Notify(ctx, actionRequired)
		if len(calls) != 0 {
			t.Fatalf("feishu calls = %d, want 0", len(calls))
		}
		if got := notificationStatus(records, "feishu_app"); got != "skipped" {
			t.Fatalf("feishu_app status = %q, want skipped", got)
		}
		if got := notificationError(records, "feishu_app"); got != "no app credentials" {
			t.Fatalf("feishu_app error = %q, want no app credentials", got)
		}
	})

	t.Run("info level filtered out", func(t *testing.T) {
		t.Setenv("LOOPER_TEST_FEISHU_APP_ID", "cli_app_id")
		t.Setenv("LOOPER_TEST_FEISHU_APP_SECRET", "app_secret_value")

		var calls []capturedFeishuCall
		gateway := newFeishuAppGateway(t, appModeConfig(), &calls)

		records := gateway.Notify(ctx, SystemNotificationPayload{Level: "info", Title: "progress", Body: "nothing"})
		if len(calls) != 0 {
			t.Fatalf("feishu calls = %d, want 0", len(calls))
		}
		if got := notificationStatus(records, "feishu_app"); got != "skipped" {
			t.Fatalf("feishu_app status = %q, want skipped", got)
		}
		if got := notificationError(records, "feishu_app"); got != "level filtered" {
			t.Fatalf("feishu_app error = %q, want level filtered", got)
		}
	})
}
