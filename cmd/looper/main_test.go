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
			name:     "close resolves a selector",
			verb:     "close",
			args:     []string{"https://github.com/o/r/pull/7"},
			wantPath: "/api/v1/runs/active/https://github.com/o/r/pull/7/close",
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
					t.Fatalf("expected error containing %q, got path %q", tc.wantErr, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %q, want it to contain %q", err.Error(), tc.wantErr)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.wantPath {
				t.Errorf("path = %q, want %q", got, tc.wantPath)
			}
		})
	}
}
