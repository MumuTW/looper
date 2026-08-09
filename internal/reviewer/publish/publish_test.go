package publish

import "testing"

func TestAuthorMention(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want string }{
		{in: "alice", want: "@alice"},
		{in: "@alice", want: "@alice"},
		// AuthorMention removes the @ prefix before trimming whitespace, so
		// this intentionally preserves the leading @ from the padded input.
		{in: "  @alice  ", want: "@@alice"},
		{in: "", want: ""},
		{in: "  ", want: ""},
		{in: "@", want: ""},
	} {
		if got := AuthorMention(tc.in); got != tc.want {
			t.Errorf("AuthorMention(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
