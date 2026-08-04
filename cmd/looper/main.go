// Command looper is the operator CLI for a running looperd.
//
// This is the minimal surface deliberately kept after the full cobra CLI was
// removed: the loop control verbs, which are the only ones the daemon has no
// other client for, plus the few onboarding verbs a first-time operator needs
// before the dashboard is reachable (init, status, project add/list/discover).
// Everything else the old CLI did (config editing, provider management, network
// and webhook administration, daemon supervision) is either the dashboard's job
// or not yet reimplemented.
//
// Control is exercised over the daemon's HTTP API rather than by touching
// SQLite or signalling processes directly, because the daemon owns run
// ownership and containment; a second writer would race it. Project
// registration follows the same rule: the daemon materializes its project
// catalog from SQLite, so a project written into the config file would only
// take effect on restart, while POST /api/v1/projects is live.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/MumuTW/looper/internal/agentdiscovery"
	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/version"
)

// requestTimeout bounds a single call. Those are one storage transaction and at
// most one process signal, so a daemon that has not answered in 30s is wedged,
// not busy.
//
// It is a var only so tests can shorten it; nothing reassigns it at runtime.
var requestTimeout = 30 * time.Second

// bulkStopRequestTimeout bounds how long an interactive client waits for the
// summary. The daemon deliberately detaches the stop-all sweep from client
// cancellation, so exceeding this deadline loses only the report and never
// truncates the operation.
//
// It is a var only so tests can shorten it; nothing reassigns it at runtime.
var bulkStopRequestTimeout = 10 * time.Minute

// projectDiscoveryTimeout bounds only the explicit post-registration discovery
// request. Registration itself uses requestTimeout because it commits before
// returning; discovery may scan every open pull request and needs a larger
// finite budget.
var projectDiscoveryTimeout = 10 * time.Minute

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	parsed, err := splitGlobalFlags(args)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "looper: %v\n", err)
		usage(stderr)
		return 2
	}
	if parsed.Verb == "-h" || parsed.Verb == "--help" || parsed.Verb == "help" {
		usage(stdout)
		return 0
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if parsed.Verb == "--version" || parsed.Verb == "version" {
		return reportError(stderr, runVersion(ctx, parsed.Global, parsed.Operands, stdout, version.Current()))
	}
	if parsed.Verb == "upgrade" {
		return reportError(stderr, runUpgrade(ctx, parsed.Global, parsed.Operands, stdout))
	}

	// `review` owns its own flag set, and the daemon's trusted review proxy
	// composes that argv. Routing it before splitGlobalFlags is what keeps
	// --event and --commit-id from being rejected as unknown flags.
	if parsed.Verb == "review" {
		return runReview(ctx, args, stdin, stdout, stderr)
	}

	// retry owns --discard-worktree-changes / --confirm and a GET preflight.
	// Those flags must not go through splitGlobalFlags (which rejects any
	// non-global dash-argument) and must not collapse to a bodyless POST.
	if parsed.Verb == "retry" {
		return runRetry(ctx, args, stdout, stderr)
	}

	// The onboarding verbs take no flags of their own — only the global ones —
	// so they are routed after splitGlobalFlags like the control verbs, and
	// unlike `review`. That is what keeps `looper --config /p status` and
	// `looper status --config /p` the same invocation, keeps an unknown flag
	// refused rather than read as an operand, and hands config.LoadFile only
	// the global flags instead of the whole command line.
	//
	// They are dispatched here rather than through routeForVerb because none of
	// them is a single daemon call: init touches no daemon at all, while status
	// and project compose several calls and report them.
	switch parsed.Verb {
	case "init":
		return reportError(stderr, runInit(parsed.Global, parsed.Operands, stdout))
	case "status":
		return reportError(stderr, runStatus(ctx, parsed.Global, parsed.Operands, stdout))
	case "dashboard":
		return reportError(stderr, runDashboard(ctx, parsed.Global, parsed.Operands, stdout))
	case "project":
		return reportError(stderr, runProject(ctx, parsed.Global, parsed.Operands, stdout))
	case "gatekeeper":
		return reportError(stderr, runGatekeeper(ctx, parsed.Global, parsed.Operands, stdout))
	case "provider":
		return reportError(stderr, runProvider(ctx, parsed.Global, parsed.Operands, stdout))
	}

	request, err := routeForVerb(parsed.Verb, parsed.Operands)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "looper: %v\n", err)
		usage(stderr)
		return 2
	}

	cfg, err := loadConfig(parsed.Global)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "looper: %v\n", err)
		return 1
	}

	body, err := post(ctx, cfg, request)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "looper: %v\n", err)
		return 1
	}
	// stop-all answers HTTP 200 even when some loops only paused or failed;
	// the old CLI exited nonzero so automation cannot treat "partial stop" as success.
	if request.Path == stopAllPath {
		if err := stopAllPartialFailure(body); err != nil {
			_, _ = fmt.Fprintln(stdout, strings.TrimSpace(body))
			_, _ = fmt.Fprintf(stderr, "looper: %v\n", err)
			return 1
		}
	}
	_, _ = fmt.Fprintln(stdout, strings.TrimSpace(body))
	return 0
}

// reportError turns a verb's error into the process exit code, keeping the two
// failure kinds a script can act on distinct: 2 for an invocation the CLI
// refused to run, 1 for one that ran and failed.
func reportError(stderr io.Writer, err error) int {
	if err == nil {
		return 0
	}
	var usageErr usageError
	if errors.As(err, &usageErr) {
		_, _ = fmt.Fprintf(stderr, "looper: %v\n", usageErr.err)
		usage(stderr)
		return 2
	}
	_, _ = fmt.Fprintf(stderr, "looper: %v\n", err)
	return 1
}

// usageError marks an operator mistake in the invocation itself, which exits 2
// with the usage text, as opposed to a runtime failure, which exits 1.
type usageError struct{ err error }

func (e usageError) Error() string { return e.err.Error() }

func badUsage(format string, args ...any) error {
	return usageError{err: fmt.Errorf(format, args...)}
}

// apiRequest is the single daemon call one CLI invocation makes. Body is nil
// for the bare control verbs; carrying it here rather than assembling it at
// the call site keeps routeForVerb a pure function, so the entire
// argument-to-request mapping is testable without a running daemon.
type apiRequest struct {
	Path string
	Body []byte
	// LongRunning selects the larger finite request budget used only for a
	// bulk stop, whose runtime includes one kill budget per active loop.
	LongRunning bool
}

// stopAllPath is the daemon's bulk stop route. The CLI branches on it twice —
// for the deadline and for the partial-failure check — so it is named once.
const stopAllPath = "/api/v1/runs/active/stop-all"

// respondBody mirrors the daemon's respondLoopRequest. It is restated rather
// than imported because internal/api is not importable from cmd, and the
// field is one string: a drift here fails loudly as "respond requires a
// non-empty answer" from the daemon, not silently.
type respondBody struct {
	Answer string `json:"answer"`
}

// globalFlag is a flag the CLI accepts before or after the verb and forwards
// to config.LoadFile rather than acting on itself. Every one takes a value, in
// either `--flag value` or `--flag=value` form.
//
// The list is deliberately a subset of what config.LoadFile parses: those are
// the three an operator needs to reach a daemon that is not the default one,
// and an unlisted flag is refused rather than silently swallowed as a
// selector.
var globalFlags = map[string]struct{}{
	"--config": {},
	"--host":   {},
	"--port":   {},
}

// parsedArgs is one command line split into the part the CLI routes on and the
// part config.LoadFile reads.
type parsedArgs struct {
	Verb     string
	Operands []string
	// Global holds the global flags in their original order, including their
	// values, so LoadFile sees exactly what the operator typed and nothing
	// else. Passing the whole command line instead would let an operand after
	// `--` be reinterpreted as configuration.
	Global []string
}

