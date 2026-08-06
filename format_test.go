package main

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mantas6/tg/store"
)

var update = flag.Bool("update", false, "update golden files")

// assertGolden compares got against testdata/<name>, rewriting it under -update.
func assertGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
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

func TestFormatClock(t *testing.T) {
	tm := time.Date(2026, 1, 2, 9, 15, 0, 0, time.UTC)
	if got := formatClock(tm, time.UTC); got != "09:15" {
		t.Errorf("formatClock = %q, want 09:15", got)
	}
}

// sampleDay builds the two-entry fixture used across golden tests.
func sampleDay() (entries []store.Entry, now time.Time) {
	start1 := time.Date(2026, 1, 2, 9, 15, 0, 0, time.UTC)
	stop1 := time.Date(2026, 1, 2, 10, 30, 0, 0, time.UTC)
	start2 := time.Date(2026, 1, 2, 10, 30, 0, 0, time.UTC)
	now = time.Date(2026, 1, 2, 11, 15, 0, 0, time.UTC)
	entries = []store.Entry{
		{ID: 11, TaskName: "Fix login bug", ProjectName: "Backend", Start: start1, Stop: &stop1, Duration: 4500},
		{ID: 12, TaskName: "Code review", ProjectName: "Backend", Start: start2, Duration: -1},
	}
	return entries, now
}

func TestRenderTodayGolden(t *testing.T) {
	entries, now := sampleDay()
	var buf bytes.Buffer
	renderToday(&buf, entries, now, time.UTC, false)
	assertGolden(t, "today.txt", buf.String())
}

