package main

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mantas6/tg/store"
)

var update = flag.Bool("update", false, "update golden files")

// goldenMu serializes the -update rewrites. The tests run in parallel and two of
// them assert the same golden (the renderer's own output and the command's), so
// without it two goroutines could truncate and write one file at once.
var goldenMu sync.Mutex

// assertGolden compares got against testdata/<name>, rewriting it under -update.
func assertGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		goldenMu.Lock()
		defer goldenMu.Unlock()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (run with -update)", name, err)
	}
	if got != string(want) {
		t.Errorf("golden %s mismatch:\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

func TestFormatHM(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   time.Duration
		want string
	}{
		{0, "0h00m"},
		{45 * time.Minute, "0h45m"},
		{75 * time.Minute, "1h15m"},
		{120 * time.Minute, "2h00m"},
		{-time.Minute, "0h00m"},
	}
	for _, c := range cases {
		if got := formatHM(c.in); got != c.want {
			t.Errorf("formatHM(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestFormatOvertime pins the signed h:mm rendering used by `tg daily`: unlike
// formatHM it must keep the sign (so an under-run is never read as an over-run)
// and it must not clamp negatives to zero.
func TestFormatOvertime(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   time.Duration
		want string
	}{
		{0, "+0:00"},
		{30 * time.Minute, "+0:30"},
		{-30 * time.Minute, "-0:30"},
		{75 * time.Minute, "+1:15"},
		{-75 * time.Minute, "-1:15"},
		{8 * time.Hour, "+8:00"},
		{-25 * time.Hour, "-25:00"},
		// Sub-minute residue is truncated, not rounded up into a minute.
		{-30*time.Second - 90*time.Minute, "-1:30"},
		{59 * time.Second, "+0:00"},
	}
	for _, c := range cases {
		if got := formatOvertime(c.in); got != c.want {
			t.Errorf("formatOvertime(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestPlural(t *testing.T) {
	t.Parallel()
	cases := []struct {
		n    int
		want string
	}{{0, "0 days"}, {1, "1 day"}, {2, "2 days"}}
	for _, c := range cases {
		if got := plural(c.n, "day"); got != c.want {
			t.Errorf("plural(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

func TestFormatClock(t *testing.T) {
	t.Parallel()
	tm := time.Date(2026, 1, 2, 9, 15, 0, 0, time.UTC)
	if got := formatClock(tm, time.UTC); got != "09:15" {
		t.Errorf("formatClock = %q, want 09:15", got)
	}
}

// sampleDay builds the two-entry fixture used across golden tests. Seq is the
// per-day number the store hands out at insert time; renderToday prints it
// verbatim rather than counting rows, so the fixtures carry it explicitly.
func sampleDay() (entries []store.Entry, now time.Time) {
	start1 := time.Date(2026, 1, 2, 9, 15, 0, 0, time.UTC)
	stop1 := time.Date(2026, 1, 2, 10, 30, 0, 0, time.UTC)
	start2 := time.Date(2026, 1, 2, 10, 30, 0, 0, time.UTC)
	now = time.Date(2026, 1, 2, 11, 15, 0, 0, time.UTC)
	entries = []store.Entry{
		{ID: 11, Seq: 1, TaskName: "Fix login bug", ProjectName: "Backend", Start: start1, Stop: &stop1, Duration: 4500},
		{ID: 12, Seq: 2, TaskName: "Code review", ProjectName: "Backend", Start: start2, Duration: -1},
	}
	return entries, now
}

func TestRenderTodayGolden(t *testing.T) {
	t.Parallel()
	entries, now := sampleDay()
	var buf bytes.Buffer
	renderToday(&buf, entries, now, time.UTC, false)
	assertGolden(t, "today.txt", buf.String())
}

func TestRenderTodayJSONGolden(t *testing.T) {
	t.Parallel()
	entries, now := sampleDay()
	var buf bytes.Buffer
	if err := renderTodayJSON(&buf, entries, now); err != nil {
		t.Fatal(err)
	}
	assertGolden(t, "today.json", buf.String())
}

// TestRenderTodayGaps covers the filler rows between entries. now is pinned to
// the last entry's stop so the trailing gap (covered by
// TestRenderTodayTrailingGap) never contributes a "(gap ...)" row of its own.
func TestRenderTodayGaps(t *testing.T) {
	t.Parallel()
	day := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	at := func(d time.Duration) time.Time { return day.Add(d) }
	pt := func(t time.Time) *time.Time { return &t }

	cases := []struct {
		name    string
		entries []store.Entry
		want    string // substring that must appear...
		absent  bool   // ...or must not appear when true
	}{
		{
			name: "gap shown",
			entries: []store.Entry{
				{Seq: 1, TaskName: "A", Start: at(9 * time.Hour), Stop: pt(at(10 * time.Hour)), Duration: 3600},
				{Seq: 2, TaskName: "B", Start: at(10*time.Hour + 25*time.Minute), Stop: pt(at(11 * time.Hour)), Duration: 2100},
			},
			want: "               (gap 0h25m)\n",
		},
		{
			name: "gap below threshold hidden",
			entries: []store.Entry{
				{Seq: 1, TaskName: "A", Start: at(9 * time.Hour), Stop: pt(at(10 * time.Hour)), Duration: 3600},
				{Seq: 2, TaskName: "B", Start: at(10*time.Hour + 30*time.Second), Stop: pt(at(11 * time.Hour)), Duration: 3570},
			},
			want:   "(gap",
			absent: true,
		},
		{
			name: "no cross-day gap",
			entries: []store.Entry{
				{Seq: 1, TaskName: "A", Start: at(-2 * time.Hour), Stop: pt(at(-1 * time.Hour)), Duration: 3600},
				{Seq: 1, TaskName: "B", Start: at(9 * time.Hour), Stop: pt(at(10 * time.Hour)), Duration: 3600},
			},
			want:   "(gap",
			absent: true,
		},
		{
			name: "no gap after running entry",
			entries: []store.Entry{
				{Seq: 1, TaskName: "A", Start: at(9 * time.Hour), Duration: -1},
				{Seq: 2, TaskName: "B", Start: at(10 * time.Hour), Stop: pt(at(11 * time.Hour)), Duration: 3600},
			},
			want:   "(gap",
			absent: true,
		},
		{
			name: "no gap on overlap",
			entries: []store.Entry{
				{Seq: 1, TaskName: "A", Start: at(9 * time.Hour), Stop: pt(at(11 * time.Hour)), Duration: 7200},
				{Seq: 2, TaskName: "B", Start: at(10 * time.Hour), Stop: pt(at(10*time.Hour + 30*time.Minute)), Duration: 1800},
			},
			want:   "(gap",
			absent: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			// now sits exactly on the last entry's stop (or its start when it
			// is running), so only inter-entry gaps can show up.
			last := c.entries[len(c.entries)-1]
			now := last.Start
			if last.Stop != nil {
				now = *last.Stop
			}
			var buf bytes.Buffer
			renderToday(&buf, c.entries, now, time.UTC, false)
			got := buf.String()
			if c.absent {
				if strings.Contains(got, c.want) {
					t.Errorf("output contains %q, want it absent:\n%s", c.want, got)
				}
			} else if !strings.Contains(got, c.want) {
				t.Errorf("output missing %q:\n%s", c.want, got)
			}
		})
	}
}

// TestRenderTodayTrailingGap covers the closing filler row: idle time between
// the last entry's stop and now, shown with the same shape (and alignment) as
// the gaps between entries and under the same rules — nothing while the last
// entry runs, nothing below the threshold, nothing across midnight.
func TestRenderTodayTrailingGap(t *testing.T) {
	t.Parallel()
	day := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	at := func(d time.Duration) time.Time { return day.Add(d) }
	pt := func(t time.Time) *time.Time { return &t }

	finished := []store.Entry{
		{Seq: 1, TaskName: "A", Start: at(9 * time.Hour), Stop: pt(at(10 * time.Hour)), Duration: 3600},
	}
	running := []store.Entry{
		{Seq: 1, TaskName: "A", Start: at(9 * time.Hour), Duration: -1},
	}
	yesterday := []store.Entry{
		{Seq: 1, TaskName: "A", Start: at(-3 * time.Hour), Stop: pt(at(-2 * time.Hour)), Duration: 3600},
	}

	cases := []struct {
		name    string
		entries []store.Entry
		now     time.Time
		want    string
		absent  bool
	}{
		{
			name:    "trailing gap shown before divider",
			entries: finished,
			now:     at(10*time.Hour + 25*time.Minute),
			// 3 columns of blank number pad + 12 of blank clock column.
			want: strings.Repeat(" ", 15) + "(gap 0h25m)\n" + todayDivider,
		},
		{
			name:    "no trailing gap at stop",
			entries: finished,
			now:     at(10 * time.Hour),
			want:    "(gap",
			absent:  true,
		},
		{
			name:    "no trailing gap below threshold",
			entries: finished,
			now:     at(10*time.Hour + 30*time.Second),
			want:    "(gap",
			absent:  true,
		},
		{
			name:    "no trailing gap while running",
			entries: running,
			now:     at(11 * time.Hour),
			want:    "(gap",
			absent:  true,
		},
		{
			name:    "no trailing gap across midnight",
			entries: yesterday,
			now:     at(9 * time.Hour),
			want:    "(gap",
			absent:  true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			renderToday(&buf, c.entries, c.now, time.UTC, false)
			got := buf.String()
			if c.absent {
				if strings.Contains(got, c.want) {
					t.Errorf("output contains %q, want it absent:\n%s", c.want, got)
				}
			} else if !strings.Contains(got, c.want) {
				t.Errorf("output missing %q:\n%s", c.want, got)
			}
		})
	}
}

// TestRenderTodayRefNumbers pins the leading local reference numbers: each
// entry shows its own persistent per-day number, filler gap rows carry no
// number, and the column widens (right-aligned) once a number reaches two
// digits.
func TestRenderTodayRefNumbers(t *testing.T) {
	t.Parallel()
	day := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	at := func(d time.Duration) time.Time { return day.Add(d) }
	pt := func(t time.Time) *time.Time { return &t }

	// Three entries, with a gap between the second and the third.
	entries := []store.Entry{
		{Seq: 1, TaskName: "A", Start: at(9 * time.Hour), Stop: pt(at(10 * time.Hour)), Duration: 3600},
		{Seq: 2, TaskName: "B", Start: at(10 * time.Hour), Stop: pt(at(11 * time.Hour)), Duration: 3600},
		{Seq: 3, TaskName: "C", Start: at(11*time.Hour + 30*time.Minute), Stop: pt(at(12 * time.Hour)), Duration: 1800},
	}
	var buf bytes.Buffer
	renderToday(&buf, entries, at(12*time.Hour), time.UTC, false)
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	want := []string{
		"1  09:00-10:00 1h00m  A",
		"2  10:00-11:00 1h00m  B",
		"               (gap 0h30m)",
		"3  11:30-12:00 0h30m  C",
		todayDivider,
		"Total: 2h30m",
	}
	if len(lines) != len(want) {
		t.Fatalf("got %d lines, want %d:\n%s", len(lines), len(want), buf.String())
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, lines[i], want[i])
		}
	}

	// Ten entries: the number column is two wide and right-aligned, so the
	// clock column stays aligned between #9 and #10.
	var many []store.Entry
	for i := 0; i < 10; i++ {
		start := at(time.Duration(i) * time.Hour)
		stop := start.Add(time.Hour)
		many = append(many, store.Entry{Seq: i + 1, TaskName: "T", Start: start, Stop: &stop, Duration: 3600})
	}
	buf.Reset()
	renderToday(&buf, many, at(10*time.Hour), time.UTC, false)
	got := buf.String()
	for _, w := range []string{" 1  00:00-01:00", " 9  08:00-09:00", "10  09:00-10:00"} {
		if !strings.Contains(got, w+" ") && !strings.Contains(got, w) {
			t.Errorf("output missing %q:\n%s", w, got)
		}
	}
}

// TestRenderTodayNumbersAreNotPositions is the display half of the persistent
// numbering: entries print the number they were given, gaps and all, so a day
// whose entry 2 was deleted still lists 1 and 3 — and the column is sized by
// the highest number, not by how many entries are left.
func TestRenderTodayNumbersAreNotPositions(t *testing.T) {
	t.Parallel()
	day := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	at := func(d time.Duration) time.Time { return day.Add(d) }
	pt := func(t time.Time) *time.Time { return &t }

	entries := []store.Entry{
		{Seq: 1, TaskName: "A", Start: at(9 * time.Hour), Stop: pt(at(10 * time.Hour)), Duration: 3600},
		{Seq: 3, TaskName: "C", Start: at(10 * time.Hour), Stop: pt(at(11 * time.Hour)), Duration: 3600},
		{Seq: 12, TaskName: "L", Start: at(11 * time.Hour), Stop: pt(at(12 * time.Hour)), Duration: 3600},
	}
	var buf bytes.Buffer
	renderToday(&buf, entries, at(12*time.Hour), time.UTC, false)
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	want := []string{
		" 1  09:00-10:00 1h00m  A",
		" 3  10:00-11:00 1h00m  C",
		"12  11:00-12:00 1h00m  L",
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, lines[i], want[i])
		}
	}
}

// TestRenderTodayCurrentMarker pins the running-entry marker: the current
// (running) entry gets a `<` right after its number, and that marker consumes
// one of the two spaces between the number and the clock column so no other
// row shifts — the finished rows land in exactly the columns they would
// without any running entry present.
func TestRenderTodayCurrentMarker(t *testing.T) {
	t.Parallel()
	day := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	at := func(d time.Duration) time.Time { return day.Add(d) }
	pt := func(t time.Time) *time.Time { return &t }

	entries := []store.Entry{
		{Seq: 1, TaskName: "A", Start: at(9 * time.Hour), Stop: pt(at(10 * time.Hour)), Duration: 3600},
		{Seq: 2, TaskName: "B", Start: at(10 * time.Hour), Duration: -1},
	}
	var buf bytes.Buffer
	renderToday(&buf, entries, at(10*time.Hour+45*time.Minute), time.UTC, false)
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	want := []string{
		"1  09:00-10:00 1h00m  A",
		"2< 10:00-  *   0h45m  B",
		todayDivider,
		"Total: 1h45m   (* running)",
	}
	if len(lines) != len(want) {
		t.Fatalf("got %d lines, want %d:\n%s", len(lines), len(want), buf.String())
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, lines[i], want[i])
		}
	}

	// The marker must not move the clock column: the finished row's clock sits
	// at exactly the byte offset it would with no running entry at all.
	finishedClk := strings.Index(lines[0], "09:00")
	runningClk := strings.Index(lines[1], "10:00")
	if finishedClk != runningClk {
		t.Errorf("clock column shifted: finished at %d, running at %d", finishedClk, runningClk)
	}
}

// TestRenderTodayMultiDayHeaders covers the one place the flat table changes
// shape: each calendar day numbers from 1, so a listing spanning days labels
// the groups. A single-day listing gets no header at all.
func TestRenderTodayMultiDayHeaders(t *testing.T) {
	t.Parallel()
	first := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	second := time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC)
	pt := func(t time.Time) *time.Time { return &t }

	entries := []store.Entry{
		{Seq: 1, TaskName: "A", Start: first, Stop: pt(first.Add(time.Hour)), Duration: 3600},
		{Seq: 2, TaskName: "B", Start: first.Add(time.Hour), Stop: pt(first.Add(2 * time.Hour)), Duration: 3600},
		{Seq: 1, TaskName: "C", Start: second, Stop: pt(second.Add(time.Hour)), Duration: 3600},
	}
	var buf bytes.Buffer
	renderToday(&buf, entries, second.Add(time.Hour), time.UTC, false)
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	want := []string{
		"Thu 2026-01-01",
		"1  09:00-10:00 1h00m  A",
		"2  10:00-11:00 1h00m  B",
		"Fri 2026-01-02",
		"1  09:00-10:00 1h00m  C",
		todayDivider,
		"Total: 3h00m",
	}
	if len(lines) != len(want) {
		t.Fatalf("got %d lines, want %d:\n%s", len(lines), len(want), buf.String())
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, lines[i], want[i])
		}
	}

	// A single-day listing keeps the plain table.
	buf.Reset()
	renderToday(&buf, entries[:2], first.Add(2*time.Hour), time.UTC, false)
	if strings.Contains(buf.String(), "2026-01-01") {
		t.Errorf("single-day listing gained a date header:\n%s", buf.String())
	}
}

