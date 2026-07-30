// Package daemonservice installs looperd as a supervised service — a launchd
// agent on macOS, a systemd user unit on Linux.
//
// # Authority
//
// The configuration file decides everything about how the daemon runs. The unit
// adds nothing: it names the executable, points at that config file, and sets
// restart behaviour. It injects no environment and invents no paths.
//
// That is a deliberate narrowing. An earlier design let the unit carry
// daemon.environment and a custom unit path, which meant the supervised daemon
// could behave differently from the foreground one, secrets lived in a second
// file, and the unit location had to be contained, symlink-checked, and guessed
// at uninstall time. Refusing those inputs removes the defects rather than
// hardening them.
//
// # Contract
//
// Install:
//
//	I1  Afterwards a unit exists at the canonical path, or install failed and
//	    said which step failed.
//	I2  Install never modifies or deletes an existing unit. Replacing one is
//	    uninstall then install, so no active service is silently redefined.
//	I3  Install writes nothing the configuration file did not already determine.
//
// Uninstall:
//
//	U1  Deactivation is always attempted, whether or not a unit file is present:
//	    a missing file does not prove the supervisor has forgotten the service.
//	U2  The unit file is removed only after deactivation succeeds, so a loaded
//	    service never loses its definition.
//	U3  Uninstalling something already gone succeeds.
//
// There is no rollback. If activation fails the unit is left in place and the
// failure is reported; the operator uninstalls. A rollback that can itself fail
// is how the previous design could delete the only unit while the supervisor
// still had the service loaded.
package daemonservice

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/MumuTW/looper/internal/config"
)

// Label identifies the service to the platform supervisor. It is also the
// launchd plist basename and the systemd unit name, so it must stay stable:
// changing it would orphan an installed service under the old name.
const Label = "io.looper.looperd"

const systemdUnitName = "looperd.service"

// Manager is the platform supervisor a plan targets.
type Manager string

const (
	ManagerLaunchd Manager = "launchd"
	ManagerSystemd Manager = "systemd"
)

// Plan is everything installing would do. Every field is data; nothing has run.
type Plan struct {
	Manager  Manager
	UnitPath string
	Unit     string
	LogDir   string
	// Activate loads the service. Deactivate unloads it. Both are ordered.
	Activate   [][]string
	Deactivate [][]string
}

// Input is what planning needs beyond the daemon configuration.
type Input struct {
	Config config.DaemonConfig
	// ToolDetection reports, per tool, whether its path was configured or merely
	// detected in the installing shell. A supervisor starts the daemon with a
	// minimal PATH, so a detected path is not a path the service can rely on.
	ToolDetection map[string]config.ToolDetectionStatus
	// ExecutablePath is the looperd binary the supervisor runs.
	ExecutablePath string
	// ConfigPath is passed as --config, verbatim.
	ConfigPath string
	HomeDir    string
	// XDGConfigHome, when set, relocates the systemd user unit directory.
	// systemd does not read ~/.config when XDG_CONFIG_HOME points elsewhere.
	XDGConfigHome string
	UID           int
	GOOS          string
}

// ForGOOS reports the manager a platform uses.
func ForGOOS(goos string) (Manager, bool) {
	switch goos {
	case "darwin":
		return ManagerLaunchd, true
	case "linux":
		return ManagerSystemd, true
	default:
		return "", false
	}
}

// Build produces the plan, refusing any configuration the unit cannot honour.
func Build(input Input) (Plan, error) {
	manager, ok := ForGOOS(input.GOOS)
	if !ok {
		return Plan{}, fmt.Errorf("looper has no supervised-service support on %s; run looperd in the foreground under your own supervisor", input.GOOS)
	}
	if err := checkInstallable(input); err != nil {
		return Plan{}, err
	}
	if manager == ManagerLaunchd {
		return buildLaunchd(input), nil
	}
	return buildSystemd(input), nil
}

