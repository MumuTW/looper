// Package reviewitem defines machine-readable review-item artifacts shared by
// trusted review publication and later Reviewer/Fixer rounds.
package reviewitem

import (
	"fmt"
	"regexp"
	"strings"
)

// Severity is the reviewer-authored severity carried by one review item.
// It is validated at the trusted review-submit boundary and persisted in the
// forge comment so later Fixer/Reviewer rounds do not have to infer it from
// prose.
type Severity string

const (
	SeverityBlocking    Severity = "blocking"
	SeverityNonBlocking Severity = "non_blocking"
	SeverityNit         Severity = "nit"
)

var markerPattern = regexp.MustCompile(`(?i)<!--\s*looper:review-item\s+severity=([a-z_]+)\s*-->`)

func ParseSeverity(value string) (Severity, error) {
	severity := Severity(strings.ToLower(strings.TrimSpace(value)))
	switch severity {
	case SeverityBlocking, SeverityNonBlocking, SeverityNit:
		return severity, nil
	default:
		return "", fmt.Errorf("unsupported review item severity %q", value)
	}
}

func Marker(severity Severity) (string, error) {
	parsed, err := ParseSeverity(string(severity))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("<!-- looper:review-item severity=%s -->", parsed), nil
}

// AttachMarker adds the validated machine-readable severity to visible review
// prose. Existing markers are rejected so one comment cannot carry competing
// severity authorities.
func AttachMarker(body string, severity Severity) (string, error) {
	if markerPattern.MatchString(body) {
		return "", fmt.Errorf("review item body already contains a severity marker")
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return "", fmt.Errorf("review item body must not be empty")
	}
	marker, err := Marker(severity)
	if err != nil {
		return "", err
	}
	return body + "\n\n" + marker, nil
}

// SeverityFromBody returns a severity only when the comment has exactly one
// well-formed marker. Malformed or duplicate markers fail closed.
func SeverityFromBody(body string) (Severity, bool) {
	matches := markerPattern.FindAllStringSubmatch(body, -1)
	if len(matches) != 1 {
		return "", false
	}
	severity, err := ParseSeverity(matches[0][1])
	return severity, err == nil
}
