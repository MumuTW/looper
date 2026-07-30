package publish

import (
	"strings"
	"testing"
)

// The marker is the edit-on-retry contract: a retried round finds its
// earlier summary by this exact string. Its shape is pinned because
// existing PR comments in the wild carry it.
func TestMarkerShapeIsStable(t *testing.T) {
	t.Parallel()

	if got, want := Marker("abc123"), "<!-- looper:fixer-round head=abc123 -->"; got != want {
		t.Fatalf("Marker() = %q, want %q", got, want)
	}
	if got := Marker(""); got != "" {
		t.Fatalf("Marker(empty head) = %q, want empty: no head means nothing to key an edit on", got)
	}
}

func TestShortSHA(t *testing.T) {
	t.Parallel()

	cases := []struct{ in, want string }{
		{"abcdef1234567", "abcdef1"},
		{"abc", "abc"},
		{"  abcdef1234567  ", "abcdef1"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := ShortSHA(tc.in); got != tc.want {
			t.Fatalf("ShortSHA(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestBodyRendersMarkerHeaderAndBullets(t *testing.T) {
	t.Parallel()

	items := []Item{
		{Kind: "comment", Author: "alice", Path: "internal/foo.go", Line: 12, ThreadURL: "https://example/threads/t1", Status: "resolved", Explanation: "Replaced strings.Title with cases.Title.", ReplyState: "sent"},
		{Kind: "comment", Author: "bob", Path: "internal/bar.go", Status: "pending"},
		{Kind: "check", Name: "ci", Status: "check"},
		{Kind: "conflict", Status: "conflict"},
	}
	got := Body("headsha1234567", "commitsha234567", items)

	wantLines := []string{
		"<!-- looper:fixer-round head=headsha1234567 -->",
		"**Looper fixer round complete** — commits",
		"- ✅ Review comment on `internal/foo.go:12` (@alice) — [thread](https://example/threads/t1)",
		"  - Replaced strings.Title with cases.Title.",
		"- 🟡 Review comment on `internal/bar.go` (@bob)",
		"- 🧪 Failing check `ci`",
		"- 🔀 Merge conflict",
	}
	for _, line := range wantLines {
		if !strings.Contains(got, line) {
			t.Fatalf("Body() missing line %q:\n%s", line, got)
		}
	}
	if strings.HasSuffix(got, "\n") {
		t.Fatalf("Body() has trailing newline:\n%q", got)
	}
	if !strings.HasPrefix(got, "<!-- looper:fixer-round") {
		t.Fatalf("Body() must start with the marker line for edit-on-retry lookup:\n%s", got)
	}
}

func TestBodyWithoutHeadOmitsMarker(t *testing.T) {
	t.Parallel()

	got := Body("", "commitsha234567", nil)
	if strings.Contains(got, "looper:fixer-round") {
		t.Fatalf("Body(no head) contains marker:\n%s", got)
	}
	if !strings.HasPrefix(got, "**Looper fixer round complete**") {
		t.Fatalf("Body(no head) = %q, want header first", got)
	}
}

func TestBulletDetailRendering(t *testing.T) {
	t.Parallel()

	// Multi-line explanations indent continuation lines under the nested
	// bullet so the markdown stays inside one list item.
	multiline := bullet(Item{Kind: "comment", Status: "resolved", Explanation: "line one\nline two"})
	if !strings.Contains(multiline, "\n  - line one\n    line two") {
		t.Fatalf("bullet(multi-line explanation) = %q, want indented continuation", multiline)
	}

	// A reply that reached "sent" is the expected outcome and stays silent;
	// anything else surfaces.
	if got := bullet(Item{Kind: "comment", Status: "resolved", ReplyState: "sent"}); strings.Contains(got, "reply:") {
		t.Fatalf("bullet(sent reply) surfaces reply state: %q", got)
	}
	if got := bullet(Item{Kind: "comment", Status: "failed", ReplyState: "skipped"}); !strings.Contains(got, "\n  - reply: skipped") {
		t.Fatalf("bullet(skipped reply) = %q, want reply state surfaced", got)
	}

	// Author and thread suffixes are comment-only decorations.
	nonComment := bullet(Item{Kind: "check", Name: "ci", Author: "alice", ThreadURL: "https://example/t", Status: "check"})
	if strings.Contains(nonComment, "@alice") || strings.Contains(nonComment, "[thread]") {
		t.Fatalf("bullet(check) = %q, want no comment-only decorations", nonComment)
	}
}

func TestLabelByKind(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		item Item
		want string
	}{
		{name: "comment with path and line", item: Item{Kind: "comment", Path: "a.go", Line: 3}, want: "Review comment on `a.go:3`"},
		{name: "comment with path only", item: Item{Kind: "comment", Path: "a.go"}, want: "Review comment on `a.go`"},
		{name: "bare comment", item: Item{Kind: "comment"}, want: "Review comment"},
		{name: "named check", item: Item{Kind: "check", Name: "ci"}, want: "Failing check `ci`"},
		{name: "bare check", item: Item{Kind: "check"}, want: "Failing check"},
		{name: "conflict", item: Item{Kind: "conflict"}, want: "Merge conflict"},
		{name: "unknown kind with name", item: Item{Kind: "custom", Name: "Special"}, want: "Special"},
		{name: "unknown kind bare", item: Item{Kind: " custom "}, want: "custom"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := label(tc.item); got != tc.want {
				t.Fatalf("label(%+v) = %q, want %q", tc.item, got, tc.want)
			}
		})
	}
}

func TestStatusIconMapping(t *testing.T) {
	t.Parallel()

	cases := []struct{ status, want string }{
		{"resolved", "✅"},
		{"already_resolved", "✅"},
		{"RESOLVED", "✅"},
		{"agent_declined", "⏸️"},
		{"failed", "⚠️"},
		{"pending", "🟡"},
		{"conflict", "🔀"},
		{"check", "🧪"},
		{"anything-else", "•"},
		{"", "•"},
	}
	for _, tc := range cases {
		if got := statusIcon(tc.status); got != tc.want {
			t.Fatalf("statusIcon(%q) = %q, want %q", tc.status, got, tc.want)
		}
	}
}
