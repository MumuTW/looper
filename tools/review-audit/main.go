// Command review-audit classifies merged pull requests by whether they were
// actually reviewed (issue #367): reviewed, rate-limit-refused (the reviewer
// declined by rate limit and left a notice that reads like participation),
// or unreviewed. It consumes `gh pr list` JSON on stdin so the audit is a
// pure classifier over recorded state:
//
//	gh pr list --state merged --limit 100 \
//	  --json number,title,additions,deletions,mergedAt,reviews,comments \
//	  | go run ./tools/review-audit -large 300
//
// -large N appends a LARGE-UNREVIEWED flag to unreviewed or refused merges
// with at least N changed lines and exits 1 when any exist, so the audit can
// gate or feed a digest; 0 (the default) reports without failing.
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
