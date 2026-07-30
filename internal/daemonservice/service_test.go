package daemonservice

import (
	"reflect"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

func testInput(goos string, mutate func(*config.DaemonConfig)) Input {
	daemon := config.DaemonConfig{
		Mode:                   config.DaemonModeLaunchd,
		RestartPolicy:          config.DaemonRestartOnFailure,
		RestartThrottleSeconds: 10,
		LogDir:                 "/home/dev/.looper/logs",
		WorkingDirectory:       "/home/dev",
		Environment:            map[string]string{},
	}
	if mutate != nil {
		mutate(&daemon)
	}
	return Input{
		Config:         daemon,
		ExecutablePath: "/home/dev/.local/bin/looperd",
		ConfigPath:     "/home/dev/.looper/config.toml",
		HomeDir:        "/home/dev",
		UID:            501,
		GOOS:           goos,
	}
}

func TestBuildLaunchdPlan(t *testing.T) {
	t.Parallel()

	plan, err := Build(testInput("darwin", nil))
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if plan.Manager != ManagerLaunchd {
		t.Fatalf("Manager = %q", plan.Manager)
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
		"<integer>10</integer>",
		"/home/dev/.looper/logs/looperd.err.log",
	} {
		if !strings.Contains(plan.Unit, want) {
			t.Fatalf("plist missing %q:\n%s", want, plan.Unit)
		}
	}
}

// The supervised process must read the same configuration the plan was built
// from, or the service silently runs a different daemon than the operator
// inspected.
func TestBuildPassesTheResolvedConfigPath(t *testing.T) {
	t.Parallel()

	for _, goos := range []string{"darwin", "linux"} {
		plan, err := Build(testInput(goos, nil))
		if err != nil {
			t.Fatalf("%s: Build() error = %v", goos, err)
		}
		if !strings.Contains(plan.Unit, "/home/dev/.looper/config.toml") {
			t.Fatalf("%s: unit does not pass --config:\n%s", goos, plan.Unit)
		}
	}
}

func TestBuildLaunchdRestartPolicies(t *testing.T) {
	t.Parallel()
	cases := []struct {
		policy config.DaemonRestartPolicy
		want   string
	}{
		{policy: config.DaemonRestartAlways, want: "<key>KeepAlive</key>\n\t<true/>"},
		// on-failure must not restart a clean exit: a deliberate stop should stay
		// stopped.
		{policy: config.DaemonRestartOnFailure, want: "<key>SuccessfulExit</key>"},
		{policy: config.DaemonRestartNever, want: "<key>KeepAlive</key>\n\t<false/>"},
	}
	for _, tc := range cases {
		t.Run(string(tc.policy), func(t *testing.T) {
			t.Parallel()
			plan, err := Build(testInput("darwin", func(d *config.DaemonConfig) { d.RestartPolicy = tc.policy }))
			if err != nil {
				t.Fatalf("Build() error = %v", err)
			}
			if !strings.Contains(plan.Unit, tc.want) {
				t.Fatalf("plist missing %q:\n%s", tc.want, plan.Unit)
			}
		})
	}
}

func TestBuildSystemdPlan(t *testing.T) {
	t.Parallel()

	plan, err := Build(testInput("linux", func(d *config.DaemonConfig) {
		d.Mode = config.DaemonModeSystemd
		d.RestartPolicy = config.DaemonRestartAlways
	}))
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if plan.UnitPath != "/home/dev/.config/systemd/user/looperd.service" {
		t.Fatalf("UnitPath = %q", plan.UnitPath)
	}
	for _, want := range []string{
		`ExecStart="/home/dev/.local/bin/looperd" "--config" "/home/dev/.looper/config.toml"`,
		"Restart=always",
		"RestartSec=10",
		"WantedBy=default.target",
	} {
		if !strings.Contains(plan.Unit, want) {
			t.Fatalf("unit missing %q:\n%s", want, plan.Unit)
		}
	}
}

// A supervisor starts the daemon with no shell and no PATH, so a relative path
// would resolve against whatever directory it happens to use.
func TestBuildRejectsRelativeExecutablePath(t *testing.T) {
	t.Parallel()

	input := testInput("darwin", nil)
	input.ExecutablePath = "./looperd"

	if _, err := Build(input); err == nil {
		t.Fatal("Build() accepted a relative executable path")
	}
}

func TestBuildRejectsRelativeServicePaths(t *testing.T) {
	t.Parallel()

	for _, mutate := range []func(*config.DaemonConfig){
		func(d *config.DaemonConfig) { d.LogDir = "logs" },
		func(d *config.DaemonConfig) { d.WorkingDirectory = "." },
	} {
		if _, err := Build(testInput("linux", mutate)); err == nil {
			t.Fatal("Build() accepted a relative supervised-service path")
		}
	}
}

func TestBuildRejectsRootServiceDomain(t *testing.T) {
	t.Parallel()

	input := testInput("darwin", nil)
	input.UID = 0
	if _, err := Build(input); err == nil {
		t.Fatal("Build() accepted a root-scoped user service")
	}
}

func TestBuildRejectsUnsupportedPlatform(t *testing.T) {
	t.Parallel()

	if _, err := Build(testInput("windows", nil)); err == nil {
		t.Fatal("Build() accepted an unsupported platform")
	}
}

// daemon.environment is how operators pass tokens to the daemon, so the unit
// that embeds them must not be readable by other users on the machine.
func TestBuildKeepsTheUnitPrivate(t *testing.T) {
	t.Parallel()

	for _, goos := range []string{"darwin", "linux"} {
		plan, err := Build(testInput(goos, func(d *config.DaemonConfig) {
			d.Environment = map[string]string{"GH_TOKEN": "secret-value"}
		}))
		if err != nil {
			t.Fatalf("%s: Build() error = %v", goos, err)
		}
		if plan.FileMode != 0o600 {
			t.Fatalf("%s: FileMode = %#o, want 0600 — the unit embeds daemon.environment", goos, plan.FileMode)
		}
		if !strings.Contains(plan.Unit, "secret-value") {
			t.Fatalf("%s: unit dropped daemon.environment:\n%s", goos, plan.Unit)
		}
	}
}

// Environment order must not depend on map iteration, or every install rewrites
// the unit and restarts the daemon for no reason.
func TestBuildRendersEnvironmentDeterministically(t *testing.T) {
	t.Parallel()

	env := map[string]string{"ZED": "3", "ALPHA": "1", "MID": "2"}
	first, err := Build(testInput("darwin", func(d *config.DaemonConfig) { d.Environment = env }))
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	for i := 0; i < 20; i++ {
		next, err := Build(testInput("darwin", func(d *config.DaemonConfig) { d.Environment = env }))
		if err != nil {
			t.Fatalf("Build() error = %v", err)
		}
		if next.Unit != first.Unit {
			t.Fatal("unit rendering is not deterministic across map iterations")
		}
	}
	if strings.Index(first.Unit, "ALPHA") > strings.Index(first.Unit, "ZED") {
		t.Fatalf("environment is not sorted:\n%s", first.Unit)
	}
}

// A value carrying XML metacharacters must not be able to close a tag and inject
// plist structure.
func TestBuildEscapesPlistValues(t *testing.T) {
	t.Parallel()

	plan, err := Build(testInput("darwin", func(d *config.DaemonConfig) {
		d.Environment = map[string]string{"TRICKY": `</string><key>RunAtLoad</key><false/><string>`}
	}))
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if strings.Contains(plan.Unit, "<key>RunAtLoad</key><false/>") {
		t.Fatalf("plist value escaped its element:\n%s", plan.Unit)
	}
	if !strings.Contains(plan.Unit, "&lt;/string&gt;") {
		t.Fatalf("plist value was not escaped:\n%s", plan.Unit)
	}
}

// systemd splits ExecStart on whitespace, so a path containing a space would
// otherwise become two arguments.
func TestBuildQuotesSystemdExecStartArguments(t *testing.T) {
	t.Parallel()

	input := testInput("linux", nil)
	input.ExecutablePath = "/home/dev/Application Support/looperd"

	plan, err := Build(input)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if !strings.Contains(plan.Unit, `ExecStart="/home/dev/Application Support/looperd"`) {
		t.Fatalf("ExecStart is not quoted:\n%s", plan.Unit)
	}
}

func TestBuildEscapesSystemdDirectiveValues(t *testing.T) {
	t.Parallel()

	plan, err := Build(testInput("linux", func(d *config.DaemonConfig) {
		d.Environment = map[string]string{"TOKEN": "safe\nExecStart=/bin/attacker"}
		d.WorkingDirectory = "/home/dev\n[Install]"
		d.LogDir = "/home/dev/logs\nExecStartPre=/bin/attacker"
	}))
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if strings.Contains(plan.Unit, "\nExecStart=/bin/attacker") || strings.Contains(plan.Unit, "\nExecStartPre=/bin/attacker") {
		t.Fatalf("systemd value injected a directive:\n%s", plan.Unit)
	}
	if !strings.Contains(plan.Unit, `Environment="TOKEN=safe\nExecStart=/bin/attacker"`) {
		t.Fatalf("environment value was not systemd-escaped:\n%s", plan.Unit)
	}
}

func TestBuildEscapesSystemdSpecifiers(t *testing.T) {
	t.Parallel()

	input := testInput("linux", nil)
	input.ExecutablePath = "/home/dev/looperd%n"
	input.ConfigPath = "/home/dev/config%i.toml"
	plan, err := Build(input)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if !strings.Contains(plan.Unit, `"/home/dev/looperd%%n"`) || !strings.Contains(plan.Unit, `"/home/dev/config%%i.toml"`) {
		t.Fatalf("systemd specifiers were not escaped:\n%s", plan.Unit)
	}
}

func TestBuildRejectsSystemdCustomPlistPath(t *testing.T) {
	t.Parallel()

	path := "/home/dev/custom/looperd.service"
	if _, err := Build(testInput("linux", func(d *config.DaemonConfig) { d.PlistPath = &path })); err == nil {
		t.Fatal("Build() accepted daemon.plistPath for a systemd unit")
	}
}

func TestBuildHonoursAnExplicitUnitPath(t *testing.T) {
	t.Parallel()

	custom := "/home/dev/Library/LaunchAgents/looperd-custom.plist"
	plan, err := Build(testInput("darwin", func(d *config.DaemonConfig) { d.PlistPath = &custom }))
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if plan.UnitPath != custom {
		t.Fatalf("UnitPath = %q, want the configured path", plan.UnitPath)
	}
	if !strings.Contains(strings.Join(plan.Activate[0], " "), custom) {
		t.Fatalf("bootstrap does not reference the configured path: %v", plan.Activate)
	}
}

func TestBuildRejectsLaunchdUnitPathOutsideLaunchAgents(t *testing.T) {
	t.Parallel()

	for _, path := range []string{"relative.plist", "/home/dev/.ssh/id_rsa"} {
		path := path
		t.Run(path, func(t *testing.T) {
			if _, err := Build(testInput("darwin", func(d *config.DaemonConfig) { d.PlistPath = &path })); err == nil {
				t.Fatalf("Build() accepted unsafe plist path %q", path)
			}
		})
	}
}

func TestBuildDefaultUninstallPlanDoesNotNeedDaemonConfig(t *testing.T) {
	t.Parallel()

	plan, err := BuildDefaultUninstallPlan(DefaultUninstallInput{HomeDir: "/home/dev", UID: 501, GOOS: "linux"})
	if err != nil {
		t.Fatalf("BuildDefaultUninstallPlan() error = %v", err)
	}
	if got, want := plan.UnitPath, "/home/dev/.config/systemd/user/looperd.service"; got != want {
		t.Fatalf("UnitPath = %q, want %q", got, want)
	}
	if got, want := plan.Deactivate, [][]string{{"systemctl", "--user", "disable", "--now", "looperd.service"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Deactivate = %v, want %v", got, want)
	}
}
