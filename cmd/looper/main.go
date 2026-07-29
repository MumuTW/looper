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
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
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

	verb := args[0]
	rest := args[1:]

	route, err := routeForVerb(verb, rest)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "looper: %v\n", err)
		usage(stderr)
		return 2
	}

	cfg, err := loadConfig(args)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "looper: %v\n", err)
		return 1
	}

	body, err := post(ctx, cfg, route)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "looper: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintln(stdout, strings.TrimSpace(body))
	return 0
}

// routeForVerb maps a control verb to its API path. Selectors are passed
// through unvalidated: the daemon resolves them (loop id, PR URL, shorthand)
// and is the only component that can say authoritatively what a selector
// means, so validating here would only invent a second, staler answer.
func routeForVerb(verb string, args []string) (string, error) {
	switch verb {
	case "stop", "close":
		if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
			return "", fmt.Errorf("%s requires exactly one selector", verb)
		}
		// Bulk stop is its own selector on the daemon, not a selector named
		// "all" with a /stop suffix: that path parses as a single-loop stop
		// and dies resolving a loop literally called "all".
		if strings.TrimSpace(args[0]) == "all" && verb == "stop" {
			return "/api/v1/runs/active/stop-all", nil
		}
		return "/api/v1/runs/active/" + args[0] + "/" + verb, nil
	case "takeover":
		if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
			return "", fmt.Errorf("takeover requires exactly one loop id")
		}
		return "/api/v1/loops/" + args[0] + "/takeover", nil
	default:
		return "", fmt.Errorf("unknown command %q", verb)
	}
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

func post(ctx context.Context, cfg config.Config, path string) (string, error) {
	host := strings.TrimSpace(cfg.Server.Host)
	if host == "" {
		host = "127.0.0.1"
	}
	endpoint := "http://" + net.JoinHostPort(host, strconv.Itoa(cfg.Server.Port)) + path

	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(nil))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/json")
	if cfg.Server.LocalToken != nil && strings.TrimSpace(*cfg.Server.LocalToken) != "" {
		request.Header.Set("Authorization", "Bearer "+*cfg.Server.LocalToken)
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
  looper takeover <loop-id>    Take a loop over for manual work
  looper version               Print the looper version

Selectors are resolved by the daemon; a loop id or a pull request URL both work.
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
