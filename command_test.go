package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mantas6/tg/api"
	"github.com/mantas6/tg/config"
	"github.com/mantas6/tg/store"
)

// ctx is the context the command and store calls in these tests run under. The
// cancellation paths that need a live context build their own.
var ctx = context.Background()

// testWorkspaceID is the workspace the fixtures below belong to: the catalog
// seeds it on every project and task, and env reports it as the configured one,
// so a command files new entries under the same workspace a real authenticated
// invocation would.
const testWorkspaceID = 1

// env builds the cmdEnv the cmd* functions take. A nil client is the offline
// case (no credentials, or none needed): the local edit still applies and stays
// dirty for a later push, and nothing is sent (see cmdEnv.offline).
//
// The day the command works on starts out as now's, exactly as withEnv builds
// it: that is the ordinary invocation, with no --date. The tests that pass one
// move the env with `.on(...)` (see dayAnchor), which is what runAdd/runMod do.
func env(w io.Writer, s *store.Store, c *api.Client, now time.Time, loc *time.Location) *cmdEnv {
	return &cmdEnv{
		ctx: ctx, w: w, st: s, c: c,
		workspaceID: testWorkspaceID, now: now, day: now, loc: loc,
	}
}

// dayAnchor is the instant `--date DATE` resolves to for the tests that move a
// command to another day: the last second of that day in loc, the same anchor
// resolveDateFlag hands the command (see cmdEnv.day). It is built through
// resolveDateFlag itself rather than by hand, so a test cannot go on passing
// against an anchor the flag would never produce.
func dayAnchor(t *testing.T, date string, now time.Time, loc *time.Location) time.Time {
	t.Helper()
	anchor, err := resolveDateFlag(date, now, loc)
	if err != nil {
		t.Fatalf("resolveDateFlag(%q): %v", date, err)
	}
	return anchor
}

// localEnv is env for the commands that only read the local catalog (tasks,
// grep, projects): they use neither the clock nor the client.
func localEnv(w io.Writer, s *store.Store) *cmdEnv {
	return env(w, s, nil, time.Time{}, time.UTC)
}

// apiEnv is env for the commands that need the client but no clock
// (`tg projects update`).
func apiEnv(w io.Writer, s *store.Store, c *api.Client) *cmdEnv {
	return env(w, s, c, time.Time{}, time.UTC)
}

// unauthenticatedEnv is env as it looks before `tg auth`: no client and no
// configured workspace, which is what the offline paths are checked against.
func unauthenticatedEnv(w io.Writer, s *store.Store, now time.Time, loc *time.Location) *cmdEnv {
	e := env(w, s, nil, now, loc)
	e.workspaceID = 0
	return e
}

// newStore opens a throwaway store pinned to UTC, matching the time.UTC the
// command tests pass as the display location: the store reckons entry days (and
// so the per-day numbering `tg ls` shows) in its own location, which a real
// invocation and its listing must agree on.
func newStore(t *testing.T) *store.Store {
	t.Helper()
	return newStoreIn(t, time.UTC)
}

// newStoreIn is newStore with an explicit calendar, for the tests that have to
// reckon days somewhere other than UTC (see the DST window tests).
func newStoreIn(t *testing.T, loc *time.Location) *store.Store {
	t.Helper()
	s, err := store.OpenIn(ctx, filepath.Join(t.TempDir(), "tg.db"), loc)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// ptrInt is the pointer helper the store's optional ids need (project/task ids,
// remote ids), matching the one each of the other packages' tests defines.
func ptrInt(v int64) *int64 { return &v }

// --- checked store reads -----------------------------------------------------
//
// The must* helpers below wrap the store reads the assertions are made against.
// They exist because these tests used to drop the errors (`entries, _ :=
// s.EntriesBetween(...)`), which turns a failing query into an empty result and
// so into a confusing assertion failure ("entries = 0, want 1") about the
// command rather than about the store call that actually broke.

// mustEntries reads the entries in [from, to).
func mustEntries(t *testing.T, s *store.Store, from, to time.Time) []store.Entry {
	t.Helper()
	entries, err := s.EntriesBetween(ctx, from, to)
	if err != nil {
		t.Fatalf("EntriesBetween(%v, %v): %v", from, to, err)
	}
	return entries
}

// mustRunning reads the running entry, if any.
func mustRunning(t *testing.T, s *store.Store) *store.Entry {
	t.Helper()
	e, err := s.Running(ctx)
	if err != nil {
		t.Fatalf("Running: %v", err)
	}
	return e
}

// mustEntryByRemoteID reads the entry mirroring a Toggl id, nil when none does.
func mustEntryByRemoteID(t *testing.T, s *store.Store, remoteID int64) *store.Entry {
	t.Helper()
	e, err := s.EntryByRemoteID(ctx, remoteID)
	if err != nil {
		t.Fatalf("EntryByRemoteID(%d): %v", remoteID, err)
	}
	return e
}

// mustDirtyEntries reads the push queue.
func mustDirtyEntries(t *testing.T, s *store.Store) []store.Entry {
	t.Helper()
	dirty, err := s.DirtyEntries(ctx)
	if err != nil {
		t.Fatalf("DirtyEntries: %v", err)
	}
	return dirty
}

// mustMeta reads a meta key, returning its value and whether it is set.
func mustMeta(t *testing.T, s *store.Store, key string) (string, bool) {
	t.Helper()
	v, ok, err := s.Meta(ctx, key)
	if err != nil {
		t.Fatalf("Meta(%q): %v", key, err)
	}
	return v, ok
}

func mustListTasks(t *testing.T, s *store.Store, includeInactive bool, projectID *int64) []store.Task {
	t.Helper()
	tasks, err := s.ListTasks(ctx, includeInactive, projectID)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	return tasks
}

func mustListProjects(t *testing.T, s *store.Store, includeInactive bool) []store.Project {
	t.Helper()
	projects, err := s.ListProjects(ctx, includeInactive)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	return projects
}

// decodeBody unmarshals a JSON request body captured by one of the stub Toggl
// handlers below. It runs on the server's goroutine, so it reports a malformed
// body with t.Errorf (t.Fatalf may only be called from the test's own
// goroutine) instead of leaving the captured map silently empty.
func decodeBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		t.Errorf("read request body: %v", err)
		return nil
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Errorf("decode request body %q: %v", raw, err)
		return nil
	}
	return body
}

func seedCatalog(t *testing.T, s *store.Store) {
	t.Helper()
	if err := s.ReplaceProjects(ctx, []store.Project{
		{ID: 1, WorkspaceID: 1, Name: "Backend", Active: true},
		{ID: 2, WorkspaceID: 1, Name: "Payments", Active: true, Billable: true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceTasks(ctx, []store.Task{
		{ID: 10, WorkspaceID: 1, ProjectID: 1, Name: "Fix login bug", Active: true},
		{ID: 11, WorkspaceID: 1, ProjectID: 1, Name: "Fix", Active: true},
		{ID: 12, WorkspaceID: 1, ProjectID: 1, Name: "Code review", Active: true},
		{ID: 13, WorkspaceID: 1, ProjectID: 1, Name: "Write tests", Active: true},
		{ID: 14, WorkspaceID: 1, ProjectID: 1, Name: "Write docs", Active: true},
		{ID: 20, WorkspaceID: 1, ProjectID: 2, Name: "Payment fix", Active: true},
	}); err != nil {
		t.Fatal(err)
	}
}

// testStart is the reference day used by the entry fixtures below.
var testStart = time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC)

// seedRunning inserts a running entry (duration -1, no stop) the way a `tg pull`
// of a timer started in the Toggl web app would: tg itself no longer starts
// entries, but it must keep handling the ones it pulls.
func seedRunning(t *testing.T, s *store.Store, taskID int64, start time.Time) {
	t.Helper()
	if _, err := s.CreateEntry(ctx, store.Entry{
		WorkspaceID: 1, ProjectID: ptrInt(1), TaskID: &taskID,
		Start: start, Duration: -1, UpdatedAt: start,
	}); err != nil {
		t.Fatalf("seed running entry: %v", err)
	}
}

// fixtureEntry describes one entry for the table-driven fixtures below: when it
// starts, how long it lasts and the task it is filed under (all in project 1 of
// seedCatalog). A zero dur means the entry is still running — no stop, Toggl's
// negative-duration marker — which is how a pulled timer looks locally.
type fixtureEntry struct {
	start  time.Time
	dur    time.Duration
	taskID int64
}

// seedFixture inserts the given entries in order, which is also what hands out
// their per-day numbers (so the first one is entry 1 of its day).
func seedFixture(t *testing.T, s *store.Store, entries ...fixtureEntry) {
	t.Helper()
	for _, fe := range entries {
		e := store.Entry{
			WorkspaceID: 1, ProjectID: ptrInt(1), TaskID: ptrInt(fe.taskID),
			Start: fe.start, Duration: -1, UpdatedAt: fe.start,
		}
		if fe.dur > 0 {
			stop := fe.start.Add(fe.dur)
			e.Stop, e.Duration, e.UpdatedAt = &stop, int64(fe.dur/time.Second), stop
		}
		if _, err := s.CreateEntry(ctx, e); err != nil {
			t.Fatalf("seed entry at %v: %v", fe.start, err)
		}
	}
}

func TestProjectIDFromEnv(t *testing.T) {
	t.Setenv("TOGGL_PROJECT_ID", "42")
	got, err := projectIDFromEnv()
	if err != nil || got == nil || *got != 42 {
		t.Errorf("projectIDFromEnv = %v err=%v, want 42", got, err)
	}
	t.Setenv("TOGGL_PROJECT_ID", "")
	got, err = projectIDFromEnv()
	if err != nil || got != nil {
		t.Errorf("projectIDFromEnv (unset) = %v err=%v, want nil", got, err)
	}
}

// TestProjectIDFromEnvInvalid pins that a malformed TOGGL_PROJECT_ID fails
// loudly: it used to parse as nil, i.e. as "unset", so a typo silently unscoped
// `add`/`update` and added entries with no project instead of refusing.
func TestProjectIDFromEnvInvalid(t *testing.T) {
	for _, v := range []string{"abc", "1x", "3.5", "42,"} {
		t.Setenv("TOGGL_PROJECT_ID", v)
		got, err := projectIDFromEnv()
		if err == nil {
			t.Errorf("projectIDFromEnv(%q) = %v, want an error", v, got)
			continue
		}
		if !strings.Contains(err.Error(), "TOGGL_PROJECT_ID") || !strings.Contains(err.Error(), v) {
			t.Errorf("projectIDFromEnv(%q) error = %v, want it to name the variable and value", v, err)
		}
		if got != nil {
			t.Errorf("projectIDFromEnv(%q) = %v, want nil alongside the error", v, got)
		}
	}
}

// addNow is the reference instant `add` timesigns resolve against in the tests
// below; the timesign grammar itself is covered by the timesig package.
var addNow = time.Date(2026, 1, 2, 15, 0, 0, 0, time.UTC)

// addWindow is the window the add fixtures live in: a day either side of the
// instant the timesigns resolve against.
func addWindow(t *testing.T, s *store.Store, now time.Time) []store.Entry {
	t.Helper()
	return mustEntries(t, s, now.Add(-24*time.Hour), now.Add(24*time.Hour))
}

func TestAddCreatesFinishedEntry(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)

	var buf bytes.Buffer
	if err := cmdAdd(env(&buf, s, nil, addNow, time.UTC), nil, false, "9-:30", "login", ""); err != nil {
		t.Fatalf("add: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"Added: Fix login bug", "09:00-09:30", "0h30m"} {
		if !strings.Contains(out, want) {
			t.Errorf("output = %q, want %q", out, want)
		}
	}

	entries := addWindow(t, s, addNow)
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	e := entries[0]
	if e.TaskID == nil || *e.TaskID != 10 {
		t.Errorf("task_id = %v, want 10", e.TaskID)
	}
	if e.ProjectID == nil || *e.ProjectID != 1 {
		t.Errorf("project_id = %v, want 1", e.ProjectID)
	}
	if e.Stop == nil {
		t.Fatal("entry should be already stopped")
	}
	wantStart := time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC)
	wantStop := time.Date(2026, 1, 2, 9, 30, 0, 0, time.UTC)
	if !e.Start.Equal(wantStart) {
		t.Errorf("start = %v, want %v", e.Start, wantStart)
	}
	if !e.Stop.Equal(wantStop) {
		t.Errorf("stop = %v, want %v", *e.Stop, wantStop)
	}
	if e.Duration != 1800 {
		t.Errorf("duration = %d, want 1800", e.Duration)
	}
	// Without --desc the description stays empty (prior behavior unchanged).
	if e.Description != "" {
		t.Errorf("description = %q, want empty", e.Description)
	}
	if !e.Dirty {
		t.Error("added entry should be dirty for a later push")
	}
	if r := mustRunning(t, s); r != nil {
		t.Errorf("add must not create a running entry, got %+v", r)
	}
}

// TestAddAcceptsRelativeTimesign checks that `add` resolves a relative
// timesign through the timesig package: the entry ends at now floored to the
// preceding 5-minute mark and starts that many minutes earlier. (Overlap
// checks are a separate concern.)
func TestAddAcceptsRelativeTimesign(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)

	// 15:07 floors to 15:05, so "+:20" spans 14:45-15:05.
	now := time.Date(2026, 1, 2, 15, 7, 0, 0, time.UTC)
	var buf bytes.Buffer
	if err := cmdAdd(env(&buf, s, nil, now, time.UTC), nil, false, "+:20", "login", ""); err != nil {
		t.Fatalf("add: %v", err)
	}
	if out := buf.String(); !strings.Contains(out, "14:45-15:05") {
		t.Errorf("output = %q, want 14:45-15:05", out)
	}

	entries := addWindow(t, s, now)
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	e := entries[0]
	wantStart := time.Date(2026, 1, 2, 14, 45, 0, 0, time.UTC)
	wantStop := time.Date(2026, 1, 2, 15, 5, 0, 0, time.UTC)
	if !e.Start.Equal(wantStart) {
		t.Errorf("start = %v, want %v", e.Start, wantStart)
	}
	if e.Stop == nil || !e.Stop.Equal(wantStop) {
		t.Errorf("stop = %v, want %v", e.Stop, wantStop)
	}
	if e.Duration != 1200 {
		t.Errorf("duration = %d, want 1200", e.Duration)
	}
}

// TestAddResolvesTask tables what `tg add` files an entry against: the fragment
// is resolved through the shared task resolver (an exact task name beating the
// longer names it is part of, a project scope narrowing the candidates, `-1`
// inert on a fragment that already resolves), the entry inherits its project
// from the resolved task — billable flag included — and --desc lands on it.
func TestAddResolvesTask(t *testing.T) {
	t.Parallel()
	payments := int64(2)
	for _, tc := range []struct {
		name          string
		projectID     *int64
		first         bool
		fragment      string
		desc          string
		wantTaskID    int64
		wantProjectID int64
		wantBillable  bool
		wantDesc      string
	}{
		{
			// Backend (project 1) is not billable, so neither is the entry.
			name: "unique fragment", fragment: "login",
			wantTaskID: 10, wantProjectID: 1,
		},
		{
			// "Fix" exactly matches task 11 even though it is a substring of
			// "Fix login bug" and "Payment fix".
			name: "exact name wins", fragment: "Fix",
			wantTaskID: 11, wantProjectID: 1,
		},
		{
			// "fix" matches several tasks, but scoping to Payments leaves one —
			// and that project is billable, which the entry inherits.
			name: "project scope narrows the fragment", projectID: &payments, fragment: "fix",
			wantTaskID: 20, wantProjectID: 2, wantBillable: true,
		},
		{
			// `-1` never changes which task a working fragment picks.
			name: "first flag is inert when unique", first: true, fragment: "login",
			wantTaskID: 10, wantProjectID: 1,
		},
		{
			name: "description is stored", fragment: "login", desc: "reset password flow",
			wantTaskID: 10, wantProjectID: 1, wantDesc: "reset password flow",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newStore(t)
			seedCatalog(t, s)

			var buf bytes.Buffer
			if err := cmdAdd(env(&buf, s, nil, addNow, time.UTC), tc.projectID, tc.first, "9-10", tc.fragment, tc.desc); err != nil {
				t.Fatalf("add: %v", err)
			}
			entries := addWindow(t, s, addNow)
			if len(entries) != 1 {
				t.Fatalf("entries = %d, want 1", len(entries))
			}
			e := entries[0]
			if e.TaskID == nil || *e.TaskID != tc.wantTaskID {
				t.Errorf("task_id = %v, want %d", e.TaskID, tc.wantTaskID)
			}
			if e.ProjectID == nil || *e.ProjectID != tc.wantProjectID {
				t.Errorf("project_id = %v, want %d", e.ProjectID, tc.wantProjectID)
			}
			if e.Billable != tc.wantBillable {
				t.Errorf("billable = %v, want %v (the project's flag)", e.Billable, tc.wantBillable)
			}
			if e.Description != tc.wantDesc {
				t.Errorf("description = %q, want %q", e.Description, tc.wantDesc)
			}
		})
	}
}

// TestAddRefusals tables the calls `tg add` declines: every malformed timesign
// form and every fragment that does not resolve to exactly one task. None of
// them may write an entry — a bad timesign is rejected before the catalog is
// even consulted — and the ambiguity error carries the candidates (with their
// projects, which is how same-named tasks are told apart) plus the way out.
func TestAddRefusals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// anchored seeds a 09:00-10:00 entry first, which is what the bare
		// duration forms would otherwise continue from.
		anchored bool
		timesign string
		fragment string
		first    bool
		wantErr  []string
	}{
		{name: "zero-length relative timesign", timesign: "+:00", fragment: "login", wantErr: []string{"timesign"}},
		{name: "unparsable timesign", timesign: "nope", fragment: "login", wantErr: []string{"timesign"}},
		{name: "zero bare duration", anchored: true, timesign: "0", fragment: "review", wantErr: []string{"timesign"}},
		{name: "zero minutes duration", anchored: true, timesign: ":00", fragment: "review", wantErr: []string{"timesign"}},
		{name: "duration out of range", anchored: true, timesign: "24", fragment: "review", wantErr: []string{"timesign"}},
		{name: "duration minutes out of range", anchored: true, timesign: "1:60", fragment: "review", wantErr: []string{"timesign"}},
		{name: "duration missing minutes", anchored: true, timesign: "1:", fragment: "review", wantErr: []string{"timesign"}},
		{
			name: "ambiguous fragment", timesign: "10-11", fragment: "write",
			wantErr: []string{"multiple tasks match", "Write tests", "Write docs", "[Backend]", "pass -1"},
		},
		{
			name: "no match", timesign: "10-11", fragment: "nonexistent",
			wantErr: []string{"no task matches", "tg update"},
		},
		{
			// `-1` resolves ambiguity; with nothing to choose from it still fails.
			name: "no match with -1", timesign: "10-11", fragment: "nonexistent", first: true,
			wantErr: []string{"tg update"},
		},
		{name: "empty fragment", timesign: "10-11", fragment: "  ", wantErr: []string{addUsage}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newStore(t)
			seedCatalog(t, s)
			wantEntries := 0
			if tc.anchored {
				if err := cmdAdd(env(io.Discard, s, nil, addNow, time.UTC), nil, false, "9-10", "login", ""); err != nil {
					t.Fatalf("seed anchor: %v", err)
				}
				wantEntries = 1
			}

			var buf bytes.Buffer
			err := cmdAdd(env(&buf, s, nil, addNow, time.UTC), nil, tc.first, tc.timesign, tc.fragment, "")
			if err == nil {
				t.Fatalf("add %q %q = nil error, want a refusal", tc.timesign, tc.fragment)
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %v, want it to mention %q", err, want)
				}
			}
			if buf.Len() != 0 {
				t.Errorf("output = %q, want nothing written", buf.String())
			}
			if got := addWindow(t, s, addNow); len(got) != wantEntries {
				t.Errorf("entries = %d, want %d (a refused add writes nothing)", len(got), wantEntries)
			}
		})
	}
}

// TestAddDurationStartsAtLastEntryEnd covers the bare duration form: with no
// start time given, the entry picks up where the last one ended and runs for
// the duration typed, so `tg add 1:30 <task>` logs the block back to back.
func TestAddDurationStartsAtLastEntryEnd(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)

	// The last entry today ends at 10:00, so "1:30" is 10:00-11:30 regardless
	// of what time it is now (15:00).
	var buf bytes.Buffer
	if err := cmdAdd(env(&buf, s, nil, addNow, time.UTC), nil, false, "9-10", "login", ""); err != nil {
		t.Fatalf("add first: %v", err)
	}
	buf.Reset()
	if err := cmdAdd(env(&buf, s, nil, addNow, time.UTC), nil, false, "1:30", "review", ""); err != nil {
		t.Fatalf("add duration: %v", err)
	}
	if out := buf.String(); !strings.Contains(out, "10:00-11:30") || !strings.Contains(out, "1h30m") {
		t.Errorf("output = %q, want 10:00-11:30 (1h30m)", out)
	}

	entries := addWindow(t, s, addNow)
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	e := entries[1]
	wantStart := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)
	wantStop := time.Date(2026, 1, 2, 11, 30, 0, 0, time.UTC)
	if !e.Start.Equal(wantStart) {
		t.Errorf("start = %v, want %v (the last entry's end)", e.Start, wantStart)
	}
	if e.Stop == nil || !e.Stop.Equal(wantStop) {
		t.Errorf("stop = %v, want %v", e.Stop, wantStop)
	}
	if e.Duration != 5400 {
		t.Errorf("duration = %d, want 5400", e.Duration)
	}
	if e.TaskID == nil || *e.TaskID != 12 {
		t.Errorf("task_id = %v, want 12 (Code review)", e.TaskID)
	}

	// A second bare duration chains off the one just added.
	buf.Reset()
	if err := cmdAdd(env(&buf, s, nil, addNow, time.UTC), nil, false, ":30", "Fix", ""); err != nil {
		t.Fatalf("add chained duration: %v", err)
	}
	if out := buf.String(); !strings.Contains(out, "11:30-12:00") {
		t.Errorf("output = %q, want 11:30-12:00", out)
	}
}

// TestAddDurationWithoutLastEntry pins the missing-anchor case: a bare duration
// has no start of its own, so on a day with nothing tracked yet it is refused
// (pointing at the forms that need no anchor) rather than silently starting at
// now or reaching back into yesterday.
func TestAddDurationWithoutLastEntry(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)

	// Yesterday's entry is history: LastEntry is today-only, so it must not
	// become the anchor.
	if err := cmdAdd(env(&bytes.Buffer{}, s, nil, addNow.AddDate(0, 0, -1), time.UTC), nil, false, "9-10", "login", ""); err != nil {
		t.Fatalf("seed yesterday: %v", err)
	}

	var buf bytes.Buffer
	err := cmdAdd(env(&buf, s, nil, addNow, time.UTC), nil, false, "1:30", "review", "")
	if err == nil {
		t.Fatal("add 1:30 with no entry today = nil error, want an error")
	}
	for _, want := range []string{"no entry tracked today", "9-:30", "+1:30"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to mention %q", err, want)
		}
	}
	if buf.Len() != 0 {
		t.Errorf("output = %q, want nothing written", buf.String())
	}
	// Only yesterday's entry exists; nothing was created today.
	entries := mustEntries(t, s, addNow.AddDate(0, 0, -2), addNow.Add(24*time.Hour))
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1 (yesterday's only)", len(entries))
	}
}

// TestAddDurationWithRunningLastEntry covers the other missing anchor: a
// running entry has no end time to continue from, so the bare form is refused
// exactly as `tg mod +DURATION` refuses one.
func TestAddDurationWithRunningLastEntry(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)

	seedRunning(t, s, 12, testStart)

	var buf bytes.Buffer
	err := cmdAdd(env(&buf, s, nil, addNow, time.UTC), nil, false, "1:30", "login", "")
	if err == nil {
		t.Fatal("add 1:30 after a running entry = nil error, want an error")
	}
	if !strings.Contains(err.Error(), "still running") {
		t.Errorf("err = %v, want it to mention the running entry", err)
	}
	if entries := addWindow(t, s, addNow); len(entries) != 1 {
		t.Fatalf("entries = %d, want 1 (the running entry only)", len(entries))
	}
}

// TestAddDurationRejectsOverlap keeps the bare form under the same overlap
// guard as the rest: an entry booked for later today is skipped when resolving
// the anchor (LastEntry ignores future starts), so only the guard can catch a
// duration long enough to run into it.
func TestAddDurationRejectsOverlap(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)

	// It is 10:30. Tracked so far today: 09:00-10:00. Booked for later:
	// 11:00-12:00, which starts after now and so is never the anchor.
	now := time.Date(2026, 1, 2, 10, 30, 0, 0, time.UTC)
	if err := cmdAdd(env(&bytes.Buffer{}, s, nil, now, time.UTC), nil, false, "9-10", "login", ""); err != nil {
		t.Fatalf("add first: %v", err)
	}
	if err := cmdAdd(env(&bytes.Buffer{}, s, nil, now, time.UTC), nil, false, "11-12", "Fix", ""); err != nil {
		t.Fatalf("add later: %v", err)
	}

	var buf bytes.Buffer
	// 10:00 + 1h30m = 11:30, which straddles the 11:00-12:00 entry.
	err := cmdAdd(env(&buf, s, nil, now, time.UTC), nil, false, "1:30", "review", "")
	if err == nil {
		t.Fatal("overlapping duration add = nil error, want an error")
	}
	for _, want := range []string{"overlaps existing entry", "10:00-11:30", "11:00-12:00"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to mention %q", err, want)
		}
	}
	if entries := addWindow(t, s, now); len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}

	// Exactly filling the gap up to the booked entry is fine (back to back).
	buf.Reset()
	if err := cmdAdd(env(&buf, s, nil, now, time.UTC), nil, false, "1", "review", ""); err != nil {
		t.Fatalf("gap-filling add: %v", err)
	}
	if out := buf.String(); !strings.Contains(out, "10:00-11:00") {
		t.Errorf("output = %q, want 10:00-11:00", out)
	}
}

// TestAddOnAnotherDay covers `tg add --date`: an absolute timesign is resolved
// on the day the flag names rather than on today's, the entry is stored there
// (numbered from 1, since the day has its own numbering) and the confirmation
// line says which day it landed on — the clock times alone would be ambiguous.
// Everything else about the entry is unchanged: it is dirty for a push and
// stamped with the real clock, not with the day it was filed under.
func TestAddOnAnotherDay(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)

	// It is 15:00 on 2026-01-02; the entry belongs to the 5th.
	anchor := dayAnchor(t, "2026-01-05", addNow, time.UTC)
	var buf bytes.Buffer
	if err := cmdAdd(env(&buf, s, nil, addNow, time.UTC).on(anchor), nil, false, "9-:30", "login", ""); err != nil {
		t.Fatalf("add: %v", err)
	}
	for _, want := range []string{"Added: Fix login bug", "09:00-09:30", "0h30m", "on 2026-01-05"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("output = %q, want it to contain %q", buf.String(), want)
		}
	}

	// Nothing landed on today.
	if got := mustEntries(t, s, startOfDay(addNow, time.UTC), startOfDay(addNow, time.UTC).AddDate(0, 0, 1)); len(got) != 0 {
		t.Errorf("today's entries = %d, want 0 (the entry belongs to the 5th)", len(got))
	}
	day := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	entries := mustEntries(t, s, day, day.AddDate(0, 0, 1))
	if len(entries) != 1 {
		t.Fatalf("entries on 2026-01-05 = %d, want 1", len(entries))
	}
	e := entries[0]
	wantStart := time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC)
	wantStop := time.Date(2026, 1, 5, 9, 30, 0, 0, time.UTC)
	if !e.Start.Equal(wantStart) || e.Stop == nil || !e.Stop.Equal(wantStop) {
		t.Errorf("span = %v-%v, want %v-%v", e.Start, e.Stop, wantStart, wantStop)
	}
	if e.Duration != 1800 {
		t.Errorf("duration = %d, want 1800", e.Duration)
	}
	if e.Seq != 1 {
		t.Errorf("seq = %d, want 1 (the moved day numbers from 1)", e.Seq)
	}
	if !e.Dirty {
		t.Error("an entry added on another day should still be dirty for a push")
	}
	// The last-writer-wins clock is the real one, not the day named.
	if !e.UpdatedAt.Equal(addNow) {
		t.Errorf("updated_at = %v, want the real clock %v", e.UpdatedAt, addNow)
	}
}