// TestRenderTodayLongNameSpacing guards the separator between the task name
// and the project bracket: names at or beyond the padding width must not run
// into "[project]".
func TestRenderTodayLongNameSpacing(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC)
	stop := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)
	now := time.Date(2026, 1, 2, 11, 0, 0, 0, time.UTC)
	entries := []store.Entry{
		{ID: 1, Seq: 1, TaskName: "A task name definitely longer than the pad", ProjectName: "Backend", Start: start, Stop: &stop, Duration: 3600},
	}
	var buf bytes.Buffer
	renderToday(&buf, entries, now, time.UTC, false)
	got := buf.String()
	if !strings.Contains(got, "A task name definitely longer than the pad [Backend]") {
		t.Errorf("missing space between task name and project:\n%s", got)
	}
}

func TestParseHexColor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in      string
		r, g, b uint8
		ok      bool
	}{
		{"#000000", 0, 0, 0, true},
		{"#ffffff", 255, 255, 255, true},
		{"#0B83D9", 11, 131, 217, true},
		{"", 0, 0, 0, false},
		{"#fff", 0, 0, 0, false},     // short form unsupported
		{"0b83d9", 0, 0, 0, false},   // missing '#'
		{"#gggggg", 0, 0, 0, false},  // bad digits
		{"#0b83d9a", 0, 0, 0, false}, // too long
	}
	for _, c := range cases {
		r, g, b, ok := parseHexColor(c.in)
		if r != c.r || g != c.g || b != c.b || ok != c.ok {
			t.Errorf("parseHexColor(%q) = (%d,%d,%d,%v), want (%d,%d,%d,%v)",
				c.in, r, g, b, ok, c.r, c.g, c.b, c.ok)
		}
	}
}

