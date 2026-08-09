package github

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/MumuTW/looper/internal/infra/shell"
)

func TestIsMergifyMergeActor(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		login string
		want  bool
	}{
		{login: "mergify", want: true},
		{login: "mergify[bot]", want: true},
		{login: "MERGIFYIO[bot]", want: true},
		{login: "maintainer", want: false},
		{login: "", want: false},
	} {
		t.Run(tc.login, func(t *testing.T) {
			if got := IsMergifyMergeActor(tc.login); got != tc.want {
				t.Fatalf("IsMergifyMergeActor(%q) = %t, want %t", tc.login, got, tc.want)
			}
		})
	}
}

func TestMergifyRoutingContractFingerprintTracksRepositoryContent(t *testing.T) {
	t.Parallel()
	content := []byte("queue_rules:\n  - name: default\n")
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		if got := strings.Join(options.Args, " "); got != "api repos/acme/looper/contents/.mergify.yml --jq .content -H Accept: application/vnd.github+json" {
			t.Fatalf("gh args = %q", got)
		}
		return shell.Result{Stdout: base64.StdEncoding.EncodeToString(content)}, nil
	}
	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	got, err := gateway.MergifyRoutingContractFingerprint(context.Background(), ValidateMergifyRoutingInput{Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("MergifyRoutingContractFingerprint() error = %v", err)
	}
	wantDigest := sha256.Sum256(content)
	if got != hex.EncodeToString(wantDigest[:]) {
		t.Fatalf("fingerprint = %q, want sha256 %q", got, hex.EncodeToString(wantDigest[:]))
	}
}

func TestValidateMergifyRoutingRequiresQueueVetoes(t *testing.T) {
	t.Parallel()
	content := `queue_rules:
  - name: default
    queue_conditions:
      - base = main
      - label != needs-human-review
      - label != do-not-merge
merge_protections:
  - name: mergeable shape
merge_protections_settings:
  auto_merge_conditions:
    - label = auto-merge
    - check-success = "Looper Gatekeeper"
`
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		if got := strings.Join(options.Args, " "); got != "api repos/acme/looper/contents/.mergify.yml --jq .content -H Accept: application/vnd.github+json" {
			t.Fatalf("gh args = %q", got)
		}
		return shell.Result{Stdout: base64.StdEncoding.EncodeToString([]byte(content))}, nil
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	if err := gateway.ValidateMergifyRouting(context.Background(), ValidateMergifyRoutingInput{Repo: "acme/looper", BaseRefName: "main"}); err != nil {
		t.Fatalf("ValidateMergifyRouting() error = %v", err)
	}
}

func TestValidateMergifyRoutingRejectsUnmatchedBaseBranch(t *testing.T) {
	t.Parallel()
	content := `queue_rules:
  - name: release-only
    queue_conditions:
      - base = release
      - label != needs-human-review
      - label != do-not-merge
merge_protections:
  - name: mergeable shape
merge_protections_settings:
  auto_merge_conditions:
    - label = auto-merge
    - check-success = "Looper Gatekeeper"
`
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		return shell.Result{Stdout: base64.StdEncoding.EncodeToString([]byte(content))}, nil
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	if err := gateway.ValidateMergifyRouting(context.Background(), ValidateMergifyRoutingInput{Repo: "acme/looper", BaseRefName: "main"}); err == nil {
		t.Fatal("ValidateMergifyRouting() error = nil, want unmatched base branch to fail closed")
	}
}

func TestQueueRuleAppliesToCompactAndRegexBaseConditions(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		conditions []string
		base       string
		want       bool
	}{
		{name: "compact exact match", conditions: []string{"base=release"}, base: "release", want: true},
		{name: "compact exact mismatch", conditions: []string{"base=release"}, base: "main", want: false},
		{name: "regex match", conditions: []string{`base ~= ^release/.*$`}, base: "release/1.0", want: true},
		{name: "negative regex match", conditions: []string{`base ~!= ^release/.*$`}, base: "release/1.0", want: false},
		{name: "invalid base condition", conditions: []string{"base ??? main"}, base: "main", want: false},
		{name: "invalid regex", conditions: []string{"base ~= ["}, base: "main", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := queueRuleAppliesToBase(tc.conditions, tc.base); got != tc.want {
				t.Fatalf("queueRuleAppliesToBase(%q, %q) = %t, want %t", tc.conditions, tc.base, got, tc.want)
			}
		})
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
    - check-success = "Looper Gatekeeper"
`
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		return shell.Result{Stdout: base64.StdEncoding.EncodeToString([]byte(content))}, nil
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	if err := gateway.ValidateMergifyRouting(context.Background(), ValidateMergifyRoutingInput{Repo: "acme/looper", BaseRefName: "main"}); err == nil {
		t.Fatal("ValidateMergifyRouting() error = nil, want missing queue veto to fail closed")
	}
}

func TestValidateMergifyRoutingRequiresCurrentHeadStatus(t *testing.T) {
	t.Parallel()
	content := `queue_rules:
  - name: default
    queue_conditions:
      - base = main
      - label != needs-human-review
      - label != do-not-merge
merge_protections:
  - name: mergeable shape
merge_protections_settings:
  auto_merge_conditions:
    - label = auto-merge
`
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		return shell.Result{Stdout: base64.StdEncoding.EncodeToString([]byte(content))}, nil
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	if err := gateway.ValidateMergifyRouting(context.Background(), ValidateMergifyRoutingInput{Repo: "acme/looper", BaseRefName: "main"}); err == nil {
		t.Fatal("ValidateMergifyRouting() error = nil, want missing current-head status to fail closed")
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
    - check-success = "Looper Gatekeeper"
`
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		return shell.Result{Stdout: base64.StdEncoding.EncodeToString([]byte(content))}, nil
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	if err := gateway.ValidateMergifyRouting(context.Background(), ValidateMergifyRoutingInput{Repo: "acme/looper", BaseRefName: "main"}); err == nil {
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
    - check-success = "Looper Gatekeeper"
`
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		return shell.Result{Stdout: base64.StdEncoding.EncodeToString([]byte(content))}, nil
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	if err := gateway.ValidateMergifyRouting(context.Background(), ValidateMergifyRoutingInput{Repo: "acme/looper", BaseRefName: "main"}); err == nil {
		t.Fatal("ValidateMergifyRouting() error = nil, want every queue rule to carry vetoes")
	}
}

// auto_merge_conditions only take effect when at least one merge_protections
// rule exists. A contract that omits merge_protections is inactive even though
// the label condition is present, so the validator must fail closed.
func TestValidateMergifyRoutingRejectsMissingMergeProtections(t *testing.T) {
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
    - check-success = "Looper Gatekeeper"
`
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		return shell.Result{Stdout: base64.StdEncoding.EncodeToString([]byte(content))}, nil
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	if err := gateway.ValidateMergifyRouting(context.Background(), ValidateMergifyRoutingInput{Repo: "acme/looper", BaseRefName: "main"}); err == nil {
		t.Fatal("ValidateMergifyRouting() error = nil, want missing merge_protections to fail closed")
	}
}