// splitGlobalFlags is the CLI's single argument-routing authority. It separates
// global flags from the verb and operands, recognizes top-level help/version,
// and preserves verb-owned flags for review/retry so their dedicated parsers
// can validate them. This keeps dispatch and flag parsing from drifting apart.
//
// It allows
// both `looper --config /path stop 12` and `looper stop 12 --config /path`
// reach the same request. Routing used to run on the raw argv, which made the
// first form an unknown command and the second an arity error — the flags were
// documented but unreachable.
//
// Value consumption matches config.parseCLIArgs: `--flag=value` inline, else
// the next argument unless it starts with `--`. The two parsers must agree,
// because a value this one skips is a value LoadFile still reads.
func splitGlobalFlags(args []string) (parsedArgs, error) {
	parsed := parsedArgs{}

	for index := 0; index < len(args); index++ {
		arg := args[index]

		// Everything after `--` is an operand, which is how an answer or a
		// selector that begins with a dash gets through.
		if arg == "--" {
			remaining := args[index+1:]
			if parsed.Verb == "" && len(remaining) > 0 {
				parsed.Verb = remaining[0]
				remaining = remaining[1:]
			}
			parsed.Operands = append(parsed.Operands, remaining...)
			break
		}

		name, inlineValue, hasInline := strings.Cut(arg, "=")
		if _, global := globalFlags[name]; global {
			if hasInline {
				parsed.Global = append(parsed.Global, name+"="+inlineValue)
				continue
			}
			if index+1 >= len(args) || strings.HasPrefix(args[index+1], "--") {
				return parsedArgs{}, fmt.Errorf("missing value for %s", name)
			}
			parsed.Global = append(parsed.Global, name, args[index+1])
			index++
			continue
		}

		if parsed.Verb == "" && (arg == "--version" || arg == "--help" || arg == "-h") {
			parsed.Verb = arg
			continue
		}
		if strings.HasPrefix(arg, "-") && arg != "-" && parsed.Verb != "review" && parsed.Verb != "retry" && parsed.Verb != "version" && parsed.Verb != "--version" && parsed.Verb != "provider" {
			return parsedArgs{}, fmt.Errorf("unknown flag %q", name)
		}
		if parsed.Verb == "" {
			parsed.Verb = arg
		} else {
			parsed.Operands = append(parsed.Operands, arg)
		}
	}

	if parsed.Verb == "" {
		return parsedArgs{}, fmt.Errorf("a command is required")
	}
	return parsed, nil
}

// routeForVerb maps a control verb to its API request. Selectors are passed
// through unvalidated: the daemon resolves them and is the only component that
// can say authoritatively what a selector means, so validating here would only
// invent a second, staler answer.
// Emptiness and argument count are checked locally because those are decidable
// here and cost a round trip otherwise.
func routeForVerb(verb string, args []string) (apiRequest, error) {
	switch verb {
	case "stop", "close":
		if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
			return apiRequest{}, fmt.Errorf("%s requires exactly one selector", verb)
		}
		// Bulk stop is its own selector on the daemon, not a selector named
		// "all" with a /stop suffix: that path parses as a single-loop stop
		// and dies resolving a loop literally called "all".
		//
		// It also gets a longer finite deadline. The daemon stops candidates one at a time and
		// each one may spend up to the runtime's kill budget (20s, see
		// internal/runtime defaultKillTimeout) draining a containment handle,
		// so two unresponsive agents already outlast the 30s deadline the other
		// verbs use. Timing out here does not merely lose the report: the
		// daemon runs the sweep on the request context, so the CLI's deadline
		// aborts it partway and no summary says which loops were stopped.
		// Ctrl-C still cancels — run()'s context is signal-bound.
		if strings.TrimSpace(args[0]) == "all" && verb == "stop" {
			return apiRequest{Path: stopAllPath, LongRunning: true}, nil
		}
		segment, err := selectorPathSegment(args[0])
		if err != nil {
			return apiRequest{}, err
		}
		return apiRequest{Path: "/api/v1/runs/active/" + segment + "/" + verb}, nil
	// retry is absent deliberately: run dispatches it to runRetry before this
	// function is reached, because it needs a GET preflight and flags of its
	// own. Leaving a case here would be a branch no invocation can take, and a
	// test covering it would report success for a path the CLI never walks.
	case "takeover", "start", "pause", "handback":
		if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
			return apiRequest{}, fmt.Errorf("%s requires exactly one loop id", verb)
		}
		segment, err := selectorPathSegment(args[0])
		if err != nil {
			return apiRequest{}, err
		}
		return apiRequest{Path: "/api/v1/loops/" + segment + "/" + verb}, nil
	case "respond":
		// The answer is one argument, not the joined tail: a loop waiting on a
		// human is answered with prose, and joining would turn a forgotten
		// quote into a silently truncated answer instead of an error.
		if len(args) != 2 || strings.TrimSpace(args[0]) == "" {
			return apiRequest{}, fmt.Errorf("respond requires a loop id and one quoted answer")
		}
		answer := strings.TrimSpace(args[1])
		if answer == "" {
			return apiRequest{}, fmt.Errorf("respond requires a non-empty answer")
		}
		segment, err := selectorPathSegment(args[0])
		if err != nil {
			return apiRequest{}, err
		}
		body, err := json.Marshal(respondBody{Answer: answer})
		if err != nil {
			return apiRequest{}, fmt.Errorf("encode answer: %w", err)
		}
		return apiRequest{Path: "/api/v1/loops/" + segment + "/respond", Body: body}, nil
	default:
		return apiRequest{}, fmt.Errorf("unknown command %q", verb)
	}
}

// selectorPathSegment turns a selector into exactly one URL path segment.
//
// Escaping matters because a selector is interpolated into the request path: a
// "#" would truncate it to a fragment and a space would not survive the
// request line at all. A "/" is refused rather than escaped, because the
// daemon routes on the *decoded* path (api.Handler splits r.URL.Path), so an
// escaped slash decodes back into a segment separator and the request lands on
// "Unknown route" — an operator would read that as "no such loop". The daemon
// resolves loop sequence numbers and loop ids, neither of which contains a
// slash, so refusing here loses nothing.
func selectorPathSegment(selector string) (string, error) {
	trimmed := strings.TrimSpace(selector)
	if strings.Contains(trimmed, "/") {
		return "", fmt.Errorf("selector %q is not a loop id or sequence number; the daemon resolves those, not pull request URLs", trimmed)
	}
	return url.PathEscape(trimmed), nil
}

func loadOptions(args []string) (config.LoadFileOptions, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return config.LoadFileOptions{}, fmt.Errorf("determine working directory: %w", err)
	}
	return config.LoadFileOptions{
		CWD:       cwd,
		Args:      append([]string(nil), args...),
		LookupEnv: os.LookupEnv,
	}, nil
}

func loadConfig(args []string) (config.Config, error) {
	options, err := loadOptions(args)
	if err != nil {
		return config.Config{}, err
	}
	loaded, err := config.LoadFile(options)
	if err != nil {
		return config.Config{}, err
	}
	return applyEndpointOverrides(loaded.Config, args), nil
}

// applyEndpointOverrides makes --host and --port replace their corresponding
// components in a configured server.baseUrl.
//
// The check reads the already-split global flags rather than the raw command
// line, so `--host` appearing as an operand (after `--`, or as a respond
// answer) cannot be mistaken for an endpoint override.
func applyEndpointOverrides(cfg config.Config, global []string) config.Config {
	overrideHost := false
	overridePort := false
	for _, arg := range global {
		name, _, _ := strings.Cut(arg, "=")
		switch name {
		case "--host":
			overrideHost = true
		case "--port":
			overridePort = true
		}
	}
	if (!overrideHost && !overridePort) || cfg.Server.BaseURL == nil || strings.TrimSpace(*cfg.Server.BaseURL) == "" {
		return cfg
	}

	base, err := url.Parse(strings.TrimSpace(*cfg.Server.BaseURL))
	if err != nil || base.Hostname() == "" {
		// Config validation normally makes this unreachable. Falling back to
		// the bind-style endpoint still honors the flags if an invalid Config
		// value is supplied directly in a test or embedding.
		cfg.Server.BaseURL = nil
		return cfg
	}
	host := base.Hostname()
	port := base.Port()
	if overrideHost {
		host = dialHost(cfg.Server.Host)
	}
	if overridePort {
		port = strconv.Itoa(cfg.Server.Port)
	}
	if port == "" {
		base.Host = host
		if strings.Contains(host, ":") {
			base.Host = "[" + host + "]"
		}
	} else {
		base.Host = net.JoinHostPort(host, port)
	}
	value := base.String()
	cfg.Server.BaseURL = &value
	return cfg
}