func TestColorBlock(t *testing.T) {
	t.Parallel()
	if got, want := colorBlock("#0B83D9"), "\x1b[38;2;11;131;217m\u25a0\x1b[0m"; got != want {
		t.Errorf("colorBlock = %q, want %q", got, want)
	}
	for _, bad := range []string{"", "#fff", "nope"} {
		if got := colorBlock(bad); got != "" {
			t.Errorf("colorBlock(%q) = %q, want empty", bad, got)
		}
	}
}

func TestRenderTodayColor(t *testing.T) {
	t.Parallel()
	entries, now := sampleDay()
	for i := range entries {
		entries[i].ProjectColor = "#0B83D9"
	}

	// color enabled: a tinted block leads the line, followed by a space.
	var buf bytes.Buffer
	renderToday(&buf, entries, now, time.UTC, true)
	if want := "\x1b[38;2;11;131;217m\u25a0\x1b[0m 09:15-10:30"; !strings.Contains(buf.String(), want) {
		t.Errorf("colored output missing %q:\n%q", want, buf.String())
	}

	// color disabled: plain output, no escape codes.
	buf.Reset()
	renderToday(&buf, entries, now, time.UTC, false)
	if strings.Contains(buf.String(), "\x1b") {
		t.Errorf("plain output contains ANSI escapes:\n%q", buf.String())
	}

	// invalid color: no block, no broken escapes.
	for i := range entries {
		entries[i].ProjectColor = "oops"
	}
	buf.Reset()
	renderToday(&buf, entries, now, time.UTC, true)
	if strings.Contains(buf.String(), "\x1b") {
		t.Errorf("invalid-color output contains ANSI escapes:\n%q", buf.String())
	}
}