// TestAddOnAnotherDayAnchorsDurationThere pins which day a bare duration
// continues from once --date moved the command: that day's last entry, never
// today's. Today has entries and the moved day does not, so the anchor is
// missing there — the refusal (naming the day, and offering only the forms that
// work on it) is what proves today's 14:00-15:00 block was not reached for.
func TestAddOnAnotherDayAnchorsDurationThere(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)

	// Tracked today: 14:00-15:00.
	if err := cmdAdd(env(&bytes.Buffer{}, s, nil, addNow, time.UTC), nil, false, "14-15", "login", ""); err != nil {
		t.Fatalf("seed today: %v", err)
	}

	anchor := dayAnchor(t, "2026-01-05", addNow, time.UTC)
	var buf bytes.Buffer
	err := cmdAdd(env(&buf, s, nil, addNow, time.UTC).on(anchor), nil, false, ":30", "review", "")
	if err == nil {
		t.Fatal("bare duration on an empty day = nil error, want a refusal")
	}
	for _, want := range []string{"no entry tracked on 2026-01-05", "9-:30"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to mention %q", err, want)
		}
	}
	// The relative form cannot be used there, so it is not offered either.
	if strings.Contains(err.Error(), "+1:30") {
		t.Errorf("err = %v, should not offer a relative timesign on a moved day", err)
	}
	if buf.Len() != 0 {
		t.Errorf("output = %q, want nothing written", buf.String())
	}

	// With a block on that day, the duration chains off ITS end (10:00), not
	// off today's 15:00.
	buf.Reset()
	if err := cmdAdd(env(&buf, s, nil, addNow, time.UTC).on(anchor), nil, false, "9-10", "login", ""); err != nil {
		t.Fatalf("add anchor on the 5th: %v", err)
	}
	buf.Reset()
	if err := cmdAdd(env(&buf, s, nil, addNow, time.UTC).on(anchor), nil, false, ":30", "review", ""); err != nil {
		t.Fatalf("add duration on the 5th: %v", err)
	}
	if out := buf.String(); !strings.Contains(out, "10:00-10:30") || !strings.Contains(out, "on 2026-01-05") {
		t.Errorf("output = %q, want 10:00-10:30 on 2026-01-05", out)
	}
	day := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	entries := mustEntries(t, s, day, day.AddDate(0, 0, 1))
	if len(entries) != 2 {
		t.Fatalf("entries on 2026-01-05 = %d, want 2", len(entries))
	}
	wantStart := time.Date(2026, 1, 5, 10, 0, 0, 0, time.UTC)
	if !entries[1].Start.Equal(wantStart) {
		t.Errorf("start = %v, want %v (that day's last entry's end)", entries[1].Start, wantStart)
	}
}

// TestAddOnAnotherDayRefusesRelativeTimesign pins the one timesign --date
// cannot take: a relative one counts back from the current 5-minute mark, which
// exists only today, so pinning it to some hour of another day would record a
// span the user never named. The refusal names the day and the forms that do
// work, and nothing is written.
func TestAddOnAnotherDayRefusesRelativeTimesign(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)

	anchor := dayAnchor(t, "2026-01-05", addNow, time.UTC)
	for _, timesign := range []string{"+:20", "+1:30", "+2"} {
		var buf bytes.Buffer
		err := cmdAdd(env(&buf, s, nil, addNow, time.UTC).on(anchor), nil, false, timesign, "login", "")
		if err == nil {
			t.Fatalf("add --date %q = nil error, want a refusal", timesign)
		}
		for _, want := range []string{"relative timesign", "on 2026-01-05", "9-:30", "1:30"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("add %q err = %v, want it to mention %q", timesign, err, want)
			}
		}
		if buf.Len() != 0 {
			t.Errorf("output = %q, want nothing written", buf.String())
		}
	}
	// A --date naming TODAY moves nothing, so the same timesign still works.
	var buf bytes.Buffer
	today := dayAnchor(t, "2026-01-02", addNow, time.UTC)
	if err := cmdAdd(env(&buf, s, nil, addNow, time.UTC).on(today), nil, false, "+:20", "login", ""); err != nil {
		t.Fatalf("add --date today: %v", err)
	}
	if out := buf.String(); !strings.Contains(out, "14:40-15:00") || strings.Contains(out, " on ") {
		t.Errorf("output = %q, want 14:40-15:00 with no day note", out)
	}
}

// TestAddOnAnotherDayOverlapGuard keeps the exclusivity rule on the moved day:
// entries there are checked against each other, while an entry at the same
// clock time today is no conflict at all — they are different days.
func TestAddOnAnotherDayOverlapGuard(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)

	anchor := dayAnchor(t, "2026-01-05", addNow, time.UTC)
	if err := cmdAdd(env(&bytes.Buffer{}, s, nil, addNow, time.UTC), nil, false, "9-10", "login", ""); err != nil {
		t.Fatalf("seed today: %v", err)
	}
	// Same clock times, another day: accepted.
	if err := cmdAdd(env(&bytes.Buffer{}, s, nil, addNow, time.UTC).on(anchor), nil, false, "9-10", "login", ""); err != nil {
		t.Fatalf("add 9-10 on the 5th: %v", err)
	}
	// Overlapping something on that day: refused.
	var buf bytes.Buffer
	err := cmdAdd(env(&buf, s, nil, addNow, time.UTC).on(anchor), nil, false, "9:30-10:30", "review", "")
	if err == nil {
		t.Fatal("overlapping add on the moved day = nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "overlaps existing entry") {
		t.Errorf("err = %v, want an overlap refusal", err)
	}
	if buf.Len() != 0 {
		t.Errorf("output = %q, want nothing written", buf.String())
	}
}

// TestAddOnAnotherDayAcrossDST puts a moved day on a 23-hour one: Vilnius jumps
// 03:00 -> 04:00 on 2026-03-29, so the day's anchor must still sit inside that
// day (a "midnight + 24h" anchor would land at 01:00 on the 30th and file the
// entry, and its numbering, under the wrong day).
func TestAddOnAnotherDayAcrossDST(t *testing.T) {
	t.Parallel()
	loc := dstLoc(t)
	s := newStoreIn(t, loc)
	seedCatalog(t, s)

	now := time.Date(2026, 3, 27, 12, 0, 0, 0, loc)
	anchor := dayAnchor(t, "2026-03-29", now, loc)
	wantAnchor := time.Date(2026, 3, 29, 23, 59, 59, 0, loc)
	if !anchor.Equal(wantAnchor) {
		t.Fatalf("anchor = %v, want %v (inside the 23-hour day)", anchor, wantAnchor)
	}

	var buf bytes.Buffer
	if err := cmdAdd(env(&buf, s, nil, now, loc).on(anchor), nil, false, "9-10", "login", ""); err != nil {
		t.Fatalf("add: %v", err)
	}
	if !strings.Contains(buf.String(), "on 2026-03-29") {
		t.Errorf("output = %q, want it to name 2026-03-29", buf.String())
	}
	day := time.Date(2026, 3, 29, 0, 0, 0, 0, loc)
	entries := mustEntries(t, s, day, day.AddDate(0, 0, 1))
	if len(entries) != 1 {
		t.Fatalf("entries on the transition day = %d, want 1", len(entries))
	}
	if y, m, d := entries[0].Start.In(loc).Date(); y != 2026 || m != time.March || d != 29 {
		t.Errorf("start = %v, want it on 2026-03-29 in %v", entries[0].Start, loc)
	}
	// A bare duration still continues from that day's own last entry.
	buf.Reset()
	if err := cmdAdd(env(&buf, s, nil, now, loc).on(anchor), nil, false, ":30", "review", ""); err != nil {
		t.Fatalf("add duration: %v", err)
	}
	if out := buf.String(); !strings.Contains(out, "10:00-10:30") {
		t.Errorf("output = %q, want 10:00-10:30", out)
	}
}

// TestAddDescriptionInPushPayload verifies a description set via --desc reaches
// Toggl in the create payload on the best-effort push.
func TestAddDescriptionInPushPayload(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)

	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body = decodeBody(t, r)
		w.Write([]byte(`{"id":9200,"at":"2026-01-02T09:00:00Z"}`))
	}))
	defer srv.Close()
	c := api.New("tok", api.WithBaseURL(srv.URL), api.WithHTTPClient(srv.Client()))

	var buf bytes.Buffer
	if err := cmdAdd(env(&buf, s, c, addNow, time.UTC), nil, false, "9-:30", "login", "reset password flow"); err != nil {
		t.Fatalf("add: %v", err)
	}
	if got, _ := body["description"].(string); got != "reset password flow" {
		t.Errorf("description = %v, want %q", body["description"], "reset password flow")
	}
}

// TestAddKeepsRunningEntry verifies a pulled running entry survives an `add`:
// tg has no timer of its own, so nothing about adding a finished entry may
// close or drop one that came down from the Toggl web app.
func TestAddKeepsRunningEntry(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)

	// A pulled running entry must survive an `add`. The added span sits before
	// the running entry began (09:00) so the overlap guard is happy: a running
	// entry occupies everything from its start onwards.
	seedRunning(t, s, 12, testStart)
	var buf bytes.Buffer
	if err := cmdAdd(env(&buf, s, nil, addNow, time.UTC), nil, false, "7-8", "login", ""); err != nil {
		t.Fatalf("add: %v", err)
	}
	r := mustRunning(t, s)
	if r == nil || r.TaskID == nil || *r.TaskID != 12 {
		t.Fatalf("running entry = %+v, want Code review still running", r)
	}
}

// TestAddRejectsOverlap covers the overlap guard: a span colliding with an
// already tracked entry is refused, the error names the existing entry, and
// nothing is written. Touching the neighbour's endpoints stays allowed.
func TestAddRejectsOverlap(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)

	var buf bytes.Buffer
	if err := cmdAdd(env(&buf, s, nil, addNow, time.UTC), nil, false, "9-10", "login", ""); err != nil {
		t.Fatalf("add first: %v", err)
	}

	// 09:30-10:30 straddles the 09:00-10:00 entry.
	buf.Reset()
	err := cmdAdd(env(&buf, s, nil, addNow, time.UTC), nil, false, "9:30-10:30", "review", "")
	if err == nil {
		t.Fatal("overlapping add = nil error, want an error")
	}
	for _, want := range []string{"overlaps existing entry", "09:00-10:00", "Fix login bug"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to mention %q", err, want)
		}
	}
	if buf.Len() != 0 {
		t.Errorf("output = %q, want nothing written", buf.String())
	}

	// Only the first entry exists; the rejected one was never created.
	entries := addWindow(t, s, addNow)
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}

	// A back-to-back entry starting exactly when the first ends is fine.
	buf.Reset()
	if err := cmdAdd(env(&buf, s, nil, addNow, time.UTC), nil, false, "10-11", "review", ""); err != nil {
		t.Fatalf("touching add: %v", err)
	}
	if entries := addWindow(t, s, addNow); len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
}

// TestAddRejectsOverlapWithRunningEntry checks a running entry blocks any span
// reaching past its start, since it is still accruing time.
func TestAddRejectsOverlapWithRunningEntry(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)

	seedRunning(t, s, 12, testStart)

	var buf bytes.Buffer
	err := cmdAdd(env(&buf, s, nil, addNow, time.UTC), nil, false, "8:30-9:30", "login", "")
	if err == nil {
		t.Fatal("add over a running entry = nil error, want an error")
	}
	for _, want := range []string{"overlaps existing entry", "09:00-running", "Code review"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to mention %q", err, want)
		}
	}
	if entries := addWindow(t, s, addNow); len(entries) != 1 {
		t.Fatalf("entries = %d, want 1 (the running entry only)", len(entries))
	}
}

// TestAddAmbiguousFirstMatchWins is `-1`'s reason to exist: two tasks sharing a
// name in different projects cannot be told apart by any fragment, so without
// the flag `add` refuses to guess and with it the FIRST candidate — the one the
// ambiguity error lists first — is the task recorded against.
func TestAddAmbiguousFirstMatchWins(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)
	// The same task name in a second project; "Code review" is now an exact
	// match for two tasks at once.
	if err := s.UpsertTask(ctx, store.Task{
		ID: 22, WorkspaceID: 1, ProjectID: 2, Name: "Code review", Active: true,
	}); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	err := cmdAdd(env(&buf, s, nil, addNow, time.UTC), nil, false, "10-11", "code review", "")
	if err == nil || !strings.Contains(err.Error(), "multiple tasks match") {
		t.Fatalf("err = %v, want an ambiguity error without -1", err)
	}

	// Candidates are ordered by name then id, so the first one is task 12.
	buf.Reset()
	if err := cmdAdd(env(&buf, s, nil, addNow, time.UTC), nil, true, "10-11", "code review", ""); err != nil {
		t.Fatalf("add -1: %v", err)
	}
	entries := addWindow(t, s, addNow)
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	e := entries[0]
	if e.TaskID == nil || *e.TaskID != 12 {
		t.Errorf("task_id = %v, want 12 (the first candidate)", e.TaskID)
	}
	if e.ProjectID == nil || *e.ProjectID != 1 {
		t.Errorf("project_id = %v, want 1 (the first candidate's project)", e.ProjectID)
	}
}

// TestResolveTaskFragment pins the shared task-fragment resolver every
// fragment-taking command goes through: one match resolves, several fail with
// the candidate list unless `-1` takes the first, and none never resolves.
func TestResolveTaskFragment(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)

	for _, tc := range []struct {
		name     string
		fragment string
		first    bool
		wantID   int64
		wantErr  string
	}{
		{name: "unique", fragment: "login", wantID: 10},
		{name: "exact name wins", fragment: "fix", wantID: 11},
		{name: "ambiguous", fragment: "write", wantErr: "multiple tasks match"},
		{name: "ambiguous with -1", fragment: "write", first: true, wantID: 14}, // Write docs
		{name: "none", fragment: "nonexistent", wantErr: "no task matches"},
		{name: "none with -1", fragment: "nonexistent", first: true, wantErr: "no task matches"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := resolveTaskFragment(ctx, s, tc.fragment, nil, tc.first)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if got.ID != tc.wantID {
				t.Errorf("task = %d (%s), want %d", got.ID, got.Name, tc.wantID)
			}
		})
	}
}

func TestAddBestEffortPush(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)

	var body map[string]any
	var gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		body = decodeBody(t, r)
		w.Write([]byte(`{"id":9100,"at":"2026-01-02T09:00:00Z"}`))
	}))
	defer srv.Close()
	c := api.New("tok", api.WithBaseURL(srv.URL), api.WithHTTPClient(srv.Client()))

	var buf bytes.Buffer
	if err := cmdAdd(env(&buf, s, c, addNow, time.UTC), nil, false, "9-:30", "login", ""); err != nil {
		t.Fatalf("add: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if v, ok := body["task_id"].(float64); !ok || int64(v) != 10 {
		t.Errorf("task_id = %v, want 10", body["task_id"])
	}
	if v, ok := body["duration"].(float64); !ok || int64(v) != 1800 {
		t.Errorf("duration = %v, want 1800", body["duration"])
	}
	// A successful push marks the entry synced (remote id set, clean).
	r := mustEntryByRemoteID(t, s, 9100)
	if r == nil {
		t.Fatal("expected the added entry to be synced with its remote id")
	}
	if r.Dirty {
		t.Error("added entry should be clean after a successful push")
	}
}

func TestAddSyncFailureIsNonFatal(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := api.New("tok", api.WithBaseURL(srv.URL), api.WithHTTPClient(srv.Client()))

	var buf bytes.Buffer
	if err := cmdAdd(env(&buf, s, c, addNow, time.UTC), nil, false, "9-:30", "login", ""); err != nil {
		t.Fatalf("add should not fail on a sync error: %v", err)
	}
	if !strings.Contains(buf.String(), "warning") {
		t.Errorf("output = %q, want a sync warning", buf.String())
	}
	entries := addWindow(t, s, addNow)
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if !entries[0].Dirty {
		t.Error("added entry should stay dirty for a later push")
	}
	if entries[0].RemoteID != nil {
		t.Errorf("remote_id = %v, want nil (never synced)", entries[0].RemoteID)
	}
}

// --- mod / del ---------------------------------------------------------------

// modNow is the reference instant `mod`/`del` resolve relative timesigns and
// updated_at against. It sits after the fixture entries below (which live on
// 2026-01-02 morning), like a real invocation later in the day.
var modNow = time.Date(2026, 1, 2, 15, 7, 0, 0, time.UTC)

// seedModDay inserts two clean, already-synced finished entries on testStart's
// day (09:00-10:00 Fix login bug, 10:00-11:00 Code review). Inserting them is
// what numbers them, so they are addressable as entry 1 and 2 without any
// listing having to run first. Both carry a remote id and are non-dirty, so a
// later mod/del has to mark them dirty itself.
func seedModDay(t *testing.T, s *store.Store) []store.Entry {
	t.Helper()
	stop1 := testStart.Add(time.Hour)
	start2 := stop1
	stop2 := start2.Add(time.Hour)
	if _, err := s.CreateEntry(ctx, store.Entry{
		RemoteID: ptrInt(9001), WorkspaceID: 1, ProjectID: ptrInt(1), TaskID: ptrInt(10),
		Start: testStart, Stop: &stop1, Duration: 3600, UpdatedAt: stop1,
		SyncedAt: &stop1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateEntry(ctx, store.Entry{
		RemoteID: ptrInt(9002), WorkspaceID: 1, ProjectID: ptrInt(1), TaskID: ptrInt(12),
		Start: start2, Stop: &stop2, Duration: 3600, UpdatedAt: stop2,
		SyncedAt: &stop2,
	}); err != nil {
		t.Fatal(err)
	}
	entries, err := s.EntriesBetween(ctx, testStart.Add(-24*time.Hour), testStart.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("fixture has %d entries, want 2", len(entries))
	}
	for i, e := range entries {
		if e.Seq != i+1 {
			t.Fatalf("fixture entry %d has seq %d, want %d", i, e.Seq, i+1)
		}
	}
	return entries
}

// entryByID reads a single entry back from the store by local id, including
// deleted ones (which no listing returns), so tests can assert on the row that
// a mutation left behind.
func entryByID(t *testing.T, s *store.Store, id int64) store.Entry {
	t.Helper()
	dirty, err := s.DirtyEntries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range dirty {
		if e.ID == id {
			return e
		}
	}
	entries, err := s.EntriesBetween(ctx, testStart.Add(-48*time.Hour), testStart.Add(48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.ID == id {
			return e
		}
	}
	t.Fatalf("no entry with id %d", id)
	return store.Entry{}
}

// TestModLastEntryRelativeTimesign covers the default target (no number = the
// last entry) and mod's reading of a relative timesign: the start is kept and
// the stop is pushed 30m later, so the 1h entry becomes 1h30m. It is
// deliberately NOT re-anchored to now the way `tg add` would be, and it is not
// read as an absolute length either.
func TestModLastEntryRelativeTimesign(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)
	entries := seedModDay(t, s)
	last := entries[1] // 10:00-11:00 Code review

	var buf bytes.Buffer
	if err := cmdMod(env(&buf, s, nil, modNow, time.UTC), 0, "+:30", "", false); err != nil {
		t.Fatalf("mod: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"Modified: Code review", "10:00-11:30", "1h30m"} {
		if !strings.Contains(out, want) {
			t.Errorf("output = %q, want %q", out, want)
		}
	}

	got := entryByID(t, s, last.ID)
	if !got.Start.Equal(last.Start) {
		t.Errorf("start = %v, want it unchanged at %v", got.Start, last.Start)
	}
	wantStop := last.Stop.Add(30 * time.Minute)
	if got.Stop == nil || !got.Stop.Equal(wantStop) {
		t.Errorf("stop = %v, want %v", got.Stop, wantStop)
	}
	if got.Duration != 5400 {
		t.Errorf("duration = %d, want 5400", got.Duration)
	}
	if !got.Dirty {
		t.Error("modified entry should be dirty for a later push")
	}
	if !got.UpdatedAt.Equal(modNow) {
		t.Errorf("updated_at = %v, want %v", got.UpdatedAt, modNow)
	}
	// The remote identity survives, so the push is an update, not a re-create.
	if got.RemoteID == nil || *got.RemoteID != 9002 {
		t.Errorf("remote_id = %v, want 9002", got.RemoteID)
	}
	// The untargeted entry is untouched.
	if first := entryByID(t, s, entries[0].ID); first.Dirty {
		t.Error("mod touched the entry it was not addressing")
	}
}

func TestModUnitlessRelativeUsesMinutes(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)
	entries := seedModDay(t, s)
	last := entries[1] // 10:00-11:00 Code review

	var buf bytes.Buffer
	if err := cmdMod(env(&buf, s, nil, modNow, time.UTC), 0, "+20", "", false); err != nil {
		t.Fatalf("mod: %v", err)
	}

	got := entryByID(t, s, last.ID)
	wantStop := last.Stop.Add(20 * time.Minute)
	if got.Stop == nil || !got.Stop.Equal(wantStop) {
		t.Errorf("stop = %v, want %v", got.Stop, wantStop)
	}
}

// TestModRelativeAddsToTheEnd pins the arithmetic a relative timesign does: the
// duration is added to the entry's CURRENT end, not to its start and not to
// now, so repeating the edit keeps pushing the end out. The 1h entry ending at
// 11:00 ends at 11:30, then 12:45 — never at start+duration (which would shrink
// it back to 30m) and never anywhere near modNow (15:07).
func TestModRelativeAddsToTheEnd(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)
	entries := seedModDay(t, s)
	last := entries[1] // 10:00-11:00 Code review

	var buf bytes.Buffer
	if err := cmdMod(env(&buf, s, nil, modNow, time.UTC), 2, "+:30", "", false); err != nil {
		t.Fatalf("first mod: %v", err)
	}
	buf.Reset()
	if err := cmdMod(env(&buf, s, nil, modNow, time.UTC), 2, "+1:15", "", false); err != nil {
		t.Fatalf("second mod: %v", err)
	}

	got := entryByID(t, s, last.ID)
	if !got.Start.Equal(last.Start) {
		t.Errorf("start = %v, want it unchanged at %v", got.Start, last.Start)
	}
	wantStop := time.Date(2026, 1, 2, 12, 45, 0, 0, time.UTC)
	if got.Stop == nil || !got.Stop.Equal(wantStop) {
		t.Errorf("stop = %v, want %v (11:00 + 30m + 1h15m)", got.Stop, wantStop)
	}
	if got.Duration != 9900 {
		t.Errorf("duration = %d, want 9900 (2h45m)", got.Duration)
	}
}

// TestModNegativeSubtractsFromTheEnd is the mirror of
// TestModRelativeAddsToTheEnd: a negative timesign takes its duration OFF the
// entry's current end, keeping the start, so the 1h entry ending at 11:00 loses
// 30 minutes and then 15 more. Like "+" it is not re-anchored to modNow (15:07)
// and not read as an absolute length, so repeating it keeps trimming.
func TestModNegativeSubtractsFromTheEnd(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)
	entries := seedModDay(t, s)
	last := entries[1] // 10:00-11:00 Code review

	var buf bytes.Buffer
	if err := cmdMod(env(&buf, s, nil, modNow, time.UTC), 0, "-:30", "", false); err != nil {
		t.Fatalf("first mod: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"Modified: Code review", "10:00-10:30", "0h30m"} {
		if !strings.Contains(out, want) {
			t.Errorf("output = %q, want %q", out, want)
		}
	}
	buf.Reset()
	if err := cmdMod(env(&buf, s, nil, modNow, time.UTC), 2, "-:15", "", false); err != nil {
		t.Fatalf("second mod: %v", err)
	}

	got := entryByID(t, s, last.ID)
	if !got.Start.Equal(last.Start) {
		t.Errorf("start = %v, want it unchanged at %v", got.Start, last.Start)
	}
	wantStop := time.Date(2026, 1, 2, 10, 15, 0, 0, time.UTC)
	if got.Stop == nil || !got.Stop.Equal(wantStop) {
		t.Errorf("stop = %v, want %v (11:00 - 30m - 15m)", got.Stop, wantStop)
	}
	if got.Duration != 900 {
		t.Errorf("duration = %d, want 900 (15m)", got.Duration)
	}
	if !got.Dirty {
		t.Error("modified entry should be dirty for a later push")
	}
	if !got.UpdatedAt.Equal(modNow) {
		t.Errorf("updated_at = %v, want %v", got.UpdatedAt, modNow)
	}
	// The remote identity survives, so the push is an update, not a re-create.
	if got.RemoteID == nil || *got.RemoteID != 9002 {
		t.Errorf("remote_id = %v, want 9002", got.RemoteID)
	}
	// The untargeted entry is untouched.
	if first := entryByID(t, s, entries[0].ID); first.Dirty {
		t.Error("mod touched the entry it was not addressing")
	}
}

// TestModNegativeSpellings tables the negative form's grammar as `tg mod` reads
// it, next to the "+" spelling it mirrors: an all-digit sign is MINUTES ("-10"),
// "-H:MM" is hours and minutes, and "-H" is whole hours. The fixture entry 2 is
// 10:00-11:00, so every case is measured back from 11:00.
func TestModNegativeSpellings(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		timesign string
		wantStop time.Time
	}{
		{"-10", time.Date(2026, 1, 2, 10, 50, 0, 0, time.UTC)},   // unitless: minutes
		{"-:10", time.Date(2026, 1, 2, 10, 50, 0, 0, time.UTC)},  // the same, spelled out
		{"-1", time.Date(2026, 1, 2, 10, 59, 0, 0, time.UTC)},    // unitless again: ONE minute, not one hour
		{"-30", time.Date(2026, 1, 2, 10, 30, 0, 0, time.UTC)},   // half the entry
		{"-59", time.Date(2026, 1, 2, 10, 1, 0, 0, time.UTC)},    // all but a minute
		{"-0:59", time.Date(2026, 1, 2, 10, 1, 0, 0, time.UTC)},  // the same, zero-padded
		{" -15 ", time.Date(2026, 1, 2, 10, 45, 0, 0, time.UTC)}, // whitespace tolerated
	} {
		t.Run(strings.TrimSpace(tc.timesign), func(t *testing.T) {
			t.Parallel()
			s := newStore(t)
			seedCatalog(t, s)
			entries := seedModDay(t, s)

			var buf bytes.Buffer
			if err := cmdMod(env(&buf, s, nil, modNow, time.UTC), 2, tc.timesign, "", false); err != nil {
				t.Fatalf("mod %q: %v", tc.timesign, err)
			}
			got := entryByID(t, s, entries[1].ID)
			if !got.Start.Equal(entries[1].Start) {
				t.Errorf("start = %v, want it unchanged at %v", got.Start, entries[1].Start)
			}
			if got.Stop == nil || !got.Stop.Equal(tc.wantStop) {
				t.Errorf("mod %q: stop = %v, want %v", tc.timesign, got.Stop, tc.wantStop)
			}
			if want := int64(tc.wantStop.Sub(got.Start) / time.Second); got.Duration != want {
				t.Errorf("mod %q: duration = %d, want %d", tc.timesign, got.Duration, want)
			}
		})
	}
}

