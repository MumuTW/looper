package main

import (
	"strings"
	"testing"
)

// TestRouteForVerb pins the whole CLI-to-API contract in one table: every verb
// the usage text advertises, the selector that routes somewhere else entirely
// ("stop all"), and the argument shapes that must fail before a request is
// ever sent.
func TestRouteForVerb(t *testing.T) {
	tests := []struct {
		name     string
		verb     string
		args     []string
		wantPath string
		wantBody string
		wantErr  string
	}{
		{
			name:     "stop resolves a selector against active runs",
			verb:     "stop",
			args:     []string{"loop-1"},
			wantPath: "/api/v1/runs/active/loop-1/stop",
		},
		{
			// The daemon's bulk stop is the selector "stop-all" on a
			// single-segment path. Routing "all" as a selector with a /stop
			// suffix instead lands in the single-loop branch, which 404s on a
			// loop named "all" — so this assertion is the bug guard, not a
			// restatement of the obvious.
			name:     "stop all is the daemon's bulk selector, not a loop named all",
			verb:     "stop",
			args:     []string{"all"},
			wantPath: "/api/v1/runs/active/stop-all",
		},
		{
			// There is no bulk close on the daemon, so this stays an ordinary
			// selector and fails loudly at resolution rather than quietly
			// closing everything.
			name:     "close all stays an ordinary selector",
			verb:     "close",
			args:     []string{"all"},
			wantPath: "/api/v1/runs/active/all/close",
		},
		{
			// See TestPullRequestURLSelectorCannotRoute: the daemon splits the
			// decoded request path, so a selector with a slash cannot reach a
			// route however it is encoded.
			name:    "close refuses a pull request URL the daemon cannot resolve",
			verb:    "close",
			args:    []string{"https://github.com/o/r/pull/7"},
			wantErr: "is not a loop id or sequence number",
		},
		{
			// Escaped rather than concatenated raw: a selector is one path
			// segment, and "#" would otherwise truncate the path to a fragment.
			name:     "close escapes a selector that would break the request path",
			verb:     "close",
			args:     []string{"loop#7"},
			wantPath: "/api/v1/runs/active/loop%237/close",
		},
		{
			name:     "takeover targets the loop",
			verb:     "takeover",
			args:     []string{"loop-1"},
			wantPath: "/api/v1/loops/loop-1/takeover",
		},
		{
			name:     "handback targets the loop",
			verb:     "handback",
			args:     []string{"loop-1"},
			wantPath: "/api/v1/loops/loop-1/handback",
		},
		{
			name:     "retry targets the loop",
			verb:     "retry",
			args:     []string{"loop-1"},
			wantPath: "/api/v1/loops/loop-1/retry",
		},
		{
			name:     "start targets the loop",
			verb:     "start",
			args:     []string{"loop-1"},
			wantPath: "/api/v1/loops/loop-1/start",
		},
		{
			name:     "pause targets the loop",
			verb:     "pause",
			args:     []string{"loop-1"},
			wantPath: "/api/v1/loops/loop-1/pause",
		},
		{
			name:     "respond carries the answer as a json body",
			verb:     "respond",
			args:     []string{"loop-1", "ship it"},
			wantPath: "/api/v1/loops/loop-1/respond",
			wantBody: `{"answer":"ship it"}`,
		},
		{
			name:     "respond escapes an answer containing quotes",
			verb:     "respond",
			args:     []string{"loop-1", `he said "no"`},
			wantPath: "/api/v1/loops/loop-1/respond",
			wantBody: `{"answer":"he said \"no\""}`,
		},
		{
			name:     "respond trims the answer the daemon would trim anyway",
			verb:     "respond",
			args:     []string{"loop-1", "  yes  "},
			wantPath: "/api/v1/loops/loop-1/respond",
			wantBody: `{"answer":"yes"}`,
		},
		{
			name:    "respond rejects a whitespace-only answer before the round trip",
			verb:    "respond",
			args:    []string{"loop-1", "   "},
			wantErr: "non-empty answer",
		},
		{
			name:    "respond rejects an unquoted multi-word answer rather than truncating it",
			verb:    "respond",
			args:    []string{"loop-1", "ship", "it"},
			wantErr: "one quoted answer",
		},
		{
			name:    "respond rejects a missing answer",
			verb:    "respond",
			args:    []string{"loop-1"},
			wantErr: "one quoted answer",
		},
		{
			name:    "stop rejects a missing selector",
			verb:    "stop",
			args:    nil,
			wantErr: "stop requires exactly one selector",
		},
		{
			name:    "close names itself in its arity error",
			verb:    "close",
			args:    []string{"a", "b"},
			wantErr: "close requires exactly one selector",
		},
		{
			name:    "pause names itself in its arity error",
			verb:    "pause",
			args:    []string{},
			wantErr: "pause requires exactly one loop id",
		},
		{
			name:    "takeover rejects a whitespace-only loop id",
			verb:    "takeover",
			args:    []string{"   "},
			wantErr: "takeover requires exactly one loop id",
		},
		{
			name:    "unknown verbs are refused",
			verb:    "resume",
			args:    []string{"loop-1"},
			wantErr: `unknown command "resume"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := routeForVerb(tc.verb, tc.args)

			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got request %+v", tc.wantErr, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %q, want it to contain %q", err.Error(), tc.wantErr)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Path != tc.wantPath {
				t.Errorf("path = %q, want %q", got.Path, tc.wantPath)
			}
			if string(got.Body) != tc.wantBody {
				t.Errorf("body = %q, want %q", string(got.Body), tc.wantBody)
			}
		})
	}
}

// TestRouteForVerbBareVerbsSendNoBody guards the split between the verbs that
// carry a payload and those that do not: handback reaches a daemon path that
// peeks at the body for discardWorktreeChanges, so a stray payload on a bare
// verb would be interpreted rather than ignored.
func TestRouteForVerbBareVerbsSendNoBody(t *testing.T) {
	for _, verb := range []string{"stop", "close", "takeover", "handback", "retry", "start", "pause"} {
		t.Run(verb, func(t *testing.T) {
			got, err := routeForVerb(verb, []string{"loop-1"})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Body != nil {
				t.Errorf("body = %q, want nil", string(got.Body))
			}
		})
	}
}