func TestRenderTodayEmpty(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	renderToday(&buf, nil, time.Now(), time.UTC, false)
	if buf.String() != "No entries.\n" {
		t.Errorf("empty today = %q", buf.String())
	}
}

func TestRenderCurrentGolden(t *testing.T) {
	t.Parallel()
	entries, now := sampleDay()
	running := entries[1] // the running entry
	total, _ := totalDuration(entries, now)

	var human bytes.Buffer
	if err := renderCurrent(&human, &running, total, now, time.UTC, false); err != nil {
		t.Fatal(err)
	}
	assertGolden(t, "current.txt", human.String())

	var js bytes.Buffer
	if err := renderCurrent(&js, &running, total, now, time.UTC, true); err != nil {
		t.Fatal(err)
	}
	assertGolden(t, "current.json", js.String())
}

// TestRenderCurrentLastGolden covers the no-timer status line: the newest
// finished entry with its wall-clock range plus the idle gap up to now.
func TestRenderCurrentLastGolden(t *testing.T) {
	t.Parallel()
	entries, _ := sampleDay()
	last := entries[0] // 09:15-10:30
	now := time.Date(2026, 1, 2, 10, 55, 0, 0, time.UTC)
	total, _ := totalDuration(entries[:1], now)

	var human bytes.Buffer
	if err := renderCurrent(&human, &last, total, now, time.UTC, false); err != nil {
		t.Fatal(err)
	}
	assertGolden(t, "current_last.txt", human.String())

	var js bytes.Buffer
	if err := renderCurrent(&js, &last, total, now, time.UTC, true); err != nil {
		t.Fatal(err)
	}
	assertGolden(t, "current_last.json", js.String())
}