// TestModNegativeIsTheInverseOfRelative pins the symmetry the two signs promise:
// adding a duration and then taking the same one back leaves the entry exactly
// as it was, whichever spelling is used. A longer subtraction than the entry's
// original length is what TestModNegativeRefusesEmptyingTheEntry covers.
func TestModNegativeIsTheInverseOfRelative(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)
	entries := seedModDay(t, s)
	last := entries[1] // 10:00-11:00 Code review

	var buf bytes.Buffer
	if err := cmdMod(env(&buf, s, nil, modNow, time.UTC), 2, "+1:20", "", false); err != nil {
		t.Fatalf("mod +1:20: %v", err)
	}
	buf.Reset()
	if err := cmdMod(env(&buf, s, nil, modNow, time.UTC), 2, "-1:20", "", false); err != nil {
		t.Fatalf("mod -1:20: %v", err)
	}

	got := entryByID(t, s, last.ID)
	if !got.Start.Equal(last.Start) || got.Stop == nil || !got.Stop.Equal(*last.Stop) {
		t.Errorf("entry = %v-%v, want the original %v-%v", got.Start, got.Stop, last.Start, last.Stop)
	}
	if got.Duration != last.Duration {
		t.Errorf("duration = %d, want the original %d", got.Duration, last.Duration)
	}
}

// TestModNegativeRefusesEmptyingTheEntry covers the edge of the negative form:
// an entry must keep some length, so taking off exactly its duration (which
// would leave nothing) or more (which would turn it inside out) is refused and
// nothing is written. `tg del` is how an entry is removed.
func TestModNegativeRefusesEmptyingTheEntry(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		timesign string
	}{
		{name: "exactly the whole entry", timesign: "-1:00"},
		{name: "a minute more than the entry", timesign: "-1:01"},
		{name: "more than the entry", timesign: "-1:30"},
		{name: "far more than the entry", timesign: "-23:59"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newStore(t)
			seedCatalog(t, s)
			entries := seedModDay(t, s)

			var buf bytes.Buffer
			err := cmdMod(env(&buf, s, nil, modNow, time.UTC), 2, tc.timesign, "", false)
			if err == nil {
				t.Fatalf("mod %q = nil error, want a refusal", tc.timesign)
			}
			for _, want := range []string{"must keep some length", "tg del"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %v, want it to mention %q", err, want)
				}
			}
			if buf.Len() != 0 {
				t.Errorf("output = %q, want nothing written", buf.String())
			}
			got := entryByID(t, s, entries[1].ID)
			if got.Dirty || got.Duration != 3600 || !got.Start.Equal(entries[1].Start) {
				t.Errorf("entry = %+v, want the original 1h clean entry", got)
			}
		})
	}
}

// TestModRelativeRefusesRunningEntry covers the one entry a signed timesign
// cannot act on: a running entry has no end to move, so mod says so instead of
// inventing one from now. An absolute sign still gives it a finished span.
func TestModRelativeRefusesRunningEntry(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)

	start := time.Date(2026, 1, 2, 14, 0, 0, 0, time.UTC)
	id, err := s.CreateEntry(ctx, store.Entry{
		WorkspaceID: 1, ProjectID: ptrInt(1), TaskID: ptrInt(10),
		Start: start, Duration: -1, UpdatedAt: start,
	})
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	// Both signs move the END, so neither has anything to work with; the
	// refusal names the direction that was asked for.
	for timesign, verb := range map[string]string{"+:30": "extend", "-:30": "shorten"} {
		buf.Reset()
		err = cmdMod(env(&buf, s, nil, modNow, time.UTC), 0, timesign, "", false)
		if err == nil {
			t.Fatalf("mod %s on a running entry = nil error, want a refusal", timesign)
		}
		for _, want := range []string{"running", verb} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("mod %s: err = %v, want it to mention %q", timesign, err, want)
			}
		}
		if buf.Len() != 0 {
			t.Errorf("output = %q, want nothing written", buf.String())
		}
		if got := entryByID(t, s, id); got.Stop != nil || got.Duration != -1 {
			t.Errorf("entry = %+v, want it left running", got)
		}
	}

	// An absolute timesign is still accepted and closes the entry.
	buf.Reset()
	if err := cmdMod(env(&buf, s, nil, modNow, time.UTC), 0, "14-14:30", "", false); err != nil {
		t.Fatalf("absolute mod on a running entry: %v", err)
	}
	if got := entryByID(t, s, id); got.Stop == nil || got.Duration != 1800 {
		t.Errorf("entry = %+v, want it finished at 14:30", got)
	}
}

// TestModLastEntrySkipsFutureEntry covers the second half of the shared
// resolution from mod's side: an entry booked for later today (here three hours
// ahead of modNow) is not the last entry, so a bare `tg mod` still edits the
// last thing actually tracked and leaves the future one alone.
func TestModLastEntrySkipsFutureEntry(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)
	entries := seedModDay(t, s) // 09:00-10:00 and 10:00-11:00 on 2026-01-02

	future := modNow.Add(3 * time.Hour) // 18:07, same day
	futureStop := future.Add(time.Hour)
	futureID, err := s.CreateEntry(ctx, store.Entry{
		WorkspaceID: 1, ProjectID: ptrInt(1), TaskID: ptrInt(13),
		Start: future, Stop: &futureStop, Duration: 3600, UpdatedAt: modNow,
	})
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := cmdMod(env(&buf, s, nil, modNow, time.UTC), 0, "+:30", "", false); err != nil {
		t.Fatalf("mod: %v", err)
	}
	if out := buf.String(); !strings.Contains(out, "Modified: Code review") {
		t.Errorf("output = %q, want the 10:00 entry, not the one starting later today", out)
	}
	if got := entryByID(t, s, entries[1].ID); got.Duration != 5400 {
		t.Errorf("last entry duration = %d, want 5400", got.Duration)
	}
	if got := entryByID(t, s, futureID); got.Dirty || got.Duration != 3600 {
		t.Errorf("future entry = %+v, want it untouched", got)
	}
}

// TestModEntryByNumber verifies an explicit local number addresses that entry
// (not the last one) and that an absolute timesign sets the range on the
// ENTRY's own calendar day. Editing is restricted to today, so the entry's day
// and today's are necessarily the same here; the restriction itself is covered
// by TestModRefusesEntryOlderThanToday.
func TestModEntryByNumber(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)
	entries := seedModDay(t, s)

	var buf bytes.Buffer
	if err := cmdMod(env(&buf, s, nil, modNow, time.UTC), 1, "8-9:30", "", false); err != nil {
		t.Fatalf("mod: %v", err)
	}
	if out := buf.String(); !strings.Contains(out, "Modified: Fix login bug") || !strings.Contains(out, "08:00-09:30") {
		t.Errorf("output = %q, want the retimed first entry", out)
	}

	got := entryByID(t, s, entries[0].ID)
	wantStart := time.Date(2026, 1, 2, 8, 0, 0, 0, time.UTC)
	wantStop := time.Date(2026, 1, 2, 9, 30, 0, 0, time.UTC)
	if !got.Start.Equal(wantStart) {
		t.Errorf("start = %v, want %v (the entry's own day)", got.Start, wantStart)
	}
	if got.Stop == nil || !got.Stop.Equal(wantStop) {
		t.Errorf("stop = %v, want %v", got.Stop, wantStop)
	}
	if got.Duration != 5400 {
		t.Errorf("duration = %d, want 5400", got.Duration)
	}
	if second := entryByID(t, s, entries[1].ID); second.Dirty {
		t.Error("mod 1 must not touch entry 2")
	}
}

// TestModRefusesEntryOlderThanToday covers where history becomes unreachable:
// once an entry's day is over it is no longer the last entry (store.LastEntry
// is today-only), so a bare `tg mod` has nothing to act on, whether the edit is
// a retime or just a description. Nothing is written, nothing is printed, and
// no request is made to Toggl. Addressing yesterday's entry BY NUMBER is just
// as unreachable (numbers are per-day, so today's numbering holds nothing); see
// TestModNumberIsScopedToToday. The store-level failsafe that backs both up
// (store.ErrEntryTooOld) is covered in the store's own tests.
func TestModRefusesEntryOlderThanToday(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)
	entries := seedModDay(t, s)

	// The fixture sits on 2026-01-02; run mod the next day.
	nextDay := modNow.AddDate(0, 0, 1)

	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := api.New("tok", api.WithBaseURL(srv.URL), api.WithHTTPClient(srv.Client()))

	cases := []struct {
		name     string
		ref      int
		timesign string
		desc     string
		setDesc  bool
	}{
		{"last entry, relative retime", 0, "+:30", "", false},
		{"last entry, absolute retime", 0, "8-9:30", "", false},
		{"last entry, description", 0, "", "late edit", true},
	}
	for _, tc := range cases {
		var buf bytes.Buffer
		err := cmdMod(env(&buf, s, c, nextDay, time.UTC), tc.ref, tc.timesign, tc.desc, tc.setDesc)
		if err == nil {
			t.Errorf("%s: err = nil, want a refusal", tc.name)
		} else if !strings.Contains(err.Error(), "today") {
			t.Errorf("%s: err = %v, want it to say nothing was tracked today", tc.name, err)
		}
		if buf.Len() != 0 {
			t.Errorf("%s: output = %q, want nothing written", tc.name, buf.String())
		}
	}
	if hits != 0 {
		t.Errorf("Toggl requests = %d, want 0 (a refused mod syncs nothing)", hits)
	}

	for i, e := range entries {
		got := entryByID(t, s, e.ID)
		if got.Dirty {
			t.Errorf("entry %d is dirty, want it untouched", i+1)
		}
		if got.Duration != e.Duration || !got.Start.Equal(e.Start) || got.Description != e.Description {
			t.Errorf("entry %d = %+v, want it unchanged (%+v)", i+1, got, e)
		}
	}
}

// TestModNumberIsScopedToToday pins where entry numbers point: they are per-day
// and `mod`/`del` resolve them on today's day, so yesterday's number is simply
// not addressable today (rather than silently reaching back into history, which
// mod would refuse anyway). Nothing is written.
func TestModNumberIsScopedToToday(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)
	entries := seedModDay(t, s)

	nextDay := modNow.AddDate(0, 0, 1)
	var buf bytes.Buffer
	err := cmdMod(env(&buf, s, nil, nextDay, time.UTC), 1, "+:30", "", false)
	if !errors.Is(err, store.ErrNoEntryNum) {
		t.Fatalf("err = %v, want store.ErrNoEntryNum", err)
	}
	if !strings.Contains(err.Error(), "2026-01-03") {
		t.Errorf("err = %v, want it to name the day it searched", err)
	}
	if buf.Len() != 0 {
		t.Errorf("output = %q, want nothing written", buf.String())
	}
	if got := entryByID(t, s, entries[0].ID); got.Dirty {
		t.Error("a refused mod must not touch the entry")
	}

	// `del` resolves numbers the same way.
	buf.Reset()
	if err := cmdDel(env(&buf, s, nil, nextDay, time.UTC), 1); !errors.Is(err, store.ErrNoEntryNum) {
		t.Errorf("del err = %v, want store.ErrNoEntryNum", err)
	}
}

// TestModAllowsTodaysEntryAtDayEnd guards the boundary from the other side: an
// entry started earlier on the current day stays editable right up to midnight.
func TestModAllowsTodaysEntryAtDayEnd(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)
	entries := seedModDay(t, s)

	lateToday := time.Date(2026, 1, 2, 23, 59, 0, 0, time.UTC)
	var buf bytes.Buffer
	// Entry 2 (10:00-11:00) is the one with room to grow; entry 1 butts
	// straight into it (see TestModRejectsOverlap).
	if err := cmdMod(env(&buf, s, nil, lateToday, time.UTC), 2, "+:30", "", false); err != nil {
		t.Fatalf("mod at 23:59 on the entry's own day: %v", err)
	}
	if got := entryByID(t, s, entries[1].ID); got.Duration != 5400 {
		t.Errorf("duration = %d, want 5400", got.Duration)
	}
}

// seedFutureDay books two back-to-back entries (09:00-10:00, 10:00-11:00) on
// the day `--date` names, through `tg add` itself: that is the only way entries
// land on a later day, and it is also what numbers them 1 and 2 there. It
// returns the day's anchor and the entries as stored.
func seedFutureDay(t *testing.T, s *store.Store, date string, now time.Time, loc *time.Location) (time.Time, []store.Entry) {
	t.Helper()
	anchor := dayAnchor(t, date, now, loc)
	for _, timesign := range []string{"9-10", "10-11"} {
		if err := cmdAdd(env(io.Discard, s, nil, now, loc).on(anchor), nil, false, timesign, "login", ""); err != nil {
			t.Fatalf("seed %s on %s: %v", timesign, date, err)
		}
	}
	day := startOfDay(anchor, loc)
	entries := mustEntries(t, s, day, day.AddDate(0, 0, 1))
	if len(entries) != 2 {
		t.Fatalf("fixture has %d entries on %s, want 2", len(entries), date)
	}
	for i, e := range entries {
		if e.Seq != i+1 {
			t.Fatalf("fixture entry %d on %s has seq %d, want %d", i, date, e.Seq, i+1)
		}
	}
	return anchor, entries
}

// TestModOnAnotherDay covers `tg mod --date`: the entry numbers and the "last
// entry" are the named day's, an absolute timesign is resolved on that day (it
// never drags the entry onto today), and a signed one moves its end there. The
// confirmation names the day, and the entry is left dirty for a push like any
// other edit.
func TestModOnAnotherDay(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)
	anchor, entries := seedFutureDay(t, s, "2026-01-05", addNow, time.UTC)

	// No number: the last entry of THAT day (10:00-11:00), not of today.
	var buf bytes.Buffer
	if err := cmdMod(env(&buf, s, nil, addNow, time.UTC).on(anchor), 0, "+:30", "", false); err != nil {
		t.Fatalf("mod: %v", err)
	}
	for _, want := range []string{"Modified: Fix login bug", "10:00-11:30", "1h30m", "on 2026-01-05"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("output = %q, want it to contain %q", buf.String(), want)
		}
	}
	got := entryByID(t, s, entries[1].ID)
	if got.Duration != 5400 {
		t.Errorf("duration = %d, want 5400", got.Duration)
	}
	if !got.Dirty {
		t.Error("a mod on another day should still mark the entry dirty")
	}
	if !got.UpdatedAt.Equal(addNow) {
		t.Errorf("updated_at = %v, want the real clock %v", got.UpdatedAt, addNow)
	}

	// By number, resolved on that day's own numbering, with an absolute
	// timesign: it lands on the entry's day, not on today's.
	buf.Reset()
	if err := cmdMod(env(&buf, s, nil, addNow, time.UTC).on(anchor), 1, "8-8:30", "", false); err != nil {
		t.Fatalf("mod 1: %v", err)
	}
	got = entryByID(t, s, entries[0].ID)
	wantStart := time.Date(2026, 1, 5, 8, 0, 0, 0, time.UTC)
	wantStop := time.Date(2026, 1, 5, 8, 30, 0, 0, time.UTC)
	if !got.Start.Equal(wantStart) || got.Stop == nil || !got.Stop.Equal(wantStop) {
		t.Errorf("span = %v-%v, want %v-%v", got.Start, got.Stop, wantStart, wantStop)
	}
	if got.Seq != 1 {
		t.Errorf("seq = %d, want it still 1 on its own day", got.Seq)
	}
}

// TestModOnAnotherDayScopesTheDay pins the flip side: without --date the same
// entries are unreachable, since numbers and the last entry are today's. The
// refusals name the day they searched, and nothing is written either way.
func TestModOnAnotherDayScopesTheDay(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)
	_, entries := seedFutureDay(t, s, "2026-01-05", addNow, time.UTC)

	// Today: nothing tracked, so a bare mod has no subject...
	var buf bytes.Buffer
	err := cmdMod(env(&buf, s, nil, addNow, time.UTC), 0, "+:30", "", false)
	if err == nil || !strings.Contains(err.Error(), "no entry tracked today to modify") {
		t.Fatalf("err = %v, want the today-only refusal", err)
	}
	// ...and the 5th's numbers are not today's numbers.
	buf.Reset()
	err = cmdMod(env(&buf, s, nil, addNow, time.UTC), 1, "+:30", "", false)
	if !errors.Is(err, store.ErrNoEntryNum) {
		t.Fatalf("err = %v, want store.ErrNoEntryNum", err)
	}
	if !strings.Contains(err.Error(), "2026-01-02") {
		t.Errorf("err = %v, want it to name the day it searched", err)
	}
	// A number that day never handed out is still a miss with --date.
	buf.Reset()
	anchor := dayAnchor(t, "2026-01-05", addNow, time.UTC)
	err = cmdMod(env(&buf, s, nil, addNow, time.UTC).on(anchor), 7, "+:30", "", false)
	if !errors.Is(err, store.ErrNoEntryNum) {
		t.Fatalf("err = %v, want store.ErrNoEntryNum", err)
	}
	if !strings.Contains(err.Error(), "2026-01-05") {
		t.Errorf("err = %v, want it to name the moved day", err)
	}
	// A day with nothing on it reports so by name.
	buf.Reset()
	empty := dayAnchor(t, "2026-01-09", addNow, time.UTC)
	err = cmdMod(env(&buf, s, nil, addNow, time.UTC).on(empty), 0, "+:30", "", false)
	if err == nil || !strings.Contains(err.Error(), "no entry tracked on 2026-01-09 to modify") {
		t.Fatalf("err = %v, want the refusal to name 2026-01-09", err)
	}
	if buf.Len() != 0 {
		t.Errorf("output = %q, want nothing written", buf.String())
	}
	// The fixture entries were created by `tg add`, so they are already dirty;
	// what a refused mod must not do is change them.
	for i, e := range entries {
		got := entryByID(t, s, e.ID)
		if !got.Start.Equal(e.Start) || got.Duration != e.Duration {
			t.Errorf("entry %d = %v (%ds), want it unchanged at %v (%ds)",
				i+1, got.Start, got.Duration, e.Start, e.Duration)
		}
	}
}

// TestModDescriptionOnly verifies --desc alone is a valid change and leaves the
// times exactly as they were.
func TestModDescriptionOnly(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)
	entries := seedModDay(t, s)
	target := entries[0]

	var buf bytes.Buffer
	if err := cmdMod(env(&buf, s, nil, modNow, time.UTC), 1, "", "rebased onto main", true); err != nil {
		t.Fatalf("mod: %v", err)
	}
	got := entryByID(t, s, target.ID)
	if got.Description != "rebased onto main" {
		t.Errorf("description = %q, want %q", got.Description, "rebased onto main")
	}
	if !got.Start.Equal(target.Start) || got.Stop == nil || !got.Stop.Equal(*target.Stop) {
		t.Errorf("times = %v-%v, want them unchanged", got.Start, got.Stop)
	}
	if got.Duration != target.Duration {
		t.Errorf("duration = %d, want %d", got.Duration, target.Duration)
	}
	if !got.Dirty {
		t.Error("modified entry should be dirty")
	}

	// An explicitly empty --desc clears the description again.
	buf.Reset()
	if err := cmdMod(env(&buf, s, nil, modNow, time.UTC), 1, "", "", true); err != nil {
		t.Fatalf("mod --desc \"\": %v", err)
	}
	if got := entryByID(t, s, target.ID); got.Description != "" {
		t.Errorf("description = %q, want it cleared", got.Description)
	}
}

// TestModTimesignAndDescription verifies both changes can be applied at once.
func TestModTimesignAndDescription(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)
	entries := seedModDay(t, s)

	var buf bytes.Buffer
	if err := cmdMod(env(&buf, s, nil, modNow, time.UTC), 2, "+:45", "pairing", true); err != nil {
		t.Fatalf("mod: %v", err)
	}
	got := entryByID(t, s, entries[1].ID)
	if got.Duration != 6300 {
		t.Errorf("duration = %d, want 6300 (1h + the 45m added)", got.Duration)
	}
	wantStop := entries[1].Stop.Add(45 * time.Minute)
	if got.Stop == nil || !got.Stop.Equal(wantStop) {
		t.Errorf("stop = %v, want %v", got.Stop, wantStop)
	}
	if got.Description != "pairing" {
		t.Errorf("description = %q, want %q", got.Description, "pairing")
	}
}

// TestModRejectsOverlap covers the overlap guard: growing an entry into its
// neighbour is refused, the error names the neighbour, and nothing is written.
// Growing it up to the neighbour's start stays allowed (half-open intervals),
// which also proves the entry is not compared against itself.
func TestModRejectsOverlap(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)
	entries := seedModDay(t, s)

	// Entry 1 is 09:00-10:00 and entry 2 is 10:00-11:00, so pushing entry 1's
	// end 90 minutes later would run into entry 2.
	var buf bytes.Buffer
	err := cmdMod(env(&buf, s, nil, modNow, time.UTC), 1, "+1:30", "", false)
	if err == nil {
		t.Fatal("overlapping mod = nil error, want an error")
	}
	for _, want := range []string{"overlaps existing entry", "10:00-11:00", "Code review"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to mention %q", err, want)
		}
	}
	if buf.Len() != 0 {
		t.Errorf("output = %q, want nothing written", buf.String())
	}
	got := entryByID(t, s, entries[0].ID)
	if got.Duration != 3600 || got.Dirty {
		t.Errorf("entry = %+v, want the original 1h clean entry", got)
	}

	// Shrink entry 1 to 09:00-09:30 so there is room to grow it back, which
	// also shows the entry is not compared against its own old span.
	buf.Reset()
	if err := cmdMod(env(&buf, s, nil, modNow, time.UTC), 1, "9-9:30", "", false); err != nil {
		t.Fatalf("shrinking mod: %v", err)
	}

	// Extending it exactly up to the neighbour's start is allowed: the
	// intervals are half-open, so 09:00-10:00 and 10:00-11:00 do not overlap.
	buf.Reset()
	if err := cmdMod(env(&buf, s, nil, modNow, time.UTC), 1, "+:30", "", false); err != nil {
		t.Fatalf("touching mod: %v", err)
	}
	if got := entryByID(t, s, entries[0].ID); got.Duration != 3600 {
		t.Errorf("duration = %d, want 3600 (grown back up to 10:00)", got.Duration)
	}

	// One more minute would collide, and a refused extension leaves the entry
	// exactly as it was.
	buf.Reset()
	if err := cmdMod(env(&buf, s, nil, modNow, time.UTC), 1, "+:01", "", false); err == nil {
		t.Error("extending past the neighbour's start = nil error, want an overlap error")
	}
	if got := entryByID(t, s, entries[0].ID); got.Duration != 3600 {
		t.Errorf("duration = %d, want the 3600 from the previous edit", got.Duration)
	}
}

// TestModRefusals tables the edits `tg mod` declines outright: an invocation
// asking for no change at all, a number today's numbering never handed out, a
// bare `tg mod` on a day with nothing tracked, and the malformed timesigns.
// Every one of them must leave the store exactly as it was — nothing written,
// nothing marked dirty — and print nothing, since a refused mod is not a
// partial one. (The day-scope refusals have fixtures of their own: see
// TestModRefusesEntryOlderThanToday and TestModNumberIsScopedToToday.)
func TestModRefusals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		seed      bool // seed the two-entry fixture day
		ref       int
		timesign  string
		desc      string
		setDesc   bool
		wantErrIs error
		wantErr   []string
	}{
		{
			name: "no change requested", seed: true, ref: 1,
			wantErr: []string{"usage: tg mod"},
		},
		{
			name: "number never handed out", seed: true, ref: 7, timesign: "+:30",
			wantErrIs: store.ErrNoEntryNum, wantErr: []string{"tg ls"},
		},
		{
			name: "nothing tracked today", timesign: "+:30",
			wantErr: []string{"no entry tracked today"},
		},
		{name: "zero-length relative timesign", seed: true, ref: 1, timesign: "+:00"},
		{name: "zero-length negative timesign", seed: true, ref: 1, timesign: "-:00"},
		{name: "negative timesign out of range", seed: true, ref: 1, timesign: "-1:99"},
		// Not a duration shape, so it is not a negative sign at all and is
		// reported as the malformed range it looks like.
		{name: "negative-looking garbage", seed: true, ref: 1, timesign: "-1:2:3"},
		{name: "reversed absolute timesign", seed: true, ref: 1, timesign: "10-9"},
		{name: "unparsable timesign", seed: true, ref: 1, timesign: "nonsense"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newStore(t)
			seedCatalog(t, s)
			var entries []store.Entry
			if tc.seed {
				entries = seedModDay(t, s)
			}

			var buf bytes.Buffer
			err := cmdMod(env(&buf, s, nil, modNow, time.UTC), tc.ref, tc.timesign, tc.desc, tc.setDesc)
			if err == nil {
				t.Fatalf("mod(%d, %q) = nil error, want a refusal", tc.ref, tc.timesign)
			}
			if tc.wantErrIs != nil && !errors.Is(err, tc.wantErrIs) {
				t.Errorf("err = %v, want it to wrap %v", err, tc.wantErrIs)
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %v, want it to mention %q", err, want)
				}
			}
			if buf.Len() != 0 {
				t.Errorf("output = %q, want nothing written", buf.String())
			}
			for i, e := range entries {
				got := entryByID(t, s, e.ID)
				if got.Dirty {
					t.Errorf("entry %d is dirty, want a refused mod to leave it alone", i+1)
				}
				if got.Duration != e.Duration || !got.Start.Equal(e.Start) || got.Description != e.Description {
					t.Errorf("entry %d = %+v, want it unchanged (%+v)", i+1, got, e)
				}
			}
		})
	}
}

// TestModPushesBestEffort verifies a successful mod pushes the change straight
// to Toggl as an update (the entry keeps its remote id) and comes back clean.
func TestModPushesBestEffort(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)
	entries := seedModDay(t, s)

	var method, path string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		body = decodeBody(t, r)
		w.Write([]byte(`{"id":9002,"at":"2026-01-02T15:07:00Z"}`))
	}))
	defer srv.Close()
	c := api.New("tok", api.WithBaseURL(srv.URL), api.WithHTTPClient(srv.Client()))

	var buf bytes.Buffer
	if err := cmdMod(env(&buf, s, c, modNow, time.UTC), 2, "+:30", "", false); err != nil {
		t.Fatalf("mod: %v", err)
	}
	if method != http.MethodPut {
		t.Errorf("method = %s %s, want PUT (an update, not a create)", method, path)
	}
	if got, _ := body["stop"].(string); got != "2026-01-02T11:30:00Z" {
		t.Errorf("pushed stop = %v, want 2026-01-02T11:30:00Z", body["stop"])
	}
	if got := entryByID(t, s, entries[1].ID); got.Dirty {
		t.Error("entry should be clean after a successful push")
	}
}