// checkInstallable rejects configurations whose supervised behaviour would differ
// from the foreground one. Each refusal replaces a class of defect that arose
// from accepting the input and trying to compensate.
func checkInstallable(input Input) error {
	if !filepath.IsAbs(strings.TrimSpace(input.ExecutablePath)) {
		// A supervisor has no shell and no PATH; a relative path resolves against
		// whatever directory it happens to use.
		return fmt.Errorf("looperd path must be absolute, got %q", input.ExecutablePath)
	}
	if strings.TrimSpace(input.HomeDir) == "" {
		return fmt.Errorf("home directory is required")
	}
	if !filepath.IsAbs(input.Config.LogDir) {
		return fmt.Errorf("daemon.logDir must be an absolute path, got %q", input.Config.LogDir)
	}
	if !filepath.IsAbs(input.Config.WorkingDirectory) {
		return fmt.Errorf("daemon.workingDirectory must be an absolute path, got %q", input.Config.WorkingDirectory)
	}
	if len(input.Config.Environment) > 0 {
		// The unit carries no environment, so honouring this would require putting
		// the values in a second file and keeping the two in step. Anything the
		// daemon needs belongs in its configuration.
		return fmt.Errorf("daemon.environment cannot be installed into a service unit: move these values into the configuration file, or manage them with your own supervisor drop-in (%s)", strings.Join(sortedKeys(input.Config.Environment), ", "))
	}
	if input.Config.PlistPath != nil && strings.TrimSpace(*input.Config.PlistPath) != "" {
		// One canonical location means activation, status, and uninstall always
		// address the same thing, with nothing to contain or resolve symlinks for.
		return fmt.Errorf("daemon.plistPath is not supported for installed services; the unit is always written to the canonical per-user location")
	}
	// git and gh are the tools every Role shells out to. A path merely detected in
	// the installing shell would be searched for again under the supervisor's
	// minimal PATH, where it may resolve differently or not at all.
	for _, tool := range []string{"gitPath", "ghPath"} {
		if input.ToolDetection[tool] == config.ToolDetectionStatusConfigured {
			continue
		}
		return fmt.Errorf("tools.%s must be set explicitly in the configuration file before installing a service: it is currently %q, and a supervised daemon does not inherit your shell's PATH",
			tool, string(input.ToolDetection[tool]))
	}
	return nil
}

// TeardownInput is what removing or inspecting a service needs. It deliberately
// excludes configuration: uninstall must work when the configuration is broken —
// which is often exactly why someone is uninstalling — and status reports what is
// on the machine, not what the current invocation would have installed.
type TeardownInput struct {
	HomeDir       string
	XDGConfigHome string
	UID           int
	GOOS          string
}

// BuildTeardown produces the plan for removing or inspecting the canonical
// service. Because the location is canonical there is nothing to guess.
func BuildTeardown(input TeardownInput) (Plan, error) {
	manager, ok := ForGOOS(input.GOOS)
	if !ok {
		return Plan{}, fmt.Errorf("looper has no supervised-service support on %s", input.GOOS)
	}
	if strings.TrimSpace(input.HomeDir) == "" {
		return Plan{}, fmt.Errorf("home directory is required")
	}
	full := Input{HomeDir: input.HomeDir, XDGConfigHome: input.XDGConfigHome, UID: input.UID, GOOS: input.GOOS}
	if manager == ManagerLaunchd {
		plan := buildLaunchd(full)
		plan.Unit = ""
		return plan, nil
	}
	plan := buildSystemd(full)
	plan.Unit = ""
	return plan, nil
}

func buildLaunchd(input Input) Plan {
	unitPath := filepath.Join(input.HomeDir, "Library", "LaunchAgents", Label+".plist")
	domain := fmt.Sprintf("gui/%d", input.UID)
	target := domain + "/" + Label
	return Plan{
		Manager:  ManagerLaunchd,
		UnitPath: unitPath,
		Unit:     renderPlist(input),
		LogDir:   input.Config.LogDir,
		Activate: [][]string{
			{"launchctl", "bootstrap", domain, unitPath},
			{"launchctl", "kickstart", "-k", target},
		},
		// bootout is the whole teardown: it both stops and forgets the service.
		Deactivate: [][]string{{"launchctl", "bootout", target}},
	}
}

func buildSystemd(input Input) Plan {
	configHome := strings.TrimSpace(input.XDGConfigHome)
	if configHome == "" {
		configHome = filepath.Join(input.HomeDir, ".config")
	}
	unitPath := filepath.Join(configHome, "systemd", "user", systemdUnitName)
	return Plan{
		Manager:  ManagerSystemd,
		UnitPath: unitPath,
		Unit:     renderSystemdUnit(input),
		LogDir:   input.Config.LogDir,
		Activate: [][]string{
			{"systemctl", "--user", "daemon-reload"},
			{"systemctl", "--user", "enable", "--now", systemdUnitName},
		},
		Deactivate: [][]string{
			{"systemctl", "--user", "disable", "--now", systemdUnitName},
			// daemon-reload runs after the file is gone, which is why it is listed
			// here rather than folded into the disable step.
			{"systemctl", "--user", "daemon-reload"},
		},
	}
}