// TestRenderCurrentGapSuppressed verifies no gap is shown while now sits at (or
// within the 5-minute quantization noise of) the last entry's stop, and that the
// gap deliberately spans calendar days once it is real.
func TestRenderCurrentGapSuppressed(t *testing.T) {
	t.Parallel()
	entries, _ := sampleDay()
	last := entries[0] // stops 10:30
	stop := *last.Stop

	cases := []struct {
		name string
		now  time.Time
		want string
	}{
		{"at stop", stop, "Fix login bug [Backend] 09:15-10:30 Today: 1h15m\n"},
		{"sub-minute noise", stop.Add(30 * time.Second), "Fix login bug [Backend] 09:15-10:30 Today: 1h15m\n"},
		{"across midnight", stop.Add(20 * time.Hour), "Fix login bug [Backend] 09:15-10:30 (gap 20h00m) Today: 1h15m\n"},
	}
	for _, c := range cases {
		var buf bytes.Buffer
		if err := renderCurrent(&buf, &last, 75*time.Minute, c.now, time.UTC, false); err != nil {
			t.Fatal(err)
		}
		if buf.String() != c.want {
			t.Errorf("%s: status = %q, want %q", c.name, buf.String(), c.want)
		}
	}
}

func TestTotalDuration(t *testing.T) {
	t.Parallel()
	entries, now := sampleDay()
	total, anyRunning := totalDuration(entries, now)
	if want := 2 * time.Hour; total != want { // 1h15m stored + 0h45m live
		t.Errorf("total = %v, want %v", total, want)
	}
	if !anyRunning {
		t.Error("anyRunning = false, want true (fixture has a running entry)")
	}

	total, anyRunning = totalDuration(entries[:1], now)
	if want := 75 * time.Minute; total != want {
		t.Errorf("total = %v, want %v", total, want)
	}
	if anyRunning {
		t.Error("anyRunning = true, want false")
	}

	if total, anyRunning := totalDuration(nil, now); total != 0 || anyRunning {
		t.Errorf("empty total = %v, %v, want 0, false", total, anyRunning)
	}
}

func TestTruncName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		in       string
		max      int
		want     string
		wantRune int
	}{
		{"short unchanged", "Code review", 30, "Code review", 11},
		{"exact fits", "123456789012345678901234567890", 30, "123456789012345678901234567890", 30},
		{"overflow truncated", "This task name is definitely way too long", 30, "This task name is definitely w", 30},
		{"multibyte counted by rune", strings.Repeat("é", 40), 30, strings.Repeat("é", 30), 30},
	}
	for _, c := range cases {
		got := truncName(c.in, c.max)
		if got != c.want {
			t.Errorf("%s: truncName(%q, %d) = %q, want %q", c.name, c.in, c.max, got, c.want)
		}
		if n := len([]rune(got)); n > c.max {
			t.Errorf("%s: result %q has %d runes, exceeds max %d", c.name, got, n, c.max)
		}
	}
}

func TestRenderCurrentTruncatesName(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 1, 2, 10, 30, 0, 0, time.UTC)
	now := time.Date(2026, 1, 2, 11, 15, 0, 0, time.UTC)
	e := store.Entry{
		ID: 12, TaskName: "This task name is definitely way too long to fit on one short line",
		ProjectName: "Backend", Start: start, Duration: -1,
	}
	var buf bytes.Buffer
	if err := renderCurrent(&buf, &e, 45*time.Minute, now, time.UTC, false); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if strings.Contains(got, "since") {
		t.Errorf("status line still shows wall-clock start: %q", got)
	}
	if strings.Contains(got, "…") || strings.Contains(got, "...") {
		t.Errorf("status line still shows an ellipsis marker: %q", got)
	}
	want := "run This task name is definitely way too long to fit on one shor [Backend] (0h45m) Today: 0h45m\n"
	if got != want {
		t.Errorf("status line = %q, want %q", got, want)
	}
}

// A name at or under the (doubled) status cap must pass through untouched.
func TestRenderCurrentKeepsNameUnderNewLimit(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 1, 2, 10, 30, 0, 0, time.UTC)
	now := time.Date(2026, 1, 2, 11, 15, 0, 0, time.UTC)
	name := "This task name is definitely way too long" // 41 runes: fit under 60
	e := store.Entry{
		ID: 12, TaskName: name,
		ProjectName: "Backend", Start: start, Duration: -1,
	}
	var buf bytes.Buffer
	if err := renderCurrent(&buf, &e, 45*time.Minute, now, time.UTC, false); err != nil {
		t.Fatal(err)
	}
	want := "run " + name + " [Backend] (0h45m) Today: 0h45m\n"
	if got := buf.String(); got != want {
		t.Errorf("status line = %q, want %q", got, want)
	}
}