// TestModSyncFailureIsNonFatal verifies a failed push only warns: the local
// edit stands and stays dirty for a later `tg push`.
func TestModSyncFailureIsNonFatal(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)
	entries := seedModDay(t, s)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := api.New("tok", api.WithBaseURL(srv.URL), api.WithHTTPClient(srv.Client()))

	var buf bytes.Buffer
	if err := cmdMod(env(&buf, s, c, modNow, time.UTC), 2, "+:30", "", false); err != nil {
		t.Fatalf("mod should not fail on a sync error: %v", err)
	}
	if !strings.Contains(buf.String(), "warning") {
		t.Errorf("output = %q, want a sync warning", buf.String())
	}
	got := entryByID(t, s, entries[1].ID)
	if !got.Dirty || got.Duration != 5400 {
		t.Errorf("entry = %+v, want the +30m edit kept and dirty", got)
	}
}

// TestDelRemovesEntry covers the happy path: the addressed entry is confirmed,
// disappears from listings, and the other entry survives.
func TestDelRemovesEntry(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)
	entries := seedModDay(t, s)

	var buf bytes.Buffer
	if err := cmdDel(env(&buf, s, nil, modNow, time.UTC), 1); err != nil {
		t.Fatalf("del: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"Deleted: Fix login bug", "09:00-10:00", "1h00m"} {
		if !strings.Contains(out, want) {
			t.Errorf("output = %q, want %q", out, want)
		}
	}

	left := mustEntries(t, s, testStart.Add(-24*time.Hour), testStart.Add(24*time.Hour))
	if len(left) != 1 || left[0].ID != entries[1].ID {
		t.Fatalf("remaining entries = %+v, want only entry 2", left)
	}
	// The number now resolves to nothing rather than to another entry: entry 2
	// keeps being entry 2 instead of sliding into the freed slot.
	if _, err := s.EntryByNum(ctx, 1, modNow); !errors.Is(err, store.ErrNoEntryNum) {
		t.Errorf("EntryByNum(1) after del = %v, want ErrNoEntryNum", err)
	}
	if got, err := s.EntryByNum(ctx, 2, modNow); err != nil || got.ID != entries[1].ID {
		t.Errorf("EntryByNum(2) after del = %+v err=%v, want the surviving entry %d",
			got, err, entries[1].ID)
	}
}

// TestDelMarksDeletedAndDirty pins the soft delete: the row survives, flagged
// deleted and dirty with a fresh LWW clock, so togglsync.Push can DELETE it remotely
// before dropping it.
func TestDelMarksDeletedAndDirty(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)
	entries := seedModDay(t, s)

	var buf bytes.Buffer
	if err := cmdDel(env(&buf, s, nil, modNow, time.UTC), 2); err != nil {
		t.Fatalf("del: %v", err)
	}
	dirty, err := s.DirtyEntries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(dirty) != 1 {
		t.Fatalf("dirty entries = %d, want 1 (the pending deletion)", len(dirty))
	}
	got := dirty[0]
	if got.ID != entries[1].ID {
		t.Errorf("dirty entry id = %d, want %d", got.ID, entries[1].ID)
	}
	if !got.Deleted {
		t.Error("deleted flag not set")
	}
	if !got.Dirty {
		t.Error("dirty flag not set")
	}
	if !got.UpdatedAt.Equal(modNow) {
		t.Errorf("updated_at = %v, want %v", got.UpdatedAt, modNow)
	}
	if got.RemoteID == nil || *got.RemoteID != 9002 {
		t.Errorf("remote_id = %v, want 9002 kept for the remote DELETE", got.RemoteID)
	}
}

// TestDelStaleNumber verifies a number that resolves to nothing is an error
// naming `tg ls`, and that nothing is deleted.
func TestDelStaleNumber(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)
	seedModDay(t, s)

	var buf bytes.Buffer
	err := cmdDel(env(&buf, s, nil, modNow, time.UTC), 9)
	if !errors.Is(err, store.ErrNoEntryNum) {
		t.Fatalf("err = %v, want ErrNoEntryNum", err)
	}
	if !strings.Contains(err.Error(), "tg ls") {
		t.Errorf("err = %v, want it to suggest `tg ls`", err)
	}
	if buf.Len() != 0 {
		t.Errorf("output = %q, want nothing written", buf.String())
	}
	if left := mustEntries(t, s, testStart.Add(-24*time.Hour), testStart.Add(24*time.Hour)); len(left) != 2 {
		t.Errorf("entries = %d, want both kept", len(left))
	}

	// Deleting the same entry twice hits the same path: the first delete
	// retires the number, and it is not handed to the surviving entry.
	if err := cmdDel(env(&buf, s, nil, modNow, time.UTC), 1); err != nil {
		t.Fatalf("del: %v", err)
	}
	if err := cmdDel(env(&buf, s, nil, modNow, time.UTC), 1); !errors.Is(err, store.ErrNoEntryNum) {
		t.Errorf("second del error = %v, want ErrNoEntryNum", err)
	}
}

// TestDelRequiresNumber verifies del never guesses a target.
func TestDelRequiresNumber(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)
	seedModDay(t, s)

	var buf bytes.Buffer
	err := cmdDel(env(&buf, s, nil, modNow, time.UTC), 0)
	if err == nil || !strings.Contains(err.Error(), "usage: tg del") {
		t.Fatalf("err = %v, want a usage error", err)
	}
}

// TestDelPushesBestEffort verifies a successful del DELETEs the entry remotely
// and drops the local row.
func TestDelPushesBestEffort(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)
	seedModDay(t, s)

	var method, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := api.New("tok", api.WithBaseURL(srv.URL), api.WithHTTPClient(srv.Client()))

	var buf bytes.Buffer
	if err := cmdDel(env(&buf, s, c, modNow, time.UTC), 1); err != nil {
		t.Fatalf("del: %v", err)
	}
	if method != http.MethodDelete {
		t.Errorf("method = %s %s, want DELETE", method, path)
	}
	if !strings.Contains(path, "9001") {
		t.Errorf("path = %s, want the entry's remote id 9001", path)
	}
	if dirty := mustDirtyEntries(t, s); len(dirty) != 0 {
		t.Errorf("dirty entries = %+v, want the row dropped after the remote delete", dirty)
	}
}

// TestRunDispatchValidatesArgs exercises the run* layer itself: run() dispatches
// the command word, the run* function parses the arguments and only then opens
// anything. Every case here is refused during that argument handling — before
// withEnv touches the state directory or prints a thing — so the test can drive
// the real entry point without a database or a config.
//
// It is what keeps the command words, the argument shapes and the usage messages
// pinned to the code a user actually reaches, rather than to a cmd* call a test
// made up.
func TestRunDispatchValidatesArgs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		cmd     string
		args    []string
		wantErr string
	}{
		{name: "add without a task", cmd: "add", args: []string{"9-10"}, wantErr: addUsage},
		{name: "add with nothing at all", cmd: "add", wantErr: addUsage},
		{name: "del without a number", cmd: "del", wantErr: "usage: tg del"},
		{name: "del with two numbers", cmd: "del", args: []string{"1", "2"}, wantErr: "usage: tg del"},
		{name: "del with a non-number", cmd: "del", args: []string{"x"}, wantErr: "invalid entry number"},
		{name: "del with zero", cmd: "del", args: []string{"0"}, wantErr: "invalid entry number"},
		{name: "mod with two numbers", cmd: "mod", args: []string{"1", "2"}, wantErr: "unexpected second entry number"},
		{name: "mod with two timesigns", cmd: "mod", args: []string{"+:30", "9-10"}, wantErr: "unexpected second timesign"},
		// A negative timesign reaches the argument classifier instead of being
		// reported as an undefined flag, which is what proves the pre-pass runs
		// on the real command line (see splitNegativeTimesigns).
		{name: "mod with two negative timesigns", cmd: "mod", args: []string{"-30", "-1:20"}, wantErr: "unexpected second timesign"},
		{name: "mod with a negative and a positive timesign", cmd: "mod", args: []string{"-30", "+30"}, wantErr: "unexpected second timesign"},
		// Not a duration shape, so it is not a negative timesign: it stays in
		// the arguments and the flag package reports the typo.
		{name: "mod with a negative-looking typo", cmd: "mod", args: []string{"-1:2:3"}, wantErr: "not defined"},
		{
			name: "update naming the project twice", cmd: "update",
			args: []string{"-p", "backend", "payments"}, wantErr: "twice",
		},
		{name: "completion without a shell", cmd: "completion", wantErr: "usage: tg completion zsh"},
		{name: "completion with too many args", cmd: "completion", args: []string{"zsh", "extra"}, wantErr: "usage: tg completion zsh"},
		{name: "completion for another shell", cmd: "completion", args: []string{"bash"}, wantErr: "usage: tg completion zsh"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := run(ctx, tc.cmd, tc.args)
			if err == nil {
				t.Fatalf("run(%q, %q) = nil error, want %q", tc.cmd, tc.args, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("run(%q, %q) error = %v, want it to contain %q", tc.cmd, tc.args, err, tc.wantErr)
			}
		})
	}
}

// --- argument parsing --------------------------------------------------------

// TestParseModArgs covers `tg mod`'s positional disambiguation: bare digits are
// the entry number, everything else is the timesign, in either order.
func TestParseModArgs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		args     []string
		ref      int
		timesign string
		wantErr  bool
	}{
		{name: "empty"},
		{name: "number only", args: []string{"2"}, ref: 2},
		{name: "timesign only", args: []string{"+:30"}, timesign: "+:30"},
		{name: "number and timesign", args: []string{"2", "+:30"}, ref: 2, timesign: "+:30"},
		{name: "reversed", args: []string{"+:30", "2"}, ref: 2, timesign: "+:30"},
		{name: "absolute timesign", args: []string{"3", "9-10:30"}, ref: 3, timesign: "9-10:30"},
		{name: "negative timesign only", args: []string{"-30"}, timesign: "-30"},
		{name: "number and negative timesign", args: []string{"2", "-1:20"}, ref: 2, timesign: "-1:20"},
		{name: "negative timesign reversed", args: []string{"-1:20", "2"}, ref: 2, timesign: "-1:20"},
		{name: "two numbers", args: []string{"2", "3"}, wantErr: true},
		{name: "two timesigns", args: []string{"+:30", "9-10"}, wantErr: true},
		{name: "two signed timesigns", args: []string{"+:30", "-:30"}, wantErr: true},
		{name: "zero is not a number", args: []string{"0"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ref, timesign, err := parseModArgs(tt.args)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseModArgs(%q) = (%d, %q, nil), want an error", tt.args, ref, timesign)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseModArgs(%q): %v", tt.args, err)
			}
			if ref != tt.ref || timesign != tt.timesign {
				t.Errorf("parseModArgs(%q) = (%d, %q), want (%d, %q)",
					tt.args, ref, timesign, tt.ref, tt.timesign)
			}
		})
	}
}

// TestParseArgsAndFlags verifies flags are still recognized after a positional
// argument, which is what makes `tg mod 2 --desc x` work (the flag package on
// its own stops parsing at "2").
func TestParseArgsAndFlags(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		args     []string
		wantPos  []string
		wantDesc string
		wantSet  bool
	}{
		{name: "flag last", args: []string{"2", "--desc", "x"}, wantPos: []string{"2"}, wantDesc: "x", wantSet: true},
		{name: "flag first", args: []string{"--desc", "x", "2"}, wantPos: []string{"2"}, wantDesc: "x", wantSet: true},
		{name: "flag between", args: []string{"2", "--desc", "x", "+:30"}, wantPos: []string{"2", "+:30"}, wantDesc: "x", wantSet: true},
		{name: "alias", args: []string{"2", "--description", "x"}, wantPos: []string{"2"}, wantDesc: "x", wantSet: true},
		{name: "no flag", args: []string{"2", "+:30"}, wantPos: []string{"2", "+:30"}},
		{name: "empty value counts as set", args: []string{"--desc", ""}, wantSet: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fs := newFlagSet("mod")
			fs.SetOutput(io.Discard)
			// The real registration `tg mod` uses, so a rename of either
			// spelling fails here rather than passing against a local copy.
			var desc string
			bindDescFlag(fs, &desc, "new entry description")
			pos, err := parseArgsAndFlags(fs, tt.args)
			if err != nil {
				t.Fatalf("parseArgsAndFlags(%q): %v", tt.args, err)
			}
			if strings.Join(pos, ",") != strings.Join(tt.wantPos, ",") {
				t.Errorf("positional = %q, want %q", pos, tt.wantPos)
			}
			if desc != tt.wantDesc {
				t.Errorf("desc = %q, want %q", desc, tt.wantDesc)
			}
			// descWasSet is what `tg mod` asks, and the distinction it draws
			// (an explicitly empty --desc CLEARS the description) is why an
			// empty value still counts as set.
			if set := descWasSet(fs); set != tt.wantSet {
				t.Errorf("descWasSet = %v, want %v", set, tt.wantSet)
			}
		})
	}
}

// TestSplitNegativeTimesigns covers the pre-pass that lets `tg mod -30` be
// written the obvious way: a negative timesign is a positional that begins with
// "-", so it is peeled off before flag.Parse can call it an undefined flag.
//
// The cases that matter are the ones it must NOT take: the flags themselves, a
// flag VALUE that happens to look like a timesign (`--desc -30`), and typos,
// which stay in the arguments so the flag package still reports them.
func TestSplitNegativeTimesigns(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		args         []string
		wantRest     []string
		wantNegative []string
	}{
		{name: "empty"},
		{name: "bare negative", args: []string{"-30"}, wantNegative: []string{"-30"}},
		{name: "with hours", args: []string{"-1:20"}, wantNegative: []string{"-1:20"}},
		{
			name: "entry number and negative", args: []string{"2", "-1:20"},
			wantRest: []string{"2"}, wantNegative: []string{"-1:20"},
		},
		{
			name: "negative before the number", args: []string{"-10", "2"},
			wantRest: []string{"2"}, wantNegative: []string{"-10"},
		},
		{
			name: "negative and a description", args: []string{"-10", "--desc", "x"},
			wantRest: []string{"--desc", "x"}, wantNegative: []string{"-10"},
		},
		{
			name: "negative and a date", args: []string{"--date", "2026-01-05", "-30"},
			wantRest: []string{"--date", "2026-01-05"}, wantNegative: []string{"-30"},
		},
		{
			name: "negative before the date", args: []string{"-30", "--date", "2026-01-05"},
			wantRest: []string{"--date", "2026-01-05"}, wantNegative: []string{"-30"},
		},
		{
			// The date IS "-30": it is the flag's value (and a bad one, which
			// resolveDateFlag reports), not a timesign.
			name: "negative-looking date value", args: []string{"--date", "-30"},
			wantRest: []string{"--date", "-30"},
		},
		{
			// The description IS "-30": it is the flag's value, not a timesign.
			name: "negative-looking flag value", args: []string{"--desc", "-30"},
			wantRest: []string{"--desc", "-30"},
		},
		{
			name: "negative-looking flag value with the alias", args: []string{"--description", "-30", "2"},
			wantRest: []string{"--description", "-30", "2"},
		},
		{
			name: "single-dash flag value", args: []string{"-desc", "-30"},
			wantRest: []string{"-desc", "-30"},
		},
		{
			// A joined value carries its own, so the next argument is free.
			name: "joined flag value", args: []string{"--desc=x", "-30"},
			wantRest: []string{"--desc=x"}, wantNegative: []string{"-30"},
		},
		{
			name: "positive timesigns are untouched", args: []string{"2", "+30"},
			wantRest: []string{"2", "+30"},
		},
		{
			name: "absolute ranges are untouched", args: []string{"9-10:30"},
			wantRest: []string{"9-10:30"},
		},
		{
			// Left for the flag package to reject as the typo it is.
			name: "unknown flag", args: []string{"-nope"}, wantRest: []string{"-nope"},
		},
		{
			name: "two negatives are both kept", args: []string{"-10", "-20"},
			wantNegative: []string{"-10", "-20"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fs := newFlagSet("mod")
			fs.SetOutput(io.Discard)
			// The real registration `tg mod` uses, so the value-taking flags
			// this has to skip past cannot drift out of sync with it.
			bindModFlags(fs)
			rest, negative := splitNegativeTimesigns(fs, tc.args)
			if strings.Join(rest, ",") != strings.Join(tc.wantRest, ",") {
				t.Errorf("rest = %q, want %q", rest, tc.wantRest)
			}
			if strings.Join(negative, ",") != strings.Join(tc.wantNegative, ",") {
				t.Errorf("negative = %q, want %q", negative, tc.wantNegative)
			}
		})
	}
}

// TestSplitNegativeTimesignsSkipsOnlyValueFlags pins the rule the pre-pass
// applies when it decides which argument is shielded by a flag: only a flag that
// actually consumes the next argument. `tg mod` has no boolean flag today, so
// one is declared here to keep the distinction covered for the day it gains one
// — a negative timesign following a boolean flag is still a timesign.
func TestSplitNegativeTimesignsSkipsOnlyValueFlags(t *testing.T) {
	t.Parallel()
	fs := newFlagSet("mod")
	fs.SetOutput(io.Discard)
	bindModFlags(fs)
	var boolean bool
	fs.BoolVar(&boolean, "flag", false, "a boolean flag, which takes no value")

	rest, negative := splitNegativeTimesigns(fs, []string{"--flag", "-30"})
	if strings.Join(rest, ",") != "--flag" {
		t.Errorf("rest = %q, want just the boolean flag", rest)
	}
	if strings.Join(negative, ",") != "-30" {
		t.Errorf("negative = %q, want the timesign after the boolean flag", negative)
	}
}

// TestFirstFlagParsing pins the wiring of the shared `-1` flag (bindFirstFlag):
// a bare "-1" is parsed as the boolean rather than swallowed as a positional or
// rejected as an unknown flag, `--first` is the same flag, and (through
// parseArgsAndFlags) either spelling may sit before, between or after the
// positionals that make up the fragment. Anything else that looks like a
// number-flag is still an error, so a typo is never taken for a fragment.
func TestFirstFlagParsing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		args      []string
		wantFirst bool
		wantPos   []string
		wantErr   bool
	}{
		{args: nil},
		{args: []string{"write"}, wantPos: []string{"write"}},
		{args: []string{"-1"}, wantFirst: true},
		{args: []string{"-1", "write"}, wantFirst: true, wantPos: []string{"write"}},
		{args: []string{"write", "-1"}, wantFirst: true, wantPos: []string{"write"}},
		{args: []string{"--first", "write"}, wantFirst: true, wantPos: []string{"write"}},
		{args: []string{"write", "--first"}, wantFirst: true, wantPos: []string{"write"}},
		{args: []string{"-1=false", "write"}, wantPos: []string{"write"}},
		// A timesign never starts with "-", so `add`'s positionals survive
		// flag parsing wherever the flag is written.
		{args: []string{"9-10", "-1", "code", "review"}, wantFirst: true, wantPos: []string{"9-10", "code", "review"}},
		{args: []string{"-2", "write"}, wantErr: true},
	} {
		fs := newFlagSet("add")
		fs.SetOutput(io.Discard)
		var first bool
		bindFirstFlag(fs, &first, "task")
		pos, err := parseArgsAndFlags(fs, tc.args)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parse %q: err = nil, want an unknown-flag error", tc.args)
			}
			continue
		}
		if err != nil {
			t.Fatalf("parse %q: %v", tc.args, err)
		}
		if first != tc.wantFirst {
			t.Errorf("parse %q: first = %v, want %v", tc.args, first, tc.wantFirst)
		}
		if strings.Join(pos, ",") != strings.Join(tc.wantPos, ",") {
			t.Errorf("parse %q: positional = %q, want %q", tc.args, pos, tc.wantPos)
		}
	}
}

// TestAddFlagRegistration pins runAdd's flag wiring, through the same
// bindAddFlags the command uses (see TestUpdateFlagRegistration for why the test
// does not declare its own): --desc/--description are aliases sharing one
// variable, -1/--first is the shared ambiguity flag, --date takes the day the
// entry belongs to, and (through parseArgsAndFlags) any of them may sit before,
// between or after the positional timesign and task fragment.
func TestAddFlagRegistration(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		args      []string
		wantPos   []string
		wantDesc  string
		wantFirst bool
		wantDate  string
		wantErr   string
	}{
		{args: nil},
		{args: []string{"9-10", "login"}, wantPos: []string{"9-10", "login"}},
		{
			args:    []string{"9-10", "login", "--date", "2026-01-05"},
			wantPos: []string{"9-10", "login"}, wantDate: "2026-01-05",
		},
		{
			args:    []string{"--date", "2026-01-05", "9-10", "login"},
			wantPos: []string{"9-10", "login"}, wantDate: "2026-01-05",
		},
		{
			args:    []string{"9-10", "--date=2026-01-05", "code", "review"},
			wantPos: []string{"9-10", "code", "review"}, wantDate: "2026-01-05",
		},
		{
			// Everything at once, in the least tidy order a shell allows.
			args:     []string{"9-10", "--date", "2026-01-05", "code", "-1", "review", "--desc", "pairing"},
			wantPos:  []string{"9-10", "code", "review"},
			wantDesc: "pairing", wantFirst: true, wantDate: "2026-01-05",
		},
		{
			args:    []string{"9-10", "login", "--description", "x"},
			wantPos: []string{"9-10", "login"}, wantDesc: "x",
		},
		// A date is not validated here, only collected: resolveDateFlag is what
		// judges it (see TestResolveDateFlag).
		{args: []string{"9-10", "login", "--date", "nope"}, wantPos: []string{"9-10", "login"}, wantDate: "nope"},
		{args: []string{"9-10", "login", "--date"}, wantErr: "needs an argument"},
		{args: []string{"--nope"}, wantErr: "not defined"},
	} {
		t.Run(fmt.Sprint(tc.args), func(t *testing.T) {
			t.Parallel()
			fs := flag.NewFlagSet("add", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			f := bindAddFlags(fs)

			rest, err := parseArgsAndFlags(fs, tc.args)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("parse %v: err = %v, want it to contain %q", tc.args, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse %v: %v", tc.args, err)
			}
			if strings.Join(rest, ",") != strings.Join(tc.wantPos, ",") {
				t.Errorf("positional = %q, want %q", rest, tc.wantPos)
			}
			if f.desc != tc.wantDesc {
				t.Errorf("desc = %q, want %q", f.desc, tc.wantDesc)
			}
			if f.first != tc.wantFirst {
				t.Errorf("first = %v, want %v", f.first, tc.wantFirst)
			}
			if f.date != tc.wantDate {
				t.Errorf("date = %q, want %q", f.date, tc.wantDate)
			}
		})
	}
}

// TestModFlagRegistration pins runMod's whole argument pipeline, in the order
// the command runs it: bindModFlags, then the negative-timesign pre-pass, then
// flag parsing, then parseModArgs. That order is what the interesting cases are
// about — a --date value is never mistaken for a negative timesign, and a
// negative timesign next to the flag is never mistaken for its value — so they
// are checked through the pipeline rather than against its pieces.
func TestModFlagRegistration(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		args         []string
		wantRef      int
		wantTimesign string
		wantDesc     string
		wantDate     string
		wantErr      string
	}{
		{args: nil},
		{args: []string{"2", "+:30"}, wantRef: 2, wantTimesign: "+:30"},
		{
			args:    []string{"--date", "2026-01-05", "2", "+:30"},
			wantRef: 2, wantTimesign: "+:30", wantDate: "2026-01-05",
		},
		{
			args:    []string{"2", "+:30", "--date", "2026-01-05"},
			wantRef: 2, wantTimesign: "+:30", wantDate: "2026-01-05",
		},
		{
			// The date's value is a date; the "-30" after it is the timesign.
			args:         []string{"--date", "2026-01-05", "-30"},
			wantTimesign: "-30", wantDate: "2026-01-05",
		},
		{
			args:         []string{"-30", "--date", "2026-01-05"},
			wantTimesign: "-30", wantDate: "2026-01-05",
		},
		{
			args:    []string{"2", "-1:20", "--date=2026-01-05", "--desc", "x"},
			wantRef: 2, wantTimesign: "-1:20", wantDate: "2026-01-05", wantDesc: "x",
		},
		{
			// Here "-30" IS the date's value (a bad one, refused later), so it
			// is not also a timesign.
			args: []string{"--date", "-30"}, wantDate: "-30",
		},
		{
			// ...and the same holds for the description.
			args:     []string{"--desc", "-30", "--date", "2026-01-05"},
			wantDesc: "-30", wantDate: "2026-01-05",
		},
		{args: []string{"--date", "2026-01-05", "-30", "-20"}, wantErr: "unexpected second timesign"},
		{args: []string{"--nope"}, wantErr: "not defined"},
	} {
		t.Run(fmt.Sprint(tc.args), func(t *testing.T) {
			t.Parallel()
			fs := flag.NewFlagSet("mod", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			f := bindModFlags(fs)

			args, negative := splitNegativeTimesigns(fs, tc.args)
			rest, err := parseArgsAndFlags(fs, args)
			if err == nil {
				var ref int
				var timesign string
				ref, timesign, err = parseModArgs(append(rest, negative...))
				if err == nil {
					if ref != tc.wantRef || timesign != tc.wantTimesign {
						t.Errorf("(ref, timesign) = (%d, %q), want (%d, %q)",
							ref, timesign, tc.wantRef, tc.wantTimesign)
					}
				}
			}
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("parse %v: err = %v, want it to contain %q", tc.args, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse %v: %v", tc.args, err)
			}
			if f.desc != tc.wantDesc {
				t.Errorf("desc = %q, want %q", f.desc, tc.wantDesc)
			}
			if f.date != tc.wantDate {
				t.Errorf("date = %q, want %q", f.date, tc.wantDate)
			}
		})
	}
}

