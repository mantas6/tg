// Package timesig parses the time signatures ("timesigns") that tg commands
// accept in place of full timestamps. Two forms exist:
//
//	ABSOLUTE  START "-" STOP   an explicit same-day wall-clock range ("9-:30")
//	RELATIVE  "+" DURATION     a span ending at the current 5-minute mark ("+:20")
//
// The full grammar, rounding rules and error cases are documented in
// docs/timesig.md; keep the two in sync.
package timesig

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Kind distinguishes the two timesign forms.
type Kind int

const (
	// Absolute is an explicit START-STOP range on now's calendar day.
	Absolute Kind = iota
	// Relative is a "+DURATION" span ending at the current 5-minute mark.
	Relative
)

// String renders the kind for error messages and tests.
func (k Kind) String() string {
	switch k {
	case Absolute:
		return "absolute"
	case Relative:
		return "relative"
	default:
		return "unknown"
	}
}

// Span is a parsed timesign: a resolved [Start, Stop) wall-clock interval plus
// the form it was written in. Stop is always strictly after Start.
type Span struct {
	Kind  Kind
	Start time.Time
	Stop  time.Time
}

// Duration is the length of the span.
func (s Span) Duration() time.Duration { return s.Stop.Sub(s.Start) }

// RelativePrefix marks a relative timesign.
const RelativePrefix = "+"

// IsRelative reports whether s looks like a relative timesign (a leading "+").
// It only inspects the prefix; Parse still validates the rest. Callers that
// accept both forms (e.g. `tg add`, `tg mod`) use this to decide whether an
// argument is anchored to "now" before parsing it.
func IsRelative(s string) bool {
	return strings.HasPrefix(strings.TrimSpace(s), RelativePrefix)
}

// Parse resolves a timesign — absolute or relative — into a Span. now is the
// reference instant (the calendar day for absolute signs, the anchor for
// relative ones) and loc is the location whose wall clock the sign refers to.
func Parse(s string, now time.Time, loc *time.Location) (Span, error) {
	if IsRelative(s) {
		return ParseRelative(s, now, loc)
	}
	return ParseAbsolute(s, now, loc)
}

// ParseAbsolute parses an absolute START-STOP timesign into two wall-clock
// times on now's calendar day in loc. The grammar is:
//
//	timesign = START "-" STOP
//	START    = H | H ":" MM
//	STOP     = H | H ":" MM | ":" MM
//	H        = hour   0-23
//	MM       = minute 0-59
//
// A minutes-only STOP (":MM") inherits the START hour, so "9-:30" is
// 09:00-09:30. Each side defaults its minutes to 0 ("10-11" is 10:00-11:00).
// STOP must be strictly after START; anything else (bad hour/minute, missing
// dash, empty side, stop <= start) is a clear error.
func ParseAbsolute(s string, now time.Time, loc *time.Location) (Span, error) {
	s = strings.TrimSpace(s)
	dash := strings.IndexByte(s, '-')
	if dash < 0 {
		return Span{}, fmt.Errorf("invalid timesign %q: expected START-STOP", s)
	}
	left := strings.TrimSpace(s[:dash])
	right := strings.TrimSpace(s[dash+1:])
	if left == "" {
		return Span{}, fmt.Errorf("invalid timesign %q: empty START", s)
	}
	if right == "" {
		return Span{}, fmt.Errorf("invalid timesign %q: empty STOP", s)
	}

	sh, sm, err := parseClockPart(left, false, 0)
	if err != nil {
		return Span{}, fmt.Errorf("invalid START %q: %w", left, err)
	}
	eh, em, err := parseClockPart(right, true, sh)
	if err != nil {
		return Span{}, fmt.Errorf("invalid STOP %q: %w", right, err)
	}

	day := now.In(loc)
	y, mo, d := day.Date()
	start := time.Date(y, mo, d, sh, sm, 0, 0, loc)
	stop := time.Date(y, mo, d, eh, em, 0, 0, loc)
	if !stop.After(start) {
		return Span{}, fmt.Errorf(
			"invalid timesign %q: STOP %s must be after START %s",
			s, clock(stop, loc), clock(start, loc))
	}
	return Span{Kind: Absolute, Start: start, Stop: stop}, nil
}