func TestRenderTodayJSONGolden(t *testing.T) {
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
				{TaskName: "A", Start: at(9 * time.Hour), Stop: pt(at(10 * time.Hour)), Duration: 3600},
				{TaskName: "B", Start: at(10*time.Hour + 25*time.Minute), Stop: pt(at(11 * time.Hour)), Duration: 2100},
			},
			want: "               (gap 0h25m)\n",
		},
		{
			name: "gap below threshold hidden",
			entries: []store.Entry{
				{TaskName: "A", Start: at(9 * time.Hour), Stop: pt(at(10 * time.Hour)), Duration: 3600},
				{TaskName: "B", Start: at(10*time.Hour + 30*time.Second), Stop: pt(at(11 * time.Hour)), Duration: 3570},
			},
			want:   "(gap",
			absent: true,
		},
		{
			name: "no cross-day gap",
			entries: []store.Entry{
				{TaskName: "A", Start: at(-2 * time.Hour), Stop: pt(at(-1 * time.Hour)), Duration: 3600},
				{TaskName: "B", Start: at(9 * time.Hour), Stop: pt(at(10 * time.Hour)), Duration: 3600},
			},
			want:   "(gap",
			absent: true,
		},
		{
			name: "no gap after running entry",
			entries: []store.Entry{
				{TaskName: "A", Start: at(9 * time.Hour), Duration: -1},
				{TaskName: "B", Start: at(10 * time.Hour), Stop: pt(at(11 * time.Hour)), Duration: 3600},
			},
			want:   "(gap",
			absent: true,
		},
		{
			name: "no gap on overlap",
			entries: []store.Entry{
				{TaskName: "A", Start: at(9 * time.Hour), Stop: pt(at(11 * time.Hour)), Duration: 7200},
				{TaskName: "B", Start: at(10 * time.Hour), Stop: pt(at(10*time.Hour + 30*time.Minute)), Duration: 1800},
			},
			want:   "(gap",
			absent: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
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
	day := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	at := func(d time.Duration) time.Time { return day.Add(d) }
	pt := func(t time.Time) *time.Time { return &t }

	finished := []store.Entry{
		{TaskName: "A", Start: at(9 * time.Hour), Stop: pt(at(10 * time.Hour)), Duration: 3600},
	}
	running := []store.Entry{
		{TaskName: "A", Start: at(9 * time.Hour), Duration: -1},
	}
	yesterday := []store.Entry{
		{TaskName: "A", Start: at(-3 * time.Hour), Stop: pt(at(-2 * time.Hour)), Duration: 3600},
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

// TestRenderTodayRefNumbers pins the leading local reference numbers: entries
// are numbered 1..N in display order, filler gap rows carry no number, and the
// column widens (right-aligned) once the listing reaches ten entries.
func TestRenderTodayRefNumbers(t *testing.T) {
	day := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	at := func(d time.Duration) time.Time { return day.Add(d) }
	pt := func(t time.Time) *time.Time { return &t }

	// Three entries, with a gap between the second and the third.
	entries := []store.Entry{
		{TaskName: "A", Start: at(9 * time.Hour), Stop: pt(at(10 * time.Hour)), Duration: 3600},
		{TaskName: "B", Start: at(10 * time.Hour), Stop: pt(at(11 * time.Hour)), Duration: 3600},
		{TaskName: "C", Start: at(11*time.Hour + 30*time.Minute), Stop: pt(at(12 * time.Hour)), Duration: 1800},
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
		many = append(many, store.Entry{TaskName: "T", Start: start, Stop: &stop, Duration: 3600})
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

// TestRenderTodayLongNameSpacing guards the separator between the task name
// and the project bracket: names at or beyond the padding width must not run
// into "[project]".
func TestRenderTodayLongNameSpacing(t *testing.T) {
	start := time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC)
	stop := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)
	now := time.Date(2026, 1, 2, 11, 0, 0, 0, time.UTC)
	entries := []store.Entry{
		{ID: 1, TaskName: "A task name definitely longer than the pad", ProjectName: "Backend", Start: start, Stop: &stop, Duration: 3600},
	}
	var buf bytes.Buffer
	renderToday(&buf, entries, now, time.UTC, false)
	got := buf.String()
	if !strings.Contains(got, "A task name definitely longer than the pad [Backend]") {
		t.Errorf("missing space between task name and project:\n%s", got)
	}
}

func TestParseHexColor(t *testing.T) {
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
	var buf bytes.Buffer
	renderToday(&buf, nil, time.Now(), time.UTC, false)
	if buf.String() != "No entries.\n" {
		t.Errorf("empty today = %q", buf.String())
	}
}

func TestRenderCurrentGolden(t *testing.T) {
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
	cases := []struct {
		name     string
		in       string
		max      int
		want     string
		wantRune int
	}{
		{"short unchanged", "Code review", 30, "Code review", 11},
		{"exact fits", "123456789012345678901234567890", 30, "123456789012345678901234567890", 30},
		{"overflow truncated", "This task name is definitely way too long", 30, "This task name is definitely …", 30},
		{"multibyte counted by rune", strings.Repeat("é", 40), 30, strings.Repeat("é", 29) + "…", 30},
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
	start := time.Date(2026, 1, 2, 10, 30, 0, 0, time.UTC)
	now := time.Date(2026, 1, 2, 11, 15, 0, 0, time.UTC)
	e := store.Entry{
		ID: 12, TaskName: "This task name is definitely way too long",
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
	want := "run This task name is definitely … [Backend] (0h45m) Today: 0h45m\n"
	if got != want {
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
	var buf bytes.Buffer
	renderTasks(&buf, sampleTasks())
	assertGolden(t, "tasks.txt", buf.String())
}

func TestRenderTasksJSONGolden(t *testing.T) {
	var buf bytes.Buffer
	if err := renderTasksJSON(&buf, sampleTasks()); err != nil {
		t.Fatal(err)
	}
	assertGolden(t, "tasks.json", buf.String())
}

func TestRenderTasksEmpty(t *testing.T) {
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
	var buf bytes.Buffer
	renderProjects(&buf, sampleProjects())
	assertGolden(t, "projects.txt", buf.String())
}

func TestRenderProjectsJSONGolden(t *testing.T) {
	var buf bytes.Buffer
	if err := renderProjectsJSON(&buf, sampleProjects()); err != nil {
		t.Fatal(err)
	}
	assertGolden(t, "projects.json", buf.String())
}

func TestRenderProjectsEmpty(t *testing.T) {
	var buf bytes.Buffer
	renderProjects(&buf, nil)
	if !strings.Contains(buf.String(), "tg projects update") {
		t.Errorf("empty projects = %q, want hint to run `tg projects update`", buf.String())
	}
}

func TestRenderCurrentNoneGolden(t *testing.T) {
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
