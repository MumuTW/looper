package config

import "testing"

func TestParseCLIArgsDispatchesInterleavedFlagFamilies(t *testing.T) {
	parsed, err := parseCLIArgs([]string{
		"--host", "127.0.0.2",
		"--reviewer-loop-enabled=true",
		"--port=9191",
		"--roles-reviewer-behavior-loop-enabled-by-default=false",
		"--allow-auto-push", "false",
	})
	if err != nil {
		t.Fatalf("parseCLIArgs() error = %v", err)
	}

	if parsed.overrides.Server == nil || parsed.overrides.Server.Host == nil || *parsed.overrides.Server.Host != "127.0.0.2" {
		t.Fatalf("server host override = %#v, want 127.0.0.2", parsed.overrides.Server)
	}
	if parsed.overrides.Server.Port == nil || *parsed.overrides.Server.Port != 9191 {
		t.Fatalf("server port override = %#v, want 9191", parsed.overrides.Server.Port)
	}
	if parsed.overrides.Defaults == nil || parsed.overrides.Defaults.AllowAutoPush == nil || *parsed.overrides.Defaults.AllowAutoPush {
		t.Fatalf("allow auto push override = %#v, want false", parsed.overrides.Defaults)
	}
	loop := parsed.overrides.Roles.Reviewer.Behavior.Loop
	if loop == nil || loop.EnabledByDefault == nil || *loop.EnabledByDefault {
		t.Fatalf("reviewer loop enabled override = %#v, want canonical false", loop)
	}
}
