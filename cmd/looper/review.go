package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/forge"
	"github.com/nexu-io/looper/internal/reviewsubmit"
)

// runReview serves the review verbs in this process. `capability` answers the
// daemon's probe and must stay free of config loading, network access, and any
// other side effect: it runs against a binary the daemon has not yet decided to
// trust.
func runReview(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	rest := args[1:]
	switch rest[0] {
	case "capability":
		_, _ = fmt.Fprintln(stdout, forge.TrustedReviewCapabilityToken)
		return 0
	case "submit":
		return runReviewSubmit(ctx, rest[1:], stdin, stdout, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "looper: unknown review subcommand %q\n", rest[0])
		return 2
	}
}

func runReviewSubmit(ctx context.Context, submitArgs []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags, err := parseReviewSubmitFlags(submitArgs)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "looper: %v\n", err)
		return 2
	}

	payload, err := io.ReadAll(stdin)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "looper: read review payload from stdin: %v\n", err)
		return 1
	}
	cwd, err := os.Getwd()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "looper: determine current working directory: %v\n", err)
		return 1
	}

	// When a daemon-side trusted review proxy is configured, forward the full
	// invocation there so provider tokens stay out of the agent process and out
	// of any agent-visible wrapper path. The proxy child clears the socket env
	// and re-enters this command with tokens injected.
	if forge.TrustedReviewSockConfigured() {
		if err := forge.ProxyReviewSubmit(os.Args[1:], payload, cwd); err != nil {
			// The proxied child already streamed its own output; surface its
			// exit status verbatim so the agent sees the real failure mode
			// rather than a uniform 1.
			var exit interface{ ExitCode() int }
			if errors.As(err, &exit) {
				return exit.ExitCode()
			}
			_, _ = fmt.Fprintf(stderr, "looper: %v\n", err)
			return 1
		}
		return 0
	}

	repo, prNumber, err := reviewsubmit.ParsePullRequestRef(flags.ref)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "looper: %v\n", err)
		return 2
	}

	cfg, err := loadReviewSubmitConfig()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "looper: %v\n", err)
		return 1
	}

	request := reviewsubmit.Request{
		Repo:                repo,
		PRNumber:            prNumber,
		Event:               flags.event,
		CommitID:            flags.commitID,
		CleanReviewEvent:    flags.cleanReviewEvent,
		BlockingReviewEvent: flags.blockingReviewEvent,
		ReviewerManual:      flags.reviewerManual,
		ReviewerRunID:       flags.reviewerRunID,
		Payload:             payload,
		CWD:                 cwd,
	}
	if err := reviewsubmit.Submit(ctx, cfg, request, stdout, stderr); err != nil {
		_, _ = fmt.Fprintf(stderr, "looper: %v\n", err)
		return 1
	}
	return 0
}

// loadReviewSubmitConfig resolves the configuration review submit publishes
// under. A daemon-spawned trusted child receives the daemon's already
// materialized snapshot over an inherited descriptor and must use it verbatim:
// the file, environment and CLI layers were resolved when the daemon captured
// the run, and re-reading them here would let the agent's own environment
// rewrite provider or review policy. The descriptor is one-shot, so this must
// be the only config load in such a child.
//
// The fallback deliberately passes no argv. Review submit accepts none of the
// config-selecting flags — the trusted proxy blocks --config outright, so a
// child can never carry one — and config.LoadFile rejects any argument it does
// not recognize, which would make every review-submit flag a load failure.
func loadReviewSubmitConfig() (config.Config, error) {
	if loaded, configured, err := forge.LoadTrustedReviewConfigSnapshot(); configured {
		if err != nil {
			return config.Config{}, err
		}
		return loaded.Config, nil
	}
	return loadConfig(nil)
}

type reviewSubmitFlags struct {
	ref                 string
	event               string
	commitID            string
	cleanReviewEvent    string
	blockingReviewEvent string
	reviewerRunID       string
	reviewerManual      bool
}

// parseReviewSubmitFlags accepts exactly the flags applyTrustedReviewProxyPolicy
// injects plus the pull request target. Unknown flags are rejected rather than
// ignored: the proxy strips agent-supplied policy overrides and rebinds them,
// so anything else arriving here means the two sides have drifted.
func parseReviewSubmitFlags(args []string) (reviewSubmitFlags, error) {
	parsed := reviewSubmitFlags{}
	targets := map[string]*string{
		"event":                 &parsed.event,
		"commit-id":             &parsed.commitID,
		"clean-review-event":    &parsed.cleanReviewEvent,
		"blocking-review-event": &parsed.blockingReviewEvent,
		"reviewer-run-id":       &parsed.reviewerRunID,
	}

	for index := 0; index < len(args); index++ {
		arg := args[index]
		if !strings.HasPrefix(arg, "-") {
			if parsed.ref != "" {
				return reviewSubmitFlags{}, fmt.Errorf("review submit accepts one pull request target, got %q and %q", parsed.ref, arg)
			}
			parsed.ref = arg
			continue
		}
		name, value, hasValue := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if name == "reviewer-manual" {
			if !hasValue {
				parsed.reviewerManual = true
				continue
			}
			manual, err := strconv.ParseBool(value)
			if err != nil {
				return reviewSubmitFlags{}, fmt.Errorf("invalid value for --reviewer-manual: %q is not a boolean", value)
			}
			parsed.reviewerManual = manual
			continue
		}
		target, ok := targets[name]
		if !ok {
			return reviewSubmitFlags{}, fmt.Errorf("unknown review submit flag %q", arg)
		}
		if !hasValue {
			if index+1 >= len(args) {
				return reviewSubmitFlags{}, fmt.Errorf("missing value for --%s", name)
			}
			index++
			value = args[index]
		}
		*target = value
	}

	if strings.TrimSpace(parsed.ref) == "" {
		return reviewSubmitFlags{}, fmt.Errorf("review submit requires a pull request target <repo>#<number>")
	}
	return parsed, nil
}
