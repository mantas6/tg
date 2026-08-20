package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/mantas6/tg/store"
)

// dateLayout is the single calendar-date layout tg's interface uses: the shape
// `--since` is parsed in (see parseSinceFlag) and the shape dates are rendered
// in (day headers, JSON output, the Reports API's date range), so what tg
// prints is always what it accepts back.
const dateLayout = "2006-01-02"

// formatHM renders a duration as "<h>h<mm>m" (e.g. 75m -> "1h15m", 50m ->
// "0h50m"). Negative durations clamp to zero.
func formatHM(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	total := int(d / time.Minute)
	return fmt.Sprintf("%dh%02dm", total/60, total%60)
}

// formatOvertime renders a signed difference from a target as "<sign><h>:<mm>"
// (e.g. +0:30, -1:15, +0:00 for exactly on target). The sign is always present
// so an over- and an under-run can never be confused, and the minutes are
// always two digits so the column lines up. Unlike formatHM this must keep the
// sign, which is why it does not clamp negatives; the value is truncated to
// whole minutes.
func formatOvertime(d time.Duration) string {
	sign := "+"
	if d < 0 {
		sign, d = "-", -d
	}
	total := int(d / time.Minute)
	return fmt.Sprintf("%s%d:%02d", sign, total/60, total%60)
}

// formatClock renders the wall-clock time (HH:MM) in loc.
func formatClock(t time.Time, loc *time.Location) string {
	return t.In(loc).Format("15:04")
}

// gapLabel names a gap entry (see store.Entry.Gap) wherever an entry is named:
// the `tg ls` line, the status line, the one-line confirmations and the overlap
// errors. It is parenthesized so it can never be read as a task actually called
// "gap", and it is the one spelling of the marker, so all of those agree.
const gapLabel = "(gap)"

// entryLabel is the task name, falling back to the free-form description.
//
// A gap entry has neither by construction (`tg gap` records no task and no
// description), so it is named by the marker itself; a description later put on
// one with `tg mod --desc lunch` is appended, since it says what the gap is for
// and the marker still identifies it as one.
func entryLabel(e store.Entry) string {
	if e.Gap {
		if e.Description != "" {
			return gapLabel + " " + e.Description
		}
		return gapLabel
	}
	if e.TaskName != "" {
		return e.TaskName
	}
	return e.Description
}

// overlapLabel identifies a conflicting entry in an error message: its
// wall-clock range in loc followed by its task name (or description). A running
// entry has no stop, so its range reads "HH:MM-running".
func overlapLabel(e store.Entry, loc *time.Location) string {
	rng := formatClock(e.Start, loc) + "-"
	if e.Running() {
		rng += "running"
	} else {
		rng += formatClock(*e.Stop, loc)
	}
	if label := entryLabel(e); label != "" {
		return rng + " " + label
	}
	return rng
}

// dayNote names the calendar day a changed entry sits on (" on 2026-08-25")
// when that is not now's day in loc, and nothing at all when it is. It is the
// suffix the one-line confirmations of `add`, `mod` and `del` carry: their
// wall-clock times alone stopped identifying an entry once `--date` let one live
// on another day, while the ordinary case — an entry on today — stays as terse
// as it was.
func dayNote(start, now time.Time, loc *time.Location) string {
	if sameDay(start, now, loc) {
		return ""
	}
	return " on " + start.In(loc).Format(dateLayout)
}

// renderEntryChange writes the one-line confirmation for a command that changed
// exactly one entry (`mod`, `del`), mirroring `add`'s "Added: ..." shape:
//
//	Modified: Fix login bug [Backend]  09:00-09:30 (0h30m)
//
// verb is the past-tense action ("Modified", "Deleted"). A running entry has no
// stop, so its range reads "HH:MM-running" and no duration is shown (the stored
// duration is the -1 running marker, not a length).
//
// now is the real clock, only so an entry that is not on today's day says which
// day it is on (see dayNote); it never changes what is rendered for an entry on
// today.
func renderEntryChange(w io.Writer, verb string, e store.Entry, now time.Time, loc *time.Location) {
	label := entryLabel(e)
	if e.ProjectName != "" {
		label += " [" + e.ProjectName + "]"
	}
	day := dayNote(e.Start, now, loc)
	if e.Stop == nil {
		fmt.Fprintf(w, "%s: %s  %s-running%s\n", verb, label, formatClock(e.Start, loc), day)
		return
	}
	fmt.Fprintf(w, "%s: %s  %s-%s (%s)%s\n", verb, label,
		formatClock(e.Start, loc), formatClock(*e.Stop, loc),
		formatHM(time.Duration(e.Duration)*time.Second), day)
}

