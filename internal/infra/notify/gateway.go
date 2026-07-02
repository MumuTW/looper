package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/eventlog"
	"github.com/nexu-io/looper/internal/infra/shell"
	"github.com/nexu-io/looper/internal/storage"
)

const (
	osascriptTimeout = 35 * time.Second
	webhookTimeout   = 10 * time.Second

	feishuAPIBase           = "https://open.feishu.cn"
	feishuTokenSafetyMargin = 60 * time.Second
)

type RunCommandFunc func(context.Context, shell.Options) (shell.Result, error)

// HTTPPostFunc delivers a webhook notification body to url and returns the
// HTTP status code. It is injectable so tests can avoid real network calls.
type HTTPPostFunc func(url string, body []byte) (int, error)

// FeishuAppHTTPFunc performs an HTTP request for the Feishu app-bot delivery
// (token fetch + message send) and returns the status code and response body.
// It is injectable so tests can avoid real network calls.
type FeishuAppHTTPFunc func(ctx context.Context, method, url string, headers map[string]string, body []byte) (int, []byte, error)

type Options struct {
	Config        config.NotificationConfig
	OsascriptPath string
	LogFilePath   string
	Repositories  *storage.Repositories
	Now           func() time.Time
	RunCommand    RunCommandFunc
	HTTPPost      HTTPPostFunc
	FeishuAppHTTP FeishuAppHTTPFunc
}

type SystemNotificationPayload struct {
	ID         string
	ProjectID  string
	LoopID     string
	RunID      string
	Level      string
	Title      string
	Subtitle   string
	Body       string
	Sound      string
	Group      string
	EntityType string
	EntityID   string
	DedupeKey  string
}

type Gateway struct {
	config        config.NotificationConfig
	osascriptPath string
	logFilePath   string
	repositories  *storage.Repositories
	now           func() time.Time
	runCommand    RunCommandFunc
	httpPost      HTTPPostFunc
	feishuAppHTTP FeishuAppHTTPFunc

	feishuTokenMu  sync.Mutex
	feishuToken    string
	feishuTokenExp time.Time
}

func NewGateway(options Options) *Gateway {
	now := options.Now
	if now == nil {
		now = time.Now
	}

	runCommand := options.RunCommand
	if runCommand == nil {
		runCommand = shell.Run
	}

	httpPost := options.HTTPPost
	if httpPost == nil {
		httpPost = defaultWebhookPost
	}

	feishuAppHTTP := options.FeishuAppHTTP
	if feishuAppHTTP == nil {
		feishuAppHTTP = defaultFeishuAppHTTP
	}

	return &Gateway{
		config:        options.Config,
		osascriptPath: options.OsascriptPath,
		logFilePath:   options.LogFilePath,
		repositories:  options.Repositories,
		now:           now,
		runCommand:    runCommand,
		httpPost:      httpPost,
		feishuAppHTTP: feishuAppHTTP,
	}
}

func (g *Gateway) Notify(ctx context.Context, payload SystemNotificationPayload) []storage.NotificationRecord {
	records := make([]storage.NotificationRecord, 0, 3)

	if record, ok := g.recordInApp(ctx, payload); ok {
		records = append(records, record)
	}

	if record, ok := g.recordOsascript(ctx, payload); ok {
		records = append(records, record)
	}

	if strings.EqualFold(strings.TrimSpace(g.config.Webhook.Mode), "app") {
		if record, ok := g.recordFeishuApp(ctx, payload); ok {
			records = append(records, record)
		}
	} else if record, ok := g.recordWebhook(ctx, payload); ok {
		records = append(records, record)
	}

	return records
}

