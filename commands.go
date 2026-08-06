package main

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/mantas6/tg/api"
	"github.com/mantas6/tg/config"
	"github.com/mantas6/tg/store"
	"github.com/mantas6/tg/sync"
	"github.com/mantas6/tg/timesig"
)

// cmdAdd records a single, already-stopped time entry from a timesign (see the
// timesig package and docs/timesig.md) and a task-title fragment. The created
// entry is complete (has a stop and a positive duration); tg has no timer, so
// this is the only way entries are created locally.
//
// All three timesign forms are accepted; a bare duration ("1:30") has no times
// of its own and picks up where the last entry ended (see addSpan).
//
// The task fragment is resolved with FindTasksByFragment scoped by projectID
// (from TOGGL_PROJECT_ID or, for the 2-argument form
// `tg add <timesign> <project> <task>`, from the resolved project-name
// argument; see runAdd / resolveAddProject): 1 match -> create the entry;
// many -> error listing candidates; none -> error suggesting `tg update`. The
// entry is stored dirty so a later `tg push` (or the best-effort push below)
// sends it to Toggl.
//
// When c is non-nil the new entry is pushed to Toggl immediately; the push is
// best-effort so a sync failure just leaves the entry dirty (a warning is
// printed) for a later `tg push`.
//
// desc is the entry's free-form description (from `--desc`/`--description`); an
// empty desc leaves the description blank, matching the prior behavior.
func cmdAdd(w io.Writer, st *store.Store, c *api.Client, workspaceID int64, projectID *int64, timesign, fragment, desc string, now time.Time, loc *time.Location) error {
	start, stop, err := addSpan(st, timesign, now, loc)
	if err != nil {
		return err
	}

	fragment = strings.TrimSpace(fragment)
	if fragment == "" {
		return errors.New("usage: tg add <timesign> [project] <task-fragment>")
	}

	// Time is exclusive: refuse to record a span that collides with something
	// already tracked (a running entry counts as occupying everything from its
	// start onwards). Reported before the catalog lookup so the conflict is the
	// error the user sees.
	clashes, err := st.FindOverlapping(start, stop)
	if err != nil {
		return err
	}
	if len(clashes) > 0 {
		return fmt.Errorf("%s-%s overlaps existing entry %s",
			formatClock(start, loc), formatClock(stop, loc), overlapLabel(clashes[0], loc))
	}

	tasks, err := st.FindTasksByFragment(fragment, projectID)
	if err != nil {
		return err
	}

	switch len(tasks) {
	case 0:
		return fmt.Errorf("no task matches %q; run `tg update` to refresh the catalog", fragment)
	case 1:
		task := tasks[0]
		taskID := task.ID
		projID := task.ProjectID
		// Carry the project's billable flag onto the entry: workspaces can
		// reject non-billable entries in billable projects.
		billable, err := projectBillable(st, projID)
		if err != nil {
			return err
		}
		dur := stop.Sub(start)
		if _, err := st.CreateEntry(store.Entry{
			WorkspaceID: workspaceID,
			ProjectID:   &projID,
			TaskID:      &taskID,
			Description: desc,
			Start:       start,
			Stop:        &stop,
			Duration:    int64(dur / time.Second),
			Billable:    billable,
			UpdatedAt:   now,
			Dirty:       true,
		}); err != nil {
			return err
		}
		label := task.Name
		if task.ProjectName != "" {
			label += " [" + task.ProjectName + "]"
		}
		fmt.Fprintf(w, "Added: %s  %s-%s (%s)\n",
			label, formatClock(start, loc), formatClock(stop, loc), formatHM(dur))
		// Push the new entry so Toggl reflects it immediately. Best-effort:
		// keep the local entry dirty for a later `tg push` if the sync fails.
		if c != nil {
			if _, err := sync.Push(st, c, now); err != nil {
				fmt.Fprintf(w, "warning: could not sync to Toggl: %v\n", err)
			}
		}
		return nil
	default:
		return fmt.Errorf("multiple tasks match %q:\n%s", fragment, candidateList(tasks))
	}
}