// TestResolveDateFlag tables what `add`/`mod`'s --date resolves to: absent, or
// naming now's own day, it is exactly now, so the flagless behavior is
// untouched; a later day resolves to that day's LAST second, the anchor that
// makes the day's entries count as started; a day that is over is refused,
// since tg never rewrites one; and a value that is not a date in tg's one
// format is refused naming the format.
//
// "Today" is a calendar day in the command's own location, not in UTC and not a
// 24-hour window: at 02:00 UTC it is still yesterday at UTC-5, so that zone's
// date is today's and the UTC one is tomorrow's.
func TestResolveDateFlag(t *testing.T) {
	t.Parallel()
	western := time.FixedZone("UTC-5", -5*60*60)
	// 2026-03-17T02:00Z is 2026-03-16T21:00 at UTC-5.
	earlyUTC := time.Date(2026, 3, 17, 2, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name    string
		flag    string
		now     time.Time
		loc     *time.Location
		want    time.Time
		wantErr []string
	}{
		{name: "absent is now", now: addNow, loc: time.UTC, want: addNow},
		{name: "blank is now", flag: "   ", now: addNow, loc: time.UTC, want: addNow},
		{name: "today is now", flag: "2026-01-02", now: addNow, loc: time.UTC, want: addNow},
		{
			name: "tomorrow is its last second", flag: "2026-01-03", now: addNow, loc: time.UTC,
			want: time.Date(2026, 1, 3, 23, 59, 59, 0, time.UTC),
		},
		{
			name: "a month out", flag: "2026-02-01", now: addNow, loc: time.UTC,
			want: time.Date(2026, 2, 1, 23, 59, 59, 0, time.UTC),
		},
		{
			name: "today is the local day", flag: "2026-03-16", now: earlyUTC, loc: western,
			want: earlyUTC,
		},
		{
			// Already the 17th in UTC, still the future at UTC-5.
			name: "the local day's tomorrow", flag: "2026-03-17", now: earlyUTC, loc: western,
			want: time.Date(2026, 3, 17, 23, 59, 59, 0, western),
		},
		{
			name: "yesterday", flag: "2026-01-01", now: addNow, loc: time.UTC,
			wantErr: []string{"in the past", "2026-01-01", "2026-01-02"},
		},
		{
			name: "long past", flag: "2020-06-01", now: addNow, loc: time.UTC,
			wantErr: []string{"in the past"},
		},
		{
			name: "the local day's yesterday", flag: "2026-03-15", now: earlyUTC, loc: western,
			wantErr: []string{"in the past"},
		},
		{
			name: "day first", flag: "05-01-2026", now: addNow, loc: time.UTC,
			wantErr: []string{"invalid --date", "YYYY-MM-DD"},
		},
		{name: "slashes", flag: "2026/01/05", now: addNow, loc: time.UTC, wantErr: []string{"invalid --date"}},
		{name: "unpadded", flag: "2026-1-5", now: addNow, loc: time.UTC, wantErr: []string{"invalid --date"}},
		{name: "month out of range", flag: "2026-13-01", now: addNow, loc: time.UTC, wantErr: []string{"invalid --date"}},
		{name: "with a time", flag: "2026-01-05 09:00", now: addNow, loc: time.UTC, wantErr: []string{"invalid --date"}},
		{name: "a word", flag: "tomorrow", now: addNow, loc: time.UTC, wantErr: []string{"invalid --date"}},
		{name: "a timesign", flag: "9-10", now: addNow, loc: time.UTC, wantErr: []string{"invalid --date"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := resolveDateFlag(tc.flag, tc.now, tc.loc)
			if len(tc.wantErr) > 0 {
				if err == nil {
					t.Fatalf("resolveDateFlag(%q) = %v, want an error", tc.flag, got)
				}
				for _, want := range tc.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("err = %v, want it to mention %q", err, want)
					}
				}
				if !got.IsZero() {
					t.Errorf("day = %v, want the zero Time alongside the error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveDateFlag(%q): %v", tc.flag, err)
			}
			if !got.Equal(tc.want) {
				t.Errorf("day = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestResolveDateFlagAcrossDST anchors a moved day on the two days that are not
// 24 hours long: the anchor must be the last second of the named day in loc, so
// a 23-hour day's anchor does not spill into the next day (which would file
// entries, and their per-day numbers, under the wrong one) and a 25-hour day's
// anchor still covers its own last hour.
func TestResolveDateFlagAcrossDST(t *testing.T) {
	t.Parallel()
	loc := dstLoc(t)
	now := time.Date(2026, 3, 20, 12, 0, 0, 0, loc)
	for _, tc := range []struct {
		name string
		flag string
		want time.Time
	}{
		{name: "spring forward (23h)", flag: "2026-03-29", want: time.Date(2026, 3, 29, 23, 59, 59, 0, loc)},
		{name: "fall back (25h)", flag: "2026-10-25", want: time.Date(2026, 10, 25, 23, 59, 59, 0, loc)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := resolveDateFlag(tc.flag, now, loc)
			if err != nil {
				t.Fatalf("resolveDateFlag(%q): %v", tc.flag, err)
			}
			if !got.Equal(tc.want) {
				t.Errorf("day = %v, want %v", got, tc.want)
			}
			// The anchor must still fall on the day it names, whichever way
			// the day's length was bent.
			if day := got.In(loc).Format(dateLayout); day != tc.flag {
				t.Errorf("day = %s, want the anchor inside %s", day, tc.flag)
			}
		})
	}
}

// TestTasksCommand tables `tg tasks`: unscoped it lists the whole cached
// catalog with each task's project, and scoped by project id (TOGGL_PROJECT_ID)
// it lists that project's tasks only. The --all/inactive half is covered by
// TestTasksCommandAllIncludesInactive, which needs a catalog of its own.
func TestTasksCommand(t *testing.T) {
	t.Parallel()
	payments := int64(2)
	for _, tc := range []struct {
		name      string
		projectID *int64
		want      []string
		absent    []string
	}{
		{
			name: "unscoped",
			want: []string{"Fix login bug", "Code review", "Payment fix", "[Backend]", "[Payments]"},
		},
		{
			name: "scoped to a project", projectID: &payments,
			want:   []string{"Payment fix"},
			absent: []string{"Fix login bug", "Code review", "Write tests"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newStore(t)
			seedCatalog(t, s)

			var buf bytes.Buffer
			if err := cmdTasks(localEnv(&buf, s), false, tc.projectID, false); err != nil {
				t.Fatalf("tasks: %v", err)
			}
			out := buf.String()
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("tasks output missing %q:\n%s", want, out)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(out, absent) {
					t.Errorf("tasks output should hide %q:\n%s", absent, out)
				}
			}
		})
	}
}

func TestTasksCommandAllIncludesInactive(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	if err := s.ReplaceProjects(ctx, []store.Project{{ID: 1, WorkspaceID: 1, Name: "Backend", Active: true}}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceTasks(ctx, []store.Task{
		{ID: 10, WorkspaceID: 1, ProjectID: 1, Name: "Active task", Active: true},
		{ID: 11, WorkspaceID: 1, ProjectID: 1, Name: "Retired task", Active: false},
	}); err != nil {
		t.Fatal(err)
	}

	var active bytes.Buffer
	if err := cmdTasks(localEnv(&active, s), false, nil, false); err != nil {
		t.Fatalf("tasks: %v", err)
	}
	if strings.Contains(active.String(), "Retired task") {
		t.Errorf("active-only listing should hide inactive tasks:\n%s", active.String())
	}

	var all bytes.Buffer
	if err := cmdTasks(localEnv(&all, s), true, nil, false); err != nil {
		t.Fatalf("tasks --all: %v", err)
	}
	if !strings.Contains(all.String(), "Retired task") {
		t.Errorf("--all listing should include inactive tasks:\n%s", all.String())
	}
}

// TestGrepCommand tables `tg grep`'s listing against the seeded catalog: it
// matches on a case-insensitive substring, all positionals form ONE fragment,
// an exact task name does NOT suppress the other matches (grep exists to show
// every candidate, unlike `add`/`total`), a project scope narrows the
// candidates, `-1` cuts the list down to the first one — the same one `tg add
// -1` would record against — and a fragment matching nothing, or no fragment at
// all, fails without printing anything.
func TestGrepCommand(t *testing.T) {
	t.Parallel()
	payments := int64(2)
	for _, tc := range []struct {
		name      string
		fragment  string
		first     bool
		projectID *int64
		wantLines int      // expected listed lines (a successful case lists >= 1)
		want      []string // substrings the listing must contain
		absent    []string // substrings it must not contain
		wantErr   []string // substrings the error must contain (a failing case)
	}{
		{
			name: "lists every match", fragment: "write", wantLines: 2,
			want:   []string{"Write docs", "Write tests", "[Backend]"},
			absent: []string{"Code review", "Payment fix"},
		},
		{name: "case insensitive", fragment: "CODE REVIEW", wantLines: 1, want: []string{"Code review"}},
		{name: "joined multi-word fragment", fragment: "code review", wantLines: 1, want: []string{"Code review"}},
		{
			// "Fix" is an exact task name in the catalog; "Fix login bug" and
			// "Payment fix" merely contain it, and all three must be listed.
			name: "exact name does not win", fragment: "fix", wantLines: 3,
			want: []string{"Fix login bug", "Payment fix", "[Payments]"},
		},
		{
			name: "project scope", fragment: "fix", projectID: &payments, wantLines: 1,
			want: []string{"Payment fix"}, absent: []string{"Fix login bug"},
		},
		{
			// Catalog order, so "Write docs" is the first candidate.
			name: "first match only", fragment: "write", first: true, wantLines: 1,
			want: []string{"Write docs"}, absent: []string{"Write tests"},
		},
		{
			name: "no match", fragment: "nothing here",
			wantErr: []string{"no task matches", "tg update"},
		},
		{
			// `-1` resolves ambiguity; it never invents a candidate.
			name: "no match with -1", fragment: "nothing here", first: true,
			wantErr: []string{"no task matches"},
		},
		// An empty fragment is a usage error, not "list everything": that is
		// `tg tasks`.
		{name: "empty fragment", fragment: "", wantErr: []string{"usage: tg grep"}},
		{name: "blank fragment", fragment: "   ", wantErr: []string{"usage: tg grep"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newStore(t)
			seedCatalog(t, s)

			var buf bytes.Buffer
			err := cmdGrep(localEnv(&buf, s), false, tc.projectID, tc.first, tc.fragment, false)
			if len(tc.wantErr) > 0 {
				if err == nil {
					t.Fatalf("grep %q = nil error, want %q", tc.fragment, tc.wantErr)
				}
				for _, want := range tc.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("err = %v, want it to mention %q", err, want)
					}
				}
				if buf.Len() != 0 {
					t.Errorf("output = %q, want nothing written when grep fails", buf.String())
				}
				return
			}
			if err != nil {
				t.Fatalf("grep %q: %v", tc.fragment, err)
			}
			out := buf.String()
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("output missing %q:\n%s", want, out)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(out, absent) {
					t.Errorf("output should not list %q:\n%s", absent, out)
				}
			}
			if got := strings.Count(strings.TrimSpace(out), "\n") + 1; got != tc.wantLines {
				t.Errorf("grep listed %d lines, want %d:\n%s", got, tc.wantLines, out)
			}
		})
	}
}

func TestGrepAllIncludesInactive(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	if err := s.ReplaceProjects(ctx, []store.Project{{ID: 1, WorkspaceID: 1, Name: "Backend", Active: true}}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceTasks(ctx, []store.Task{
		{ID: 10, WorkspaceID: 1, ProjectID: 1, Name: "Active task", Active: true},
		{ID: 11, WorkspaceID: 1, ProjectID: 1, Name: "Retired task", Active: false},
	}); err != nil {
		t.Fatal(err)
	}

	var active bytes.Buffer
	if err := cmdGrep(localEnv(&active, s), false, nil, false, "task", false); err != nil {
		t.Fatalf("grep: %v", err)
	}
	if strings.Contains(active.String(), "Retired task") {
		t.Errorf("active-only grep should hide inactive tasks:\n%s", active.String())
	}

	var all bytes.Buffer
	if err := cmdGrep(localEnv(&all, s), true, nil, false, "task", false); err != nil {
		t.Fatalf("grep --all: %v", err)
	}
	if !strings.Contains(all.String(), "Retired task") {
		t.Errorf("--all grep should include inactive tasks:\n%s", all.String())
	}
}

func TestGrepJSON(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)

	var buf bytes.Buffer
	if err := cmdGrep(localEnv(&buf, s), false, nil, false, "write", true); err != nil {
		t.Fatalf("grep --json: %v", err)
	}
	var got []struct {
		ID      int64  `json:"id"`
		Name    string `json:"name"`
		Project string `json:"project"`
		Active  bool   `json:"active"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("json: %v (%s)", err, buf.String())
	}
	if len(got) != 2 {
		t.Fatalf("matches = %d, want 2 (%s)", len(got), buf.String())
	}
	// Catalog order: project then task name, so Write docs precedes Write tests.
	if got[0].Name != "Write docs" || got[0].Project != "Backend" || got[0].ID != 14 || !got[0].Active {
		t.Errorf("matches[0] = %+v, want Write docs / Backend / 14 / active", got[0])
	}
	if got[1].Name != "Write tests" {
		t.Errorf("matches[1] = %+v, want Write tests", got[1])
	}
}

func TestGrepTasksPreservesOrder(t *testing.T) {
	t.Parallel()
	in := []store.Task{
		{ID: 1, Name: "Fix login bug"},
		{ID: 2, Name: "Code review"},
		{ID: 3, Name: "Fix"},
	}
	got := grepTasks(in, "FIX")
	if len(got) != 2 || got[0].ID != 1 || got[1].ID != 3 {
		t.Errorf("grepTasks = %+v, want tasks 1 and 3 in input order", got)
	}
	if grepTasks(in, "  ") != nil {
		t.Error("grepTasks with a blank fragment should match nothing")
	}
}

// TestResolvePullProject tables the project resolution behind `tg pull
// <project>`: the argument is required (pull ignores TOGGL_PROJECT_ID, so there
// is no fallback to fall back to), a unique fragment resolves, an ambiguous one
// fails with the candidates listed and a pointer at `-1`, and `-1` then takes
// the first candidate — but never invents one.
func TestResolvePullProject(t *testing.T) {
	t.Parallel()
	// The ambiguous cases need two projects sharing a prefix; the others use
	// the standard catalog.
	ambiguousCatalog := func(t *testing.T, s *store.Store) {
		t.Helper()
		if err := s.ReplaceProjects(ctx, []store.Project{
			{ID: 1, WorkspaceID: 1, Name: "Backend", Active: true},
			{ID: 2, WorkspaceID: 1, Name: "Back office", Active: true},
		}); err != nil {
			t.Fatal(err)
		}
	}

	for _, tc := range []struct {
		name      string
		ambiguous bool
		fragment  string
		first     bool
		want      *int64
		wantErr   []string
		absentErr []string
	}{
		{name: "unique fragment", fragment: "back", want: ptrInt(1)},
		{
			name: "blank fragment", fragment: "  ",
			wantErr:   []string{"project-name argument"},
			absentErr: []string{"TOGGL_PROJECT_ID"},
		},
		{
			name: "blank fragment with -1", fragment: "  ", first: true,
			wantErr: []string{"project-name argument"},
		},
		{name: "no match", fragment: "nonexistent", wantErr: []string{"tg update"}},
		{name: "no match with -1", fragment: "nonexistent", first: true, wantErr: []string{"tg update"}},
		{
			name: "ambiguous fragment", ambiguous: true, fragment: "back",
			wantErr: []string{"Backend", "Back office", "pass -1"},
		},
		{
			// Candidates are ordered by name then id, so "Back office" (2)
			// wins over "Backend" (1).
			name: "ambiguous fragment with -1", ambiguous: true, fragment: "back", first: true,
			want: ptrInt(2),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newStore(t)
			if tc.ambiguous {
				ambiguousCatalog(t, s)
			} else {
				seedCatalog(t, s)
			}

			got, err := resolvePullProject(ctx, s, tc.fragment, tc.first)
			if len(tc.wantErr) > 0 {
				if err == nil {
					t.Fatalf("resolvePullProject(%q) = %v, want an error", tc.fragment, got)
				}
				for _, want := range tc.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("err = %v, want it to mention %q", err, want)
					}
				}
				for _, absent := range tc.absentErr {
					if strings.Contains(err.Error(), absent) {
						t.Errorf("err = %v, should not mention %q", err, absent)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if got == nil || *got != *tc.want {
				t.Errorf("resolved = %v, want %d", got, *tc.want)
			}
		})
	}
}

// TestResolvePullScope covers the scope `tg pull` derives from its optional
// argument: a blank one means every project (a nil scope), a fragment scopes to
// the project it names. That pull never falls back to TOGGL_PROJECT_ID is
// covered by TestResolvePullScopeIgnoresEnv, which has to set the environment.
func TestResolvePullScope(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		fragment string
		want     *int64
	}{
		{name: "unscoped means all projects", fragment: "   "},
		{name: "fragment scopes to one project", fragment: "pay", want: ptrInt(2)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newStore(t)
			seedCatalog(t, s)

			got, err := resolvePullScope(ctx, s, tc.fragment, false)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			switch {
			case tc.want == nil && got != nil:
				t.Errorf("resolved = %v, want nil (pull all projects)", got)
			case tc.want != nil && (got == nil || *got != *tc.want):
				t.Errorf("resolved = %v, want %d", got, *tc.want)
			}
		})
	}
}

// TestResolvePullScopeIgnoresEnv verifies pull's scope resolution never falls
// back to TOGGL_PROJECT_ID: with the env set but no argument, the scope is nil
// (all projects), unlike the env-honoring resolvers for start/update.
func TestResolvePullScopeIgnoresEnv(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	t.Setenv("TOGGL_PROJECT_ID", "2")
	got, err := resolvePullScope(ctx, s, "", false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != nil {
		t.Errorf("resolved = %v, want nil (pull ignores TOGGL_PROJECT_ID)", got)
	}
}

// --- push --------------------------------------------------------------------

// pushNow is the reference instant the push tests below run at.
var pushNow = time.Date(2026, 1, 2, 18, 0, 0, 0, time.UTC)

// seedDirty inserts a dirty, finished entry with the given description, an hour
// apart per index so the day's entries do not overlap.
func seedDirty(t *testing.T, s *store.Store, i int, desc string) int64 {
	t.Helper()
	start := testStart.Add(time.Duration(i) * time.Hour)
	stop := start.Add(30 * time.Minute)
	id, err := s.CreateEntry(ctx, store.Entry{
		WorkspaceID: 1, TaskID: ptrInt(10), Description: desc,
		Start: start, Stop: &stop, Duration: 1800, UpdatedAt: stop, Dirty: true,
	})
	if err != nil {
		t.Fatalf("seed dirty entry: %v", err)
	}
	return id
}

// TestPushReportsRejectedEntry verifies `tg push` no longer stops at the first
// entry Toggl refuses: the rest of the queue is still sent, the summary counts
// what got through, and the rejection is reported as the command's error (so the
// exit status is non-zero) while the entry stays dirty for a later attempt.
func TestPushReportsRejectedEntry(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)

	var created int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		body = decodeBody(t, r)
		if body["description"] == "poison" {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":"task not in project"}`))
			return
		}
		created++
		fmt.Fprintf(w, `{"id":%d,"at":"2026-01-02T18:00:00Z"}`, 9200+created)
	}))
	defer srv.Close()
	c := api.New("tok", api.WithBaseURL(srv.URL), api.WithHTTPClient(srv.Client()))

	seedDirty(t, s, 0, "first")
	poisoned := seedDirty(t, s, 1, "poison")
	seedDirty(t, s, 2, "last")

	var buf bytes.Buffer
	err := cmdPush(env(&buf, s, c, pushNow, time.UTC), false)
	if err == nil {
		t.Fatal("push = nil error, want the rejection reported")
	}
	if !strings.Contains(err.Error(), "task not in project") {
		t.Errorf("err = %v, want the server's complaint", err)
	}
	// The summary is still printed: two entries did land.
	if !strings.Contains(buf.String(), "Pushed: 2 created") {
		t.Errorf("output = %q, want the successful entries summarized", buf.String())
	}
	if created != 2 {
		t.Errorf("created = %d, want 2 (the rejection must not block the queue)", created)
	}
	dirty := mustDirtyEntries(t, s)
	if len(dirty) != 1 || dirty[0].ID != poisoned {
		t.Fatalf("dirty = %+v, want only the rejected entry %d", dirty, poisoned)
	}
}