func (g *Gateway) recordInApp(ctx context.Context, payload SystemNotificationPayload) (storage.NotificationRecord, bool) {
	nowISO := eventlog.FormatJavaScriptISOString(g.now())
	record := storage.NotificationRecord{
		ID:           firstNonEmpty(payload.ID, eventlog.NewEventID("notification")),
		ProjectID:    nilIfEmpty(payload.ProjectID),
		LoopID:       nilIfEmpty(payload.LoopID),
		RunID:        nilIfEmpty(payload.RunID),
		EntityType:   nilIfEmpty(payload.EntityType),
		EntityID:     nilIfEmpty(payload.EntityID),
		Channel:      "in_app",
		Level:        payload.Level,
		Title:        payload.Title,
		Subtitle:     nilIfEmpty(payload.Subtitle),
		Body:         payload.Body,
		Status:       ternaryString(g.config.InApp, "success", "skipped"),
		DedupeKey:    nilIfEmpty(payload.DedupeKey),
		ErrorMessage: ternaryPointer(!g.config.InApp, "disabled"),
		PayloadJSON:  stringPointer(mustMarshalPayload(payload)),
		SentAt:       ternaryTimePointer(g.config.InApp, nowISO),
		CreatedAt:    nowISO,
		UpdatedAt:    nowISO,
	}

	if err := g.persistNotification(ctx, record); err != nil {
		return storage.NotificationRecord{}, false
	}

	return record, true
}

func (g *Gateway) recordOsascript(ctx context.Context, payload SystemNotificationPayload) (storage.NotificationRecord, bool) {
	nowISO := eventlog.FormatJavaScriptISOString(g.now())
	id := eventlog.NewEventID("notification")

	if payload.DedupeKey != "" && g.repositories != nil && g.repositories.Notifications != nil {
		dedupeRecord, err := g.repositories.Notifications.GetLatestByDedupe(ctx, "osascript", payload.DedupeKey)
		if err == nil && dedupeRecord != nil {
			createdAt, parseErr := time.Parse(time.RFC3339Nano, dedupeRecord.CreatedAt)
			if parseErr == nil {
				throttleWindow := time.Duration(g.config.Osascript.ThrottleWindowSeconds) * time.Second
				if g.now().UTC().Sub(createdAt.UTC()) < throttleWindow {
					record := storage.NotificationRecord{
						ID:           id,
						ProjectID:    nilIfEmpty(payload.ProjectID),
						LoopID:       nilIfEmpty(payload.LoopID),
						RunID:        nilIfEmpty(payload.RunID),
						EntityType:   nilIfEmpty(payload.EntityType),
						EntityID:     nilIfEmpty(payload.EntityID),
						Channel:      "osascript",
						Level:        payload.Level,
						Title:        payload.Title,
						Subtitle:     nilIfEmpty(payload.Subtitle),
						Body:         payload.Body,
						Status:       "skipped",
						DedupeKey:    nilIfEmpty(payload.DedupeKey),
						ErrorMessage: stringPointer("deduped"),
						PayloadJSON:  stringPointer(mustMarshalPayload(payload)),
						CreatedAt:    nowISO,
						UpdatedAt:    nowISO,
					}
					if err := g.persistNotification(ctx, record); err != nil {
						return storage.NotificationRecord{}, false
					}
					return record, true
				}
			}
		}
	}

	if !g.config.Osascript.Enabled || strings.TrimSpace(g.osascriptPath) == "" {
		record := storage.NotificationRecord{
			ID:           id,
			ProjectID:    nilIfEmpty(payload.ProjectID),
			LoopID:       nilIfEmpty(payload.LoopID),
			RunID:        nilIfEmpty(payload.RunID),
			EntityType:   nilIfEmpty(payload.EntityType),
			EntityID:     nilIfEmpty(payload.EntityID),
			Channel:      "osascript",
			Level:        payload.Level,
			Title:        payload.Title,
			Subtitle:     nilIfEmpty(payload.Subtitle),
			Body:         payload.Body,
			Status:       "skipped",
			DedupeKey:    nilIfEmpty(payload.DedupeKey),
			ErrorMessage: stringPointer("disabled"),
			PayloadJSON:  stringPointer(mustMarshalPayload(payload)),
			CreatedAt:    nowISO,
			UpdatedAt:    nowISO,
		}
		if err := g.persistNotification(ctx, record); err != nil {
			return storage.NotificationRecord{}, false
		}
		return record, true
	}

	_, err := g.runCommand(ctx, shell.Options{
		Command: g.osascriptPath,
		Args:    []string{"-e", buildAppleScript(payload, g.config, g.logFilePath)},
		Timeout: osascriptTimeout,
	})
	if err != nil {
		record := storage.NotificationRecord{
			ID:           id,
			ProjectID:    nilIfEmpty(payload.ProjectID),
			LoopID:       nilIfEmpty(payload.LoopID),
			RunID:        nilIfEmpty(payload.RunID),
			EntityType:   nilIfEmpty(payload.EntityType),
			EntityID:     nilIfEmpty(payload.EntityID),
			Channel:      "osascript",
			Level:        payload.Level,
			Title:        payload.Title,
			Subtitle:     nilIfEmpty(payload.Subtitle),
			Body:         payload.Body,
			Status:       "failed",
			DedupeKey:    nilIfEmpty(payload.DedupeKey),
			ErrorMessage: stringPointer(err.Error()),
			PayloadJSON:  stringPointer(mustMarshalPayload(payload)),
			CreatedAt:    nowISO,
			UpdatedAt:    nowISO,
		}
		if persistErr := g.persistNotification(ctx, record); persistErr != nil {
			return storage.NotificationRecord{}, false
		}
		return record, true
	}

	record := storage.NotificationRecord{
		ID:          id,
		ProjectID:   nilIfEmpty(payload.ProjectID),
		LoopID:      nilIfEmpty(payload.LoopID),
		RunID:       nilIfEmpty(payload.RunID),
		EntityType:  nilIfEmpty(payload.EntityType),
		EntityID:    nilIfEmpty(payload.EntityID),
		Channel:     "osascript",
		Level:       payload.Level,
		Title:       payload.Title,
		Subtitle:    nilIfEmpty(payload.Subtitle),
		Body:        payload.Body,
		Status:      "success",
		DedupeKey:   nilIfEmpty(payload.DedupeKey),
		PayloadJSON: stringPointer(mustMarshalPayload(payload)),
		SentAt:      stringPointer(nowISO),
		CreatedAt:   nowISO,
		UpdatedAt:   nowISO,
	}
	if err := g.persistNotification(ctx, record); err != nil {
		return storage.NotificationRecord{}, false
	}

	return record, true
}