// addSpan resolves `tg add`'s timesign into the new entry's [start, stop).
//
// The absolute (9-:30) and relative (+:20) forms carry their own times, so they
// are handed to the timesig package unchanged. A bare duration (1:30) does not:
// it means "that long, starting where I left off", so it is anchored to the END
// of the last entry — store.LastEntry, the same subject `tg status` reports and
// a bare `tg mod` edits: today's newest already-started entry. Typing only the
// length is the common case of logging work back to back, and anchoring it to
// the last stop (rather than to now) keeps the day gapless.
//
// The anchor can be missing in two ways, both refused with an error naming the
// forms that do not need one rather than guessing a start:
//
//   - no entry today (a fresh day, so there is nothing to continue from);
//   - the last entry is still running, so it has no end to start at (the same
//     reason `tg mod +DURATION` refuses one).
//
// The resulting span is otherwise ordinary: the caller's overlap guard still
// applies, which is what catches a duration long enough to run into an entry
// already booked for later today (LastEntry skips those, so they are never the
// anchor).
func addSpan(st *store.Store, timesign string, now time.Time, loc *time.Location) (time.Time, time.Time, error) {
	if !timesig.IsDuration(timesign) {
		span, err := timesig.Parse(timesign, now, loc)
		if err != nil {
			return time.Time{}, time.Time{}, err
		}
		return span.Start, span.Stop, nil
	}
	dur, err := timesig.ParseDuration(timesign)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	last, err := st.LastEntry(now)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if last == nil {
		return time.Time{}, time.Time{}, errors.New(
			"no entry tracked today to continue from: give an absolute timesign (e.g. 9-:30) or a relative one (e.g. +1:30) instead")
	}
	if last.Running() {
		return time.Time{}, time.Time{}, errors.New(
			"the last entry is still running: it has no end time to continue from, give an absolute timesign (e.g. 9-:30) or a relative one (e.g. +1:30) instead")
	}
	start := *last.Stop
	return start, start.Add(dur), nil
}

// cmdMod edits a single existing entry in place: its times (from a timesign),
// its description, or both. At least one change must be requested.
//
// ref selects the entry: its number on today's listing (see store.EntryByNum;
// the numbering is per-day and persistent), or 0 meaning "the last entry",
// resolved by store.LastEntry exactly as `tg status` resolves it — today only
// and never something that starts later today.
//
// The timesign is interpreted by form (see docs/timesig.md):
//
//	absolute (9-10:30)  start and stop are set on the ENTRY's calendar day,
//	                    not today's, so an edit never drags an entry onto
//	                    another day.
//	relative (+:20)     only the DURATION is taken and it EXTENDS the entry:
//	                    the start is kept and the stop moves to stop+duration.
//	                    Topping up the entry you just finished is what mod is
//	                    for, so a relative timesign is neither re-anchored to
//	                    now (unlike `add`) nor read as an absolute length.
//
// setDesc distinguishes an omitted --desc from an explicit empty one, so a
// description can be cleared with --desc "".
//
// Only today's entries may be edited: an entry that started on an earlier
// calendar day (in loc), or an edit that would move an entry back before
// today's midnight, is refused with an error wrapping store.ErrEntryTooOld.
// The same failsafe is enforced again inside store.UpdateEntry, so no code path
// can write past days.
//
// Retiming is refused when the new span would overlap another entry (the entry
// being modified is excluded from the check). The entry is stored dirty so a
// later `tg push` sends it; when c is non-nil the push is attempted immediately,
// best-effort, exactly like `tg add`.
func cmdMod(w io.Writer, st *store.Store, c *api.Client, ref int, timesign, desc string, setDesc bool, now time.Time, loc *time.Location) error {
	if timesign == "" && !setDesc {
		return errors.New("usage: tg mod [entry-number] [timesign] [--desc TEXT]")
	}

	e, err := modTarget(st, ref, now)
	if err != nil {
		return err
	}
	// Failsafe: history is read-only. Checked here so the refusal is the first
	// thing reported (before any timesign or overlap complaint) and again in
	// store.UpdateEntry, which is the write that must never happen.
	if err := store.CheckEditableDay(e.Start, now, loc); err != nil {
		return err
	}

	if timesign != "" {
		start, stop, err := modSpan(e, timesign, now, loc)
		if err != nil {
			return err
		}
		if err := store.CheckEditableDay(start, now, loc); err != nil {
			return err
		}
		// Time is exclusive (see cmdAdd), but the entry may of course keep
		// overlapping itself, so it is excluded from the conflict search.
		clashes, err := st.FindOverlappingExcluding(start, stop, e.ID)
		if err != nil {
			return err
		}
		if len(clashes) > 0 {
			return fmt.Errorf("%s-%s overlaps existing entry %s",
				formatClock(start, loc), formatClock(stop, loc), overlapLabel(clashes[0], loc))
		}
		e.Start = start
		e.Stop = &stop
		e.Duration = int64(stop.Sub(start) / time.Second)
	}
	if setDesc {
		e.Description = strings.TrimSpace(desc)
	}

	e.UpdatedAt = now
	if err := st.UpdateEntry(e, now, loc); err != nil {
		return err
	}
	renderEntryChange(w, "Modified", e, loc)
	if c != nil {
		if _, err := sync.Push(st, c, now); err != nil {
			fmt.Fprintf(w, "warning: could not sync to Toggl: %v\n", err)
		}
	}
	return nil
}