// ParseRelative parses a relative timesign — a duration counted back from now —
// into a Span. The grammar is:
//
//	timesign = "+" DURATION
//	DURATION = H | H ":" MM | ":" MM
//	H        = hours   0-23
//	MM       = minutes 0-59
//
// So "+:20" is 20 minutes, "+1:20" is 1h20m and "+1" is one hour. The span ENDS
// at now rounded DOWN to the nearest 5-minute wall-clock mark in loc (14:23 ->
// 14:20, 14:00 -> 14:00) and STARTS at that mark minus DURATION, which may fall
// on the previous day. The duration must be greater than zero, so "+0", "+:00"
// and "+0:00" are errors, as is a missing or malformed duration.
func ParseRelative(s string, now time.Time, loc *time.Location) (Span, error) {
	raw := strings.TrimSpace(s)
	if !strings.HasPrefix(raw, RelativePrefix) {
		return Span{}, fmt.Errorf("invalid timesign %q: expected +DURATION", raw)
	}
	body := strings.TrimSpace(strings.TrimPrefix(raw, RelativePrefix))
	if body == "" {
		return Span{}, fmt.Errorf("invalid timesign %q: empty duration", raw)
	}
	h, m, err := parseClockPart(body, true, 0)
	if err != nil {
		return Span{}, fmt.Errorf("invalid duration %q: %w", body, err)
	}
	dur := time.Duration(h)*time.Hour + time.Duration(m)*time.Minute
	if dur <= 0 {
		return Span{}, fmt.Errorf("invalid timesign %q: duration must be greater than zero", raw)
	}

	stop := Floor5(now.In(loc))
	return Span{Kind: Relative, Start: stop.Add(-dur), Stop: stop}, nil
}

// Floor5 rounds t DOWN to the preceding wall-clock 5-minute mark: minutes land
// on {00,05,...,55} with seconds and sub-second components zeroed. A time that
// already sits on a mark is returned unchanged (modulo its sub-minute parts).
// Flooring happens in t's own location so the result is a wall-clock mark, not
// a UTC one.
func Floor5(t time.Time) time.Time {
	const step = 5 * time.Minute
	// Top of t's hour, in its own location; the offset within the hour is what
	// gets truncated, which keeps the result correct across odd UTC offsets.
	hour := time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, t.Location())
	return hour.Add((t.Sub(hour) / step) * step)
}

// parseClockPart parses one side of a timesign ("H", "H:MM", or, when
// allowMinuteOnly, ":MM") into an hour and minute. A minutes-only part inherits
// inheritHour for its hour.
func parseClockPart(s string, allowMinuteOnly bool, inheritHour int) (hour, min int, err error) {
	if strings.HasPrefix(s, ":") {
		if !allowMinuteOnly {
			return 0, 0, errors.New("minutes-only form is not allowed here")
		}
		min, err = parseMinute(s[1:])
		if err != nil {
			return 0, 0, err
		}
		return inheritHour, min, nil
	}
	if i := strings.IndexByte(s, ':'); i >= 0 {
		hour, err = parseHour(s[:i])
		if err != nil {
			return 0, 0, err
		}
		min, err = parseMinute(s[i+1:])
		if err != nil {
			return 0, 0, err
		}
		return hour, min, nil
	}
	hour, err = parseHour(s)
	if err != nil {
		return 0, 0, err
	}
	return hour, 0, nil
}

// parseHour parses a 0-23 hour from a non-empty run of decimal digits.
func parseHour(s string) (int, error) {
	if s == "" {
		return 0, errors.New("missing hour")
	}
	v, err := atoiDigits(s)
	if err != nil {
		return 0, fmt.Errorf("bad hour %q", s)
	}
	if v < 0 || v > 23 {
		return 0, fmt.Errorf("hour %d out of range (0-23)", v)
	}
	return v, nil
}

// parseMinute parses a 0-59 minute from a non-empty run of decimal digits.
func parseMinute(s string) (int, error) {
	if s == "" {
		return 0, errors.New("missing minutes")
	}
	v, err := atoiDigits(s)
	if err != nil {
		return 0, fmt.Errorf("bad minutes %q", s)
	}
	if v < 0 || v > 59 {
		return 0, fmt.Errorf("minutes %d out of range (0-59)", v)
	}
	return v, nil
}

// atoiDigits parses a non-empty run of ASCII digits. Unlike strconv.Atoi it
// rejects signs and other decorations, so "+9" or "-1" never sneak into an
// hour or minute field.
func atoiDigits(s string) (int, error) {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, fmt.Errorf("not a number: %q", s)
		}
	}
	return strconv.Atoi(s)
}

// clock renders a wall-clock time (HH:MM) in loc for error messages.
func clock(t time.Time, loc *time.Location) string {
	return t.In(loc).Format("15:04")
}