// sampleTasks builds the catalog-listing fixture (project name joined),
// pre-sorted by project then task name as ListTasks returns them.
func sampleTasks() []store.Task {
	return []store.Task{
		{ID: 12, Name: "Code review", ProjectName: "Backend", Active: true},
		{ID: 10, Name: "Fix login bug", ProjectName: "Backend", Active: true},
		{ID: 20, Name: "Payment fix", ProjectName: "Payments", Active: true},
	}
}

func TestRenderTasksGolden(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	renderTasks(&buf, sampleTasks())
	assertGolden(t, "tasks.txt", buf.String())
}

func TestRenderTasksJSONGolden(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := renderTasksJSON(&buf, sampleTasks()); err != nil {
		t.Fatal(err)
	}
	assertGolden(t, "tasks.json", buf.String())
}

func TestRenderTasksEmpty(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	renderTasks(&buf, nil)
	if !strings.Contains(buf.String(), "tg update") {
		t.Errorf("empty tasks = %q, want hint to run `tg update`", buf.String())
	}
}

// sampleProjects builds the project-listing fixture, pre-sorted by name as
// ListProjects returns them.
func sampleProjects() []store.Project {
	return []store.Project{
		{ID: 1, Name: "Backend", Active: true},
		{ID: 2, Name: "Payments", ClientName: "Acme", Active: true, Billable: true},
	}
}

func TestRenderProjectsGolden(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	renderProjects(&buf, sampleProjects())
	assertGolden(t, "projects.txt", buf.String())
}

func TestRenderProjectsJSONGolden(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := renderProjectsJSON(&buf, sampleProjects()); err != nil {
		t.Fatal(err)
	}
	assertGolden(t, "projects.json", buf.String())
}

func TestRenderProjectsEmpty(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	renderProjects(&buf, nil)
	if !strings.Contains(buf.String(), "tg projects update") {
		t.Errorf("empty projects = %q, want hint to run `tg projects update`", buf.String())
	}
}

// sampleTotals builds the `tg total` fixture for a single fragment: the one
// group cmdTotal hands renderTotals after joining the report rows to the
// catalog, already sorted by task name.
func sampleTotals() []totalGroup {
	return []totalGroup{{Fragment: "write", Rows: []totalRow{
		{TaskID: 14, Name: "Write docs", ProjectName: "Backend", Seconds: 900},
		{TaskID: 13, Name: "Write tests", ProjectName: "Backend", Seconds: 3600},
	}}}
}

// sampleTotalFragments builds the multi-fragment fixture (`tg total write docs
// legacy`): the first two fragments deliberately overlap (both match "Write
// docs"), so the goldens pin the per-fragment headers AND the footer counting
// the shared task once, and the third holds a task the local catalog does not
// know, which has no project to show.
func sampleTotalFragments() []totalGroup {
	return append(sampleTotals(),
		totalGroup{Fragment: "docs", Rows: []totalRow{
			{TaskID: 14, Name: "Write docs", ProjectName: "Backend", Seconds: 900},
		}},
		totalGroup{Fragment: "legacy", Rows: []totalRow{
			{TaskID: 98, Name: "Legacy work", Seconds: 600},
		}},
	)
}

func TestRenderTotalsGolden(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	renderTotals(&buf, sampleTotals())
	assertGolden(t, "total.txt", buf.String())
}

func TestRenderTotalsJSONGolden(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := renderTotalsJSON(&buf, sampleTotals()); err != nil {
		t.Fatal(err)
	}
	assertGolden(t, "total.json", buf.String())
}

// TestRenderTotalsFragmentsGolden pins the multi-fragment report: a header per
// fragment with its own total, its tasks indented beneath it, and a footer that
// totals the DISTINCT tasks (0h15m + 1h00m + 0h10m), not the sum of the headers.
func TestRenderTotalsFragmentsGolden(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	renderTotals(&buf, sampleTotalFragments())
	assertGolden(t, "total_fragments.txt", buf.String())
}

func TestRenderTotalsFragmentsJSONGolden(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := renderTotalsJSON(&buf, sampleTotalFragments()); err != nil {
		t.Fatal(err)
	}
	assertGolden(t, "total_fragments.json", buf.String())
}

// TestRenderTotalsSingleFragmentIsUngrouped pins the single-fragment shape: one
// fragment needs no header to tell it apart, so its rows are neither labelled
// nor indented (the same way renderToday only dates a multi-day listing).
func TestRenderTotalsSingleFragmentIsUngrouped(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	renderTotals(&buf, sampleTotals())
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if strings.HasPrefix(line, " ") {
			t.Errorf("line %q is indented, want the ungrouped single-fragment shape", line)
		}
	}
	if strings.Contains(buf.String(), "write  ") {
		t.Errorf("single fragment should carry no header:\n%s", buf.String())
	}
}

func TestRenderTotalsEmpty(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	renderTotals(&buf, nil)
	if !strings.Contains(buf.String(), "No matching tasks.") {
		t.Errorf("empty totals = %q, want \"No matching tasks.\"", buf.String())
	}
}

