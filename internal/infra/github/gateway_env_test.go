package github

import (
	"context"
	"os"
	"testing"

	"github.com/nexu-io/looper/internal/infra/shell"
)

// The daemon runs detached, so a gh child that inherits only looperd's own
// environment is unauthenticated and dies on GitHub's per-IP rate limit. These
// tests pin the propagation contract that keeps that from happening.

func TestGatewayPropagatesCredentialEnvToGHChildren(t *testing.T) {
	t.Setenv("LOOPER_GATEWAY_ENV_PROBE", "inherited-value")

	var captured shell.Options
	gateway := New(Options{
		GHPath: "gh",
		Env:    map[string]string{"GH_TOKEN": "daemon-token"},
		GHRun: func(_ context.Context, options shell.Options) (shell.Result, error) {
			captured = options
			return shell.Result{Stdout: `{"login":"octocat"}`}, nil
		},
	})

	if _, err := gateway.GetIssueLabels(context.Background(), ViewIssueInput{Repo: "acme/looper", IssueNumber: 1}); err != nil {
		t.Fatalf("GetIssueLabels() error = %v", err)
	}
	if got := captured.Env["GH_TOKEN"]; got != "daemon-token" {
		t.Fatalf("gh child GH_TOKEN = %q, want the configured daemon credential", got)
	}
	if got := captured.Env["LOOPER_GATEWAY_ENV_PROBE"]; got != "inherited-value" {
		t.Fatalf("gh child LOOPER_GATEWAY_ENV_PROBE = %q, want the parent environment preserved", got)
	}
	if _, ok := captured.Env["PATH"]; !ok && os.Getenv("PATH") != "" {
		t.Fatal("gh child env dropped PATH; a partial env map would break gh command resolution")
	}
}

func TestGatewayWithoutCredentialEnvInheritsUnchanged(t *testing.T) {
	var captured shell.Options
	gateway := New(Options{
		GHPath: "gh",
		GHRun: func(_ context.Context, options shell.Options) (shell.Result, error) {
			captured = options
			return shell.Result{Stdout: `{"login":"octocat"}`}, nil
		},
	})

	if _, err := gateway.GetIssueLabels(context.Background(), ViewIssueInput{Repo: "acme/looper", IssueNumber: 1}); err != nil {
		t.Fatalf("GetIssueLabels() error = %v", err)
	}
	if captured.Env != nil {
		t.Fatalf("gh child Env = %v, want nil so the child inherits the daemon environment", captured.Env)
	}
}