// --- provider discovery ----------------------------------------------------

type providerConfigPatch struct {
	Revision string                     `json:"revision"`
	Set      map[string]json.RawMessage `json:"set"`
	Unset    []string                   `json:"unset"`
}

func runProvider(ctx context.Context, global, operands []string, stdout io.Writer) error {
	if len(operands) == 0 || operands[0] != "discover" {
		return badUsage("provider requires discover")
	}
	apply := false
	confirm := false
	for _, operand := range operands[1:] {
		switch operand {
		case "--apply":
			apply = true
		case "--confirm":
			confirm = true
		default:
			return badUsage("provider discover accepts only --apply and --confirm")
		}
	}
	if confirm && !apply {
		return badUsage("provider discover --confirm requires --apply")
	}

	cfg, err := loadConfig(global)
	if err != nil {
		return err
	}
	report, err := requestJSON[agentdiscovery.Report](ctx, cfg, http.MethodGet, "/api/v1/agent/providers/discovery", nil)
	if err != nil {
		return err
	}
	for _, candidate := range report.Candidates {
		line := fmt.Sprintf("%s: %s", candidate.Vendor, candidate.Status)
		if candidate.Executable != "" {
			line += " path=" + candidate.Executable
		}
		if candidate.Version != "" {
			line += " version=" + strconv.Quote(candidate.Version)
		}
		if candidate.Diagnostic != "" {
			line += " (" + singleLine(candidate.Diagnostic) + ")"
		}
		_, _ = fmt.Fprintln(stdout, line)
	}
	if report.Suggestion == nil || len(report.Suggestion.Set) == 0 {
		_, _ = fmt.Fprintln(stdout, "suggestion: none")
		return nil
	}
	suggestionJSON, err := json.Marshal(report.Suggestion)
	if err != nil {
		return fmt.Errorf("encode provider suggestion: %w", err)
	}
	_, _ = fmt.Fprintf(stdout, "suggestion: %s\n", suggestionJSON)
	if !apply {
		_, _ = fmt.Fprintln(stdout, "not applied; review the suggestion and rerun with --apply --confirm")
		return nil
	}
	if !confirm {
		_, _ = fmt.Fprintln(stdout, "not applied; --confirm is required after reviewing the suggestion")
		return nil
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("provider discovery cancelled before apply: %w", err)
	}
	if strings.TrimSpace(report.ConfigRevision) == "" {
		return fmt.Errorf("provider discovery response carried no config revision; refusing mutation")
	}
	patch := providerConfigPatch{Revision: report.ConfigRevision, Set: report.Suggestion.Set, Unset: []string{}}
	body, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("encode provider config patch: %w", err)
	}
	if _, err := doHTTP(ctx, cfg, http.MethodPatch, "/api/v1/config", body); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(stdout, "applied the reviewed provider suggestion")
	return nil
}

// --- init ------------------------------------------------------------------

// starterConfig is the file `looper init` writes. It is a commented template
// rather than a marshalled config.Config because the point of the file is to
// show an operator which knobs matter; encoding a fully defaulted struct would
// emit hundreds of fields and no explanation, and no encoder preserves
// comments. Everything omitted here falls back to the built-in defaults.
const starterConfig = `# looper configuration
#
# Only the fields you want to change need to be present; anything omitted uses
# looper's built-in defaults. See docs/configuration.md for the full reference.

[server]
# Address looperd listens on, and the address ` + "`looper`" + ` talks to.
host = "127.0.0.1"
port = 17310

[agent]
# The coding agent looperd drives. One of:
#   claude-code, codex, opencode, cursor-cli, grok-build, devin-experimental
# devin-experimental is fresh-run only.
vendor = "claude-code"
# model = "opus"

[defaults]
# Branch new work is based on when a project does not override it.
baseBranch = "main"

# Projects are normally registered against the running daemon with
#   looper project add /path/to/repo
# which stores them in looper's database and takes effect immediately.
#
# Pin a project to this file instead only if you want it managed as
# configuration; file-managed projects are read at daemon startup and cannot be
# changed through the API.
#
# [[projects]]
# id = "my-project"
# name = "My Project"
# repoPath = "/absolute/path/to/repo"
# repo = "owner/name"
`

func runInit(global []string, operands []string, stdout io.Writer) error {
	if len(operands) != 0 {
		return badUsage("init takes no arguments")
	}
	options, err := loadOptions(global)
	if err != nil {
		return err
	}
	path, err := config.SelectConfigPath(options)
	if err != nil {
		return err
	}
	if !strings.EqualFold(filepath.Ext(path), ".toml") {
		return fmt.Errorf("init writes TOML, but the selected config path is %s; pass --config <file>.toml or write %s by hand", path, path)
	}

	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("config already exists at %s; edit that file instead (init never overwrites)", path)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("check %s: %w", path, err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	if err := writeNewConfigFile(path, strings.NewReader(starterConfig)); err != nil {
		return err
	}

	_, _ = fmt.Fprintf(stdout, "wrote starter config to %s\n", path)
	_, _ = fmt.Fprintf(stdout, "next: edit it, start looperd, then run `looper project add <repo>` and `looper status`\n")
	return nil
}

// writeNewConfigFile creates path and fills it, or leaves nothing behind.
//
// O_EXCL closes the window between init's stat and this write: a config that
// appeared in between must not be clobbered. Every failure after that create
// unlinks the file, because a truncated config is worse than no config at all —
// init would refuse to repair it (the path exists), and looperd fails fast
// trying to load it, so the operator is stuck holding a file nothing will
// accept and nothing will replace.
func writeNewConfigFile(path string, contents io.Reader) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("config already exists at %s; edit that file instead (init never overwrites)", path)
		}
		return fmt.Errorf("create %s: %w", path, err)
	}
	if _, err := io.Copy(file, contents); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// --- status ----------------------------------------------------------------

