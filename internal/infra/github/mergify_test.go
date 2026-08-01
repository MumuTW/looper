package github

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/MumuTW/looper/internal/infra/shell"
)

func TestValidateMergifyRoutingRequiresQueueVetoes(t *testing.T) {
	t.Parallel()
	content := `queue_rules:
  - name: default
    queue_conditions:
      - base = main
      - label != needs-human-review
      - label != do-not-merge
merge_protections_settings:
  auto_merge_conditions:
    - label = auto-merge
`
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		if got := strings.Join(options.Args, " "); got != "api repos/acme/looper/contents/.mergify.yml --jq .content -H Accept: application/vnd.github+json" {
			t.Fatalf("gh args = %q", got)
		}
		return shell.Result{Stdout: base64.StdEncoding.EncodeToString([]byte(content))}, nil
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	if err := gateway.ValidateMergifyRouting(context.Background(), ValidateMergifyRoutingInput{Repo: "acme/looper"}); err != nil {
		t.Fatalf("ValidateMergifyRouting() error = %v", err)
	}
}

func TestValidateMergifyRoutingRejectsMissingQueueVeto(t *testing.T) {
	t.Parallel()
	content := `queue_rules:
  - name: default
    queue_conditions:
      - base = main
merge_protections_settings:
  auto_merge_conditions:
    - label = auto-merge
`
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		return shell.Result{Stdout: base64.StdEncoding.EncodeToString([]byte(content))}, nil
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	if err := gateway.ValidateMergifyRouting(context.Background(), ValidateMergifyRoutingInput{Repo: "acme/looper"}); err == nil {
		t.Fatal("ValidateMergifyRouting() error = nil, want missing queue veto to fail closed")
	}
}

func TestValidateMergifyRoutingIgnoresCommentsAndUnrelatedNestedFields(t *testing.T) {
	t.Parallel()
	content := `queue_rules:
  - name: default
    queue_conditions:
      - base = main
      # label != needs-human-review
      - label != do-not-merge
unrelated:
  queue_conditions:
    - label != needs-human-review
merge_protections_settings:
  auto_merge_conditions:
    - label = auto-merge
`
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		return shell.Result{Stdout: base64.StdEncoding.EncodeToString([]byte(content))}, nil
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	if err := gateway.ValidateMergifyRouting(context.Background(), ValidateMergifyRoutingInput{Repo: "acme/looper"}); err == nil {
		t.Fatal("ValidateMergifyRouting() error = nil, want inactive commented/nested veto to fail closed")
	}
}

func TestValidateMergifyRoutingChecksEveryQueueRule(t *testing.T) {
	t.Parallel()
	content := `queue_rules:
  - name: default
    queue_conditions:
      - label != needs-human-review
      - label != do-not-merge
  - name: manual
    queue_conditions:
      - base = main
merge_protections_settings:
  auto_merge_conditions:
    - label = auto-merge
`
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		return shell.Result{Stdout: base64.StdEncoding.EncodeToString([]byte(content))}, nil
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	if err := gateway.ValidateMergifyRouting(context.Background(), ValidateMergifyRoutingInput{Repo: "acme/looper"}); err == nil {
		t.Fatal("ValidateMergifyRouting() error = nil, want every queue rule to carry vetoes")
	}
}
