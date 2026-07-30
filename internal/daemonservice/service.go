// Package daemonservice turns Looper's daemon configuration into a supervised
// service definition — a launchd agent on macOS, a systemd user unit on Linux.
//
// Planning is separated from installation on purpose: Plan is a pure function
// over configuration, so what would be written to disk and run as root-adjacent
// commands can be inspected and tested without touching either.
package daemonservice

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nexu-io/looper/internal/config"
)

// Label identifies the service to the platform's supervisor. It is also the
// launchd plist basename and the systemd unit name, so it must stay stable:
// changing it would orphan an already-installed service under the old name.
const Label = "io.looper.looperd"

// Manager is the platform supervisor a plan targets.
type Manager string

const (
	ManagerLaunchd Manager = "launchd"
	ManagerSystemd Manager = "systemd"
)

// Plan is everything installing the service would do. Every field is data:
// nothing here has run yet.
type Plan struct {
	Manager Manager
	// UnitPath is the file to write, and Unit is its exact contents.
	UnitPath string
	Unit     string
	// FileMode is 0600 because the unit embeds daemon.environment, which is how
	// operators pass tokens to the daemon. A world-readable unit would widen the
	// config file's own secrecy.
	FileMode uint32
	// LogDir must exist before the supervisor starts the daemon; launchd and
	// systemd both fail to redirect output into a missing directory.
	LogDir string
	// Activate and Deactivate are the commands that load and unload the service.
	Activate   [][]string
	Deactivate [][]string
}

// Input is what Plan needs beyond the daemon configuration itself.
type Input struct {
	Config config.DaemonConfig
	// ExecutablePath is the looperd binary the supervisor will run. It must be an
	// absolute path that survives the current shell: a supervisor has no PATH to
	// speak of.
	ExecutablePath string
	// ConfigPath is passed to the daemon as --config so the supervised process
	// reads the same configuration this plan was built from.
	ConfigPath string
	// HomeDir locates the per-user agent/unit directory.
	HomeDir string
	// UID is the launchd domain target (gui/<uid>). Ignored for systemd.
	UID int
	// GOOS selects the manager. Passed in rather than read from runtime so the
	// plan for either platform can be tested from either platform.
	GOOS string
}

// ForGOOS reports the manager a platform uses, or false when Looper has no
// supervised-service support there.
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

// Build produces the plan for the given configuration.
func Build(input Input) (Plan, error) {
	manager, ok := ForGOOS(input.GOOS)
	if !ok {
		return Plan{}, fmt.Errorf("looper has no supervised-service support on %s; run looperd in the foreground under your own supervisor", input.GOOS)
	}
	if strings.TrimSpace(input.ExecutablePath) == "" {
		return Plan{}, fmt.Errorf("executable path is required")
	}
	if !filepath.IsAbs(input.ExecutablePath) {
		// A supervisor starts the daemon with no shell and no PATH; a relative path
		// would resolve against whatever directory it happens to use.
		return Plan{}, fmt.Errorf("executable path must be absolute, got %q", input.ExecutablePath)
	}
	if strings.TrimSpace(input.HomeDir) == "" {
		return Plan{}, fmt.Errorf("home directory is required")
	}
	if input.UID <= 0 {
		return Plan{}, fmt.Errorf("supervised service installation is user-scoped; refusing uid %d", input.UID)
	}
	logDir := strings.TrimSpace(input.Config.LogDir)
	if logDir == "" {
		return Plan{}, fmt.Errorf("daemon.logDir is required to install a service")
	}

	switch manager {
	case ManagerLaunchd:
		return buildLaunchd(input, logDir)
	default:
		return buildSystemd(input, logDir)
	}
}

func buildLaunchd(input Input, logDir string) (Plan, error) {
	unitPath := strings.TrimSpace(derefString(input.Config.PlistPath))
	if unitPath == "" {
		unitPath = filepath.Join(input.HomeDir, "Library", "LaunchAgents", Label+".plist")
	} else {
		if !filepath.IsAbs(unitPath) {
			return Plan{}, fmt.Errorf("daemon.plistPath must be absolute, got %q", unitPath)
		}
		launchAgents := filepath.Join(input.HomeDir, "Library", "LaunchAgents")
		if !pathWithin(launchAgents, unitPath) {
			return Plan{}, fmt.Errorf("daemon.plistPath must stay under %s", launchAgents)
		}
	}
	domain := fmt.Sprintf("gui/%d", input.UID)
	target := domain + "/" + Label
	return Plan{
		Manager:  ManagerLaunchd,
		UnitPath: unitPath,
		Unit:     renderPlist(input, logDir),
		FileMode: 0o600,
		LogDir:   logDir,
		Activate: [][]string{
			// Do not boot out an existing label here. Install refuses replacement
			// of its requested path, and unloading a label from another path would
			// exceed this command's authority.
			{"launchctl", "bootstrap", domain, unitPath},
			{"launchctl", "kickstart", "-k", target},
		},
		Deactivate: [][]string{{"launchctl", "bootout", target}},
	}, nil
}

