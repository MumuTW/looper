package notify

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

func TestGatewayPersistsInAppNotificationsAndDedupesOsascriptDelivery(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rootDir := t.TempDir()
	capturePath := filepath.Join(rootDir, "osascript.log")
	scriptPath := filepath.Join(rootDir, "osascript")
	writeExecutableScript(t, scriptPath, "#!/bin/sh\nprintf '%s\n' \"$*\" >> \""+capturePath+"\"\n")

	coordinator := openNotifyCoordinator(t, rootDir)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 11, 12, 0, 0, 0, time.UTC)

	gateway := NewGateway(Options{
		Config: config.NotificationConfig{
			InApp: true,
			Osascript: config.OsascriptNotificationConfig{
				Enabled:               true,
				SoundForLevels:        []config.NotificationSoundLevel{config.NotificationSoundLevelFailure, config.NotificationSoundLevelActionRequired},
				ThrottleWindowSeconds: 60,
			},
		},
		OsascriptPath: scriptPath,
		LogFilePath:   filepath.Join(rootDir, "logs", "looperd.log"),
		Repositories:  repos,
		Now:           func() time.Time { return now },
	})

	first := gateway.Notify(ctx, SystemNotificationPayload{
		Level:      "failure",
		Title:      "Worker blocked",
		Subtitle:   "task_1",
		Body:       "Needs attention",
		Sound:      "Funk",
		EntityType: "task",
		EntityID:   "task_1",
		DedupeKey:  "worker.blocked:task:task_1",
	})
	second := gateway.Notify(ctx, SystemNotificationPayload{
		Level:      "failure",
		Title:      "Worker blocked",
		Subtitle:   "task_1",
		Body:       "Needs attention",
		Sound:      "Funk",
		EntityType: "task",
		EntityID:   "task_1",
		DedupeKey:  "worker.blocked:task:task_1",
	})

	if got := notificationStatus(first, "osascript"); got != "success" {
		t.Fatalf("first osascript status = %q, want success", got)
	}
	if got := notificationStatus(second, "osascript"); got != "skipped" {
		t.Fatalf("second osascript status = %q, want skipped", got)
	}

	notifications, err := repos.Notifications.List(ctx, 10)
	if err != nil {
		t.Fatalf("Notifications.List() error = %v", err)
	}
	if len(notifications) != 6 {
		t.Fatalf("Notifications.List() len = %d, want 6", len(notifications))
	}

	events, err := repos.Events.ListByEntity(ctx, "task", "task_1")
	if err != nil {
		t.Fatalf("Events.ListByEntity() error = %v", err)
	}
	if len(events) != 6 {
		t.Fatalf("Events.ListByEntity() len = %d, want 6", len(events))
	}

	osascriptCallsBytes, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", capturePath, err)
	}
	osascriptCalls := string(osascriptCallsBytes)
	assertContains(t, osascriptCalls, "display dialog")
	assertContains(t, osascriptCalls, "Open Log")
	assertContains(t, osascriptCalls, "open ")
	assertContains(t, osascriptCalls, filepath.Join(rootDir, "logs", "looperd.log"))
}

func TestGatewayUsesLightweightOsascriptNotificationForNonFailureLevels(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rootDir := t.TempDir()
	capturePath := filepath.Join(rootDir, "osascript.log")
	scriptPath := filepath.Join(rootDir, "osascript")
	writeExecutableScript(t, scriptPath, "#!/bin/sh\nprintf '%s\n' \"$*\" >> \""+capturePath+"\"\n")

	coordinator := openNotifyCoordinator(t, rootDir)
	repos := storage.NewRepositories(coordinator.DB())

	gateway := NewGateway(Options{
		Config: config.NotificationConfig{
			InApp: true,
			Osascript: config.OsascriptNotificationConfig{
				Enabled:               true,
				SoundForLevels:        []config.NotificationSoundLevel{config.NotificationSoundLevelFailure, config.NotificationSoundLevelActionRequired},
				ThrottleWindowSeconds: 60,
			},
		},
		OsascriptPath: scriptPath,
		Repositories:  repos,
	})

	gateway.Notify(ctx, SystemNotificationPayload{
		Level:    "success",
		Title:    "Loop completed",
		Subtitle: "loop_1",
		Body:     "All good",
		Sound:    "Funk",
	})

	osascriptCallsBytes, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", capturePath, err)
	}
	osascriptCalls := string(osascriptCallsBytes)
	assertContains(t, osascriptCalls, "display notification")
	if strings.Contains(osascriptCalls, "display dialog") {
		t.Fatalf("osascript calls = %q, want no display dialog", osascriptCalls)
	}
}

