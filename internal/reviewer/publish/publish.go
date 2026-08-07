// Package publish contains the small, pure formatting helpers shared by the
// Reviewer runner. Merge-gate and acceptance-criteria formatting was retired
// with Reviewer auto-merge; this package now only owns author mention syntax.
package publish

import "strings"

// AuthorMention normalizes a login into an @mention, tolerating an already
// prefixed or blank login. A blank login renders as no mention at all.
func AuthorMention(login string) string {
	login = strings.TrimSpace(strings.TrimPrefix(login, "@"))
	if login == "" {
		return ""
	}
	return "@" + login
}
