package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/mantas6/tg/api"
	"github.com/mantas6/tg/config"
	"github.com/mantas6/tg/store"
	"github.com/mantas6/tg/timesig"
	"github.com/mantas6/tg/togglsync"
)

// cmdEnv is everything a command needs of its surroundings, as opposed to its
// own arguments: the context it runs under, where its output goes, the local
// store, the Toggl client, and the clock and calendar it reckons time in. The
// run* wiring in main.go builds one per invocation (see withEnv) and the cmd*
// functions take it as their first parameter, so their signatures are about the
// command — `tg add`'s timesign, `tg mod`'s entry number — rather than about
// re-threading the same six values through every one of them.
//
// A cmdEnv is read-only for the commands: nothing mutates it.
type cmdEnv struct {
	ctx context.Context
	w   io.Writer
	st  *store.Store

	// c is the Toggl client, nil exactly when tg has no credentials yet (see
	// offline). Commands never test it directly: they either tolerate its
	// absence via bestEffortPush or demand it via client.
	c *api.Client

	// workspaceID is the configured workspace new entries are filed under; 0
	// when tg is unauthenticated (see workspaceFor).
	workspaceID int64

	now time.Time
	loc *time.Location
}

// offline reports whether this invocation has no Toggl credentials, i.e. `tg
// auth` has not run (or its config was removed). tg is local-first, so the
// commands that only touch the local store keep working offline; the ones that
// cannot do anything without the API ask for the client explicitly and get a
// clear error instead (see client).
func (e *cmdEnv) offline() bool { return e.c == nil }

// client returns the Toggl client for a command that cannot work without it
// (push, pull, update, total), or config.ErrNotConfigured — the same "run
// `tg auth`" error a missing config produced before — when tg is offline. It is
// the single explicit place the credentials requirement is stated, rather than a
// nil check spread over the command bodies.
func (e *cmdEnv) client() (*api.Client, error) {
	if e.offline() {
		return nil, config.ErrNotConfigured
	}
	return e.c, nil
}

// bestEffortPush sends the dirty local entries right after a local edit
// (`add`/`mod`/`del`) so Toggl reflects it immediately. It is best-effort by
// design: a failure is reported as a warning on the command's own output and the
// entry simply stays dirty for a later `tg push`, which is what keeps editing
// usable with no network — or with no credentials at all, where the push is
// skipped entirely.
func (e *cmdEnv) bestEffortPush() {
	if e.offline() {
		return
	}
	if _, err := togglsync.Push(e.ctx, e.st, e.c, e.now); err != nil {
		fmt.Fprintf(e.w, "warning: could not sync to Toggl: %v\n", err)
	}
}

// workspaceFor decides which workspace a newly created entry belongs to: the
// configured one when tg is authenticated, otherwise the workspace of the task
// the entry is filed under, which the local catalog knows because every cached
// task was fetched from that workspace. That is what lets `tg add` work while
// offline or after the config was lost, like `tg mod`/`tg del` always have,
// instead of refusing an edit the local store is perfectly able to record.
//
// Neither source knowing a workspace is a genuine dead end (the entry could
// never be pushed), so it is an error naming `tg auth`.
func (e *cmdEnv) workspaceFor(task store.Task) (int64, error) {
	if e.workspaceID != 0 {
		return e.workspaceID, nil
	}
	if task.WorkspaceID != 0 {
		return task.WorkspaceID, nil
	}
	return 0, fmt.Errorf("unknown workspace for task %q: %w", task.Name, config.ErrNotConfigured)
}

// addUsage is `tg add`'s usage line. Three places reject a call that names no
// task — the argument peeling in runAdd, cmdAdd's own guard, and the project
// resolver when only a project was given — so the line lives here once rather
// than being spelled out (and drifting) in each of them.
const addUsage = "usage: tg add <timesign> [project] <task-fragment>"

