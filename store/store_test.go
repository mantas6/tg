package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ctx is the context every store call in these tests runs under.
var ctx = context.Background()

func ptrInt(v int64) *int64 { return &v }

// openTest opens a throwaway store pinned to UTC, so the calendar the store
// reckons entry days (and therefore the per-day numbering) in matches the UTC
// timestamps the fixtures below use, whatever the test machine's zone is.
func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := OpenIn(ctx, filepath.Join(t.TempDir(), "tg.db"), time.UTC)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mustCreate(t *testing.T, s *Store, e Entry) int64 {
	t.Helper()
	id, err := s.CreateEntry(ctx, e)
	if err != nil {
		t.Fatalf("create entry: %v", err)
	}
	return id
}

func TestMigrateIdempotent(t *testing.T) {
	s := openTest(t)
	// Re-running migrate must not error or wipe data.
	if err := s.migrate(ctx); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if v, ok, _ := s.Meta(ctx, MetaSchemaVersion); !ok || v != schemaVersion {
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
	s, err := OpenIn(ctx, path, time.UTC)
	if err != nil {
		t.Fatalf("open (migrate): %v", err)
	}
	t.Cleanup(func() { s.Close() })

	for _, tbl := range []string{"entries", "projects"} {
		has, err := s.hasColumn(ctx, tbl, "billable")
		if err != nil {
			t.Fatalf("hasColumn %s: %v", tbl, err)
		}
		if !has {
			t.Errorf("%s.billable missing after migrate", tbl)
		}
	}
	if v, ok, _ := s.Meta(ctx, MetaSchemaVersion); !ok || v != schemaVersion {
		t.Errorf("schema_version = %q ok=%v, want %q", v, ok, schemaVersion)
	}

	// The migrated store round-trips billable on both projects and entries.
	if err := s.ReplaceProjects(ctx, []Project{
		{ID: 1, WorkspaceID: 1, Name: "P", Active: true, Billable: true},
	}); err != nil {
		t.Fatalf("replace projects: %v", err)
	}
	got, err := s.ProjectByID(ctx, 1)
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
	got, err := s.EntryByRemoteID(ctx, 42)
	if err != nil || got == nil {
		t.Fatalf("by remote: %v err=%v", got, err)
	}
	if !got.Billable {
		t.Errorf("Billable = false, want true")
	}
}

func TestProjectByIDMissing(t *testing.T) {
	s := openTest(t)
	got, err := s.ProjectByID(ctx, 999)
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

	r, err := s.Running(ctx)
	if err != nil {
		t.Fatalf("Running: %v", err)
	}
	if r == nil || r.ID != newest {
		t.Fatalf("Running = %+v, want id %d", r, newest)
	}

	if _, err := s.db.Exec("UPDATE entries SET deleted = 1 WHERE id = ?", newest); err != nil {
		t.Fatal(err)
	}
	r, err = s.Running(ctx)
	if err != nil {
		t.Fatalf("Running (deleted): %v", err)
	}
	if r == nil || !r.Start.Equal(base) {
		t.Fatalf("Running (deleted) = %+v, want the 08:00 entry", r)
	}
}

// TestRunningPredicateIsUnified pins the one definition of "running" tg has:
// Entry.Running, Store.Running's SQL and the overlap guard must all answer the
// same for the same row. Store.Running used to test stop IS NULL alone, so a
// pulled row carrying only Toggl's negative-duration marker was invisible to
// `tg status` yet open-ended to `tg add`'s overlap check.
func TestRunningPredicateIsUnified(t *testing.T) {
	start := time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		e    Entry
		want bool
	}{
		{"no stop", Entry{WorkspaceID: 1, Start: start, Duration: 3600, UpdatedAt: start}, true},
		{"negative duration", Entry{
			WorkspaceID: 1, Start: start, Stop: ptrTime(start.Add(time.Hour)),
			Duration: -1, UpdatedAt: start,
		}, true},
		{"finished", Entry{
			WorkspaceID: 1, Start: start, Stop: ptrTime(start.Add(time.Hour)),
			Duration: 3600, UpdatedAt: start,
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openTest(t)
			id := mustCreate(t, s, tc.e)

			if got := tc.e.Running(); got != tc.want {
				t.Errorf("Entry.Running() = %v, want %v", got, tc.want)
			}

			r, err := s.Running(ctx)
			if err != nil {
				t.Fatalf("Running: %v", err)
			}
			if got := r != nil && r.ID == id; got != tc.want {
				t.Errorf("Store.Running() = %+v, want running=%v", r, tc.want)
			}

			// The overlap guard reads the same predicate: a running entry has no
			// end, so it still conflicts with a range hours later.
			over, err := s.FindOverlapping(ctx, start.Add(4*time.Hour), start.Add(5*time.Hour))
			if err != nil {
				t.Fatalf("FindOverlapping: %v", err)
			}
			if got := len(over) == 1 && over[0].ID == id; got != tc.want {
				t.Errorf("FindOverlapping = %+v, want open-ended=%v", over, tc.want)
			}
		})
	}
}

