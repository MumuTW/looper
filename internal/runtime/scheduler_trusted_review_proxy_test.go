package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/forge"
)

func TestMintTrustedReviewProxyResolvesConfiguredLooperCommandFromPATH(t *testing.T) {
	binDir := t.TempDir()
	looperPath := filepath.Join(binDir, "looper")
	if err := os.WriteFile(looperPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(looperPath) error = %v", err)
	}
	t.Setenv("PATH", binDir)

	sock, cleanup, err := mintTrustedReviewProxyForPR(
		"looper",
		nil,
		"acme/looper#42",
		t.TempDir(),
		config.Config{},
		forge.TrustedReviewProxyPolicy{
			Clean:            "COMMENT",
			Blocking:         "REQUEST_CHANGES",
			ExpectedCommitID: "head-42",
		},
	)
	if err != nil {
		t.Fatalf("mintTrustedReviewProxyForPR() error = %v", err)
	}
	defer cleanup()
	if !filepath.IsAbs(sock) {
		t.Fatalf("socket path = %q, want absolute path", sock)
	}
}

// TestTrustedReviewProxyUsesTheDaemonSubmitterContract keeps the socket and
// daemon-side submitter in one integration contract. It invokes the real
// runTrustedReviewSubmission -> reviewsubmit.RunTrusted -> GitHub gateway path
// with a fake gh executable at the subprocess boundary, so an injected fake
// submitter cannot hide a broken field mapping or missing production wiring.
func TestTrustedReviewProxyUsesTheDaemonSubmitterContract(t *testing.T) {
	root := t.TempDir()
	ghPath := filepath.Join(root, "gh")
	submitLog := filepath.Join(root, "submitted-review.json")
	script := fmt.Sprintf(`#!/bin/sh
set -eu
submit_log=%q

if [ "$1" = "pr" ] && [ "$2" = "view" ]; then
  cat <<'JSON'
{"number":42,"title":"Contract PR","body":"Body","state":"OPEN","isDraft":false,"headRefOid":"daemon-bound-head","baseRefOid":"base-head","author":{"login":"octocat"},"labels":[],"headRefName":"feature","baseRefName":"main","mergeStateStatus":"CLEAN"}
JSON
  exit 0
fi

if [ "$1" = "pr" ] && [ "$2" = "diff" ]; then
  exit 0
fi

if [ "$1" = "api" ]; then
  case "$*" in
    *pulls/*/reviews*)
      cat > "$submit_log"
      echo '{"id":1,"state":"COMMENTED"}'
      exit 0
      ;;
  esac
fi

echo "unexpected gh invocation: $*" >&2
exit 1
`, submitLog)
	if err := os.WriteFile(ghPath, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}

	snapshot, err := config.DefaultConfig(root)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	snapshot.Tools.GHPath = &ghPath
	gitPath := "git"
	snapshot.Tools.GitPath = &gitPath
	policy := forge.TrustedReviewProxyPolicy{
		Clean:            "COMMENT",
		Blocking:         "COMMENT",
		ExpectedCommitID: "daemon-bound-head",
	}
	sockPath, cleanup, err := forge.StartTrustedReviewProxy(runTrustedReviewSubmission, nil, "acme/looper#42", root, snapshot, policy)
	if err != nil {
		t.Fatalf("StartTrustedReviewProxy() error = %v", err)
	}
	t.Cleanup(cleanup)
	t.Setenv(forge.TrustedReviewSockEnv, sockPath)

	argv := []string{
		"review", "submit", "acme/looper#42",
		"--event", "COMMENT",
		"--commit-id", "agent-head",
		"--clean-review-event", "COMMENT",
		"--blocking-review-event", "COMMENT",
	}
	payload := []byte(`{"body":"internal/reviewsubmit/reviewsubmit.go: publish the validated review.\n<!-- looper:review id=contract-1 head=daemon-bound-head outcome=actionable -->","comments":[]}`)
	if err := forge.ProxyReviewSubmit(argv, payload, root); err != nil {
		t.Fatalf("ProxyReviewSubmit() error = %v", err)
	}

	submitted, err := os.ReadFile(submitLog)
	if err != nil {
		t.Fatalf("read submitted review: %v", err)
	}
	if !strings.Contains(string(submitted), `"event":"COMMENT"`) || !strings.Contains(string(submitted), "daemon-bound-head") {
		t.Fatalf("submitted review = %s, want daemon-bound event and head", submitted)
	}
}
