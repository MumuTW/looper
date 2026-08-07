package webui

import (
	"fmt"
	"time"
)

// compactCount renders a count in the width the meta column has: exact below a
// thousand, one significant decimal up to ten thousand, whole thousands above.
func compactCount(value int) string {
	switch {
	case value < 1000:
		return fmt.Sprintf("%d", value)
	case value < 10000:
		return trimZeroDecimal(float64(value)/1000) + "k"
	case value < 1000000:
		return fmt.Sprintf("%dk", value/1000)
	default:
		return trimZeroDecimal(float64(value)/1000000) + "m"
	}
}

func trimZeroDecimal(value float64) string {
	text := fmt.Sprintf("%.1f", value)
	if len(text) > 2 && text[len(text)-2:] == ".0" {
		return text[:len(text)-2]
	}
	return text
}

// relativeAge is the single-token age in the meta column.
func relativeAge(age time.Duration) string {
	switch {
	case age < time.Minute:
		seconds := int(age / time.Second)
		if seconds < 1 {
			seconds = 1
		}
		return fmt.Sprintf("%ds", seconds)
	case age < time.Hour:
		return fmt.Sprintf("%dm", int(age/time.Minute))
	case age < 24*time.Hour:
		return fmt.Sprintf("%dh", int(age/time.Hour))
	case age < 365*24*time.Hour:
		return fmt.Sprintf("%dd", int(age/(24*time.Hour)))
	default:
		return fmt.Sprintf("%dy", int(age/(365*24*time.Hour)))
	}
}
