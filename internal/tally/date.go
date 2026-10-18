// Package tally holds the rules of the product: what a day entry is, how amounts and dates read,
// and how the balance and its changes are worked out. The web app and the MCP tools both go
// through it, so a person and an agent always see the same numbers.
package tally

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// DateLayout is how a day is written everywhere: in the database, the API and the MCP tools.
const DateLayout = "2006-01-02"

// earliest keeps typos like year 0026 out of the data.
const earliest = "2000-01-01"

// ParseDate reads a YYYY-MM-DD day. The result is midnight UTC, so adding days never meets a
// daylight saving change.
func ParseDate(s string) (time.Time, error) {
	t, err := time.Parse(DateLayout, s)
	if err != nil || t.Format(DateLayout) != s {
		return time.Time{}, fmt.Errorf("%q is not a date, use YYYY-MM-DD", s)
	}
	return t, nil
}

func formatDate(t time.Time) string { return t.Format(DateLayout) }

func addDays(s string, n int) string {
	t, err := ParseDate(s)
	if err != nil {
		return s
	}
	return formatDate(t.AddDate(0, 0, n))
}

// Monday is the first day of the week that s falls in.
func Monday(s string) string {
	t, err := ParseDate(s)
	if err != nil {
		return s
	}
	return formatDate(t.AddDate(0, 0, -((int(t.Weekday()) + 6) % 7)))
}

// StartOf is the first day of this week, month, quarter or year, counted from today.
func StartOf(unit, today string) string {
	t, err := ParseDate(today)
	if err != nil {
		return today
	}
	y, m := t.Year(), t.Month()
	switch unit {
	case "week":
		return Monday(today)
	case "month":
		return formatDate(time.Date(y, m, 1, 0, 0, 0, 0, time.UTC))
	case "quarter":
		return formatDate(time.Date(y, m-(m-1)%3, 1, 0, 0, 0, 0, time.UTC))
	default:
		return formatDate(time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC))
	}
}

// ISOWeek names the week s falls in, like 2026-W41.
func ISOWeek(s string) string {
	t, err := ParseDate(s)
	if err != nil {
		return ""
	}
	y, w := t.ISOWeek()
	return fmt.Sprintf("%d-W%02d", y, w)
}

// Weekday is the short day name, like Fri.
func Weekday(s string) string {
	t, err := ParseDate(s)
	if err != nil {
		return ""
	}
	return t.Weekday().String()[:3]
}

// Format writes signed minutes the way the app shows them: +1h 30m, −45m, +2h, 0m.
// The minus is U+2212.
func Format(min int) string {
	sign := ""
	if min > 0 {
		sign = "+"
	} else if min < 0 {
		sign = "−"
		min = -min
	}
	h, m := min/60, min%60
	switch {
	case h > 0 && m > 0:
		return fmt.Sprintf("%s%dh %dm", sign, h, m)
	case h > 0:
		return fmt.Sprintf("%s%dh", sign, h)
	default:
		return fmt.Sprintf("%s%dm", sign, m)
	}
}

// Amount pairs minutes with how they read, so an agent gets both the number and the words.
type Amount struct {
	Minutes int    `json:"minutes"`
	Text    string `json:"text"`
}

func amount(min int) Amount { return Amount{Minutes: min, Text: Format(min)} }

// Limits on what one day can hold. tally does not enforce the 15 minute rule, these only stop
// mistakes that no working day could produce.
const (
	MaxNote  = 40
	MaxAdd   = 120 * 60  // one save or one add_day call, above the form's 99h 999m
	MaxEntry = 1000 * 60 // the total on one day
)

// ErrInvalid marks an error the caller can fix, as opposed to a failure on our side.
var ErrInvalid = errors.New("invalid")

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// Message is the part of an error worth showing to a person or an agent.
func Message(err error) string {
	return strings.TrimPrefix(err.Error(), ErrInvalid.Error()+": ")
}

// CleanNote trims a note to one line of at most 40 characters.
func CleanNote(s string) (string, error) {
	s = strings.Join(strings.FieldsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f }), " ")
	s = strings.TrimSpace(s)
	if n := len([]rune(s)); n > MaxNote {
		return "", invalid("a note is at most %d characters, this one has %d", MaxNote, n)
	}
	return s, nil
}
