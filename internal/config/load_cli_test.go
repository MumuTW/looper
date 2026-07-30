package config

import "testing"

func TestParseCLIArgsDispatchesInterleavedFlagFamilies(t *testing.T) {
	parsed, err := parseCLIArgs([]string{
		"--host", "127.0.0.2",
		"--reviewer-loop-enabled=true",
		"--port=9191",
		"--roles-reviewer-behavior-loop-enabled-by-default=false",
		"--allow-auto-push", "false",
	})
	if err != nil {
		t.Fatalf("parseCLIArgs() error = %v", err)
	}

	if parsed.overrides.Server == nil || parsed.overrides.Server.Host == nil || *parsed.overrides.Server.Host != "127.0.0.2" {
		t.Fatalf("server host override = %#v, want 127.0.0.2", parsed.overrides.Server)
	}
	if parsed.overrides.Server.Port == nil || *parsed.overrides.Server.Port != 9191 {
		t.Fatalf("server port override = %#v, want 9191", parsed.overrides.Server.Port)
	}
	if parsed.overrides.Defaults == nil || parsed.overrides.Defaults.AllowAutoPush == nil || *parsed.overrides.Defaults.AllowAutoPush {
		t.Fatalf("allow auto push override = %#v, want false", parsed.overrides.Defaults)
	}
	loop := parsed.overrides.Roles.Reviewer.Behavior.Loop
	if loop == nil || loop.EnabledByDefault == nil || *loop.EnabledByDefault {
		t.Fatalf("reviewer loop enabled override = %#v, want canonical false", loop)
	}
}

func TestParseCLIArgsAcceptsDeprecatedAutoUpgradeFlagsAsNoOps(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "legacy bare", args: []string{"--no-auto-upgrade"}},
		{name: "legacy inline", args: []string{"--no-auto-upgrade=false"}},
		{name: "legacy separate", args: []string{"--no-auto-upgrade", "true"}},
		{name: "canonical inline", args: []string{"--package-auto-upgrade-enabled=true"}},
		{name: "canonical separate", args: []string{"--package-auto-upgrade-enabled", "false"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parsed, err := parseCLIArgs(test.args)
			if err != nil {
				t.Fatalf("parseCLIArgs() error = %v", err)
			}
			if parsed.overrides.Package != nil {
				t.Fatalf("deprecated flag populated package override = %#v, want no-op", parsed.overrides.Package)
			}
		})
	}
}

func TestParseCLIArgsCompatibilityPrecedenceAcrossInterleavedFamilies(t *testing.T) {
	canonical := []string{
		"--roles-reviewer-behavior-review-events-clean=COMMENT",
		"--roles-fixer-triggers-author-filter=current_user",
		"--roles-reviewer-behavior-loop-enabled-by-default=false",
		"--roles-reviewer-discovery-triggers-enable-self-review=false",
		"--roles-reviewer-behavior-review-events-blocking=COMMENT",
	}
	legacy := []string{
		"--allow-auto-approve=true",
		"--fix-all-pull-requests=true",
		"--reviewer-loop-enabled=true",
		"--reviewer-enable-self-review=true",
		"--reviewer-blocking-review-event=REQUEST_CHANGES",
	}

	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "canonical before legacy", args: append(append([]string{}, canonical...), legacy...)},
		{name: "legacy before canonical", args: append(append([]string{}, legacy...), canonical...)},
	} {
		t.Run(test.name, func(t *testing.T) {
			parsed, err := parseCLIArgs(test.args)
			if err != nil {
				t.Fatalf("parseCLIArgs() error = %v", err)
			}

			reviewer := parsed.overrides.Roles.Reviewer
			if got := *reviewer.Behavior.ReviewEvents.Clean; got != ReviewerReviewEventComment {
				t.Fatalf("clean review event = %q, want %q", got, ReviewerReviewEventComment)
			}
			if got := *reviewer.Behavior.ReviewEvents.Blocking; got != ReviewerReviewEventComment {
				t.Fatalf("blocking review event = %q, want %q", got, ReviewerReviewEventComment)
			}
			if got := *reviewer.Behavior.Loop.EnabledByDefault; got {
				t.Fatalf("reviewer loop enabled = %v, want false", got)
			}
			if got := *reviewer.Triggers.EnableSelfReview; got {
				t.Fatalf("reviewer self review = %v, want false", got)
			}
			if got := *parsed.overrides.Roles.Fixer.Triggers.AuthorFilter; got != FixerAuthorFilterCurrentUser {
				t.Fatalf("fixer author filter = %q, want %q", got, FixerAuthorFilterCurrentUser)
			}
		})
	}
}