// modTarget resolves the entry `tg mod` acts on: the local number ref when it
// is positive, otherwise the last entry (store.LastEntry, the shared
// resolution `tg status` also reports). Both are scoped to now's calendar day —
// numbers are per-day (see store.EntryByNum) and the last entry is today's —
// which is also the only day mod may write to, so an unaddressable entry is
// reported as missing rather than as a refused edit.
func modTarget(st *store.Store, ref int, now time.Time) (store.Entry, error) {
	if ref > 0 {
		return st.EntryByNum(ref, now)
	}
	last, err := st.LastEntry(now)
	if err != nil {
		return store.Entry{}, err
	}
	if last == nil {
		return store.Entry{}, errors.New("no entry tracked today to modify")
	}
	return *last, nil
}

// modSpan computes an entry's new [start, stop) from a timesign: an absolute
// sign is resolved on the entry's own calendar day, while a relative sign
// contributes only its duration, which is ADDED to the entry's stop — the start
// never moves (see cmdMod). A running entry has no stop to add to, so extending
// one is refused; it can still be retimed with an absolute sign.
func modSpan(e store.Entry, timesign string, now time.Time, loc *time.Location) (time.Time, time.Time, error) {
	if timesig.IsRelative(timesign) {
		relative := timesign
		raw := strings.TrimSpace(timesign)
		body := strings.TrimSpace(strings.TrimPrefix(raw, timesig.RelativePrefix))
		// mod's unitless shorthand is minutes; the shared parser uses hours.
		if isDigits(body) {
			relative = timesig.RelativePrefix + ":" + body
		}
		span, err := timesig.ParseRelative(relative, now, loc)
		if err != nil {
			return time.Time{}, time.Time{}, err
		}
		if e.Stop == nil {
			return time.Time{}, time.Time{}, errors.New(
				"entry is still running: it has no end time to extend, give an absolute timesign (e.g. 9-10:30) instead")
		}
		return e.Start, e.Stop.Add(span.Duration()), nil
	}
	// e.Start stands in for "now" so the range lands on the entry's day.
	span, err := timesig.ParseAbsolute(timesign, e.Start, loc)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	return span.Start, span.Stop, nil
}

// cmdDel deletes the entry addressed by the local number ref, resolved on
// today's calendar day (see store.EntryByNum). The deletion is soft: the row is
// marked deleted and dirty so it vanishes from every listing at once and the
// removal can still be pushed to Toggl, which is where the row is finally
// dropped (see sync.Push). When c is non-nil that push is attempted
// immediately, best-effort, exactly like `tg add`.
//
// The deleted entry's number is retired with it: the day's numbering keeps the
// gap instead of shifting every later entry down one.
func cmdDel(w io.Writer, st *store.Store, c *api.Client, ref int, now time.Time, loc *time.Location) error {
	if ref < 1 {
		return errors.New("usage: tg del <entry-number>")
	}
	e, err := st.EntryByNum(ref, now)
	if err != nil {
		return err
	}
	if err := st.SoftDeleteEntry(e.ID, now); err != nil {
		return err
	}
	renderEntryChange(w, "Deleted", e, loc)
	if c != nil {
		if _, err := sync.Push(st, c, now); err != nil {
			fmt.Fprintf(w, "warning: could not sync to Toggl: %v\n", err)
		}
	}
	return nil
}

// totalRow is one line of `tg total`: the seconds come from the Reports API,
// the display name and project from the local catalog.
type totalRow struct {
	TaskID      int64
	Name        string
	ProjectName string
	Seconds     int64
}