// TestUpdateFromRemoteUnknownRemoteID pins that a remote id nothing mirrors is
// an error wrapping ErrEntryNotFound, not a silent no-op: sync counts the result
// of this call, so an UPDATE that matched no row must not pass for an update.
func TestUpdateFromRemoteUnknownRemoteID(t *testing.T) {
	s := openTest(t)
	start := time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC)
	remote := Entry{
		RemoteID: ptrInt(4242), WorkspaceID: 1, Start: start,
		Stop: ptrTime(start.Add(time.Hour)), Duration: 3600,
		UpdatedAt: start, SyncedAt: ptrTime(start),
	}

	err := s.UpdateFromRemote(ctx, remote)
	if !errors.Is(err, ErrEntryNotFound) {
		t.Fatalf("UpdateFromRemote(unknown) = %v, want ErrEntryNotFound", err)
	}
	if !strings.Contains(err.Error(), "4242") {
		t.Errorf("error = %v, want it to name remote id 4242", err)
	}
	// Nothing was written, so the miss cannot be mistaken for a stored entry.
	if got, err := s.EntryByRemoteID(ctx, 4242); err != nil || got != nil {
		t.Errorf("EntryByRemoteID = %+v err=%v, want no row", got, err)
	}

	// An entry with no remote id at all cannot be matched either.
	remote.RemoteID = nil
	if err := s.UpdateFromRemote(ctx, remote); !errors.Is(err, ErrEntryNotFound) {
		t.Errorf("UpdateFromRemote(no remote id) = %v, want ErrEntryNotFound", err)
	}

	// The known-id path still succeeds, so the guard only rejects real misses.
	mustCreate(t, s, Entry{
		RemoteID: ptrInt(4242), WorkspaceID: 1, Description: "old", Start: start,
		Stop: ptrTime(start.Add(time.Hour)), Duration: 3600, UpdatedAt: start,
	})
	remote.RemoteID = ptrInt(4242)
	remote.Description = "new"
	if err := s.UpdateFromRemote(ctx, remote); err != nil {
		t.Fatalf("UpdateFromRemote(known): %v", err)
	}
	got, err := s.EntryByRemoteID(ctx, 4242)
	if err != nil || got == nil || got.Description != "new" {
		t.Errorf("entry = %+v err=%v, want the remote description", got, err)
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

	got, err := s.EntriesBetween(ctx, day, day.Add(24*time.Hour))
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

// TestLastEntry covers the shared resolution behind `tg status` and a bare
// `tg mod`: the newest already-started entry of now's day wins, deleted entries
// are skipped, and an empty store yields nil.
func TestLastEntry(t *testing.T) {
	s := openTest(t)

	day := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	now := day.Add(16 * time.Hour)

	got, err := s.LastEntry(ctx, now)
	if err != nil {
		t.Fatalf("LastEntry (empty): %v", err)
	}
	if got != nil {
		t.Fatalf("LastEntry (empty) = %+v, want nil", got)
	}

	mk := func(start time.Time) int64 {
		return mustCreate(t, s, Entry{
			WorkspaceID: 1, Start: start, Stop: ptrTime(start.Add(time.Hour)),
			Duration: 3600, UpdatedAt: start,
		})
	}
	mk(day.Add(9 * time.Hour))
	mk(day.AddDate(0, 0, -3).Add(9 * time.Hour)) // an older day
	newest := mk(day.Add(14 * time.Hour))

	got, err = s.LastEntry(ctx, now)
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
	got, err = s.LastEntry(ctx, now)
	if err != nil {
		t.Fatalf("LastEntry (deleted): %v", err)
	}
	if got == nil || !got.Start.Equal(day.Add(9*time.Hour)) {
		t.Fatalf("LastEntry (deleted) = %+v, want the 09:00 entry", got)
	}
}

// TestLastEntryIsTodayOnly pins the day scope: yesterday's entry is never the
// last entry, however recent it is, so a day that has tracked nothing yet has
// no last entry at all (rather than reaching back into history, which `tg mod`
// may not edit anyway).
func TestLastEntryIsTodayOnly(t *testing.T) {
	s := openTest(t)

	day := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	yesterdayEvening := day.Add(-2 * time.Hour) // 2026-01-01 22:00
	mustCreate(t, s, Entry{
		WorkspaceID: 1, Start: yesterdayEvening, Stop: ptrTime(yesterdayEvening.Add(time.Hour)),
		Duration: 3600, UpdatedAt: yesterdayEvening,
	})

	// Just after midnight, yesterday's entry is already out of reach.
	got, err := s.LastEntry(ctx, day.Add(30*time.Minute))
	if err != nil {
		t.Fatalf("LastEntry: %v", err)
	}
	if got != nil {
		t.Fatalf("LastEntry = %+v, want nil (yesterday is history)", got)
	}

	// Anything tracked today becomes the last entry immediately, and the
	// boundary is midnight itself.
	midnight := mustCreate(t, s, Entry{
		WorkspaceID: 1, Start: day, Stop: ptrTime(day.Add(time.Hour)),
		Duration: 3600, UpdatedAt: day,
	})
	got, err = s.LastEntry(ctx, day.Add(30*time.Minute))
	if err != nil {
		t.Fatalf("LastEntry: %v", err)
	}
	if got == nil || got.ID != midnight {
		t.Fatalf("LastEntry = %+v, want id %d (today's 00:00 entry)", got, midnight)
	}

	// The same store, asked on the following day, is empty again.
	got, err = s.LastEntry(ctx, day.AddDate(0, 0, 1).Add(9*time.Hour))
	if err != nil {
		t.Fatalf("LastEntry (next day): %v", err)
	}
	if got != nil {
		t.Fatalf("LastEntry (next day) = %+v, want nil", got)
	}
}

// TestLastEntryIgnoresFutureStarts pins the other filter: an entry booked for
// later today has not happened yet, so it is skipped by its START datetime and
// the entry before it is still the last one. An entry starting exactly at now
// has begun and counts.
func TestLastEntryIgnoresFutureStarts(t *testing.T) {
	s := openTest(t)

	day := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	now := day.Add(12 * time.Hour)

	earlier := mustCreate(t, s, Entry{
		WorkspaceID: 1, Start: day.Add(9 * time.Hour),
		Stop: ptrTime(day.Add(10 * time.Hour)), Duration: 3600, UpdatedAt: now,
	})
	// Three hours from now, same day: still in the future.
	future := day.Add(15 * time.Hour)
	mustCreate(t, s, Entry{
		WorkspaceID: 1, Start: future, Stop: ptrTime(future.Add(time.Hour)),
		Duration: 3600, UpdatedAt: now,
	})

	got, err := s.LastEntry(ctx, now)
	if err != nil {
		t.Fatalf("LastEntry: %v", err)
	}
	if got == nil || got.ID != earlier {
		t.Fatalf("LastEntry = %+v, want id %d (the future entry must be filtered out)", got, earlier)
	}

	// Once now reaches its start, the later entry takes over.
	got, err = s.LastEntry(ctx, future)
	if err != nil {
		t.Fatalf("LastEntry (at its start): %v", err)
	}
	if got == nil || !got.Start.Equal(future) {
		t.Fatalf("LastEntry (at its start) = %+v, want the 15:00 entry", got)
	}
}

// TestEntrySeqPerDay covers the numbering handed out at insert time: entries
// are numbered in insertion order, the sequence restarts at 1 on every calendar
// day, and EntryByNum resolves a number back to its entry (with the catalog
// joins intact) on the day it was given out on.
func TestEntrySeqPerDay(t *testing.T) {
	s := openTest(t)
	if err := s.ReplaceProjects(ctx, []Project{{ID: 1, WorkspaceID: 1, Name: "Backend", Active: true}}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceTasks(ctx, []Task{{ID: 10, WorkspaceID: 1, ProjectID: 1, Name: "Fix login bug", Active: true}}); err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	next := day.AddDate(0, 0, 1)
	mk := func(base time.Time, h int) int64 {
		start := base.Add(time.Duration(h) * time.Hour)
		return mustCreate(t, s, Entry{
			WorkspaceID: 1, ProjectID: ptrInt(1), TaskID: ptrInt(10), Start: start,
			Stop: ptrTime(start.Add(time.Hour)), Duration: 3600, UpdatedAt: start,
		})
	}
	first, second, third := mk(day, 9), mk(day, 11), mk(day, 13)
	tomorrow := mk(next, 9)

	for num, wantID := range map[int]int64{1: first, 2: second, 3: third} {
		got, err := s.EntryByNum(ctx, num, day)
		if err != nil {
			t.Fatalf("EntryByNum(%d): %v", num, err)
		}
		if got.ID != wantID {
			t.Errorf("EntryByNum(%d).ID = %d, want %d", num, got.ID, wantID)
		}
		if got.Seq != num {
			t.Errorf("EntryByNum(%d).Seq = %d, want %d", num, got.Seq, num)
		}
	}

	// The next day starts its own sequence at 1 rather than continuing.
	got, err := s.EntryByNum(ctx, 1, next)
	if err != nil {
		t.Fatalf("EntryByNum(1) on the next day: %v", err)
	}
	if got.ID != tomorrow {
		t.Errorf("EntryByNum(1, next day).ID = %d, want %d", got.ID, tomorrow)
	}

	// The joined display fields come along, as they do for every entry read.
	got, err = s.EntryByNum(ctx, 1, day)
	if err != nil {
		t.Fatal(err)
	}
	if got.TaskName != "Fix login bug" || got.ProjectName != "Backend" {
		t.Errorf("EntryByNum(1) joins = %q/%q, want task and project names", got.TaskName, got.ProjectName)
	}

	for _, num := range []int{0, 4, -1} {
		if _, err := s.EntryByNum(ctx, num, day); !errors.Is(err, ErrNoEntryNum) {
			t.Errorf("EntryByNum(%d) error = %v, want ErrNoEntryNum", num, err)
		}
	}
	if _, err := s.EntryByNum(ctx, 4, day); err == nil || !strings.Contains(err.Error(), "tg ls") {
		t.Errorf("EntryByNum error = %v, want it to point at `tg ls`", err)
	}
	// A day that has never held an entry resolves nothing either.
	if _, err := s.EntryByNum(ctx, 1, day.AddDate(0, 0, -1)); !errors.Is(err, ErrNoEntryNum) {
		t.Errorf("EntryByNum(1) on an empty day = %v, want ErrNoEntryNum", err)
	}
}

// TestEntrySeqSurvivesDeletion is the heart of the persistent numbering:
// deleting an entry retires its number instead of sliding the later entries
// down, and the number is not hand out again to the next insert.
func TestEntrySeqSurvivesDeletion(t *testing.T) {
	s := openTest(t)
	day := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	mk := func(h int) int64 {
		start := day.Add(time.Duration(h) * time.Hour)
		return mustCreate(t, s, Entry{
			WorkspaceID: 1, Start: start, Stop: ptrTime(start.Add(time.Hour)),
			Duration: 3600, UpdatedAt: start,
		})
	}
	first, second, third := mk(9), mk(11), mk(13)

	if err := s.SoftDeleteEntry(ctx, second, day.Add(20*time.Hour)); err != nil {
		t.Fatalf("SoftDeleteEntry: %v", err)
	}

	// The survivors keep the numbers they were listed under...
	for num, wantID := range map[int]int64{1: first, 3: third} {
		got, err := s.EntryByNum(ctx, num, day)
		if err != nil {
			t.Fatalf("EntryByNum(%d) after delete: %v", num, err)
		}
		if got.ID != wantID {
			t.Errorf("EntryByNum(%d).ID = %d, want %d (numbers must not shift)", num, got.ID, wantID)
		}
	}
	// ...and the freed number stays vacant rather than resolving to a
	// neighbour.
	if _, err := s.EntryByNum(ctx, 2, day); !errors.Is(err, ErrNoEntryNum) {
		t.Errorf("EntryByNum(2) after delete = %v, want ErrNoEntryNum", err)
	}

	// A hard delete (what a pushed deletion leaves behind) behaves the same,
	// and the next insert continues past the highest number ever used.
	if err := s.DeleteRow(ctx, second); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EntryByNum(ctx, 2, day); !errors.Is(err, ErrNoEntryNum) {
		t.Errorf("EntryByNum(2) after row removal = %v, want ErrNoEntryNum", err)
	}
	fourth := mk(15)
	got, err := s.EntryByNum(ctx, 4, day)
	if err != nil || got.ID != fourth {
		t.Fatalf("EntryByNum(4) = %+v err=%v, want the new entry %d", got, err, fourth)
	}
}

// TestEntryByNumIgnoresOtherDays keeps a number from reaching across days: the
// same number exists on both days and each resolves to its own entry.
func TestEntryByNumIgnoresOtherDays(t *testing.T) {
	s := openTest(t)
	day := time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC)
	older := day.AddDate(0, 0, -1)
	oldID := mustCreate(t, s, Entry{
		WorkspaceID: 1, Start: older, Stop: ptrTime(older.Add(time.Hour)),
		Duration: 3600, UpdatedAt: older,
	})
	newID := mustCreate(t, s, Entry{
		WorkspaceID: 1, Start: day, Stop: ptrTime(day.Add(time.Hour)),
		Duration: 3600, UpdatedAt: day,
	})

	for _, tc := range []struct {
		when time.Time
		want int64
	}{{older, oldID}, {day, newID}} {
		got, err := s.EntryByNum(ctx, 1, tc.when)
		if err != nil {
			t.Fatalf("EntryByNum(1, %s): %v", tc.when.Format("2006-01-02"), err)
		}
		if got.ID != tc.want {
			t.Errorf("EntryByNum(1, %s).ID = %d, want %d",
				tc.when.Format("2006-01-02"), got.ID, tc.want)
		}
	}
	// The error names the day that was searched, so a number that only exists
	// on another day does not read as a mystery.
	_, err := s.EntryByNum(ctx, 2, day)
	if !errors.Is(err, ErrNoEntryNum) || !strings.Contains(err.Error(), "2026-01-02") {
		t.Errorf("EntryByNum(2) = %v, want ErrNoEntryNum naming 2026-01-02", err)
	}
}

// TestUpdateFromRemoteRenumbersOnDayChange covers the one edit that can move an
// entry to another calendar day: it must join the new day's numbering instead
// of keeping a number that day never handed out.
func TestUpdateFromRemoteRenumbersOnDayChange(t *testing.T) {
	s := openTest(t)
	day := time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC)
	next := day.AddDate(0, 0, 1)

	// Two entries on the target day, so the moved one must land on 3.
	mustCreate(t, s, Entry{WorkspaceID: 1, Start: next, Stop: ptrTime(next.Add(time.Hour)), Duration: 3600, UpdatedAt: next})
	mustCreate(t, s, Entry{WorkspaceID: 1, Start: next.Add(2 * time.Hour), Stop: ptrTime(next.Add(3 * time.Hour)), Duration: 3600, UpdatedAt: next})
	moved := mustCreate(t, s, Entry{
		RemoteID: ptrInt(77), WorkspaceID: 1, Start: day, Stop: ptrTime(day.Add(time.Hour)),
		Duration: 3600, UpdatedAt: day,
	})

	newStart := next.Add(5 * time.Hour)
	if err := s.UpdateFromRemote(ctx, Entry{
		RemoteID: ptrInt(77), WorkspaceID: 1, Start: newStart,
		Stop: ptrTime(newStart.Add(time.Hour)), Duration: 3600,
		UpdatedAt: newStart, SyncedAt: ptrTime(newStart),
	}); err != nil {
		t.Fatalf("UpdateFromRemote: %v", err)
	}

	got, err := s.EntryByNum(ctx, 3, next)
	if err != nil || got.ID != moved {
		t.Fatalf("EntryByNum(3, next day) = %+v err=%v, want the moved entry %d", got, err, moved)
	}
	if _, err := s.EntryByNum(ctx, 1, day); !errors.Is(err, ErrNoEntryNum) {
		t.Errorf("EntryByNum(1) on the vacated day = %v, want ErrNoEntryNum", err)
	}

	// An update that leaves the entry on its day keeps its number.
	sameDayStart := next.Add(6 * time.Hour)
	if err := s.UpdateFromRemote(ctx, Entry{
		RemoteID: ptrInt(77), WorkspaceID: 1, Start: sameDayStart,
		Stop: ptrTime(sameDayStart.Add(time.Hour)), Duration: 3600,
		UpdatedAt: sameDayStart, SyncedAt: ptrTime(sameDayStart),
	}); err != nil {
		t.Fatalf("UpdateFromRemote (same day): %v", err)
	}
	if got, err := s.EntryByNum(ctx, 3, next); err != nil || got.ID != moved {
		t.Errorf("EntryByNum(3) = %+v err=%v, want the number kept", got, err)
	}
}