// statusNameMax caps the task name shown by `status`/`current` so the terse
// status line stays short enough for a status bar.
const statusNameMax = 60

// truncName hard-cuts s to at most limit runes. No ellipsis marker is appended,
// so the result is a plain prefix of s and never exceeds limit runes.
func truncName(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit])
}

// displayDuration is the duration shown for an entry: live (un-rounded) elapsed
// while running, otherwise the stored quantized duration.
func displayDuration(e store.Entry, now time.Time) time.Duration {
	if e.Stop == nil {
		return now.Sub(e.Start)
	}
	return time.Duration(e.Duration) * time.Second
}

const todayDivider = "----------------------------------------"

// totalDuration sums the displayed durations of entries (live elapsed for a
// running entry, the stored duration otherwise) and reports whether any of them
// is still running. It is the shared tracked-time total behind `today`'s footer
// and `status`'s day total.
//
// Gap entries are left out: a gap is time deliberately NOT tracked (see
// store.Entry.Gap), so counting it would inflate every total tg reports and put
// them at odds with Toggl's, which never sees a gap at all. gapDuration reports
// that time separately, which is what the listing's footer notes.
func totalDuration(entries []store.Entry, now time.Time) (total time.Duration, anyRunning bool) {
	for _, e := range entries {
		if e.Gap {
			continue
		}
		total += displayDuration(e, now)
		if e.Stop == nil {
			anyRunning = true
		}
	}
	return total, anyRunning
}

// gapDuration sums the spans held by the gap entries among entries, i.e. the
// time the listing accounts for without tracking it (see totalDuration). It is 0
// when there are no gaps, which is what keeps the footer note out of an ordinary
// listing.
func gapDuration(entries []store.Entry) time.Duration {
	var total time.Duration
	for _, e := range entries {
		if e.Gap {
			total += time.Duration(e.Duration) * time.Second
		}
	}
	return total
}

// parseHexColor parses a "#RRGGBB" hex color (as stored on projects) into its
// 8-bit channels. ok is false for any other shape (empty, short, bad digits).
func parseHexColor(s string) (r, g, b uint8, ok bool) {
	if len(s) != 7 || s[0] != '#' {
		return 0, 0, 0, false
	}
	v, err := strconv.ParseUint(s[1:], 16, 32)
	if err != nil {
		return 0, 0, 0, false
	}
	return uint8(v >> 16), uint8(v >> 8), uint8(v), true
}

// colorBlock renders a small block tinted with the given "#RRGGBB" color via a
// 24-bit ANSI foreground escape, reset afterwards. Missing or malformed colors
// yield "" so callers never emit broken escape codes.
func colorBlock(hex string) string {
	r, g, b, ok := parseHexColor(hex)
	if !ok {
		return ""
	}
	return fmt.Sprintf("\x1b[38;2;%d;%d;%dm\u25a0\x1b[0m", r, g, b)
}

// faint wraps s in the ANSI faint (dim) attribute, reset afterwards, so it
// renders greyed out next to normal text. Like colorBlock it emits escapes
// unconditionally, so callers must only reach for it when the output is a
// terminal (the `color` flag on renderToday / renderDaily).
func faint(s string) string {
	return "\x1b[2m" + s + "\x1b[0m"
}

// gapThreshold is the smallest distance rendered as a gap (in the daily table
// and in the status line). Entry times land on whole minutes at best, so a
// sub-minute distance is rounding noise rather than real idle time.
const gapThreshold = time.Minute