func (g *Gateway) recordWebhook(ctx context.Context, payload SystemNotificationPayload) (storage.NotificationRecord, bool) {
	nowISO := eventlog.FormatJavaScriptISOString(g.now())
	id := eventlog.NewEventID("notification")

	build := func(status, errorMessage string, sentAt *string) storage.NotificationRecord {
		return storage.NotificationRecord{
			ID:           id,
			ProjectID:    nilIfEmpty(payload.ProjectID),
			LoopID:       nilIfEmpty(payload.LoopID),
			RunID:        nilIfEmpty(payload.RunID),
			EntityType:   nilIfEmpty(payload.EntityType),
			EntityID:     nilIfEmpty(payload.EntityID),
			Channel:      "webhook",
			Level:        payload.Level,
			Title:        payload.Title,
			Subtitle:     nilIfEmpty(payload.Subtitle),
			Body:         payload.Body,
			Status:       status,
			DedupeKey:    nilIfEmpty(payload.DedupeKey),
			ErrorMessage: nilIfEmpty(errorMessage),
			PayloadJSON:  stringPointer(mustMarshalPayload(payload)),
			SentAt:       sentAt,
			CreatedAt:    nowISO,
			UpdatedAt:    nowISO,
		}
	}

	persist := func(record storage.NotificationRecord) (storage.NotificationRecord, bool) {
		if err := g.persistNotification(ctx, record); err != nil {
			return storage.NotificationRecord{}, false
		}
		return record, true
	}

	if !g.config.Webhook.Enabled {
		return persist(build("skipped", "disabled", nil))
	}

	url := ""
	if envName := strings.TrimSpace(g.config.Webhook.URLEnv); envName != "" {
		url = strings.TrimSpace(os.Getenv(envName))
	}
	if url == "" {
		return persist(build("skipped", "no url", nil))
	}

	if !webhookLevelAllowed(g.config.Webhook.Levels, payload.Level) {
		return persist(build("skipped", "level filtered", nil))
	}

	if payload.DedupeKey != "" && g.repositories != nil && g.repositories.Notifications != nil {
		dedupeRecord, err := g.repositories.Notifications.GetLatestByDedupe(ctx, "webhook", payload.DedupeKey)
		if err == nil && dedupeRecord != nil {
			createdAt, parseErr := time.Parse(time.RFC3339Nano, dedupeRecord.CreatedAt)
			if parseErr == nil {
				throttleWindow := time.Duration(g.config.Webhook.ThrottleWindowSeconds) * time.Second
				if g.now().UTC().Sub(createdAt.UTC()) < throttleWindow {
					return persist(build("skipped", "deduped", nil))
				}
			}
		}
	}

	body, err := buildWebhookBody(g.config.Webhook.Format, payload)
	if err != nil {
		return persist(build("failed", err.Error(), nil))
	}

	status, err := g.httpPost(url, body)
	if err != nil {
		return persist(build("failed", err.Error(), nil))
	}
	if status < 200 || status >= 300 {
		return persist(build("failed", fmt.Sprintf("webhook responded with status %d", status), nil))
	}

	return persist(build("success", "", stringPointer(nowISO)))
}

