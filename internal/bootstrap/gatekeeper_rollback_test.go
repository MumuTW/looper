package bootstrap

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/MumuTW/looper/internal/config"
)

func TestBootstrapStartsWithWithdrawnGatekeeperTrustConfiguration(t *testing.T) {
	cwd := t.TempDir()
	configPath := filepath.Join(cwd, "config.toml")
	contents := "[storage]\ndbPath = \"" + filepath.Join(cwd, "looper.sqlite") + "\"\n\n[daemon]\nlogDir = \"" + filepath.Join(cwd, "logs") + "\"\nworkingDirectory = \"" + cwd + "\"\n\n[roles.gatekeeper]\ntrust = \"advise\"\n\n[[projects]]\nid = \"demo\"\nname = \"Demo\"\nrepoPath = \"" + cwd + "\"\n\n[projects.roles.gatekeeper]\ntrust = \"advise\"\n"
	if err := os.WriteFile(configPath, []byte(contents), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	started := false
	_, err := Bootstrap(context.Background(), Options{
		CWD:                 cwd,
		Args:                []string{"--config", configPath},
		Env:                 map[string]string{},
		CreateLogger:        func(config.LoggingConfig, string, LoggerOptions) (Logger, error) { return &recordingLogger{}, nil },
		CheckSandboxRuntime: func() error { return nil },
		StartRuntime: func(_ context.Context, deps RuntimeDependencies) (Runtime, error) {
			started = true
			if deps.InitialConfig.Partial.Roles == nil || deps.InitialConfig.Partial.Roles.Gatekeeper == nil {
				t.Fatal("withdrawn roles.gatekeeper setting was not decoded")
			}
			return struct{}{}, nil
		},
	})
	if err != nil {
		t.Fatalf("Bootstrap() error = %v", err)
	}
	if !started {
		t.Fatal("Bootstrap() did not start the daemon runtime")
	}
}
