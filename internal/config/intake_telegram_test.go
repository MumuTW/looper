package config

import (
	"strings"
	"testing"
)

func hasIntakeIssuePath(issues []ValidationIssue, path string) bool {
	for _, issue := range issues {
		if issue.Path == path {
			return true
		}
	}
	return false
}

func intakeIssuePaths(issues []ValidationIssue) string {
	paths := make([]string, 0, len(issues))
	for _, issue := range issues {
		paths = append(paths, issue.Path)
	}
	return strings.Join(paths, ", ")
}

func TestValidateIntakeTelegramDisabledNeedsNothing(t *testing.T) {
	t.Parallel()

	var issues []ValidationIssue
	validateIntakeConfig(IntakeConfig{Telegram: &TelegramIntakeConfig{Enabled: false}}, &issues)

	if len(issues) != 0 {
		t.Fatalf("disabled intake produced issues: %s", intakeIssuePaths(issues))
	}
}

// An intake bot with no allowlist lets anyone who finds the bot queue agent runs
// against the user's repositories, so an empty list must fail validation rather
// than default to open.
func TestValidateIntakeTelegramRequiresAllowlist(t *testing.T) {
	t.Parallel()

	var issues []ValidationIssue
	validateIntakeConfig(IntakeConfig{Telegram: &TelegramIntakeConfig{
		Enabled:          true,
		BotTokenEnv:      "TELEGRAM_BOT_TOKEN",
		DefaultProjectID: "looper",
	}}, &issues)

	if !hasIntakeIssuePath(issues, "intake.telegram.allowedUserIds") {
		t.Fatalf("missing allowlist issue; got: %s", intakeIssuePaths(issues))
	}
}

func TestValidateIntakeTelegramRequiresTokenEnvAndDefaultProject(t *testing.T) {
	t.Parallel()

	var issues []ValidationIssue
	validateIntakeConfig(IntakeConfig{Telegram: &TelegramIntakeConfig{
		Enabled:        true,
		AllowedUserIDs: []int64{7},
	}}, &issues)

	for _, path := range []string{"intake.telegram.botTokenEnv", "intake.telegram.defaultProjectId"} {
		if !hasIntakeIssuePath(issues, path) {
			t.Fatalf("missing %s issue; got: %s", path, intakeIssuePaths(issues))
		}
	}
}

func TestValidateIntakeTelegramRejectsTokenValueInsteadOfEnvName(t *testing.T) {
	t.Parallel()

	var issues []ValidationIssue
	validateIntakeConfig(IntakeConfig{Telegram: &TelegramIntakeConfig{
		Enabled:          true,
		BotTokenEnv:      "123456:AAH-actual-token-value",
		AllowedUserIDs:   []int64{7},
		DefaultProjectID: "looper",
	}}, &issues)

	if !hasIntakeIssuePath(issues, "intake.telegram.botTokenEnv") {
		t.Fatalf("a pasted token passed as an env name; got: %s", intakeIssuePaths(issues))
	}
}

func TestValidateIntakeTelegramAcceptsCompleteConfig(t *testing.T) {
	t.Parallel()

	var issues []ValidationIssue
	validateIntakeConfig(IntakeConfig{Telegram: &TelegramIntakeConfig{
		Enabled:          true,
		BotTokenEnv:      "TELEGRAM_BOT_TOKEN",
		AllowedUserIDs:   []int64{7},
		DefaultProjectID: "looper",
		ChatID:           "-1001234567890",
	}}, &issues)

	if len(issues) != 0 {
		t.Fatalf("complete config produced issues: %s", intakeIssuePaths(issues))
	}
}

func TestValidateHITLTelegramTransportRequiresIntake(t *testing.T) {
	t.Parallel()

	var issues []ValidationIssue
	validateHITLConfig(HITLConfig{Enabled: true, AnswerTransport: "telegram"}, IntakeConfig{}, &issues)

	if !hasIntakeIssuePath(issues, "intake.telegram.enabled") {
		t.Fatalf("telegram transport accepted without intake; got: %s", intakeIssuePaths(issues))
	}
}

// Intake alone can reply in whichever chat a message arrived from, but an
// outbound ask has no incoming message to answer, so it needs an explicit chat.
func TestValidateHITLTelegramTransportRequiresChatID(t *testing.T) {
	t.Parallel()

	var issues []ValidationIssue
	validateHITLConfig(
		HITLConfig{Enabled: true, AnswerTransport: "telegram"},
		IntakeConfig{Telegram: &TelegramIntakeConfig{Enabled: true, BotTokenEnv: "TELEGRAM_BOT_TOKEN", AllowedUserIDs: []int64{7}, DefaultProjectID: "looper"}},
		&issues,
	)

	if !hasIntakeIssuePath(issues, "intake.telegram.chatId") {
		t.Fatalf("telegram transport accepted without a chat id; got: %s", intakeIssuePaths(issues))
	}
}

func TestMergePartialIntakeTelegram(t *testing.T) {
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
		Enabled:          &enabled,
		BotTokenEnv:      &tokenEnv,
		AllowedUserIDs:   &ids,
		DefaultProjectID: &project,
	}}})

	tg := cfg.Intake.Telegram
	if tg == nil || !tg.Enabled || tg.BotTokenEnv != tokenEnv || tg.DefaultProjectID != project {
		t.Fatalf("merged intake = %+v", tg)
	}
	if len(tg.AllowedUserIDs) != 2 || tg.AllowedUserIDs[0] != 7 {
		t.Fatalf("merged allowlist = %v", tg.AllowedUserIDs)
	}
	// The merged slice must not alias the caller's, or a later mutation of the
	// partial would rewrite live config.
	ids[0] = 99
	if tg.AllowedUserIDs[0] != 7 {
		t.Fatal("merged allowlist aliases the partial's slice")
	}
}