// gapBetween returns the idle time between prev and next worth showing, or 0.
// No gap is reported when prev is still running, when the entries overlap or
// sit closer than gapThreshold, or when they fall on different calendar days
// in loc (a "gap" across midnight is just the night, not tracked idle time).
func gapBetween(prev, next store.Entry, loc *time.Location) time.Duration {
	if prev.Stop == nil {
		return 0
	}
	gap := next.Start.Sub(*prev.Stop)
	if gap < gapThreshold {
		return 0
	}
	ps, ns := prev.Stop.In(loc), next.Start.In(loc)
	py, pm, pd := ps.Date()
	ny, nm, nd := ns.Date()
	if py != ny || pm != nm || pd != nd {
		return 0
	}
	return gap
}

// trailingGap returns the idle time between the last listed entry's stop and
// now worth showing at the bottom of the daily table, or 0. It is deliberately
// gapBetween against a zero-length entry starting at now, so the trailing gap
// obeys exactly the same rules as the gaps between entries: nothing for a
// running last entry, nothing below gapThreshold, and nothing across midnight
// (the daily table is a today view; the cross-day case is `tg status`'s job).
func trailingGap(last store.Entry, now time.Time, loc *time.Location) time.Duration {
	return gapBetween(last, store.Entry{Start: now}, loc)
}

// refWidth is the width of the leading local-number column for a listing whose
// highest entry number is `highest`: enough digits to hold it, so the numbers
// stay right-aligned. A listing with no numbers still gets a single column.
func refWidth(highest int) int {
	if highest < 1 {
		highest = 1
	}
	return len(strconv.Itoa(highest))
}

// maxSeq returns the highest per-day number among entries, which is what sizes
// the number column. It is not len(entries): numbers are persistent, so a day
// that has had entries deleted lists fewer entries than its highest number.
func maxSeq(entries []store.Entry) int {
	highest := 0
	for _, e := range entries {
		if e.Seq > highest {
			highest = e.Seq
		}
	}
	return highest
}

// dayHeader labels a day group in a multi-day listing (see renderToday).
func dayHeader(t time.Time, loc *time.Location) string {
	return t.In(loc).Format("Mon " + dateLayout)
}

// spansDays reports whether entries fall on more than one calendar day in loc,
// which is what turns on the date headers in renderToday.
func spansDays(entries []store.Entry, loc *time.Location) bool {
	for i := 1; i < len(entries); i++ {
		if dayHeader(entries[i].Start, loc) != dayHeader(entries[i-1].Start, loc) {
			return true
		}
	}
	return false
}