// TestMigrateBackfillsSeq covers the v4 upgrade of a pre-existing database: the
// seq column is added in place and every existing row is numbered in insertion
// (id) order, restarting per calendar day, so numbering works without a reset.
// The retired entry_refs table is dropped.
func TestMigrateBackfillsSeq(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tg.db")

	// Seed a v3 database: entries + entry_refs + meta, but no seq column. Two
	// entries on 2026-01-02 and one on 2026-01-03, inserted out of start order
	// so the back-fill has to follow ids rather than clocks.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := raw.Exec(`
CREATE TABLE entries (
  id INTEGER PRIMARY KEY AUTOINCREMENT, remote_id INTEGER UNIQUE,
  workspace_id INTEGER NOT NULL, project_id INTEGER, task_id INTEGER,
  description TEXT NOT NULL DEFAULT '', start TEXT NOT NULL, stop TEXT,
  duration INTEGER NOT NULL, billable INTEGER NOT NULL DEFAULT 0,
  updated_at TEXT NOT NULL, synced_at TEXT,
  dirty INTEGER NOT NULL DEFAULT 1, deleted INTEGER NOT NULL DEFAULT 0);
CREATE TABLE entry_refs (num INTEGER PRIMARY KEY, entry_id INTEGER NOT NULL);
CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT);
INSERT INTO entries (id, workspace_id, description, start, duration, updated_at)
VALUES (1, 1, 'late',  '2026-01-02T13:00:00Z', 3600, '2026-01-02T13:00:00Z'),
       (2, 1, 'early', '2026-01-02T09:00:00Z', 3600, '2026-01-02T09:00:00Z'),
       (3, 1, 'next',  '2026-01-03T09:00:00Z', 3600, '2026-01-03T09:00:00Z');
