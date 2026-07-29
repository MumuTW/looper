package runtime

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	coordinatorrole "github.com/nexu-io/looper/internal/coordinator"
	"github.com/nexu-io/looper/internal/projects"
	"github.com/nexu-io/looper/internal/storage"
)

const validationGateDisabledWarning = "worker/fixer validation gate disabled: defaults.validationCommands is empty; the validate step passes without running anything"

// runnerValidationCommands reads the runner's frozen validation command list.
// The field is unexported on purpose (runners own their own config snapshot),
// so this wiring assertion reads it reflectively rather than widening the API.
func runnerValidationCommands(t *testing.T, runner any) []string {
	t.Helper()

	if runner == nil {
		t.Fatal("runner = nil, want a constructed runner")
	}
	field := reflect.ValueOf(runner).Elem().FieldByName("validationCommands")
	if !field.IsValid() {
		t.Fatalf("%T has no validationCommands field", runner)
	}
	commands := make([]string, 0, field.Len())
	for index := 0; index < field.Len(); index++ {
		commands = append(commands, field.Index(index).String())
	}
	return commands
}

func runnerStringField(t *testing.T, runner any, name string) string {
	t.Helper()
	field := reflect.ValueOf(runner).Elem().FieldByName(name)
	if !field.IsValid() || field.Kind() != reflect.String {
		t.Fatalf("%T has no string field %q", runner, name)
	}
	return field.String()
}

func buildValidationCommandHandlers(t *testing.T, cfg config.Config, logger *capturingSchedulerLogger) defaultSchedulerHandlers {
	t.Helper()

	root := t.TempDir()
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), filepath.Join(root, "looper.sqlite"), storage.SQLiteCoordinatorOptions{})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}
	repositories := storage.NewRepositories(coordinator.DB())

	return buildDefaultSchedulerHandlersWithOptions(
		cfg,
		"",
		logger,
		coordinator,
		repositories,
		nil,
		nil,
		NewActiveExecutionRegistry(),
		nil,
		nil,
		time.Now,
		nil,
		false,
		nil,
		nil,
		newSchedulerNotificationGatewayFactory(),
		coordinatorrole.NewRuntimeState(),
	)
}

func TestBuildDefaultSchedulerHandlersThreadsValidationCommandsIntoWorkerAndFixer(t *testing.T) {
	t.Parallel()

	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	vendor := config.AgentVendorCodex
	cfg.Agent.Vendor = &vendor
	cfg.Agent.Params = map[string]any{"command": "/opt/open-codex/bin/codex"}
	// Blank/padded entries exercise the resolver on the way through.
	cfg.Defaults.ValidationCommands = []string{"  go vet ./...  ", "go test ./..."}

	logger := &capturingSchedulerLogger{}
	handlers := buildValidationCommandHandlers(t, cfg, logger)
	if handlers.input == nil {
		t.Fatal("handlers.input = nil")
	}
	input := handlers.input(Services{})

	want := []string{"go vet ./...", "go test ./..."}
	if got := runnerValidationCommands(t, input.Worker); !reflect.DeepEqual(got, want) {
		t.Fatalf("worker validationCommands = %#v, want %#v", got, want)
	}
	if got := runnerValidationCommands(t, input.Fixer); !reflect.DeepEqual(got, want) {
		t.Fatalf("fixer validationCommands = %#v, want %#v", got, want)
	}
	for name, runner := range map[string]any{"worker": input.Worker, "fixer": input.Fixer} {
		if got := runnerStringField(t, runner, "validationCodexCommand"); got != "/opt/open-codex/bin/codex" {
			t.Fatalf("%s validationCodexCommand = %q, want configured command", name, got)
		}
	}
	if schedulerLoggerContains(logger, validationGateDisabledWarning) {
		t.Fatal("scheduler warned about a disabled validation gate while commands are configured")
	}
}

func TestCatalogSchedulerWarnsOnceWhenValidationGateIsEmpty(t *testing.T) {
	t.Parallel()

	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	vendor := config.AgentVendorCodex
	cfg.Agent.Vendor = &vendor

	root := t.TempDir()
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), filepath.Join(root, "looper.sqlite"), storage.SQLiteCoordinatorOptions{})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}
	repositories := storage.NewRepositories(coordinator.DB())
	logger := &capturingSchedulerLogger{}
	handlers := buildCatalogSchedulerHandlers(projects.NewCatalog(cfg), nil, "", logger, coordinator, repositories, nil, nil, NewActiveExecutionRegistry(), nil, nil, time.Now, nil, nil, nil)
	if handlers.snapshot == nil {
		t.Fatal("handlers.snapshot = nil")
	}
	_ = handlers.snapshot()
	_ = handlers.snapshot()

	logger.mu.Lock()
	defer logger.mu.Unlock()
	count := 0
	for _, entry := range logger.entries {
		if entry.message == validationGateDisabledWarning {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("validation warning count = %d, want 1 across repeated snapshots", count)
	}
}