func buildSystemd(input Input, logDir string) (Plan, error) {
	if strings.TrimSpace(derefString(input.Config.PlistPath)) != "" {
		// plistPath predates systemd support. systemctl resolves looperd.service
		// from its user unit search path, so writing a custom path would make
		// status inspect one file while enable starts another (or none at all).
		return Plan{}, fmt.Errorf("daemon.plistPath is supported only for launchd; systemd installs use the managed user unit path")
	}
	unitPath := filepath.Join(input.HomeDir, ".config", "systemd", "user", "looperd.service")
	return Plan{
		Manager:  ManagerSystemd,
		UnitPath: unitPath,
		Unit:     renderSystemdUnit(input, logDir),
		FileMode: 0o600,
		LogDir:   logDir,
		Activate: [][]string{
			{"systemctl", "--user", "daemon-reload"},
			{"systemctl", "--user", "enable", "--now", "looperd.service"},
		},
		Deactivate: [][]string{
			{"systemctl", "--user", "disable", "--now", "looperd.service"},
			{"systemctl", "--user", "daemon-reload"},
		},
	}, nil
}

func renderPlist(input Input, logDir string) string {
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
		// A clean exit is a deliberate stop; only a crash should be restarted.
		b.WriteString("\t<key>KeepAlive</key>\n\t<dict>\n")
		writePlistBoolIndented(&b, "SuccessfulExit", false, "\t\t")
		b.WriteString("\t</dict>\n")
	default:
		writePlistBool(&b, "KeepAlive", false)
	}
	writePlistInt(&b, "ThrottleInterval", input.Config.RestartThrottleSeconds)
	writePlistString(&b, "WorkingDirectory", input.Config.WorkingDirectory)
	writePlistString(&b, "StandardOutPath", filepath.Join(logDir, "looperd.out.log"))
	writePlistString(&b, "StandardErrorPath", filepath.Join(logDir, "looperd.err.log"))

	if env := sortedEnvironment(input.Config.Environment); len(env) > 0 {
		b.WriteString("\t<key>EnvironmentVariables</key>\n\t<dict>\n")
		for _, key := range env {
			b.WriteString("\t\t<key>" + escapeXML(key) + "</key>\n")
			b.WriteString("\t\t<string>" + escapeXML(input.Config.Environment[key]) + "</string>\n")
		}
		b.WriteString("\t</dict>\n")
	}
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

func renderSystemdUnit(input Input, logDir string) string {
	var b strings.Builder
	b.WriteString("[Unit]\n")
	b.WriteString("Description=Looper daemon\n")
	b.WriteString("After=network-online.target\n\n")

	b.WriteString("[Service]\n")
	b.WriteString("Type=simple\n")
	b.WriteString("ExecStart=" + systemdJoin(programArguments(input)) + "\n")
	b.WriteString("WorkingDirectory=" + systemdQuote(input.Config.WorkingDirectory) + "\n")
	switch input.Config.RestartPolicy {
	case config.DaemonRestartAlways:
		b.WriteString("Restart=always\n")
	case config.DaemonRestartOnFailure:
		b.WriteString("Restart=on-failure\n")
	default:
		b.WriteString("Restart=no\n")
	}
	b.WriteString(fmt.Sprintf("RestartSec=%d\n", input.Config.RestartThrottleSeconds))
	b.WriteString("StandardOutput=" + systemdQuote("append:"+filepath.Join(logDir, "looperd.out.log")) + "\n")
	b.WriteString("StandardError=" + systemdQuote("append:"+filepath.Join(logDir, "looperd.err.log")) + "\n")
	for _, key := range sortedEnvironment(input.Config.Environment) {
		b.WriteString("Environment=" + systemdQuote(key+"="+input.Config.Environment[key]) + "\n")
	}
	b.WriteString("\n[Install]\nWantedBy=default.target\n")
	return b.String()
}

func programArguments(input Input) []string {
	args := []string{input.ExecutablePath}
	if configPath := strings.TrimSpace(input.ConfigPath); configPath != "" {
		args = append(args, "--config", configPath)
	}
	return args
}

func sortedEnvironment(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// systemdJoin quotes every systemd ExecStart argument. Unit files are parsed by
// systemd rather than a shell, so control characters and quotes must be escaped
// before a configuration value reaches the unit grammar.
func systemdJoin(args []string) string {
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		quoted = append(quoted, systemdQuote(arg))
	}
	return strings.Join(quoted, " ")
}

func systemdQuote(value string) string {
	escaped := strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
		"\n", `\n`,
		"\r", `\r`,
		"\t", `\t`,
	).Replace(value)
	return `"` + escaped + `"`
}

func pathWithin(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
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
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&apos;",
	).Replace(value)
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