// TestPushJSONListsFailures verifies --json reports the rejected entries in the
// result rather than only in the error text.
func TestPushJSONListsFailures(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`nope`))
	}))
	defer srv.Close()
	c := api.New("tok", api.WithBaseURL(srv.URL), api.WithHTTPClient(srv.Client()))

	id := seedDirty(t, s, 0, "first")

	var buf bytes.Buffer
	if err := cmdPush(env(&buf, s, c, pushNow, time.UTC), true); err == nil {
		t.Fatal("push = nil error, want the rejection reported")
	}
	var got struct {
		Created int `json:"created"`
		Failed  []struct {
			EntryID int64  `json:"entry_id"`
			Err     string `json:"error"`
		} `json:"failed"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("json: %v (%s)", err, buf.String())
	}
	if got.Created != 0 || len(got.Failed) != 1 {
		t.Fatalf("json = %+v, want one failure and nothing created", got)
	}
	if got.Failed[0].EntryID != id || !strings.Contains(got.Failed[0].Err, "nope") {
		t.Errorf("failure = %+v, want entry %d and the server's message", got.Failed[0], id)
	}
}

// --- credentials -------------------------------------------------------------

// TestAddWorksUnauthenticated pins the offline behavior `add` now shares with
// `mod`/`del`: with no credentials the entry is still recorded locally (dirty,
// for a later `tg push`) instead of the command refusing to run, and its
// workspace comes from the task's own catalog row since no config names one.
func TestAddWorksUnauthenticated(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)

	var buf bytes.Buffer
	if err := cmdAdd(unauthenticatedEnv(&buf, s, addNow, time.UTC), nil, false, "9-:30", "login", ""); err != nil {
		t.Fatalf("add while unauthenticated: %v", err)
	}
	if !strings.Contains(buf.String(), "Added: Fix login bug") {
		t.Errorf("output = %q, want the entry added", buf.String())
	}
	if strings.Contains(buf.String(), "warning") {
		t.Errorf("output = %q, want no sync warning with no credentials", buf.String())
	}
	entries := addWindow(t, s, addNow)
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if !entries[0].Dirty {
		t.Error("entry should stay dirty for a later push")
	}
	if entries[0].WorkspaceID != 1 {
		t.Errorf("workspace_id = %d, want 1 from the task's catalog row", entries[0].WorkspaceID)
	}
}

// TestAddUnknownWorkspaceIsRefused covers the one case `add` cannot recover
// from: neither a config nor the catalog knows a workspace, so the entry could
// never be pushed and the command says to run `tg auth`.
func TestAddUnknownWorkspaceIsRefused(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	// A catalog row with no workspace, as only a hand-edited database has.
	if err := s.ReplaceProjects(ctx, []store.Project{{ID: 1, Name: "Backend", Active: true}}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceTasks(ctx, []store.Task{{ID: 10, ProjectID: 1, Name: "Fix login bug", Active: true}}); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	err := cmdAdd(unauthenticatedEnv(&buf, s, addNow, time.UTC), nil, false, "9-:30", "login", "")
	if !errors.Is(err, config.ErrNotConfigured) {
		t.Fatalf("err = %v, want config.ErrNotConfigured", err)
	}
	entries := addWindow(t, s, addNow)
	if len(entries) != 0 {
		t.Errorf("entries = %+v, want nothing recorded", entries)
	}
}

// TestSyncCommandsRequireCredentials verifies the commands that cannot do
// anything locally say so uniformly — with the same "run `tg auth`" error a
// missing config always produced — instead of tripping over a nil client.
func TestSyncCommandsRequireCredentials(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)
	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name string
		run  func(env *cmdEnv) error
	}{
		{"push", func(e *cmdEnv) error { return cmdPush(e, false) }},
		{"pull", func(e *cmdEnv) error { return cmdPull(e, false, "", since, false) }},
		{"update", func(e *cmdEnv) error { return cmdUpdate(e, ptrInt(1), false, "", since, false, false) }},
		{"projects update", func(e *cmdEnv) error { return cmdUpdateProjects(e, false, false) }},
		{"total", func(e *cmdEnv) error { return cmdTotal(e, false, nil, since, false) }},
	} {
		var buf bytes.Buffer
		err := tc.run(unauthenticatedEnv(&buf, s, pushNow, time.UTC))
		if !errors.Is(err, config.ErrNotConfigured) {
			t.Errorf("%s: err = %v, want config.ErrNotConfigured", tc.name, err)
		}
		if buf.Len() != 0 {
			t.Errorf("%s: output = %q, want nothing written", tc.name, buf.String())
		}
	}
}

// TestLocalCommandsWorkUnauthenticated verifies the read-only and edit commands
// keep working with no credentials, which is what local-first means here.
func TestLocalCommandsWorkUnauthenticated(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)
	seedDirty(t, s, 0, "work")

	for _, tc := range []struct {
		name string
		run  func(env *cmdEnv) error
	}{
		{"current", func(e *cmdEnv) error { return cmdCurrent(e, false) }},
		{"today", func(e *cmdEnv) error { return cmdToday(e, 1, false, false) }},
		{"daily", func(e *cmdEnv) error { return cmdDaily(e, dailyDefaultTarget, false, false, false) }},
		{"tasks", func(e *cmdEnv) error { return cmdTasks(e, false, nil, false) }},
		{"grep", func(e *cmdEnv) error { return cmdGrep(e, false, nil, false, "login", false) }},
		{"projects", func(e *cmdEnv) error { return cmdProjects(e, false, false) }},
		{"mod", func(e *cmdEnv) error { return cmdMod(e, 1, "", "renamed", true) }},
		{"del", func(e *cmdEnv) error { return cmdDel(e, 1) }},
	} {
		var buf bytes.Buffer
		// The fixture entry sits on testStart's day, so `now` must too for the
		// per-day resolution mod/del use.
		now := testStart.Add(6 * time.Hour)
		if err := tc.run(unauthenticatedEnv(&buf, s, now, time.UTC)); err != nil {
			t.Errorf("%s: err = %v, want it to work offline", tc.name, err)
		}
		if strings.Contains(buf.String(), "warning") {
			t.Errorf("%s: output = %q, want no sync warning", tc.name, buf.String())
		}
	}
}

// pullEntriesJSON is the /me/time_entries payload the pull tests are served: one
// entry in project 1 (Backend) and one in project 2 (Payments), both on
// 2026-01-02.
const pullEntriesJSON = `[
  {"id":1,"workspace_id":1,"project_id":1,"description":"backend",
   "start":"2026-01-02T09:00:00Z","stop":"2026-01-02T09:30:00Z",
   "duration":1800,"at":"2026-01-02T09:30:00Z"},
  {"id":2,"workspace_id":1,"project_id":2,"description":"payments",
   "start":"2026-01-02T10:00:00Z","stop":"2026-01-02T10:30:00Z",
   "duration":1800,"at":"2026-01-02T10:30:00Z"}]`

// TestPullProjectScope tables `tg pull`'s two scopes over the same remote
// payload: with no project argument it reconciles every project in one pass and,
// having covered everything, advances the last_pull watermark; with a project
// fragment it reconciles only that project's entries and, being a partial pull,
// leaves the watermark alone so a later full pull still sees the rest. (That the
// unscoped form also ignores TOGGL_PROJECT_ID is TestPullIgnoresProjectEnv,
// which has to set the environment and so cannot run in parallel.)
func TestPullProjectScope(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		fragment      string
		wantOutput    string
		wantRemoteIDs []int64 // entries that must have landed
		goneRemoteIDs []int64 // entries that must have been ignored
		wantWatermark bool
	}{
		{
			name: "unscoped pulls every project", wantOutput: "2 inserted",
			wantRemoteIDs: []int64{1, 2}, wantWatermark: true,
		},
		{
			// "back" resolves to Backend (project 1).
			name: "fragment scopes to one project", fragment: "back", wantOutput: "1 inserted",
			wantRemoteIDs: []int64{1}, goneRemoteIDs: []int64{2},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newStore(t)
			seedCatalog(t, s)

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(pullEntriesJSON))
			}))
			defer srv.Close()
			c := api.New("tok", api.WithBaseURL(srv.URL), api.WithHTTPClient(srv.Client()))

			since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
			var buf bytes.Buffer
			if err := cmdPull(env(&buf, s, c, now, time.UTC), false, tc.fragment, since, false); err != nil {
				t.Fatalf("pull: %v", err)
			}
			if !strings.Contains(buf.String(), tc.wantOutput) {
				t.Errorf("output = %q, want %q", buf.String(), tc.wantOutput)
			}
			for _, id := range tc.wantRemoteIDs {
				if got := mustEntryByRemoteID(t, s, id); got == nil {
					t.Errorf("entry %d should have been inserted", id)
				}
			}
			for _, id := range tc.goneRemoteIDs {
				if got := mustEntryByRemoteID(t, s, id); got != nil {
					t.Errorf("entry %d = %+v, want it ignored by a scoped pull", id, got)
				}
			}
			if _, ok := mustMeta(t, s, store.MetaLastPull); ok != tc.wantWatermark {
				t.Errorf("last_pull set = %v, want %v", ok, tc.wantWatermark)
			}
		})
	}
}

// TestPullIgnoresProjectEnv verifies `tg pull` reconciles entries from EVERY
// project even when TOGGL_PROJECT_ID is set. Unlike start/tasks/update, pull
// deliberately ignores the env project and spans the whole workspace, so an
// entry belonging to a project other than the env one is still pulled and the
// last_pull watermark advances (a full pull).
func TestPullIgnoresProjectEnv(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	// Scope the env to a single project; pull must NOT honor it.
	t.Setenv("TOGGL_PROJECT_ID", "1")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(pullEntriesJSON))
	}))
	defer srv.Close()
	c := api.New("tok", api.WithBaseURL(srv.URL), api.WithHTTPClient(srv.Client()))

	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	var buf bytes.Buffer
	if err := cmdPull(env(&buf, s, c, now, time.UTC), false, "", since, false); err != nil {
		t.Fatalf("pull: %v", err)
	}
	if !strings.Contains(buf.String(), "2 inserted") {
		t.Errorf("output = %q, want 2 inserted (all projects)", buf.String())
	}
	// The env project's entry is pulled...
	if got := mustEntryByRemoteID(t, s, 1); got == nil {
		t.Error("project 1 entry should be inserted")
	}
	// ...and so is the entry for a project other than TOGGL_PROJECT_ID.
	if got := mustEntryByRemoteID(t, s, 2); got == nil {
		t.Error("project 2 entry should be inserted despite TOGGL_PROJECT_ID=1")
	}
	// Ignoring the env means this is a full pull: the watermark advances.
	if _, ok := mustMeta(t, s, store.MetaLastPull); !ok {
		t.Error("pull ignoring env should be a full pull and advance last_pull")
	}
}

// totalReportsServer returns a client whose Reports API points at a test server
// serving a fixed summary payload, and records the request body seen.
//
// The payload deliberately mirrors what the real endpoint sends: task
// sub-groups carry ids and seconds but NO titles (task 10 4500s, 12 2700s, 13
// 3600s, 14 900s, all in seedCatalog), so anything that matched on reported
// titles would match nothing. Two extra rows are not in the local catalog: 98
// (with a title, as older responses carried) and 99 (untitled).
func totalReportsServer(t *testing.T) (*api.Client, *map[string]any) {
	t.Helper()
	body := map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/workspace/1/summary/time_entries" {
			t.Errorf("path = %q, want /workspace/1/summary/time_entries", r.URL.Path)
		}
		body = decodeBody(t, r)
		w.Write([]byte(`{"groups":[
		  {"id":1,"sub_groups":[
		    {"id":10,"seconds":4500},
		    {"id":12,"seconds":2700},
		    {"id":13,"seconds":3600},
		    {"id":14,"seconds":900},
		    {"id":98,"title":"Legacy work","seconds":600},
		    {"id":99,"seconds":1200},
		    {"id":null,"seconds":300}]}]}`))
	}))
	t.Cleanup(srv.Close)
	c := api.New("tok", api.WithReportsBaseURL(srv.URL), api.WithHTTPClient(srv.Client()))
	return c, &body
}

var totalNow = time.Date(2026, 1, 2, 15, 0, 0, 0, time.UTC)

// totalSince is the default `tg total` window start: three calendar months
// before totalNow (see resolveTotalSince), i.e. 2025-10-02.
var totalSince = totalNow.AddDate(0, -3, 0)

// TestTotalCommand tables `tg total`'s matching. The regression it guards is
// that the fragment is matched against the LOCAL catalog and the summary rows
// are joined to it by task id: the report's task sub-groups carry no titles, so
// anything matching on reported titles would match nothing at all. Beyond that,
// EACH positional is its own fragment (reported as its own group, with the
// overall total counting a task matched twice only once), an exact task name
// wins over the longer names it is part of, `-1` narrows every fragment to its
// first candidate, no fragment at all lists everything with tracked time
// (including rows the local catalog does not know), and a fragment matching no
// catalogued task — or one with no tracked time — fails without printing
// anything.
func TestTotalCommand(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		fragments []string
		first     bool
		want      []string // substrings the report must contain
		absent    []string // substrings it must not contain
		wantErr   string
	}{
		{
			name: "unique fragment joined by id", fragments: []string{"login"},
			want:   []string{"Fix login bug", "1h15m", "[Backend]", "Total: 1h15m"},
			absent: []string{"Code review", "Write tests", "Write docs", "task #99"},
		},
		{
			// A quoted multi-word argument is still ONE search, so this finds
			// "Write docs" and not everything matching "write" or "docs".
			name: "quoted multi-word fragment", fragments: []string{"write docs"},
			want:   []string{"Write docs", "Total: 0h15m"},
			absent: []string{"Write tests"},
		},
		{
			// One fragment may match several tasks (the store's substring
			// semantics); all of them are listed and summed.
			name: "fragment matching many", fragments: []string{"write"},
			want: []string{"Write tests", "1h00m", "Write docs", "0h15m", "Total: 1h15m"},
		},
		{
			// Two fragments are two searches: each gets a header line with its
			// own total, and the footer sums both.
			name: "two fragments grouped", fragments: []string{"login", "review"},
			want: []string{
				"login  1h15m", "  Fix login bug", "1h15m",
				"review  0h45m", "  Code review",
				"Total: 2h00m",
			},
			absent: []string{"Write docs", "Write tests"},
		},
		{
			// Overlapping fragments list the shared task under both headers,
			// but the footer counts its 15m once: 1h00m (Write tests) + 0h15m.
			name: "overlapping fragments count a task once", fragments: []string{"write", "docs"},
			want:   []string{"write  1h15m", "docs  0h15m", "Total: 1h15m"},
			absent: []string{"Total: 1h30m"},
		},
		{
			// Candidates are ordered by name, so "Write docs" is the first —
			// the same one `tg add -1` would record against.
			name: "first match only", fragments: []string{"write"}, first: true,
			want:   []string{"Write docs", "0h15m", "Total: 0h15m"},
			absent: []string{"Write tests"},
		},
		{
			// -1 applies to every fragment, not just the first one.
			name: "first match applies per fragment", fragments: []string{"write", "fix login"}, first: true,
			want:   []string{"write  0h15m", "  Write docs", "fix login  1h15m", "Total: 1h30m"},
			absent: []string{"Write tests"},
		},
		{
			name: "no fragment lists all",
			want: []string{
				"Fix login bug", "Code review", "Write tests", "Write docs",
				"Legacy work", // uncatalogued, but titled by the API
				"task #99",    // uncatalogued and untitled
				"Total: 3h45m",
			},
		},
		{
			// A blank positional is no search at all, so this is the
			// unfiltered listing rather than a match-everything fragment.
			name: "blank fragment ignored", fragments: []string{"  "},
			want: []string{"Fix login bug", "task #99", "Total: 3h45m"},
		},
		{
			// Task 11 ("Fix") is an exact name and has no tracked time in the
			// report, so the exact match winning is what makes this an empty
			// result rather than "Fix login bug".
			name: "exact name wins", fragments: []string{"fix"}, wantErr: "no tracked time",
		},
		{
			// Task 20 (Payment fix) exists locally; the report never mentions
			// it, which is reported as such rather than as a bogus no-match.
			name: "matched but untracked", fragments: []string{"payment"}, wantErr: "no tracked time",
		},
		{
			// One bad fragment fails the whole report rather than being
			// dropped from it, so a typo is never mistaken for "no time".
			name: "one bad fragment fails all", fragments: []string{"login", "nonexistent"},
			wantErr: `no task matches "nonexistent"`,
		},
		{
			name: "one untracked fragment fails all", fragments: []string{"login", "payment"},
			wantErr: `no tracked time for "payment"`,
		},
		{
			// A row only the API named cannot be reached by a fragment:
			// matching is catalog-only.
			name: "api-only title not matchable", fragments: []string{"legacy"}, wantErr: "no task matches",
		},
		{name: "no match", fragments: []string{"nonexistent"}, wantErr: "no task matches"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newStore(t)
			seedCatalog(t, s)
			c, _ := totalReportsServer(t)

			var buf bytes.Buffer
			err := cmdTotal(env(&buf, s, c, totalNow, time.UTC), tc.first, tc.fragments, totalSince, false)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
				}
				if buf.Len() != 0 {
					t.Errorf("output = %q, want nothing written when total fails", buf.String())
				}
				return
			}
			if err != nil {
				t.Fatalf("total %v: %v", tc.fragments, err)
			}
			out := buf.String()
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("output missing %q:\n%s", want, out)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(out, absent) {
					t.Errorf("output should not report %q:\n%s", absent, out)
				}
			}
		})
	}
}

// TestTotalWindow pins the range `tg total` asks the Reports API for: the
// default start is three calendar months before now, --since overrides it, and
// the end date is today either way.
func TestTotalWindow(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		since     time.Time
		wantStart string
	}{
		{name: "default three months", since: totalSince, wantStart: "2025-10-02"},
		{name: "explicit since", since: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), wantStart: "2025-01-01"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newStore(t)
			seedCatalog(t, s)
			c, body := totalReportsServer(t)

			var buf bytes.Buffer
			if err := cmdTotal(env(&buf, s, c, totalNow, time.UTC), false, []string{"login"}, tc.since, false); err != nil {
				t.Fatalf("total: %v", err)
			}
			if got := (*body)["start_date"]; got != tc.wantStart {
				t.Errorf("start_date = %v, want %s", got, tc.wantStart)
			}
			if got := (*body)["end_date"]; got != "2026-01-02" {
				t.Errorf("end_date = %v, want 2026-01-02 (today)", got)
			}
		})
	}
}

// totalJSONOut is the shape `tg total --json` is read back as: the distinct
// tasks and overall total, plus the per-fragment breakdown that only appears
// once several fragments were given.
type totalJSONOut struct {
	Fragments []struct {
		Fragment     string             `json:"fragment"`
		Tasks        []totalTaskJSONOut `json:"tasks"`
		TotalSeconds int64              `json:"total_seconds"`
	} `json:"fragments"`
	Tasks        []totalTaskJSONOut `json:"tasks"`
	TotalSeconds int64              `json:"total_seconds"`
}

type totalTaskJSONOut struct {
	Task            string `json:"task"`
	Project         string `json:"project"`
	DurationSeconds int64  `json:"duration_seconds"`
}

func TestTotalJSON(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)
	c, _ := totalReportsServer(t)

	var buf bytes.Buffer
	if err := cmdTotal(env(&buf, s, c, totalNow, time.UTC), false, []string{"write"}, totalSince, true); err != nil {
		t.Fatalf("total --json: %v", err)
	}
	var got totalJSONOut
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("json: %v (%s)", err, buf.String())
	}
	if len(got.Tasks) != 2 {
		t.Fatalf("tasks = %d, want 2", len(got.Tasks))
	}
	// Sorted by name: Write docs (900s) then Write tests (3600s), both named
	// and project-tagged from the local catalog.
	if got.Tasks[0].Task != "Write docs" || got.Tasks[0].Project != "Backend" || got.Tasks[0].DurationSeconds != 900 {
		t.Errorf("tasks[0] = %+v, want Write docs / Backend / 900s", got.Tasks[0])
	}
	if got.Tasks[1].Task != "Write tests" {
		t.Errorf("tasks[1] = %+v, want Write tests", got.Tasks[1])
	}
	if got.TotalSeconds != 4500 {
		t.Errorf("total_seconds = %d, want 4500", got.TotalSeconds)
	}
	// One fragment needs no breakdown, so the shape is exactly what it has
	// always been.
	if got.Fragments != nil {
		t.Errorf("fragments = %+v, want none for a single fragment", got.Fragments)
	}
}

// TestTotalMultipleFragmentsJSON pins the multi-fragment JSON: one entry per
// fragment with its own tasks and total, alongside the distinct task list whose
// total counts the overlapping task ("Write docs" matches both fragments) once.
func TestTotalMultipleFragmentsJSON(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)
	c, _ := totalReportsServer(t)

	var buf bytes.Buffer
	err := cmdTotal(env(&buf, s, c, totalNow, time.UTC), false, []string{"write", "docs"}, totalSince, true)
	if err != nil {
		t.Fatalf("total --json: %v", err)
	}
	var got totalJSONOut
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("json: %v (%s)", err, buf.String())
	}
	if len(got.Fragments) != 2 {
		t.Fatalf("fragments = %+v, want one per fragment", got.Fragments)
	}
	if got.Fragments[0].Fragment != "write" || got.Fragments[0].TotalSeconds != 4500 {
		t.Errorf("fragments[0] = %+v, want write / 4500s", got.Fragments[0])
	}
	if len(got.Fragments[0].Tasks) != 2 {
		t.Errorf("fragments[0].tasks = %+v, want Write docs and Write tests", got.Fragments[0].Tasks)
	}
	if got.Fragments[1].Fragment != "docs" || got.Fragments[1].TotalSeconds != 900 {
		t.Errorf("fragments[1] = %+v, want docs / 900s", got.Fragments[1])
	}
	// The distinct list holds Write docs once, so the overall total is the
	// tracked time (4500s), not the 5400s the fragments add up to.
	if len(got.Tasks) != 2 || got.TotalSeconds != 4500 {
		t.Errorf("tasks = %+v, total = %d; want 2 distinct tasks and 4500s", got.Tasks, got.TotalSeconds)
	}
}

// TestResolveTotalSince pins `tg total`'s window start: the default is three
// calendar months before now, an explicit --since date is parsed as midnight in
// the given location, and a malformed one is rejected in the shared
// "invalid --since" style.
func TestResolveTotalSince(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		flag    string
		want    time.Time
		wantErr string
	}{
		{name: "default is three months back", want: totalNow.AddDate(0, -3, 0)},
		{name: "explicit date", flag: "2025-01-01", want: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)},
		{name: "malformed date", flag: "not-a-date", wantErr: "invalid --since"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := resolveTotalSince(tc.flag, totalNow, time.UTC)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveTotalSince(%q): %v", tc.flag, err)
			}
			if !got.Equal(tc.want) {
				t.Errorf("since = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestTotalFlagRegistration pins runTotal's own flag wiring: the flags come
// from bindTotalFlags, the very function the command uses, so renaming or
// dropping one fails here. The point of the table is the positionals: every one
// of them is handed to cmdTotal as its OWN fragment (`tg total login docs` is
// two searches, not the joined "login docs" `tg add` would make of it), a
// multi-word fragment is therefore a single quoted argument, and the flags may
// still sit before, between or after them.
func TestTotalFlagRegistration(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		args      []string
		wantFrags []string
		wantSince string
		wantJSON  bool
		wantFirst bool
		wantErr   string
	}{
		{args: nil},
		{args: []string{"login"}, wantFrags: []string{"login"}},
		{args: []string{"login", "docs"}, wantFrags: []string{"login", "docs"}},
		{args: []string{"code review"}, wantFrags: []string{"code review"}},
		{args: []string{"login", "--json"}, wantFrags: []string{"login"}, wantJSON: true},
		{args: []string{"--json", "login", "docs"}, wantFrags: []string{"login", "docs"}, wantJSON: true},
		{args: []string{"login", "-1", "docs"}, wantFrags: []string{"login", "docs"}, wantFirst: true},
		{args: []string{"--first", "login"}, wantFrags: []string{"login"}, wantFirst: true},
		{
			args:      []string{"--since", "2025-01-01", "login", "docs", "--json"},
			wantFrags: []string{"login", "docs"}, wantSince: "2025-01-01", wantJSON: true,
		},
		{args: []string{"--since=2025-01-01", "login"}, wantFrags: []string{"login"}, wantSince: "2025-01-01"},
		{args: []string{"--nope"}, wantErr: "not defined"},
	} {
		t.Run(fmt.Sprint(tc.args), func(t *testing.T) {
			t.Parallel()
			fs := flag.NewFlagSet("total", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			f := bindTotalFlags(fs)

			rest, err := parseArgsAndFlags(fs, tc.args)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("parse %v: err = %v, want it to contain %q", tc.args, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse %v: %v", tc.args, err)
			}
			if strings.Join(rest, "|") != strings.Join(tc.wantFrags, "|") {
				t.Errorf("fragments = %q, want %q", rest, tc.wantFrags)
			}
			if f.since != tc.wantSince {
				t.Errorf("since = %q, want %q", f.since, tc.wantSince)
			}
			if f.jsonOut != tc.wantJSON || f.first != tc.wantFirst {
				t.Errorf("json/first = %v/%v, want %v/%v",
					f.jsonOut, f.first, tc.wantJSON, tc.wantFirst)
			}
		})
	}
}

// TestResolveAddProject covers the project-name form of `tg add`
// (`tg add <timesign> <project> <task>`): the fragment is resolved against the
// cached catalog, and a fragment matching nothing points at `tg update`.
func TestResolveAddProject(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		fragment string
		want     *int64
		wantErr  string
	}{
		{name: "unique fragment", fragment: "pay", want: ptrInt(2)},
		{name: "no match", fragment: "nonexistent", wantErr: "tg update"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newStore(t)
			seedCatalog(t, s)

			got, err := resolveAddProject(ctx, s, tc.fragment, false)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if got == nil || *got != *tc.want {
				t.Errorf("resolved = %v, want %d", got, *tc.want)
			}
		})
	}
}

// TestResolveUpdateProject covers `tg update`'s two ways of naming a project:
// TOGGL_PROJECT_ID wins outright when set (the fragment is not even consulted),
// and with neither an id nor a fragment the command says which of the two is
// missing.
func TestResolveUpdateProject(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		projectID *int64
		fragment  string
		want      *int64
		wantErr   string
	}{
		{name: "env id wins over the fragment", projectID: ptrInt(2), fragment: "backend", want: ptrInt(2)},
		{name: "fragment resolves", fragment: "backend", want: ptrInt(1)},
		{name: "neither is given", fragment: "  ", wantErr: "TOGGL_PROJECT_ID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newStore(t)
			seedCatalog(t, s)

			got, err := resolveUpdateProject(ctx, s, tc.projectID, tc.fragment, false)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if got == nil || *got != *tc.want {
				t.Errorf("resolved = %v, want %d", got, *tc.want)
			}
		})
	}
}

// updateSince/updateNow are the entry-pull window used by the update tests.
var (
	updateSince = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	updateNow   = time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
)

// updateEntriesJSON is the /me/time_entries payload served to the update tests:
// one entry in project 1 (Backend) and one in project 2 (Payments).
const updateEntriesJSON = `[
  {"id":1,"workspace_id":1,"project_id":1,"description":"backend",
   "start":"2026-01-02T09:00:00Z","stop":"2026-01-02T09:30:00Z",
   "duration":1800,"at":"2026-01-02T09:30:00Z"},
  {"id":2,"workspace_id":1,"project_id":2,"description":"payments",
   "start":"2026-01-02T10:00:00Z","stop":"2026-01-02T10:30:00Z",
   "duration":1800,"at":"2026-01-02T10:30:00Z"}]`

// TestUpdateScopedToOneProject verifies update fetches only the selected
// project's tasks (never the whole workspace, and never the project catalog
// itself) and upserts them without wiping other projects' cached tasks.
func TestUpdateScopedToOneProject(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)

	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/workspaces/1/projects/2/tasks":
			w.Write([]byte(`[{"id":21,"workspace_id":1,"project_id":2,"name":"New payment task","active":true}]`))
		case "/me/time_entries":
			w.Write([]byte(`[]`))
		default:
			t.Errorf("unexpected path %q (update must not sync projects)", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := api.New("tok", api.WithBaseURL(srv.URL), api.WithHTTPClient(srv.Client()))

	pid := int64(2)
	var buf bytes.Buffer
	if err := cmdUpdate(env(&buf, s, c, updateNow, time.UTC), &pid, false, "", updateSince, false, false); err != nil {
		t.Fatalf("update: %v", err)
	}
	// update is quiet: no progress or summary lines in human mode.
	if buf.Len() != 0 {
		t.Errorf("output = %q, want no output", buf.String())
	}

	// Only the scoped task fetch and the entry pull were hit: no project
	// endpoint at all, since update no longer syncs projects.
	for _, p := range paths {
		switch p {
		case "/workspaces/1/projects/2/tasks", "/me/time_entries":
		default:
			t.Errorf("unexpected request path %q", p)
		}
	}

	// Project 2's tasks were replaced with the fetched one...
	p2 := int64(2)
	scoped := mustListTasks(t, s, false, &p2)
	if len(scoped) != 1 || scoped[0].ID != 21 {
		t.Errorf("project 2 tasks = %+v, want only id 21", scoped)
	}
	// ...while project 1's cached tasks are untouched.
	p1 := int64(1)
	backend := mustListTasks(t, s, false, &p1)
	if len(backend) == 0 {
		t.Error("project 1 tasks should be untouched by a project-2 update")
	}
}

// TestUpdatePullsRecentEntries verifies update also reconciles time entries: it
// asks Toggl for everything modified since the window start and, being scoped
// to one project, keeps other projects' entries out and the last_pull watermark
// untouched so a later full `tg pull` still sees them.
func TestUpdatePullsRecentEntries(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)

	var gotSince string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/workspaces/1/projects/2/tasks":
			w.Write([]byte(`[]`))
		case "/me/time_entries":
			gotSince = r.URL.Query().Get("since")
			w.Write([]byte(updateEntriesJSON))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := api.New("tok", api.WithBaseURL(srv.URL), api.WithHTTPClient(srv.Client()))

	pid := int64(2)
	var buf bytes.Buffer
	if err := cmdUpdate(env(&buf, s, c, updateNow, time.UTC), &pid, false, "", updateSince, false, false); err != nil {
		t.Fatalf("update: %v", err)
	}

	// The window start is passed through to the API as a unix timestamp.
	if want := "1767225600"; gotSince != want { // 2026-01-01T00:00:00Z
		t.Errorf("since = %q, want %q", gotSince, want)
	}
	// Only the scoped project's entry landed locally.
	if got := mustEntryByRemoteID(t, s, 2); got == nil {
		t.Error("payments entry should be inserted by a project-2 update")
	}
	if got := mustEntryByRemoteID(t, s, 1); got != nil {
		t.Error("backend entry should be ignored by a project-2 update")
	}
	// A scoped pull is partial: the watermark must stay untouched.
	if _, ok := mustMeta(t, s, store.MetaLastPull); ok {
		t.Error("update should not advance last_pull (it is a scoped pull)")
	}
}

// TestUpdateJSONStillReports pins that making `tg update` quiet only silenced
// human mode: --json keeps emitting the machine-readable summary, now including
// the entry-pull counts.
func TestUpdateJSONStillReports(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/me/time_entries":
			w.Write([]byte(updateEntriesJSON))
		default:
			w.Write([]byte(`[{"id":21,"workspace_id":1,"project_id":2,"name":"New payment task","active":true}]`))
		}
	}))
	defer srv.Close()
	c := api.New("tok", api.WithBaseURL(srv.URL), api.WithHTTPClient(srv.Client()))

	pid := int64(2)
	var buf bytes.Buffer
	if err := cmdUpdate(env(&buf, s, c, updateNow, time.UTC), &pid, false, "", updateSince, false, true); err != nil {
		t.Fatalf("update --json: %v", err)
	}
	out := buf.String()
	// The project name comes from the local catalog, not from a fetch.
	for _, want := range []string{`"project":"Payments"`, `"tasks":1`, `"inserted":1`} {
		if !strings.Contains(out, want) {
			t.Errorf("json output = %q, want %s", out, want)
		}
	}
}

// TestResolveUpdateSince pins `tg update`'s entry window: --days/-n walks the
// start back a whole number of calendar days from midnight (rather than making a
// rolling 24h cut), the default is one day back, 0 means today only and a
// negative count is clamped to today instead of erroring.
func TestResolveUpdateSince(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 2, 15, 30, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		days int
		want time.Time
	}{
		{name: "default", days: updateDefaultDays, want: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		{name: "today only", days: 0, want: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)},
		{name: "three days back", days: 3, want: time.Date(2025, 12, 30, 0, 0, 0, 0, time.UTC)},
		{name: "negative is clamped", days: -5, want: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := resolveUpdateSince(tc.days, now, time.UTC); !got.Equal(tc.want) {
				t.Errorf("resolveUpdateSince(%d) = %v, want %v", tc.days, got, tc.want)
			}
		})
	}
}

// TestUpdateFlagRegistration pins runUpdate's own flag wiring: the flags come
// from bindUpdateFlags, the very function the command uses, so renaming or
// dropping one fails here — which a test declaring its own FlagSet could not
// do. --days/-n and --project/-p are aliases sharing one variable and one
// default, -1/--first is the shared ambiguity flag, and (through
// parseArgsAndFlags) any of them may sit before, between or after the positional
// project fragment.
func TestUpdateFlagRegistration(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		args      []string
		wantFrag  string
		wantDays  int
		wantAll   bool
		wantJSON  bool
		wantFirst bool
		wantErr   string
	}{
		{args: nil, wantDays: updateDefaultDays},
		{args: []string{"backend"}, wantFrag: "backend", wantDays: updateDefaultDays},
		{args: []string{"backend", "-n", "3"}, wantFrag: "backend", wantDays: 3},
		{args: []string{"--days", "7", "backend"}, wantFrag: "backend", wantDays: 7},
		{args: []string{"backend", "--days=2"}, wantFrag: "backend", wantDays: 2},
		// The flag form of the project reaches the same fragment as the
		// positional one.
		{args: []string{"-p", "backend"}, wantFrag: "backend", wantDays: updateDefaultDays},
		{args: []string{"--project", "backend"}, wantFrag: "backend", wantDays: updateDefaultDays},
		{args: []string{"--project=backend"}, wantFrag: "backend", wantDays: updateDefaultDays},
		{args: []string{"-p", "backend", "-n", "3"}, wantFrag: "backend", wantDays: 3},
		{args: []string{"-n", "3", "-p", "backend"}, wantFrag: "backend", wantDays: 3},
		// Positionals are joined, so a multi-word project name works unquoted.
		{args: []string{"code", "review"}, wantFrag: "code review", wantDays: updateDefaultDays},
		{
			args: []string{"backend", "--all", "--json", "-1"}, wantFrag: "backend",
			wantDays: updateDefaultDays, wantAll: true, wantJSON: true, wantFirst: true,
		},
		{args: []string{"--first", "backend"}, wantFrag: "backend", wantDays: updateDefaultDays, wantFirst: true},
		// Naming the project twice is a usage error, not a silent precedence
		// rule (see updateProjectFragment).
		{args: []string{"-p", "backend", "payments"}, wantErr: "twice"},
		{args: []string{"--nope"}, wantErr: "not defined"},
	} {
		t.Run(fmt.Sprint(tc.args), func(t *testing.T) {
			t.Parallel()
			fs := flag.NewFlagSet("update", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			f := bindUpdateFlags(fs)

			frag := ""
			rest, err := parseArgsAndFlags(fs, tc.args)
			if err == nil {
				frag, err = f.resolveFragment(rest)
			}
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("parse %v: err = %v, want it to contain %q", tc.args, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse %v: %v", tc.args, err)
			}
			if frag != tc.wantFrag {
				t.Errorf("fragment = %q, want %q", frag, tc.wantFrag)
			}
			if f.days != tc.wantDays {
				t.Errorf("days = %d, want %d", f.days, tc.wantDays)
			}
			if f.all != tc.wantAll || f.jsonOut != tc.wantJSON || f.first != tc.wantFirst {
				t.Errorf("all/json/first = %v/%v/%v, want %v/%v/%v",
					f.all, f.jsonOut, f.first, tc.wantAll, tc.wantJSON, tc.wantFirst)
			}
		})
	}
}

// TestUpdateProjectFragmentSources pins how the two equivalent ways of naming
// `tg update`'s project fold into one fragment: the --project/-p flag, the
// positionals (joined so a multi-word name works unquoted), neither (left empty
// for resolveUpdateProject to reject or for TOGGL_PROJECT_ID to cover), and
// both at once, which is a usage error rather than a silent precedence rule.
func TestUpdateProjectFragmentSources(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		flagValue  string
		positional []string
		want       string
		wantErr    bool
	}{
		{"positional", "", []string{"backend"}, "backend", false},
		{"positional multiword", "", []string{"code", "review"}, "code review", false},
		{"flag", "backend", nil, "backend", false},
		{"flag multiword", "code review", nil, "code review", false},
		{"neither", "", nil, "", false},
		{"blank both", "  ", []string{" "}, "", false},
		{"both", "backend", []string{"payments"}, "", true},
	} {
		got, err := updateProjectFragment(tc.flagValue, tc.positional)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: err = nil, want a project-given-twice error", tc.name)
			} else if !strings.Contains(err.Error(), "twice") {
				t.Errorf("%s: err = %v, want it to say the project was given twice", tc.name, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: fragment = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestUpdateResolvesProjectByFragment verifies `tg update` picks its project by
// a case-insensitive name fragment against the cached catalog (no env id, no
// exact name): "pay" reaches Payments (id 2), so the tasks fetched and the
// entries kept are that project's.
func TestUpdateResolvesProjectByFragment(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)

	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/workspaces/1/projects/2/tasks":
			w.Write([]byte(`[{"id":21,"workspace_id":1,"project_id":2,"name":"New payment task","active":true}]`))
		case "/me/time_entries":
			w.Write([]byte(updateEntriesJSON))
		default:
			t.Errorf("unexpected path %q (fragment should resolve to project 2)", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := api.New("tok", api.WithBaseURL(srv.URL), api.WithHTTPClient(srv.Client()))

	var buf bytes.Buffer
	if err := cmdUpdate(env(&buf, s, c, updateNow, time.UTC), nil, false, "pay", updateSince, false, false); err != nil {
		t.Fatalf("update: %v", err)
	}
	for _, p := range paths {
		if p != "/workspaces/1/projects/2/tasks" && p != "/me/time_entries" {
			t.Errorf("unexpected request path %q", p)
		}
	}
	p2 := int64(2)
	scoped := mustListTasks(t, s, false, &p2)
	if len(scoped) != 1 || scoped[0].ID != 21 {
		t.Errorf("project 2 tasks = %+v, want only id 21", scoped)
	}
	// The entry pull was scoped to the resolved project too.
	if got := mustEntryByRemoteID(t, s, 2); got == nil {
		t.Error("payments entry should be inserted")
	}
	if got := mustEntryByRemoteID(t, s, 1); got != nil {
		t.Error("backend entry should be ignored by a payments-scoped update")
	}
}

// TestUpdateAmbiguousProjectFragment verifies a fragment matching more than one
// cached project fails with the candidate list (name + id) instead of guessing,
// and that nothing is fetched before the ambiguity is resolved.
func TestUpdateAmbiguousProjectFragment(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)
	if err := s.PutProject(ctx, store.Project{
		ID: 3, WorkspaceID: 1, Name: "Backend API", Active: true,
	}); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %q: an ambiguous fragment must fail first", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := api.New("tok", api.WithBaseURL(srv.URL), api.WithHTTPClient(srv.Client()))

	var buf bytes.Buffer
	err := cmdUpdate(env(&buf, s, c, updateNow, time.UTC), nil, false, "back", updateSince, false, false)
	if err == nil {
		t.Fatal("update: expected an ambiguous-fragment error")
	}
	for _, want := range []string{"multiple projects match", "Backend (1)", "Backend API (3)"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to contain %q", err, want)
		}
	}
	// An exact (case-insensitive) name still wins over the substring matches,
	// so the ambiguity is escapable without reaching for TOGGL_PROJECT_ID.
	got, err := resolveUpdateProject(ctx, s, nil, "backend", false)
	if err != nil {
		t.Fatalf("resolve exact name: %v", err)
	}
	if got == nil || *got != 1 {
		t.Errorf("resolved = %v, want 1 (exact name wins)", got)
	}
}

// TestUpdateAmbiguousProjectFragmentFirst verifies `-1` resolves the same
// ambiguous fragment by taking the first candidate ("Backend", id 1) and that
// update then really refreshes THAT project.
func TestUpdateAmbiguousProjectFragmentFirst(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)
	if err := s.PutProject(ctx, store.Project{
		ID: 3, WorkspaceID: 1, Name: "Backend API", Active: true,
	}); err != nil {
		t.Fatal(err)
	}

	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/workspaces/1/projects/1/tasks":
			w.Write([]byte(`[{"id":15,"workspace_id":1,"project_id":1,"name":"Fresh task","active":true}]`))
		case "/me/time_entries":
			w.Write([]byte(`[]`))
		default:
			t.Errorf("unexpected path %q (-1 should pick project 1)", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := api.New("tok", api.WithBaseURL(srv.URL), api.WithHTTPClient(srv.Client()))

	var buf bytes.Buffer
	if err := cmdUpdate(env(&buf, s, c, updateNow, time.UTC), nil, true, "back", updateSince, false, false); err != nil {
		t.Fatalf("update -1: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("update -1 made no requests")
	}
	p1 := int64(1)
	tasks := mustListTasks(t, s, false, &p1)
	if len(tasks) != 1 || tasks[0].ID != 15 {
		t.Errorf("project 1 tasks = %+v, want only id 15 (Backend was refreshed)", tasks)
	}
}

// pullNow is a fixed mid-month, mid-day clock for the `tg pull` window tests.
var pullNow = time.Date(2026, 3, 17, 15, 30, 0, 0, time.UTC)

// TestResolvePullSince tables `tg pull`'s window start: the default is today,
// -a/--all widens it to the whole current calendar month, an explicit --since
// date wins over both, and a malformed one is rejected in the shared
// "invalid --since" style. Both defaults are day-aligned in the CALLER's
// location rather than in UTC, so they mean "today" and "this month" in calendar
// terms.
func TestResolvePullSince(t *testing.T) {
	t.Parallel()
	// 2026-03-01T01:00+03:00 is still February in UTC, so a UTC-framed
	// alignment would put these two cases in the wrong month.
	eastern := time.FixedZone("UTC+3", 3*60*60)
	lateFebruaryUTC := time.Date(2026, 2, 28, 22, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name    string
		flag    string
		all     bool
		now     time.Time
		loc     *time.Location
		want    time.Time
		wantErr string
	}{
		{
			name: "default is today", now: pullNow, loc: time.UTC,
			want: time.Date(2026, 3, 17, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "--all is this month", all: true, now: pullNow, loc: time.UTC,
			want: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "explicit since", flag: "2025-11-04", now: pullNow, loc: time.UTC,
			want: time.Date(2025, 11, 4, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "explicit since wins over --all", flag: "2025-11-04", all: true, now: pullNow, loc: time.UTC,
			want: time.Date(2025, 11, 4, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "today is day-aligned in loc", now: lateFebruaryUTC, loc: eastern,
			want: time.Date(2026, 3, 1, 0, 0, 0, 0, eastern),
		},
		{
			name: "the month is aligned in loc too", all: true, now: lateFebruaryUTC, loc: eastern,
			want: time.Date(2026, 3, 1, 0, 0, 0, 0, eastern),
		},
		{
			name: "malformed since", flag: "17-03-2026", now: pullNow, loc: time.UTC,
			wantErr: "invalid --since",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := resolvePullSince(tc.flag, tc.all, tc.now, tc.loc)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolvePullSince: %v", err)
			}
			if !got.Equal(tc.want) {
				t.Errorf("since = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestPullFlagRegistration pins runPull's own flag wiring, through the same
// bindPullFlags the command uses (see TestUpdateFlagRegistration for why the
// test does not declare its own): --all/-a are aliases sharing one variable and
// default to false (today only), --since takes the explicit window start,
// -1/--first is the shared ambiguity flag, and any of them may follow the
// positional project fragment, as in `tg pull backend -a`.
func TestPullFlagRegistration(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		args      []string
		wantFrag  string
		wantAll   bool
		wantSince string
		wantJSON  bool
		wantFirst bool
		wantErr   string
	}{
		{args: nil},
		{args: []string{"backend"}, wantFrag: "backend"},
		{args: []string{"-a"}, wantAll: true},
		{args: []string{"--all"}, wantAll: true},
		{args: []string{"backend", "-a"}, wantFrag: "backend", wantAll: true},
		{args: []string{"--all", "backend"}, wantFrag: "backend", wantAll: true},
		{args: []string{"--since", "2025-11-04"}, wantSince: "2025-11-04"},
		{args: []string{"backend", "--since=2025-11-04", "--json"}, wantFrag: "backend", wantSince: "2025-11-04", wantJSON: true},
		{args: []string{"back", "office", "-1"}, wantFrag: "back office", wantFirst: true},
		{args: []string{"--first", "backend"}, wantFrag: "backend", wantFirst: true},
		{args: []string{"--nope"}, wantErr: "not defined"},
	} {
		t.Run(fmt.Sprint(tc.args), func(t *testing.T) {
			t.Parallel()
			fs := flag.NewFlagSet("pull", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			f := bindPullFlags(fs)

			rest, err := parseArgsAndFlags(fs, tc.args)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("parse %v: err = %v, want it to contain %q", tc.args, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse %v: %v", tc.args, err)
			}
			// runPull joins the positionals into one project fragment.
			if frag := strings.Join(rest, " "); frag != tc.wantFrag {
				t.Errorf("fragment = %q, want %q", frag, tc.wantFrag)
			}
			if f.all != tc.wantAll {
				t.Errorf("all = %v, want %v", f.all, tc.wantAll)
			}
			if f.since != tc.wantSince {
				t.Errorf("since = %q, want %q", f.since, tc.wantSince)
			}
			if f.jsonOut != tc.wantJSON || f.first != tc.wantFirst {
				t.Errorf("json/first = %v/%v, want %v/%v", f.jsonOut, f.first, tc.wantJSON, tc.wantFirst)
			}
		})
	}
}

// TestPullTodayWindowKeepsStaleWatermark verifies the default today-only window
// is treated as a partial pull at the command level: an older watermark is left
// alone so a later wider pull still reconciles the days in between.
func TestPullTodayWindowKeepsStaleWatermark(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)
	if err := s.SetMeta(ctx, store.MetaLastPull, "2026-01-01T09:00:00Z"); err != nil {
		t.Fatalf("seed watermark: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	c := api.New("tok", api.WithBaseURL(srv.URL), api.WithHTTPClient(srv.Client()))

	now := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	since := startOfDay(now, time.UTC)
	var buf bytes.Buffer
	if err := cmdPull(env(&buf, s, c, now, time.UTC), false, "", since, false); err != nil {
		t.Fatalf("pull: %v", err)
	}
	v, _ := mustMeta(t, s, store.MetaLastPull)
	if v != "2026-01-01T09:00:00Z" {
		t.Errorf("last_pull = %q, want the untouched watermark", v)
	}
}

// TestProjectsUpdateSyncsWholeWorkspace verifies `projects update` walks the
// entire workspace project list and upserts it (without wiping other cached
// projects) while never fetching tasks.
func TestProjectsUpdateSyncsWholeWorkspace(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)

	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path != "/workspaces/1/projects" {
			t.Errorf("unexpected path %q (projects update must not fetch tasks)", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write([]byte(`[{"id":2,"workspace_id":1,"name":"Payments","billable":true,"active":true},{"id":3,"workspace_id":1,"name":"Frontend","active":true}]`))
	}))
	defer srv.Close()
	c := api.New("tok", api.WithBaseURL(srv.URL), api.WithHTTPClient(srv.Client()))

	var buf bytes.Buffer
	if err := cmdUpdateProjects(apiEnv(&buf, s, c), false, false); err != nil {
		t.Fatalf("projects update: %v", err)
	}
	if !strings.Contains(buf.String(), "2 projects") {
		t.Errorf("output = %q, want project count", buf.String())
	}

	// Only the workspace projects endpoint was hit; tasks were never fetched.
	for _, p := range paths {
		if p != "/workspaces/1/projects" {
			t.Errorf("unexpected request path %q", p)
		}
	}

	// The fetched project was added and the pre-existing project 1 (Backend,
	// not in the response) is left untouched by the upsert.
	projects := mustListProjects(t, s, false)
	names := map[string]bool{}
	for _, p := range projects {
		names[p.Name] = true
	}
	for _, want := range []string{"Backend", "Payments", "Frontend"} {
		if !names[want] {
			t.Errorf("project %q missing after sync: %+v", want, projects)
		}
	}

	// Cached tasks are untouched: projects update never syncs tasks.
	p1 := int64(1)
	backend := mustListTasks(t, s, false, &p1)
	if len(backend) == 0 {
		t.Error("project 1 tasks should be untouched by projects update")
	}
}

func TestProjectsUpdateJSON(t *testing.T) {
	t.Parallel()
	s := newStore(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"id":2,"workspace_id":1,"name":"Payments","active":true}]`))
	}))
	defer srv.Close()
	c := api.New("tok", api.WithBaseURL(srv.URL), api.WithHTTPClient(srv.Client()))

	var buf bytes.Buffer
	if err := cmdUpdateProjects(apiEnv(&buf, s, c), false, true); err != nil {
		t.Fatalf("projects update --json: %v", err)
	}
	if !strings.Contains(buf.String(), `"projects":1`) {
		t.Errorf("json output = %q, want projects count", buf.String())
	}
}