INSERT INTO entry_refs (num, entry_id) VALUES (1, 2), (2, 1);
INSERT INTO meta (key, value) VALUES ('schema_version', '3');`); err != nil {
		t.Fatalf("seed v3 schema: %v", err)
	}
	raw.Close()

	s, err := OpenIn(ctx, path, time.UTC)
	if err != nil {
		t.Fatalf("open (migrate): %v", err)
	}
	t.Cleanup(func() { s.Close() })

	if v, ok, _ := s.Meta(ctx, MetaSchemaVersion); !ok || v != schemaVersion {
		t.Errorf("schema_version = %q ok=%v, want %q", v, ok, schemaVersion)
	}
	has, err := s.hasColumn(ctx, "entries", "seq")
	if err != nil || !has {
		t.Fatalf("entries.seq missing after migrate (err=%v)", err)
	}
	// The dead mapping table is gone.
	if _, err := s.db.Exec("SELECT 1 FROM entry_refs"); err == nil {
		t.Error("entry_refs still exists after migrate, want it dropped")
	}

	day := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	for num, wantID := range map[int]int64{1: 1, 2: 2} {
		got, err := s.EntryByNum(ctx, num, day)
		if err != nil {
			t.Fatalf("EntryByNum(%d): %v", num, err)
		}
		if got.ID != wantID {
			t.Errorf("EntryByNum(%d).ID = %d, want %d (id order, not start order)", num, got.ID, wantID)
		}
	}
	// The second day restarts at 1.
	got, err := s.EntryByNum(ctx, 1, day.AddDate(0, 0, 1))
	if err != nil || got.ID != 3 {
		t.Fatalf("EntryByNum(1) on 2026-01-03 = %+v err=%v, want id 3", got, err)
	}

	// A fresh insert continues the back-filled day rather than colliding.
	start := time.Date(2026, 1, 2, 17, 0, 0, 0, time.UTC)
	id := mustCreate(t, s, Entry{
		WorkspaceID: 1, Start: start, Stop: ptrTime(start.Add(time.Hour)),
		Duration: 3600, UpdatedAt: start,
	})
	if got, err := s.EntryByNum(ctx, 3, day); err != nil || got.ID != id {
		t.Fatalf("EntryByNum(3) = %+v err=%v, want the new entry %d", got, err, id)
	}

	// Re-running migrate must not renumber anything.
	if err := s.migrate(ctx); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if got, err := s.EntryByNum(ctx, 1, day); err != nil || got.ID != 1 {
		t.Errorf("EntryByNum(1) after a second migrate = %+v err=%v, want id 1", got, err)
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
			got, err := s.FindOverlapping(ctx, tc.start, tc.stop)
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

	got, err := s.FindOverlapping(ctx, start, start.Add(time.Hour))
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
		got, err := s.FindOverlapping(ctx, tc.start, tc.stop)
		if err != nil {
			t.Fatalf("FindOverlapping %s: %v", tc.name, err)
		}
		if len(got) != 1 || got[0].ID != id {
			t.Errorf("FindOverlapping %s = %v, want the running entry %d", tc.name, got, id)
		}
	}

	// Ending exactly at the running start only touches it.
	got, err := s.FindOverlapping(ctx, start.Add(-time.Hour), start)
	if err != nil {
		t.Fatalf("FindOverlapping before: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("FindOverlapping before = %d entries, want 0", len(got))
	}
}

// TestFindOverlappingExcluding pins the `tg mod` variant: the excluded entry is
// invisible to the search (so retiming an entry never conflicts with itself)
// while every other entry still blocks the range.
func TestFindOverlappingExcluding(t *testing.T) {
	s := openTest(t)
	day := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	at := func(h, m int) time.Time { return day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute) }
	first := mustCreate(t, s, Entry{
		WorkspaceID: 1, Start: at(9, 0), Stop: ptrTime(at(10, 0)),
		Duration: 3600, UpdatedAt: at(9, 0),
	})
	second := mustCreate(t, s, Entry{
		WorkspaceID: 1, Start: at(10, 0), Stop: ptrTime(at(11, 0)),
		Duration: 3600, UpdatedAt: at(10, 0),
	})

	// Excluding the first entry hides it from a range it fully covers.
	got, err := s.FindOverlappingExcluding(ctx, at(9, 0), at(10, 0), first)
	if err != nil {
		t.Fatalf("FindOverlappingExcluding: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d entries, want 0 (the excluded entry itself)", len(got))
	}

	// The other entry is still reported when the range grows into it.
	got, err = s.FindOverlappingExcluding(ctx, at(9, 0), at(10, 30), first)
	if err != nil {
		t.Fatalf("FindOverlappingExcluding: %v", err)
	}
	if len(got) != 1 || got[0].ID != second {
		t.Fatalf("got %v, want the second entry %d", got, second)
	}

	// FindOverlapping is the same search with nothing excluded.
	got, err = s.FindOverlapping(ctx, at(9, 0), at(10, 30))
	if err != nil {
		t.Fatalf("FindOverlapping: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("FindOverlapping = %d entries, want both", len(got))
	}
}

// TestUpdateEntry pins `tg mod`'s write: the editable fields change, the LWW
// clock advances, the row is marked dirty, and the remote identity (remote_id,
// synced_at) is preserved so the push is an update rather than a re-create.
func TestUpdateEntry(t *testing.T) {
	s := openTest(t)
	at := time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC)
	stop := at.Add(time.Hour)
	id := mustCreate(t, s, Entry{
		RemoteID: ptrInt(4242), WorkspaceID: 1, Description: "old",
		Start: at, Stop: ptrTime(stop), Duration: 3600,
		UpdatedAt: at, SyncedAt: ptrTime(at), Dirty: false,
	})

	newStart := at.Add(30 * time.Minute)
	newStop := newStart.Add(45 * time.Minute)
	modAt := time.Date(2026, 1, 2, 15, 0, 0, 0, time.UTC)
	if err := s.UpdateEntry(ctx, Entry{
		ID: id, Description: "new", Start: newStart, Stop: &newStop,
		Duration: 2700, UpdatedAt: modAt,
	}, modAt, time.UTC); err != nil {
		t.Fatalf("UpdateEntry: %v", err)
	}

	dirty, err := s.DirtyEntries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(dirty) != 1 {
		t.Fatalf("dirty entries = %d, want 1", len(dirty))
	}
	got := dirty[0]
	if got.Description != "new" {
		t.Errorf("description = %q, want %q", got.Description, "new")
	}
	if !got.Start.Equal(newStart) {
		t.Errorf("start = %v, want %v", got.Start, newStart)
	}
	if got.Stop == nil || !got.Stop.Equal(newStop) {
		t.Errorf("stop = %v, want %v", got.Stop, newStop)
	}
	if got.Duration != 2700 {
		t.Errorf("duration = %d, want 2700", got.Duration)
	}
	if !got.UpdatedAt.Equal(modAt) {
		t.Errorf("updated_at = %v, want %v", got.UpdatedAt, modAt)
	}
	if got.RemoteID == nil || *got.RemoteID != 4242 {
		t.Errorf("remote_id = %v, want 4242 preserved", got.RemoteID)
	}
	if got.SyncedAt == nil || !got.SyncedAt.Equal(at) {
		t.Errorf("synced_at = %v, want %v preserved", got.SyncedAt, at)
	}
	if got.Deleted {
		t.Error("deleted flag must not be touched by an update")
	}
}

// TestUpdateEntryMissing verifies an update against an unknown id fails loudly
// instead of silently matching no row.
func TestUpdateEntryMissing(t *testing.T) {
	s := openTest(t)
	at := time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC)
	err := s.UpdateEntry(ctx, Entry{ID: 999, Start: at, Duration: 60, UpdatedAt: at}, at, time.UTC)
	if err == nil {
		t.Fatal("UpdateEntry on a missing id = nil error, want an error")
	}
	if !strings.Contains(err.Error(), "999") {
		t.Errorf("err = %v, want it to name the id", err)
	}
}

// TestUpdateEntryRefusesOlderThanToday pins the failsafe at its lowest level:
// the store itself refuses to rewrite an entry that sits on an earlier calendar
// day, no matter what the caller asks for, and leaves the row untouched.
func TestUpdateEntryRefusesOlderThanToday(t *testing.T) {
	s := openTest(t)
	at := time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC)
	stop := at.Add(time.Hour)
	id := mustCreate(t, s, Entry{
		RemoteID: ptrInt(4242), WorkspaceID: 1, Description: "old",
		Start: at, Stop: ptrTime(stop), Duration: 3600,
		UpdatedAt: at, SyncedAt: ptrTime(at), Dirty: false,
	})

	// One day later the entry is history and must not be editable.
	now := time.Date(2026, 1, 3, 10, 0, 0, 0, time.UTC)
	newStop := at.Add(30 * time.Minute)
	err := s.UpdateEntry(ctx, Entry{
		ID: id, Description: "new", Start: at, Stop: &newStop,
		Duration: 1800, UpdatedAt: now,
	}, now, time.UTC)
	if !errors.Is(err, ErrEntryTooOld) {
		t.Fatalf("err = %v, want ErrEntryTooOld", err)
	}

	dirty, err := s.DirtyEntries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(dirty) != 0 {
		t.Errorf("dirty entries = %d, want 0 (nothing written)", len(dirty))
	}
	got, err := s.EntryByRemoteID(ctx, 4242)
	if err != nil {
		t.Fatal(err)
	}
	if got.Description != "old" || got.Duration != 3600 {
		t.Errorf("entry = %+v, want the original 1h %q entry", got, "old")
	}
}

// TestUpdateEntryRefusesMoveIntoThePast covers the other half of the failsafe:
// today's entry may be edited, but not dragged back onto an earlier day.
func TestUpdateEntryRefusesMoveIntoThePast(t *testing.T) {
	s := openTest(t)
	now := time.Date(2026, 1, 3, 10, 0, 0, 0, time.UTC)
	at := time.Date(2026, 1, 3, 9, 0, 0, 0, time.UTC)
	stop := at.Add(time.Hour)
	id := mustCreate(t, s, Entry{
		WorkspaceID: 1, Description: "today", Start: at, Stop: ptrTime(stop),
		Duration: 3600, UpdatedAt: at,
	})

	past := at.AddDate(0, 0, -1)
	pastStop := past.Add(time.Hour)
	err := s.UpdateEntry(ctx, Entry{
		ID: id, Description: "today", Start: past, Stop: &pastStop,
		Duration: 3600, UpdatedAt: now,
	}, now, time.UTC)
	if !errors.Is(err, ErrEntryTooOld) {
		t.Fatalf("err = %v, want ErrEntryTooOld", err)
	}
	entries, err := s.EntriesBetween(ctx, past, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !entries[0].Start.Equal(at) {
		t.Errorf("entries = %+v, want the entry left on its own day %v", entries, at)
	}
}

// TestCheckEditableDayLocalMidnight verifies the day boundary is the one in the
// supplied location, not UTC: just after local midnight, an entry from a few
// hours earlier is already yesterday's and is refused.
func TestCheckEditableDayLocalMidnight(t *testing.T) {
	loc := time.FixedZone("UTC+3", 3*60*60)
	now := time.Date(2026, 1, 3, 0, 30, 0, 0, loc)

	if err := CheckEditableDay(time.Date(2026, 1, 3, 0, 5, 0, 0, loc), now, loc); err != nil {
		t.Errorf("entry from earlier today = %v, want it editable", err)
	}
	err := CheckEditableDay(time.Date(2026, 1, 2, 23, 30, 0, 0, loc), now, loc)
	if !errors.Is(err, ErrEntryTooOld) {
		t.Fatalf("err = %v, want ErrEntryTooOld", err)
	}
	// The same instants are the same calendar day in UTC, so a UTC-only
	// comparison would (wrongly) allow the edit.
	if err := CheckEditableDay(time.Date(2026, 1, 2, 23, 30, 0, 0, loc), now, time.UTC); err != nil {
		t.Errorf("UTC-framed check = %v, want it editable there", err)
	}
}

// TestSoftDeleteEntry pins `tg del`'s write: the row survives flagged deleted
// and dirty (so the deletion can be pushed) while dropping out of every read
// path immediately.
func TestSoftDeleteEntry(t *testing.T) {
	s := openTest(t)
	at := time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC)
	stop := at.Add(time.Hour)
	id := mustCreate(t, s, Entry{
		RemoteID: ptrInt(4242), WorkspaceID: 1, Start: at, Stop: ptrTime(stop),
		Duration: 3600, UpdatedAt: at, SyncedAt: ptrTime(at),
	})

	delAt := time.Date(2026, 1, 2, 15, 0, 0, 0, time.UTC)
	if err := s.SoftDeleteEntry(ctx, id, delAt); err != nil {
		t.Fatalf("SoftDeleteEntry: %v", err)
	}

	dirty, err := s.DirtyEntries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(dirty) != 1 || dirty[0].ID != id {
		t.Fatalf("dirty entries = %v, want the pending deletion of %d", dirty, id)
	}
	if !dirty[0].Deleted || !dirty[0].Dirty {
		t.Errorf("flags = deleted:%v dirty:%v, want both set", dirty[0].Deleted, dirty[0].Dirty)
	}
	if !dirty[0].UpdatedAt.Equal(delAt) {
		t.Errorf("updated_at = %v, want %v", dirty[0].UpdatedAt, delAt)
	}
	if dirty[0].RemoteID == nil || *dirty[0].RemoteID != 4242 {
		t.Errorf("remote_id = %v, want 4242 kept for the remote DELETE", dirty[0].RemoteID)
	}

	// Every read path hides it at once, including the number it was listed as.
	if got, err := s.EntriesBetween(ctx, at.Add(-time.Hour), at.Add(2*time.Hour)); err != nil || len(got) != 0 {
		t.Errorf("EntriesBetween = %v err=%v, want no entries", got, err)
	}
	if got, err := s.LastEntry(ctx, delAt); err != nil || got != nil {
		t.Errorf("LastEntry = %v err=%v, want nil", got, err)
	}
	if _, err := s.EntryByNum(ctx, 1, at); !errors.Is(err, ErrNoEntryNum) {
		t.Errorf("EntryByNum(1) = %v, want ErrNoEntryNum", err)
	}
}

// TestSoftDeleteEntryMissing verifies deleting an unknown id is an error.
func TestSoftDeleteEntryMissing(t *testing.T) {
	s := openTest(t)
	err := s.SoftDeleteEntry(ctx, 999, time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC))
	if err == nil {
		t.Fatal("SoftDeleteEntry on a missing id = nil error, want an error")
	}
	if !strings.Contains(err.Error(), "999") {
		t.Errorf("err = %v, want it to name the id", err)
	}
}

func TestEntryJoinsProjectColor(t *testing.T) {
	s := openTest(t)
	if err := s.ReplaceProjects(ctx, []Project{
		{ID: 1, WorkspaceID: 1, Name: "Backend", Color: "#0B83D9", Active: true},
	}); err != nil {
		t.Fatalf("replace projects: %v", err)
	}
	at := time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC)
	mustCreate(t, s, Entry{
		WorkspaceID: 1, ProjectID: ptrInt(1), Start: at,
		Stop: ptrTime(at.Add(time.Hour)), Duration: 3600, UpdatedAt: at,
	})

	got, err := s.EntriesBetween(ctx, at.Add(-time.Hour), at.Add(time.Hour))
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

	dirty, err := s.DirtyEntries(ctx)
	if err != nil || len(dirty) != 1 {
		t.Fatalf("dirty = %v err=%v, want 1", dirty, err)
	}

	syncedAt := at.Add(time.Minute)
	if err := s.MarkSynced(ctx, id, 999, syncedAt); err != nil {
		t.Fatalf("mark synced: %v", err)
	}
	dirty, _ = s.DirtyEntries(ctx)
	if len(dirty) != 0 {
		t.Fatalf("dirty after sync = %d, want 0", len(dirty))
	}
	got, err := s.EntryByRemoteID(ctx, 999)
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
	if err := s.ReplaceProjects(ctx, []Project{{ID: 1, WorkspaceID: 1, Name: "Backend", Active: true}}); err != nil {
		t.Fatalf("replace projects: %v", err)
	}
	if err := s.ReplaceTasks(ctx, []Task{
		{ID: 10, WorkspaceID: 1, ProjectID: 1, Name: "Fix login bug", Active: true},
		{ID: 11, WorkspaceID: 1, ProjectID: 1, Name: "Old task", Active: true},
	}); err != nil {
		t.Fatalf("replace tasks: %v", err)
	}

	// Second replace must wipe the previous contents entirely.
	if err := s.ReplaceTasks(ctx, []Task{{ID: 12, WorkspaceID: 1, ProjectID: 1, Name: "Code review", Active: true}}); err != nil {
		t.Fatalf("replace tasks 2: %v", err)
	}
	all, err := s.activeTasks(ctx)
	if err != nil {
		t.Fatalf("active tasks: %v", err)
	}
	if len(all) != 1 || all[0].ID != 12 {
		t.Fatalf("tasks after replace = %+v, want only id 12", all)
	}
}

func TestReplaceProjectTasksScoped(t *testing.T) {
	s := openTest(t)
	if err := s.ReplaceTasks(ctx, []Task{
		{ID: 10, WorkspaceID: 1, ProjectID: 1, Name: "Backend A", Active: true},
		{ID: 11, WorkspaceID: 1, ProjectID: 1, Name: "Backend B", Active: true},
		{ID: 20, WorkspaceID: 1, ProjectID: 2, Name: "Payments A", Active: true},
	}); err != nil {
		t.Fatalf("seed tasks: %v", err)
	}

	// Replacing project 1's tasks must leave project 2 untouched.
	if err := s.ReplaceProjectTasks(ctx, 1, []Task{
		{ID: 12, WorkspaceID: 1, ProjectID: 1, Name: "Backend C", Active: true},
	}); err != nil {
		t.Fatalf("replace project tasks: %v", err)
	}

	all, err := s.activeTasks(ctx)
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
	if err := s.PutProject(ctx, Project{ID: 5, WorkspaceID: 1, Name: "Old", Active: true, Billable: false}); err != nil {
		t.Fatalf("put project: %v", err)
	}
	// A second put must fully overwrite mutable fields (name, billable, active).
	if err := s.PutProject(ctx, Project{ID: 5, WorkspaceID: 1, Name: "New", Active: false, Billable: true}); err != nil {
		t.Fatalf("put project 2: %v", err)
	}
	p, err := s.ProjectByID(ctx, 5)
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
	if err := s.UpsertProject(ctx, Project{ID: 7, WorkspaceID: 1, Name: "Backend", Color: "#0B83D9", Active: true}); err != nil {
		t.Fatalf("upsert insert: %v", err)
	}
	p, err := s.ProjectByID(ctx, 7)
	if err != nil || p == nil {
		t.Fatalf("project by id: %v (p=%v)", err, p)
	}
	if p.Color != "#0B83D9" {
		t.Fatalf("color after insert = %q, want %q", p.Color, "#0B83D9")
	}

	// A later heal without a color (empty) must NOT clobber the stored color.
	if err := s.UpsertProject(ctx, Project{ID: 7, WorkspaceID: 1, Name: "Backend", Color: "", Active: true}); err != nil {
		t.Fatalf("upsert empty color: %v", err)
	}
	p, _ = s.ProjectByID(ctx, 7)
	if p.Color != "#0B83D9" {
		t.Fatalf("color after empty upsert = %q, want preserved %q", p.Color, "#0B83D9")
	}

	// A heal that carries a (new) color refreshes it.
	if err := s.UpsertProject(ctx, Project{ID: 7, WorkspaceID: 1, Name: "Backend", Color: "#E36A00", Active: true}); err != nil {
		t.Fatalf("upsert new color: %v", err)
	}
	p, _ = s.ProjectByID(ctx, 7)
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
	if err := s.ReplaceTasks(ctx, tasks); err != nil {
		t.Fatalf("replace: %v", err)
	}

	// Substring: matches across projects, excludes inactive, sorted by name.
	// "Fi" is a substring of every active "Fix…" task but exactly equals none.
	got, _ := s.FindTasksByFragment(ctx, "Fi", nil)
	if names := taskNames(got); !equal(names, []string{"Fix", "Fix login bug", "Fix payment"}) {
		t.Fatalf("substring match = %v", names)
	}

	// Exact title precedence: "Fix" wins over the broader substrings.
	got, _ = s.FindTasksByFragment(ctx, "Fix", nil)
	if names := taskNames(got); !equal(names, []string{"Fix"}) {
		t.Fatalf("exact match = %v", names)
	}

	// Project scoping restricts candidates.
	pid := int64(200)
	got, _ = s.FindTasksByFragment(ctx, "fix", &pid)
	if names := taskNames(got); !equal(names, []string{"Fix payment"}) {
		t.Fatalf("scoped match = %v", names)
	}

	// No match.
	if got, _ := s.FindTasksByFragment(ctx, "nonexistent", nil); len(got) != 0 {
		t.Fatalf("expected no matches, got %v", taskNames(got))
	}
}

func TestListProjects(t *testing.T) {
	s := openTest(t)
	if err := s.ReplaceProjects(ctx, []Project{
		{ID: 2, WorkspaceID: 1, Name: "Payments", Active: true},
		{ID: 1, WorkspaceID: 1, Name: "Backend", Active: true},
		{ID: 3, WorkspaceID: 1, Name: "Archived", Active: false},
	}); err != nil {
		t.Fatalf("replace projects: %v", err)
	}

	// Active-only, ordered by name.
	got, err := s.ListProjects(ctx, false)
	if err != nil {
		t.Fatalf("list projects: %v", err)
	}
	if len(got) != 2 || got[0].Name != "Backend" || got[1].Name != "Payments" {
		t.Fatalf("active projects = %+v, want [Backend Payments]", got)
	}

	// --all includes inactive.
	all, err := s.ListProjects(ctx, true)
	if err != nil {
		t.Fatalf("list projects --all: %v", err)
	}
	if len(all) != 3 || all[0].Name != "Archived" {
		t.Fatalf("all projects = %+v, want 3 incl. Archived", all)
	}
}

func TestListTasksProjectScope(t *testing.T) {
	s := openTest(t)
	if err := s.ReplaceProjects(ctx, []Project{
		{ID: 100, WorkspaceID: 1, Name: "Backend", Active: true},
		{ID: 200, WorkspaceID: 1, Name: "Payments", Active: true},
	}); err != nil {
		t.Fatalf("replace projects: %v", err)
	}
	if err := s.ReplaceTasks(ctx, []Task{
		{ID: 1, WorkspaceID: 1, ProjectID: 100, Name: "Fix login bug", Active: true},
		{ID: 2, WorkspaceID: 1, ProjectID: 100, Name: "Code review", Active: true},
		{ID: 3, WorkspaceID: 1, ProjectID: 200, Name: "Payment fix", Active: true},
	}); err != nil {
		t.Fatalf("replace tasks: %v", err)
	}

	// Unscoped: every active task.
	all, err := s.ListTasks(ctx, false, nil)
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("tasks = %d, want 3", len(all))
	}

	// Scoped to project 200: only its tasks.
	pid := int64(200)
	scoped, err := s.ListTasks(ctx, false, &pid)
	if err != nil {
		t.Fatalf("list tasks scoped: %v", err)
	}
	if len(scoped) != 1 || scoped[0].ID != 3 {
		t.Fatalf("scoped tasks = %+v, want only task 3 (Payment fix)", scoped)
	}
}

func TestFindProjectsByFragment(t *testing.T) {
	s := openTest(t)
	if err := s.ReplaceProjects(ctx, []Project{
		{ID: 1, WorkspaceID: 1, Name: "Backend", Active: true},
		{ID: 2, WorkspaceID: 1, Name: "Back office", Active: true},
		{ID: 3, WorkspaceID: 1, Name: "Payments", Active: true},
		{ID: 4, WorkspaceID: 1, Name: "Backup", Active: false},
	}); err != nil {
		t.Fatalf("replace projects: %v", err)
	}

	// Substring: matches active projects across the catalog, sorted by name,
	// excluding the inactive "Backup".
	got, _ := s.FindProjectsByFragment(ctx, "back")
	if names := projectNames(got); !equal(names, []string{"Back office", "Backend"}) {
		t.Fatalf("substring match = %v", names)
	}

	// Exact full-name precedence over broader substrings.
	got, _ = s.FindProjectsByFragment(ctx, "Backend")
	if names := projectNames(got); !equal(names, []string{"Backend"}) {
		t.Fatalf("exact match = %v", names)
	}

	// Unique fragment.
	got, _ = s.FindProjectsByFragment(ctx, "pay")
	if len(got) != 1 || got[0].ID != 3 {
		t.Fatalf("unique match = %+v, want project 3", got)
	}

	// No match.
	if got, _ := s.FindProjectsByFragment(ctx, "nonexistent"); len(got) != 0 {
		t.Fatalf("expected no matches, got %v", projectNames(got))
	}
}

func TestMetaRoundTrip(t *testing.T) {
	s := openTest(t)
	if _, ok, _ := s.Meta(ctx, MetaLastPull); ok {
		t.Fatal("last_pull should be absent initially")
	}
	if err := s.SetMeta(ctx, MetaLastPull, "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := s.SetMeta(ctx, MetaLastPull, "2026-02-01T00:00:00Z"); err != nil {
		t.Fatalf("update: %v", err)
	}
	v, ok, err := s.Meta(ctx, MetaLastPull)
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

// --- transactions ------------------------------------------------------------

// TestWithTxCommits verifies the writes made through the transaction store are
// visible afterwards.
func TestWithTxCommits(t *testing.T) {
	s := openTest(t)
	at := time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC)
	if err := s.WithTx(ctx, func(tx *Store) error {
		if _, err := tx.CreateEntry(ctx, Entry{
			WorkspaceID: 1, Start: at, Duration: 300, UpdatedAt: at, Dirty: true,
		}); err != nil {
			return err
		}
		return tx.UpsertProject(ctx, Project{ID: 5, WorkspaceID: 1, Name: "Backend"})
	}); err != nil {
		t.Fatalf("WithTx: %v", err)
	}
	entries, err := s.EntriesBetween(ctx, at.Add(-time.Hour), at.Add(time.Hour))
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries = %+v err=%v, want the committed entry", entries, err)
	}
	if p, _ := s.ProjectByID(ctx, 5); p == nil {
		t.Error("project should have been committed")
	}
}

// TestWithTxRollsBack verifies a failure part-way through discards everything
// the transaction wrote: that is what keeps a half-applied multi-statement write
// (a pull dying mid-loop, say) out of the database.
func TestWithTxRollsBack(t *testing.T) {
	s := openTest(t)
	at := time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC)
	boom := errors.New("boom")
	err := s.WithTx(ctx, func(tx *Store) error {
		if _, err := tx.CreateEntry(ctx, Entry{
			WorkspaceID: 1, Start: at, Duration: 300, UpdatedAt: at, Dirty: true,
		}); err != nil {
			return err
		}
		if err := tx.UpsertProject(ctx, Project{ID: 5, WorkspaceID: 1, Name: "Backend"}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("WithTx = %v, want the callback's error", err)
	}
	entries, err := s.EntriesBetween(ctx, at.Add(-time.Hour), at.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("entries = %+v, want none (rolled back)", entries)
	}
	if p, _ := s.ProjectByID(ctx, 5); p != nil {
		t.Errorf("project = %+v, want none (rolled back)", p)
	}
}

// TestWithTxNests verifies a nested WithTx joins the transaction in progress
// rather than committing early: the inner block's writes are rolled back with
// the outer one. That is what lets UpdateEntry/UpdateFromRemote be atomic on
// their own and still compose into a pull's transaction.
func TestWithTxNests(t *testing.T) {
	s := openTest(t)
	at := time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC)
	stop := at.Add(time.Hour)
	id := mustCreate(t, s, Entry{
		WorkspaceID: 1, Description: "before", Start: at, Stop: ptrTime(stop),
		Duration: 3600, UpdatedAt: at, Dirty: false,
	})
	boom := errors.New("boom")
	err := s.WithTx(ctx, func(tx *Store) error {
		// UpdateEntry wraps itself in WithTx; inside this one it must not
		// commit on its own.
		if err := tx.UpdateEntry(ctx, Entry{
			ID: id, Description: "after", Start: at, Stop: ptrTime(stop),
			Duration: 3600, UpdatedAt: at,
		}, at, time.UTC); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("WithTx = %v, want the callback's error", err)
	}
	entries, err := s.EntriesBetween(ctx, at.Add(-time.Hour), stop.Add(time.Hour))
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries = %+v err=%v", entries, err)
	}
	if entries[0].Description != "before" {
		t.Errorf("description = %q, want the rolled-back %q", entries[0].Description, "before")
	}
}

// TestWithTxRollsBackRefusedUpdate verifies UpdateEntry's own transaction: an
// edit refused by the day failsafe after the row was read leaves nothing behind.
func TestWithTxRollsBackRefusedUpdate(t *testing.T) {
	s := openTest(t)
	at := time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC)
	stop := at.Add(time.Hour)
	id := mustCreate(t, s, Entry{
		WorkspaceID: 1, Description: "kept", Start: at, Stop: ptrTime(stop),
		Duration: 3600, UpdatedAt: at, Dirty: false,
	})
	// now is the following day, so the entry is history and may not be edited.
	next := at.AddDate(0, 0, 1)
	err := s.UpdateEntry(ctx, Entry{
		ID: id, Description: "rewritten", Start: at, Stop: ptrTime(stop),
		Duration: 3600, UpdatedAt: next,
	}, next, time.UTC)
	if !errors.Is(err, ErrEntryTooOld) {
		t.Fatalf("UpdateEntry = %v, want ErrEntryTooOld", err)
	}
	entries, err := s.EntriesBetween(ctx, at.Add(-time.Hour), stop.Add(time.Hour))
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries = %+v err=%v", entries, err)
	}
	if entries[0].Description != "kept" || entries[0].Dirty {
		t.Errorf("entry = %+v, want it untouched and clean", entries[0])
	}
}

// TestCancelledContextStopsWork verifies the context reaches the database layer
// rather than being accepted and ignored: with it already cancelled (a Ctrl-C
// mid-command, in practice) reads and writes fail instead of running.
func TestCancelledContextStopsWork(t *testing.T) {
	s := openTest(t)
	at := time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := s.EntriesBetween(cancelled, at, at.Add(time.Hour)); !errors.Is(err, context.Canceled) {
		t.Errorf("EntriesBetween = %v, want context.Canceled", err)
	}
	if _, err := s.CreateEntry(cancelled, Entry{
		WorkspaceID: 1, Start: at, Duration: 300, UpdatedAt: at, Dirty: true,
	}); !errors.Is(err, context.Canceled) {
		t.Errorf("CreateEntry = %v, want context.Canceled", err)
	}
	if err := s.WithTx(cancelled, func(tx *Store) error {
		t.Error("WithTx should not run its callback on a cancelled context")
		return nil
	}); !errors.Is(err, context.Canceled) {
		t.Errorf("WithTx = %v, want context.Canceled", err)
	}
	// Nothing was written.
	entries, err := s.EntriesBetween(ctx, at, at.Add(time.Hour))
	if err != nil || len(entries) != 0 {
		t.Errorf("entries = %+v err=%v, want none", entries, err)
	}
}