func runStatus(ctx context.Context, global []string, operands []string, stdout io.Writer) error {
	if len(operands) != 0 {
		return badUsage("status takes no arguments")
	}
	options, err := loadOptions(global)
	if err != nil {
		return err
	}

	path, pathErr := config.SelectConfigPath(options)
	if pathErr != nil {
		path = "(unknown)"
	}

	// The config is reported even when it fails to load, because "which file is
	// broken" is the answer the operator needs. The daemon is not probed in that
	// case: the config is what says where the daemon listens, so any address
	// tried here would be a guess reported as fact.
	var cfg config.Config
	loadErr := pathErr
	if loadErr == nil {
		loaded, err := config.LoadFile(options)
		if err != nil {
			loadErr = err
		} else {
			cfg = applyEndpointOverrides(loaded.Config, global)
			path = loaded.Metadata.ConfigPath
		}
	}

	if loadErr != nil {
		_, _ = fmt.Fprintf(stdout, "config:   %s\n", path)
		_, _ = fmt.Fprintf(stdout, "          FAILED: %v\n", singleLine(loadErr.Error()))
		_, _ = fmt.Fprintf(stdout, "daemon:   unknown (no usable config to say where it listens)\n")
		_, _ = fmt.Fprintf(stdout, "projects: unknown\n")
		return fmt.Errorf("config at %s did not load", path)
	}

	state := "ok"
	if _, err := os.Stat(path); os.IsNotExist(err) {
		state = "not found, using built-in defaults"
	}
	_, _ = fmt.Fprintf(stdout, "config:   %s (%s)\n", path, state)

	endpoint := daemonBaseURL(cfg)
	health, healthErr := requestJSON[healthResponse](ctx, cfg, http.MethodGet, "/api/v1/healthz", nil)
	if healthErr != nil {
		_, _ = fmt.Fprintf(stdout, "daemon:   %s (unreachable)\n", endpoint)
		_, _ = fmt.Fprintf(stdout, "          %v\n", singleLine(healthErr.Error()))
		_, _ = fmt.Fprintf(stdout, "projects: unknown (needs a running daemon)\n")
		return fmt.Errorf("looperd is not reachable at %s", endpoint)
	}

	daemonState := "healthy"
	if !health.Healthy {
		daemonState = "degraded"
	}
	_, _ = fmt.Fprintf(stdout, "daemon:   %s (reachable, %s)\n", endpoint, daemonState)

	// /api/v1/status carries ops readiness (review publish + quarantine debt).
	// healthz stays liveness-only; failure here is non-fatal so onboarding still
	// reports projects when an older daemon lacks the newer status fields.
	if status, statusErr := requestJSON[daemonStatusResponse](ctx, cfg, http.MethodGet, "/api/v1/status", nil); statusErr != nil {
		_, _ = fmt.Fprintf(stdout, "ops:      status unavailable: %v\n", singleLine(statusErr.Error()))
	} else {
		writeStatusOpsLines(stdout, status)
	}

	projects, projectsErr := requestJSON[projectsListResponse](ctx, cfg, http.MethodGet, "/api/v1/projects", nil)
	if projectsErr != nil {
		_, _ = fmt.Fprintf(stdout, "projects: unavailable: %v\n", singleLine(projectsErr.Error()))
		return projectsErr
	}
	_, _ = fmt.Fprintf(stdout, "projects: %d\n", len(projects.Items))
	for _, project := range projects.Items {
		_, _ = fmt.Fprintf(stdout, "  - %s\n", describeProject(project))
	}

	return nil
}

// --- project ---------------------------------------------------------------

func runProject(ctx context.Context, global []string, operands []string, stdout io.Writer) error {
	if len(operands) == 0 {
		return badUsage("project requires a subcommand (add, list, discover)")
	}
	switch operands[0] {
	case "list":
		if len(operands) != 1 {
			return badUsage("project list takes no arguments")
		}
		return runProjectList(ctx, global, stdout)
	case "add":
		if len(operands) != 2 || strings.TrimSpace(operands[1]) == "" {
			return badUsage("project add requires exactly one repository path")
		}
		return runProjectAdd(ctx, global, operands[1], stdout)
	case "discover":
		if len(operands) != 2 || strings.TrimSpace(operands[1]) == "" {
			return badUsage("project discover requires exactly one project id")
		}
		return runProjectDiscover(ctx, global, operands[1], stdout)
	default:
		return badUsage("unknown project subcommand %q", operands[0])
	}
}

func runProjectList(ctx context.Context, global []string, stdout io.Writer) error {
	cfg, err := loadConfig(global)
	if err != nil {
		return err
	}
	projects, err := requestJSON[projectsListResponse](ctx, cfg, http.MethodGet, "/api/v1/projects", nil)
	if err != nil {
		return err
	}
	if len(projects.Items) == 0 {
		_, _ = fmt.Fprintln(stdout, "no projects registered")
		return nil
	}
	for _, project := range projects.Items {
		_, _ = fmt.Fprintln(stdout, describeProject(project))
	}
	return nil
}

func runProjectAdd(ctx context.Context, global []string, repoPath string, stdout io.Writer) error {
	cfg, err := loadConfig(global)
	if err != nil {
		return err
	}

	// Repository validation happens in the CLI process, so it must resolve git
	// from this process's PATH. tools.gitPath belongs to the daemon environment
	// and may name a path that does not exist on a remote client.
	resolved, err := resolveRepoRoot(ctx, "git", repoPath)
	if err != nil {
		return err
	}

	// Refuse an already-registered checkout with a focused message. The daemon
	// remains authoritative for IDs and enforces derived-ID collisions under
	// its mutation lock; this catalog read is only an early duplicate-path
	// diagnostic.
	existing, err := requestJSON[projectsListResponse](ctx, cfg, http.MethodGet, "/api/v1/projects", nil)
	if err != nil {
		return err
	}
	for _, project := range existing.Items {
		if project.Archived {
			continue
		}
		if sameRepoPath(project.RepoPath, resolved) {
			return fmt.Errorf("%s is already registered as project %q", resolved, project.ID)
		}
	}

	payload, err := json.Marshal(map[string]string{"repoPath": resolved})
	if err != nil {
		return err
	}
	created, err := requestJSON[createProjectResponse](ctx, cfg, http.MethodPost, "/api/v1/projects", payload)
	if err != nil {
		return err
	}

	_, _ = fmt.Fprintf(stdout, "registered project %s\n", created.ID)
	_, _ = fmt.Fprintf(stdout, "  repoPath:   %s\n", created.RepoPath)
	_, _ = fmt.Fprintf(stdout, "  baseBranch: %s\n", created.BaseBranch)
	if created.Repo != nil && strings.TrimSpace(*created.Repo) != "" {
		_, _ = fmt.Fprintf(stdout, "  repo:       %s\n", *created.Repo)
	}
	// Registration completed; worktree/PR discovery runs post-commit in the
	// daemon and never gates this result.
	if created.Discovery != nil && created.Discovery.Status != "" {
		_, _ = fmt.Fprintf(stdout, "  discovery:  %s (post-commit; retry with `looper project discover %s` if it fails)\n", created.Discovery.Status, created.ID)
	}
	for _, warning := range created.Warnings {
		_, _ = fmt.Fprintf(stdout, "  warning:    %s\n", singleLine(warning))
	}
	return nil
}

func runProjectDiscover(ctx context.Context, global []string, identifier string, stdout io.Writer) error {
	cfg, err := loadConfig(global)
	if err != nil {
		return err
	}
	result, err := requestProjectDiscoveryWithin(ctx, projectDiscoveryTimeout, cfg, identifier)
	if err != nil {
		return err
	}
	return printProjectDiscoveryResult(result, stdout)
}