func TestProjectsCommand(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)

	var buf bytes.Buffer
	if err := cmdProjects(localEnv(&buf, s), false, false); err != nil {
		t.Fatalf("projects: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"Backend", "Payments", "1", "2"} {
		if !strings.Contains(out, want) {
			t.Errorf("projects output missing %q:\n%s", want, out)
		}
	}
}

// seedSampleDay mirrors the fixture behind today.txt / current.txt goldens.
func seedSampleDay(t *testing.T, s *store.Store) (now time.Time, loc *time.Location) {
	t.Helper()
	if err := s.ReplaceProjects(ctx, []store.Project{{ID: 1, WorkspaceID: 1, Name: "Backend", Active: true}}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceTasks(ctx, []store.Task{
		{ID: 10, WorkspaceID: 1, ProjectID: 1, Name: "Fix login bug", Active: true},
		{ID: 12, WorkspaceID: 1, ProjectID: 1, Name: "Code review", Active: true},
	}); err != nil {
		t.Fatal(err)
	}
	start1 := time.Date(2026, 1, 2, 9, 15, 0, 0, time.UTC)
	stop1 := time.Date(2026, 1, 2, 10, 30, 0, 0, time.UTC)
	start2 := time.Date(2026, 1, 2, 10, 30, 0, 0, time.UTC)
	if _, err := s.CreateEntry(ctx, store.Entry{
		WorkspaceID: 1, ProjectID: ptrInt(1), TaskID: ptrInt(10),
		Start: start1, Stop: &stop1, Duration: 4500, UpdatedAt: stop1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateEntry(ctx, store.Entry{
		WorkspaceID: 1, ProjectID: ptrInt(1), TaskID: ptrInt(12),
		Start: start2, Duration: -1, UpdatedAt: start2,
	}); err != nil {
		t.Fatal(err)
	}
	return time.Date(2026, 1, 2, 11, 15, 0, 0, time.UTC), time.UTC
}

func TestTodayCommandGolden(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	now, loc := seedSampleDay(t, s)
	var buf bytes.Buffer
	if err := cmdToday(env(&buf, s, nil, now, loc), 1, false, false); err != nil {
		t.Fatalf("today: %v", err)
	}
	assertGolden(t, "today.txt", buf.String())
}

// TestTodayCommandNumbers pins the numbers behind `tg mod 2` / `tg del 3`: the
// listing shows each entry's own persistent per-day number (human and JSON
// alike), and those are exactly the numbers the store resolves — listing is a
// read, so it never renumbers anything.
func TestTodayCommandNumbers(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	now, loc := seedSampleDay(t, s)

	entries, err := s.EntriesBetween(ctx, startOfDay(now, loc), startOfDay(now, loc).Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("fixture has %d entries, want 2", len(entries))
	}

	var buf bytes.Buffer
	if err := cmdToday(env(&buf, s, nil, now, loc), 1, false, false); err != nil {
		t.Fatalf("today: %v", err)
	}
	for i, e := range entries {
		got, err := s.EntryByNum(ctx, i+1, now)
		if err != nil {
			t.Fatalf("EntryByNum(%d): %v", i+1, err)
		}
		if got.ID != e.ID {
			t.Errorf("EntryByNum(%d).ID = %d, want %d", i+1, got.ID, e.ID)
		}
	}
	if _, err := s.EntryByNum(ctx, 3, now); !errors.Is(err, store.ErrNoEntryNum) {
		t.Errorf("EntryByNum(3) error = %v, want ErrNoEntryNum", err)
	}

	// The numbers are in the human output and in the JSON shape.
	if !strings.Contains(buf.String(), "1  09:15-10:30") {
		t.Errorf("listing missing leading entry numbers:\n%s", buf.String())
	}
	buf.Reset()
	if err := cmdToday(env(&buf, s, nil, now, loc), 1, true, false); err != nil {
		t.Fatalf("today --json: %v", err)
	}
	if !strings.Contains(buf.String(), `"num":1`) || !strings.Contains(buf.String(), `"num":2`) {
		t.Errorf("json listing missing entry numbers:\n%s", buf.String())
	}

	// Deleting the first entry leaves the second one addressable as 2: the
	// listing shows the surviving numbers, gap and all.
	if err := cmdDel(env(io.Discard, s, nil, now, loc), 1); err != nil {
		t.Fatalf("del: %v", err)
	}
	buf.Reset()
	if err := cmdToday(env(&buf, s, nil, now, loc), 1, false, false); err != nil {
		t.Fatalf("today after del: %v", err)
	}
	if !strings.Contains(buf.String(), "2  10:30-") {
		t.Errorf("listing renumbered the survivor:\n%s", buf.String())
	}

	// Listing a day with nothing tracked is just an empty listing; it cannot
	// make another day's numbers resolve.
	empty := now.AddDate(0, 0, 1)
	buf.Reset()
	if err := cmdToday(env(&buf, s, nil, empty, loc), 1, false, false); err != nil {
		t.Fatalf("today (empty day): %v", err)
	}
	if _, err := s.EntryByNum(ctx, 2, empty); !errors.Is(err, store.ErrNoEntryNum) {
		t.Errorf("EntryByNum(2) on an empty day = %v, want ErrNoEntryNum", err)
	}
	if got, err := s.EntryByNum(ctx, 2, now); err != nil || got.ID != entries[1].ID {
		t.Errorf("EntryByNum(2) = %+v err=%v, want the number still live on its own day", got, err)
	}
}

// TestTodayCommandMultiDayGrouping covers `tg ls --days N`: each day carries its
// own 1..N, so the listing groups entries under a date header instead of
// showing a flat run of repeated numbers.
func TestTodayCommandMultiDayGrouping(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)

	mk := func(start time.Time) {
		stop := start.Add(time.Hour)
		if _, err := s.CreateEntry(ctx, store.Entry{
			WorkspaceID: 1, ProjectID: ptrInt(1), TaskID: ptrInt(10),
			Start: start, Stop: &stop, Duration: 3600, UpdatedAt: stop,
		}); err != nil {
			t.Fatal(err)
		}
	}
	yesterday := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	today := time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC)
	mk(yesterday)
	mk(yesterday.Add(2 * time.Hour))
	mk(today)

	now := today.Add(3 * time.Hour)
	var buf bytes.Buffer
	if err := cmdToday(env(&buf, s, nil, now, time.UTC), 2, false, false); err != nil {
		t.Fatalf("today --days 2: %v", err)
	}
	got := buf.String()
	for _, want := range []string{"Thu 2026-01-01", "Fri 2026-01-02"} {
		if !strings.Contains(got, want) {
			t.Errorf("listing missing date header %q:\n%s", want, got)
		}
	}
	// Both days number from 1, and today's single entry is entry 1 of its own
	// day rather than entry 3 of the listing.
	if strings.Count(got, "1  09:00-10:00") != 2 {
		t.Errorf("want each day to start numbering at 1:\n%s", got)
	}
	if e, err := s.EntryByNum(ctx, 1, now); err != nil || !e.Start.Equal(today) {
		t.Errorf("EntryByNum(1, today) = %+v err=%v, want today's entry", e, err)
	}
}

// TestTodayCommandTrailingGap covers the closing filler row: with the last
// entry finished and now past its stop, the listing reports the idle time
// before the total footer.
func TestTodayCommandTrailingGap(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)

	start := time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC)
	stop := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)
	if _, err := s.CreateEntry(ctx, store.Entry{
		WorkspaceID: 1, ProjectID: ptrInt(1), TaskID: ptrInt(10),
		Start: start, Stop: &stop, Duration: 3600, UpdatedAt: stop,
	}); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 1, 2, 10, 25, 0, 0, time.UTC)
	var buf bytes.Buffer
	if err := cmdToday(env(&buf, s, nil, now, time.UTC), 1, false, false); err != nil {
		t.Fatalf("today: %v", err)
	}
	want := "1  09:00-10:00 1h00m  Fix login bug     [Backend]\n" +
		strings.Repeat(" ", 15) + "(gap 0h25m)\n" +
		todayDivider + "\nTotal: 1h00m\n"
	if buf.String() != want {
		t.Errorf("listing = %q, want %q", buf.String(), want)
	}
}

// seedDailyMonth records one finished entry per given (day, duration) pair in
// January 2026. Each entry starts at 09:00 on its day, so the durations never
// collide.
func seedDailyMonth(t *testing.T, s *store.Store, days map[int]time.Duration) {
	t.Helper()
	seedCatalog(t, s)
	for day, d := range days {
		seedFixture(t, s, fixtureEntry{
			start: time.Date(2026, 1, day, 9, 0, 0, 0, time.UTC), dur: d, taskID: 10,
		})
	}
}

