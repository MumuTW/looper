package daemonservice

import (
	"strings"
	"testing"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/processsandbox"
)

func testInput(goos string, mutate func(*Input)) Input {
	input := Input{
		Config: config.DaemonConfig{
			Mode:                   config.DaemonModeLaunchd,
			RestartPolicy:          config.DaemonRestartOnFailure,
			RestartThrottleSeconds: 10,
			LogDir:                 "/home/dev/.looper/logs",
			WorkingDirectory:       "/home/dev",
			Environment:            map[string]string{},
		},
		ToolDetection: map[string]config.ToolDetectionStatus{
			"gitPath": config.ToolDetectionStatusConfigured,
			"ghPath":  config.ToolDetectionStatusConfigured,
		},
		ExecutablePath: "/home/dev/.local/bin/looperd",
		ConfigPath:     "/home/dev/.looper/config.toml",
		HomeDir:        "/home/dev",
		UID:            501,
		GOOS:           goos,
	}
	if mutate != nil {
		mutate(&input)
	}
	return input
}

func TestBuildLaunchdPlanUsesTheCanonicalPath(t *testing.T) {
	t.Parallel()

	plan, err := Build(testInput("darwin", nil))
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if plan.UnitPath != "/home/dev/Library/LaunchAgents/"+Label+".plist" {
		t.Fatalf("UnitPath = %q", plan.UnitPath)
	}
	for _, want := range []string{
		"<string>" + Label + "</string>",
		"<string>/home/dev/.local/bin/looperd</string>",
		"<string>--config</string>",
		"<string>/home/dev/.looper/config.toml</string>",
		"<key>RunAtLoad</key>",
	} {
		if !strings.Contains(plan.Unit, want) {
			t.Fatalf("plist missing %q:\n%s", want, plan.Unit)
		}
	}
}

// systemd does not read ~/.config when XDG_CONFIG_HOME points elsewhere, so a
// unit written there would never be found.
func TestBuildSystemdHonoursXDGConfigHome(t *testing.T) {
	t.Parallel()

	plan, err := Build(testInput("linux", func(input *Input) { input.XDGConfigHome = "/xdg/config" }))
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if plan.UnitPath != "/xdg/config/systemd/user/looperd.service" {
		t.Fatalf("UnitPath = %q, want the XDG location", plan.UnitPath)
	}

	fallback, err := Build(testInput("linux", nil))
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if fallback.UnitPath != "/home/dev/.config/systemd/user/looperd.service" {
		t.Fatalf("UnitPath = %q, want the default location", fallback.UnitPath)
	}
}

// The unit carries no user-configurable environment. The systemd unit has one
// fixed PATH so sandbox preflight and the manager run under the same search
// path; accepting daemon.environment would put user values in a second file
// and let the supervised daemon diverge from the foreground one.
func TestBuildRefusesDaemonEnvironment(t *testing.T) {
	t.Parallel()

	_, err := Build(testInput("darwin", func(input *Input) {
		input.Config.Environment = map[string]string{"LOOPER_LOG_DIR": "/elsewhere", "GH_TOKEN": "secret"}
	}))

	if err == nil {
		t.Fatal("Build() accepted daemon.environment")
	}
	if !strings.Contains(err.Error(), "GH_TOKEN") || !strings.Contains(err.Error(), "LOOPER_LOG_DIR") {
		t.Fatalf("error does not name the offending keys: %v", err)
	}
	for _, goos := range []string{"darwin", "linux"} {
		plan, err := Build(testInput(goos, nil))
		if err != nil {
			t.Fatalf("%s: Build() error = %v", goos, err)
		}
		if goos == "linux" {
			want := "Environment=PATH=" + processsandbox.ServiceProbePATHSystemd
			if !strings.Contains(plan.Unit, want) {
				t.Fatalf("linux: unit missing fixed service PATH %q:\n%s", want, plan.Unit)
			}
		} else if strings.Contains(plan.Unit, "Environment") {
			t.Fatalf("%s: unit carries an unexpected environment section:\n%s", goos, plan.Unit)
		}
	}
}

// One canonical location means activation, status, and uninstall always address
// the same thing, with no path to contain or resolve symlinks for.
func TestBuildRefusesACustomUnitPath(t *testing.T) {
	t.Parallel()
	custom := "/home/dev/Library/LaunchAgents/link/looperd.plist"

	if _, err := Build(testInput("darwin", func(input *Input) { input.Config.PlistPath = &custom })); err == nil {
		t.Fatal("Build() accepted daemon.plistPath")
	}
}

