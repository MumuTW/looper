package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/MumuTW/looper/internal/config"
	pkgapi "github.com/MumuTW/looper/pkg/api"
)

// runRetry is the dirty-worktree-safe retry path. A bodyless POST would requeue
// immediately and let unreviewed local edits ride into the next agent run; the
// dashboard already gates on GET /worktree, and so must this CLI.
//
// argv is the full command line (including global flags). We split it here so
// --discard-worktree-changes is not rejected as an unknown global flag.
func runRetry(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	parsed, err := splitRetryArgs(args)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "looper: %v\n", err)
		usage(stderr)
		return 2
	}
	opts := parsed.Retry

	cfg, err := loadConfig(parsed.Global)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "looper: %v\n", err)
		return 1
	}

	discard := opts.DiscardWorktreeChanges
	if discard && !opts.Confirm {
		_, _ = fmt.Fprintf(stderr, "looper: --discard-worktree-changes requires --confirm\n")
		return 2
	}

	if !discard {
		status, statusErr := fetchLoopWorktreeStatus(ctx, cfg, opts.Selector)
		if statusErr != nil {
			// Older daemons without /worktree: keep plain retry, matching the
			// dashboard fallback when the route is missing.
			if !isWorktreeRouteMissing(statusErr) && !isRetrySafeWorktreePreflight(statusErr) {
				_, _ = fmt.Fprintf(stderr, "looper: %v\n", statusErr)
				return 1
			}
		} else {
			switch classifyRetryWorktree(status) {
			case retryWorktreeOK:
				// clean or no worktree — proceed.
			case retryWorktreeOfferDiscard:
				_, _ = fmt.Fprintf(stderr, "looper: %v\n", dirtyWorktreeGuidance(opts.Selector, status, true))
				return 1
			case retryWorktreeInspectOnly:
				_, _ = fmt.Fprintf(stderr, "looper: %v\n", dirtyWorktreeGuidance(opts.Selector, status, false))
				return 1
			}
		}
	}

	segment, err := selectorPathSegment(opts.Selector)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "looper: %v\n", err)
		return 2
	}
	body := map[string]any{"mode": "auto", "resetAttempts": true}
	if discard {
		body["discardWorktreeChanges"] = true
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "looper: encode retry body: %v\n", err)
		return 1
	}

	response, err := post(ctx, cfg, apiRequest{
		Path: "/api/v1/loops/" + segment + "/retry",
		Body: encoded,
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "looper: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintln(stdout, strings.TrimSpace(response))
	return 0
}

type retryOptions struct {
	Selector               string
	DiscardWorktreeChanges bool
	Confirm                bool
}

type retryParsed struct {
	Global []string
	Retry  retryOptions
}

// splitRetryArgs separates global flags from the retry verb and its own flags.
// Global flags may appear before or after `retry`, matching the rest of the CLI.
func splitRetryArgs(args []string) (retryParsed, error) {
	var out retryParsed
	var positionals []string
	seenVerb := false

	for i := 0; i < len(args); i++ {
		arg := args[i]

		if arg == "--" {
			positionals = append(positionals, args[i+1:]...)
			break
		}

		name, inlineValue, hasInline := strings.Cut(arg, "=")
		if _, global := globalFlags[name]; global {
			if hasInline {
				out.Global = append(out.Global, name+"="+inlineValue)
				continue
			}
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
				return retryParsed{}, fmt.Errorf("missing value for %s", name)
			}
			out.Global = append(out.Global, name, args[i+1])
			i++
			continue
		}

		if !seenVerb {
			if arg == "retry" {
				seenVerb = true
				continue
			}
			if strings.HasPrefix(arg, "-") && arg != "-" {
				return retryParsed{}, fmt.Errorf("unknown flag %q", name)
			}
			return retryParsed{}, fmt.Errorf("expected retry, got %q", arg)
		}

		switch arg {
		case "--discard-worktree-changes":
			out.Retry.DiscardWorktreeChanges = true
		case "--confirm":
			out.Retry.Confirm = true
		default:
			if strings.HasPrefix(arg, "-") && arg != "-" {
				return retryParsed{}, fmt.Errorf("unknown retry flag %q", arg)
			}
			positionals = append(positionals, arg)
		}
	}

	if !seenVerb {
		return retryParsed{}, fmt.Errorf("retry requires a loop id")
	}
	if len(positionals) != 1 || strings.TrimSpace(positionals[0]) == "" {
		return retryParsed{}, fmt.Errorf("retry requires exactly one loop id")
	}
	out.Retry.Selector = strings.TrimSpace(positionals[0])
	return out, nil
}

