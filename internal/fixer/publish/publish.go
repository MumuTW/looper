// Package publish holds the fixer's publishing authority for the PR-facing
// round summary: the display contract for a fix item's outcome and the
// rendering of the summary comment body, including the hidden head-SHA
// marker that lets a retried round edit its earlier summary instead of
// posting a duplicate. It has no I/O and no dependency on the fixer's
// checkpoint or domain types — the fixer package owns GitHub access and
// the mapping from its state onto []Item. Fourth extraction under issue
// #120, following internal/reviewer/workflow (#131), internal/fixer/
// workflow (#309), and internal/fixer/reconcile (#318).
package publish

import (
	"fmt"
	"strings"
)

// Item is one fix item's display-ready outcome. The fixer maps its domain
// state onto this contract; everything here is already resolved for
// presentation (ThreadURL fallbacks applied, fallback summaries collapsed
// to a single line in Explanation).
type Item struct {
	// Kind is the fix item type: "comment", "check", "conflict", or a
	// free-form name for future kinds.
	Kind   string
	Path   string
	Line   int64
	Name   string
	Author string
	// ThreadURL links the review thread; rendered only for comments.
	ThreadURL string
	// Status selects the outcome icon: resolved/already_resolved,
	// agent_declined, failed, pending, conflict, check.
	Status string
	// Explanation is the nested detail line: the agent's explanation when
	// present, else a pre-collapsed summary of the original comment.
	Explanation string
	// ReplyState is rendered when the thread reply did not reach "sent".
	ReplyState string
}

const markerPrefix = "<!-- looper:fixer-round head="

// Marker returns the hidden summary marker keyed by head SHA, or "" when
// there is no head to key on. The marker on the body's first line is the
// edit-on-retry contract: a later run for the same head finds the comment
// by marker and edits it instead of posting a duplicate.
func Marker(headSHA string) string {
	if headSHA == "" {
		return ""
	}
	return markerPrefix + headSHA + " -->"
}

// ShortSHA abbreviates a commit SHA to the conventional 7 characters for
// display; shorter values (and "") pass through unchanged.
func ShortSHA(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 7 {
		return value
	}
	return value[:7]
}

// Body renders the round summary comment: the marker line, a header naming
// the round's commit, and one bullet per item. The caller appends any
// disclosure stamp or footer.
func Body(headSHA, commitSHA string, items []Item) string {
	var b strings.Builder
	if marker := Marker(headSHA); marker != "" {
		b.WriteString(marker)
		b.WriteString("\n")
	}
	b.WriteString("**Looper fixer round complete**")
	if shortSHA := ShortSHA(commitSHA); shortSHA != "" {
		b.WriteString(" — ")
		b.WriteString(shortSHA)
	}
	b.WriteString("\n\n")
	for _, item := range items {
		b.WriteString(bullet(item))
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func bullet(item Item) string {
	var b strings.Builder
	b.WriteString("- ")
	b.WriteString(statusIcon(item.Status))
	b.WriteString(" ")
	b.WriteString(label(item))
	if item.Author != "" && item.Kind == "comment" {
		b.WriteString(" (@")
		b.WriteString(item.Author)
		b.WriteString(")")
	}
	if item.ThreadURL != "" && item.Kind == "comment" {
		b.WriteString(" — [thread](")
		b.WriteString(item.ThreadURL)
		b.WriteString(")")
	}
	if explanation := strings.TrimSpace(item.Explanation); explanation != "" {
		b.WriteString("\n  - ")
		b.WriteString(strings.ReplaceAll(explanation, "\n", "\n    "))
	}
	if item.ReplyState != "" && item.ReplyState != "sent" {
		b.WriteString("\n  - reply: ")
		b.WriteString(item.ReplyState)
	}
	return b.String()
}

func label(item Item) string {
	switch item.Kind {
	case "comment":
		if item.Path != "" {
			if item.Line > 0 {
				return fmt.Sprintf("Review comment on `%s:%d`", item.Path, item.Line)
			}
			return fmt.Sprintf("Review comment on `%s`", item.Path)
		}
		return "Review comment"
	case "check":
		if item.Name != "" {
			return "Failing check `" + item.Name + "`"
		}
		return "Failing check"
	case "conflict":
		return "Merge conflict"
	default:
		if item.Name != "" {
			return item.Name
		}
		return strings.TrimSpace(item.Kind)
	}
}

func statusIcon(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "resolved", "already_resolved":
		return "✅"
	case "agent_declined":
		return "⏸️"
	case "failed":
		return "⚠️"
	case "pending":
		return "🟡"
	case "conflict":
		return "🔀"
	case "check":
		return "🧪"
	default:
		return "•"
	}
}
