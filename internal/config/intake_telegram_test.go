package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func intakeIssuePaths(issues []ValidationIssue) string {
	paths := make([]string, 0, len(issues))
	for _, issue := range issues {
		paths = append(paths, issue.Path)
	}
	return strings.Join(paths, ", ")
}

func hasIntakeIssuePath(issues []ValidationIssue, path string) bool {
	for _, issue := range issues {
		if issue.Path == path {
			return true
		}
	}
	return false
}

func intakeConfigWithProject(projectID string, telegram *TelegramIntakeConfig) Config {
	return Config{
		Intake:   IntakeConfig{Telegram: telegram},
		Projects: []ProjectRefConfig{{ID: projectID}},
	}
}

// The decoder skips unregistered top-level sections silently, so a configured
// [intake.telegram] section reaching Config is the property worth pinning: unit
// tests that call mergeConfig directly cannot see this failure at all.
func TestLoadFileDecodesTheIntakeSection(t *testing.T) {
	cwd := t.TempDir()
	configPath := filepath.Join(cwd, "looper.json")
	body, err := json.Marshal(map[string]any{
		"projects": []map[string]any{{"id": "looper", "name": "Looper", "repoPath": cwd}},
		"intake": map[string]any{
			"telegram": map[string]any{
				"enabled":          true,
				"botTokenEnv":      "TELEGRAM_BOT_TOKEN",
				"allowedUserIds":   []int64{123456789},
				"defaultProjectId": "looper",
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(configPath, body, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	loaded, err := LoadFile(LoadFileOptions{
		CWD:        cwd,
		ConfigPath: configPath,
		LookupEnv:  emptyEnvLookup,
		LookPath:   fakeLookPath(map[string]string{"git": "/detected/git", "gh": "/detected/gh", "osascript": "/detected/osascript"}),
	})
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}

	tg := loaded.Config.Intake.Telegram
	if tg == nil {
		t.Fatal("LoadFile().Config.Intake.Telegram = nil — the intake section never reached Config")
	}
	if !tg.Enabled || tg.BotTokenEnv != "TELEGRAM_BOT_TOKEN" || tg.DefaultProjectID != "looper" {
		t.Fatalf("decoded intake = %+v", tg)
	}
	if len(tg.AllowedUserIDs) != 1 || tg.AllowedUserIDs[0] != 123456789 {
		t.Fatalf("decoded allowlist = %v", tg.AllowedUserIDs)
	}
}

func TestValidateIntakeTelegramDisabledNeedsNothing(t *testing.T) {
	t.Parallel()

	var issues []ValidationIssue
	validateIntakeConfig(intakeConfigWithProject("looper", &TelegramIntakeConfig{Enabled: false}), &issues)

	if len(issues) != 0 {
		t.Fatalf("disabled intake produced issues: %s", intakeIssuePaths(issues))
	}
}

// An intake bot with no allowlist lets anyone who finds the bot queue agent runs
// against the configured repositories, so an empty list must fail startup rather
// than default to open.
func TestValidateIntakeTelegramRequiresAllowlist(t *testing.T) {
	t.Parallel()

	var issues []ValidationIssue
	validateIntakeConfig(intakeConfigWithProject("looper", &TelegramIntakeConfig{
		Enabled: true, BotTokenEnv: "TELEGRAM_BOT_TOKEN", DefaultProjectID: "looper",
	}), &issues)

	if !hasIntakeIssuePath(issues, "intake.telegram.allowedUserIds") {
		t.Fatalf("missing allowlist issue; got: %s", intakeIssuePaths(issues))
	}
}

// A negative id is a group/channel chat id, which people commonly collect during
// setup. Accepting it would silently reject every real sender.
func TestValidateIntakeTelegramRejectsNonPositiveUserIDs(t *testing.T) {
	t.Parallel()

	var issues []ValidationIssue
	validateIntakeConfig(intakeConfigWithProject("looper", &TelegramIntakeConfig{
		Enabled: true, BotTokenEnv: "TELEGRAM_BOT_TOKEN", DefaultProjectID: "looper",
		AllowedUserIDs: []int64{-1001234567890},
	}), &issues)

	if !hasIntakeIssuePath(issues, "intake.telegram.allowedUserIds[0]") {
		t.Fatalf("a chat id passed as a user id; got: %s", intakeIssuePaths(issues))
	}
}

func TestValidateIntakeTelegramRejectsTokenValueInsteadOfEnvName(t *testing.T) {
	t.Parallel()

	var issues []ValidationIssue
	validateIntakeConfig(intakeConfigWithProject("looper", &TelegramIntakeConfig{
		Enabled: true, BotTokenEnv: "123456:AAH-actual-token-value",
		AllowedUserIDs: []int64{7}, DefaultProjectID: "looper",
	}), &issues)

	if !hasIntakeIssuePath(issues, "intake.telegram.botTokenEnv") {
		t.Fatalf("a pasted token passed as an env name; got: %s", intakeIssuePaths(issues))
	}
}

// A typo here would otherwise pass startup and reject every unprefixed message at
// runtime, after the sender has already typed the request.
func TestValidateIntakeTelegramRequiresARegisteredDefaultProject(t *testing.T) {
	t.Parallel()

	var issues []ValidationIssue
	validateIntakeConfig(intakeConfigWithProject("looper", &TelegramIntakeConfig{
		Enabled: true, BotTokenEnv: "TELEGRAM_BOT_TOKEN",
		AllowedUserIDs: []int64{7}, DefaultProjectID: "loper",
	}), &issues)

	if !hasIntakeIssuePath(issues, "intake.telegram.defaultProjectId") {
		t.Fatalf("an unregistered default project passed validation; got: %s", intakeIssuePaths(issues))
	}
}

func TestValidateIntakeTelegramAcceptsCompleteConfig(t *testing.T) {
	t.Parallel()

	var issues []ValidationIssue
	validateIntakeConfig(intakeConfigWithProject("looper", &TelegramIntakeConfig{
		Enabled: true, BotTokenEnv: "TELEGRAM_BOT_TOKEN",
		AllowedUserIDs: []int64{123456789}, DefaultProjectID: "looper",
	}), &issues)

	if len(issues) != 0 {
		t.Fatalf("complete config produced issues: %s", intakeIssuePaths(issues))
	}
}

func TestMergePartialIntakeTelegramDoesNotAliasTheAllowlist(t *testing.T) {
	t.Parallel()

	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	enabled := true
	tokenEnv := "TELEGRAM_BOT_TOKEN"
	ids := []int64{7, 9}
	project := "looper"
	mergeConfig(&cfg, PartialConfig{Intake: &PartialIntakeConfig{Telegram: &PartialTelegramIntakeConfig{
		Enabled: &enabled, BotTokenEnv: &tokenEnv, AllowedUserIDs: &ids, DefaultProjectID: &project,
	}}})

	tg := cfg.Intake.Telegram
	if tg == nil || !tg.Enabled || tg.BotTokenEnv != tokenEnv || tg.DefaultProjectID != project {
		t.Fatalf("merged intake = %+v", tg)
	}
	ids[0] = 99
	if tg.AllowedUserIDs[0] != 7 {
		t.Fatal("merged allowlist aliases the partial's slice")
	}
}