type loopWorktreeStatus struct {
	LoopID       string  `json:"loopId"`
	Seq          int64   `json:"seq"`
	Present      bool    `json:"present"`
	WorktreePath *string `json:"worktreePath"`
	Branch       *string `json:"branch"`
	Managed      bool    `json:"managed"`
	Clean        *bool   `json:"clean"`
	Dirty        *bool   `json:"dirty"`
	Reason       string  `json:"reason"`
}

type retryWorktreeDecision int

const (
	retryWorktreeOK retryWorktreeDecision = iota
	retryWorktreeOfferDiscard
	retryWorktreeInspectOnly
)

// classifyRetryWorktree matches the dashboard gate: discard is only offered for
// present + managed + dirty. Unmanaged dirty trees are inspect-only.
func classifyRetryWorktree(status loopWorktreeStatus) retryWorktreeDecision {
	if !status.Present {
		return retryWorktreeOK
	}
	if status.Dirty == nil || !*status.Dirty {
		return retryWorktreeOK
	}
	if status.Managed {
		return retryWorktreeOfferDiscard
	}
	return retryWorktreeInspectOnly
}

func fetchLoopWorktreeStatus(ctx context.Context, cfg config.Config, selector string) (loopWorktreeStatus, error) {
	segment, err := selectorPathSegment(selector)
	if err != nil {
		return loopWorktreeStatus{}, err
	}
	raw, err := get(ctx, cfg, "/api/v1/loops/"+segment+"/worktree")
	if err != nil {
		return loopWorktreeStatus{}, err
	}
	var envelope struct {
		Data loopWorktreeStatus `json:"data"`
	}
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		return loopWorktreeStatus{}, fmt.Errorf("decode worktree status: %w", err)
	}
	// Some fixtures may return the object bare.
	if envelope.Data.LoopID == "" && envelope.Data.Seq == 0 {
		var bare loopWorktreeStatus
		if err := json.Unmarshal([]byte(raw), &bare); err == nil && (bare.Present || bare.Reason != "") {
			return bare, nil
		}
	}
	return envelope.Data, nil
}

func dirtyWorktreeGuidance(selector string, status loopWorktreeStatus, offerDiscard bool) error {
	path := ""
	if status.WorktreePath != nil {
		path = strings.TrimSpace(*status.WorktreePath)
	}
	if offerDiscard {
		if path != "" {
			return fmt.Errorf("worktree is dirty for loop %s at %s; inspect it, or re-run with --discard-worktree-changes --confirm", selector, path)
		}
		return fmt.Errorf("worktree is dirty for loop %s; inspect it, or re-run with --discard-worktree-changes --confirm", selector)
	}
	if path != "" {
		return fmt.Errorf("worktree is dirty and not Looper-managed for loop %s at %s; inspect and clean it before retrying", selector, path)
	}
	return fmt.Errorf("worktree is dirty and not Looper-managed for loop %s; inspect and clean it before retrying", selector)
}

// isWorktreeRouteMissing reports the one case that may skip the dirty-worktree
// gate: a daemon too old to serve /worktree at all. It matches on the daemon's
// typed error code — a stronger signal than the dashboard's, whose second
// branch still substring-matches and is fixed separately.
//
// Matching the error text instead would silently disarm the gate this preflight
// exists to enforce. Every 404 carries "not found" — including the
// PROJECT_NOT_FOUND the route itself returns for an archived project — and any
// message that merely contains "404" would qualify, which a managed worktree
// path for PR #404 does.
func isWorktreeRouteMissing(err error) bool {
	var daemonErr *daemonError
	if !errors.As(err, &daemonErr) {
		return false
	}
	// The code is read out of a response body, which anything between this CLI
	// and the daemon can author; the status line is the transport's own. Both
	// must agree before the gate stands down.
	return daemonErr.StatusCode == http.StatusNotFound &&
		daemonErr.Code == string(pkgapi.ErrorCodeRouteNotFound)
}

// isRetrySafeWorktreePreflight permits only the daemon's explicit statement
// that a failed status check blocks discard but not plain retry. The worker
// revalidates the checkpoint checkout before agent execution, so accepting a
// generic 400 here would be unsafe.
func isRetrySafeWorktreePreflight(err error) bool {
	var daemonErr *daemonError
	if !errors.As(err, &daemonErr) {
		return false
	}
	return daemonErr.StatusCode == http.StatusBadRequest &&
		daemonErr.Code == string(pkgapi.ErrorCodeValidationFailed) &&
		daemonErr.RetrySafe
}