// cmdAdd records a single, already-stopped time entry from a timesign (see the
// timesig package and docs/timesig.md) and a task-title fragment. The created
// entry is complete (has a stop and a positive duration); tg has no timer, so
// this is the only way entries are created locally.
//
// All three timesign forms are accepted; a bare duration ("1:30") has no times
// of its own and picks up where the last entry ended (see addSpan).
//
// The task fragment is resolved with resolveTaskFragment scoped by projectID
// (from TOGGL_PROJECT_ID or, for the 2-argument form
// `tg add <timesign> <project> <task>`, from the resolved project-name
// argument; see runAdd / resolveAddProject): 1 match -> create the entry;
// many -> error listing candidates (unless first, the `-1` flag, picks the top
// one); none -> error suggesting `tg update`. The entry is stored dirty so a
// later `tg push` (or the best-effort push below) sends it to Toggl.
//
// The new entry is pushed to Toggl immediately unless tg is offline; the push is
// best-effort so a sync failure just leaves the entry dirty (a warning is
// printed) for a later `tg push`. See cmdEnv.bestEffortPush.
//
// desc is the entry's free-form description (from `--desc`/`--description`); an
// empty desc leaves the description blank, matching the prior behavior.
func cmdAdd(env *cmdEnv, projectID *int64, first bool, timesign, fragment, desc string) error {
	start, stop, err := addSpan(env.ctx, env.st, timesign, env.now, env.loc)
	if err != nil {
		return err
	}

	fragment = strings.TrimSpace(fragment)
	if fragment == "" {
		return errors.New(addUsage)
	}

	// Time is exclusive: refuse to record a span that collides with something
	// already tracked (a running entry counts as occupying everything from its
	// start onwards). Reported before the catalog lookup so the conflict is the
	// error the user sees.
	clashes, err := env.st.FindOverlapping(env.ctx, start, stop)
	if err != nil {
		return err
	}
	if len(clashes) > 0 {
		return fmt.Errorf("%s-%s overlaps existing entry %s",
			formatClock(start, env.loc), formatClock(stop, env.loc), overlapLabel(clashes[0], env.loc))
	}

	task, err := resolveTaskFragment(env.ctx, env.st, fragment, projectID, first)
	if err != nil {
		return err
	}

	workspaceID, err := env.workspaceFor(task)
	if err != nil {
		return err
	}
	taskID := task.ID
	projID := task.ProjectID
	// Carry the project's billable flag onto the entry: workspaces can
	// reject non-billable entries in billable projects.
	billable, err := projectBillable(env.ctx, env.st, projID)
	if err != nil {
		return err
	}
	dur := stop.Sub(start)
	if _, err := env.st.CreateEntry(env.ctx, store.Entry{
		WorkspaceID: workspaceID,
		ProjectID:   &projID,
		TaskID:      &taskID,
		Description: desc,
		Start:       start,
		Stop:        &stop,
		Duration:    int64(dur / time.Second),
		Billable:    billable,
		UpdatedAt:   env.now,
		Dirty:       true,
	}); err != nil {
		return err
	}
	label := task.Name
	if task.ProjectName != "" {
		label += " [" + task.ProjectName + "]"
	}
	fmt.Fprintf(env.w, "Added: %s  %s-%s (%s)\n",
		label, formatClock(start, env.loc), formatClock(stop, env.loc), formatHM(dur))
	// Push the new entry so Toggl reflects it immediately. Best-effort:
	// keep the local entry dirty for a later `tg push` if the sync fails.
	env.bestEffortPush()
	return nil
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
func addSpan(ctx context.Context, st *store.Store, timesign string, now time.Time, loc *time.Location) (time.Time, time.Time, error) {
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
	last, err := st.LastEntry(ctx, now)
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
// later `tg push` sends it; unless tg is offline the push is attempted
// immediately, best-effort, exactly like `tg add`.
func cmdMod(env *cmdEnv, ref int, timesign, desc string, setDesc bool) error {
	if timesign == "" && !setDesc {
		return errors.New("usage: tg mod [entry-number] [timesign] [--desc TEXT]")
	}

	e, err := modTarget(env.ctx, env.st, ref, env.now)
	if err != nil {
		return err
	}
	// Failsafe: history is read-only. Checked here so the refusal is the first
	// thing reported (before any timesign or overlap complaint) and again in
	// store.UpdateEntry, which is the write that must never happen.
	if err := store.CheckEditableDay(e.Start, env.now, env.loc); err != nil {
		return err
	}

	if timesign != "" {
		start, stop, err := modSpan(e, timesign, env.now, env.loc)
		if err != nil {
			return err
		}
		if err := store.CheckEditableDay(start, env.now, env.loc); err != nil {
			return err
		}
		// Time is exclusive (see cmdAdd), but the entry may of course keep
		// overlapping itself, so it is excluded from the conflict search.
		clashes, err := env.st.FindOverlappingExcluding(env.ctx, start, stop, e.ID)
		if err != nil {
			return err
		}
		if len(clashes) > 0 {
			return fmt.Errorf("%s-%s overlaps existing entry %s",
				formatClock(start, env.loc), formatClock(stop, env.loc), overlapLabel(clashes[0], env.loc))
		}
		e.Start = start
		e.Stop = &stop
		e.Duration = int64(stop.Sub(start) / time.Second)
	}
	if setDesc {
		e.Description = strings.TrimSpace(desc)
	}

	e.UpdatedAt = env.now
	if err := env.st.UpdateEntry(env.ctx, e, env.now, env.loc); err != nil {
		return err
	}
	renderEntryChange(env.w, "Modified", e, env.loc)
	env.bestEffortPush()
	return nil
}

// modTarget resolves the entry `tg mod` acts on: the local number ref when it
// is positive, otherwise the last entry (store.LastEntry, the shared
// resolution `tg status` also reports). Both are scoped to now's calendar day —
// numbers are per-day (see store.EntryByNum) and the last entry is today's —
// which is also the only day mod may write to, so an unaddressable entry is
// reported as missing rather than as a refused edit.
func modTarget(ctx context.Context, st *store.Store, ref int, now time.Time) (store.Entry, error) {
	if ref > 0 {
		return st.EntryByNum(ctx, ref, now)
	}
	last, err := st.LastEntry(ctx, now)
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
// dropped (see togglsync.Push). Unless tg is offline that push is attempted
// immediately, best-effort, exactly like `tg add`.
//
// The deleted entry's number is retired with it: the day's numbering keeps the
// gap instead of shifting every later entry down one.
func cmdDel(env *cmdEnv, ref int) error {
	if ref < 1 {
		return errors.New("usage: tg del <entry-number>")
	}
	e, err := env.st.EntryByNum(env.ctx, ref, env.now)
	if err != nil {
		return err
	}
	if err := env.st.SoftDeleteEntry(env.ctx, e.ID, env.now); err != nil {
		return err
	}
	renderEntryChange(env.w, "Deleted", e, env.loc)
	env.bestEffortPush()
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
// A fragment matching several cached tasks totals all of them; first (`-1`)
// narrows it to the first candidate (the same ordering resolveTaskFragment
// picks from), so an ambiguous name can be reported on its own.
//
// The window defaults to the last three months (see runTotal/resolveTotalSince)
// and can be overridden with `--since`. Output is one line per task with its
// total, followed by the sum of all listed tasks.
func cmdTotal(env *cmdEnv, first bool, fragment string, since time.Time, jsonOut bool) error {
	c, err := env.client()
	if err != nil {
		return err
	}
	fragment = strings.TrimSpace(fragment)

	startDate := since.In(env.loc).Format(dateLayout)
	endDate := env.now.In(env.loc).Format(dateLayout)
	rows, err := c.SummaryByTask(env.ctx, env.workspaceID, startDate, endDate)
	if err != nil {
		return fmt.Errorf("fetch totals for %s..%s: %w", startDate, endDate, err)
	}

	// Catalog lookup for display: inactive tasks are included so a row for an
	// archived task still gets a name, and ListTasks joins the project name.
	catalog, err := env.st.ListTasks(env.ctx, true, nil)
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
		matched, err := env.st.FindTasksByFragment(env.ctx, fragment, nil)
		if err != nil {
			return err
		}
		if len(matched) == 0 {
			return fmt.Errorf("no task matches %q", fragment)
		}
		if first {
			matched = matched[:1]
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
		return renderTotalsJSON(env.w, out)
	}
	renderTotals(env.w, out)
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
//
// color enables ANSI styling in the human output (never in JSON): days after
// today are greyed out, see renderDaily.
func cmdDaily(env *cmdEnv, targetHours float64, jsonOut, color bool) error {
	if targetHours < 0 {
		return fmt.Errorf("invalid target %g: hours per day must not be negative", targetHours)
	}
	from := startOfMonth(env.now, env.loc)
	to := from.AddDate(0, 1, 0)
	entries, err := env.st.EntriesBetween(env.ctx, from, to)
	if err != nil {
		return err
	}
	rows := groupDaily(entries, env.now, env.loc)
	target := time.Duration(targetHours * float64(time.Hour))
	if jsonOut {
		return renderDailyJSON(env.w, rows, target, env.loc)
	}
	renderDaily(env.w, rows, env.now, target, env.loc, color)
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
func cmdCurrent(env *cmdEnv, jsonOut bool) error {
	last, err := env.st.Running(env.ctx)
	if err != nil {
		return err
	}
	if last == nil {
		if last, err = env.st.LastEntry(env.ctx, env.now); err != nil {
			return err
		}
	}
	dayStart := startOfDay(env.now, env.loc)
	// A calendar day is not always 24 hours long: AddDate walks to the next
	// midnight in loc, so a DST transition day neither leaks an hour of
	// tomorrow into today's total nor drops one of its own (see cmdDaily).
	entries, err := env.st.EntriesBetween(env.ctx, dayStart, dayStart.AddDate(0, 0, 1))
	if err != nil {
		return err
	}
	total, _ := totalDuration(entries, env.now)
	return renderCurrent(env.w, last, total, env.now, env.loc, jsonOut)
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
func cmdToday(env *cmdEnv, days int, jsonOut, color bool) error {
	if days < 1 {
		days = 1
	}
	dayStart := startOfDay(env.now, env.loc)
	from := dayStart.AddDate(0, 0, -(days - 1))
	// Both bounds are walked by calendar days rather than by 24-hour steps, so
	// the window ends at tomorrow's midnight in loc even on a 23- or 25-hour
	// DST transition day (see cmdCurrent).
	to := dayStart.AddDate(0, 0, 1)

	entries, err := env.st.EntriesBetween(env.ctx, from, to)
	if err != nil {
		return err
	}
	if jsonOut {
		return renderTodayJSON(env.w, entries, env.now)
	}
	renderToday(env.w, entries, env.now, env.loc, color)
	return nil
}

// cmdTasks lists the locally cached task catalog. `--all` includes inactive
// tasks; a non-nil projectID (from TOGGL_PROJECT_ID) scopes the listing to one
// project. Refresh the cache with `tg update`.
func cmdTasks(env *cmdEnv, all bool, projectID *int64, jsonOut bool) error {
	tasks, err := env.st.ListTasks(env.ctx, all, projectID)
	if err != nil {
		return err
	}
	if jsonOut {
		return renderTasksJSON(env.w, tasks)
	}
	renderTasks(env.w, tasks)
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
//
// first (`-1`) cuts the listing down to its first line, mirroring the flag the
// other fragment-taking commands accept. The candidate is grep's own first one
// (catalog order, no exact-name precedence), so it is not necessarily the task
// `tg add -1` would pick with the same fragment.
func cmdGrep(env *cmdEnv, all bool, projectID *int64, first bool, fragment string, jsonOut bool) error {
	fragment = strings.TrimSpace(fragment)
	if fragment == "" {
		return errors.New("usage: tg grep <fragment>")
	}
	tasks, err := env.st.ListTasks(env.ctx, all, projectID)
	if err != nil {
		return err
	}
	matches := grepTasks(tasks, fragment)
	if len(matches) == 0 {
		return fmt.Errorf("no task matches %q; run `tg update` to refresh the catalog", fragment)
	}
	if first {
		matches = matches[:1]
	}
	if jsonOut {
		return renderTasksJSON(env.w, matches)
	}
	renderTasks(env.w, matches)
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
func cmdProjects(env *cmdEnv, all, jsonOut bool) error {
	projects, err := env.st.ListProjects(env.ctx, all)
	if err != nil {
		return err
	}
	if jsonOut {
		return renderProjectsJSON(env.w, projects)
	}
	renderProjects(env.w, projects)
	return nil
}

// cmdUpdate refreshes the local state for a SINGLE project (never the whole
// workspace): its tasks are fetched and upserted, and its recent time entries
// are pulled. The project is chosen by projectID (from TOGGL_PROJECT_ID) when
// set; otherwise fragment must uniquely match a cached project name, or match
// several with first (`-1`) picking the top candidate.
// Refreshing every project at once is intentionally disallowed (see
// resolveUpdateProject).
//
// The project catalog itself is NOT synced here: update never fetches project
// metadata from Toggl. Refresh the catalog with `tg projects update`. (The
// entry pull may still self-heal catalog rows from each entry's meta payload;
// see togglsync's healCatalog.)
//
// The entry pull reconciles everything modified in [since, now] and is scoped
// to the same project, so it is partial and leaves the last_pull watermark
// untouched (see togglsync.Pull): a later `tg pull` still sees every other
// project's changes. runUpdate derives since from --days/-n, which defaults to
// one day back (see resolveUpdateSince).
//
// The command is quiet: in human mode it prints nothing at all (no progress or
// summary lines) and reports only errors. Machine-readable output is still
// available via --json.
func cmdUpdate(env *cmdEnv, projectID *int64, first bool, fragment string, since time.Time, all, jsonOut bool) error {
	c, err := env.client()
	if err != nil {
		return err
	}
	pid, err := resolveUpdateProject(env.ctx, env.st, projectID, fragment, first)
	if err != nil {
		return err
	}
	tasks, err := c.ProjectTasks(env.ctx, env.workspaceID, *pid, all)
	if err != nil {
		return fmt.Errorf("fetch tasks of project %d: %w", *pid, err)
	}
	if err := env.st.ReplaceProjectTasks(env.ctx, *pid, toStoreTasks(tasks)); err != nil {
		return err
	}
	entries, err := togglsync.Pull(env.ctx, env.st, c, pid, since, env.now)
	if err != nil {
		return err
	}

	if jsonOut {
		// The name comes from the local catalog (looked up after the pull so a
		// row healed by it is visible) and is empty for a project that is not
		// cached yet, e.g. an uncached TOGGL_PROJECT_ID.
		name, err := projectName(env.ctx, env.st, *pid)
		if err != nil {
			return err
		}
		return writeJSON(env.w, map[string]any{
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
func cmdUpdateProjects(env *cmdEnv, all, jsonOut bool) error {
	c, err := env.client()
	if err != nil {
		return err
	}
	// Progress line goes to the writer only in human mode; under --json it is
	// suppressed so the JSON output stays clean (see cmdUpdate).
	if !jsonOut {
		fmt.Fprintln(env.w, "Fetching projects...")
	}
	projects, err := c.Projects(env.ctx, env.workspaceID, all)
	if err != nil {
		return fmt.Errorf("fetch projects: %w", err)
	}
	for _, p := range projects {
		if err := env.st.PutProject(env.ctx, toStoreProject(p)); err != nil {
			return err
		}
	}
	if jsonOut {
		return writeJSON(env.w, map[string]any{"projects": len(projects)})
	}
	fmt.Fprintf(env.w, "Updated project catalog: %d projects.\n", len(projects))
	return nil
}

// cmdPush sends dirty local entries to Toggl.
//
// Entries Toggl refuses do not abort the push (see togglsync.Push): the rest of
// the queue is still sent, so the summary is printed either way and the
// rejections are reported afterwards — as the command's error, so the exit
// status still says something went wrong, and under --json as the result's
// "failed" list. Anything else (a store failure, a cancelled context) is fatal
// and reported on its own.
func cmdPush(env *cmdEnv, jsonOut bool) error {
	c, err := env.client()
	if err != nil {
		return err
	}
	res, pushErr := togglsync.Push(env.ctx, env.st, c, env.now)
	var failures *togglsync.PushError
	if pushErr != nil && !errors.As(pushErr, &failures) {
		return pushErr
	}
	if jsonOut {
		if err := writeJSON(env.w, res); err != nil {
			return err
		}
		return pushErr
	}
	fmt.Fprintf(env.w, "Pushed: %d created, %d updated, %d deleted.\n", res.Created, res.Updated, res.Deleted)
	return pushErr
}

// cmdPull reconciles remote entries into the local store (LWW). With no project
// argument it pulls EVERY project's entries in a single pass. Unlike
// start/tasks/update, `tg pull` deliberately ignores TOGGL_PROJECT_ID: scoping
// happens only via an explicit project-name fragment that uniquely matches a
// cached project (or matches several with first, `-1`, taking the top
// candidate), and such a scoped (partial) pull leaves the last_pull watermark
// untouched.
//
// The time window is the caller's: runPull defaults it to today and widens it
// to the current month under --all/-a (see resolvePullSince). A window that
// does not reach back to the watermark is partial too and leaves it untouched
// (see togglsync.Pull).
func cmdPull(env *cmdEnv, first bool, fragment string, since time.Time, jsonOut bool) error {
	c, err := env.client()
	if err != nil {
		return err
	}
	pid, err := resolvePullScope(env.ctx, env.st, fragment, first)
	if err != nil {
		return err
	}
	res, err := togglsync.Pull(env.ctx, env.st, c, pid, since, env.now)
	if err != nil {
		return err
	}
	if jsonOut {
		return writeJSON(env.w, res)
	}
	fmt.Fprintf(env.w, "Pulled: %d inserted, %d updated, %d deleted, %d skipped.\n",
		res.Inserted, res.Updated, res.Deleted, res.Skipped)
	return nil
}

// firstMatchHint closes every ambiguous-fragment error: refining the fragment
// is one way out, `-1` (see the first parameter threaded through the resolvers
// below) is the other, so the flag is advertised where it is needed.
const firstMatchHint = "pass -1 to use the first match"

// resolveTaskFragment resolves a task-name fragment to the single cached task a
// command should act on, optionally scoped to projectID (from TOGGL_PROJECT_ID
// or an explicit project argument). Matching is the store's
// (store.FindTasksByFragment: case-insensitive substring, an exact name winning
// over mere substrings), and the candidates are ordered as candidateList prints
// them, so "the first match" means the first line of the ambiguity error.
//
// A fragment matching several tasks is an error listing the candidates, unless
// first (the `-1` flag) is set: then the top candidate is taken, which is how
// two identically named tasks in different projects are disambiguated without
// reaching for TOGGL_PROJECT_ID. No match is always an error, since there is
// nothing to pick.
func resolveTaskFragment(ctx context.Context, st *store.Store, fragment string, projectID *int64, first bool) (store.Task, error) {
	tasks, err := st.FindTasksByFragment(ctx, fragment, projectID)
	if err != nil {
		return store.Task{}, err
	}
	switch {
	case len(tasks) == 0:
		return store.Task{}, fmt.Errorf("no task matches %q; run `tg update` to refresh the catalog", fragment)
	case len(tasks) == 1, first:
		return tasks[0], nil
	default:
		return store.Task{}, fmt.Errorf("multiple tasks match %q:\n%s%s",
			fragment, candidateList(labelCandidates(ctx, st, tasks)), firstMatchHint)
	}
}

// labelCandidates fills in the candidates' project names for the ambiguity
// error: task matching runs on a name-only query (store.FindTasksByFragment
// does not join projects), yet the projects are precisely what distinguishes
// two tasks sharing a name — the case `-1` exists for. A lookup that fails is
// ignored rather than replacing the ambiguity report with a database error: the
// candidate then simply keeps its bare name.
func labelCandidates(ctx context.Context, st *store.Store, tasks []store.Task) []store.Task {
	out := make([]store.Task, len(tasks))
	copy(out, tasks)
	for i := range out {
		if out[i].ProjectName != "" {
			continue
		}
		if name, err := projectName(ctx, st, out[i].ProjectID); err == nil {
			out[i].ProjectName = name
		}
	}
	return out
}

// resolveCachedProject resolves an optional env project id or a project-name
// fragment to exactly one cached project id. When projectID (TOGGL_PROJECT_ID)
// is non-nil it wins and fragment is ignored. Otherwise fragment is required
// (emptyErr is returned verbatim when it is blank) and must resolve to exactly
// one cached project: none -> error + noMatchHint; many -> error listing
// candidates, unless first (the `-1` flag) takes the top candidate, exactly as
// resolveTaskFragment does for tasks. This is the shared machinery that keeps
// `add`, `pull`, and `update` scoped to a single project rather than the whole
// workspace.
func resolveCachedProject(ctx context.Context, st *store.Store, projectID *int64, fragment string, first bool, emptyErr error, noMatchHint string) (*int64, error) {
	if projectID != nil {
		return projectID, nil
	}
	fragment = strings.TrimSpace(fragment)
	if fragment == "" {
		return nil, emptyErr
	}
	projects, err := st.FindProjectsByFragment(ctx, fragment)
	if err != nil {
		return nil, err
	}
	switch {
	case len(projects) == 0:
		return nil, fmt.Errorf("no project matches %q%s", fragment, noMatchHint)
	case len(projects) == 1, first:
		id := projects[0].ID
		return &id, nil
	default:
		return nil, fmt.Errorf("multiple projects match %q:\n%s%s",
			fragment, projectCandidateList(projects), firstMatchHint)
	}
}

// resolvePullScope decides which project(s) `tg pull` reconciles from its
// optional project-name argument. A nil result means "all projects", which is
// the default when no argument is given. TOGGL_PROJECT_ID is intentionally NOT
// consulted here, so pull spans every project unless a name is given
// explicitly. Otherwise the pull is scoped to exactly one cached project (see
// resolvePullProject), with first (`-1`) resolving an ambiguous name.
func resolvePullScope(ctx context.Context, st *store.Store, fragment string, first bool) (*int64, error) {
	if strings.TrimSpace(fragment) == "" {
		return nil, nil // no argument -> pull every project
	}
	return resolvePullProject(ctx, st, fragment, first)
}

// resolvePullProject resolves the single-project scope requested by `tg pull`'s
// explicit project-name argument; see resolveCachedProject. The unscoped "pull
// all projects" case is handled earlier by resolvePullScope. Unlike other
// commands, pull never falls back to TOGGL_PROJECT_ID.
func resolvePullProject(ctx context.Context, st *store.Store, fragment string, first bool) (*int64, error) {
	return resolveCachedProject(ctx, st, nil, fragment, first,
		errors.New("pull requires a project-name argument"),
		"; run `tg update` to refresh the catalog")
}

// resolveUpdateProject decides which single project `tg update` refreshes. When
// TOGGL_PROJECT_ID is set it wins; otherwise the project-name argument must
// uniquely match a cached project (or name several with `-1` set, which takes
// the first). This keeps update from ever refreshing every project at once.
func resolveUpdateProject(ctx context.Context, st *store.Store, projectID *int64, fragment string, first bool) (*int64, error) {
	return resolveCachedProject(ctx, st, projectID, fragment, first,
		errors.New("update requires a project-name argument (or set TOGGL_PROJECT_ID)"),
		"; set TOGGL_PROJECT_ID to its id to update a project not yet cached")
}

// resolveAddProject resolves the project-name argument accepted by the 2-fragment
// form of `tg add` (`tg add <timesign> <project> <task>`) to exactly one cached
// project id, so the task search can be scoped to it. first (`-1`) applies to
// this fragment as well as to the task one, so an ambiguous pair is resolved in
// one go.
func resolveAddProject(ctx context.Context, st *store.Store, fragment string, first bool) (*int64, error) {
	return resolveCachedProject(ctx, st, nil, fragment, first,
		errors.New(addUsage),
		"; run `tg update` to refresh the catalog")
}

// cmdAuth acquires a token (via tokenSource), verifies it against GET /me, and
// on success writes config.json. Nothing is written on an invalid token.
func cmdAuth(ctx context.Context, w io.Writer, tokenSource func() (string, error), newClient func(token string) *api.Client) error {
	token, err := tokenSource()
	if err != nil {
		return err
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return errors.New("no API token provided")
	}

	me, err := newClient(token).Me(ctx)
	if err != nil {
		// Toggl answers bad credentials with either status depending on the
		// endpoint and the token's shape, so both mean "this token is no good"
		// here even though they are distinct errors elsewhere.
		if errors.Is(err, api.ErrUnauthorized) || errors.Is(err, api.ErrForbidden) {
			return errors.New("authentication failed: invalid token (nothing written)")
		}
		return err
	}

	cfg := &config.Config{APIToken: token, WorkspaceID: me.DefaultWorkspaceID}
	if err := cfg.Save(); err != nil {
		return fmt.Errorf("save config: %w", err)
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
func projectName(ctx context.Context, st *store.Store, projectID int64) (string, error) {
	p, err := st.ProjectByID(ctx, projectID)
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
func projectBillable(ctx context.Context, st *store.Store, projectID int64) (bool, error) {
	p, err := st.ProjectByID(ctx, projectID)
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