// runGatekeeper owns Gatekeeper operator views and the explicit trust-promotion
// action. It routes through looperd rather than opening SQLite: the daemon is
// authoritative for the project catalog and immutable Gatekeeper event stream.
func runGatekeeper(ctx context.Context, global []string, operands []string, stdout io.Writer) error {
	if len(operands) == 0 || (operands[0] != "agreements" && operands[0] != "verdicts" && operands[0] != "promote") {
		return badUsage("gatekeeper requires the agreements, verdicts, or promote subcommand")
	}
	if operands[0] == "promote" {
		if len(operands) != 3 || strings.TrimSpace(operands[1]) == "" || strings.TrimSpace(operands[2]) == "" {
			return badUsage("gatekeeper promote requires a project id and target trust (advise or auto)")
		}
		target := strings.ToLower(strings.TrimSpace(operands[2]))
		if target != "advise" && target != "auto" {
			return badUsage("gatekeeper promote target must be advise or auto")
		}
		cfg, err := loadConfig(global)
		if err != nil {
			return err
		}
		body, err := json.Marshal(map[string]string{"gatekeeperTrust": target})
		if err != nil {
			return err
		}
		projectID := strings.TrimSpace(operands[1])
		updated, err := requestJSON[projectResponse](ctx, cfg, http.MethodPatch, "/api/v1/projects/"+url.PathEscape(projectID), body)
		if err != nil {
			return err
		}
		trust := strings.TrimSpace(updated.GatekeeperTrust)
		if trust == "" {
			trust = "observe"
		}
		_, _ = fmt.Fprintf(stdout, "project %s  gatekeeper-trust=%s\n", updated.ID, trust)
		return nil
	}
	if len(operands) > 2 || (len(operands) == 2 && strings.TrimSpace(operands[1]) == "") {
		return badUsage(fmt.Sprintf("gatekeeper %s accepts at most one project id", operands[0]))
	}

	cfg, err := loadConfig(global)
	if err != nil {
		return err
	}
	path := "/api/v1/gatekeeper/" + operands[0]
	if len(operands) == 2 {
		// Project IDs are exact configuration keys; only the all-whitespace
		// validation above treats surrounding whitespace as insignificant.
		path += "?projectId=" + url.QueryEscape(operands[1])
	}
	if operands[0] == "agreements" {
		agreements, err := requestJSON[gatekeeperAgreementsResponse](ctx, cfg, http.MethodGet, path, nil)
		if err != nil {
			return err
		}
		if len(agreements.Items) == 0 {
			_, _ = fmt.Fprintln(stdout, "no advise agreements recorded")
			return nil
		}
		for _, agreement := range agreements.Items {
			outcome := strings.TrimSpace(agreement.Outcome)
			if outcome == "" {
				outcome = "unknown"
			}
			agreementLabel := "disagreement"
			if agreement.Agreement {
				agreementLabel = "agreement"
			}
			_, _ = fmt.Fprintf(stdout, "%s  %s#%d  %s  %s  %s  verdict=%s\n", agreement.ProjectID, agreement.Repo, agreement.PRNumber, outcome, agreementLabel, agreement.RecordedAt, agreement.VerdictEventID)
		}
		return nil
	}

	verdicts, err := requestJSON[gatekeeperVerdictsResponse](ctx, cfg, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	if len(verdicts.Items) == 0 {
		_, _ = fmt.Fprintln(stdout, "no gatekeeper verdicts recorded")
		return nil
	}
	for _, verdict := range verdicts.Items {
		status := strings.TrimSpace(verdict.Status)
		if status == "" {
			status = "unknown"
		}
		reasons := make([]string, 0, len(verdict.Reasons))
		for _, reason := range verdict.Reasons {
			value := string(reason.Code)
			if strings.TrimSpace(reason.Subject) != "" {
				value += "(" + strings.TrimSpace(reason.Subject) + ")"
			}
			reasons = append(reasons, value)
		}
		reasonText := "-"
		if len(reasons) > 0 {
			reasonText = strings.Join(reasons, ",")
		}
		_, _ = fmt.Fprintf(stdout, "%s  %s#%d  %s  head=%s  evaluated=%s  reasons=%s\n", verdict.ProjectID, verdict.Repo, verdict.PRNumber, status, verdict.ObservedHeadSHA, verdict.EvaluatedAt, reasonText)
	}
	return nil
}

func printProjectDiscoveryResult(result createProjectResponse, stdout io.Writer) error {
	if result.Discovery == nil {
		_, _ = fmt.Fprintf(stdout, "discovery for project %s: unknown (daemon did not report discovery status)\n", result.ID)
		return nil
	}
	_, _ = fmt.Fprintf(stdout, "discovery for project %s: %s\n", result.ID, result.Discovery.Status)
	if result.Discovery.Error != "" {
		_, _ = fmt.Fprintf(stdout, "  error:      %s\n", singleLine(result.Discovery.Error))
	}
	if result.Discovery.Status == "succeeded" {
		_, _ = fmt.Fprintf(stdout, "  worktrees:  %d\n", result.Discovery.DiscoveredWorktrees)
		_, _ = fmt.Fprintf(stdout, "  pullRequests: %d\n", result.Discovery.DiscoveredPullRequests)
	}
	for _, warning := range result.Discovery.Warnings {
		_, _ = fmt.Fprintf(stdout, "  warning:    %s\n", singleLine(warning))
	}
	if result.Discovery.Status == "failed" {
		if message := strings.TrimSpace(result.Discovery.Error); message != "" {
			return errors.New(message)
		}
		return errors.New("project discovery failed")
	}
	return nil
}

func requestProjectDiscoveryWithin(ctx context.Context, timeout time.Duration, cfg config.Config, identifier string) (createProjectResponse, error) {
	return requestJSONWithin[createProjectResponse](ctx, timeout, cfg, http.MethodPost, "/api/v1/projects/"+url.PathEscape(identifier)+"/discover", nil)
}

// resolveRepoRoot turns an operator-supplied path into the absolute repository
// root the daemon should store.
//
// Authority is git, asked directly. A path merely *containing* something named
// .git proves nothing: it can be an empty directory, a plain file, or a
// worktree pointer into a repository that has since been deleted, and looperd
// would only discover that after the project was persisted, when discovery or
// worktree creation fails. `git rev-parse --show-toplevel` is the same question
// the daemon's own git invocations ask, so a path this accepts is one they can
// use, and a broken checkout is refused while the operator is still looking at
// the command that named it.
//
// Only a root is accepted: looperd creates worktrees from this path, and a
// subdirectory would silently produce a project whose worktrees do not match
// its checkout. rev-parse answers from inside a subdirectory too, which is what
// lets the refusal name the root the operator probably meant.
//
// The root git reports is also the canonical one — symlinks resolved — so it,
// not the typed path, is what gets stored and compared. /tmp/repo and
// /private/tmp/repo are one checkout, and two projects for one checkout would
// discover and operate on the same worktrees.
func resolveRepoRoot(ctx context.Context, gitPath string, repoPath string) (string, error) {
	resolved, err := filepath.Abs(strings.TrimSpace(repoPath))
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", repoPath, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("%s does not exist", resolved)
		}
		return "", fmt.Errorf("inspect %s: %w", resolved, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", resolved)
	}

	toplevel, err := gitTopLevel(ctx, gitPath, resolved)
	if err != nil {
		return "", err
	}
	// Compare against the canonicalized input, not the literal argument:
	// rev-parse already resolved symlinks, so /tmp/repo would otherwise read as
	// a subdirectory of /private/tmp/repo.
	canonical := resolved
	if evaluated, err := filepath.EvalSymlinks(resolved); err == nil {
		canonical = evaluated
	}
	if filepath.Clean(toplevel) != filepath.Clean(canonical) {
		return "", fmt.Errorf("%s is inside the repository at %s, not its root; register %s instead", resolved, toplevel, toplevel)
	}
	return toplevel, nil
}

