package timesig

import (
	"testing"
	"time"
)

// now is the reference instant timesigns resolve against: 2026-01-02 15:00 UTC.
var now = time.Date(2026, 1, 2, 15, 0, 0, 0, time.UTC)

// hm builds a wall-clock time on now's calendar day (2026-01-02) in UTC.
func hm(h, m int) time.Time {
	return time.Date(2026, 1, 2, h, m, 0, 0, time.UTC)
}

func TestParseAbsoluteValid(t *testing.T) {
	cases := []struct {
		in                     string
		wantStartH, wantStartM int
		wantStopH, wantStopM   int
	}{
		{"9-:30", 9, 0, 9, 30},      // minutes-only stop inherits start hour
		{"10-11", 10, 0, 11, 0},     // bare hours default minutes to 0
		{"10:30-11", 10, 30, 11, 0}, // H:MM start, bare-hour stop
		{"9:15-9:45", 9, 15, 9, 45}, // both sides H:MM
		{"0-:01", 0, 0, 0, 1},       // midnight hour, one-minute span
		{"23-23:59", 23, 0, 23, 59}, // last hour of the day
		{" 9 - :30 ", 9, 0, 9, 30},  // surrounding/inner whitespace tolerated
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			span, err := Parse(tc.in, now, time.UTC)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.in, err)
			}
			if span.Kind != Absolute {
				t.Errorf("kind = %v, want absolute", span.Kind)
			}
			if !span.Start.Equal(hm(tc.wantStartH, tc.wantStartM)) {
				t.Errorf("start = %v, want %02d:%02d", span.Start, tc.wantStartH, tc.wantStartM)
			}
			if !span.Stop.Equal(hm(tc.wantStopH, tc.wantStopM)) {
				t.Errorf("stop = %v, want %02d:%02d", span.Stop, tc.wantStopH, tc.wantStopM)
			}
		})
	}
}

func TestParseAbsoluteErrors(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"missing dash", "9"},
		{"empty", ""},
		{"empty start", "-11"},
		{"empty stop", "9-"},
		{"empty both", "-"},
		{"bad start hour", "24-25"},
		{"bad stop hour", "9-24"},
		{"bad start minutes", "9:60-10"},
		{"bad stop minutes", "9-9:60"},
		{"minutes-only start", ":30-10"},
		{"stop equals start", "9-9"},
		{"stop before start", "10-9"},
		{"minutes-only stop not after start", "9-:00"},
		{"non-numeric hour", "ab-cd"},
		{"non-numeric minutes", "9:aa-10"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse(tc.in, now, time.UTC); err == nil {
				t.Errorf("Parse(%q) = nil error, want an error", tc.in)
			}
		})
	}
}

func TestParseRelativeValid(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		now     time.Time
		wantDur time.Duration
		wantEnd time.Time
	}{
		{"minutes only", "+:20", hm(15, 0), 20 * time.Minute, hm(15, 0)},
		{"hour and minutes", "+1:20", hm(15, 0), 80 * time.Minute, hm(15, 0)},
		{"bare hour", "+1", hm(15, 0), time.Hour, hm(15, 0)},
		{"two hours", "+2", hm(15, 0), 2 * time.Hour, hm(15, 0)},
		{"five minutes", "+:05", hm(15, 0), 5 * time.Minute, hm(15, 0)},
		{"max duration", "+23:59", hm(15, 0), 23*time.Hour + 59*time.Minute, hm(15, 0)},
		{"zero padded hour", "+01:05", hm(15, 0), 65 * time.Minute, hm(15, 0)},
		{"whitespace tolerated", " + 1:20 ", hm(15, 0), 80 * time.Minute, hm(15, 0)},

		// now is floored DOWN to the preceding 5-minute mark.
		{"floors now down", "+:20", hm(14, 23), 20 * time.Minute, hm(14, 20)},
		{"already on mark", "+:20", hm(14, 0), 20 * time.Minute, hm(14, 0)},
		{"on a 5m mark", "+:20", hm(14, 25), 20 * time.Minute, hm(14, 25)},
		{"one minute before mark", "+:20", hm(14, 29), 20 * time.Minute, hm(14, 25)},
		{"never rounds up", "+:20", hm(14, 58), 20 * time.Minute, hm(14, 55)},

		// Crossing midnight backwards is allowed.
		{"crosses midnight", "+1", hm(0, 10), time.Hour, hm(0, 10)},
		{"crosses midnight with minutes", "+2:30", hm(1, 0), 150 * time.Minute, hm(1, 0)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			span, err := Parse(tc.in, tc.now, time.UTC)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.in, err)
			}
			if span.Kind != Relative {
				t.Errorf("kind = %v, want relative", span.Kind)
			}
			if !span.Stop.Equal(tc.wantEnd) {
				t.Errorf("stop = %v, want %v", span.Stop, tc.wantEnd)
			}
			if !span.Start.Equal(tc.wantEnd.Add(-tc.wantDur)) {
				t.Errorf("start = %v, want %v", span.Start, tc.wantEnd.Add(-tc.wantDur))
			}
			if span.Duration() != tc.wantDur {
				t.Errorf("duration = %v, want %v", span.Duration(), tc.wantDur)
			}
		})
	}
}