// renderToday writes the human-readable daily table to w. color enables the
// per-project ANSI color block and should only be set when w is a terminal.
//
// Every entry line leads with the entry's own per-day number (store.Entry.Seq,
// assigned when it was inserted) so it can be addressed later as `tg mod 2` /
// `tg del 3`. Those numbers are persistent and start again at 1 each calendar
// day, so a listing can legitimately show gaps (an entry was deleted) and a
// multi-day listing repeats numbers; the latter is grouped under a date header
// so it stays readable. Filler rows (the gaps between entries and the trailing
// gap up to now) are not entries and carry no number.
//
// Three things are therefore told apart in the same table:
//
//   - a tracked entry, which reads as its number, range, duration and task;
//   - a GAP ENTRY (`tg gap`, see store.Entry.Gap), which is a real entry and so
//     keeps its number and range, but is labelled with the marker instead of a
//     task and is dimmed when color is on: its span is accounted for, not
//     tracked, so it is left out of the total and noted separately in the footer;
//   - an untracked hole, the filler row, which is no entry at all: no number, and
//     its "(gap H:MM)" text sits in the duration column.
func renderToday(w io.Writer, entries []store.Entry, now time.Time, loc *time.Location, color bool) {
	if len(entries) == 0 {
		fmt.Fprintln(w, "No entries.")
		return
	}

	// leadPad is the blank lead-in that matches the width of a color block plus
	// its trailing space (the block is one display column). It keeps lines
	// without a project color aligned with colored ones in color mode, and is
	// empty when color is off so plain output carries no stray indentation.
	leadPad := ""
	if color {
		leadPad = "  "
	}

	// numWidth sizes the reference-number column; numPad blanks it out on the
	// filler rows so their "(gap ...)" text stays aligned with the entries.
	numWidth := refWidth(maxSeq(entries))
	numPad := strings.Repeat(" ", numWidth+2)

	// Date headers only appear when the listing actually covers several days
	// (`tg ls --days N`), so the common single-day table is unchanged.
	multiDay := spansDays(entries, loc)

	total, anyRunning := totalDuration(entries, now)
	for i, e := range entries {
		if multiDay && (i == 0 || dayHeader(entries[i-1].Start, loc) != dayHeader(e.Start, loc)) {
			fmt.Fprintln(w, dayHeader(e.Start, loc))
		}
		if i > 0 {
			if gap := gapBetween(entries[i-1], e, loc); gap > 0 {
				// Indented to the duration column so it reads as a filler row.
				fmt.Fprintf(w, "%s%s%-12s(gap %s)\n", numPad, leadPad, "", formatHM(gap))
			}
		}
		startClk := formatClock(e.Start, loc)
		stopClk := "  *"
		if e.Stop != nil {
			stopClk = formatClock(*e.Stop, loc)
		}
		dur := displayDuration(e, now)

		label := entryLabel(e)
		project := ""
		if e.ProjectName != "" {
			project = "[" + e.ProjectName + "]"
		}
		// Lead the line with the project's color block (padded to leadPad so
		// entries without a color still line up).
		lead := leadPad
		if color {
			if block := colorBlock(e.ProjectColor); block != "" {
				lead = block + " "
			}
		}
		line := strings.TrimRight(fmt.Sprintf("%*d  %s%-12s%-7s%-17s %s",
			numWidth, e.Seq, lead, startClk+"-"+stopClk, formatHM(dur), label, project), " ")
		// A gap holds no worked time, so it is greyed out next to the entries
		// that do, exactly as `tg daily` dims the days not worked yet.
		if color && e.Gap {
			line = faint(line)
		}
		fmt.Fprintln(w, line)
	}

	// Idle time since the last entry stopped, rendered as a closing filler row.
	if gap := trailingGap(entries[len(entries)-1], now, loc); gap > 0 {
		fmt.Fprintf(w, "%s%s%-12s(gap %s)\n", numPad, leadPad, "", formatHM(gap))
	}

	fmt.Fprintln(w, todayDivider)
	footer := "Total: " + formatHM(total)
	// The gap entries' own time, so the total can be read against the lines
	// above it: their spans are listed but not tracked (see totalDuration).
	if gaps := gapDuration(entries); gaps > 0 {
		footer += "   (gap " + formatHM(gaps) + ")"
	}
	if anyRunning {
		footer += "   (* running)"
	}
	fmt.Fprintln(w, footer)
}

// currentJSON is the stable --json shape for `current`/`status`. The fields
// describe the last entry (running or finished): ElapsedSeconds is its live
// elapsed time while running and its stored duration once stopped, GapSeconds
// is the idle time between its stop and now (0 while running or with no
// entries), and DayTotalSeconds is today's tracked total.
type currentJSON struct {
	Running bool `json:"running"`
	// Gap marks the last entry as a gap placeholder (see store.Entry.Gap),
	// i.e. a span the day accounts for without tracking work in it. It is
	// omitted for an ordinary entry, so the shape is unchanged for one.
	Gap             bool   `json:"gap,omitempty"`
	Task            string `json:"task,omitempty"`
	Project         string `json:"project,omitempty"`
	Start           string `json:"start,omitempty"`
	Stop            string `json:"stop,omitempty"`
	ElapsedSeconds  int64  `json:"elapsed_seconds,omitempty"`
	GapSeconds      int64  `json:"gap_seconds"`
	DayTotalSeconds int64  `json:"day_total_seconds"`
	ID              int64  `json:"id,omitempty"`
}

// statusGap returns the idle time between last's stop and now worth reporting,
// or 0. Nothing is reported for a missing or still-running entry, nor for a
// sub-gapThreshold distance (5-minute quantization noise). Unlike the gaps in
// the daily table this deliberately spans calendar days: "nothing tracked for
// 18h" is exactly what the status line is for.
func statusGap(last *store.Entry, now time.Time) time.Duration {
	if last == nil || last.Stop == nil {
		return 0
	}
	gap := now.Sub(*last.Stop)
	if gap < gapThreshold {
		return 0
	}
	return gap
}