// gitTopLevel asks git for the work-tree root containing dir. A repository
// without a work tree (a bare one) reports an empty root, which is equally
// unusable: looperd cannot create worktrees from it.
func gitTopLevel(ctx context.Context, gitPath string, dir string) (string, error) {
	command := exec.CommandContext(ctx, gitPath, "-C", dir, "rev-parse", "--show-toplevel")
	stderr := &bytes.Buffer{}
	command.Stderr = stderr
	output, err := command.Output()
	if err != nil {
		detail := singleLine(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("%s is not a usable git repository: %s", dir, detail)
	}
	toplevel := strings.TrimSpace(string(output))
	if toplevel == "" {
		return "", fmt.Errorf("%s is not a usable git repository: git reported no work tree", dir)
	}
	return toplevel, nil
}

func sameRepoPath(left string, right string) bool {
	return canonicalRepoPath(left) == canonicalRepoPath(right)
}

func canonicalRepoPath(path string) string {
	cleaned := filepath.Clean(strings.TrimSpace(path))
	if evaluated, err := filepath.EvalSymlinks(cleaned); err == nil {
		return filepath.Clean(evaluated)
	}
	return cleaned
}

func describeProject(project projectResponse) string {
	line := fmt.Sprintf("%s\t%s", project.ID, project.RepoPath)
	trust := strings.TrimSpace(project.GatekeeperTrust)
	if trust == "" {
		trust = "observe"
	}
	line += "\tgatekeeper-trust=" + trust
	if project.Archived {
		line += "\t(archived)"
	}
	if project.Discovery != nil && project.Discovery.Status != "" {
		line += "\tdiscovery=" + project.Discovery.Status
		if project.Discovery.Error != "" {
			line += ": " + singleLine(project.Discovery.Error)
		}
	}
	return line
}

// --- HTTP ------------------------------------------------------------------

type healthResponse struct {
	Healthy bool `json:"healthy"`
}

// daemonStatusResponse is the subset of GET /api/v1/status that `looper status`
// prints for ops readiness. Unknown fields are ignored.
type daemonStatusResponse struct {
	Service       statusServiceView       `json:"service"`
	Tools         statusToolsView         `json:"tools"`
	ResourceGuard statusResourceGuardView `json:"resourceGuard"`
}

type statusServiceView struct {
	DegradedReasons []string               `json:"degradedReasons"`
	Recovery        statusRecoveryView     `json:"recovery"`
	Triage          statusTriageView       `json:"triage"`
	Binary          statusBinaryView       `json:"binary"`
	AgentHealth     *statusAgentHealthView `json:"agentHealth"`
}

type statusAgentHealthView struct {
	State     string                          `json:"state"`
	Partial   bool                            `json:"partial"`
	Failures  int                             `json:"failures"`
	Total     int                             `json:"total"`
	OpenUntil *string                         `json:"openUntil"`
	Trips     int                             `json:"trips"`
	Providers []statusAgentProviderHealthView `json:"providers"`
}

type statusAgentProviderHealthView struct {
	Provider  string  `json:"provider"`
	State     string  `json:"state"`
	Failures  int     `json:"failures"`
	Total     int     `json:"total"`
	OpenUntil *string `json:"openUntil"`
	Trips     int     `json:"trips"`
}

type statusTriageView struct {
	AwaitingConfirmation statusAwaitingConfirmationView `json:"awaitingConfirmation"`
}

type statusAwaitingConfirmationView struct {
	Count   int                                    `json:"count"`
	Sources []statusAwaitingConfirmationSourceView `json:"sources"`
}

type statusAwaitingConfirmationSourceView struct {
	Repo        string `json:"repo"`
	IssueNumber int64  `json:"issueNumber"`
	AgeSeconds  int64  `json:"ageSeconds"`
	// Command is the comment that confirms this source, token included.
	Command string `json:"command"`
}

type statusBinaryView struct {
	Identity statusBinaryIdentityView `json:"identity"`
}

type statusBinaryIdentityView struct {
	Swapped bool   `json:"swapped"`
	Reason  string `json:"reason"`
}

type statusRecoveryView struct {
	Outstanding statusOutstandingView `json:"outstanding"`
}

type statusOutstandingView struct {
	QuarantinedActiveExecutions int                         `json:"quarantinedActiveExecutions"`
	QuarantinedRunningRuns      int                         `json:"quarantinedRunningRuns"`
	Loops                       []statusQuarantinedLoopView `json:"loops"`
}

type statusQuarantinedLoopView struct {
	Seq           int64  `json:"seq"`
	Type          string `json:"type"`
	Target        string `json:"target"`
	Status        string `json:"status"`
	QuarantinedAt string `json:"quarantinedAt"`
}

type statusToolsView struct {
	LooperPath    string                   `json:"looperPath"`
	ReviewPublish *statusReviewPublishView `json:"reviewPublish"`
}

type statusReviewPublishView struct {
	Known              bool   `json:"known"`
	Capable            bool   `json:"capable"`
	Capability         string `json:"capability"`
	PublishingDisabled bool   `json:"publishingDisabled"`
	Reason             string `json:"reason"`
}

// statusResourceGuardView is the resource-guard slice of GET /api/v1/status.
// The daemon publishes the structured admission decision so a deliberately idle
// scheduler is distinguishable from an empty queue; `looper status` prints the
// hold so an operator sees why no new work is starting.
type statusResourceGuardView struct {
	Enabled bool     `json:"enabled"`
	Admit   bool     `json:"admit"`
	Reasons []string `json:"reasons"`
	Detail  string   `json:"detail"`
}

func writeStatusOpsLines(stdout io.Writer, status daemonStatusResponse) {
	review := status.Tools.ReviewPublish
	if review != nil {
		switch {
		case !review.Known:
			reason := strings.TrimSpace(review.Reason)
			if reason == "" {
				reason = "capability has not been probed yet"
			}
			path := strings.TrimSpace(status.Tools.LooperPath)
			if path != "" {
				_, _ = fmt.Fprintf(stdout, "review:   publish readiness unknown (%s; looperPath=%s)\n", singleLine(reason), path)
			} else {
				_, _ = fmt.Fprintf(stdout, "review:   publish readiness unknown (%s)\n", singleLine(reason))
			}
		case review.PublishingDisabled:
			reason := strings.TrimSpace(review.Reason)
			if reason == "" {
				reason = "capability probe failed"
			}
			path := strings.TrimSpace(status.Tools.LooperPath)
			if path != "" {
				_, _ = fmt.Fprintf(stdout, "review:   publishing disabled (%s; looperPath=%s)\n", singleLine(reason), path)
			} else {
				_, _ = fmt.Fprintf(stdout, "review:   publishing disabled (%s)\n", singleLine(reason))
			}
		case review.Capable:
			token := strings.TrimSpace(review.Capability)
			if token == "" {
				token = "ok"
			}
			_, _ = fmt.Fprintf(stdout, "review:   publish ready (%s)\n", token)
		default:
			reason := strings.TrimSpace(review.Reason)
			if reason == "" {
				reason = "capability reported not capable"
			}
			_, _ = fmt.Fprintf(stdout, "review:   publish not ready (%s)\n", singleLine(reason))
		}
	}

	// A replaced binary is invisible in every other line: the daemon is healthy,
	// on the old image, and will switch builds at the next restart (#154).
	if identity := status.Service.Binary.Identity; identity.Swapped {
		reason := strings.TrimSpace(identity.Reason)
		if reason == "" {
			reason = "on-disk looperd no longer matches the running image"
		}
		_, _ = fmt.Fprintf(stdout, "binary:   %s\n", singleLine(reason))
	}

	outstanding := status.Service.Recovery.Outstanding
	if outstanding.QuarantinedActiveExecutions > 0 || outstanding.QuarantinedRunningRuns > 0 {
		_, _ = fmt.Fprintf(stdout, "orphans:  quarantinedActiveExecutions=%d quarantinedRunningRuns=%d\n",
			outstanding.QuarantinedActiveExecutions, outstanding.QuarantinedRunningRuns)
		for _, loop := range outstanding.Loops {
			_, _ = fmt.Fprintf(stdout, "  - %s\n", quarantinedLoopLine(loop))
		}
	}
	awaiting := status.Service.Triage.AwaitingConfirmation
	if awaiting.Count > 0 {
		_, _ = fmt.Fprintf(stdout, "triage:  awaitingHumanConfirmation=%d\n", awaiting.Count)
		for _, source := range awaiting.Sources {
			_, _ = fmt.Fprintf(stdout, "  - %s\n", awaitingConfirmationLine(source))
		}
	}
	// An idle daemon that is waiting out a bad provider looks exactly like a
	// stuck one. Say which it is, and when it will try again.
	if health := status.Service.AgentHealth; health != nil {
		_, _ = fmt.Fprintf(stdout, "agents:   %s\n", agentHealthLine(*health))
	}
	if len(status.Service.DegradedReasons) > 0 {
		_, _ = fmt.Fprintf(stdout, "degraded: %s\n", strings.Join(status.Service.DegradedReasons, ", "))
	}

	// A resource-guard hold explains why the scheduler is deliberately idle:
	// under low disk or high load the daemon withholds slots, and without this
	// line a guarded daemon and an idle one look identical from `looper status`.
	// Only a holding guard prints; an admitting or disabled one is not an
	// anomaly worth a line.
	if guard := status.ResourceGuard; guard.Enabled && !guard.Admit {
		detail := strings.TrimSpace(guard.Detail)
		if detail == "" {
			detail = strings.Join(guard.Reasons, ", ")
		}
		if detail == "" {
			detail = "host pressure"
		}
		_, _ = fmt.Fprintf(stdout, "resources: scheduler held (%s)\n", singleLine(detail))
	}
}

func agentHealthLine(health statusAgentHealthView) string {
	stickyProviderPaused := health.State == "closed"
	for _, provider := range health.Providers {
		if provider.State != "closed" {
			stickyProviderPaused = true
			break
		}
	}
	if health.Partial || stickyProviderPaused {
		if len(health.Providers) == 0 {
			return "partially paused: one or more agent providers are unavailable"
		}
		parts := make([]string, 0, len(health.Providers))
		for _, provider := range health.Providers {
			line := fmt.Sprintf("%s=%s", provider.Provider, provider.State)
			if provider.State == "open" && provider.OpenUntil != nil && strings.TrimSpace(*provider.OpenUntil) != "" {
				line += fmt.Sprintf(" (retrying at %s)", strings.TrimSpace(*provider.OpenUntil))
			}
			parts = append(parts, line)
		}
		return "partially paused: " + strings.Join(parts, ", ")
	}
	switch health.State {
	case "open":
		line := fmt.Sprintf("work paused: %d of %d recent agent runs failed", health.Failures, health.Total)
		if health.OpenUntil != nil && strings.TrimSpace(*health.OpenUntil) != "" {
			line += fmt.Sprintf("; retrying at %s", strings.TrimSpace(*health.OpenUntil))
		}
		if health.Trips > 1 {
			line += fmt.Sprintf(" (%d trips)", health.Trips)
		}
		return line
	case "half_open":
		return "probing: recovery checks must succeed before work resumes"
	default:
		return health.State
	}
}

// awaitingConfirmationLine renders one parked triage source and the single
// comment that releases it. The command carries a per-report token that exists
// nowhere else an operator reads, so a line without it names a problem the
// reader still cannot act on (#255). A source the daemon reported without one
// is printed without the trailing clause rather than with an unusable command.
func awaitingConfirmationLine(source statusAwaitingConfirmationSourceView) string {
	line := fmt.Sprintf("%s#%d  waiting %s", source.Repo, source.IssueNumber, statusAge(source.AgeSeconds))
	if command := strings.TrimSpace(source.Command); command != "" {
		line += fmt.Sprintf("  ->  comment %q on the issue", command)
	}
	return line
}

func statusAge(seconds int64) string {
	if seconds < 0 {
		seconds = 0
	}
	if seconds < 60 {
		return fmt.Sprintf("%ds", seconds)
	}
	minutes := seconds / 60
	if minutes < 60 {
		return fmt.Sprintf("%dm", minutes)
	}
	hours := minutes / 60
	if hours < 48 {
		return fmt.Sprintf("%dh", hours)
	}
	return fmt.Sprintf("%dd", hours/24)
}

// quarantinedLoopLine renders one outstanding quarantined loop and the single
// command that clears it. Columns the daemon did not report are dropped rather
// than printed empty.
func quarantinedLoopLine(loop statusQuarantinedLoopView) string {
	columns := []string{fmt.Sprintf("loop %d", loop.Seq)}
	for _, column := range []string{loop.Type, loop.Target, loop.Status} {
		if strings.TrimSpace(column) != "" {
			columns = append(columns, column)
		}
	}
	if strings.TrimSpace(loop.QuarantinedAt) != "" {
		columns = append(columns, "quarantined "+loop.QuarantinedAt)
	}
	columns = append(columns, "->", fmt.Sprintf("looper retry %d", loop.Seq))
	return strings.Join(columns, "  ")
}

type projectResponse struct {
	ID              string             `json:"id"`
	Name            string             `json:"name"`
	RepoPath        string             `json:"repoPath"`
	BaseBranch      string             `json:"baseBranch"`
	Archived        bool               `json:"archived"`
	Repo            *string            `json:"repo"`
	GatekeeperTrust string             `json:"gatekeeperTrust"`
	Discovery       *discoveryResponse `json:"discovery"`
}

type projectsListResponse struct {
	Items []projectResponse `json:"items"`
}

type createProjectResponse struct {
	projectResponse
	Discovery *discoveryResponse `json:"discovery"`
	Warnings  []string           `json:"warnings"`
}

type discoveryResponse struct {
	Status                 string   `json:"status"`
	Error                  string   `json:"error"`
	DiscoveredPullRequests int      `json:"discoveredPullRequests"`
	DiscoveredWorktrees    int      `json:"discoveredWorktrees"`
	Warnings               []string `json:"warnings"`
}

type gatekeeperAgreementsResponse struct {
	Items []gatekeeperAgreement `json:"items"`
}

type gatekeeperVerdictsResponse struct {
	Items []gatekeeperVerdict `json:"items"`
}

type gatekeeperAgreement struct {
	Repo           string `json:"repo"`
	ProjectID      string `json:"projectId"`
	PRNumber       int64  `json:"prNumber"`
	VerdictEventID string `json:"verdictEventId"`
	Outcome        string `json:"outcome"`
	Agreement      bool   `json:"agreement"`
	RecordedAt     string `json:"recordedAt"`
}

type gatekeeperVerdictReason struct {
	Code    string `json:"code"`
	Subject string `json:"subject"`
}

type gatekeeperVerdict struct {
	Repo            string                    `json:"repo"`
	ProjectID       string                    `json:"projectId"`
	PRNumber        int64                     `json:"prNumber"`
	Status          string                    `json:"status"`
	Eligible        bool                      `json:"eligible"`
	ObservedHeadSHA string                    `json:"observedHeadSha"`
	Reasons         []gatekeeperVerdictReason `json:"reasons"`
	EvaluatedAt     string                    `json:"evaluatedAt"`
}

// daemonBaseURL is where this CLI expects the daemon to answer.
//
// server.baseUrl wins when set: it is the operator's statement of the address
// the daemon is actually reachable at, which is not derivable from the bind
// address once a reverse proxy, a tunnel, or TLS is in front of it. Only when
// it is unset is the bind host used, and then a wildcard bind is mapped to
// loopback — dialing 0.0.0.0 or :: reaches nothing, and it is also the Host
// the daemon's own guard expects.
func daemonBaseURL(cfg config.Config) string {
	if cfg.Server.BaseURL != nil {
		if base := strings.TrimSpace(*cfg.Server.BaseURL); base != "" {
			return strings.TrimRight(base, "/")
		}
	}
	// JoinHostPort brackets IPv6 literals, so hosts like ::1 stay dialable.
	return "http://" + net.JoinHostPort(dialHost(cfg.Server.Host), strconv.Itoa(cfg.Server.Port))
}

func dialHost(host string) string {
	switch trimmed := strings.TrimSpace(host); trimmed {
	case "", "0.0.0.0", "::", "[::]":
		return "127.0.0.1"
	default:
		return trimmed
	}
}

// post issues a control verb's request and returns the daemon's raw body, which
// those verbs echo verbatim rather than reformat.
func post(ctx context.Context, cfg config.Config, call apiRequest) (string, error) {
	timeout := requestTimeout
	if call.LongRunning {
		timeout = bulkStopRequestTimeout
	}
	return doHTTPWithin(ctx, timeout, cfg, http.MethodPost, call.Path, call.Body)
}

func get(ctx context.Context, cfg config.Config, path string) (string, error) {
	return doHTTP(ctx, cfg, http.MethodGet, path, nil)
}

// requestJSON performs a request and unwraps the daemon's success envelope, so
// callers work with the payload the API documents rather than with transport
// shape. The control verbs stop at doHTTP instead, because they echo the raw
// body rather than reading fields out of it.
func requestJSON[T any](ctx context.Context, cfg config.Config, method string, path string, body []byte) (T, error) {
	return requestJSONWithin[T](ctx, requestTimeout, cfg, method, path, body)
}

// requestJSONWithin is requestJSON for a call whose cost is not bounded by the
// control-verb deadline. Explicit project discovery may enumerate worktrees,
// pull requests, and snapshots, so it gets a separate long deadline while the
// parent context still carries operator cancellation.
func requestJSONWithin[T any](ctx context.Context, timeout time.Duration, cfg config.Config, method string, path string, body []byte) (T, error) {
	var value T
	payload, err := doHTTPWithin(ctx, timeout, cfg, method, path, body)
	if err != nil {
		return value, err
	}
	envelope := struct {
		Data json.RawMessage `json:"data"`
	}{}
	if err := json.Unmarshal([]byte(payload), &envelope); err != nil {
		return value, fmt.Errorf("decode response from %s: %w", path, err)
	}
	if len(envelope.Data) == 0 {
		return value, fmt.Errorf("response from %s carried no data", path)
	}
	if err := json.Unmarshal(envelope.Data, &value); err != nil {
		return value, fmt.Errorf("decode response from %s: %w", path, err)
	}
	return value, nil
}

func doHTTP(ctx context.Context, cfg config.Config, method, path string, body []byte) (string, error) {
	return doHTTPWithin(ctx, requestTimeout, cfg, method, path, body)
}

func doHTTPWithin(ctx context.Context, timeout time.Duration, cfg config.Config, method, path string, body []byte) (string, error) {
	endpoint := daemonBaseURL(cfg) + path

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return "", err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	// Only local-token auth mode has a token the daemon will accept. Sending
	// one whenever server.localToken happens to be set leaks it to whatever is
	// listening — including a proxy in front of a daemon that never asked for
	// it.
	if cfg.Server.AuthMode == config.AuthModeLocalToken && cfg.Server.LocalToken != nil && strings.TrimSpace(*cfg.Server.LocalToken) != "" {
		request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(*cfg.Server.LocalToken))
	}

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("contact looperd at %s: %w (is the daemon running?)", endpoint, err)
	}
	defer func() { _ = response.Body.Close() }()

	payload, err := io.ReadAll(response.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		code, message, retrySafe := apiErrorEnvelope(payload)
		return "", &daemonError{Status: response.Status, StatusCode: response.StatusCode, Code: code, Message: message, RetrySafe: retrySafe}
	}
	return string(payload), nil
}

