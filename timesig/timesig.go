// Package timesig parses the time signatures ("timesigns") that tg commands
// accept in place of full timestamps. Four forms exist:
//
//	ABSOLUTE  START "-" STOP   an explicit same-day wall-clock range ("9-:30")
//	RELATIVE  "+" DURATION     a span ending at the current 5-minute mark ("+:20")
//	NEGATIVE  "-" DURATION     a length to take AWAY from a span ("-:20")
//	DURATION  DURATION         a bare length with no times of its own ("1:30")
//
// Within an ABSOLUTE range either side may be the "@" token, which resolves to
// now floored to the preceding 5-minute mark; as a START it also stands in for
// the range separator, so "@:30" reads as "from now until :30" (see
// ParseAbsolute).
//
// The first two resolve to a Span on their own; the last two do not, so
// ParseNegative and ParseDuration return just the length and the caller anchors
// it (see `tg add`, which starts the entry at the last entry's end, and
// `tg mod`, which pulls an entry's end back by a negative sign's length).
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

// Kind distinguishes the two timesign forms that resolve to a Span. The bare
// duration and negative forms have no times of their own, so they never yield a
// Span (see ParseDuration and ParseNegative).
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

// IsDuration reports whether s looks like a bare duration timesign ("1:30"):
// non-empty, without the relative "+" prefix, without the "-" that separates an
// absolute range and without the "@" now-token that marks an absolute range.
// Like IsRelative it is a classifier, not a validator, so IsDuration("bad") is
// true while ParseDuration("bad") errors. Callers that accept the form (only
// `tg add`, which anchors it to the last entry's end) use this to route the
// argument before parsing it.
func IsDuration(s string) bool {
	s = strings.TrimSpace(s)
	return s != "" && !strings.HasPrefix(s, RelativePrefix) &&
		!strings.Contains(s, "-") && !strings.Contains(s, AtSign)
}

// AtSign is the token that stands for "now" inside an absolute timesign,
// floored to the preceding 5-minute wall-clock mark (see Floor5).
const AtSign = "@"

// IsAt reports whether s uses the "@" now-token. Like the "@" itself this is an
// absolute-range affair, so it is a substring test rather than a prefix one:
// "@:30", "@-11" and "9-@" all count. It is a classifier, not a validator —
// ParseAbsolute still decides whether the surrounding range is well formed.
//
// Callers that resolve a timesign against "now" (only `tg add`) use it to
// refuse the token on a day --date moved them to, where there is no "now" for
// it to mean, exactly as they refuse the relative form there.
func IsAt(s string) bool {
	return strings.Contains(strings.TrimSpace(s), AtSign)
}

// NegativePrefix marks a negative timesign.
const NegativePrefix = "-"

// IsNegative reports whether s is a negative timesign: the "-" prefix followed
// by something SHAPED like a duration ("-30", "-1:20"), i.e. digits with at most
// one ":" and nothing else.
//
// Unlike IsRelative and IsDuration it inspects the whole string rather than just
// the punctuation in front, because "-" is overloaded three ways: it separates
// an absolute range ("9-10"), it starts a negative sign ("-30"), and it starts a
// command-line flag ("-desc"). Only the shape tells them apart, so the check has
// to be tight enough for `tg mod` to pick its arguments out of a command line
// before the flag package sees them (see splitNegativeTimesigns).
//
// It is still only a shape test, not a validator: IsNegative("-99:99") is true
// while ParseNegative("-99:99") errors.
func IsNegative(s string) bool {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, NegativePrefix) {
		return false
	}
	return isDurationShape(strings.TrimSpace(strings.TrimPrefix(s, NegativePrefix)))
}

// isDurationShape reports whether s is written like a DURATION ("H", "H:MM" or
// ":MM"): at least one ASCII digit, at most one ":", and nothing else.
func isDurationShape(s string) bool {
	digits, colons := 0, 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= '0' && c <= '9':
			digits++
		case c == ':':
			colons++
		default:
			return false
		}
	}
	return digits > 0 && colons <= 1
}

