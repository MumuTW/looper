package main

import "testing"

// TestReviewSubmitArgvFromTrustedProxyRoutes pins the contract between the
// trusted review proxy and this CLI. The daemon mints a proxy that execs
// `<tools.looperPath> review submit ...` with captured provider credentials
// (forge.StartTrustedReviewProxy), and the reviewer prompt tells the agent to
// publish through exactly these command shapes. Both sides are generated from
// daemon-side state, so nothing here is under the agent's control: if the CLI
// cannot route them, every reviewer run on a host with `looper` on PATH fails
// at publication time rather than falling back to the fail-closed path.
//
// The argv below is what the child actually receives: the prompt-emitted flags
// plus the policy flags applyTrustedReviewProxyPolicy appends before spawning.
func TestReviewSubmitArgvFromTrustedProxyRoutes(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{
			// Non-blocking findings: the prompt's actionableReviewSubmitCommand.
			name: "COMMENT review",
			args: []string{
				"review", "submit", "acme/looper#42",
				"--event", "COMMENT",
				"--clean-review-event", "COMMENT",
				"--blocking-review-event", "REQUEST_CHANGES",
				"--commit-id", "head-42",
			},
		},
		{
			// Clean result under an APPROVE clean-review policy.
			name: "APPROVE review",
			args: []string{
				"review", "submit", "acme/looper#42",
				"--event", "APPROVE",
				"--clean-review-event", "APPROVE",
				"--blocking-review-event", "REQUEST_CHANGES",
				"--commit-id", "head-42",
			},
		},
		{
			// Blocking findings.
			name: "REQUEST_CHANGES review",
			args: []string{
				"review", "submit", "acme/looper#42",
				"--event", "REQUEST_CHANGES",
				"--clean-review-event", "COMMENT",
				"--blocking-review-event", "REQUEST_CHANGES",
				"--commit-id", "head-42",
			},
		},
		{
			// Manual reviewer loops: the proxy appends the held-run identity.
			name: "manual reviewer run",
			args: []string{
				"review", "submit", "acme/looper#42",
				"--event", "COMMENT",
				"--clean-review-event", "COMMENT",
				"--blocking-review-event", "REQUEST_CHANGES",
				"--commit-id", "head-42",
				"--reviewer-manual",
				"--reviewer-run-id", "run-7",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := routeForVerb(tt.args[0], tt.args[1:]); err != nil {
				t.Fatalf("routeForVerb(%q, %q) error = %v; the trusted review proxy "+
					"execs this argv against the looper binary, so an unroutable "+
					"shape means the reviewer role cannot publish reviews at all",
					tt.args[0], tt.args[1:], err)
			}
		})
	}
}
