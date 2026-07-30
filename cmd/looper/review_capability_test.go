package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/MumuTW/looper/internal/forge"
)

// The daemon decides whether to tell a reviewer agent to publish through this
// binary by running `looper review capability` and comparing stdout to
// forge.TrustedReviewCapabilityToken. Both sides read the token from that one
// constant, so this test is what proves the verb is wired to it rather than to
// a second literal that could drift out from under the probe.
func TestReviewCapabilityAnswersTheDaemonProbe(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := run([]string{"review", "capability"}, strings.NewReader(""), &stdout, &stderr)

	if code != 0 {
		t.Fatalf("run(review capability) = %d, want 0; the daemon reads a non-zero exit as no review support (stderr=%q)", code, stderr.String())
	}
	if got, want := stdout.String(), forge.TrustedReviewCapabilityToken+"\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

// A binary that answered the probe by falling into review submit would load
// config and reach the forge, which is exactly what must not happen: the probe
// runs against a binary the daemon has not yet decided to trust.
func TestReviewCapabilityIsExactAndDoesNotReachSubmit(t *testing.T) {
	for _, args := range [][]string{
		{"review", "capability", "extra"},
		{"review", "capability", "--event", "COMMENT"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer

			code := run(args, strings.NewReader(""), &stdout, &stderr)

			if code == 0 {
				t.Fatalf("run(%q) = 0, want refusal", args)
			}
			if strings.Contains(stdout.String(), forge.TrustedReviewCapabilityToken) {
				t.Fatalf("stdout = %q, want no capability token for a non-probe invocation", stdout.String())
			}
		})
	}
}
