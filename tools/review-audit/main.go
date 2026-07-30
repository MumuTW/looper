// Command review-audit classifies merged pull requests by whether they were
// actually reviewed (issue #367): reviewed (a submitted review exists for
// the merged head), stale-reviewed (reviews exist, none for the head that
// merged), rate-limit-refused (the trusted reviewer account declined by
// rate limit and left a notice that reads like participation), or
// unreviewed. It consumes `gh pr list` JSON on stdin so the audit is a
// pure classifier over recorded state:
//
//	gh pr list --state merged --limit 100 \
//	  --json number,title,additions,deletions,mergedAt,headRefOid,reviews,comments \
//	  | go run ./tools/review-audit -large 300
//
// Authority: every verdict derives from GitHub's own records — submitted
// review objects (PENDING drafts and DISMISSED reviews do not count) bound
// to the reviewed commit; missing commit provenance does not count, and the
// refusal notice posted by the trusted reviewer account.
// The tool never infers review quality from discussion content; it only
// distinguishes recorded scrutiny from recorded refusal from silence.
//
// -large N appends a LARGE-UNREVIEWED flag to merges without a head
// review at or above N changed lines and exits 1 when any exist, so the
// audit can gate or feed a digest; 0 (the default) reports without
// failing, and negative values are rejected rather than silently
// disabling the gate.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
)

func main() {
	largeThreshold := flag.Int64("large", 0, "flag unreviewed merges with at least this many changed lines and exit 1")
	flag.Parse()
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "review-audit: read stdin: %v\n", err)
		os.Exit(2)
	}
	report, flagged, err := Audit(input, *largeThreshold)
	if err != nil {
		fmt.Fprintf(os.Stderr, "review-audit: %v\n", err)
		os.Exit(2)
	}
	fmt.Print(report)
	if flagged {
		os.Exit(1)
	}
}