// cmdTotal reports per-task tracked totals for the date range [since, now]
// (both taken as calendar days in loc). The seconds come from the Reports API
// (see api.SummaryByTask); everything about task identity comes from the local
// catalog: summary rows are joined to cached tasks BY TASK ID. That join is
// what makes fragment matching work at all, since the summary sub-groups
// routinely carry no titles, so matching reported names matched nothing.
//
// fragment is a single task-name fragment (`tg total code review` searches for
// "code review", exactly like `tg add`), matched against the cached task names
// by the store's own matcher (store.FindTasksByFragment: case-insensitive
// substring, exact name wins). An empty fragment lists every task with tracked
// time in the range. Rows whose task id is not in the local catalog cannot
// match a fragment; unfiltered they are still listed, labelled with whatever
// title the API supplied or `task #<id>`.
//
// The window defaults to the last three months (see runTotal/resolveTotalSince)
// and can be overridden with `--since`. Output is one line per task with its
// total, followed by the sum of all listed tasks.
func cmdTotal(w io.Writer, st *store.Store, c *api.Client, workspaceID int64, fragment string, since, now time.Time, loc *time.Location, jsonOut bool) error {
	fragment = strings.TrimSpace(fragment)

	startDate := since.In(loc).Format("2006-01-02")
	endDate := now.In(loc).Format("2006-01-02")
	rows, err := c.SummaryByTask(workspaceID, startDate, endDate)
	if err != nil {
		return err
	}

	// Catalog lookup for display: inactive tasks are included so a row for an
	// archived task still gets a name, and ListTasks joins the project name.
	catalog, err := st.ListTasks(true, nil)
	if err != nil {
		return err
	}
	byID := make(map[int64]store.Task, len(catalog))
	for _, t := range catalog {
		byID[t.ID] = t
	}

	// Fragment matching happens on the local catalog (never on report titles);
	// the resulting task ids are what filters the summary rows.
	var keep map[int64]bool
	if fragment != "" {
		matched, err := st.FindTasksByFragment(fragment, nil)
		if err != nil {
			return err
		}
		if len(matched) == 0 {
			return fmt.Errorf("no task matches %q", fragment)
		}
		keep = make(map[int64]bool, len(matched))
		for _, t := range matched {
			keep[t.ID] = true
		}
	}

	var out []totalRow
	for _, r := range rows {
		if keep != nil && !keep[r.TaskID] {
			continue
		}
		row := totalRow{TaskID: r.TaskID, Name: r.Name, Seconds: r.Seconds}
		if t, ok := byID[r.TaskID]; ok {
			row.Name = t.Name
			row.ProjectName = t.ProjectName
		} else if row.Name == "" {
			row.Name = fmt.Sprintf("task #%d", r.TaskID)
		}
		out = append(out, row)
	}
	if len(out) == 0 {
		if fragment != "" {
			return fmt.Errorf("no tracked time for %q in %s..%s", fragment, startDate, endDate)
		}
		return fmt.Errorf("no tracked time in %s..%s", startDate, endDate)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].TaskID < out[j].TaskID
	})

	if jsonOut {
		return renderTotalsJSON(w, out)
	}
	renderTotals(w, out)
	return nil
}

// dailyRow is one line of `tg daily`: a calendar day with everything tracked on
// it. Day is midnight of that day in the reporting location, Tracked is the sum
// of the day's entry durations (live elapsed for a running one, see
// displayDuration) and Running records that one of them has not stopped yet, so
// the total is still growing.
type dailyRow struct {
	Day     time.Time
	Tracked time.Duration
	Running bool
}

// cmdDaily reports tracked time per calendar day for the CURRENT month: the
// window is [first of now's month, first of next month) in loc, so the listing
// covers the whole month regardless of how far into it now is. Everything comes
// from the local store (store.EntriesBetween) — daily never talks to Toggl, so
// run `tg pull` first if remote days are missing.
//
// Only days that actually have entries get a line: a month is listed as the
// days you worked, not as 30 rows most of which are empty. That is also what
// makes the footer's target meaningful, since it is targetHours multiplied by
// the number of LISTED days (weekends and days off never count against you).
//
// targetHours is the target number of hours per day (`-t`/`--target`, default
// dailyDefaultTarget); each day line and the footer show the signed difference
// between tracked and target time (see formatOvertime). A zero target is
// allowed and simply makes every overtime figure equal the tracked time; a
// negative one is a usage error.
//
// A running entry contributes its live elapsed time, exactly as `tg today` and
// `tg status` count it, so today's row keeps moving while something runs.
func cmdDaily(w io.Writer, st *store.Store, now time.Time, loc *time.Location, targetHours float64, jsonOut bool) error {
	if targetHours < 0 {
		return fmt.Errorf("invalid target %g: hours per day must not be negative", targetHours)
	}
	from := startOfMonth(now, loc)
	to := from.AddDate(0, 1, 0)
	entries, err := st.EntriesBetween(from, to)
	if err != nil {
		return err
	}
	rows := groupDaily(entries, now, loc)
	target := time.Duration(targetHours * float64(time.Hour))
	if jsonOut {
		return renderDailyJSON(w, rows, target, loc)
	}
	renderDaily(w, rows, target, loc)
	return nil
}