// stopAllPartialFailure inspects a successful stop-all envelope. The daemon
// keeps walking remaining loops and returns 200 with counters; a nonzero
// failed or pausedOnly count is still an operator-visible failure.
func stopAllPartialFailure(payload string) error {
	failed, pausedOnly, err := stopAllResultCounts([]byte(payload))
	if err != nil {
		return err
	}
	if failed > 0 {
		return fmt.Errorf("failed to stop %d running task(s)", failed)
	}
	if pausedOnly > 0 {
		return fmt.Errorf("paused %d task(s) without signaling a verified process", pausedOnly)
	}
	return nil
}

func stopAllResultCounts(payload []byte) (failed, pausedOnly int, err error) {
	// Prefer the success envelope the daemon actually returns (`data.summary`);
	// fall back to a bare summary object for fixtures that skip the envelope.
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(payload, &probe); err != nil {
		return 0, 0, fmt.Errorf("decode stop-all response: %w", err)
	}
	type summary struct {
		Failed     int `json:"failed"`
		PausedOnly int `json:"pausedOnly"`
	}
	if raw, ok := probe["data"]; ok {
		var data struct {
			Summary summary `json:"summary"`
		}
		if err := json.Unmarshal(raw, &data); err != nil {
			return 0, 0, fmt.Errorf("decode stop-all data: %w", err)
		}
		return data.Summary.Failed, data.Summary.PausedOnly, nil
	}
	if raw, ok := probe["summary"]; ok {
		var s summary
		if err := json.Unmarshal(raw, &s); err != nil {
			return 0, 0, fmt.Errorf("decode stop-all summary: %w", err)
		}
		return s.Failed, s.PausedOnly, nil
	}
	return 0, 0, fmt.Errorf("decode stop-all response: missing summary")
}