func TestBrowserDashboardBaseURLAndLoopURL(t *testing.T) {
	t.Parallel()

	if got := BrowserDashboardBaseURL("0.0.0.0", 17310, nil); got != "http://127.0.0.1:17310" {
		t.Fatalf("wildcard host = %q", got)
	}
	if got := BrowserDashboardBaseURL("::1", 17310, nil); got != "http://[::1]:17310" {
		t.Fatalf("ipv6 host = %q", got)
	}
	base := "https://looper.example"
	if got := BrowserDashboardBaseURL("0.0.0.0", 1, &base); got != "https://looper.example" {
		t.Fatalf("baseURL override = %q", got)
	}
	if got := DashboardLoopURL("http://127.0.0.1:17310", 71); got != "http://127.0.0.1:17310/dashboard/loops/71" {
		t.Fatalf("loop URL = %q", got)
	}
	if got := DashboardLoopURL("", 71); got != "" {
		t.Fatalf("empty base = %q, want empty", got)
	}
}

func TestBuildHITLAskAppleScriptIsStickyAndOpensDashboard(t *testing.T) {
	t.Parallel()

	script := buildHITLAskAppleScript(HITLAskCard{
		Title:    "Fixer needs a decision",
		Question: "Keep RollingUpdate or follow reviewer?",
		Options:  []string{"keep", "follow reviewer"},
	}, "http://127.0.0.1:17310/dashboard/loops/71")

	assertContains(t, script, `display dialog`)
	assertContains(t, script, `Open Dashboard`)
	assertContains(t, script, `http://127.0.0.1:17310/dashboard/loops/71`)
	assertContains(t, script, `do shell script "open "`)
	if strings.Contains(script, "giving up after") {
		t.Fatalf("HITL dialog must not auto-dismiss, got %q", script)
	}
}

func TestSendHITLAskLaunchesStickyOsascriptWithoutFeishu(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rootDir := t.TempDir()
	capturePath := filepath.Join(rootDir, "osascript.log")
	scriptPath := filepath.Join(rootDir, "osascript")
	// Block briefly so the async launch is observable, then exit.
	writeExecutableScript(t, scriptPath, "#!/bin/sh\nprintf '%s\n' \"$*\" >> \""+capturePath+"\"\n")

	coordinator := openNotifyCoordinator(t, rootDir)
	repos := storage.NewRepositories(coordinator.DB())

	gateway := NewGateway(Options{
		Config: config.NotificationConfig{
			Osascript: config.OsascriptNotificationConfig{Enabled: true},
		},
		OsascriptPath:    scriptPath,
		DashboardBaseURL: "http://127.0.0.1:17310",
		Repositories:     repos,
	})

	if err := gateway.SendHITLAsk(ctx, HITLAskCard{
		ProjectID: "p1",
		LoopID:    "loop_1",
		LoopSeq:   71,
		Title:     "Fixer needs a decision",
		Question:  "Which path?",
		Options:   []string{"A", "B"},
	}); err != nil {
		t.Fatalf("SendHITLAsk() error = %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	var osascriptCalls string
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(capturePath); err == nil && len(b) > 0 {
			osascriptCalls = string(b)
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if osascriptCalls == "" {
		t.Fatal("osascript was not invoked")
	}
	assertContains(t, osascriptCalls, "display dialog")
	assertContains(t, osascriptCalls, "Open Dashboard")
	assertContains(t, osascriptCalls, "http://127.0.0.1:17310/dashboard/loops/71")
	if strings.Contains(osascriptCalls, "giving up after") {
		t.Fatalf("sticky HITL dialog must not auto-dismiss: %q", osascriptCalls)
	}
}

func openNotifyCoordinator(t *testing.T, rootDir string) *storage.SQLiteCoordinator {
	t.Helper()

	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), filepath.Join(rootDir, "state", "looper.sqlite"), storage.SQLiteCoordinatorOptions{
		Migrations: storage.EmbeddedMigrations,
		BackupDir:  filepath.Join(rootDir, "backups"),
	})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() {
		if closeErr := coordinator.Close(); closeErr != nil {
			t.Fatalf("coordinator.Close() error = %v", closeErr)
		}
	})

	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("MigrationRunner.RunPending() error = %v", err)
	}

	return coordinator
}

func writeExecutableScript(t *testing.T, path string, contents string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatalf("Chmod(%q) error = %v", path, err)
	}
}

func notificationStatus(records []storage.NotificationRecord, channel string) string {
	for _, record := range records {
		if record.Channel == channel {
			return record.Status
		}
	}

	return ""
}

func assertContains(t *testing.T, got string, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Fatalf("string %q does not contain %q", got, want)
	}
}