// renderCurrent writes the status line for the last entry (nil when nothing has
// ever been tracked) plus today's tracked total dayTotal. A running entry is
// reported as running with its live elapsed time; a finished one is reported
// with its wall-clock range and, when now has moved past its stop, the idle gap.
func renderCurrent(w io.Writer, last *store.Entry, dayTotal time.Duration, now time.Time, loc *time.Location, jsonOut bool) error {
	gap := statusGap(last, now)
	if jsonOut {
		out := currentJSON{
			Running:         last != nil && last.Stop == nil,
			GapSeconds:      int64(gap / time.Second),
			DayTotalSeconds: int64(dayTotal / time.Second),
		}
		if last != nil {
			out.Gap = last.Gap
			out.Task = entryLabel(*last)
			out.Project = last.ProjectName
			out.Start = last.Start.UTC().Format(time.RFC3339)
			if last.Stop != nil {
				out.Stop = last.Stop.UTC().Format(time.RFC3339)
			}
			out.ElapsedSeconds = int64(displayDuration(*last, now) / time.Second)
			out.ID = last.ID
		}
		return writeJSON(w, out)
	}

	if last == nil {
		fmt.Fprintln(w, "No entries. Today: "+formatHM(dayTotal))
		return nil
	}
	label := truncName(entryLabel(*last), statusNameMax)
	if last.ProjectName != "" {
		label += " [" + last.ProjectName + "]"
	}
	var line string
	if last.Stop == nil {
		line = fmt.Sprintf("run %s (%s)", label, formatHM(now.Sub(last.Start)))
	} else {
		line = fmt.Sprintf("%s %s-%s", label,
			formatClock(last.Start, loc), formatClock(*last.Stop, loc))
		if gap > 0 {
			line += " (gap " + formatHM(gap) + ")"
		}
	}
	fmt.Fprintln(w, line+" Today: "+formatHM(dayTotal))
	return nil
}

// todayEntryJSON / todayJSON are the stable --json shapes for `today`. Num is
// the entry's persistent per-day number (store.Entry.Seq), the same one the
// human listing leads with, so scripted callers can address an entry the way
// `tg mod`/`tg del` expect.
//
// Gap marks a gap placeholder (see store.Entry.Gap): the entry occupies
// DurationSeconds without tracking work, so it is the one kind of entry whose
// duration is NOT part of the listing's TotalSeconds. It is omitted on an
// ordinary entry, leaving that shape as it was.
type todayEntryJSON struct {
	Num             int    `json:"num"`
	ID              int64  `json:"id"`
	Gap             bool   `json:"gap,omitempty"`
	Task            string `json:"task,omitempty"`
	Project         string `json:"project,omitempty"`
	Description     string `json:"description,omitempty"`
	Start           string `json:"start"`
	Stop            string `json:"stop,omitempty"`
	DurationSeconds int64  `json:"duration_seconds"`
	Running         bool   `json:"running"`
}

type todayJSON struct {
	Entries      []todayEntryJSON `json:"entries"`
	TotalSeconds int64            `json:"total_seconds"`
}

// renderTodayJSON writes the daily entries as the stable JSON shape.
func renderTodayJSON(w io.Writer, entries []store.Entry, now time.Time) error {
	out := todayJSON{Entries: []todayEntryJSON{}}
	for _, e := range entries {
		dur := displayDuration(e, now)
		je := todayEntryJSON{
			Num:             e.Seq,
			ID:              e.ID,
			Gap:             e.Gap,
			Task:            e.TaskName,
			Project:         e.ProjectName,
			Start:           e.Start.UTC().Format(time.RFC3339),
			DurationSeconds: int64(dur / time.Second),
			Running:         e.Stop == nil,
		}
		if e.TaskName == "" {
			je.Description = e.Description
		}
		if e.Stop != nil {
			je.Stop = e.Stop.UTC().Format(time.RFC3339)
		}
		out.Entries = append(out.Entries, je)
	}
	total, _ := totalDuration(entries, now)
	out.TotalSeconds = int64(total / time.Second)
	return writeJSON(w, out)
}