// Parse resolves a self-contained timesign — absolute or relative — into a
// Span. now is the reference instant (the calendar day for absolute signs, the
// anchor for relative ones) and loc is the location whose wall clock the sign
// refers to. A bare duration has no times to resolve, so it is not handled
// here: it falls through to ParseAbsolute and is reported as a malformed range
// (see ParseDuration for the form itself).
func Parse(s string, now time.Time, loc *time.Location) (Span, error) {
	if IsRelative(s) {
		return ParseRelative(s, now, loc)
	}
	if IsNegative(s) {
		// A negative sign names a length to take away from a span that
		// already exists, so on its own there is nothing for it to resolve
		// to. Saying so beats reporting it as a range with an empty START.
		return Span{}, fmt.Errorf(
			"invalid timesign %q: a negative timesign only subtracts from an existing entry (`tg mod`)",
			strings.TrimSpace(s))
	}
	return ParseAbsolute(s, now, loc)
}

// ParseAbsolute parses an absolute START-STOP timesign into two wall-clock
// times on now's calendar day in loc. The grammar is:
//
//	timesign = START "-" STOP | "@" STOP
//	START    = H | H ":" MM | "@"
//	STOP     = H | H ":" MM | ":" MM | "@"
//	H        = hour   0-23
//	MM       = minute 0-59
//
// A minutes-only STOP (":MM") inherits the START hour, so "9-:30" is
// 09:00-09:30. Each side defaults its minutes to 0 ("10-11" is 10:00-11:00).
// STOP must be strictly after START; anything else (bad hour/minute, missing
// dash, empty side, stop <= start) is a clear error.
//
// The "@" token resolves to now floored to the preceding 5-minute wall-clock
// mark in loc (see Floor5): "9-@" runs from 09:00 until that mark. As a START,
// "@" also stands in for the range separator, so "@:30" needs no dash and reads
// as "from now until :30" (the ":30" inheriting @'s hour); the explicit "@-:30"
// is accepted too.
func ParseAbsolute(s string, now time.Time, loc *time.Location) (Span, error) {
	s = strings.TrimSpace(s)

	// now floored to the preceding 5-minute mark, which the "@" token resolves
	// to on either side of the range.
	at := Floor5(now.In(loc))

	var left, right string
	if strings.HasPrefix(s, AtSign) && strings.IndexByte(s, '-') < 0 {
		// "@STOP": the "@" START implies the separator, so no dash is needed.
		left = AtSign
		right = strings.TrimSpace(s[len(AtSign):])
	} else {
		dash := strings.IndexByte(s, '-')
		if dash < 0 {
			return Span{}, fmt.Errorf("invalid timesign %q: expected START-STOP", s)
		}
		left = strings.TrimSpace(s[:dash])
		right = strings.TrimSpace(s[dash+1:])
	}
	if left == "" {
		return Span{}, fmt.Errorf("invalid timesign %q: empty START", s)
	}
	if right == "" {
		return Span{}, fmt.Errorf("invalid timesign %q: empty STOP", s)
	}

	sh, sm, err := parseClockOrAt(left, false, 0, at)
	if err != nil {
		return Span{}, fmt.Errorf("invalid START %q: %w", left, err)
	}
	eh, em, err := parseClockOrAt(right, true, sh, at)
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
	dur, err := parseDurationBody(body, raw)
	if err != nil {
		return Span{}, err
	}

	stop := Floor5(now.In(loc))
	return Span{Kind: Relative, Start: stop.Add(-dur), Stop: stop}, nil
}

// ParseNegative parses a negative timesign — a length of time to take AWAY from
// a span that already exists — into a NEGATIVE time.Duration. The grammar is the
// DURATION of the relative form with a "-" instead of the "+":
//
//	timesign = "-" DURATION
//	DURATION = H | H ":" MM | ":" MM
//	H        = hours   0-23
//	MM       = minutes 0-59
//
// So "-1:20" is -1h20m, "-:45" is -45 minutes and "-2" is minus two hours. The
// sign is kept in the result so a caller shortens a span by ADDING it to a time,
// which is the same arithmetic the relative form's duration gets and so cannot
// be applied the wrong way round by accident.
//
// Like the bare duration form it carries no anchor, so neither now nor a
// location is needed: the caller decides what the length is taken off (`tg mod`
// pulls the entry's end back by it). The magnitude must be greater than zero, so
// "-0", "-:00" and "-0:00" are errors, as is a missing or malformed one.
func ParseNegative(s string) (time.Duration, error) {
	raw := strings.TrimSpace(s)
	if !strings.HasPrefix(raw, NegativePrefix) {
		return 0, fmt.Errorf("invalid timesign %q: expected -DURATION", raw)
	}
	body := strings.TrimSpace(strings.TrimPrefix(raw, NegativePrefix))
	dur, err := parseDurationBody(body, raw)
	if err != nil {
		return 0, err
	}
	return -dur, nil
}