func TestRenderCurrentNoneGolden(t *testing.T) {
	t.Parallel()
	var human bytes.Buffer
	if err := renderCurrent(&human, nil, 0, time.Now(), time.UTC, false); err != nil {
		t.Fatal(err)
	}
	assertGolden(t, "current_none.txt", human.String())

	var js bytes.Buffer
	if err := renderCurrent(&js, nil, 0, time.Now(), time.UTC, true); err != nil {
		t.Fatal(err)
	}
	assertGolden(t, "current_none.json", js.String())
}

// sampleMonth builds the `tg daily` fixture: three worked days, one over the
// 8h target, one under it, and one exactly on target that is still running.
// now sits on the last of them, so none of the rows is in the future.
func sampleMonth() (rows []dailyRow, now time.Time) {
	day := func(d int) time.Time { return time.Date(2026, 1, d, 0, 0, 0, 0, time.UTC) }
	now = time.Date(2026, 1, 7, 17, 0, 0, 0, time.UTC)
	rows = []dailyRow{
		{Day: day(5), Tracked: 8*time.Hour + 30*time.Minute},
		{Day: day(6), Tracked: 7*time.Hour + 15*time.Minute},
		{Day: day(7), Tracked: 8 * time.Hour, Running: true},
	}
	return rows, now
}

func TestRenderDailyGolden(t *testing.T) {
	t.Parallel()
	rows, now := sampleMonth()
	var buf bytes.Buffer
	renderDaily(&buf, rows, now, 8*time.Hour, time.UTC, false)
	assertGolden(t, "daily.txt", buf.String())
}

func TestRenderDailyJSONGolden(t *testing.T) {
	t.Parallel()
	rows, _ := sampleMonth()
	var buf bytes.Buffer
	if err := renderDailyJSON(&buf, rows, 8*time.Hour, time.UTC); err != nil {
		t.Fatal(err)
	}
	assertGolden(t, "daily.json", buf.String())
}

// TestRenderDailyOvertimeColumn pins the per-day arithmetic: each line's third
// column is tracked-minus-target, signed, and the divider footer sums the days
// and measures them against target x the number of LISTED days.
func TestRenderDailyOvertimeColumn(t *testing.T) {
	t.Parallel()
	rows, now := sampleMonth()
	var buf bytes.Buffer
	renderDaily(&buf, rows, now, 8*time.Hour, time.UTC, false)
	out := buf.String()
	for _, want := range []string{
		"Mon 2026-01-05  8h30m   +0:30\n",
		"Tue 2026-01-06  7h15m   -0:45\n",
		"Wed 2026-01-07  8h00m*  +0:00\n",
		todayDivider + "\n",
		// 23h45m tracked against a 3 x 8h = 24h target.
		"Total: 23h45m  -0:15  (3 days x 8h00m)",
		// A running day keeps the day total live, so the footer says so.
		"(* running)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("renderDaily output missing %q:\n%s", want, out)
		}
	}
}

// TestRenderDailyTargetChangesOnlyOvertime pins that --target moves the
// overtime column (and the footer's target) without touching the tracked
// durations, and that a non-integer target works.
func TestRenderDailyTargetChangesOnlyOvertime(t *testing.T) {
	t.Parallel()
	rows, now := sampleMonth()
	var buf bytes.Buffer
	renderDaily(&buf, rows, now, 7*time.Hour+30*time.Minute, time.UTC, false)
	out := buf.String()
	for _, want := range []string{
		"Mon 2026-01-05  8h30m   +1:00\n",
		"Tue 2026-01-06  7h15m   -0:15\n",
		"Wed 2026-01-07  8h00m*  +0:30\n",
		// 23h45m against 3 x 7h30m = 22h30m.
		"Total: 23h45m  +1:15  (3 days x 7h30m)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("renderDaily(target 7h30m) missing %q:\n%s", want, out)
		}
	}
}

// TestRenderDailyZeroTarget covers the degenerate target: with no target every
// overtime figure is just the tracked time, and nothing is ever negative.
func TestRenderDailyZeroTarget(t *testing.T) {
	t.Parallel()
	rows, now := sampleMonth()
	var buf bytes.Buffer
	renderDaily(&buf, rows, now, 0, time.UTC, false)
	out := buf.String()
	if strings.Contains(out, "-") && !strings.Contains(out, "2026-01") {
		t.Fatalf("unexpected negative overtime:\n%s", out)
	}
	for _, want := range []string{"+8:30", "+7:15", "+8:00", "Total: 23h45m  +23:45  (3 days x 0h00m)"} {
		if !strings.Contains(out, want) {
			t.Errorf("renderDaily(target 0) missing %q:\n%s", want, out)
		}
	}
}

// TestRenderDailySingleDayFooter covers the footer's singular noun and the fact
// that the footer target scales with the number of listed days.
func TestRenderDailySingleDayFooter(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	rows := []dailyRow{{Day: time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC), Tracked: 9 * time.Hour}}
	renderDaily(&buf, rows, time.Date(2026, 1, 5, 18, 0, 0, 0, time.UTC), 8*time.Hour, time.UTC, false)
	if want := "Total: 9h00m   +1:00  (1 day x 8h00m)\n"; !strings.Contains(buf.String(), want) {
		t.Errorf("renderDaily footer = %q, want it to contain %q", buf.String(), want)
	}
	if strings.Contains(buf.String(), "running") {
		t.Errorf("no entry is running, footer should not say so:\n%s", buf.String())
	}
}