// taskRow is the stable --json shape for `tasks`.
type taskRow struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Project string `json:"project,omitempty"`
	Active  bool   `json:"active"`
}

// renderTasks writes the catalog task list, aligning the project column to the
// widest task name.
func renderTasks(w io.Writer, tasks []store.Task) {
	if len(tasks) == 0 {
		fmt.Fprintln(w, "No tasks. Run `tg update` to refresh the catalog.")
		return
	}
	width := 0
	for _, t := range tasks {
		if n := len(t.Name); n > width {
			width = n
		}
	}
	for _, t := range tasks {
		if t.ProjectName != "" {
			fmt.Fprintf(w, "%-*s  [%s]\n", width, t.Name, t.ProjectName)
		} else {
			fmt.Fprintln(w, t.Name)
		}
	}
}

// renderTasksJSON writes the catalog tasks as the stable JSON shape.
func renderTasksJSON(w io.Writer, tasks []store.Task) error {
	out := make([]taskRow, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, taskRow{ID: t.ID, Name: t.Name, Project: t.ProjectName, Active: t.Active})
	}
	return writeJSON(w, out)
}

// projectRow is the stable --json shape for `projects`.
type projectRow struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Client   string `json:"client,omitempty"`
	Active   bool   `json:"active"`
	Billable bool   `json:"billable"`
}

// renderProjects writes the catalog project list with ids, leading with the id
// column (right-aligned) so it can be exported as TOGGL_PROJECT_ID.
func renderProjects(w io.Writer, projects []store.Project) {
	if len(projects) == 0 {
		fmt.Fprintln(w, "No projects. Run `tg projects update` to refresh the catalog.")
		return
	}
	width := 0
	for _, p := range projects {
		if n := len(strconv.FormatInt(p.ID, 10)); n > width {
			width = n
		}
	}
	for _, p := range projects {
		if p.ClientName != "" {
			fmt.Fprintf(w, "%*d  %s  [%s]\n", width, p.ID, p.Name, p.ClientName)
		} else {
			fmt.Fprintf(w, "%*d  %s\n", width, p.ID, p.Name)
		}
	}
}

// renderProjectsJSON writes the catalog projects as the stable JSON shape.
func renderProjectsJSON(w io.Writer, projects []store.Project) error {
	out := make([]projectRow, 0, len(projects))
	for _, p := range projects {
		out = append(out, projectRow{
			ID: p.ID, Name: p.Name, Client: p.ClientName,
			Active: p.Active, Billable: p.Billable,
		})
	}
	return writeJSON(w, out)
}

// totalTaskJSON / totalJSON are the stable --json shapes for `total`.
type totalTaskJSON struct {
	Task            string `json:"task"`
	Project         string `json:"project,omitempty"`
	DurationSeconds int64  `json:"duration_seconds"`
}

// totalFragmentJSON is one fragment's own totals. It is only present when
// several fragments were given (`tg total login docs`), so the single-fragment
// shape stays exactly what it always was.
type totalFragmentJSON struct {
	Fragment     string          `json:"fragment"`
	Tasks        []totalTaskJSON `json:"tasks"`
	TotalSeconds int64           `json:"total_seconds"`
}

type totalJSON struct {
	Fragments    []totalFragmentJSON `json:"fragments,omitempty"`
	Tasks        []totalTaskJSON     `json:"tasks"`
	TotalSeconds int64               `json:"total_seconds"`
}

// totalRowsDuration sums one group's tracked time.
func totalRowsDuration(rows []totalRow) time.Duration {
	var total time.Duration
	for _, r := range rows {
		total += time.Duration(r.Seconds) * time.Second
	}
	return total
}

// totalNameWidth is the width of the task-name column: the widest name in any
// group, so the duration column lines up across the whole report.
func totalNameWidth(groups []totalGroup) int {
	width := 0
	for _, g := range groups {
		for _, r := range g.Rows {
			if n := len(r.Name); n > width {
				width = n
			}
		}
	}
	return width
}