func TestParseRelativeSecondsAreDropped(t *testing.T) {
	// Sub-minute components of now never leak into the resolved span.
	in := time.Date(2026, 1, 2, 14, 23, 47, 123456789, time.UTC)
	span, err := Parse("+:15", in, time.UTC)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !span.Stop.Equal(hm(14, 20)) {
		t.Errorf("stop = %v, want 14:20", span.Stop)
	}
	if !span.Start.Equal(hm(14, 5)) {
		t.Errorf("start = %v, want 14:05", span.Start)
	}
}

func TestParseRelativeCrossesMidnightBackwards(t *testing.T) {
	span, err := Parse("+2", hm(0, 30), time.UTC)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := time.Date(2026, 1, 1, 22, 30, 0, 0, time.UTC)
	if !span.Start.Equal(want) {
		t.Errorf("start = %v, want %v", span.Start, want)
	}
	if !span.Stop.Equal(hm(0, 30)) {
		t.Errorf("stop = %v, want 00:30", span.Stop)
	}
}

func TestParseRelativeErrors(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"zero minutes", "+:00"},
		{"zero hours", "+0"},
		{"zero hours and minutes", "+0:00"},
		{"empty duration", "+"},
		{"only whitespace", "+ "},
		{"hours out of range", "+24"},
		{"minutes out of range", "+1:60"},
		{"non-numeric hours", "+a"},
		{"non-numeric minutes", "+:ab"},
		{"missing minutes", "+1:"},
		{"negative", "+-1"},
		{"double sign", "++1"},
		{"trailing garbage", "+1:20x"},
		{"range not allowed", "+9-10"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse(tc.in, now, time.UTC); err == nil {
				t.Errorf("Parse(%q) = nil error, want an error", tc.in)
			}
		})
	}
}

func TestIsRelative(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"+:20", true},
		{"+1:20", true},
		{" +1", true},
		{"+", true},    // prefix only; Parse still rejects it
		{"+bad", true}, // prefix only; Parse still rejects it
		{"9-:30", false},
		{"10-11", false},
		{"", false},
		{" ", false},
	}
	for _, tc := range cases {
		if got := IsRelative(tc.in); got != tc.want {
			t.Errorf("IsRelative(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestParseDispatchesOnPrefix(t *testing.T) {
	// A relative sign parsed as absolute (and vice versa) must fail, proving
	// Parse dispatches on the "+" prefix rather than guessing.
	if _, err := ParseAbsolute("+:20", now, time.UTC); err == nil {
		t.Error("ParseAbsolute(\"+:20\") = nil error, want an error")
	}
	if _, err := ParseRelative("9-:30", now, time.UTC); err == nil {
		t.Error("ParseRelative(\"9-:30\") = nil error, want an error")
	}
}

func TestFloor5(t *testing.T) {
	cases := []struct {
		in, want time.Time
	}{
		{hm(14, 0), hm(14, 0)},
		{hm(14, 1), hm(14, 0)},
		{hm(14, 4), hm(14, 0)},
		{hm(14, 5), hm(14, 5)},
		{hm(14, 23), hm(14, 20)},
		{hm(14, 59), hm(14, 55)},
		{time.Date(2026, 1, 2, 14, 2, 30, 0, time.UTC), hm(14, 0)},
		{time.Date(2026, 1, 2, 0, 3, 0, 0, time.UTC), hm(0, 0)},
		{time.Date(2026, 1, 2, 23, 59, 59, 999999999, time.UTC), hm(23, 55)},
	}
	for _, tc := range cases {
		if got := Floor5(tc.in); !got.Equal(tc.want) {
			t.Errorf("Floor5(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestFloor5KeepsLocation(t *testing.T) {
	// Flooring is a wall-clock operation: a location with a non-hour UTC offset
	// must still land on a local :00/:05/... mark.
	loc := time.FixedZone("half", 30*60)
	in := time.Date(2026, 1, 2, 14, 23, 0, 0, loc)
	got := Floor5(in)
	if got.Minute() != 20 || got.Hour() != 14 || got.Location() != loc {
		t.Errorf("Floor5(%v) = %v, want 14:20 in %v", in, got, loc)
	}
}

func TestParseRelativeUsesLocationWallClock(t *testing.T) {
	// now is given in UTC but loc is offset, so the 5-minute mark is computed
	// against loc's wall clock.
	loc := time.FixedZone("plus2", 2*60*60)
	in := time.Date(2026, 1, 2, 14, 23, 0, 0, time.UTC) // 16:23 in loc
	span, err := Parse("+:20", in, loc)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	stop := span.Stop.In(loc)
	if stop.Hour() != 16 || stop.Minute() != 20 {
		t.Errorf("stop = %v, want 16:20 local", stop)
	}
	if span.Duration() != 20*time.Minute {
		t.Errorf("duration = %v, want 20m", span.Duration())
	}
}

func TestKindString(t *testing.T) {
	if got := Absolute.String(); got != "absolute" {
		t.Errorf("Absolute.String() = %q", got)
	}
	if got := Relative.String(); got != "relative" {
		t.Errorf("Relative.String() = %q", got)
	}
}