func TestRenderDailyEmpty(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	renderDaily(&buf, nil, time.Now(), 8*time.Hour, time.UTC, false)
	if got := buf.String(); got != "No entries this month.\n" {
		t.Errorf("empty daily = %q", got)
	}
}

// TestRenderDailyJSONEmpty pins that the JSON shape stays a well-formed object
// with an empty (never null) days array when nothing was tracked.
func TestRenderDailyJSONEmpty(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := renderDailyJSON(&buf, nil, 8*time.Hour, time.UTC); err != nil {
		t.Fatal(err)
	}
	want := `{"days":[],"total_seconds":0,"target_seconds":28800,"overtime_seconds":0}` + "\n"
	if got := buf.String(); got != want {
		t.Errorf("empty daily json = %q, want %q", got, want)
	}
}

// TestRenderDailyLocalDates pins that the date column is rendered in the
// reporting location, not UTC: a day whose midnight is stored in a UTC-offset
// zone must still print its own calendar date.
func TestRenderDailyLocalDates(t *testing.T) {
	t.Parallel()
	loc := time.FixedZone("UTC+3", 3*60*60)
	rows := []dailyRow{{Day: time.Date(2026, 1, 5, 0, 0, 0, 0, loc), Tracked: 8 * time.Hour}}
	now := time.Date(2026, 1, 5, 18, 0, 0, 0, loc)
	var human bytes.Buffer
	renderDaily(&human, rows, now, 8*time.Hour, loc, false)
	if !strings.Contains(human.String(), "Mon 2026-01-05") {
		t.Errorf("daily = %q, want the local date", human.String())
	}
	var js bytes.Buffer
	if err := renderDailyJSON(&js, rows, 8*time.Hour, loc); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(js.String(), `"date":"2026-01-05"`) {
		t.Errorf("daily json = %q, want the local date", js.String())
	}
}

func TestFaint(t *testing.T) {
	t.Parallel()
	if got, want := faint("x"), "\x1b[2mx\x1b[0m"; got != want {
		t.Errorf("faint = %q, want %q", got, want)
	}
}

// TestRenderDailyGreysFutureDays pins the dimming of upcoming days: a day after
// today (time booked ahead) is wrapped in the ANSI faint attribute, while today
// and the days already behind stay plain — so the listing separates worked time
// from planned time. Nothing is styled when color is off.
func TestRenderDailyGreysFutureDays(t *testing.T) {
	t.Parallel()
	day := func(d int) time.Time { return time.Date(2026, 1, d, 0, 0, 0, 0, time.UTC) }
	rows := []dailyRow{
		{Day: day(5), Tracked: 8 * time.Hour}, // yesterday
		{Day: day(6), Tracked: 7 * time.Hour}, // today
		{Day: day(7), Tracked: 6 * time.Hour}, // tomorrow
	}
	now := time.Date(2026, 1, 6, 12, 0, 0, 0, time.UTC)

	var buf bytes.Buffer
	renderDaily(&buf, rows, now, 8*time.Hour, time.UTC, true)
	out := buf.String()
	for _, want := range []string{
		"Mon 2026-01-05  8h00m   +0:00\n",
		"Tue 2026-01-06  7h00m   -1:00\n",
		faint("Wed 2026-01-07  6h00m   -2:00") + "\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("renderDaily output missing %q:\n%q", want, out)
		}
	}
	// Only the one upcoming line is styled: the divider and the footer belong to
	// no particular day, so they carry no escapes (faint opens and resets one).
	if got, want := strings.Count(out, "\x1b"), 2; got != want {
		t.Errorf("renderDaily emitted %d escapes, want %d:\n%q", got, want, out)
	}

	// color disabled: plain output, upcoming day included.
	buf.Reset()
	renderDaily(&buf, rows, now, 8*time.Hour, time.UTC, false)
	if strings.Contains(buf.String(), "\x1b") {
		t.Errorf("plain output contains ANSI escapes:\n%q", buf.String())
	}
}

// TestRenderDailyFutureDayUsesLocalCalendar pins that "upcoming" is a calendar-
// day question in the reporting location, not a raw instant comparison: just
// after local midnight, today's row is still today (its midnight is behind now)
// and only the next day is dimmed, even though both sit ahead of now in UTC.
func TestRenderDailyFutureDayUsesLocalCalendar(t *testing.T) {
	t.Parallel()
	loc := time.FixedZone("UTC+3", 3*60*60)
	rows := []dailyRow{
		{Day: time.Date(2026, 1, 5, 0, 0, 0, 0, loc), Tracked: 8 * time.Hour},
		{Day: time.Date(2026, 1, 6, 0, 0, 0, 0, loc), Tracked: 8 * time.Hour},
	}
	now := time.Date(2026, 1, 5, 0, 30, 0, 0, loc)
	var buf bytes.Buffer
	renderDaily(&buf, rows, now, 8*time.Hour, loc, true)
	out := buf.String()
	if want := "Mon 2026-01-05  8h00m   +0:00\n"; !strings.Contains(out, want) {
		t.Errorf("today's row should be plain, want %q:\n%q", want, out)
	}
	if want := faint("Tue 2026-01-06  8h00m   +0:00") + "\n"; !strings.Contains(out, want) {
		t.Errorf("tomorrow's row should be dimmed, want %q:\n%q", want, out)
	}
}