// renderTotals writes the per-task totals table to w, one line per task with
// its tracked time and project, then a divider and the summed total. Task names
// are padded to the widest name so the duration column lines up (mirroring
// renderToday). The project is omitted when the task is not in the local
// catalog (see cmdTotal).
//
// Several fragments (`tg total login docs`) are reported as one group each,
// under a header naming the fragment and its own total, with the group's tasks
// indented beneath it — the same "header per group" shape renderToday uses for
// a multi-day listing, and equally absent from the single-group output. Since
// fragments are independent searches that may overlap, a task can be listed
// under two of them; the footer counts every distinct task once, so it stays
// the tracked time for the range rather than the sum of the headers.
func renderTotals(w io.Writer, groups []totalGroup) {
	rows := distinctTotalRows(groups)
	if len(rows) == 0 {
		fmt.Fprintln(w, "No matching tasks.")
		return
	}
	// Headers (and the indent that goes with them) only appear once there is
	// more than one fragment to tell apart.
	grouped := len(groups) > 1
	indent := ""
	if grouped {
		indent = "  "
	}
	width := totalNameWidth(groups)
	for _, g := range groups {
		if grouped {
			fmt.Fprintf(w, "%s  %s\n", g.Fragment, formatHM(totalRowsDuration(g.Rows)))
		}
		for _, r := range g.Rows {
			project := ""
			if r.ProjectName != "" {
				project = "  [" + r.ProjectName + "]"
			}
			d := time.Duration(r.Seconds) * time.Second
			fmt.Fprintf(w, "%s%-*s  %s%s\n", indent, width, r.Name, formatHM(d), project)
		}
	}
	fmt.Fprintln(w, todayDivider)
	fmt.Fprintln(w, "Total: "+formatHM(totalRowsDuration(rows)))
}

// renderTotalsJSON writes the per-task totals as the stable JSON shape: the
// distinct tasks and the overall total, plus a per-fragment breakdown when
// several fragments were given (see renderTotals for why the top-level total is
// not the sum of the fragments').
func renderTotalsJSON(w io.Writer, groups []totalGroup) error {
	rows := distinctTotalRows(groups)
	out := totalJSON{Tasks: totalTasksJSON(rows)}
	for _, r := range rows {
		out.TotalSeconds += r.Seconds
	}
	if len(groups) > 1 {
		out.Fragments = make([]totalFragmentJSON, 0, len(groups))
		for _, g := range groups {
			f := totalFragmentJSON{Fragment: g.Fragment, Tasks: totalTasksJSON(g.Rows)}
			for _, r := range g.Rows {
				f.TotalSeconds += r.Seconds
			}
			out.Fragments = append(out.Fragments, f)
		}
	}
	return writeJSON(w, out)
}

// totalTasksJSON is the JSON task list for one set of report lines.
func totalTasksJSON(rows []totalRow) []totalTaskJSON {
	out := make([]totalTaskJSON, 0, len(rows))
	for _, r := range rows {
		out = append(out, totalTaskJSON{
			Task: r.Name, Project: r.ProjectName, DurationSeconds: r.Seconds,
		})
	}
	return out
}

// dailyDayJSON / dailyJSON are the stable --json shapes for `daily`. Date is
// the calendar day (YYYY-MM-DD) in the reporting location, OvertimeSeconds is
// the signed difference between the day's tracked time and TargetSeconds, and
// the top-level OvertimeSeconds is measured against TargetSeconds multiplied by
// the number of listed days (see cmdDaily).
type dailyDayJSON struct {
	Date            string `json:"date"`
	DurationSeconds int64  `json:"duration_seconds"`
	OvertimeSeconds int64  `json:"overtime_seconds"`
	Running         bool   `json:"running"`
}

type dailyJSON struct {
	Days            []dailyDayJSON `json:"days"`
	Tracked         int64          `json:"total_seconds"`
	TargetSeconds   int64          `json:"target_seconds"`
	OvertimeSeconds int64          `json:"overtime_seconds"`
}

// dailyTotals sums the listed days and reports the aggregate the footer needs:
// the tracked total, the overall overtime against target * number of listed
// days, and whether any listed day is still accumulating time.
func dailyTotals(rows []dailyRow, target time.Duration) (tracked, overtime time.Duration, anyRunning bool) {
	for _, r := range rows {
		tracked += r.Tracked
		if r.Running {
			anyRunning = true
		}
	}
	return tracked, tracked - target*time.Duration(len(rows)), anyRunning
}