// apiErrorEnvelope returns the daemon's typed error code alongside its message.
// The code is what callers must branch on: the message is prose that varies by
// cause, and the status alone cannot distinguish "this route does not exist on
// an older daemon" from "this daemon answered the route with a 404 of its own".
func apiErrorEnvelope(payload []byte) (code, message string, retrySafe bool) {
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Details struct {
				RetrySafe bool `json:"retrySafe"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &envelope); err == nil {
		if trimmed := strings.TrimSpace(envelope.Error.Message); trimmed != "" {
			return strings.TrimSpace(envelope.Error.Code), trimmed, envelope.Error.Details.RetrySafe
		}
	}
	return "", strings.TrimSpace(string(payload)), false
}

// daemonError is a non-2xx answer from looperd, carrying the typed error code
// so a caller can tell one 404 from another without reading prose.
type daemonError struct {
	Status     string
	StatusCode int
	Code       string
	Message    string
	RetrySafe  bool
}

func (e *daemonError) Error() string {
	return fmt.Sprintf("looperd returned %s: %s", e.Status, e.Message)
}

// singleLine keeps multi-line validation errors from breaking the aligned
// status report into unreadable fragments.
func singleLine(text string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(text, "\n", " ")), " ")
}

func usage(w io.Writer) {
	_, _ = fmt.Fprint(w, `looper - control a running looperd

Usage:
  looper init                  Write a starter config file (never overwrites)
  looper status                Report config, daemon reachability, and projects
  looper dashboard             Print a dashboard URL authenticated for this session
  looper gatekeeper agreements [project-id]
                               Inspect immutable advise agreement outcomes
  looper gatekeeper verdicts [project-id]
                               Inspect the newest Gatekeeper verdict per PR
  looper gatekeeper promote <project-id> <advise|auto>
                               Explicitly promote an API-managed project
  looper project add <path>    Register a git repository root with the daemon
  looper project list          List registered projects
  looper project discover <id> Retry post-commit worktree/PR discovery for a project
  looper stop <selector>       Stop the active run for a loop ("all" stops every run)
  looper close <selector>      Stop the active run and close the loop
  looper takeover <selector>   Take a loop over for manual work
  looper handback <selector>   Hand a taken-over loop back to the daemon
  looper retry <selector>      Requeue a paused or parked loop
                               (refuses dirty managed worktrees unless
                               --discard-worktree-changes --confirm)
  looper start <selector>      Start a loop now
  looper pause <selector>      Pause a loop
  looper respond <selector> "<answer>"
                               Answer a loop waiting on a human and resume it
  looper version               Print the looper version
  looper version --json        Print the complete local build identity
  looper version --check-daemon
                               Compare local and running-daemon build identity
  looper upgrade preflight --target-looper <path> --target-looperd <path>
                               Report upgrade compatibility without changing the daemon
  looper upgrade backup / verify / drain / stage-release / activate-release / verify-start
                               Controlled cutover steps (see docs/installation.md)

Global flags, accepted before or after the verb:
  --config <path>              Config file to load
  --host <host>                Daemon host, overriding the config
  --port <port>                Daemon port, overriding the config

Retry flags (after the selector):
  --discard-worktree-changes   Drop uncommitted worktree edits before retry
  --confirm                    Required with --discard-worktree-changes

There is one more command, which is not for operators:
  looper review submit <repo>#<number> --event <event> --commit-id <sha>
				               Publish a reviewer agent's pull request review.
				               Reviewer agents reach it through a wrapper the
				               daemon writes; direct execution is refused.

A selector is a loop sequence number (looper stop 12) or a loop id
(looper stop loop_1cf3); those are what the daemon resolves.
The respond answer is a single argument, so quote anything with spaces.
Every verb except init and version talks to looperd over HTTP and needs it
running.
`)
}