// recordFeishuApp delivers an interactive card through a Feishu app bot's IM
// API (posts to a group chat the bot belongs to). Like the other channels it is
// best-effort and records a NotificationRecord on the "feishu_app" channel.
func (g *Gateway) recordFeishuApp(ctx context.Context, payload SystemNotificationPayload) (storage.NotificationRecord, bool) {
	nowISO := eventlog.FormatJavaScriptISOString(g.now())
	id := eventlog.NewEventID("notification")

	build := func(status, errorMessage string, sentAt *string) storage.NotificationRecord {
		return storage.NotificationRecord{
			ID:           id,
			ProjectID:    nilIfEmpty(payload.ProjectID),
			LoopID:       nilIfEmpty(payload.LoopID),
			RunID:        nilIfEmpty(payload.RunID),
			EntityType:   nilIfEmpty(payload.EntityType),
			EntityID:     nilIfEmpty(payload.EntityID),
			Channel:      "feishu_app",
			Level:        payload.Level,
			Title:        payload.Title,
			Subtitle:     nilIfEmpty(payload.Subtitle),
			Body:         payload.Body,
			Status:       status,
			DedupeKey:    nilIfEmpty(payload.DedupeKey),
			ErrorMessage: nilIfEmpty(errorMessage),
			PayloadJSON:  stringPointer(mustMarshalPayload(payload)),
			SentAt:       sentAt,
			CreatedAt:    nowISO,
			UpdatedAt:    nowISO,
		}
	}

	persist := func(record storage.NotificationRecord) (storage.NotificationRecord, bool) {
		if err := g.persistNotification(ctx, record); err != nil {
			return storage.NotificationRecord{}, false
		}
		return record, true
	}

	cfg := g.config.Webhook
	if !cfg.Enabled {
		return persist(build("skipped", "disabled", nil))
	}

	appID := strings.TrimSpace(os.Getenv(strings.TrimSpace(cfg.AppIDEnv)))
	appSecret := strings.TrimSpace(os.Getenv(strings.TrimSpace(cfg.AppSecretEnv)))
	chatID := strings.TrimSpace(cfg.ChatID)
	if strings.TrimSpace(cfg.AppIDEnv) == "" || strings.TrimSpace(cfg.AppSecretEnv) == "" || chatID == "" {
		return persist(build("skipped", "no app config", nil))
	}
	if appID == "" || appSecret == "" {
		return persist(build("skipped", "no app credentials", nil))
	}

	if !webhookLevelAllowed(cfg.Levels, payload.Level) {
		return persist(build("skipped", "level filtered", nil))
	}

	if payload.DedupeKey != "" && g.repositories != nil && g.repositories.Notifications != nil {
		dedupeRecord, err := g.repositories.Notifications.GetLatestByDedupe(ctx, "feishu_app", payload.DedupeKey)
		if err == nil && dedupeRecord != nil {
			createdAt, parseErr := time.Parse(time.RFC3339Nano, dedupeRecord.CreatedAt)
			if parseErr == nil {
				throttleWindow := time.Duration(cfg.ThrottleWindowSeconds) * time.Second
				if g.now().UTC().Sub(createdAt.UTC()) < throttleWindow {
					return persist(build("skipped", "deduped", nil))
				}
			}
		}
	}

	token, err := g.feishuTenantToken(ctx, appID, appSecret)
	if err != nil {
		return persist(build("failed", err.Error(), nil))
	}

	card, err := buildFeishuCard(payload)
	if err != nil {
		return persist(build("failed", err.Error(), nil))
	}
	messageBody, err := json.Marshal(map[string]any{
		"receive_id": chatID,
		"msg_type":   "interactive",
		"content":    string(card),
	})
	if err != nil {
		return persist(build("failed", err.Error(), nil))
	}

	status, respBody, err := g.feishuAppHTTP(ctx, http.MethodPost, feishuAPIBase+"/open-apis/im/v1/messages?receive_id_type=chat_id", map[string]string{
		"Authorization": "Bearer " + token,
		"Content-Type":  "application/json; charset=utf-8",
	}, messageBody)
	if err != nil {
		return persist(build("failed", err.Error(), nil))
	}
	if status < 200 || status >= 300 {
		return persist(build("failed", fmt.Sprintf("feishu im responded with status %d", status), nil))
	}
	if code, msg := feishuResponseCode(respBody); code != 0 {
		return persist(build("failed", fmt.Sprintf("feishu im error code %d: %s", code, msg), nil))
	}

	return persist(build("success", "", stringPointer(nowISO)))
}