// groupDaily folds entries into one row per calendar day in loc, preserving the
// chronological order EntriesBetween returns. Entries are bucketed by the day
// their START falls on, so an entry that crosses midnight counts entirely
// towards the day it began — the same day its per-day number belongs to.
func groupDaily(entries []store.Entry, now time.Time, loc *time.Location) []dailyRow {
	var rows []dailyRow
	for _, e := range entries {
		day := startOfDay(e.Start, loc)
		if n := len(rows); n == 0 || !rows[n-1].Day.Equal(day) {
			rows = append(rows, dailyRow{Day: day})
		}
		r := &rows[len(rows)-1]
		r.Tracked += displayDuration(e, now)
		if e.Stop == nil {
			r.Running = true
		}
	}
	return rows
}

// cmdCurrent shows the terse status line: the most recent entry, the idle gap
// between its stop and now, and today's tracked total.
//
// A running entry wins over a newer finished one. tg itself never starts a
// timer, so a running entry can only arrive from a `tg pull` of remote data;
// when one exists it is still what the user is tracking and is reported as
// running, whatever day it began on. Otherwise the entry comes from the shared
// resolution `tg mod` also uses (store.LastEntry): today's newest entry that has
// already started, so a fresh day reports no entry rather than yesterday's, and
// something booked for later today is not mistaken for the last thing tracked.
// The total always covers today only.
func cmdCurrent(w io.Writer, st *store.Store, now time.Time, loc *time.Location, jsonOut bool) error {
	last, err := st.Running()
	if err != nil {
		return err
	}
	if last == nil {
		if last, err = st.LastEntry(now); err != nil {
			return err
		}
	}
	dayStart := startOfDay(now, loc)
	entries, err := st.EntriesBetween(dayStart, dayStart.Add(24*time.Hour))
	if err != nil {
		return err
	}
	total, _ := totalDuration(entries, now)
	return renderCurrent(w, last, total, now, loc, jsonOut)
}

// cmdToday lists entries for the current day (or the last `days` days). color
// enables ANSI project-color blocks in the human output (never in JSON) and
// should reflect whether w is a terminal.
//
// The number each line leads with is the entry's own persistent per-day number,
// handed out when the entry was inserted (see store.CreateEntry): it restarts
// at 1 every calendar day, never changes, and is never reused, so `tg mod 2` /
// `tg del 3` keep meaning the same entries however often the day is listed.
// Deleting an entry therefore leaves a gap rather than renumbering the rest. A
// multi-day listing groups entries under a date header, since each day carries
// its own 1..N; `mod`/`del` address today's numbers.
func cmdToday(w io.Writer, st *store.Store, now time.Time, loc *time.Location, days int, jsonOut, color bool) error {
	if days < 1 {
		days = 1
	}
	dayStart := startOfDay(now, loc)
	from := dayStart.AddDate(0, 0, -(days - 1))
	to := dayStart.Add(24 * time.Hour)

	entries, err := st.EntriesBetween(from, to)
	if err != nil {
		return err
	}
	if jsonOut {
		return renderTodayJSON(w, entries, now)
	}
	renderToday(w, entries, now, loc, color)
	return nil
}

// cmdTasks lists the locally cached task catalog. `--all` includes inactive
// tasks; a non-nil projectID (from TOGGL_PROJECT_ID) scopes the listing to one
// project. Refresh the cache with `tg update`.
func cmdTasks(w io.Writer, st *store.Store, all bool, projectID *int64, jsonOut bool) error {
	tasks, err := st.ListTasks(all, projectID)
	if err != nil {
		return err
	}
	if jsonOut {
		return renderTasksJSON(w, tasks)
	}
	renderTasks(w, tasks)
	return nil
}

