package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/nexu-io/looper/internal/reviewsubmit"
)

// `looper review submit` is not an operator command and is not in the usage
// text: it is the child half of the daemon's trusted review proxy. The daemon
// writes a wrapper that reviewer agents call, validates the argv shape, rewrites
// the review-event policy flags to the ones it bound at run start, and spawns
// this binary with provider credentials and a config snapshot attached. Removing
// the verb did not remove that machinery — it only made every reviewer run fail
// at publish time.
//
// Its flags therefore belong to the proxy's contract, not to taste. See
// forge.applyTrustedReviewProxyPolicy for the argv it appends and
// reviewer.buildReviewPrompt for the argv it tells agents to type.

// reviewSubmitValueFlags are the flags that take a value; --reviewer-manual is
// the only boolean, and the proxy appends it with no argument.
var reviewSubmitValueFlags = map[string]func(*reviewsubmit.Options, string){
	"--event":                 func(o *reviewsubmit.Options, v string) { o.Event = v },
	"--commit-id":             func(o *reviewsubmit.Options, v string) { o.CommitID = v },
	"--clean-review-event":    func(o *reviewsubmit.Options, v string) { o.CleanReviewEvent = v },
	"--blocking-review-event": func(o *reviewsubmit.Options, v string) { o.BlockingReviewEvent = v },
	"--reviewer-run-id":       func(o *reviewsubmit.Options, v string) { o.ReviewerRunID = v },
}

// runReview executes `looper review submit`. It returns the process exit code.
func runReview(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	opts, err := parseReviewSubmit(args)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "looper: %v\n", err)
		return 2
	}

	cwd, err := os.Getwd()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "looper: determine working directory: %v\n", err)
		return 1
	}
	opts.Argv = append([]string(nil), args...)
	opts.CWD = cwd
	opts.Stdin = stdin
	opts.Stdout = stdout
	opts.Stderr = stderr

	if err := reviewsubmit.Run(ctx, opts); err != nil {
		_, _ = fmt.Fprintf(stderr, "looper: %v\n", err)
		return 1
	}
	return 0
}

// parseReviewSubmit maps `review submit <pr> [flags]` onto the submission
// options.
//
// Flags are accepted in any position, including after the pull request target,
// because that is where the proxy appends the policy flags it rewrote. Unknown
// flags are refused rather than skipped: the proxy's own argv validator skips
// what it does not recognise, so a flag silently ignored here is one that
// passed a security check without anyone acting on it.
func parseReviewSubmit(args []string) (reviewsubmit.Options, error) {
	opts := reviewsubmit.Options{}
	positional := make([]string, 0, 3)

	for index := 0; index < len(args); index++ {
		arg := args[index]
		name, inline, hasInline := strings.Cut(arg, "=")

		if name == "--reviewer-manual" {
			if hasInline {
				return reviewsubmit.Options{}, fmt.Errorf("--reviewer-manual takes no value")
			}
			opts.ReviewerManual = true
			continue
		}
		if assign, known := reviewSubmitValueFlags[name]; known {
			value, next, err := reviewFlagValue(args, index, name, inline, hasInline)
			if err != nil {
				return reviewsubmit.Options{}, err
			}
			assign(&opts, value)
			index = next
			continue
		}
		// --config selects the file this submission is judged against, so it is
		// accepted here as it is elsewhere. The trusted proxy rejects it before
		// spawning us, which is the point: on that path the daemon's snapshot is
		// the only configuration, and an agent must not be able to swap it.
		if name == "--config" {
			value, next, err := reviewFlagValue(args, index, name, inline, hasInline)
			if err != nil {
				return reviewsubmit.Options{}, err
			}
			opts.ConfigArgs = append(opts.ConfigArgs, name, value)
			index = next
			continue
		}
		if strings.HasPrefix(arg, "-") && arg != "-" {
			return reviewsubmit.Options{}, fmt.Errorf("unknown flag %q for review submit", name)
		}
		positional = append(positional, arg)
	}

	if len(positional) < 2 || positional[0] != "review" || positional[1] != "submit" {
		return reviewsubmit.Options{}, fmt.Errorf("the only review command is `looper review submit <repo>#<number>`")
	}
	if len(positional) != 3 {
		return reviewsubmit.Options{}, fmt.Errorf("review submit requires exactly one pull request target <repo>#<number>")
	}
	opts.PRRef = positional[2]
	return opts, nil
}

// reviewFlagValue reads a flag's value in either `--flag=value` or
// `--flag value` form and returns the index the caller should resume from.
func reviewFlagValue(args []string, index int, name, inline string, hasInline bool) (string, int, error) {
	if hasInline {
		return inline, index, nil
	}
	if index+1 >= len(args) || strings.HasPrefix(args[index+1], "-") {
		return "", index, fmt.Errorf("missing value for %s", name)
	}
	return args[index+1], index + 1, nil
}
