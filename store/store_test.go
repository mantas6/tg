package store

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func ptrInt(v int64) *int64 { return &v }

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "tg.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mustCreate(t *testing.T, s *Store, e Entry) int64 {
	t.Helper()
	id, err := s.CreateEntry(e)
	if err != nil {
		t.Fatalf("create entry: %v", err)
	}
	return id
}

func TestMigrateIdempotent(t *testing.T) {
	s := openTest(t)
	// Re-running migrate must not error or wipe data.
	if err := s.migrate(); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if v, ok, _ := s.GetMeta(MetaSchemaVersion); !ok || v != schemaVersion {
		t.Fatalf("schema_version = %q ok=%v, want %q", v, ok, schemaVersion)
	}
}

func TestMigrateAddsBillableColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tg.db")

	// Seed a pre-v2 database whose entries/projects tables predate billable.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := raw.Exec(`
CREATE TABLE entries (
  id INTEGER PRIMARY KEY AUTOINCREMENT, remote_id INTEGER UNIQUE,
  workspace_id INTEGER NOT NULL, project_id INTEGER, task_id INTEGER,
  description TEXT NOT NULL DEFAULT '', start TEXT NOT NULL, stop TEXT,
  duration INTEGER NOT NULL, updated_at TEXT NOT NULL, synced_at TEXT,
  dirty INTEGER NOT NULL DEFAULT 1, deleted INTEGER NOT NULL DEFAULT 0);
CREATE TABLE projects (
  id INTEGER PRIMARY KEY, workspace_id INTEGER NOT NULL, name TEXT NOT NULL,
  color TEXT, client_name TEXT, active INTEGER NOT NULL DEFAULT 1, at TEXT);
CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT);`); err != nil {
		t.Fatalf("seed old schema: %v", err)
	}
	raw.Close()

	// Open runs migrate, which must add the billable columns in place.
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open (migrate): %v", err)
	}
	t.Cleanup(func() { s.Close() })

	for _, tbl := range []string{"entries", "projects"} {
		has, err := s.hasColumn(tbl, "billable")
		if err != nil {
			t.Fatalf("hasColumn %s: %v", tbl, err)
		}
		if !has {
			t.Errorf("%s.billable missing after migrate", tbl)
		}
	}
	if v, ok, _ := s.GetMeta(MetaSchemaVersion); !ok || v != schemaVersion {
		t.Errorf("schema_version = %q ok=%v, want %q", v, ok, schemaVersion)
	}

	// The migrated store round-trips billable on both projects and entries.
	if err := s.ReplaceProjects([]Project{
		{ID: 1, WorkspaceID: 1, Name: "P", Active: true, Billable: true},
	}); err != nil {
		t.Fatalf("replace projects: %v", err)
	}
	got, err := s.ProjectByID(1)
	if err != nil || got == nil || !got.Billable {
		t.Fatalf("ProjectByID = %+v err=%v, want billable", got, err)
	}
}

func TestEntryBillableRoundTrip(t *testing.T) {
	s := openTest(t)
	start := time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC)
	mustCreate(t, s, Entry{
		WorkspaceID: 1, RemoteID: ptrInt(42), Start: start, Duration: 300,
		Billable: true, UpdatedAt: start, Dirty: true,
	})
	got, err := s.EntryByRemoteID(42)
	if err != nil || got == nil {
		t.Fatalf("by remote: %v err=%v", got, err)
	}
	if !got.Billable {
		t.Errorf("Billable = false, want true")
	}
}

func TestProjectByIDMissing(t *testing.T) {
	s := openTest(t)
	got, err := s.ProjectByID(999)
	if err != nil {
		t.Fatalf("ProjectByID: %v", err)
	}
	if got != nil {
		t.Errorf("ProjectByID(missing) = %+v, want nil", got)
	}
}

// TestRunningPicksNewest pins Running()'s tie-breaking: tg no longer starts
// entries itself, so running rows arrive from pulls and the store can end up
// holding more than one. The newest by start wins, and deleted rows are ignored.
func TestRunningPicksNewest(t *testing.T) {
	s := openTest(t)
	base := time.Date(2026, 1, 2, 8, 0, 0, 0, time.UTC)
	mustCreate(t, s, Entry{WorkspaceID: 1, TaskID: ptrInt(7), Start: base, Duration: -1, UpdatedAt: base})
	newest := mustCreate(t, s, Entry{
		WorkspaceID: 1, TaskID: ptrInt(8), Start: base.Add(time.Hour), Duration: -1, UpdatedAt: base,
	})

	r, err := s.Running()
	if err != nil {
		t.Fatalf("Running: %v", err)
	}
	if r == nil || r.ID != newest {
		t.Fatalf("Running = %+v, want id %d", r, newest)
	}

	if _, err := s.db.Exec("UPDATE entries SET deleted = 1 WHERE id = ?", newest); err != nil {
		t.Fatal(err)
	}
	r, err = s.Running()
	if err != nil {
		t.Fatalf("Running (deleted): %v", err)
	}
	if r == nil || !r.Start.Equal(base) {
		t.Fatalf("Running (deleted) = %+v, want the 08:00 entry", r)
	}
}

func TestEntriesBetweenOrdering(t *testing.T) {
	s := openTest(t)
	day := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	mk := func(h int) {
		st := day.Add(time.Duration(h) * time.Hour)
		mustCreate(t, s, Entry{WorkspaceID: 1, Start: st, Stop: ptrTime(st.Add(time.Hour)), Duration: 3600, UpdatedAt: st})
	}
	mk(11)
	mk(9)
	mk(14)
	// An entry outside the window must be excluded.
	mustCreate(t, s, Entry{WorkspaceID: 1, Start: day.Add(-2 * time.Hour), Duration: 3600, UpdatedAt: day})

	got, err := s.EntriesBetween(day, day.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("between: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("entries = %d, want 3", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].Start.Before(got[i-1].Start) {
			t.Fatalf("entries not ordered by start: %v", got)
		}
	}
}

// TestLastEntry covers the query behind `tg status`: the newest entry by start
// wins regardless of the day it falls on, deleted entries are skipped, and an
// empty store yields nil.
func TestLastEntry(t *testing.T) {
	s := openTest(t)

	got, err := s.LastEntry()
	if err != nil {
		t.Fatalf("LastEntry (empty): %v", err)
	}
	if got != nil {
		t.Fatalf("LastEntry (empty) = %+v, want nil", got)
	}

	day := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	mk := func(start time.Time) int64 {
		return mustCreate(t, s, Entry{
			WorkspaceID: 1, Start: start, Stop: ptrTime(start.Add(time.Hour)),
			Duration: 3600, UpdatedAt: start,
		})
	}
	mk(day.Add(9 * time.Hour))
	mk(day.AddDate(0, 0, -3).Add(9 * time.Hour)) // an older day
	newest := mk(day.Add(14 * time.Hour))

	got, err = s.LastEntry()
	if err != nil {
		t.Fatalf("LastEntry: %v", err)
	}
	if got == nil || got.ID != newest {
		t.Fatalf("LastEntry = %+v, want id %d", got, newest)
	}

	// A deleted newest entry is skipped in favour of the previous one.
	if _, err := s.db.Exec("UPDATE entries SET deleted = 1 WHERE id = ?", newest); err != nil {
		t.Fatal(err)
	}
	got, err = s.LastEntry()
	if err != nil {
		t.Fatalf("LastEntry (deleted): %v", err)
	}
	if got == nil || !got.Start.Equal(day.Add(9*time.Hour)) {
		t.Fatalf("LastEntry (deleted) = %+v, want the 09:00 entry", got)
	}
}

// TestFindOverlapping tables the interval arithmetic of the overlap guard
// against a single stored 10:00-11:00 entry: intersecting ranges match, and
// ranges that only touch an endpoint do not.
func TestFindOverlapping(t *testing.T) {
	s := openTest(t)
	day := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	at := func(h, m int) time.Time { return day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute) }
	existing := mustCreate(t, s, Entry{
		WorkspaceID: 1, Start: at(10, 0), Stop: ptrTime(at(11, 0)),
		Duration: 3600, UpdatedAt: at(10, 0),
	})

	cases := []struct {
		name        string
		start, stop time.Time
		want        bool
	}{
		{"straddles start", at(9, 30), at(10, 30), true},
		{"straddles stop", at(10, 30), at(11, 30), true},
		{"contained", at(10, 15), at(10, 45), true},
		{"contains", at(9, 0), at(12, 0), true},
		{"identical", at(10, 0), at(11, 0), true},
		{"touching before", at(9, 0), at(10, 0), false},
		{"touching after", at(11, 0), at(12, 0), false},
		{"well before", at(8, 0), at(9, 0), false},
		{"well after", at(12, 0), at(13, 0), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.FindOverlapping(tc.start, tc.stop)
			if err != nil {
				t.Fatalf("FindOverlapping: %v", err)
			}
			if (len(got) > 0) != tc.want {
				t.Fatalf("FindOverlapping(%s, %s) = %d entries, want overlap=%v",
					tc.start.Format("15:04"), tc.stop.Format("15:04"), len(got), tc.want)
			}
			if tc.want && got[0].ID != existing {
				t.Errorf("overlapping id = %d, want %d", got[0].ID, existing)
			}
		})
	}
}

// TestFindOverlappingIgnoresDeleted keeps soft-deleted entries from blocking a
// range they no longer occupy.
func TestFindOverlappingIgnoresDeleted(t *testing.T) {
	s := openTest(t)
	start := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)
	mustCreate(t, s, Entry{
		WorkspaceID: 1, Start: start, Stop: ptrTime(start.Add(time.Hour)),
		Duration: 3600, UpdatedAt: start, Deleted: true,
	})

	got, err := s.FindOverlapping(start, start.Add(time.Hour))
	if err != nil {
		t.Fatalf("FindOverlapping: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("FindOverlapping = %d entries, want 0 (deleted ignored)", len(got))
	}
}

// TestFindOverlappingRunningEntry pins the open-ended treatment of a running
// entry: it blocks anything reaching past its start, but not a range that ends
// at or before it.
func TestFindOverlappingRunningEntry(t *testing.T) {
	s := openTest(t)
	start := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)
	id := mustCreate(t, s, Entry{
		WorkspaceID: 1, Start: start, Duration: -1, UpdatedAt: start, Dirty: true,
	})

	// A range extending past the running start overlaps, even far in the future.
	for _, tc := range []struct {
		name        string
		start, stop time.Time
	}{
		{"overlaps head", start.Add(-30 * time.Minute), start.Add(30 * time.Minute)},
		{"after start", start.Add(2 * time.Hour), start.Add(3 * time.Hour)},
	} {
		got, err := s.FindOverlapping(tc.start, tc.stop)
		if err != nil {
			t.Fatalf("FindOverlapping %s: %v", tc.name, err)
		}
		if len(got) != 1 || got[0].ID != id {
			t.Errorf("FindOverlapping %s = %v, want the running entry %d", tc.name, got, id)
		}
	}

	// Ending exactly at the running start only touches it.
	got, err := s.FindOverlapping(start.Add(-time.Hour), start)
	if err != nil {
		t.Fatalf("FindOverlapping before: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("FindOverlapping before = %d entries, want 0", len(got))
	}
}

func TestEntryJoinsProjectColor(t *testing.T) {
	s := openTest(t)
	if err := s.ReplaceProjects([]Project{
		{ID: 1, WorkspaceID: 1, Name: "Backend", Color: "#0B83D9", Active: true},
	}); err != nil {
		t.Fatalf("replace projects: %v", err)
	}
	at := time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC)
	mustCreate(t, s, Entry{
		WorkspaceID: 1, ProjectID: ptrInt(1), Start: at,
		Stop: ptrTime(at.Add(time.Hour)), Duration: 3600, UpdatedAt: at,
	})

	got, err := s.EntriesBetween(at.Add(-time.Hour), at.Add(time.Hour))
	if err != nil || len(got) != 1 {
		t.Fatalf("between = %v err=%v, want 1 entry", got, err)
	}
	if got[0].ProjectName != "Backend" || got[0].ProjectColor != "#0B83D9" {
		t.Errorf("joined project = (%q, %q), want (Backend, #0B83D9)",
			got[0].ProjectName, got[0].ProjectColor)
	}
}

func TestDirtyEntriesAndMarkSynced(t *testing.T) {
	s := openTest(t)
	at := time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC)
	id := mustCreate(t, s, Entry{WorkspaceID: 1, Start: at, Duration: 3600, UpdatedAt: at, Dirty: true})

	dirty, err := s.DirtyEntries()
	if err != nil || len(dirty) != 1 {
		t.Fatalf("dirty = %v err=%v, want 1", dirty, err)
	}

	syncedAt := at.Add(time.Minute)
	if err := s.MarkSynced(id, 999, syncedAt); err != nil {
		t.Fatalf("mark synced: %v", err)
	}
	dirty, _ = s.DirtyEntries()
	if len(dirty) != 0 {
		t.Fatalf("dirty after sync = %d, want 0", len(dirty))
	}
	got, err := s.EntryByRemoteID(999)
	if err != nil || got == nil {
		t.Fatalf("by remote: %v err=%v", got, err)
	}
	if got.RemoteID == nil || *got.RemoteID != 999 {
		t.Errorf("remote_id = %v, want 999", got.RemoteID)
	}
	if got.SyncedAt == nil || !got.SyncedAt.Equal(syncedAt) {
		t.Errorf("synced_at = %v, want %v", got.SyncedAt, syncedAt)
	}
	if got.Dirty {
		t.Error("entry should be clean after MarkSynced")
	}
}

func TestCatalogFullReplace(t *testing.T) {
	s := openTest(t)
	if err := s.ReplaceProjects([]Project{{ID: 1, WorkspaceID: 1, Name: "Backend", Active: true}}); err != nil {
		t.Fatalf("replace projects: %v", err)
	}
	if err := s.ReplaceTasks([]Task{
		{ID: 10, WorkspaceID: 1, ProjectID: 1, Name: "Fix login bug", Active: true},
		{ID: 11, WorkspaceID: 1, ProjectID: 1, Name: "Old task", Active: true},
	}); err != nil {
		t.Fatalf("replace tasks: %v", err)
	}

	// Second replace must wipe the previous contents entirely.
	if err := s.ReplaceTasks([]Task{{ID: 12, WorkspaceID: 1, ProjectID: 1, Name: "Code review", Active: true}}); err != nil {
		t.Fatalf("replace tasks 2: %v", err)
	}
	all, err := s.activeTasks()
	if err != nil {
		t.Fatalf("active tasks: %v", err)
	}
	if len(all) != 1 || all[0].ID != 12 {
		t.Fatalf("tasks after replace = %+v, want only id 12", all)
	}
}

func TestReplaceProjectTasksScoped(t *testing.T) {
	s := openTest(t)
	if err := s.ReplaceTasks([]Task{
		{ID: 10, WorkspaceID: 1, ProjectID: 1, Name: "Backend A", Active: true},
		{ID: 11, WorkspaceID: 1, ProjectID: 1, Name: "Backend B", Active: true},
		{ID: 20, WorkspaceID: 1, ProjectID: 2, Name: "Payments A", Active: true},
	}); err != nil {
		t.Fatalf("seed tasks: %v", err)
	}

	// Replacing project 1's tasks must leave project 2 untouched.
	if err := s.ReplaceProjectTasks(1, []Task{
		{ID: 12, WorkspaceID: 1, ProjectID: 1, Name: "Backend C", Active: true},
	}); err != nil {
		t.Fatalf("replace project tasks: %v", err)
	}

	all, err := s.activeTasks()
	if err != nil {
		t.Fatalf("active tasks: %v", err)
	}
	ids := map[int64]bool{}
	for _, task := range all {
		ids[task.ID] = true
	}
	if len(all) != 2 || !ids[12] || !ids[20] {
		t.Fatalf("tasks after scoped replace = %+v, want only 12 (proj 1) and 20 (proj 2)", all)
	}
}

func TestPutProjectFullUpsert(t *testing.T) {
	s := openTest(t)
	if err := s.PutProject(Project{ID: 5, WorkspaceID: 1, Name: "Old", Active: true, Billable: false}); err != nil {
		t.Fatalf("put project: %v", err)
	}
	// A second put must fully overwrite mutable fields (name, billable, active).
	if err := s.PutProject(Project{ID: 5, WorkspaceID: 1, Name: "New", Active: false, Billable: true}); err != nil {
		t.Fatalf("put project 2: %v", err)
	}
	p, err := s.ProjectByID(5)
	if err != nil {
		t.Fatalf("project by id: %v", err)
	}
	if p == nil || p.Name != "New" || !p.Billable || p.Active {
		t.Fatalf("project = %+v, want New billable inactive", p)
	}
}

// TestUpsertProjectColor covers the self-heal upsert used by pulls: it must
// populate the color on first insert, keep an authoritative color when a later
// meta payload carries none (no clobber), and refresh it when a color is given.
func TestUpsertProjectColor(t *testing.T) {
	s := openTest(t)

	// First heal (from a meta pull) inserts the project with its color.
	if err := s.UpsertProject(Project{ID: 7, WorkspaceID: 1, Name: "Backend", Color: "#0B83D9", Active: true}); err != nil {
		t.Fatalf("upsert insert: %v", err)
	}
	p, err := s.ProjectByID(7)
	if err != nil || p == nil {
		t.Fatalf("project by id: %v (p=%v)", err, p)
	}
	if p.Color != "#0B83D9" {
		t.Fatalf("color after insert = %q, want %q", p.Color, "#0B83D9")
	}

	// A later heal without a color (empty) must NOT clobber the stored color.
	if err := s.UpsertProject(Project{ID: 7, WorkspaceID: 1, Name: "Backend", Color: "", Active: true}); err != nil {
		t.Fatalf("upsert empty color: %v", err)
	}
	p, _ = s.ProjectByID(7)
	if p.Color != "#0B83D9" {
		t.Fatalf("color after empty upsert = %q, want preserved %q", p.Color, "#0B83D9")
	}

	// A heal that carries a (new) color refreshes it.
	if err := s.UpsertProject(Project{ID: 7, WorkspaceID: 1, Name: "Backend", Color: "#E36A00", Active: true}); err != nil {
		t.Fatalf("upsert new color: %v", err)
	}
	p, _ = s.ProjectByID(7)
	if p.Color != "#E36A00" {
		t.Fatalf("color after new upsert = %q, want %q", p.Color, "#E36A00")
	}
}

func TestFindTasksByFragment(t *testing.T) {
	s := openTest(t)
	tasks := []Task{
		{ID: 1, WorkspaceID: 1, ProjectID: 100, Name: "Fix login bug", Active: true},
		{ID: 2, WorkspaceID: 1, ProjectID: 100, Name: "Fix", Active: true},
		{ID: 3, WorkspaceID: 1, ProjectID: 200, Name: "Fix payment", Active: true},
		{ID: 4, WorkspaceID: 1, ProjectID: 100, Name: "Inactive fix", Active: false},
	}
	if err := s.ReplaceTasks(tasks); err != nil {
		t.Fatalf("replace: %v", err)
	}

	// Substring: matches across projects, excludes inactive, sorted by name.
	// "Fi" is a substring of every active "Fix…" task but exactly equals none.
	got, _ := s.FindTasksByFragment("Fi", nil)
	if names := taskNames(got); !equal(names, []string{"Fix", "Fix login bug", "Fix payment"}) {
		t.Fatalf("substring match = %v", names)
	}

	// Exact title precedence: "Fix" wins over the broader substrings.
	got, _ = s.FindTasksByFragment("Fix", nil)
	if names := taskNames(got); !equal(names, []string{"Fix"}) {
		t.Fatalf("exact match = %v", names)
	}

	// Project scoping restricts candidates.
	pid := int64(200)
	got, _ = s.FindTasksByFragment("fix", &pid)
	if names := taskNames(got); !equal(names, []string{"Fix payment"}) {
		t.Fatalf("scoped match = %v", names)
	}

	// No match.
	if got, _ := s.FindTasksByFragment("nonexistent", nil); len(got) != 0 {
		t.Fatalf("expected no matches, got %v", taskNames(got))
	}
}

func TestListProjects(t *testing.T) {
	s := openTest(t)
	if err := s.ReplaceProjects([]Project{
		{ID: 2, WorkspaceID: 1, Name: "Payments", Active: true},
		{ID: 1, WorkspaceID: 1, Name: "Backend", Active: true},
		{ID: 3, WorkspaceID: 1, Name: "Archived", Active: false},
	}); err != nil {
		t.Fatalf("replace projects: %v", err)
	}

	// Active-only, ordered by name.
	got, err := s.ListProjects(false)
	if err != nil {
		t.Fatalf("list projects: %v", err)
	}
	if len(got) != 2 || got[0].Name != "Backend" || got[1].Name != "Payments" {
		t.Fatalf("active projects = %+v, want [Backend Payments]", got)
	}

	// --all includes inactive.
	all, err := s.ListProjects(true)
	if err != nil {
		t.Fatalf("list projects --all: %v", err)
	}
	if len(all) != 3 || all[0].Name != "Archived" {
		t.Fatalf("all projects = %+v, want 3 incl. Archived", all)
	}
}

func TestListTasksProjectScope(t *testing.T) {
	s := openTest(t)
	if err := s.ReplaceProjects([]Project{
		{ID: 100, WorkspaceID: 1, Name: "Backend", Active: true},
		{ID: 200, WorkspaceID: 1, Name: "Payments", Active: true},
	}); err != nil {
		t.Fatalf("replace projects: %v", err)
	}
	if err := s.ReplaceTasks([]Task{
		{ID: 1, WorkspaceID: 1, ProjectID: 100, Name: "Fix login bug", Active: true},
		{ID: 2, WorkspaceID: 1, ProjectID: 100, Name: "Code review", Active: true},
		{ID: 3, WorkspaceID: 1, ProjectID: 200, Name: "Payment fix", Active: true},
	}); err != nil {
		t.Fatalf("replace tasks: %v", err)
	}

	// Unscoped: every active task.
	all, err := s.ListTasks(false, nil)
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("tasks = %d, want 3", len(all))
	}

	// Scoped to project 200: only its tasks.
	pid := int64(200)
	scoped, err := s.ListTasks(false, &pid)
	if err != nil {
		t.Fatalf("list tasks scoped: %v", err)
	}
	if len(scoped) != 1 || scoped[0].ID != 3 {
		t.Fatalf("scoped tasks = %+v, want only task 3 (Payment fix)", scoped)
	}
}

func TestFindProjectsByFragment(t *testing.T) {
	s := openTest(t)
	if err := s.ReplaceProjects([]Project{
		{ID: 1, WorkspaceID: 1, Name: "Backend", Active: true},
		{ID: 2, WorkspaceID: 1, Name: "Back office", Active: true},
		{ID: 3, WorkspaceID: 1, Name: "Payments", Active: true},
		{ID: 4, WorkspaceID: 1, Name: "Backup", Active: false},
	}); err != nil {
		t.Fatalf("replace projects: %v", err)
	}

	// Substring: matches active projects across the catalog, sorted by name,
	// excluding the inactive "Backup".
	got, _ := s.FindProjectsByFragment("back")
	if names := projectNames(got); !equal(names, []string{"Back office", "Backend"}) {
		t.Fatalf("substring match = %v", names)
	}

	// Exact full-name precedence over broader substrings.
	got, _ = s.FindProjectsByFragment("Backend")
	if names := projectNames(got); !equal(names, []string{"Backend"}) {
		t.Fatalf("exact match = %v", names)
	}

	// Unique fragment.
	got, _ = s.FindProjectsByFragment("pay")
	if len(got) != 1 || got[0].ID != 3 {
		t.Fatalf("unique match = %+v, want project 3", got)
	}

	// No match.
	if got, _ := s.FindProjectsByFragment("nonexistent"); len(got) != 0 {
		t.Fatalf("expected no matches, got %v", projectNames(got))
	}
}

func TestMetaRoundTrip(t *testing.T) {
	s := openTest(t)
	if _, ok, _ := s.GetMeta(MetaLastPull); ok {
		t.Fatal("last_pull should be absent initially")
	}
	if err := s.SetMeta(MetaLastPull, "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := s.SetMeta(MetaLastPull, "2026-02-01T00:00:00Z"); err != nil {
		t.Fatalf("update: %v", err)
	}
	v, ok, err := s.GetMeta(MetaLastPull)
	if err != nil || !ok || v != "2026-02-01T00:00:00Z" {
		t.Fatalf("get = %q ok=%v err=%v", v, ok, err)
	}
}

// --- helpers ---

func ptrTime(t time.Time) *time.Time { return &t }

func taskNames(tasks []Task) []string {
	out := make([]string, len(tasks))
	for i, t := range tasks {
		out[i] = t.Name
	}
	return out
}

func projectNames(projects []Project) []string {
	out := make([]string, len(projects))
	for i, p := range projects {
		out[i] = p.Name
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