func renderPlist(input Input) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString(`<plist version="1.0">` + "\n<dict>\n")
	writePlistString(&b, "Label", Label)

	b.WriteString("\t<key>ProgramArguments</key>\n\t<array>\n")
	for _, arg := range programArguments(input) {
		b.WriteString("\t\t<string>" + escapeXML(arg) + "</string>\n")
	}
	b.WriteString("\t</array>\n")

	writePlistBool(&b, "RunAtLoad", true)
	switch input.Config.RestartPolicy {
	case config.DaemonRestartAlways:
		writePlistBool(&b, "KeepAlive", true)
	case config.DaemonRestartOnFailure:
		// A clean exit is a deliberate stop; only a crash should restart.
		b.WriteString("\t<key>KeepAlive</key>\n\t<dict>\n")
		writePlistBoolIndented(&b, "SuccessfulExit", false, "\t\t")
		b.WriteString("\t</dict>\n")
	default:
		writePlistBool(&b, "KeepAlive", false)
	}
	writePlistInt(&b, "ThrottleInterval", input.Config.RestartThrottleSeconds)
	writePlistString(&b, "WorkingDirectory", input.Config.WorkingDirectory)
	writePlistString(&b, "StandardOutPath", filepath.Join(input.Config.LogDir, "looperd.out.log"))
	writePlistString(&b, "StandardErrorPath", filepath.Join(input.Config.LogDir, "looperd.err.log"))
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

func renderSystemdUnit(input Input) string {
	var b strings.Builder
	b.WriteString("[Unit]\nDescription=Looper daemon\nAfter=network-online.target\n\n")
	b.WriteString("[Service]\nType=simple\n")
	b.WriteString("ExecStart=" + systemdExecStart(programArguments(input)) + "\n")
	b.WriteString("WorkingDirectory=" + systemdValue(input.Config.WorkingDirectory) + "\n")
	switch input.Config.RestartPolicy {
	case config.DaemonRestartAlways:
		b.WriteString("Restart=always\n")
	case config.DaemonRestartOnFailure:
		b.WriteString("Restart=on-failure\n")
	default:
		b.WriteString("Restart=no\n")
	}
	b.WriteString(fmt.Sprintf("RestartSec=%d\n", input.Config.RestartThrottleSeconds))
	b.WriteString("StandardOutput=append:" + systemdValue(filepath.Join(input.Config.LogDir, "looperd.out.log")) + "\n")
	b.WriteString("StandardError=append:" + systemdValue(filepath.Join(input.Config.LogDir, "looperd.err.log")) + "\n")
	b.WriteString("\n[Install]\nWantedBy=default.target\n")
	return b.String()
}

// programArguments is the exact command the supervisor runs. The config path is
// passed verbatim: it has already been resolved, and trimming it here would make
// the supervised daemon read a different file than the one just inspected.
func programArguments(input Input) []string {
	args := []string{input.ExecutablePath}
	if input.ConfigPath != "" {
		args = append(args, "--config", input.ConfigPath)
	}
	return args
}

// systemdExecStart quotes each argument. systemd splits on whitespace, so a path
// containing a space would otherwise become two arguments.
func systemdExecStart(args []string) string {
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		quoted = append(quoted, `"`+systemdEscape(arg)+`"`)
	}
	return strings.Join(quoted, " ")
}

// systemdValue escapes a bare directive value.
func systemdValue(value string) string {
	return systemdEscape(value)
}

// systemdEscape escapes the characters systemd interprets. Percent is doubled
// because systemd expands %-specifiers in unit values, so a literal percent in a
// path would otherwise be read as a specifier.
func systemdEscape(value string) string {
	return strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
		`%`, `%%`,
	).Replace(value)
}

func writePlistString(b *strings.Builder, key, value string) {
	b.WriteString("\t<key>" + escapeXML(key) + "</key>\n\t<string>" + escapeXML(value) + "</string>\n")
}

func writePlistInt(b *strings.Builder, key string, value int) {
	b.WriteString(fmt.Sprintf("\t<key>%s</key>\n\t<integer>%d</integer>\n", escapeXML(key), value))
}

func writePlistBool(b *strings.Builder, key string, value bool) {
	writePlistBoolIndented(b, key, value, "\t")
}

func writePlistBoolIndented(b *strings.Builder, key string, value bool, indent string) {
	marker := "false"
	if value {
		marker = "true"
	}
	b.WriteString(indent + "<key>" + escapeXML(key) + "</key>\n" + indent + "<" + marker + "/>\n")
}

func escapeXML(value string) string {
	return strings.NewReplacer(
		"&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;",
	).Replace(value)
}

func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