// ParseDuration parses a bare duration timesign — a length of time with no
// wall-clock times of its own — into a positive time.Duration. The grammar is
// the DURATION of the relative form, without its "+":
//
//	timesign = DURATION
//	DURATION = H | H ":" MM | ":" MM
//	H        = hours   0-23
//	MM       = minutes 0-59
//
// So "1:30" is 1h30m, ":45" is 45 minutes and "2" is two hours. Because the
// form carries no anchor, neither now nor a location is needed: the caller
// decides where the span starts (`tg add` starts it at the last entry's end).
// The duration must be greater than zero, so "0", ":00" and "0:00" are errors,
// as is a missing or malformed one.
func ParseDuration(s string) (time.Duration, error) {
	raw := strings.TrimSpace(s)
	return parseDurationBody(raw, raw)
}

// parseDurationBody parses the shared DURATION production ("H", "H:MM" or
// ":MM") into a positive length of time. body is the duration text itself and
// raw the whole timesign as the user wrote it (with the "+" for the relative
// form), which is what error messages quote.
func parseDurationBody(body, raw string) (time.Duration, error) {
	if body == "" {
		return 0, fmt.Errorf("invalid timesign %q: empty duration", raw)
	}
	h, m, err := parseClockPart(body, true, 0)
	if err != nil {
		// A relative sign names both the whole timesign and the duration
		// inside it; a bare duration IS the timesign, so quoting it twice
		// would only repeat the same text.
		if body != raw {
			return 0, fmt.Errorf("invalid timesign %q: invalid duration %q: %w", raw, body, err)
		}
		return 0, fmt.Errorf("invalid timesign %q: %w", raw, err)
	}
	dur := time.Duration(h)*time.Hour + time.Duration(m)*time.Minute
	if dur <= 0 {
		return 0, fmt.Errorf("invalid timesign %q: duration must be greater than zero", raw)
	}
	return dur, nil
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

// parseClockOrAt parses one side of an absolute range, resolving the "@" token
// to at's wall-clock hour and minute (already floored by ParseAbsolute) and
// deferring every other form to parseClockPart. Passing at's minute through as
// the START hour of a minutes-only STOP is intentional: "@:30" inherits @'s
// hour just as "9-:30" inherits 9.
func parseClockOrAt(s string, allowMinuteOnly bool, inheritHour int, at time.Time) (hour, minute int, err error) {
	if s == AtSign {
		return at.Hour(), at.Minute(), nil
	}
	return parseClockPart(s, allowMinuteOnly, inheritHour)
}

// parseClockPart parses one side of a timesign ("H", "H:MM", or, when
// allowMinuteOnly, ":MM") into an hour and minute. A minutes-only part inherits
// inheritHour for its hour.
func parseClockPart(s string, allowMinuteOnly bool, inheritHour int) (hour, minute int, err error) {
	if strings.HasPrefix(s, ":") {
		if !allowMinuteOnly {
			return 0, 0, errors.New("minutes-only form is not allowed here")
		}
		minute, err = parseMinute(s[1:])
		if err != nil {
			return 0, 0, err
		}
		return inheritHour, minute, nil
	}
	if i := strings.IndexByte(s, ':'); i >= 0 {
		hour, err = parseHour(s[:i])
		if err != nil {
			return 0, 0, err
		}
		minute, err = parseMinute(s[i+1:])
		if err != nil {
			return 0, 0, err
		}
		return hour, minute, nil
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
