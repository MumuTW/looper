// Command looper is the operator CLI for a running looperd.
//
// This is the minimal surface deliberately kept after the full cobra CLI was
// removed: the loop control verbs, which are the only ones the daemon has no
// other client for. Everything else the old CLI did (config editing, project
// and provider management, network and webhook administration) is either the
// dashboard's job or not yet reimplemented.
//
// Control is exercised over the daemon's HTTP API rather than by touching
// SQLite or signalling processes directly, because the daemon owns run
// ownership and containment; a second writer would race it.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/version"
)

const requestTimeout = 30 * time.Second

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	if indexOf(args, "--version") >= 0 || args[0] == "version" {
		_, _ = fmt.Fprintln(stdout, version.Value)
		return 0
	}
	if args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		usage(stdout)
		return 0
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// `review` owns its own flag set, and the daemon's trusted review proxy
	// composes that argv. Routing it before splitGlobalFlags is what keeps
	// --event and --commit-id from being rejected as unknown flags.
	if verb, ok := leadingVerb(args); ok && verb == "review" {
		return runReview(ctx, args, stdin, stdout, stderr)
	}

	// retry owns --discard-worktree-changes / --confirm and a GET preflight.
	// Those flags must not go through splitGlobalFlags (which rejects any
	// non-global dash-argument) and must not collapse to a bodyless POST.
	if verb, ok := leadingVerb(args); ok && verb == "retry" {
		return runRetry(ctx, args, stdout, stderr)
	}

	parsed, err := splitGlobalFlags(args)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "looper: %v\n", err)
		usage(stderr)
		return 2
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
	if request.Path == "/api/v1/runs/active/stop-all" {
		if err := stopAllPartialFailure(body); err != nil {
			_, _ = fmt.Fprintln(stdout, strings.TrimSpace(body))
			_, _ = fmt.Fprintf(stderr, "looper: %v\n", err)
			return 1
		}
	}
	_, _ = fmt.Fprintln(stdout, strings.TrimSpace(body))
	return 0
}

// apiRequest is the single daemon call one CLI invocation makes. Body is nil
// for the bare control verbs; carrying it here rather than assembling it at
// the call site keeps routeForVerb a pure function, so the entire
// argument-to-request mapping is testable without a running daemon.
type apiRequest struct {
	Path string
	Body []byte
}

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

// leadingVerb returns the first positional argument, skipping global flags and
// the values they consume. It exists so a verb that parses its own flags can be
// routed before splitGlobalFlags rejects them, and it deliberately mirrors the
// tolerance of the trusted review proxy's own argv scan, which also allows
// global flags to precede `review`.
func leadingVerb(args []string) (string, bool) {
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--" {
			if index+1 < len(args) {
				return args[index+1], true
			}
			return "", false
		}
		name, _, hasInline := strings.Cut(arg, "=")
		if _, global := globalFlags[name]; global {
			if !hasInline && index+1 < len(args) && !strings.HasPrefix(args[index+1], "--") {
				index++
			}
			continue
		}
		if strings.HasPrefix(arg, "-") && arg != "-" {
			return "", false
		}
		return arg, true
	}
	return "", false
}

// splitGlobalFlags separates global flags from the verb and its operands so
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
	positional := make([]string, 0, len(args))

	for index := 0; index < len(args); index++ {
		arg := args[index]

		// Everything after `--` is an operand, which is how an answer or a
		// selector that begins with a dash gets through.
		if arg == "--" {
			positional = append(positional, args[index+1:]...)
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

		if strings.HasPrefix(arg, "-") && arg != "-" {
			return parsedArgs{}, fmt.Errorf("unknown flag %q", name)
		}

		positional = append(positional, arg)
	}

	if len(positional) == 0 {
		return parsedArgs{}, fmt.Errorf("a command is required")
	}
	parsed.Verb = positional[0]
	parsed.Operands = positional[1:]
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
		if strings.TrimSpace(args[0]) == "all" && verb == "stop" {
			return apiRequest{Path: "/api/v1/runs/active/stop-all"}, nil
		}
		segment, err := selectorPathSegment(args[0])
		if err != nil {
			return apiRequest{}, err
		}
		return apiRequest{Path: "/api/v1/runs/active/" + segment + "/" + verb}, nil
	case "takeover", "retry", "start", "pause", "handback":
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

func loadConfig(args []string) (config.Config, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return config.Config{}, fmt.Errorf("determine working directory: %w", err)
	}
	loaded, err := config.LoadFile(config.LoadFileOptions{
		CWD:       cwd,
		Args:      append([]string(nil), args...),
		LookupEnv: os.LookupEnv,
	})
	if err != nil {
		return config.Config{}, err
	}
	return loaded.Config, nil
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

func post(ctx context.Context, cfg config.Config, call apiRequest) (string, error) {
	return doHTTP(ctx, cfg, http.MethodPost, call.Path, call.Body)
}

func get(ctx context.Context, cfg config.Config, path string) (string, error) {
	return doHTTP(ctx, cfg, http.MethodGet, path, nil)
}

func doHTTP(ctx context.Context, cfg config.Config, method, path string, body []byte) (string, error) {
	endpoint := daemonBaseURL(cfg) + path

	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
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
		return "", fmt.Errorf("looperd returned %s: %s", response.Status, apiErrorMessage(payload))
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

// apiErrorMessage pulls the daemon's own message out of an error envelope so
// the operator sees "loop not found", not a raw JSON blob.
func apiErrorMessage(payload []byte) string {
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &envelope); err == nil {
		if message := strings.TrimSpace(envelope.Error.Message); message != "" {
			return message
		}
	}
	return strings.TrimSpace(string(payload))
}

func usage(w io.Writer) {
	_, _ = fmt.Fprint(w, `looper - control a running looperd

Usage:
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
                               daemon writes; run directly it has no provider
                               credentials and will fail.

A selector is a loop sequence number (looper stop 12) or a loop id
(looper stop loop_1cf3); those are what the daemon resolves.
The respond answer is a single argument, so quote anything with spaces.
`)
}

func indexOf(args []string, target string) int {
	for i, arg := range args {
		if arg == target {
			return i
		}
	}
	return -1
}