// renderDaily writes the per-day table for the current month: one line per day
// that has tracked time, with its date, tracked total and signed overtime
// against target, then a divider and the month's total. The footer's overtime is
// measured against target multiplied by the number of LISTED days, so days with
// nothing tracked never count against it (see cmdDaily).
//
// A day holding a still-running entry is flagged with `*` next to its duration
// (and the footer says so), mirroring how `tg ls` marks a running entry: its
// total is live and keeps growing.
//
// Days after today in loc (entries booked ahead) are dimmed when color is set,
// since their time has not been worked yet and their overtime is a plan rather
// than a result. color enables ANSI styling and should only be set when w is a
// terminal.
func renderDaily(w io.Writer, rows []dailyRow, now time.Time, target time.Duration, loc *time.Location, color bool) {
	if len(rows) == 0 {
		fmt.Fprintln(w, "No entries this month.")
		return
	}
	today := startOfDay(now, loc)
	for _, r := range rows {
		dur := formatHM(r.Tracked)
		if r.Running {
			dur += "*"
		}
		line := fmt.Sprintf("%s  %-8s%s", dayHeader(r.Day, loc), dur, formatOvertime(r.Tracked-target))
		// r.Day is already midnight, but normalising through startOfDay keeps
		// the comparison a pure calendar-day one whatever zone it carries.
		if color && startOfDay(r.Day, loc).After(today) {
			line = faint(line)
		}
		fmt.Fprintln(w, line)
	}
	tracked, overtime, anyRunning := dailyTotals(rows, target)
	fmt.Fprintln(w, todayDivider)
	footer := fmt.Sprintf("Total: %-8s%s  (%s x %s)",
		formatHM(tracked), formatOvertime(overtime), plural(len(rows), "day"), formatHM(target))
	if anyRunning {
		footer += "   (* running)"
	}
	fmt.Fprintln(w, footer)
}

// plural renders a count with its noun, appending "s" for anything but one
// ("1 day", "2 days"). Only the regular plural is needed here.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// renderDailyJSON writes the per-day totals as the stable JSON shape.
func renderDailyJSON(w io.Writer, rows []dailyRow, target time.Duration, loc *time.Location) error {
	out := dailyJSON{
		Days:          make([]dailyDayJSON, 0, len(rows)),
		TargetSeconds: int64(target / time.Second),
	}
	for _, r := range rows {
		out.Days = append(out.Days, dailyDayJSON{
			Date:            r.Day.In(loc).Format(dateLayout),
			DurationSeconds: int64(r.Tracked / time.Second),
			OvertimeSeconds: int64((r.Tracked - target) / time.Second),
			Running:         r.Running,
		})
	}
	tracked, overtime, _ := dailyTotals(rows, target)
	out.Tracked = int64(tracked / time.Second)
	out.OvertimeSeconds = int64(overtime / time.Second)
	return writeJSON(w, out)
}

// writeJSON emits compact JSON followed by a newline.
func writeJSON(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(data))
	return err
}

// candidateList renders task match candidates for the ambiguous `add`/`total`
// case, in the order the resolver would pick from, so the first line is the one
// `-1` takes (see resolveTaskFragment). The project is appended when known:
// two tasks may share a name in different projects, which is exactly the
// ambiguity `-1` exists for, and the bare names alone would not tell them apart.
func candidateList(tasks []store.Task) string {
	var b strings.Builder
	for _, t := range tasks {
		if t.ProjectName != "" {
			fmt.Fprintf(&b, "  %s [%s]\n", t.Name, t.ProjectName)
			continue
		}
		fmt.Fprintf(&b, "  %s\n", t.Name)
	}
	return b.String()
}

// projectCandidateList renders project match candidates (name + id) for the
// ambiguous `pull` case so the fragment can be refined or the id exported.
func projectCandidateList(projects []store.Project) string {
	var b strings.Builder
	for _, p := range projects {
		fmt.Fprintf(&b, "  %s (%d)\n", p.Name, p.ID)
	}
	return b.String()
}