// A supervisor starts the daemon with a minimal PATH, so a tool merely detected
// in the installing shell would be searched for again and may resolve
// differently or not at all.
func TestBuildRequiresConfiguredToolPaths(t *testing.T) {
	t.Parallel()

	for _, tool := range []string{"gitPath", "ghPath"} {
		_, err := Build(testInput("darwin", func(input *Input) {
			input.ToolDetection[tool] = config.ToolDetectionStatusDetected
		}))
		if err == nil {
			t.Fatalf("Build() accepted a merely detected %s", tool)
		}
		if !strings.Contains(err.Error(), tool) {
			t.Fatalf("error does not name %s: %v", tool, err)
		}
	}
}

func TestBuildRequiresAbsolutePaths(t *testing.T) {
	t.Parallel()
	cases := map[string]func(*Input){
		"executable":        func(i *Input) { i.ExecutablePath = "./looperd" },
		"log dir":           func(i *Input) { i.Config.LogDir = "logs" },
		"working directory": func(i *Input) { i.Config.WorkingDirectory = "work" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := Build(testInput("darwin", mutate)); err == nil {
				t.Fatalf("Build() accepted a relative %s", name)
			}
		})
	}
}

func TestBuildRejectsUnsupportedPlatform(t *testing.T) {
	t.Parallel()

	if _, err := Build(testInput("windows", nil)); err == nil {
		t.Fatal("Build() accepted an unsupported platform")
	}
}

func TestBuildLaunchdRestartPolicies(t *testing.T) {
	t.Parallel()
	cases := []struct {
		policy config.DaemonRestartPolicy
		want   string
	}{
		{policy: config.DaemonRestartAlways, want: "<key>KeepAlive</key>\n\t<true/>"},
		// on-failure must not restart a clean exit: a deliberate stop stays stopped.
		{policy: config.DaemonRestartOnFailure, want: "<key>SuccessfulExit</key>"},
		{policy: config.DaemonRestartNever, want: "<key>KeepAlive</key>\n\t<false/>"},
	}
	for _, tc := range cases {
		t.Run(string(tc.policy), func(t *testing.T) {
			t.Parallel()
			plan, err := Build(testInput("darwin", func(i *Input) { i.Config.RestartPolicy = tc.policy }))
			if err != nil {
				t.Fatalf("Build() error = %v", err)
			}
			if !strings.Contains(plan.Unit, tc.want) {
				t.Fatalf("plist missing %q:\n%s", tc.want, plan.Unit)
			}
		})
	}
}

// systemd expands %-specifiers in unit values, so a literal percent in a path
// would otherwise be read as a specifier and change what runs.
func TestBuildEscapesPercentInSystemdValues(t *testing.T) {
	t.Parallel()

	plan, err := Build(testInput("linux", func(i *Input) {
		i.ExecutablePath = "/home/dev/bin/100%/looperd"
		i.Config.LogDir = "/home/dev/logs%h"
	}))
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if strings.Contains(plan.Unit, "100%/") || !strings.Contains(plan.Unit, "100%%/") {
		t.Fatalf("ExecStart does not escape a literal percent:\n%s", plan.Unit)
	}
	if !strings.Contains(plan.Unit, "logs%%h") {
		t.Fatalf("log path does not escape a literal percent:\n%s", plan.Unit)
	}
}

// systemd splits ExecStart on whitespace, so a path containing a space would
// otherwise become two arguments.
func TestBuildQuotesSystemdExecStart(t *testing.T) {
	t.Parallel()

	plan, err := Build(testInput("linux", func(i *Input) {
		i.ExecutablePath = "/home/dev/Application Support/looperd"
	}))
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if !strings.Contains(plan.Unit, `ExecStart="/home/dev/Application Support/looperd"`) {
		t.Fatalf("ExecStart is not quoted:\n%s", plan.Unit)
	}
}

// The resolved config path is embedded verbatim: trimming it would make the
// supervised daemon read a different file than the one just inspected.
func TestBuildPassesTheConfigPathVerbatim(t *testing.T) {
	t.Parallel()
	const padded = "/home/dev/.looper/ spaced .toml"

	plan, err := Build(testInput("darwin", func(i *Input) { i.ConfigPath = padded }))
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if !strings.Contains(plan.Unit, "<string>"+padded+"</string>") {
		t.Fatalf("config path was altered:\n%s", plan.Unit)
	}
}