// cmdGrep lists every cached task whose name contains fragment
// (case-insensitive), in the same shape as `tg tasks`. `--all` includes
// inactive tasks and a non-nil projectID (from TOGGL_PROJECT_ID) scopes the
// search to one project, exactly like cmdTasks.
//
// Unlike the fragment matching used by `add`/`total`
// (store.FindTasksByFragment), grep never lets an exact name win over the
// substring matches: the point of the command is to SEE every candidate, so a
// task named "Fix" must not hide "Fix login bug". Ordering is the catalog's
// (project, then task name).
//
// An empty fragment is a usage error rather than "list everything" (that is
// what `tg tasks` is for), and finding nothing is an error too, so grep exits
// non-zero when there is no match.
func cmdGrep(w io.Writer, st *store.Store, all bool, projectID *int64, fragment string, jsonOut bool) error {
	fragment = strings.TrimSpace(fragment)
	if fragment == "" {
		return errors.New("usage: tg grep <fragment>")
	}
	tasks, err := st.ListTasks(all, projectID)
	if err != nil {
		return err
	}
	matches := grepTasks(tasks, fragment)
	if len(matches) == 0 {
		return fmt.Errorf("no task matches %q; run `tg update` to refresh the catalog", fragment)
	}
	if jsonOut {
		return renderTasksJSON(w, matches)
	}
	renderTasks(w, matches)
	return nil
}

// grepTasks filters tasks down to those whose name contains fragment,
// case-insensitively, preserving the input order (see cmdGrep).
func grepTasks(tasks []store.Task, fragment string) []store.Task {
	frag := strings.ToLower(strings.TrimSpace(fragment))
	if frag == "" {
		return nil
	}
	var out []store.Task
	for _, t := range tasks {
		if strings.Contains(strings.ToLower(t.Name), frag) {
			out = append(out, t)
		}
	}
	return out
}

// cmdProjects lists the locally cached project catalog with ids so the id can
// be exported as TOGGL_PROJECT_ID to scope other commands. `--all` includes
// inactive projects; refresh the cache with `tg update`.
func cmdProjects(w io.Writer, st *store.Store, all, jsonOut bool) error {
	projects, err := st.ListProjects(all)
	if err != nil {
		return err
	}
	if jsonOut {
		return renderProjectsJSON(w, projects)
	}
	renderProjects(w, projects)
	return nil
}

// cmdUpdate refreshes the local state for a SINGLE project (never the whole
// workspace): its tasks are fetched and upserted, and its recent time entries
// are pulled. The project is chosen by projectID (from TOGGL_PROJECT_ID) when
// set; otherwise fragment must uniquely match a cached project name.
// Refreshing every project at once is intentionally disallowed (see
// resolveUpdateProject).
//
// The project catalog itself is NOT synced here: update never fetches project
// metadata from Toggl. Refresh the catalog with `tg projects update`. (The
// entry pull may still self-heal catalog rows from each entry's meta payload;
// see sync.healCatalog.)
//
// The entry pull reconciles everything modified in [since, now] and is scoped
// to the same project, so it is partial and leaves the last_pull watermark
// untouched (see sync.Pull): a later `tg pull` still sees every other
// project's changes. runUpdate derives since from --days/-n, which defaults to
// one day back (see resolveUpdateSince).
//
// The command is quiet: in human mode it prints nothing at all (no progress or
// summary lines) and reports only errors. Machine-readable output is still
// available via --json.
func cmdUpdate(w io.Writer, st *store.Store, c *api.Client, workspaceID int64, projectID *int64, fragment string, since, now time.Time, all, jsonOut bool) error {
	pid, err := resolveUpdateProject(st, projectID, fragment)
	if err != nil {
		return err
	}
	tasks, err := c.ProjectTasks(workspaceID, *pid, all)
	if err != nil {
		return err
	}
	if err := st.ReplaceProjectTasks(*pid, toStoreTasks(tasks)); err != nil {
		return err
	}
	entries, err := sync.Pull(st, c, pid, since, now)
	if err != nil {
		return err
	}

	if jsonOut {
		// The name comes from the local catalog (looked up after the pull so a
		// row healed by it is visible) and is empty for a project that is not
		// cached yet, e.g. an uncached TOGGL_PROJECT_ID.
		name, err := projectName(st, *pid)
		if err != nil {
			return err
		}
		return writeJSON(w, map[string]any{
			"project": name,
			"tasks":   len(tasks),
			"entries": entries,
		})
	}
	return nil
}

