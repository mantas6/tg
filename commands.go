package main

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
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
// this is the only way entries are created locally. The task fragment is
// resolved with FindTasksByFragment scoped by projectID (from TOGGL_PROJECT_ID
// or, for the 2-argument form `tg add <timesign> <project> <task>`, from the
// resolved project-name argument; see runAdd / resolveAddProject): 1 match ->
// create the entry; many -> error listing candidates; none -> error suggesting
// `tg update`. The entry is stored dirty so a later `tg push` (or the
// best-effort push below) sends it to Toggl.
//
// When c is non-nil the new entry is pushed to Toggl immediately; the push is
// best-effort so a sync failure just leaves the entry dirty (a warning is
// printed) for a later `tg push`.
//
// desc is the entry's free-form description (from `--desc`/`--description`); an
// empty desc leaves the description blank, matching the prior behavior.
func cmdAdd(w io.Writer, st *store.Store, c *api.Client, workspaceID int64, projectID *int64, timesign, fragment, desc string, now time.Time, loc *time.Location) error {
	span, err := timesig.Parse(timesign, now, loc)
	if err != nil {
		return err
	}
	start, stop := span.Start, span.Stop

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

// cmdTotal reports per-task tracked totals straight from the Toggl Reports API
// (never the local store): it fetches every task's summed seconds over the date
// range [since, now] (both taken as calendar days in loc) and then keeps only
// the tasks whose names match the given fragments. The window defaults to the
// last three months (see runTotal/resolveTotalSince) and can be overridden with
// `--since`. Each fragment is matched against the reported task names with the
// same case-insensitive substring / exact-title-wins semantics as `tg add`
// (see matchSummaryTasks); tasks matched by more than one fragment are listed
// once. Output is one line per matched task with its total, followed by the sum
// of all listed tasks. No fragments, or no matches at all, is an error.
func cmdTotal(w io.Writer, c *api.Client, workspaceID int64, fragments []string, since, now time.Time, loc *time.Location, jsonOut bool) error {
	var cleaned []string
	for _, f := range fragments {
		if f = strings.TrimSpace(f); f != "" {
			cleaned = append(cleaned, f)
		}
	}
	if len(cleaned) == 0 {
		return errors.New("usage: tg total <task-fragment> [task-fragment...]")
	}

	startDate := since.In(loc).Format("2006-01-02")
	endDate := now.In(loc).Format("2006-01-02")
	rows, err := c.SummaryByTask(workspaceID, startDate, endDate)
	if err != nil {
		return err
	}

	// Collect the union of tasks matched by any fragment, deduped by task id so
	// a task caught by two fragments is only listed (and summed) once.
	var matched []api.SummaryTask
	seen := map[int64]bool{}
	for _, frag := range cleaned {
		for _, r := range matchSummaryTasks(rows, frag) {
			if seen[r.TaskID] {
				continue
			}
			seen[r.TaskID] = true
			matched = append(matched, r)
		}
	}
	if len(matched) == 0 {
		return fmt.Errorf("no task matches %s", strings.Join(quoteAll(cleaned), ", "))
	}

	sort.Slice(matched, func(i, j int) bool {
		if matched[i].Name != matched[j].Name {
			return matched[i].Name < matched[j].Name
		}
		return matched[i].TaskID < matched[j].TaskID
	})

	if jsonOut {
		return renderTotalsJSON(w, matched)
	}
	renderTotals(w, matched)
	return nil
}

// matchSummaryTasks mirrors the store's matchTasks for Reports API rows: a
// case-insensitive substring match on the task name, with an exact
// (case-insensitive) full-name match taking precedence over mere substrings.
func matchSummaryTasks(rows []api.SummaryTask, fragment string) []api.SummaryTask {
	frag := strings.ToLower(strings.TrimSpace(fragment))
	if frag == "" {
		return nil
	}
	var subs, exact []api.SummaryTask
	for _, r := range rows {
		name := strings.ToLower(r.Name)
		if !strings.Contains(name, frag) {
			continue
		}
		subs = append(subs, r)
		if name == frag {
			exact = append(exact, r)
		}
	}
	if len(exact) > 0 {
		return exact
	}
	return subs
}

// quoteAll wraps each string in double quotes for a readable error listing.
func quoteAll(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = strconv.Quote(s)
	}
	return out
}

// cmdCurrent shows the terse status line: the most recent entry, the idle gap
// between its stop and now, and today's tracked total.
//
// A running entry wins over a newer finished one. tg itself never starts a
// timer, so a running entry can only arrive from a `tg pull` of remote data;
// when one exists it is still what the user is tracking and is reported as
// running. Otherwise the newest entry by start time is used, regardless of the
// day it falls on (see store.LastEntry), so the status line is never empty just
// because nothing was tracked today. The total always covers today only.
func cmdCurrent(w io.Writer, st *store.Store, now time.Time, loc *time.Location, jsonOut bool) error {
	last, err := st.Running()
	if err != nil {
		return err
	}
	if last == nil {
		if last, err = st.LastEntry(); err != nil {
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
// Listing also publishes the local entry numbers: the listed entries are
// numbered 1..N in display order and that mapping is persisted (replacing the
// previous one) so later commands can address an entry as `tg mod 2` /
// `tg del 3`. The mapping is saved in both human and JSON mode, and an empty
// listing clears it, so a number never resolves to something that was not on
// screen.
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
	ids := make([]int64, len(entries))
	for i, e := range entries {
		ids[i] = e.ID
	}
	if err := st.SaveEntryRefs(ids); err != nil {
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

// cmdUpdate refreshes the cached catalog for a SINGLE project (never the whole
// workspace): its metadata plus its tasks are fetched and upserted. The project
// is chosen by projectID (from TOGGL_PROJECT_ID) when set; otherwise fragment
// must uniquely match a cached project name. Refreshing every project at once
// is intentionally disallowed (see resolveUpdateProject).
func cmdUpdate(w io.Writer, st *store.Store, c *api.Client, workspaceID int64, projectID *int64, fragment string, all, jsonOut bool) error {
	pid, err := resolveUpdateProject(st, projectID, fragment)
	if err != nil {
		return err
	}
	// Progress lines go to the same writer as the summary, but only in human
	// mode: suppressing them under --json keeps the JSON output clean.
	if !jsonOut {
		fmt.Fprintln(w, "Fetching project...")
	}
	project, err := c.Project(workspaceID, *pid)
	if err != nil {
		return err
	}
	if !jsonOut {
		fmt.Fprintln(w, "Fetching tasks...")
	}
	tasks, err := c.ProjectTasks(workspaceID, *pid, all)
	if err != nil {
		return err
	}
	if err := st.PutProject(toStoreProject(project)); err != nil {
		return err
	}
	if err := st.ReplaceProjectTasks(*pid, toStoreTasks(tasks)); err != nil {
		return err
	}

	if jsonOut {
		return writeJSON(w, map[string]any{"project": project.Name, "tasks": len(tasks)})
	}
	fmt.Fprintf(w, "Updated catalog for %s: %d tasks.\n", project.Name, len(tasks))
	return nil
}

// cmdUpdateProjects syncs the WHOLE workspace project catalog: every available
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
// argument it pulls EVERY project's entries in a single pass and advances the
// last_pull watermark. Unlike start/tasks/update, `tg pull` deliberately
// ignores TOGGL_PROJECT_ID: scoping happens only via an explicit project-name
// fragment that uniquely matches a cached project, and such a scoped (partial)
// pull leaves the watermark untouched.
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