// feishuTenantToken returns a cached tenant_access_token when still valid, else
// fetches a fresh one from Feishu. Access is serialized so concurrent
// notifications share one token.
func (g *Gateway) feishuTenantToken(ctx context.Context, appID, appSecret string) (string, error) {
	g.feishuTokenMu.Lock()
	defer g.feishuTokenMu.Unlock()

	if g.feishuToken != "" && g.now().UTC().Before(g.feishuTokenExp) {
		return g.feishuToken, nil
	}

	body, err := json.Marshal(map[string]string{"app_id": appID, "app_secret": appSecret})
	if err != nil {
		return "", err
	}
	status, respBody, err := g.feishuAppHTTP(ctx, http.MethodPost, feishuAPIBase+"/open-apis/auth/v3/tenant_access_token/internal", map[string]string{
		"Content-Type": "application/json; charset=utf-8",
	}, body)
	if err != nil {
		return "", err
	}
	if status < 200 || status >= 300 {
		return "", fmt.Errorf("feishu token responded with status %d", status)
	}

	var parsed struct {
		Code              int    `json:"code"`
		Msg               string `json:"msg"`
		TenantAccessToken string `json:"tenant_access_token"`
		Expire            int    `json:"expire"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("decode feishu token response: %w", err)
	}
	if parsed.Code != 0 || strings.TrimSpace(parsed.TenantAccessToken) == "" {
		return "", fmt.Errorf("feishu token error code %d: %s", parsed.Code, parsed.Msg)
	}

	ttl := time.Duration(parsed.Expire) * time.Second
	if ttl <= 0 {
		ttl = 2 * time.Hour
	}
	g.feishuToken = parsed.TenantAccessToken
	g.feishuTokenExp = g.now().UTC().Add(ttl - feishuTokenSafetyMargin)
	return g.feishuToken, nil
}

func feishuResponseCode(body []byte) (int, string) {
	var parsed struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return 0, ""
	}
	return parsed.Code, parsed.Msg
}

// buildFeishuCard renders a notification payload into a simple Feishu
// interactive card (header + markdown body + note).
func buildFeishuCard(payload SystemNotificationPayload) ([]byte, error) {
	template := "blue"
	switch payload.Level {
	case "action_required":
		template = "orange"
	case "failure":
		template = "red"
	}

	title := strings.TrimSpace(payload.Title)
	if title == "" {
		title = "Looper"
	}

	body := strings.TrimSpace(payload.Body)
	if strings.TrimSpace(payload.Subtitle) != "" {
		if body != "" {
			body = payload.Subtitle + "\n\n" + body
		} else {
			body = payload.Subtitle
		}
	}
	if body == "" {
		body = title
	}

	noteParts := make([]string, 0, 3)
	if strings.TrimSpace(payload.Level) != "" {
		noteParts = append(noteParts, "Looper · "+payload.Level)
	} else {
		noteParts = append(noteParts, "Looper")
	}
	if strings.TrimSpace(payload.ProjectID) != "" {
		noteParts = append(noteParts, payload.ProjectID)
	}
	if strings.TrimSpace(payload.LoopID) != "" {
		noteParts = append(noteParts, payload.LoopID)
	}

	card := map[string]any{
		"config": map[string]any{"wide_screen_mode": true},
		"header": map[string]any{
			"template": template,
			"title":    map[string]any{"tag": "plain_text", "content": title},
		},
		"elements": []any{
			map[string]any{"tag": "div", "text": map[string]any{"tag": "lark_md", "content": body}},
			map[string]any{"tag": "note", "elements": []any{
				map[string]any{"tag": "lark_md", "content": strings.Join(noteParts, " · ")},
			}},
		},
	}
	return json.Marshal(card)
}

func defaultFeishuAppHTTP(ctx context.Context, method, url string, headers map[string]string, body []byte) (int, []byte, error) {
	client := &http.Client{Timeout: webhookTimeout}

	request, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}

	response, err := client.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()

	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return response.StatusCode, nil, err
	}
	return response.StatusCode, responseBody, nil
}

func webhookLevelAllowed(levels []config.NotificationSoundLevel, level string) bool {
	allowed := levels
	if len(allowed) == 0 {
		allowed = []config.NotificationSoundLevel{
			config.NotificationSoundLevelActionRequired,
			config.NotificationSoundLevelFailure,
		}
	}

	for _, candidate := range allowed {
		if string(candidate) == level {
			return true
		}
	}

	return false
}

type webhookGenericBody struct {
	Level      string `json:"level"`
	Title      string `json:"title"`
	Subtitle   string `json:"subtitle,omitempty"`
	Body       string `json:"body"`
	ProjectID  string `json:"projectId,omitempty"`
	LoopID     string `json:"loopId,omitempty"`
	RunID      string `json:"runId,omitempty"`
	EntityType string `json:"entityType,omitempty"`
	EntityID   string `json:"entityId,omitempty"`
	DedupeKey  string `json:"dedupeKey,omitempty"`
}

func buildWebhookBody(format string, payload SystemNotificationPayload) ([]byte, error) {
	if format == "feishu" {
		text := payload.Title
		if strings.TrimSpace(payload.Subtitle) != "" {
			text += "\n" + payload.Subtitle
		}
		if strings.TrimSpace(payload.Body) != "" {
			text += "\n" + payload.Body
		}

		return json.Marshal(map[string]any{
			"msg_type": "text",
			"content": map[string]any{
				"text": text,
			},
		})
	}

	return json.Marshal(webhookGenericBody{
		Level:      payload.Level,
		Title:      payload.Title,
		Subtitle:   payload.Subtitle,
		Body:       payload.Body,
		ProjectID:  payload.ProjectID,
		LoopID:     payload.LoopID,
		RunID:      payload.RunID,
		EntityType: payload.EntityType,
		EntityID:   payload.EntityID,
		DedupeKey:  payload.DedupeKey,
	})
}

func defaultWebhookPost(url string, body []byte) (int, error) {
	client := &http.Client{Timeout: webhookTimeout}

	request, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := client.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()

	_, _ = io.Copy(io.Discard, response.Body)

	return response.StatusCode, nil
}

func (g *Gateway) persistNotification(ctx context.Context, record storage.NotificationRecord) error {
	if g.repositories == nil || g.repositories.Notifications == nil || g.repositories.Events == nil {
		return fmt.Errorf("notification repositories are not configured")
	}

	if err := g.repositories.Notifications.Upsert(ctx, record); err != nil {
		return err
	}

	return eventlog.Append(ctx, g.repositories, eventlog.AppendInput{
		ID:         eventlog.NewEventID("event"),
		EventType:  "notification.sent",
		ProjectID:  record.ProjectID,
		LoopID:     record.LoopID,
		RunID:      record.RunID,
		EntityType: firstPointer(record.EntityType, stringPointer("notification")),
		EntityID:   firstPointer(record.EntityID, &record.ID),
		Payload: map[string]any{
			"channel":   record.Channel,
			"level":     record.Level,
			"status":    record.Status,
			"dedupeKey": record.DedupeKey,
			"title":     record.Title,
		},
		CreatedAt: mustParseJSISOString(record.CreatedAt),
	})
}

func buildAppleScript(payload SystemNotificationPayload, cfg config.NotificationConfig, logFilePath string) string {
	body := escapeAppleScriptString(payload.Body)
	title := escapeAppleScriptString(payload.Title)

	if payload.Level == "failure" && strings.TrimSpace(logFilePath) != "" {
		openLogPath := escapeAppleScriptString(logFilePath)
		return fmt.Sprintf(`set dialogResult to display dialog %q with title %q buttons {"Open Log", "Dismiss"} default button "Dismiss" cancel button "Dismiss" giving up after 30
if gave up of dialogResult is false and button returned of dialogResult is "Open Log" then
  do shell script "open " & quoted form of %q
end if`, body, title, openLogPath)
	}

	subtitle := ""
	if payload.Subtitle != "" {
		subtitle = fmt.Sprintf(` subtitle %q`, escapeAppleScriptString(payload.Subtitle))
	}

	sound := ""
	if payload.Sound != "" && isSoundEnabledForLevel(cfg, payload.Level) {
		sound = fmt.Sprintf(` sound name %q`, escapeAppleScriptString(payload.Sound))
	}

	return fmt.Sprintf(`display notification %q with title %q%s%s`, body, title, subtitle, sound)
}

func escapeAppleScriptString(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	value = strings.ReplaceAll(value, "\n", " ")
	return value
}

func isSoundEnabledForLevel(cfg config.NotificationConfig, level string) bool {
	for _, candidate := range cfg.Osascript.SoundForLevels {
		if string(candidate) == level {
			return true
		}
	}

	return false
}

func mustMarshalPayload(payload SystemNotificationPayload) string {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "{}"
	}

	return string(encoded)
}

func mustParseJSISOString(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Now().UTC()
	}

	return parsed
}

func nilIfEmpty(value string) *string {
	if strings.TrimSpace(value) == "" {
		return nil
	}

	return &value
}

func stringPointer(value string) *string {
	return &value
}

func firstPointer(values ...*string) *string {
	for _, value := range values {
		if value != nil {
			return value
		}
	}

	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}

	return ""
}

func ternaryString(condition bool, whenTrue, whenFalse string) string {
	if condition {
		return whenTrue
	}

	return whenFalse
}

func ternaryPointer(condition bool, value string) *string {
	if !condition {
		return nil
	}

	return &value
}

func ternaryTimePointer(condition bool, value string) *string {
	if !condition {
		return nil
	}

	return &value
}