// cmdUpdateProjects backs `tg projects update`. It syncs the WHOLE workspace
// project catalog: every available
// project is fetched from Toggl and upserted into the local store. Unlike
// cmdUpdate (which is deliberately scoped to a single project), this walks the
// entire workspace, but it never fetches tasks — refresh a project's tasks with
// `tg update`. `--all` includes inactive projects.
func cmdUpdateProjects(w io.Writer, st *store.Store, c *api.Client, workspaceID int64, all, jsonOut bool) error {
	// Progress line goes to the writer only in human mode; under --json it is
	// suppressed so the JSON output stays clean (see cmdUpdate).
	if !jsonOut {
		fmt.Fprintln(w, "Fetching projects...")
	}
	projects, err := c.Projects(workspaceID, all)
	if err != nil {
		return err
	}
	for _, p := range projects {
		if err := st.PutProject(toStoreProject(p)); err != nil {
			return err
		}
	}
	if jsonOut {
		return writeJSON(w, map[string]any{"projects": len(projects)})
	}
	fmt.Fprintf(w, "Updated project catalog: %d projects.\n", len(projects))
	return nil
}

// cmdPush sends dirty local entries to Toggl.
func cmdPush(w io.Writer, st *store.Store, c *api.Client, now time.Time, jsonOut bool) error {
	res, err := sync.Push(st, c, now)
	if err != nil {
		return err
	}
	if jsonOut {
		return writeJSON(w, res)
	}
	fmt.Fprintf(w, "Pushed: %d created, %d updated, %d deleted.\n", res.Created, res.Updated, res.Deleted)
	return nil
}

// cmdPull reconciles remote entries into the local store (LWW). With no project
// argument it pulls EVERY project's entries in a single pass. Unlike
// start/tasks/update, `tg pull` deliberately ignores TOGGL_PROJECT_ID: scoping
// happens only via an explicit project-name fragment that uniquely matches a
// cached project, and such a scoped (partial) pull leaves the last_pull
// watermark untouched.
//
// The time window is the caller's: runPull defaults it to today and widens it
// to the current month under --all/-a (see resolvePullSince). A window that
// does not reach back to the watermark is partial too and leaves it untouched
// (see sync.Pull).
func cmdPull(w io.Writer, st *store.Store, c *api.Client, fragment string, since, now time.Time, jsonOut bool) error {
	pid, err := resolvePullScope(st, fragment)
	if err != nil {
		return err
	}
	res, err := sync.Pull(st, c, pid, since, now)
	if err != nil {
		return err
	}
	if jsonOut {
		return writeJSON(w, res)
	}
	fmt.Fprintf(w, "Pulled: %d inserted, %d updated, %d deleted, %d skipped.\n",
		res.Inserted, res.Updated, res.Deleted, res.Skipped)
	return nil
}

// resolveCachedProject resolves an optional env project id or a project-name
// fragment to exactly one cached project id. When projectID (TOGGL_PROJECT_ID)
// is non-nil it wins and fragment is ignored. Otherwise fragment is required
// (emptyErr is returned verbatim when it is blank) and must resolve to exactly
// one cached project: none -> error + noMatchHint; many -> error listing
// candidates. This is the shared machinery that keeps `add`, `pull`, and
// `update` scoped to a single project rather than the whole workspace.
func resolveCachedProject(st *store.Store, projectID *int64, fragment string, emptyErr error, noMatchHint string) (*int64, error) {
	if projectID != nil {
		return projectID, nil
	}
	fragment = strings.TrimSpace(fragment)
	if fragment == "" {
		return nil, emptyErr
	}
	projects, err := st.FindProjectsByFragment(fragment)
	if err != nil {
		return nil, err
	}
	switch len(projects) {
	case 0:
		return nil, fmt.Errorf("no project matches %q%s", fragment, noMatchHint)
	case 1:
		id := projects[0].ID
		return &id, nil
	default:
		return nil, fmt.Errorf("multiple projects match %q:\n%s", fragment, projectCandidateList(projects))
	}
}

// resolvePullScope decides which project(s) `tg pull` reconciles from its
// optional project-name argument. A nil result means "all projects", which is
// the default when no argument is given. TOGGL_PROJECT_ID is intentionally NOT
// consulted here, so pull spans every project unless a name is given
// explicitly. Otherwise the pull is scoped to exactly one cached project (see
// resolvePullProject).
func resolvePullScope(st *store.Store, fragment string) (*int64, error) {
	if strings.TrimSpace(fragment) == "" {
		return nil, nil // no argument -> pull every project
	}
	return resolvePullProject(st, fragment)
}