// TestDailyOutput tables `tg daily`'s listing: entries are summed per calendar
// day, one line each, every line reports that day's overtime against the target
// and the footer sums the month and measures it against target x the number of
// LISTED days. A running entry contributes its elapsed-so-far time (flagged with
// a star, as `tg today`/`tg status` count it), days after today are dimmed when
// color is on so booked-ahead time reads as planned rather than worked, and a
// month with nothing tracked is an explanatory line rather than an error.
func TestDailyOutput(t *testing.T) {
	t.Parallel()
	on := func(day, hour int) time.Time { return time.Date(2026, 1, day, hour, 0, 0, 0, time.UTC) }
	for _, tc := range []struct {
		name    string
		entries []fixtureEntry
		now     time.Time
		color   bool
		want    string
	}{
		{
			name: "sums entries per day",
			entries: []fixtureEntry{
				{start: on(5, 9), dur: 4 * time.Hour, taskID: 10},
				{start: on(5, 13), dur: 4*time.Hour + 30*time.Minute, taskID: 10},
				{start: on(6, 9), dur: 7*time.Hour + 15*time.Minute, taskID: 10},
			},
			now: on(20, 12),
			want: "Mon 2026-01-05  8h30m   +0:30\n" +
				"Tue 2026-01-06  7h15m   -0:45\n" +
				todayDivider + "\n" +
				"Total: 15h45m  -0:15  (2 days x 8h00m)\n",
		},
		{
			name:    "a running entry counts live",
			entries: []fixtureEntry{{start: on(5, 9), taskID: 12}},
			now:     time.Date(2026, 1, 5, 11, 30, 0, 0, time.UTC),
			want: "Mon 2026-01-05  2h30m*  -5:30\n" +
				todayDivider + "\n" +
				"Total: 2h30m   -5:30  (1 day x 8h00m)   (* running)\n",
		},
		{
			name: "upcoming days are dimmed",
			entries: []fixtureEntry{
				{start: on(19, 9), dur: 8 * time.Hour, taskID: 10}, // yesterday
				{start: on(20, 9), dur: 6 * time.Hour, taskID: 10}, // today
				{start: on(21, 9), dur: 4 * time.Hour, taskID: 10}, // booked ahead
			},
			now:   time.Date(2026, 1, 20, 18, 0, 0, 0, time.UTC),
			color: true,
			want: "Mon 2026-01-19  8h00m   +0:00\n" +
				"Tue 2026-01-20  6h00m   -2:00\n" +
				faint("Wed 2026-01-21  4h00m   -4:00") + "\n" +
				todayDivider + "\n" +
				"Total: 18h00m  -6:00  (3 days x 8h00m)\n",
		},
		{
			// Safe to run on a fresh install.
			name: "empty month",
			now:  on(20, 12),
			want: "No entries this month.\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newStore(t)
			seedCatalog(t, s)
			seedFixture(t, s, tc.entries...)

			var buf bytes.Buffer
			if err := cmdDaily(env(&buf, s, nil, tc.now, time.UTC), dailyDefaultTarget, false, false, tc.color); err != nil {
				t.Fatalf("daily: %v", err)
			}
			if got := buf.String(); got != tc.want {
				t.Errorf("daily = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDailyCoversWholeMonth pins the window: the listing spans the FULL
// calendar month containing now, so days later in the month are included even
// when now sits early in it, while the neighbouring months are excluded.
func TestDailyCoversWholeMonth(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)

	hour := func(y int, m time.Month, d int) fixtureEntry {
		return fixtureEntry{start: time.Date(y, m, d, 9, 0, 0, 0, time.UTC), dur: time.Hour, taskID: 10}
	}
	seedFixture(t, s,
		hour(2025, time.December, 31), // previous month
		hour(2026, time.January, 1),   // first day of the month
		hour(2026, time.January, 31),  // last day of the month
		hour(2026, time.February, 1),  // next month
	)

	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	var buf bytes.Buffer
	if err := cmdDaily(env(&buf, s, nil, now, time.UTC), dailyDefaultTarget, false, false, false); err != nil {
		t.Fatalf("daily: %v", err)
	}
	got := buf.String()
	for _, want := range []string{"Thu 2026-01-01", "Sat 2026-01-31", "(2 days x 8h00m)"} {
		if !strings.Contains(got, want) {
			t.Errorf("daily missing %q:\n%s", want, got)
		}
	}
	for _, gone := range []string{"2025-12-31", "2026-02-01"} {
		if strings.Contains(got, gone) {
			t.Errorf("daily leaked a neighbouring month (%s):\n%s", gone, got)
		}
	}
}

// TestDailyExcludeToday pins -n/--no-today: today's row is dropped from both the
// listing and the footer's totals (its target shrinks with it), while earlier
// days stay and so do days booked ahead — the filter removes only the one day
// that is now's, reckoned as a calendar day.
func TestDailyExcludeToday(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedDailyMonth(t, s, map[int]time.Duration{
		5:  8 * time.Hour, // an earlier, finished day
		20: 6 * time.Hour, // today, still in progress
		21: 4 * time.Hour, // booked ahead
	})
	now := time.Date(2026, 1, 20, 18, 0, 0, 0, time.UTC)

	var buf bytes.Buffer
	if err := cmdDaily(env(&buf, s, nil, now, time.UTC), dailyDefaultTarget, true, false, false); err != nil {
		t.Fatalf("daily -n: %v", err)
	}
	got := buf.String()
	if strings.Contains(got, "2026-01-20") {
		t.Errorf("daily -n still lists today:\n%s", got)
	}
	for _, want := range []string{
		"Mon 2026-01-05", "Wed 2026-01-21",
		"Total: 12h00m  -4:00  (2 days x 8h00m)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("daily -n missing %q:\n%s", want, got)
		}
	}
}

// TestDailyExcludeTodayOnlyToday pins the degenerate case: when today is the
// only tracked day, -n empties the listing, which reads as the same "nothing
// this month" line an empty month shows.
func TestDailyExcludeTodayOnlyToday(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedDailyMonth(t, s, map[int]time.Duration{20: 6 * time.Hour})
	now := time.Date(2026, 1, 20, 18, 0, 0, 0, time.UTC)

	var buf bytes.Buffer
	if err := cmdDaily(env(&buf, s, nil, now, time.UTC), dailyDefaultTarget, true, false, false); err != nil {
		t.Fatalf("daily -n: %v", err)
	}
	if got := buf.String(); got != "No entries this month.\n" {
		t.Errorf("daily -n with only today = %q, want the empty-month line", got)
	}
}

// TestDailyExcludeTodayJSON pins that -n reaches the machine-readable shape too:
// today's object is gone and the month totals are recomputed over the remaining
// days, so --json and the human listing agree on which days count.
func TestDailyExcludeTodayJSON(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedDailyMonth(t, s, map[int]time.Duration{
		5:  8 * time.Hour,
		20: 6 * time.Hour, // today
	})
	now := time.Date(2026, 1, 20, 18, 0, 0, 0, time.UTC)

	var buf bytes.Buffer
	if err := cmdDaily(env(&buf, s, nil, now, time.UTC), dailyDefaultTarget, true, true, false); err != nil {
		t.Fatalf("daily -n --json: %v", err)
	}
	var got dailyJSON
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal %q: %v", buf.String(), err)
	}
	want := dailyJSON{
		Days: []dailyDayJSON{
			{Date: "2026-01-05", DurationSeconds: 28800, OvertimeSeconds: 0},
		},
		Tracked:         28800,
		TargetSeconds:   28800,
		OvertimeSeconds: 0,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("daily -n --json = %+v, want %+v", got, want)
	}
}

// TestDailyJSONCarriesNoStyling pins that --json stays a data shape: even on a
// terminal (color on), where the human listing dims the upcoming days, the JSON
// output carries no ANSI escapes.
func TestDailyJSONCarriesNoStyling(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedDailyMonth(t, s, map[int]time.Duration{
		19: 8 * time.Hour, // yesterday
		20: 6 * time.Hour, // today
		21: 4 * time.Hour, // booked ahead
	})
	now := time.Date(2026, 1, 20, 18, 0, 0, 0, time.UTC)

	var buf bytes.Buffer
	if err := cmdDaily(env(&buf, s, nil, now, time.UTC), dailyDefaultTarget, false, true, true); err != nil {
		t.Fatalf("daily --json: %v", err)
	}
	if strings.Contains(buf.String(), "\x1b") {
		t.Errorf("daily --json contains ANSI escapes: %q", buf.String())
	}
}

// TestDailyTargetFlag pins -t/--target: it shifts every overtime figure,
// including the footer's, and defaults to 8 hours.
func TestDailyTargetFlag(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedDailyMonth(t, s, map[int]time.Duration{5: 6 * time.Hour, 6: 6 * time.Hour})
	now := time.Date(2026, 1, 20, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		target float64
		day    string
		footer string
	}{
		{dailyDefaultTarget, "-2:00", "Total: 12h00m  -4:00  (2 days x 8h00m)"},
		{6, "+0:00", "Total: 12h00m  +0:00  (2 days x 6h00m)"},
		{7.5, "-1:30", "Total: 12h00m  -3:00  (2 days x 7h30m)"},
		{0, "+6:00", "Total: 12h00m  +12:00  (2 days x 0h00m)"},
	}
	for _, c := range cases {
		var buf bytes.Buffer
		if err := cmdDaily(env(&buf, s, nil, now, time.UTC), c.target, false, false, false); err != nil {
			t.Fatalf("daily -t %v: %v", c.target, err)
		}
		got := buf.String()
		// The overtime closes each day line, so the newline keeps the footer's
		// own figure out of the count.
		if strings.Count(got, c.day+"\n") != 2 {
			t.Errorf("daily -t %v: want both days at %q:\n%s", c.target, c.day, got)
		}
		if !strings.Contains(got, c.footer) {
			t.Errorf("daily -t %v: want footer %q:\n%s", c.target, c.footer, got)
		}
	}
}

// TestDailyRejectsNegativeTarget: a negative target is nonsense (it would turn
// every worked minute into double overtime), so it is a usage error rather than
// something silently accepted.
func TestDailyRejectsNegativeTarget(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedDailyMonth(t, s, map[int]time.Duration{5: 8 * time.Hour})
	now := time.Date(2026, 1, 20, 12, 0, 0, 0, time.UTC)
	var buf bytes.Buffer
	err := cmdDaily(env(&buf, s, nil, now, time.UTC), -1, false, false, false)
	if err == nil {
		t.Fatalf("daily -t -1: expected an error, got %q", buf.String())
	}
	if !strings.Contains(err.Error(), "negative") {
		t.Errorf("daily -t -1 error = %v, want it to mention the negative target", err)
	}
}

// TestDailySkipsDeletedEntries pins that a soft-deleted entry stops counting
// towards its day, and that a day left with nothing tracked drops out of the
// listing entirely (which also shrinks the footer's target).
func TestDailySkipsDeletedEntries(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedDailyMonth(t, s, map[int]time.Duration{5: 8 * time.Hour, 6: 8 * time.Hour})
	now := time.Date(2026, 1, 6, 20, 0, 0, 0, time.UTC)

	// Entry 1 of 2026-01-06 is today's only entry, so `tg del 1` removes it.
	if err := cmdDel(env(io.Discard, s, nil, now, time.UTC), 1); err != nil {
		t.Fatalf("del: %v", err)
	}
	var buf bytes.Buffer
	if err := cmdDaily(env(&buf, s, nil, now, time.UTC), dailyDefaultTarget, false, false, false); err != nil {
		t.Fatalf("daily: %v", err)
	}
	got := buf.String()
	if strings.Contains(got, "2026-01-06") {
		t.Errorf("daily still lists the emptied day:\n%s", got)
	}
	if !strings.Contains(got, "Total: 8h00m   +0:00  (1 day x 8h00m)") {
		t.Errorf("daily footer should count only the surviving day:\n%s", got)
	}
}

// TestDailyJSON pins the machine-readable shape: one object per listed day with
// its date, tracked seconds and signed overtime, plus the month totals and the
// per-day target the overtimes were measured against.
func TestDailyJSON(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedDailyMonth(t, s, map[int]time.Duration{
		5: 8*time.Hour + 30*time.Minute,
		6: 7*time.Hour + 15*time.Minute,
	})
	now := time.Date(2026, 1, 20, 12, 0, 0, 0, time.UTC)
	var buf bytes.Buffer
	if err := cmdDaily(env(&buf, s, nil, now, time.UTC), dailyDefaultTarget, false, true, false); err != nil {
		t.Fatalf("daily --json: %v", err)
	}
	var got dailyJSON
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal %q: %v", buf.String(), err)
	}
	want := dailyJSON{
		Days: []dailyDayJSON{
			{Date: "2026-01-05", DurationSeconds: 30600, OvertimeSeconds: 1800},
			{Date: "2026-01-06", DurationSeconds: 26100, OvertimeSeconds: -2700},
		},
		Tracked:         56700,
		TargetSeconds:   28800,
		OvertimeSeconds: -900,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("daily --json = %+v, want %+v", got, want)
	}
}

// TestGroupDailyBucketsByStartDay pins the grouping rule: an entry belongs
// entirely to the calendar day it STARTED on, even when it runs past midnight,
// and the rows come out in chronological order.
func TestGroupDailyBucketsByStartDay(t *testing.T) {
	t.Parallel()
	loc := time.UTC
	start1 := time.Date(2026, 1, 5, 9, 0, 0, 0, loc)
	stop1 := start1.Add(time.Hour)
	// 23:00 -> 01:00 the next day: two hours, all of them on the 6th.
	start2 := time.Date(2026, 1, 6, 23, 0, 0, 0, loc)
	stop2 := start2.Add(2 * time.Hour)
	rows := groupDaily([]store.Entry{
		{Start: start1, Stop: &stop1, Duration: 3600},
		{Start: start2, Stop: &stop2, Duration: 7200},
	}, time.Date(2026, 1, 7, 9, 0, 0, 0, loc), loc)

	if len(rows) != 2 {
		t.Fatalf("groupDaily returned %d rows, want 2", len(rows))
	}
	if !rows[0].Day.Equal(time.Date(2026, 1, 5, 0, 0, 0, 0, loc)) || rows[0].Tracked != time.Hour {
		t.Errorf("rows[0] = %+v, want the 5th with 1h", rows[0])
	}
	if !rows[1].Day.Equal(time.Date(2026, 1, 6, 0, 0, 0, 0, loc)) || rows[1].Tracked != 2*time.Hour {
		t.Errorf("rows[1] = %+v, want the 6th with 2h (the whole cross-midnight entry)", rows[1])
	}
	if rows[0].Running || rows[1].Running {
		t.Errorf("no entry is running: %+v", rows)
	}
}

func TestCurrentCommandGolden(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	now, loc := seedSampleDay(t, s)
	var buf bytes.Buffer
	if err := cmdCurrent(env(&buf, s, nil, now, loc), false); err != nil {
		t.Fatalf("current: %v", err)
	}
	assertGolden(t, "current.txt", buf.String())
}

// TestCurrentCommandLine tables `tg status`'s one-line report. With a timer
// running it reports that live; otherwise it reports the newest already-started
// entry of TODAY together with the idle gap since it stopped. Time booked later
// today is not the last entry (it has not happened yet) but does count towards
// the day total, yesterday's entries are out of reach entirely, and an empty
// store still reports a total.
func TestCurrentCommandLine(t *testing.T) {
	t.Parallel()
	day := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	at := func(h, m int) time.Time {
		return day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute)
	}
	for _, tc := range []struct {
		name    string
		entries []fixtureEntry
		now     time.Time
		want    string
	}{
		{
			name: "last entry, gap and day total",
			entries: []fixtureEntry{
				{start: at(9, 15), dur: 75 * time.Minute, taskID: 10},
				{start: at(10, 30), dur: 30 * time.Minute, taskID: 12},
			},
			now:  at(11, 25),
			want: "Code review [Backend] 10:30-11:00 (gap 0h25m) Today: 1h45m\n",
		},
		{
			// The 18:00 entry starts later today, so it is not the last entry;
			// the day total covers it all the same.
			name: "entry booked later today is skipped",
			entries: []fixtureEntry{
				{start: at(9, 15), dur: 75 * time.Minute, taskID: 10},
				{start: at(18, 0), dur: time.Hour, taskID: 12},
			},
			now:  at(11, 25),
			want: "Fix login bug [Backend] 09:15-10:30 (gap 0h55m) Today: 2h15m\n",
		},
		{
			// A new day starts with no last entry rather than reporting
			// yesterday's with an overnight gap.
			name:    "yesterday is history",
			entries: []fixtureEntry{{start: day.Add(-8 * time.Hour), dur: time.Hour, taskID: 10}},
			now:     at(9, 30),
			want:    "No entries. Today: 0h00m\n",
		},
		{
			name: "empty store still reports a total",
			now:  at(9, 0),
			want: "No entries. Today: 0h00m\n",
		},
		{
			// A pulled running entry is reported as running, and its elapsed
			// time counts towards the day total alongside the finished entry.
			name: "running entry wins",
			entries: []fixtureEntry{
				{start: at(7, 0), dur: time.Hour, taskID: 10},
				{start: at(9, 0), taskID: 12},
			},
			now:  at(9, 30),
			want: "run Code review [Backend] (0h30m) Today: 1h30m\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newStore(t)
			seedCatalog(t, s)
			seedFixture(t, s, tc.entries...)

			var buf bytes.Buffer
			if err := cmdCurrent(env(&buf, s, nil, tc.now, time.UTC), false); err != nil {
				t.Fatalf("current: %v", err)
			}
			if got := buf.String(); got != tc.want {
				t.Errorf("status = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCurrentCommandJSON pins the machine-readable shape of the no-timer case:
// the same facts the human line carries, as fields.
func TestCurrentCommandJSON(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	seedCatalog(t, s)
	day := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	seedFixture(t, s,
		fixtureEntry{start: day.Add(9*time.Hour + 15*time.Minute), dur: 75 * time.Minute, taskID: 10},
		fixtureEntry{start: day.Add(10*time.Hour + 30*time.Minute), dur: 30 * time.Minute, taskID: 12},
	)

	var buf bytes.Buffer
	if err := cmdCurrent(env(&buf, s, nil, day.Add(11*time.Hour+25*time.Minute), time.UTC), true); err != nil {
		t.Fatalf("current --json: %v", err)
	}
	for _, want := range []string{
		`"running":false`, `"task":"Code review"`,
		`"stop":"2026-01-02T11:00:00Z"`, `"gap_seconds":1500`, `"day_total_seconds":6300`,
	} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("json = %s, want it to contain %s", buf.String(), want)
		}
	}
}

// dstLoc loads a zone that observes DST, for the day-window tests below. It
// skips the test when the machine has no zone database rather than failing on
// something the code under test cannot influence.
func dstLoc(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Vilnius")
	if err != nil {
		t.Skipf("no tzdata for Europe/Vilnius: %v", err)
	}
	return loc
}

// TestDayWindowSpringForward covers the 23-hour day: on 2026-03-29 Vilnius jumps
// 03:00 -> 04:00, so midnight + 24h lands at 01:00 the NEXT day and used to pull
// an hour of tomorrow into today. `tg ls` must list only today's entry and
// `tg status`'s day total must ignore tomorrow's 00:30 one.
func TestDayWindowSpringForward(t *testing.T) {
	t.Parallel()
	loc := dstLoc(t)
	s := newStoreIn(t, loc)
	seedCatalog(t, s)

	today := time.Date(2026, 3, 29, 10, 0, 0, 0, loc)
	todayStop := today.Add(time.Hour)
	// 00:30 on the 30th: inside midnight+24h (01:00), outside the calendar day.
	tomorrow := time.Date(2026, 3, 30, 0, 30, 0, 0, loc)
	tomorrowStop := tomorrow.Add(30 * time.Minute)
	for _, e := range []store.Entry{
		{WorkspaceID: 1, ProjectID: ptrInt(1), TaskID: ptrInt(10), Start: today, Stop: &todayStop, Duration: 3600, UpdatedAt: todayStop},
		{WorkspaceID: 1, ProjectID: ptrInt(1), TaskID: ptrInt(12), Start: tomorrow, Stop: &tomorrowStop, Duration: 1800, UpdatedAt: tomorrowStop},
	} {
		if _, err := s.CreateEntry(ctx, e); err != nil {
			t.Fatal(err)
		}
	}

	now := time.Date(2026, 3, 29, 12, 0, 0, 0, loc)
	var buf bytes.Buffer
	if err := cmdToday(env(&buf, s, nil, now, loc), 1, true, false); err != nil {
		t.Fatalf("today: %v", err)
	}
	var listing todayJSON
	if err := json.Unmarshal(buf.Bytes(), &listing); err != nil {
		t.Fatalf("today json: %v", err)
	}
	if len(listing.Entries) != 1 || listing.Entries[0].Task != "Fix login bug" {
		t.Errorf("entries = %+v, want only today's", listing.Entries)
	}
	if listing.TotalSeconds != 3600 {
		t.Errorf("total = %ds, want 3600 (tomorrow's entry excluded)", listing.TotalSeconds)
	}

	buf.Reset()
	if err := cmdCurrent(env(&buf, s, nil, now, loc), true); err != nil {
		t.Fatalf("current: %v", err)
	}
	if !strings.Contains(buf.String(), `"day_total_seconds":3600`) {
		t.Errorf("status json = %s, want day_total_seconds 3600", buf.String())
	}
}

// TestDayWindowFallBack covers the 25-hour day: on 2026-10-25 Vilnius falls back
// 04:00 -> 03:00, so midnight + 24h lands at 23:00 the same day and used to drop
// the final hour. An entry at 23:15 belongs to the day and must be listed and
// counted.
func TestDayWindowFallBack(t *testing.T) {
	t.Parallel()
	loc := dstLoc(t)
	s := newStoreIn(t, loc)
	seedCatalog(t, s)

	late := time.Date(2026, 10, 25, 23, 15, 0, 0, loc)
	lateStop := late.Add(30 * time.Minute)
	if _, err := s.CreateEntry(ctx, store.Entry{
		WorkspaceID: 1, ProjectID: ptrInt(1), TaskID: ptrInt(10),
		Start: late, Stop: &lateStop, Duration: 1800, UpdatedAt: lateStop,
	}); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 10, 25, 23, 50, 0, 0, loc)
	var buf bytes.Buffer
	if err := cmdToday(env(&buf, s, nil, now, loc), 1, true, false); err != nil {
		t.Fatalf("today: %v", err)
	}
	var listing todayJSON
	if err := json.Unmarshal(buf.Bytes(), &listing); err != nil {
		t.Fatalf("today json: %v", err)
	}
	if len(listing.Entries) != 1 || listing.TotalSeconds != 1800 {
		t.Errorf("listing = %+v, want the 23:15 entry (the day's 25th hour)", listing)
	}

	buf.Reset()
	if err := cmdCurrent(env(&buf, s, nil, now, loc), true); err != nil {
		t.Fatalf("current: %v", err)
	}
	if !strings.Contains(buf.String(), `"day_total_seconds":1800`) {
		t.Errorf("status json = %s, want day_total_seconds 1800", buf.String())
	}
}

// TestSyncCommandsSurfaceUnauthorized pins what an expired or revoked token
// looks like from a command: the api layer's ErrUnauthorized reaches the caller
// — carrying Toggl's own complaint, so the failure is diagnosable — instead of
// being swallowed or reported as a missing config, and the local store is left
// exactly as it was, dirty entry included, for a retry after `tg auth`.
//
// `push` is the one exception to the sentinel reaching through: it reports
// per-entry failures as a *togglsync.PushError summary (the queue is not
// abandoned on the first rejection), so only the message survives.
func TestSyncCommandsSurfaceUnauthorized(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		run  func(env *cmdEnv) error
		// wantSentinel is false for push, whose per-entry failure summary
		// carries the message but not the error value.
		wantSentinel bool
	}{
		{name: "pull", run: func(e *cmdEnv) error { return cmdPull(e, false, "", since, false) }, wantSentinel: true},
		{name: "update", run: func(e *cmdEnv) error { return cmdUpdate(e, ptrInt(1), false, "", since, false, false) }, wantSentinel: true},
		{name: "projects update", run: func(e *cmdEnv) error { return cmdUpdateProjects(e, false, false) }, wantSentinel: true},
		{name: "total", run: func(e *cmdEnv) error { return cmdTotal(e, false, nil, since, false) }, wantSentinel: true},
		{name: "push", run: func(e *cmdEnv) error { return cmdPush(e, false) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newStore(t)
			seedCatalog(t, s)
			id := seedDirty(t, s, 0, "work")

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte(`token expired`))
			}))
			defer srv.Close()
			c := api.New("tok", api.WithBaseURL(srv.URL),
				api.WithReportsBaseURL(srv.URL), api.WithHTTPClient(srv.Client()))

			var buf bytes.Buffer
			err := tc.run(env(&buf, s, c, pushNow, time.UTC))
			if err == nil {
				t.Fatal("err = nil, want the 401 reported")
			}
			if tc.wantSentinel && !errors.Is(err, api.ErrUnauthorized) {
				t.Errorf("err = %v, want it to wrap api.ErrUnauthorized", err)
			}
			if !strings.Contains(err.Error(), "token expired") {
				t.Errorf("err = %v, want Toggl's own complaint carried through", err)
			}
			if errors.Is(err, config.ErrNotConfigured) {
				t.Errorf("err = %v, want a rejected token, not a missing config", err)
			}
			// The local edit is untouched and still queued for a later push.
			dirty := mustDirtyEntries(t, s)
			if len(dirty) != 1 || dirty[0].ID != id {
				t.Fatalf("dirty = %+v, want the entry %d still queued", dirty, id)
			}
			if dirty[0].RemoteID != nil {
				t.Errorf("remote_id = %v, want none: nothing was accepted", dirty[0].RemoteID)
			}
		})
	}
}

// --- the shared command prologue ---------------------------------------------
//
// These tests exercise withEnv (through withEnvOut, which only adds the output
// stream): the config loading, the state directory and database creation and the
// cmdEnv every cmd* function is handed. They set XDG_STATE_HOME to a throwaway
// directory, so — like the auth tests below — they cannot run in parallel.

// TestWithEnvOffline covers the local-first case: with no config.json the
// prologue is not an error, it opens the database and hands the command a cmdEnv
// with no client, so a local command runs and only the API-bound ones complain
// (see TestSyncCommandsRequireCredentials).
func TestWithEnvOffline(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	var buf bytes.Buffer
	var got *cmdEnv
	if err := withEnvOut(ctx, &buf, func(e *cmdEnv) error {
		got = e
		return cmdCurrent(e, false)
	}); err != nil {
		t.Fatalf("withEnv: %v", err)
	}
	if got.st == nil {
		t.Fatal("no store was opened")
	}
	if !got.offline() {
		t.Error("env should be offline when there is no config")
	}
	if got.workspaceID != 0 {
		t.Errorf("workspace_id = %d, want 0 with no config", got.workspaceID)
	}
	if got.now.IsZero() || got.loc == nil {
		t.Errorf("clock/calendar = %v/%v, want them pinned once per invocation", got.now, got.loc)
	}
	if want := "No entries. Today: 0h00m\n"; buf.String() != want {
		t.Errorf("output = %q, want %q written to the given writer", buf.String(), want)
	}
	// The database was created inside the state directory rather than anywhere
	// else, which is what EnsureDir/DBPath are there for.
	path, err := config.DBPath()
	if err != nil {
		t.Fatalf("db path: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("stat %s: %v, want the database created in the state directory", path, err)
	}
}

// TestWithEnvUsesStoredCredentials verifies a stored config is what supplies the
// client and the workspace new entries are filed under.
func TestWithEnvUsesStoredCredentials(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := &config.Config{APIToken: "stored-token", WorkspaceID: 4242}
	if err := cfg.Save(); err != nil {
		t.Fatalf("save config: %v", err)
	}

	ran := false
	if err := withEnvOut(ctx, io.Discard, func(e *cmdEnv) error {
		ran = true
		if e.offline() {
			t.Error("env should carry a client when a config exists")
		}
		if e.workspaceID != 4242 {
			t.Errorf("workspace_id = %d, want 4242 from the stored config", e.workspaceID)
		}
		return nil
	}); err != nil {
		t.Fatalf("withEnv: %v", err)
	}
	if !ran {
		t.Error("the command never ran")
	}
}

// TestWithEnvCorruptedConfig pins the failure path a broken config.json takes:
// it is reported (naming the config load), which is what keeps a corrupted file
// from silently reading as "not authenticated" — that would downgrade the
// invocation to offline mode, hide the real problem behind "run `tg auth`" and
// invite overwriting the file. The command never runs.
func TestWithEnvCorruptedConfig(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if _, err := config.EnsureDir(); err != nil {
		t.Fatalf("ensure dir: %v", err)
	}
	path, err := config.Path()
	if err != nil {
		t.Fatalf("config path: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"api_token":`), 0o600); err != nil {
		t.Fatalf("write truncated config: %v", err)
	}

	ran := false
	err = withEnvOut(ctx, io.Discard, func(*cmdEnv) error {
		ran = true
		return nil
	})
	if err == nil {
		t.Fatal("withEnv on a corrupted config = nil error, want it reported")
	}
	if !strings.Contains(err.Error(), "load config") {
		t.Errorf("err = %v, want it to name the config load", err)
	}
	if errors.Is(err, config.ErrNotConfigured) {
		t.Errorf("err = %v, want it distinct from the not-authenticated error", err)
	}
	if ran {
		t.Error("the command must not run when the config cannot be loaded")
	}
}

func meHandler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/me" {
			t.Errorf("path = %q, want /me", r.URL.Path)
		}
		w.Write([]byte(`{"id":1,"default_workspace_id":12345,"fullname":"Test User"}`))
	}
}

func TestAuthSuccessWritesConfig(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	srv := httptest.NewServer(meHandler(t))
	defer srv.Close()

	var buf bytes.Buffer
	err := cmdAuth(ctx, &buf, func() (string, error) { return "tok123", nil }, func(token string) *api.Client {
		return api.New(token, api.WithBaseURL(srv.URL), api.WithHTTPClient(srv.Client()))
	})
	if err != nil {
		t.Fatalf("auth: %v", err)
	}
	if !strings.Contains(buf.String(), "Authenticated as Test User") {
		t.Errorf("output = %q", buf.String())
	}

	path, err := config.Path()
	if err != nil {
		t.Fatalf("config path: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("config.json missing: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("perm = %o, want 600", perm)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.APIToken != "tok123" || cfg.WorkspaceID != 12345 {
		t.Errorf("config = %+v", cfg)
	}
}

func TestAuthForbiddenWritesNothing(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	var buf bytes.Buffer
	err := cmdAuth(ctx, &buf, func() (string, error) { return "bad", nil }, func(token string) *api.Client {
		return api.New(token, api.WithBaseURL(srv.URL), api.WithHTTPClient(srv.Client()))
	})
	if err == nil {
		t.Fatal("expected an error for 403")
	}

	path, err := config.Path()
	if err != nil {
		t.Fatalf("config path: %v", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("config.json should not exist, stat err = %v", statErr)
	}
}