// resolvePullProject resolves the single-project scope requested by `tg pull`'s
// explicit project-name argument; see resolveCachedProject. The unscoped "pull
// all projects" case is handled earlier by resolvePullScope. Unlike other
// commands, pull never falls back to TOGGL_PROJECT_ID.
func resolvePullProject(st *store.Store, fragment string) (*int64, error) {
	return resolveCachedProject(st, nil, fragment,
		errors.New("pull requires a project-name argument"),
		"; run `tg update` to refresh the catalog")
}

// resolveUpdateProject decides which single project `tg update` refreshes. When
// TOGGL_PROJECT_ID is set it wins; otherwise the project-name argument must
// uniquely match a cached project. This keeps update from ever refreshing every
// project at once.
func resolveUpdateProject(st *store.Store, projectID *int64, fragment string) (*int64, error) {
	return resolveCachedProject(st, projectID, fragment,
		errors.New("update requires a project-name argument (or set TOGGL_PROJECT_ID)"),
		"; set TOGGL_PROJECT_ID to its id to update a project not yet cached")
}

// resolveAddProject resolves the project-name argument accepted by the 2-fragment
// form of `tg add` (`tg add <timesign> <project> <task>`) to exactly one cached
// project id, so the task search can be scoped to it.
func resolveAddProject(st *store.Store, fragment string) (*int64, error) {
	return resolveCachedProject(st, nil, fragment,
		errors.New("usage: tg add <timesign> [project] <task-fragment>"),
		"; run `tg update` to refresh the catalog")
}

// cmdAuth acquires a token (via tokenSource), verifies it against GET /me, and
// on success writes config.json. Nothing is written on an invalid token.
func cmdAuth(w io.Writer, tokenSource func() (string, error), newClient func(token string) *api.Client) error {
	token, err := tokenSource()
	if err != nil {
		return err
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return errors.New("no API token provided")
	}

	me, err := newClient(token).Me()
	if err != nil {
		if errors.Is(err, api.ErrUnauthorized) {
			return errors.New("authentication failed: invalid token (nothing written)")
		}
		return err
	}

	cfg := &config.Config{APIToken: token, WorkspaceID: me.DefaultWorkspaceID}
	if err := cfg.Save(); err != nil {
		return err
	}
	name := me.Fullname
	if name == "" {
		name = me.Email
	}
	fmt.Fprintf(w, "Authenticated as %s (workspace %d).\n", name, me.DefaultWorkspaceID)
	return nil
}

// projectName returns the cached project's name, or "" when the project is not
// in the local catalog yet (e.g. an id that has never been cached).
func projectName(st *store.Store, projectID int64) (string, error) {
	p, err := st.ProjectByID(projectID)
	if err != nil {
		return "", err
	}
	if p == nil {
		return "", nil
	}
	return p.Name, nil
}

// projectBillable reports whether the cached project is billable, defaulting to
// false when the project is not in the local catalog yet (e.g. before the first
// `tg update`).
func projectBillable(st *store.Store, projectID int64) (bool, error) {
	p, err := st.ProjectByID(projectID)
	if err != nil {
		return false, err
	}
	if p == nil {
		return false, nil
	}
	return p.Billable, nil
}

// startOfDay returns midnight of t's calendar day in loc.
func startOfDay(t time.Time, loc *time.Location) time.Time {
	t = t.In(loc)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
}

// startOfMonth returns midnight of the first day of t's calendar month in loc.
func startOfMonth(t time.Time, loc *time.Location) time.Time {
	t = t.In(loc)
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, loc)
}

func toStoreProject(p api.Project) store.Project {
	return store.Project{
		ID: p.ID, WorkspaceID: p.WorkspaceID, Name: p.Name,
		Color: p.Color, ClientName: p.ClientName, Active: p.Active,
		Billable: p.Billable, At: p.At,
	}
}

func toStoreTasks(ts []api.Task) []store.Task {
	out := make([]store.Task, len(ts))
	for i, t := range ts {
		out[i] = store.Task{
			ID: t.ID, WorkspaceID: t.WorkspaceID, ProjectID: t.ProjectID,
			Name: t.Name, Active: t.Active, At: t.At,
		}
	}
	return out
}
