package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
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
	s, err := store.OpenIn(filepath.Join(t.TempDir(), "tg.db"), loc)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func seedCatalog(t *testing.T, s *store.Store) {
	t.Helper()
	if err := s.ReplaceProjects([]store.Project{
		{ID: 1, WorkspaceID: 1, Name: "Backend", Active: true},
		{ID: 2, WorkspaceID: 1, Name: "Payments", Active: true, Billable: true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceTasks([]store.Task{
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
	if _, err := s.CreateEntry(store.Entry{
		WorkspaceID: 1, ProjectID: p(1), TaskID: &taskID,
		Start: start, Duration: -1, UpdatedAt: start,
	}); err != nil {
		t.Fatalf("seed running entry: %v", err)
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

func TestAddCreatesFinishedEntry(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	var buf bytes.Buffer
	if err := cmdAdd(&buf, s, nil, 1, nil, false, "9-:30", "login", "", addNow, time.UTC); err != nil {
		t.Fatalf("add: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"Added: Fix login bug", "09:00-09:30", "0h30m"} {
		if !strings.Contains(out, want) {
			t.Errorf("output = %q, want %q", out, want)
		}
	}

	entries, _ := s.EntriesBetween(addNow.Add(-24*time.Hour), addNow.Add(24*time.Hour))
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
	if r, _ := s.Running(); r != nil {
		t.Errorf("add must not create a running entry, got %+v", r)
	}
}

// TestAddAcceptsRelativeTimesign checks that `add` resolves a relative
// timesign through the timesig package: the entry ends at now floored to the
// preceding 5-minute mark and starts that many minutes earlier. (Overlap
// checks are a separate concern.)
func TestAddAcceptsRelativeTimesign(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	// 15:07 floors to 15:05, so "+:20" spans 14:45-15:05.
	now := time.Date(2026, 1, 2, 15, 7, 0, 0, time.UTC)
	var buf bytes.Buffer
	if err := cmdAdd(&buf, s, nil, 1, nil, false, "+:20", "login", "", now, time.UTC); err != nil {
		t.Fatalf("add: %v", err)
	}
	if out := buf.String(); !strings.Contains(out, "14:45-15:05") {
		t.Errorf("output = %q, want 14:45-15:05", out)
	}

	entries, _ := s.EntriesBetween(now.Add(-24*time.Hour), now.Add(24*time.Hour))
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

// TestAddRejectsInvalidRelativeTimesign keeps the zero-duration relative form
// out of the store.
func TestAddRejectsInvalidRelativeTimesign(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	var buf bytes.Buffer
	if err := cmdAdd(&buf, s, nil, 1, nil, false, "+:00", "login", "", addNow, time.UTC); err == nil {
		t.Fatal("add with +:00 = nil error, want an error")
	}
	if entries, _ := s.EntriesBetween(addNow.Add(-24*time.Hour), addNow.Add(24*time.Hour)); len(entries) != 0 {
		t.Errorf("entries = %d, want 0", len(entries))
	}
}

// TestAddDurationStartsAtLastEntryEnd covers the bare duration form: with no
// start time given, the entry picks up where the last one ended and runs for
// the duration typed, so `tg add 1:30 <task>` logs the block back to back.
func TestAddDurationStartsAtLastEntryEnd(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	// The last entry today ends at 10:00, so "1:30" is 10:00-11:30 regardless
	// of what time it is now (15:00).
	var buf bytes.Buffer
	if err := cmdAdd(&buf, s, nil, 1, nil, false, "9-10", "login", "", addNow, time.UTC); err != nil {
		t.Fatalf("add first: %v", err)
	}
	buf.Reset()
	if err := cmdAdd(&buf, s, nil, 1, nil, false, "1:30", "review", "", addNow, time.UTC); err != nil {
		t.Fatalf("add duration: %v", err)
	}
	if out := buf.String(); !strings.Contains(out, "10:00-11:30") || !strings.Contains(out, "1h30m") {
		t.Errorf("output = %q, want 10:00-11:30 (1h30m)", out)
	}

	entries, _ := s.EntriesBetween(addNow.Add(-24*time.Hour), addNow.Add(24*time.Hour))
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
	if err := cmdAdd(&buf, s, nil, 1, nil, false, ":30", "Fix", "", addNow, time.UTC); err != nil {
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
	s := newStore(t)
	seedCatalog(t, s)

	// Yesterday's entry is history: LastEntry is today-only, so it must not
	// become the anchor.
	if err := cmdAdd(&bytes.Buffer{}, s, nil, 1, nil, false, "9-10", "login", "",
		addNow.AddDate(0, 0, -1), time.UTC); err != nil {
		t.Fatalf("seed yesterday: %v", err)
	}

	var buf bytes.Buffer
	err := cmdAdd(&buf, s, nil, 1, nil, false, "1:30", "review", "", addNow, time.UTC)
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
	entries, _ := s.EntriesBetween(addNow.AddDate(0, 0, -2), addNow.Add(24*time.Hour))
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1 (yesterday's only)", len(entries))
	}
}

// TestAddDurationWithRunningLastEntry covers the other missing anchor: a
// running entry has no end time to continue from, so the bare form is refused
// exactly as `tg mod +DURATION` refuses one.
func TestAddDurationWithRunningLastEntry(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	seedRunning(t, s, 12, testStart)

	var buf bytes.Buffer
	err := cmdAdd(&buf, s, nil, 1, nil, false, "1:30", "login", "", addNow, time.UTC)
	if err == nil {
		t.Fatal("add 1:30 after a running entry = nil error, want an error")
	}
	if !strings.Contains(err.Error(), "still running") {
		t.Errorf("err = %v, want it to mention the running entry", err)
	}
	if entries, _ := s.EntriesBetween(addNow.Add(-24*time.Hour), addNow.Add(24*time.Hour)); len(entries) != 1 {
		t.Fatalf("entries = %d, want 1 (the running entry only)", len(entries))
	}
}

// TestAddDurationRejectsOverlap keeps the bare form under the same overlap
// guard as the rest: an entry booked for later today is skipped when resolving
// the anchor (LastEntry ignores future starts), so only the guard can catch a
// duration long enough to run into it.
func TestAddDurationRejectsOverlap(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	// It is 10:30. Tracked so far today: 09:00-10:00. Booked for later:
	// 11:00-12:00, which starts after now and so is never the anchor.
	now := time.Date(2026, 1, 2, 10, 30, 0, 0, time.UTC)
	if err := cmdAdd(&bytes.Buffer{}, s, nil, 1, nil, false, "9-10", "login", "", now, time.UTC); err != nil {
		t.Fatalf("add first: %v", err)
	}
	if err := cmdAdd(&bytes.Buffer{}, s, nil, 1, nil, false, "11-12", "Fix", "", now, time.UTC); err != nil {
		t.Fatalf("add later: %v", err)
	}

	var buf bytes.Buffer
	// 10:00 + 1h30m = 11:30, which straddles the 11:00-12:00 entry.
	err := cmdAdd(&buf, s, nil, 1, nil, false, "1:30", "review", "", now, time.UTC)
	if err == nil {
		t.Fatal("overlapping duration add = nil error, want an error")
	}
	for _, want := range []string{"overlaps existing entry", "10:00-11:30", "11:00-12:00"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to mention %q", err, want)
		}
	}
	if entries, _ := s.EntriesBetween(now.Add(-24*time.Hour), now.Add(24*time.Hour)); len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}

	// Exactly filling the gap up to the booked entry is fine (back to back).
	buf.Reset()
	if err := cmdAdd(&buf, s, nil, 1, nil, false, "1", "review", "", now, time.UTC); err != nil {
		t.Fatalf("gap-filling add: %v", err)
	}
	if out := buf.String(); !strings.Contains(out, "10:00-11:00") {
		t.Errorf("output = %q, want 10:00-11:00", out)
	}
}

// TestAddRejectsInvalidDuration keeps a malformed or zero bare duration out of
// the store, reported as a timesign error before any anchor lookup.
func TestAddRejectsInvalidDuration(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	if err := cmdAdd(&bytes.Buffer{}, s, nil, 1, nil, false, "9-10", "login", "", addNow, time.UTC); err != nil {
		t.Fatalf("add first: %v", err)
	}
	for _, ts := range []string{"0", ":00", "24", "1:60", "1:"} {
		var buf bytes.Buffer
		err := cmdAdd(&buf, s, nil, 1, nil, false, ts, "review", "", addNow, time.UTC)
		if err == nil || !strings.Contains(err.Error(), "timesign") {
			t.Errorf("add %q: err = %v, want a timesign parse error", ts, err)
		}
	}
	if entries, _ := s.EntriesBetween(addNow.Add(-24*time.Hour), addNow.Add(24*time.Hour)); len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
}

// TestAddWithDescription verifies --desc/--description sets the entry's
// description on the stored entry.
func TestAddWithDescription(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	var buf bytes.Buffer
	if err := cmdAdd(&buf, s, nil, 1, nil, false, "9-:30", "login", "reset password flow", addNow, time.UTC); err != nil {
		t.Fatalf("add: %v", err)
	}
	entries, _ := s.EntriesBetween(addNow.Add(-24*time.Hour), addNow.Add(24*time.Hour))
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if got := entries[0].Description; got != "reset password flow" {
		t.Errorf("description = %q, want %q", got, "reset password flow")
	}
}

// TestAddDescriptionInPushPayload verifies a description set via --desc reaches
// Toggl in the create payload on the best-effort push.
func TestAddDescriptionInPushPayload(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &body)
		w.Write([]byte(`{"id":9200,"at":"2026-01-02T09:00:00Z"}`))
	}))
	defer srv.Close()
	c := api.New("tok", api.WithBaseURL(srv.URL), api.WithHTTPClient(srv.Client()))

	var buf bytes.Buffer
	if err := cmdAdd(&buf, s, c, 1, nil, false, "9-:30", "login", "reset password flow", addNow, time.UTC); err != nil {
		t.Fatalf("add: %v", err)
	}
	if got, _ := body["description"].(string); got != "reset password flow" {
		t.Errorf("description = %v, want %q", body["description"], "reset password flow")
	}
}

// TestAddExactWins covers the shared fragment matching: an exact task title
// beats the longer titles it is a substring of.
func TestAddExactWins(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	var buf bytes.Buffer
	// "Fix" exactly matches task 11 even though it is a substring of others.
	if err := cmdAdd(&buf, s, nil, 1, nil, false, "9-10", "Fix", "", addNow, time.UTC); err != nil {
		t.Fatalf("add: %v", err)
	}
	entries, _ := s.EntriesBetween(addNow.Add(-24*time.Hour), addNow.Add(24*time.Hour))
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if entries[0].TaskID == nil || *entries[0].TaskID != 11 {
		t.Errorf("task_id = %v, want 11 (Fix)", entries[0].TaskID)
	}
}

// TestAddNonBillableProject verifies a task in a non-billable project (Backend,
// id 1) yields a non-billable entry (the billable counterpart is covered by
// TestAddProjectScopeViaEnvID).
func TestAddNonBillableProject(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	var buf bytes.Buffer
	if err := cmdAdd(&buf, s, nil, 1, nil, false, "9-10", "login", "", addNow, time.UTC); err != nil {
		t.Fatalf("add: %v", err)
	}
	entries, _ := s.EntriesBetween(addNow.Add(-24*time.Hour), addNow.Add(24*time.Hour))
	if len(entries) != 1 || entries[0].Billable {
		t.Fatalf("entries = %+v, want a single non-billable entry", entries)
	}
}

func TestAddKeepsRunningEntry(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	// A pulled running entry must survive an `add`. The added span sits before
	// the running entry began (09:00) so the overlap guard is happy: a running
	// entry occupies everything from its start onwards.
	seedRunning(t, s, 12, testStart)
	var buf bytes.Buffer
	if err := cmdAdd(&buf, s, nil, 1, nil, false, "7-8", "login", "", addNow, time.UTC); err != nil {
		t.Fatalf("add: %v", err)
	}
	r, _ := s.Running()
	if r == nil || r.TaskID == nil || *r.TaskID != 12 {
		t.Fatalf("running entry = %+v, want Code review still running", r)
	}
}

// TestAddRejectsOverlap covers the overlap guard: a span colliding with an
// already tracked entry is refused, the error names the existing entry, and
// nothing is written. Touching the neighbour's endpoints stays allowed.
func TestAddRejectsOverlap(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	var buf bytes.Buffer
	if err := cmdAdd(&buf, s, nil, 1, nil, false, "9-10", "login", "", addNow, time.UTC); err != nil {
		t.Fatalf("add first: %v", err)
	}

	// 09:30-10:30 straddles the 09:00-10:00 entry.
	buf.Reset()
	err := cmdAdd(&buf, s, nil, 1, nil, false, "9:30-10:30", "review", "", addNow, time.UTC)
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
	entries, _ := s.EntriesBetween(addNow.Add(-24*time.Hour), addNow.Add(24*time.Hour))
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}

	// A back-to-back entry starting exactly when the first ends is fine.
	buf.Reset()
	if err := cmdAdd(&buf, s, nil, 1, nil, false, "10-11", "review", "", addNow, time.UTC); err != nil {
		t.Fatalf("touching add: %v", err)
	}
	if entries, _ := s.EntriesBetween(addNow.Add(-24*time.Hour), addNow.Add(24*time.Hour)); len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
}

// TestAddRejectsOverlapWithRunningEntry checks a running entry blocks any span
// reaching past its start, since it is still accruing time.
func TestAddRejectsOverlapWithRunningEntry(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	seedRunning(t, s, 12, testStart)

	var buf bytes.Buffer
	err := cmdAdd(&buf, s, nil, 1, nil, false, "8:30-9:30", "login", "", addNow, time.UTC)
	if err == nil {
		t.Fatal("add over a running entry = nil error, want an error")
	}
	for _, want := range []string{"overlaps existing entry", "09:00-running", "Code review"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to mention %q", err, want)
		}
	}
	if entries, _ := s.EntriesBetween(addNow.Add(-24*time.Hour), addNow.Add(24*time.Hour)); len(entries) != 1 {
		t.Fatalf("entries = %d, want 1 (the running entry only)", len(entries))
	}
}

func TestAddProjectScopeViaEnvID(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	// "fix" matches several tasks, but scoping to project 2 leaves only one.
	pid := int64(2)
	var buf bytes.Buffer
	if err := cmdAdd(&buf, s, nil, 1, &pid, false, "10-11", "fix", "", addNow, time.UTC); err != nil {
		t.Fatalf("add: %v", err)
	}
	entries, _ := s.EntriesBetween(addNow.Add(-24*time.Hour), addNow.Add(24*time.Hour))
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	e := entries[0]
	if e.TaskID == nil || *e.TaskID != 20 {
		t.Errorf("task_id = %v, want 20 (Payment fix)", e.TaskID)
	}
	if e.ProjectID == nil || *e.ProjectID != 2 {
		t.Errorf("project_id = %v, want 2", e.ProjectID)
	}
	// The billable project carries its flag onto the entry.
	if !e.Billable {
		t.Error("entry in a billable project should be billable")
	}
}

func TestAddAmbiguous(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	var buf bytes.Buffer
	err := cmdAdd(&buf, s, nil, 1, nil, false, "10-11", "write", "", addNow, time.UTC)
	if err == nil {
		t.Fatal("expected ambiguity error")
	}
	if !strings.Contains(err.Error(), "Write tests") || !strings.Contains(err.Error(), "Write docs") {
		t.Errorf("error should list candidates: %v", err)
	}
	// Each candidate carries its project, so same-named tasks are told apart,
	// and the way out (`-1`) is advertised with the list.
	if !strings.Contains(err.Error(), "[Backend]") {
		t.Errorf("candidates should name their project: %v", err)
	}
	if !strings.Contains(err.Error(), "pass -1") {
		t.Errorf("error should point at -1: %v", err)
	}
	if entries, _ := s.EntriesBetween(addNow.Add(-24*time.Hour), addNow.Add(24*time.Hour)); len(entries) != 0 {
		t.Errorf("no entry should be created on ambiguity, got %d", len(entries))
	}
}

// TestAddAmbiguousFirstMatchWins is `-1`'s reason to exist: two tasks sharing a
// name in different projects cannot be told apart by any fragment, so without
// the flag `add` refuses to guess and with it the FIRST candidate — the one the
// ambiguity error lists first — is the task recorded against.
func TestAddAmbiguousFirstMatchWins(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)
	// The same task name in a second project; "Code review" is now an exact
	// match for two tasks at once.
	if err := s.UpsertTask(store.Task{
		ID: 22, WorkspaceID: 1, ProjectID: 2, Name: "Code review", Active: true,
	}); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	err := cmdAdd(&buf, s, nil, 1, nil, false, "10-11", "code review", "", addNow, time.UTC)
	if err == nil || !strings.Contains(err.Error(), "multiple tasks match") {
		t.Fatalf("err = %v, want an ambiguity error without -1", err)
	}

	// Candidates are ordered by name then id, so the first one is task 12.
	buf.Reset()
	if err := cmdAdd(&buf, s, nil, 1, nil, true, "10-11", "code review", "", addNow, time.UTC); err != nil {
		t.Fatalf("add -1: %v", err)
	}
	entries, _ := s.EntriesBetween(addNow.Add(-24*time.Hour), addNow.Add(24*time.Hour))
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

// TestAddFirstMatchIsNotNeededWhenUnique verifies `-1` is inert on a fragment
// that already resolves: it never changes which task a working command picks.
func TestAddFirstMatchIsNotNeededWhenUnique(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	var buf bytes.Buffer
	if err := cmdAdd(&buf, s, nil, 1, nil, true, "9-:30", "login", "", addNow, time.UTC); err != nil {
		t.Fatalf("add -1: %v", err)
	}
	entries, _ := s.EntriesBetween(addNow.Add(-24*time.Hour), addNow.Add(24*time.Hour))
	if len(entries) != 1 || entries[0].TaskID == nil || *entries[0].TaskID != 10 {
		t.Errorf("entries = %+v, want one entry on task 10", entries)
	}
}

// TestAddFirstMatchDoesNotInventAMatch verifies `-1` only resolves ambiguity:
// with nothing to choose from it still fails, pointing at `tg update`.
func TestAddFirstMatchDoesNotInventAMatch(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	var buf bytes.Buffer
	err := cmdAdd(&buf, s, nil, 1, nil, true, "10-11", "nonexistent", "", addNow, time.UTC)
	if err == nil || !strings.Contains(err.Error(), "tg update") {
		t.Errorf("err = %v, want the no-match error even with -1", err)
	}
}

// TestResolveTaskFragment pins the shared task-fragment resolver every
// fragment-taking command goes through: one match resolves, several fail with
// the candidate list unless `-1` takes the first, and none never resolves.
func TestResolveTaskFragment(t *testing.T) {
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
			got, err := resolveTaskFragment(s, tc.fragment, nil, tc.first)
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

func TestAddNoneSuggestsUpdate(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	var buf bytes.Buffer
	err := cmdAdd(&buf, s, nil, 1, nil, false, "10-11", "nonexistent", "", addNow, time.UTC)
	if err == nil || !strings.Contains(err.Error(), "tg update") {
		t.Errorf("err = %v, want suggestion to run `tg update`", err)
	}
}

func TestAddInvalidTimesign(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	var buf bytes.Buffer
	err := cmdAdd(&buf, s, nil, 1, nil, false, "nope", "login", "", addNow, time.UTC)
	if err == nil || !strings.Contains(err.Error(), "timesign") {
		t.Errorf("err = %v, want a timesign parse error", err)
	}
	// A bad timesign must be rejected before any task lookup or write.
	if entries, _ := s.EntriesBetween(addNow.Add(-24*time.Hour), addNow.Add(24*time.Hour)); len(entries) != 0 {
		t.Errorf("no entry should be created for a bad timesign, got %d", len(entries))
	}
}

func TestAddBestEffortPush(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	var body map[string]any
	var gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &body)
		w.Write([]byte(`{"id":9100,"at":"2026-01-02T09:00:00Z"}`))
	}))
	defer srv.Close()
	c := api.New("tok", api.WithBaseURL(srv.URL), api.WithHTTPClient(srv.Client()))

	var buf bytes.Buffer
	if err := cmdAdd(&buf, s, c, 1, nil, false, "9-:30", "login", "", addNow, time.UTC); err != nil {
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
	r, _ := s.EntryByRemoteID(9100)
	if r == nil {
		t.Fatal("expected the added entry to be synced with its remote id")
	}
	if r.Dirty {
		t.Error("added entry should be clean after a successful push")
	}
}

func TestAddSyncFailureIsNonFatal(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := api.New("tok", api.WithBaseURL(srv.URL), api.WithHTTPClient(srv.Client()))

	var buf bytes.Buffer
	if err := cmdAdd(&buf, s, c, 1, nil, false, "9-:30", "login", "", addNow, time.UTC); err != nil {
		t.Fatalf("add should not fail on a sync error: %v", err)
	}
	if !strings.Contains(buf.String(), "warning") {
		t.Errorf("output = %q, want a sync warning", buf.String())
	}
	entries, _ := s.EntriesBetween(addNow.Add(-24*time.Hour), addNow.Add(24*time.Hour))
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
	if _, err := s.CreateEntry(store.Entry{
		RemoteID: p(9001), WorkspaceID: 1, ProjectID: p(1), TaskID: p(10),
		Start: testStart, Stop: &stop1, Duration: 3600, UpdatedAt: stop1,
		SyncedAt: &stop1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateEntry(store.Entry{
		RemoteID: p(9002), WorkspaceID: 1, ProjectID: p(1), TaskID: p(12),
		Start: start2, Stop: &stop2, Duration: 3600, UpdatedAt: stop2,
		SyncedAt: &stop2,
	}); err != nil {
		t.Fatal(err)
	}
	entries, err := s.EntriesBetween(testStart.Add(-24*time.Hour), testStart.Add(24*time.Hour))
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
	dirty, err := s.DirtyEntries()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range dirty {
		if e.ID == id {
			return e
		}
	}
	entries, err := s.EntriesBetween(testStart.Add(-48*time.Hour), testStart.Add(48*time.Hour))
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
	s := newStore(t)
	seedCatalog(t, s)
	entries := seedModDay(t, s)
	last := entries[1] // 10:00-11:00 Code review

	var buf bytes.Buffer
	if err := cmdMod(&buf, s, nil, 0, "+:30", "", false, modNow, time.UTC); err != nil {
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
	s := newStore(t)
	seedCatalog(t, s)
	entries := seedModDay(t, s)
	last := entries[1] // 10:00-11:00 Code review

	var buf bytes.Buffer
	if err := cmdMod(&buf, s, nil, 0, "+20", "", false, modNow, time.UTC); err != nil {
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
	s := newStore(t)
	seedCatalog(t, s)
	entries := seedModDay(t, s)
	last := entries[1] // 10:00-11:00 Code review

	var buf bytes.Buffer
	if err := cmdMod(&buf, s, nil, 2, "+:30", "", false, modNow, time.UTC); err != nil {
		t.Fatalf("first mod: %v", err)
	}
	buf.Reset()
	if err := cmdMod(&buf, s, nil, 2, "+1:15", "", false, modNow, time.UTC); err != nil {
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

// TestModRelativeRefusesRunningEntry covers the one entry a relative timesign
// cannot act on: a running entry has no end to add to, so mod says so instead
// of inventing one from now. An absolute sign still gives it a finished span.
func TestModRelativeRefusesRunningEntry(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	start := time.Date(2026, 1, 2, 14, 0, 0, 0, time.UTC)
	id, err := s.CreateEntry(store.Entry{
		WorkspaceID: 1, ProjectID: p(1), TaskID: p(10),
		Start: start, Duration: -1, UpdatedAt: start,
	})
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	err = cmdMod(&buf, s, nil, 0, "+:30", "", false, modNow, time.UTC)
	if err == nil {
		t.Fatal("mod +:30 on a running entry = nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "running") {
		t.Errorf("err = %v, want it to say the entry is still running", err)
	}
	if buf.Len() != 0 {
		t.Errorf("output = %q, want nothing written", buf.String())
	}
	if got := entryByID(t, s, id); got.Stop != nil || got.Duration != -1 {
		t.Errorf("entry = %+v, want it left running", got)
	}

	// An absolute timesign is still accepted and closes the entry.
	buf.Reset()
	if err := cmdMod(&buf, s, nil, 0, "14-14:30", "", false, modNow, time.UTC); err != nil {
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
	s := newStore(t)
	seedCatalog(t, s)
	entries := seedModDay(t, s) // 09:00-10:00 and 10:00-11:00 on 2026-01-02

	future := modNow.Add(3 * time.Hour) // 18:07, same day
	futureStop := future.Add(time.Hour)
	futureID, err := s.CreateEntry(store.Entry{
		WorkspaceID: 1, ProjectID: p(1), TaskID: p(13),
		Start: future, Stop: &futureStop, Duration: 3600, UpdatedAt: modNow,
	})
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := cmdMod(&buf, s, nil, 0, "+:30", "", false, modNow, time.UTC); err != nil {
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
	s := newStore(t)
	seedCatalog(t, s)
	entries := seedModDay(t, s)

	var buf bytes.Buffer
	if err := cmdMod(&buf, s, nil, 1, "8-9:30", "", false, modNow, time.UTC); err != nil {
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
		err := cmdMod(&buf, s, c, tc.ref, tc.timesign, tc.desc, tc.setDesc, nextDay, time.UTC)
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
	s := newStore(t)
	seedCatalog(t, s)
	entries := seedModDay(t, s)

	nextDay := modNow.AddDate(0, 0, 1)
	var buf bytes.Buffer
	err := cmdMod(&buf, s, nil, 1, "+:30", "", false, nextDay, time.UTC)
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
	if err := cmdDel(&buf, s, nil, 1, nextDay, time.UTC); !errors.Is(err, store.ErrNoEntryNum) {
		t.Errorf("del err = %v, want store.ErrNoEntryNum", err)
	}
}

// TestModAllowsTodaysEntryAtDayEnd guards the boundary from the other side: an
// entry started earlier on the current day stays editable right up to midnight.
func TestModAllowsTodaysEntryAtDayEnd(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)
	entries := seedModDay(t, s)

	lateToday := time.Date(2026, 1, 2, 23, 59, 0, 0, time.UTC)
	var buf bytes.Buffer
	// Entry 2 (10:00-11:00) is the one with room to grow; entry 1 butts
	// straight into it (see TestModRejectsOverlap).
	if err := cmdMod(&buf, s, nil, 2, "+:30", "", false, lateToday, time.UTC); err != nil {
		t.Fatalf("mod at 23:59 on the entry's own day: %v", err)
	}
	if got := entryByID(t, s, entries[1].ID); got.Duration != 5400 {
		t.Errorf("duration = %d, want 5400", got.Duration)
	}
}

// TestModDescriptionOnly verifies --desc alone is a valid change and leaves the
// times exactly as they were.
func TestModDescriptionOnly(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)
	entries := seedModDay(t, s)
	target := entries[0]

	var buf bytes.Buffer
	if err := cmdMod(&buf, s, nil, 1, "", "rebased onto main", true, modNow, time.UTC); err != nil {
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
	if err := cmdMod(&buf, s, nil, 1, "", "", true, modNow, time.UTC); err != nil {
		t.Fatalf("mod --desc \"\": %v", err)
	}
	if got := entryByID(t, s, target.ID); got.Description != "" {
		t.Errorf("description = %q, want it cleared", got.Description)
	}
}

// TestModTimesignAndDescription verifies both changes can be applied at once.
func TestModTimesignAndDescription(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)
	entries := seedModDay(t, s)

	var buf bytes.Buffer
	if err := cmdMod(&buf, s, nil, 2, "+:45", "pairing", true, modNow, time.UTC); err != nil {
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

// TestModRequiresAChange keeps a no-op invocation a usage error instead of a
// silent dirty write.
func TestModRequiresAChange(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)
	entries := seedModDay(t, s)

	var buf bytes.Buffer
	err := cmdMod(&buf, s, nil, 1, "", "", false, modNow, time.UTC)
	if err == nil {
		t.Fatal("mod with no changes = nil error, want a usage error")
	}
	if !strings.Contains(err.Error(), "usage: tg mod") {
		t.Errorf("err = %v, want a usage message", err)
	}
	if got := entryByID(t, s, entries[0].ID); got.Dirty {
		t.Error("a rejected mod must not mark the entry dirty")
	}
}

// TestModRejectsOverlap covers the overlap guard: growing an entry into its
// neighbour is refused, the error names the neighbour, and nothing is written.
// Growing it up to the neighbour's start stays allowed (half-open intervals),
// which also proves the entry is not compared against itself.
func TestModRejectsOverlap(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)
	entries := seedModDay(t, s)

	// Entry 1 is 09:00-10:00 and entry 2 is 10:00-11:00, so pushing entry 1's
	// end 90 minutes later would run into entry 2.
	var buf bytes.Buffer
	err := cmdMod(&buf, s, nil, 1, "+1:30", "", false, modNow, time.UTC)
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
	if err := cmdMod(&buf, s, nil, 1, "9-9:30", "", false, modNow, time.UTC); err != nil {
		t.Fatalf("shrinking mod: %v", err)
	}

	// Extending it exactly up to the neighbour's start is allowed: the
	// intervals are half-open, so 09:00-10:00 and 10:00-11:00 do not overlap.
	buf.Reset()
	if err := cmdMod(&buf, s, nil, 1, "+:30", "", false, modNow, time.UTC); err != nil {
		t.Fatalf("touching mod: %v", err)
	}
	if got := entryByID(t, s, entries[0].ID); got.Duration != 3600 {
		t.Errorf("duration = %d, want 3600 (grown back up to 10:00)", got.Duration)
	}

	// One more minute would collide, and a refused extension leaves the entry
	// exactly as it was.
	buf.Reset()
	if err := cmdMod(&buf, s, nil, 1, "+:01", "", false, modNow, time.UTC); err == nil {
		t.Error("extending past the neighbour's start = nil error, want an overlap error")
	}
	if got := entryByID(t, s, entries[0].ID); got.Duration != 3600 {
		t.Errorf("duration = %d, want the 3600 from the previous edit", got.Duration)
	}
}

// TestModStaleNumber verifies a number today's numbering never handed out is
// reported as such (wrapping store.ErrNoEntryNum) rather than silently
// modifying something else.
func TestModStaleNumber(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)
	seedModDay(t, s)

	var buf bytes.Buffer
	err := cmdMod(&buf, s, nil, 7, "+:30", "", false, modNow, time.UTC)
	if !errors.Is(err, store.ErrNoEntryNum) {
		t.Fatalf("err = %v, want ErrNoEntryNum", err)
	}
	if !strings.Contains(err.Error(), "tg ls") {
		t.Errorf("err = %v, want it to suggest `tg ls`", err)
	}
	if buf.Len() != 0 {
		t.Errorf("output = %q, want nothing written", buf.String())
	}
}

// TestModNoEntries verifies the default target is an error, not a panic, in an
// empty store.
func TestModNoEntries(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	var buf bytes.Buffer
	if err := cmdMod(&buf, s, nil, 0, "+:30", "", false, modNow, time.UTC); err == nil {
		t.Fatal("mod on an empty store = nil error, want an error")
	}
}

// TestModInvalidTimesign keeps a malformed timesign from touching the store.
func TestModInvalidTimesign(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)
	entries := seedModDay(t, s)

	for _, sign := range []string{"+:00", "10-9", "nonsense"} {
		var buf bytes.Buffer
		if err := cmdMod(&buf, s, nil, 1, sign, "", false, modNow, time.UTC); err == nil {
			t.Errorf("mod %q = nil error, want an error", sign)
		}
	}
	if got := entryByID(t, s, entries[0].ID); got.Dirty {
		t.Error("a rejected mod must not mark the entry dirty")
	}
}

// TestModPushesBestEffort verifies a successful mod pushes the change straight
// to Toggl as an update (the entry keeps its remote id) and comes back clean.
func TestModPushesBestEffort(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)
	entries := seedModDay(t, s)

	var method, path string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &body)
		w.Write([]byte(`{"id":9002,"at":"2026-01-02T15:07:00Z"}`))
	}))
	defer srv.Close()
	c := api.New("tok", api.WithBaseURL(srv.URL), api.WithHTTPClient(srv.Client()))

	var buf bytes.Buffer
	if err := cmdMod(&buf, s, c, 2, "+:30", "", false, modNow, time.UTC); err != nil {
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
	s := newStore(t)
	seedCatalog(t, s)
	entries := seedModDay(t, s)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := api.New("tok", api.WithBaseURL(srv.URL), api.WithHTTPClient(srv.Client()))

	var buf bytes.Buffer
	if err := cmdMod(&buf, s, c, 2, "+:30", "", false, modNow, time.UTC); err != nil {
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
	s := newStore(t)
	seedCatalog(t, s)
	entries := seedModDay(t, s)

	var buf bytes.Buffer
	if err := cmdDel(&buf, s, nil, 1, modNow, time.UTC); err != nil {
		t.Fatalf("del: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"Deleted: Fix login bug", "09:00-10:00", "1h00m"} {
		if !strings.Contains(out, want) {
			t.Errorf("output = %q, want %q", out, want)
		}
	}

	left, _ := s.EntriesBetween(testStart.Add(-24*time.Hour), testStart.Add(24*time.Hour))
	if len(left) != 1 || left[0].ID != entries[1].ID {
		t.Fatalf("remaining entries = %+v, want only entry 2", left)
	}
	// The number now resolves to nothing rather than to another entry: entry 2
	// keeps being entry 2 instead of sliding into the freed slot.
	if _, err := s.EntryByNum(1, modNow); !errors.Is(err, store.ErrNoEntryNum) {
		t.Errorf("EntryByNum(1) after del = %v, want ErrNoEntryNum", err)
	}
	if got, err := s.EntryByNum(2, modNow); err != nil || got.ID != entries[1].ID {
		t.Errorf("EntryByNum(2) after del = %+v err=%v, want the surviving entry %d",
			got, err, entries[1].ID)
	}
}

// TestDelMarksDeletedAndDirty pins the soft delete: the row survives, flagged
// deleted and dirty with a fresh LWW clock, so sync.Push can DELETE it remotely
// before dropping it.
func TestDelMarksDeletedAndDirty(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)
	entries := seedModDay(t, s)

	var buf bytes.Buffer
	if err := cmdDel(&buf, s, nil, 2, modNow, time.UTC); err != nil {
		t.Fatalf("del: %v", err)
	}
	dirty, err := s.DirtyEntries()
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
	s := newStore(t)
	seedCatalog(t, s)
	seedModDay(t, s)

	var buf bytes.Buffer
	err := cmdDel(&buf, s, nil, 9, modNow, time.UTC)
	if !errors.Is(err, store.ErrNoEntryNum) {
		t.Fatalf("err = %v, want ErrNoEntryNum", err)
	}
	if !strings.Contains(err.Error(), "tg ls") {
		t.Errorf("err = %v, want it to suggest `tg ls`", err)
	}
	if buf.Len() != 0 {
		t.Errorf("output = %q, want nothing written", buf.String())
	}
	if left, _ := s.EntriesBetween(testStart.Add(-24*time.Hour), testStart.Add(24*time.Hour)); len(left) != 2 {
		t.Errorf("entries = %d, want both kept", len(left))
	}

	// Deleting the same entry twice hits the same path: the first delete
	// retires the number, and it is not handed to the surviving entry.
	if err := cmdDel(&buf, s, nil, 1, modNow, time.UTC); err != nil {
		t.Fatalf("del: %v", err)
	}
	if err := cmdDel(&buf, s, nil, 1, modNow, time.UTC); !errors.Is(err, store.ErrNoEntryNum) {
		t.Errorf("second del error = %v, want ErrNoEntryNum", err)
	}
}

// TestDelRequiresNumber verifies del never guesses a target.
func TestDelRequiresNumber(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)
	seedModDay(t, s)

	var buf bytes.Buffer
	err := cmdDel(&buf, s, nil, 0, modNow, time.UTC)
	if err == nil || !strings.Contains(err.Error(), "usage: tg del") {
		t.Fatalf("err = %v, want a usage error", err)
	}
}

// TestDelPushesBestEffort verifies a successful del DELETEs the entry remotely
// and drops the local row.
func TestDelPushesBestEffort(t *testing.T) {
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
	if err := cmdDel(&buf, s, c, 1, modNow, time.UTC); err != nil {
		t.Fatalf("del: %v", err)
	}
	if method != http.MethodDelete {
		t.Errorf("method = %s %s, want DELETE", method, path)
	}
	if !strings.Contains(path, "9001") {
		t.Errorf("path = %s, want the entry's remote id 9001", path)
	}
	if dirty, _ := s.DirtyEntries(); len(dirty) != 0 {
		t.Errorf("dirty entries = %+v, want the row dropped after the remote delete", dirty)
	}
}

// --- argument parsing --------------------------------------------------------

// TestParseModArgs covers `tg mod`'s positional disambiguation: bare digits are
// the entry number, everything else is the timesign, in either order.
func TestParseModArgs(t *testing.T) {
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
		{name: "two numbers", args: []string{"2", "3"}, wantErr: true},
		{name: "two timesigns", args: []string{"+:30", "9-10"}, wantErr: true},
		{name: "zero is not a number", args: []string{"0"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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
			fs := newFlagSet("mod")
			fs.SetOutput(io.Discard)
			var desc string
			fs.StringVar(&desc, "desc", "", "")
			fs.StringVar(&desc, "description", "", "")
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
			set := false
			fs.Visit(func(*flag.Flag) { set = true })
			if set != tt.wantSet {
				t.Errorf("flag set = %v, want %v", set, tt.wantSet)
			}
		})
	}
}

// TestFirstFlagParsing pins the wiring of the shared `-1` flag (bindFirstFlag):
// a bare "-1" is parsed as the boolean rather than swallowed as a positional or
// rejected as an unknown flag, `--first` is the same flag, and (through
// parseArgsAndFlags) either spelling may sit before, between or after the
// positionals that make up the fragment. Anything else that looks like a
// number-flag is still an error, so a typo is never taken for a fragment.
func TestFirstFlagParsing(t *testing.T) {
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

func TestTasksCommand(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	var buf bytes.Buffer
	if err := cmdTasks(&buf, s, false, nil, false); err != nil {
		t.Fatalf("tasks: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"Fix login bug", "Code review", "Payment fix", "[Backend]", "[Payments]"} {
		if !strings.Contains(out, want) {
			t.Errorf("tasks output missing %q:\n%s", want, out)
		}
	}
}

func TestTasksCommandAllIncludesInactive(t *testing.T) {
	s := newStore(t)
	if err := s.ReplaceProjects([]store.Project{{ID: 1, WorkspaceID: 1, Name: "Backend", Active: true}}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceTasks([]store.Task{
		{ID: 10, WorkspaceID: 1, ProjectID: 1, Name: "Active task", Active: true},
		{ID: 11, WorkspaceID: 1, ProjectID: 1, Name: "Retired task", Active: false},
	}); err != nil {
		t.Fatal(err)
	}

	var active bytes.Buffer
	if err := cmdTasks(&active, s, false, nil, false); err != nil {
		t.Fatalf("tasks: %v", err)
	}
	if strings.Contains(active.String(), "Retired task") {
		t.Errorf("active-only listing should hide inactive tasks:\n%s", active.String())
	}

	var all bytes.Buffer
	if err := cmdTasks(&all, s, true, nil, false); err != nil {
		t.Fatalf("tasks --all: %v", err)
	}
	if !strings.Contains(all.String(), "Retired task") {
		t.Errorf("--all listing should include inactive tasks:\n%s", all.String())
	}
}

func TestTasksCommandProjectScope(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	pid := int64(2) // Payments
	var buf bytes.Buffer
	if err := cmdTasks(&buf, s, false, &pid, false); err != nil {
		t.Fatalf("tasks: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Payment fix") {
		t.Errorf("scoped tasks should list Payment fix:\n%s", out)
	}
	for _, hidden := range []string{"Fix login bug", "Code review", "Write tests"} {
		if strings.Contains(out, hidden) {
			t.Errorf("scoped tasks should hide %q:\n%s", hidden, out)
		}
	}
}

func TestGrepListsMatches(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	var buf bytes.Buffer
	if err := cmdGrep(&buf, s, false, nil, false, "write", false); err != nil {
		t.Fatalf("grep: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"Write docs", "Write tests", "[Backend]"} {
		if !strings.Contains(out, want) {
			t.Errorf("grep output missing %q:\n%s", want, out)
		}
	}
	for _, hidden := range []string{"Code review", "Payment fix"} {
		if strings.Contains(out, hidden) {
			t.Errorf("grep output should not list %q:\n%s", hidden, out)
		}
	}
}

func TestGrepCaseInsensitive(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	var buf bytes.Buffer
	if err := cmdGrep(&buf, s, false, nil, false, "CODE REVIEW", false); err != nil {
		t.Fatalf("grep: %v", err)
	}
	if !strings.Contains(buf.String(), "Code review") {
		t.Errorf("grep should match case-insensitively:\n%s", buf.String())
	}
}

// TestGrepExactDoesNotWin pins grep's key difference from `add`/`total`
// matching: an exact name match must not suppress the other substring matches,
// since grep exists to show every candidate.
func TestGrepExactDoesNotWin(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	var buf bytes.Buffer
	// "Fix" is an exact task name in the catalog; "Fix login bug" and
	// "Payment fix" merely contain it, and all three must be listed.
	if err := cmdGrep(&buf, s, false, nil, false, "fix", false); err != nil {
		t.Fatalf("grep: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"Fix login bug", "Payment fix", "[Payments]"} {
		if !strings.Contains(out, want) {
			t.Errorf("grep output missing %q:\n%s", want, out)
		}
	}
	if lines := strings.Count(strings.TrimSpace(out), "\n") + 1; lines != 3 {
		t.Errorf("grep listed %d lines, want 3:\n%s", lines, out)
	}
}

// TestGrepFirstListsOneMatch verifies `-1` cuts grep down to its first
// candidate, which is how the fragment a user is about to pass to `tg add -1`
// is checked. The order is grep's own (catalog order), so "Write docs" leads.
func TestGrepFirstListsOneMatch(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	var buf bytes.Buffer
	if err := cmdGrep(&buf, s, false, nil, true, "write", false); err != nil {
		t.Fatalf("grep -1: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Write docs") {
		t.Errorf("grep -1 should list the first match:\n%s", out)
	}
	if strings.Contains(out, "Write tests") {
		t.Errorf("grep -1 should list only one match:\n%s", out)
	}
	if lines := strings.Count(strings.TrimSpace(out), "\n") + 1; lines != 1 {
		t.Errorf("grep -1 listed %d lines, want 1:\n%s", lines, out)
	}
}

// TestGrepFirstStillFailsWithoutAMatch verifies `-1` does not turn an empty
// result into a success (there is no first candidate to take).
func TestGrepFirstStillFailsWithoutAMatch(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	var buf bytes.Buffer
	err := cmdGrep(&buf, s, false, nil, true, "nothing here", false)
	if err == nil || !strings.Contains(err.Error(), "no task matches") {
		t.Errorf("err = %v, want a no-match error even with -1", err)
	}
}

func TestGrepJoinsFragment(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	var buf bytes.Buffer
	if err := cmdGrep(&buf, s, false, nil, false, "code review", false); err != nil {
		t.Fatalf("grep: %v", err)
	}
	if !strings.Contains(buf.String(), "Code review") {
		t.Errorf("grep should match a multi-word fragment:\n%s", buf.String())
	}
}

func TestGrepProjectScope(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	pid := int64(2) // Payments
	var buf bytes.Buffer
	if err := cmdGrep(&buf, s, false, &pid, false, "fix", false); err != nil {
		t.Fatalf("grep: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Payment fix") {
		t.Errorf("scoped grep should list Payment fix:\n%s", out)
	}
	if strings.Contains(out, "Fix login bug") {
		t.Errorf("scoped grep should hide other projects' tasks:\n%s", out)
	}
}

func TestGrepAllIncludesInactive(t *testing.T) {
	s := newStore(t)
	if err := s.ReplaceProjects([]store.Project{{ID: 1, WorkspaceID: 1, Name: "Backend", Active: true}}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceTasks([]store.Task{
		{ID: 10, WorkspaceID: 1, ProjectID: 1, Name: "Active task", Active: true},
		{ID: 11, WorkspaceID: 1, ProjectID: 1, Name: "Retired task", Active: false},
	}); err != nil {
		t.Fatal(err)
	}

	var active bytes.Buffer
	if err := cmdGrep(&active, s, false, nil, false, "task", false); err != nil {
		t.Fatalf("grep: %v", err)
	}
	if strings.Contains(active.String(), "Retired task") {
		t.Errorf("active-only grep should hide inactive tasks:\n%s", active.String())
	}

	var all bytes.Buffer
	if err := cmdGrep(&all, s, true, nil, false, "task", false); err != nil {
		t.Fatalf("grep --all: %v", err)
	}
	if !strings.Contains(all.String(), "Retired task") {
		t.Errorf("--all grep should include inactive tasks:\n%s", all.String())
	}
}

func TestGrepNoMatch(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	var buf bytes.Buffer
	err := cmdGrep(&buf, s, false, nil, false, "nothing here", false)
	if err == nil {
		t.Fatal("expected an error when nothing matches")
	}
	if !strings.Contains(err.Error(), "no task matches") || !strings.Contains(err.Error(), "tg update") {
		t.Errorf("err = %v, want a no-match error suggesting `tg update`", err)
	}
	if buf.Len() != 0 {
		t.Errorf("grep should print nothing when it fails:\n%s", buf.String())
	}
}

func TestGrepRequiresFragment(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	// An empty fragment is a usage error, not "list everything": that is
	// `tg tasks`.
	for _, frag := range []string{"", "   "} {
		var buf bytes.Buffer
		err := cmdGrep(&buf, s, false, nil, false, frag, false)
		if err == nil || !strings.Contains(err.Error(), "usage: tg grep") {
			t.Errorf("grep %q: err = %v, want a usage error", frag, err)
		}
	}
}

func TestGrepJSON(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	var buf bytes.Buffer
	if err := cmdGrep(&buf, s, false, nil, false, "write", true); err != nil {
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

func TestResolvePullProjectRequiresFragment(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	// pull ignores TOGGL_PROJECT_ID, so a blank argument is a hard error that
	// must NOT suggest the env var as a fallback.
	_, err := resolvePullProject(s, "  ", false)
	if err == nil || !strings.Contains(err.Error(), "project-name argument") {
		t.Errorf("err = %v, want a required-argument error", err)
	}
	if strings.Contains(err.Error(), "TOGGL_PROJECT_ID") {
		t.Errorf("err = %v, should not mention TOGGL_PROJECT_ID (pull ignores it)", err)
	}
}

func TestResolvePullProjectUnique(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	got, err := resolvePullProject(s, "back", false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got == nil || *got != 1 {
		t.Errorf("resolved = %v, want project 1 (Backend)", got)
	}
}

func TestResolvePullProjectAmbiguous(t *testing.T) {
	s := newStore(t)
	if err := s.ReplaceProjects([]store.Project{
		{ID: 1, WorkspaceID: 1, Name: "Backend", Active: true},
		{ID: 2, WorkspaceID: 1, Name: "Back office", Active: true},
	}); err != nil {
		t.Fatal(err)
	}

	_, err := resolvePullProject(s, "back", false)
	if err == nil {
		t.Fatal("expected ambiguity error")
	}
	if !strings.Contains(err.Error(), "Backend") || !strings.Contains(err.Error(), "Back office") {
		t.Errorf("error should list candidates: %v", err)
	}
	if !strings.Contains(err.Error(), "pass -1") {
		t.Errorf("error should point at -1: %v", err)
	}

	// With -1 the first candidate is taken instead: candidates are ordered by
	// name then id, so "Back office" (2) wins over "Backend" (1).
	got, err := resolvePullProject(s, "back", true)
	if err != nil {
		t.Fatalf("resolve -1: %v", err)
	}
	if got == nil || *got != 2 {
		t.Errorf("resolved = %v, want 2 (Back office, the first candidate)", got)
	}
}

// TestResolvePullProjectFirstNeedsAMatch verifies `-1` only resolves ambiguity:
// a fragment matching nothing, and a missing fragment, still fail.
func TestResolvePullProjectFirstNeedsAMatch(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	if _, err := resolvePullProject(s, "nonexistent", true); err == nil {
		t.Error("expected a no-match error even with -1")
	}
	if _, err := resolvePullProject(s, "  ", true); err == nil {
		t.Error("expected a required-argument error even with -1")
	}
}

func TestResolvePullProjectNone(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	_, err := resolvePullProject(s, "nonexistent", false)
	if err == nil || !strings.Contains(err.Error(), "tg update") {
		t.Errorf("err = %v, want suggestion to run `tg update`", err)
	}
}

func TestResolvePullScopeUnscopedMeansAll(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	// A blank argument means "pull every project": nil scope.
	got, err := resolvePullScope(s, "   ", false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != nil {
		t.Errorf("resolved = %v, want nil (pull all projects)", got)
	}
}

// TestResolvePullScopeIgnoresEnv verifies pull's scope resolution never falls
// back to TOGGL_PROJECT_ID: with the env set but no argument, the scope is nil
// (all projects), unlike the env-honoring resolvers for start/update.
func TestResolvePullScopeIgnoresEnv(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	t.Setenv("TOGGL_PROJECT_ID", "2")
	got, err := resolvePullScope(s, "", false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != nil {
		t.Errorf("resolved = %v, want nil (pull ignores TOGGL_PROJECT_ID)", got)
	}
}

func TestResolvePullScopeFragment(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	got, err := resolvePullScope(s, "pay", false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got == nil || *got != 2 {
		t.Errorf("resolved = %v, want 2 (Payments)", got)
	}
}

// TestPullAllProjectsUnscoped verifies `tg pull` with no project scope
// reconciles entries from every project in one pass and advances last_pull.
func TestPullAllProjectsUnscoped(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[
		  {"id":1,"workspace_id":1,"project_id":1,"description":"a",
		   "start":"2026-01-02T09:00:00Z","stop":"2026-01-02T09:30:00Z",
		   "duration":1800,"at":"2026-01-02T09:30:00Z"},
		  {"id":2,"workspace_id":1,"project_id":2,"description":"b",
		   "start":"2026-01-02T10:00:00Z","stop":"2026-01-02T10:30:00Z",
		   "duration":1800,"at":"2026-01-02T10:30:00Z"}]`))
	}))
	defer srv.Close()
	c := api.New("tok", api.WithBaseURL(srv.URL), api.WithHTTPClient(srv.Client()))

	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	var buf bytes.Buffer
	// empty argument => pull every project.
	if err := cmdPull(&buf, s, c, false, "", since, now, false); err != nil {
		t.Fatalf("pull: %v", err)
	}
	if !strings.Contains(buf.String(), "2 inserted") {
		t.Errorf("output = %q, want 2 inserted", buf.String())
	}
	if got, _ := s.EntryByRemoteID(1); got == nil {
		t.Error("project 1 entry should be inserted")
	}
	if got, _ := s.EntryByRemoteID(2); got == nil {
		t.Error("project 2 entry should be inserted")
	}
	// A full (unscoped) pull advances the watermark.
	if _, ok, _ := s.GetMeta(store.MetaLastPull); !ok {
		t.Error("unscoped pull should advance last_pull")
	}
}

// TestPullScopedByFragment verifies a fragment still scopes the pull to one
// project (backwards compatible) and leaves the watermark untouched.
func TestPullScopedByFragment(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[
		  {"id":1,"workspace_id":1,"project_id":1,"description":"backend",
		   "start":"2026-01-02T09:00:00Z","stop":"2026-01-02T09:30:00Z",
		   "duration":1800,"at":"2026-01-02T09:30:00Z"},
		  {"id":2,"workspace_id":1,"project_id":2,"description":"payments",
		   "start":"2026-01-02T10:00:00Z","stop":"2026-01-02T10:30:00Z",
		   "duration":1800,"at":"2026-01-02T10:30:00Z"}]`))
	}))
	defer srv.Close()
	c := api.New("tok", api.WithBaseURL(srv.URL), api.WithHTTPClient(srv.Client()))

	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	var buf bytes.Buffer
	// "back" resolves to Backend (project 1); only its entry is reconciled.
	if err := cmdPull(&buf, s, c, false, "back", since, now, false); err != nil {
		t.Fatalf("pull: %v", err)
	}
	if got, _ := s.EntryByRemoteID(1); got == nil {
		t.Error("backend entry should be inserted")
	}
	if got, _ := s.EntryByRemoteID(2); got != nil {
		t.Error("payments entry should be ignored under a backend-scoped pull")
	}
	// A scoped pull is partial and must not advance the watermark.
	if _, ok, _ := s.GetMeta(store.MetaLastPull); ok {
		t.Error("scoped pull should not advance last_pull")
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
		w.Write([]byte(`[
		  {"id":1,"workspace_id":1,"project_id":1,"description":"a",
		   "start":"2026-01-02T09:00:00Z","stop":"2026-01-02T09:30:00Z",
		   "duration":1800,"at":"2026-01-02T09:30:00Z"},
		  {"id":2,"workspace_id":1,"project_id":2,"description":"b",
		   "start":"2026-01-02T10:00:00Z","stop":"2026-01-02T10:30:00Z",
		   "duration":1800,"at":"2026-01-02T10:30:00Z"}]`))
	}))
	defer srv.Close()
	c := api.New("tok", api.WithBaseURL(srv.URL), api.WithHTTPClient(srv.Client()))

	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	var buf bytes.Buffer
	if err := cmdPull(&buf, s, c, false, "", since, now, false); err != nil {
		t.Fatalf("pull: %v", err)
	}
	if !strings.Contains(buf.String(), "2 inserted") {
		t.Errorf("output = %q, want 2 inserted (all projects)", buf.String())
	}
	// The env project's entry is pulled...
	if got, _ := s.EntryByRemoteID(1); got == nil {
		t.Error("project 1 entry should be inserted")
	}
	// ...and so is the entry for a project other than TOGGL_PROJECT_ID.
	if got, _ := s.EntryByRemoteID(2); got == nil {
		t.Error("project 2 entry should be inserted despite TOGGL_PROJECT_ID=1")
	}
	// Ignoring the env means this is a full pull: the watermark advances.
	if _, ok, _ := s.GetMeta(store.MetaLastPull); !ok {
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
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &body)
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

// TestTotalMatchesLocalCatalogByID is the regression test for `tg total`
// matching nothing: the fragment is matched against the LOCAL catalog and the
// summary rows are joined to it by task id, so a titleless report still yields
// a named, project-tagged line. It also checks the default 3-month range.
func TestTotalMatchesLocalCatalogByID(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)
	c, body := totalReportsServer(t)

	var buf bytes.Buffer
	if err := cmdTotal(&buf, s, c, 1, false, "login", totalSince, totalNow, time.UTC, false); err != nil {
		t.Fatalf("total: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"Fix login bug", "1h15m", "[Backend]", "Total: 1h15m"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// Only the matched task is listed.
	for _, unwanted := range []string{"Code review", "Write tests", "Write docs", "task #99"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("output should only list the matched task, has %q:\n%s", unwanted, out)
		}
	}
	// The default 3-month start date and today's end date are sent.
	if (*body)["start_date"] != "2025-10-02" {
		t.Errorf("start_date = %v, want 2025-10-02", (*body)["start_date"])
	}
	if (*body)["end_date"] != "2026-01-02" {
		t.Errorf("end_date = %v, want 2026-01-02", (*body)["end_date"])
	}
}

// TestTotalJoinsFragment verifies the positionals form ONE fragment like
// `tg add` does: "write docs" is a single search, not two.
func TestTotalJoinsFragment(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)
	c, _ := totalReportsServer(t)

	var buf bytes.Buffer
	// runTotal joins the positionals with a space before calling cmdTotal.
	if err := cmdTotal(&buf, s, c, 1, false, strings.Join([]string{"write", "docs"}, " "), totalSince, totalNow, time.UTC, false); err != nil {
		t.Fatalf("total: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Write docs") || !strings.Contains(out, "Total: 0h15m") {
		t.Errorf("joined fragment should match only Write docs:\n%s", out)
	}
	if strings.Contains(out, "Write tests") {
		t.Errorf("joined fragment must not be treated as two searches:\n%s", out)
	}
}

// TestTotalSinceOverridesStart verifies an explicit since date sets the Reports
// API start_date instead of the default 3-month window, while end_date stays
// today.
func TestTotalSinceOverridesStart(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)
	c, body := totalReportsServer(t)

	since := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	var buf bytes.Buffer
	if err := cmdTotal(&buf, s, c, 1, false, "login", since, totalNow, time.UTC, false); err != nil {
		t.Fatalf("total: %v", err)
	}
	if (*body)["start_date"] != "2025-01-01" {
		t.Errorf("start_date = %v, want 2025-01-01", (*body)["start_date"])
	}
	if (*body)["end_date"] != "2026-01-02" {
		t.Errorf("end_date = %v, want 2026-01-02", (*body)["end_date"])
	}
}

// TestTotalFragmentMatchingMany verifies one fragment can match several tasks
// (the store's substring semantics), all of which are listed and summed.
func TestTotalFragmentMatchingMany(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)
	c, _ := totalReportsServer(t)

	var buf bytes.Buffer
	if err := cmdTotal(&buf, s, c, 1, false, "write", totalSince, totalNow, time.UTC, false); err != nil {
		t.Fatalf("total: %v", err)
	}
	out := buf.String()
	// "write" matches both Write tests (1h00m) and Write docs (0h15m).
	for _, want := range []string{"Write tests", "1h00m", "Write docs", "0h15m", "Total: 1h15m"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// TestTotalFirstMatchOnly verifies `-1` narrows a several-match fragment to its
// first candidate — the same one `tg add -1` would record against — so only that
// task's time is reported.
func TestTotalFirstMatchOnly(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)
	c, _ := totalReportsServer(t)

	var buf bytes.Buffer
	if err := cmdTotal(&buf, s, c, 1, true, "write", totalSince, totalNow, time.UTC, false); err != nil {
		t.Fatalf("total -1: %v", err)
	}
	out := buf.String()
	// Candidates are ordered by name, so "Write docs" (0h15m) is the first.
	for _, want := range []string{"Write docs", "0h15m", "Total: 0h15m"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Write tests") {
		t.Errorf("total -1 should report only the first match:\n%s", out)
	}
}

// TestTotalExactNameWins verifies the store's exact-match-wins rule reaches
// `total`: "fix" is a full task name (task 11) as well as a substring of
// "Fix login bug", and only the exact one is considered.
func TestTotalExactNameWins(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)
	c, _ := totalReportsServer(t)

	var buf bytes.Buffer
	// Task 11 ("Fix") has no tracked time in the report, so the exact match
	// winning is what makes this an empty result rather than "Fix login bug".
	err := cmdTotal(&buf, s, c, 1, false, "fix", totalSince, totalNow, time.UTC, false)
	if err == nil || !strings.Contains(err.Error(), "no tracked time") {
		t.Errorf("err = %v, want a no-tracked-time error for the exact match", err)
	}
}

// TestTotalNoFragmentListsAll verifies an empty fragment lists every task with
// tracked time, including rows whose task id is missing from the local catalog:
// those keep the API's title when it sent one, else fall back to `task #<id>`.
func TestTotalNoFragmentListsAll(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)
	c, _ := totalReportsServer(t)

	var buf bytes.Buffer
	if err := cmdTotal(&buf, s, c, 1, false, "", totalSince, totalNow, time.UTC, false); err != nil {
		t.Fatalf("total: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"Fix login bug", "Code review", "Write tests", "Write docs",
		"Legacy work", // uncatalogued but titled by the API
		"task #99",    // uncatalogued and untitled
		"Total: 3h45m",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// TestTotalUncataloguedNotMatchable verifies a row that only the API named
// cannot be reached by a fragment: matching is catalog-only.
func TestTotalUncataloguedNotMatchable(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)
	c, _ := totalReportsServer(t)

	var buf bytes.Buffer
	err := cmdTotal(&buf, s, c, 1, false, "legacy", totalSince, totalNow, time.UTC, false)
	if err == nil || !strings.Contains(err.Error(), "no task matches") {
		t.Errorf("err = %v, want a no-match error for an API-only title", err)
	}
}

func TestTotalNoMatches(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)
	c, _ := totalReportsServer(t)

	var buf bytes.Buffer
	err := cmdTotal(&buf, s, c, 1, false, "nonexistent", totalSince, totalNow, time.UTC, false)
	if err == nil || !strings.Contains(err.Error(), "no task matches") {
		t.Errorf("err = %v, want a no-match error", err)
	}
	if buf.Len() != 0 {
		t.Errorf("nothing should be written on no match, got %q", buf.String())
	}
}

// TestTotalMatchedButUntracked verifies a task that exists locally but has no
// tracked time in the range reports that, rather than a bogus no-match.
func TestTotalMatchedButUntracked(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)
	c, _ := totalReportsServer(t)

	var buf bytes.Buffer
	// "payment" matches task 20, which the report does not mention.
	err := cmdTotal(&buf, s, c, 1, false, "payment", totalSince, totalNow, time.UTC, false)
	if err == nil || !strings.Contains(err.Error(), "no tracked time") {
		t.Errorf("err = %v, want a no-tracked-time error", err)
	}
}

func TestTotalJSON(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)
	c, _ := totalReportsServer(t)

	var buf bytes.Buffer
	if err := cmdTotal(&buf, s, c, 1, false, "write", totalSince, totalNow, time.UTC, true); err != nil {
		t.Fatalf("total --json: %v", err)
	}
	var got struct {
		Tasks []struct {
			Task            string `json:"task"`
			Project         string `json:"project"`
			DurationSeconds int64  `json:"duration_seconds"`
		} `json:"tasks"`
		TotalSeconds int64 `json:"total_seconds"`
	}
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
}

// TestResolveTotalSinceDefault verifies the default window starts three
// calendar months before now when no --since is given.
func TestResolveTotalSinceDefault(t *testing.T) {
	got, err := resolveTotalSince("", totalNow, time.UTC)
	if err != nil {
		t.Fatalf("resolveTotalSince: %v", err)
	}
	want := totalNow.AddDate(0, -3, 0)
	if !got.Equal(want) {
		t.Errorf("since = %v, want %v (3 months before now)", got, want)
	}
}

// TestResolveTotalSinceOverride verifies an explicit --since date is parsed in
// the given location.
func TestResolveTotalSinceOverride(t *testing.T) {
	got, err := resolveTotalSince("2025-01-01", totalNow, time.UTC)
	if err != nil {
		t.Fatalf("resolveTotalSince: %v", err)
	}
	want := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("since = %v, want %v", got, want)
	}
}

// TestResolveTotalSinceInvalid verifies a malformed --since date is rejected
// with the shared "invalid --since" error style.
func TestResolveTotalSinceInvalid(t *testing.T) {
	_, err := resolveTotalSince("not-a-date", totalNow, time.UTC)
	if err == nil || !strings.Contains(err.Error(), "invalid --since") {
		t.Errorf("err = %v, want an invalid --since error", err)
	}
}

func TestResolveAddProjectUnique(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	got, err := resolveAddProject(s, "pay", false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got == nil || *got != 2 {
		t.Errorf("resolved = %v, want project 2 (Payments)", got)
	}
}

func TestResolveAddProjectNone(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	_, err := resolveAddProject(s, "nonexistent", false)
	if err == nil || !strings.Contains(err.Error(), "tg update") {
		t.Errorf("err = %v, want suggestion to run `tg update`", err)
	}
}

func TestResolveUpdateProjectEnvWins(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	pid := int64(2)
	got, err := resolveUpdateProject(s, &pid, "backend", false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got == nil || *got != 2 {
		t.Errorf("resolved = %v, want 2 (env wins)", got)
	}
}

func TestResolveUpdateProjectRequiresScope(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	_, err := resolveUpdateProject(s, nil, "  ", false)
	if err == nil || !strings.Contains(err.Error(), "TOGGL_PROJECT_ID") {
		t.Errorf("err = %v, want required-argument error mentioning TOGGL_PROJECT_ID", err)
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
	if err := cmdUpdate(&buf, s, c, 1, &pid, false, "", updateSince, updateNow, false, false); err != nil {
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
	scoped, _ := s.ListTasks(false, &p2)
	if len(scoped) != 1 || scoped[0].ID != 21 {
		t.Errorf("project 2 tasks = %+v, want only id 21", scoped)
	}
	// ...while project 1's cached tasks are untouched.
	p1 := int64(1)
	backend, _ := s.ListTasks(false, &p1)
	if len(backend) == 0 {
		t.Error("project 1 tasks should be untouched by a project-2 update")
	}
}

// TestUpdatePullsRecentEntries verifies update also reconciles time entries: it
// asks Toggl for everything modified since the window start and, being scoped
// to one project, keeps other projects' entries out and the last_pull watermark
// untouched so a later full `tg pull` still sees them.
func TestUpdatePullsRecentEntries(t *testing.T) {
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
	if err := cmdUpdate(&buf, s, c, 1, &pid, false, "", updateSince, updateNow, false, false); err != nil {
		t.Fatalf("update: %v", err)
	}

	// The window start is passed through to the API as a unix timestamp.
	if want := "1767225600"; gotSince != want { // 2026-01-01T00:00:00Z
		t.Errorf("since = %q, want %q", gotSince, want)
	}
	// Only the scoped project's entry landed locally.
	if got, _ := s.EntryByRemoteID(2); got == nil {
		t.Error("payments entry should be inserted by a project-2 update")
	}
	if got, _ := s.EntryByRemoteID(1); got != nil {
		t.Error("backend entry should be ignored by a project-2 update")
	}
	// A scoped pull is partial: the watermark must stay untouched.
	if _, ok, _ := s.GetMeta(store.MetaLastPull); ok {
		t.Error("update should not advance last_pull (it is a scoped pull)")
	}
}

// TestUpdateJSONStillReports pins that making `tg update` quiet only silenced
// human mode: --json keeps emitting the machine-readable summary, now including
// the entry-pull counts.
func TestUpdateJSONStillReports(t *testing.T) {
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
	if err := cmdUpdate(&buf, s, c, 1, &pid, false, "", updateSince, updateNow, false, true); err != nil {
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

// TestResolveUpdateSinceDefault pins `tg update`'s default window: one calendar
// day back, aligned to midnight rather than a rolling 24h cut.
func TestResolveUpdateSinceDefault(t *testing.T) {
	now := time.Date(2026, 1, 2, 15, 30, 0, 0, time.UTC)
	got := resolveUpdateSince(updateDefaultDays, now, time.UTC)
	want := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("since = %v, want %v", got, want)
	}
}

// TestResolveUpdateSinceDays verifies --days/-n walks the window start back a
// whole number of calendar days, with 0 meaning today only.
func TestResolveUpdateSinceDays(t *testing.T) {
	now := time.Date(2026, 1, 2, 15, 30, 0, 0, time.UTC)
	for _, tc := range []struct {
		days int
		want time.Time
	}{
		{0, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)},
		{3, time.Date(2025, 12, 30, 0, 0, 0, 0, time.UTC)},
		{-5, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)}, // clamped to today
	} {
		if got := resolveUpdateSince(tc.days, now, time.UTC); !got.Equal(tc.want) {
			t.Errorf("resolveUpdateSince(%d) = %v, want %v", tc.days, got, tc.want)
		}
	}
}

// TestUpdateDaysFlagAliases pins how runUpdate wires --days/-n: both spellings
// share one variable and one default, and (thanks to parseArgsAndFlags) may
// follow the project fragment, as in `tg update backend -n 3`.
func TestUpdateDaysFlagAliases(t *testing.T) {
	newFS := func() (*flag.FlagSet, *int) {
		fs := flag.NewFlagSet("update", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		days := new(int)
		fs.IntVar(days, "days", updateDefaultDays, "")
		fs.IntVar(days, "n", updateDefaultDays, "")
		return fs, days
	}

	for _, tc := range []struct {
		args     []string
		wantDays int
		wantFrag string
	}{
		{[]string{"backend"}, updateDefaultDays, "backend"},
		{[]string{"backend", "-n", "3"}, 3, "backend"},
		{[]string{"--days", "7", "backend"}, 7, "backend"},
		{[]string{"backend", "--days=2"}, 2, "backend"},
	} {
		fs, days := newFS()
		rest, err := parseArgsAndFlags(fs, tc.args)
		if err != nil {
			t.Fatalf("parse %v: %v", tc.args, err)
		}
		if *days != tc.wantDays {
			t.Errorf("parse %v: days = %d, want %d", tc.args, *days, tc.wantDays)
		}
		if frag := strings.Join(rest, " "); frag != tc.wantFrag {
			t.Errorf("parse %v: fragment = %q, want %q", tc.args, frag, tc.wantFrag)
		}
	}
}

// TestUpdateProjectFragmentSources pins how the two equivalent ways of naming
// `tg update`'s project fold into one fragment: the --project/-p flag, the
// positionals (joined so a multi-word name works unquoted), neither (left empty
// for resolveUpdateProject to reject or for TOGGL_PROJECT_ID to cover), and
// both at once, which is a usage error rather than a silent precedence rule.
func TestUpdateProjectFragmentSources(t *testing.T) {
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

// TestUpdateProjectFlagAliases pins runUpdate's flag wiring for --project/-p:
// both spellings share one variable, they mix freely with --days/-n, and (via
// parseArgsAndFlags) order does not matter.
func TestUpdateProjectFlagAliases(t *testing.T) {
	parse := func(args []string) (project string, days int, rest []string, err error) {
		fs := flag.NewFlagSet("update", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		fs.IntVar(&days, "days", updateDefaultDays, "")
		fs.IntVar(&days, "n", updateDefaultDays, "")
		fs.StringVar(&project, "project", "", "")
		fs.StringVar(&project, "p", "", "")
		rest, err = parseArgsAndFlags(fs, args)
		return project, days, rest, err
	}

	for _, tc := range []struct {
		args     []string
		wantFrag string
		wantDays int
	}{
		{[]string{"-p", "backend"}, "backend", updateDefaultDays},
		{[]string{"--project", "backend"}, "backend", updateDefaultDays},
		{[]string{"--project=backend"}, "backend", updateDefaultDays},
		{[]string{"-p", "backend", "-n", "3"}, "backend", 3},
		{[]string{"-n", "3", "-p", "backend"}, "backend", 3},
		// The positional form still works and reaches the same fragment.
		{[]string{"backend", "-n", "3"}, "backend", 3},
	} {
		project, days, rest, err := parse(tc.args)
		if err != nil {
			t.Fatalf("parse %v: %v", tc.args, err)
		}
		frag, err := updateProjectFragment(project, rest)
		if err != nil {
			t.Fatalf("fragment %v: %v", tc.args, err)
		}
		if frag != tc.wantFrag {
			t.Errorf("parse %v: fragment = %q, want %q", tc.args, frag, tc.wantFrag)
		}
		if days != tc.wantDays {
			t.Errorf("parse %v: days = %d, want %d", tc.args, days, tc.wantDays)
		}
	}
}

// TestUpdateResolvesProjectByFragment verifies `tg update` picks its project by
// a case-insensitive name fragment against the cached catalog (no env id, no
// exact name): "pay" reaches Payments (id 2), so the tasks fetched and the
// entries kept are that project's.
func TestUpdateResolvesProjectByFragment(t *testing.T) {
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
	if err := cmdUpdate(&buf, s, c, 1, nil, false, "pay", updateSince, updateNow, false, false); err != nil {
		t.Fatalf("update: %v", err)
	}
	for _, p := range paths {
		if p != "/workspaces/1/projects/2/tasks" && p != "/me/time_entries" {
			t.Errorf("unexpected request path %q", p)
		}
	}
	p2 := int64(2)
	scoped, _ := s.ListTasks(false, &p2)
	if len(scoped) != 1 || scoped[0].ID != 21 {
		t.Errorf("project 2 tasks = %+v, want only id 21", scoped)
	}
	// The entry pull was scoped to the resolved project too.
	if got, _ := s.EntryByRemoteID(2); got == nil {
		t.Error("payments entry should be inserted")
	}
	if got, _ := s.EntryByRemoteID(1); got != nil {
		t.Error("backend entry should be ignored by a payments-scoped update")
	}
}

// TestUpdateAmbiguousProjectFragment verifies a fragment matching more than one
// cached project fails with the candidate list (name + id) instead of guessing,
// and that nothing is fetched before the ambiguity is resolved.
func TestUpdateAmbiguousProjectFragment(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)
	if err := s.PutProject(store.Project{
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
	err := cmdUpdate(&buf, s, c, 1, nil, false, "back", updateSince, updateNow, false, false)
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
	got, err := resolveUpdateProject(s, nil, "backend", false)
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
	s := newStore(t)
	seedCatalog(t, s)
	if err := s.PutProject(store.Project{
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
	if err := cmdUpdate(&buf, s, c, 1, nil, true, "back", updateSince, updateNow, false, false); err != nil {
		t.Fatalf("update -1: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("update -1 made no requests")
	}
	p1 := int64(1)
	tasks, _ := s.ListTasks(false, &p1)
	if len(tasks) != 1 || tasks[0].ID != 15 {
		t.Errorf("project 1 tasks = %+v, want only id 15 (Backend was refreshed)", tasks)
	}
}

// pullNow is a fixed mid-month, mid-day clock for the `tg pull` window tests.
var pullNow = time.Date(2026, 3, 17, 15, 30, 0, 0, time.UTC)

// TestResolvePullSinceDefault pins `tg pull`'s default window: today only,
// aligned to midnight in the given location.
func TestResolvePullSinceDefault(t *testing.T) {
	got, err := resolvePullSince("", false, pullNow, time.UTC)
	if err != nil {
		t.Fatalf("resolvePullSince: %v", err)
	}
	want := time.Date(2026, 3, 17, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("since = %v, want %v (start of today)", got, want)
	}
}

// TestResolvePullSinceAll verifies -a/--all widens the window to the whole
// current calendar month, starting at midnight on the 1st.
func TestResolvePullSinceAll(t *testing.T) {
	got, err := resolvePullSince("", true, pullNow, time.UTC)
	if err != nil {
		t.Fatalf("resolvePullSince: %v", err)
	}
	want := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("since = %v, want %v (start of this month)", got, want)
	}
}

// TestResolvePullSinceLocation verifies both windows are day-aligned in the
// caller's location, not in UTC.
func TestResolvePullSinceLocation(t *testing.T) {
	loc := time.FixedZone("UTC+3", 3*60*60)
	// 2026-03-01T01:00+03:00 is still February in UTC.
	now := time.Date(2026, 2, 28, 22, 0, 0, 0, time.UTC)
	day, err := resolvePullSince("", false, now, loc)
	if err != nil {
		t.Fatalf("resolvePullSince: %v", err)
	}
	if want := time.Date(2026, 3, 1, 0, 0, 0, 0, loc); !day.Equal(want) {
		t.Errorf("today window = %v, want %v", day, want)
	}
	month, err := resolvePullSince("", true, now, loc)
	if err != nil {
		t.Fatalf("resolvePullSince: %v", err)
	}
	if want := time.Date(2026, 3, 1, 0, 0, 0, 0, loc); !month.Equal(want) {
		t.Errorf("month window = %v, want %v", month, want)
	}
}

// TestResolvePullSinceOverride verifies an explicit --since date wins over both
// defaults, including over --all.
func TestResolvePullSinceOverride(t *testing.T) {
	for _, all := range []bool{false, true} {
		got, err := resolvePullSince("2025-11-04", all, pullNow, time.UTC)
		if err != nil {
			t.Fatalf("resolvePullSince(all=%v): %v", all, err)
		}
		want := time.Date(2025, 11, 4, 0, 0, 0, 0, time.UTC)
		if !got.Equal(want) {
			t.Errorf("since(all=%v) = %v, want %v", all, got, want)
		}
	}
}

// TestResolvePullSinceInvalid verifies a malformed --since date is rejected
// with the shared "invalid --since" error style.
func TestResolvePullSinceInvalid(t *testing.T) {
	_, err := resolvePullSince("17-03-2026", false, pullNow, time.UTC)
	if err == nil || !strings.Contains(err.Error(), "invalid --since") {
		t.Errorf("err = %v, want an invalid --since error", err)
	}
}

// TestPullAllFlagAliases pins how runPull wires --all/-a: both spellings share
// one variable, default to false (today only), and (thanks to parseArgsAndFlags)
// may follow the project fragment, as in `tg pull backend -a`.
func TestPullAllFlagAliases(t *testing.T) {
	newFS := func() (*flag.FlagSet, *bool) {
		fs := flag.NewFlagSet("pull", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		all := new(bool)
		fs.BoolVar(all, "all", false, "")
		fs.BoolVar(all, "a", false, "")
		return fs, all
	}

	for _, tc := range []struct {
		args     []string
		wantAll  bool
		wantFrag string
	}{
		{[]string{}, false, ""},
		{[]string{"backend"}, false, "backend"},
		{[]string{"-a"}, true, ""},
		{[]string{"--all"}, true, ""},
		{[]string{"backend", "-a"}, true, "backend"},
		{[]string{"--all", "backend"}, true, "backend"},
	} {
		fs, all := newFS()
		rest, err := parseArgsAndFlags(fs, tc.args)
		if err != nil {
			t.Fatalf("parse %v: %v", tc.args, err)
		}
		if *all != tc.wantAll {
			t.Errorf("parse %v: all = %v, want %v", tc.args, *all, tc.wantAll)
		}
		if frag := strings.Join(rest, " "); frag != tc.wantFrag {
			t.Errorf("parse %v: fragment = %q, want %q", tc.args, frag, tc.wantFrag)
		}
	}
}

// TestPullTodayWindowKeepsStaleWatermark verifies the default today-only window
// is treated as a partial pull at the command level: an older watermark is left
// alone so a later wider pull still reconciles the days in between.
func TestPullTodayWindowKeepsStaleWatermark(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)
	if err := s.SetMeta(store.MetaLastPull, "2026-01-01T09:00:00Z"); err != nil {
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
	if err := cmdPull(&buf, s, c, false, "", since, now, false); err != nil {
		t.Fatalf("pull: %v", err)
	}
	v, _, _ := s.GetMeta(store.MetaLastPull)
	if v != "2026-01-01T09:00:00Z" {
		t.Errorf("last_pull = %q, want the untouched watermark", v)
	}
}

// TestProjectsUpdateSyncsWholeWorkspace verifies `projects update` walks the
// entire workspace project list and upserts it (without wiping other cached
// projects) while never fetching tasks.
func TestProjectsUpdateSyncsWholeWorkspace(t *testing.T) {
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
	if err := cmdUpdateProjects(&buf, s, c, 1, false, false); err != nil {
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
	projects, _ := s.ListProjects(false)
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
	backend, _ := s.ListTasks(false, &p1)
	if len(backend) == 0 {
		t.Error("project 1 tasks should be untouched by projects update")
	}
}

func TestProjectsUpdateJSON(t *testing.T) {
	s := newStore(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"id":2,"workspace_id":1,"name":"Payments","active":true}]`))
	}))
	defer srv.Close()
	c := api.New("tok", api.WithBaseURL(srv.URL), api.WithHTTPClient(srv.Client()))

	var buf bytes.Buffer
	if err := cmdUpdateProjects(&buf, s, c, 1, false, true); err != nil {
		t.Fatalf("projects update --json: %v", err)
	}
	if !strings.Contains(buf.String(), `"projects":1`) {
		t.Errorf("json output = %q, want projects count", buf.String())
	}
}

func TestProjectsCommand(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	var buf bytes.Buffer
	if err := cmdProjects(&buf, s, false, false); err != nil {
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
	if err := s.ReplaceProjects([]store.Project{{ID: 1, WorkspaceID: 1, Name: "Backend", Active: true}}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceTasks([]store.Task{
		{ID: 10, WorkspaceID: 1, ProjectID: 1, Name: "Fix login bug", Active: true},
		{ID: 12, WorkspaceID: 1, ProjectID: 1, Name: "Code review", Active: true},
	}); err != nil {
		t.Fatal(err)
	}
	start1 := time.Date(2026, 1, 2, 9, 15, 0, 0, time.UTC)
	stop1 := time.Date(2026, 1, 2, 10, 30, 0, 0, time.UTC)
	start2 := time.Date(2026, 1, 2, 10, 30, 0, 0, time.UTC)
	if _, err := s.CreateEntry(store.Entry{
		WorkspaceID: 1, ProjectID: p(1), TaskID: p(10),
		Start: start1, Stop: &stop1, Duration: 4500, UpdatedAt: stop1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateEntry(store.Entry{
		WorkspaceID: 1, ProjectID: p(1), TaskID: p(12),
		Start: start2, Duration: -1, UpdatedAt: start2,
	}); err != nil {
		t.Fatal(err)
	}
	return time.Date(2026, 1, 2, 11, 15, 0, 0, time.UTC), time.UTC
}

func p(v int64) *int64 { return &v }

func TestTodayCommandGolden(t *testing.T) {
	s := newStore(t)
	now, loc := seedSampleDay(t, s)
	var buf bytes.Buffer
	if err := cmdToday(&buf, s, now, loc, 1, false, false); err != nil {
		t.Fatalf("today: %v", err)
	}
	assertGolden(t, "today.txt", buf.String())
}

// TestTodayCommandNumbers pins the numbers behind `tg mod 2` / `tg del 3`: the
// listing shows each entry's own persistent per-day number (human and JSON
// alike), and those are exactly the numbers the store resolves — listing is a
// read, so it never renumbers anything.
func TestTodayCommandNumbers(t *testing.T) {
	s := newStore(t)
	now, loc := seedSampleDay(t, s)

	entries, err := s.EntriesBetween(startOfDay(now, loc), startOfDay(now, loc).Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("fixture has %d entries, want 2", len(entries))
	}

	var buf bytes.Buffer
	if err := cmdToday(&buf, s, now, loc, 1, false, false); err != nil {
		t.Fatalf("today: %v", err)
	}
	for i, e := range entries {
		got, err := s.EntryByNum(i+1, now)
		if err != nil {
			t.Fatalf("EntryByNum(%d): %v", i+1, err)
		}
		if got.ID != e.ID {
			t.Errorf("EntryByNum(%d).ID = %d, want %d", i+1, got.ID, e.ID)
		}
	}
	if _, err := s.EntryByNum(3, now); !errors.Is(err, store.ErrNoEntryNum) {
		t.Errorf("EntryByNum(3) error = %v, want ErrNoEntryNum", err)
	}

	// The numbers are in the human output and in the JSON shape.
	if !strings.Contains(buf.String(), "1  09:15-10:30") {
		t.Errorf("listing missing leading entry numbers:\n%s", buf.String())
	}
	buf.Reset()
	if err := cmdToday(&buf, s, now, loc, 1, true, false); err != nil {
		t.Fatalf("today --json: %v", err)
	}
	if !strings.Contains(buf.String(), `"num":1`) || !strings.Contains(buf.String(), `"num":2`) {
		t.Errorf("json listing missing entry numbers:\n%s", buf.String())
	}

	// Deleting the first entry leaves the second one addressable as 2: the
	// listing shows the surviving numbers, gap and all.
	if err := cmdDel(io.Discard, s, nil, 1, now, loc); err != nil {
		t.Fatalf("del: %v", err)
	}
	buf.Reset()
	if err := cmdToday(&buf, s, now, loc, 1, false, false); err != nil {
		t.Fatalf("today after del: %v", err)
	}
	if !strings.Contains(buf.String(), "2  10:30-") {
		t.Errorf("listing renumbered the survivor:\n%s", buf.String())
	}

	// Listing a day with nothing tracked is just an empty listing; it cannot
	// make another day's numbers resolve.
	empty := now.AddDate(0, 0, 1)
	buf.Reset()
	if err := cmdToday(&buf, s, empty, loc, 1, false, false); err != nil {
		t.Fatalf("today (empty day): %v", err)
	}
	if _, err := s.EntryByNum(2, empty); !errors.Is(err, store.ErrNoEntryNum) {
		t.Errorf("EntryByNum(2) on an empty day = %v, want ErrNoEntryNum", err)
	}
	if got, err := s.EntryByNum(2, now); err != nil || got.ID != entries[1].ID {
		t.Errorf("EntryByNum(2) = %+v err=%v, want the number still live on its own day", got, err)
	}
}

// TestTodayCommandMultiDayGrouping covers `tg ls --days N`: each day carries its
// own 1..N, so the listing groups entries under a date header instead of
// showing a flat run of repeated numbers.
func TestTodayCommandMultiDayGrouping(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	mk := func(start time.Time) {
		stop := start.Add(time.Hour)
		if _, err := s.CreateEntry(store.Entry{
			WorkspaceID: 1, ProjectID: p(1), TaskID: p(10),
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
	if err := cmdToday(&buf, s, now, time.UTC, 2, false, false); err != nil {
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
	if e, err := s.EntryByNum(1, now); err != nil || !e.Start.Equal(today) {
		t.Errorf("EntryByNum(1, today) = %+v err=%v, want today's entry", e, err)
	}
}

// TestTodayCommandTrailingGap covers the closing filler row: with the last
// entry finished and now past its stop, the listing reports the idle time
// before the total footer.
func TestTodayCommandTrailingGap(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	start := time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC)
	stop := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)
	if _, err := s.CreateEntry(store.Entry{
		WorkspaceID: 1, ProjectID: p(1), TaskID: p(10),
		Start: start, Stop: &stop, Duration: 3600, UpdatedAt: stop,
	}); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 1, 2, 10, 25, 0, 0, time.UTC)
	var buf bytes.Buffer
	if err := cmdToday(&buf, s, now, time.UTC, 1, false, false); err != nil {
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
// January 2026 and returns a `now` inside that month. Each entry starts at
// 09:00 on its day, so the durations never collide.
func seedDailyMonth(t *testing.T, s *store.Store, days map[int]time.Duration) {
	t.Helper()
	seedCatalog(t, s)
	for day, d := range days {
		start := time.Date(2026, 1, day, 9, 0, 0, 0, time.UTC)
		stop := start.Add(d)
		if _, err := s.CreateEntry(store.Entry{
			WorkspaceID: 1, ProjectID: p(1), TaskID: p(10),
			Start: start, Stop: &stop, Duration: int64(d / time.Second), UpdatedAt: stop,
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// TestDailySumsPerDay is the core of `tg daily`: entries are summed per
// calendar day, one line each, and every line reports the day's overtime
// against the target. The footer sums the month and measures it against
// target x the number of listed days.
func TestDailySumsPerDay(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	// Two entries on 2026-01-05 (8h30m together) and one on 2026-01-06 (7h15m).
	mk := func(start time.Time, d time.Duration) {
		stop := start.Add(d)
		if _, err := s.CreateEntry(store.Entry{
			WorkspaceID: 1, ProjectID: p(1), TaskID: p(10),
			Start: start, Stop: &stop, Duration: int64(d / time.Second), UpdatedAt: stop,
		}); err != nil {
			t.Fatal(err)
		}
	}
	mk(time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC), 4*time.Hour)
	mk(time.Date(2026, 1, 5, 13, 0, 0, 0, time.UTC), 4*time.Hour+30*time.Minute)
	mk(time.Date(2026, 1, 6, 9, 0, 0, 0, time.UTC), 7*time.Hour+15*time.Minute)

	now := time.Date(2026, 1, 20, 12, 0, 0, 0, time.UTC)
	var buf bytes.Buffer
	if err := cmdDaily(&buf, s, now, time.UTC, dailyDefaultTarget, false, false); err != nil {
		t.Fatalf("daily: %v", err)
	}
	want := "Mon 2026-01-05  8h30m   +0:30\n" +
		"Tue 2026-01-06  7h15m   -0:45\n" +
		todayDivider + "\n" +
		"Total: 15h45m  -0:15  (2 days x 8h00m)\n"
	if buf.String() != want {
		t.Errorf("daily = %q, want %q", buf.String(), want)
	}
}

// TestDailyCoversWholeMonth pins the window: the listing spans the FULL
// calendar month containing now, so days later in the month are included even
// when now sits early in it, while the neighbouring months are excluded.
func TestDailyCoversWholeMonth(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	mk := func(day time.Time) {
		stop := day.Add(time.Hour)
		if _, err := s.CreateEntry(store.Entry{
			WorkspaceID: 1, ProjectID: p(1), TaskID: p(10),
			Start: day, Stop: &stop, Duration: 3600, UpdatedAt: stop,
		}); err != nil {
			t.Fatal(err)
		}
	}
	mk(time.Date(2025, 12, 31, 9, 0, 0, 0, time.UTC)) // previous month
	mk(time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC))   // first day of the month
	mk(time.Date(2026, 1, 31, 9, 0, 0, 0, time.UTC))  // last day of the month
	mk(time.Date(2026, 2, 1, 9, 0, 0, 0, time.UTC))   // next month

	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	var buf bytes.Buffer
	if err := cmdDaily(&buf, s, now, time.UTC, dailyDefaultTarget, false, false); err != nil {
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

// TestDailyGreysUpcomingDays pins that days after today — the month's remaining
// days, which can carry time booked ahead — are dimmed in the human listing so
// they read as planned rather than worked. Today and the days behind it stay
// plain, and JSON never carries styling.
func TestDailyGreysUpcomingDays(t *testing.T) {
	s := newStore(t)
	seedDailyMonth(t, s, map[int]time.Duration{
		19: 8 * time.Hour, // yesterday
		20: 6 * time.Hour, // today
		21: 4 * time.Hour, // booked ahead
	})
	now := time.Date(2026, 1, 20, 18, 0, 0, 0, time.UTC)

	var buf bytes.Buffer
	if err := cmdDaily(&buf, s, now, time.UTC, dailyDefaultTarget, false, true); err != nil {
		t.Fatalf("daily: %v", err)
	}
	want := "Mon 2026-01-19  8h00m   +0:00\n" +
		"Tue 2026-01-20  6h00m   -2:00\n" +
		faint("Wed 2026-01-21  4h00m   -4:00") + "\n" +
		todayDivider + "\n" +
		"Total: 18h00m  -6:00  (3 days x 8h00m)\n"
	if buf.String() != want {
		t.Errorf("daily = %q, want %q", buf.String(), want)
	}

	// --json is a data shape, so it stays free of escapes even on a terminal.
	buf.Reset()
	if err := cmdDaily(&buf, s, now, time.UTC, dailyDefaultTarget, true, true); err != nil {
		t.Fatalf("daily --json: %v", err)
	}
	if strings.Contains(buf.String(), "\x1b") {
		t.Errorf("daily --json contains ANSI escapes: %q", buf.String())
	}
}

// TestDailyTargetFlag pins -t/--target: it shifts every overtime figure,
// including the footer's, and defaults to 8 hours.
func TestDailyTargetFlag(t *testing.T) {
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
		if err := cmdDaily(&buf, s, now, time.UTC, c.target, false, false); err != nil {
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
	s := newStore(t)
	seedDailyMonth(t, s, map[int]time.Duration{5: 8 * time.Hour})
	now := time.Date(2026, 1, 20, 12, 0, 0, 0, time.UTC)
	var buf bytes.Buffer
	err := cmdDaily(&buf, s, now, time.UTC, -1, false, false)
	if err == nil {
		t.Fatalf("daily -t -1: expected an error, got %q", buf.String())
	}
	if !strings.Contains(err.Error(), "negative") {
		t.Errorf("daily -t -1 error = %v, want it to mention the negative target", err)
	}
}

// TestDailyCountsRunningEntryLive pins that a running entry contributes its
// elapsed-so-far time, exactly as `tg today`/`tg status` count it, and that the
// day is flagged as still growing.
func TestDailyCountsRunningEntryLive(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)
	start := time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC)
	if _, err := s.CreateEntry(store.Entry{
		WorkspaceID: 1, ProjectID: p(1), TaskID: p(12),
		Start: start, Duration: -1, UpdatedAt: start,
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 5, 11, 30, 0, 0, time.UTC)
	var buf bytes.Buffer
	if err := cmdDaily(&buf, s, now, time.UTC, dailyDefaultTarget, false, false); err != nil {
		t.Fatalf("daily: %v", err)
	}
	want := "Mon 2026-01-05  2h30m*  -5:30\n" +
		todayDivider + "\n" +
		"Total: 2h30m   -5:30  (1 day x 8h00m)   (* running)\n"
	if buf.String() != want {
		t.Errorf("daily = %q, want %q", buf.String(), want)
	}
}

// TestDailySkipsDeletedEntries pins that a soft-deleted entry stops counting
// towards its day, and that a day left with nothing tracked drops out of the
// listing entirely (which also shrinks the footer's target).
func TestDailySkipsDeletedEntries(t *testing.T) {
	s := newStore(t)
	seedDailyMonth(t, s, map[int]time.Duration{5: 8 * time.Hour, 6: 8 * time.Hour})
	now := time.Date(2026, 1, 6, 20, 0, 0, 0, time.UTC)

	// Entry 1 of 2026-01-06 is today's only entry, so `tg del 1` removes it.
	if err := cmdDel(io.Discard, s, nil, 1, now, time.UTC); err != nil {
		t.Fatalf("del: %v", err)
	}
	var buf bytes.Buffer
	if err := cmdDaily(&buf, s, now, time.UTC, dailyDefaultTarget, false, false); err != nil {
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

// TestDailyEmptyMonth covers a month with nothing tracked: an explanatory line
// rather than an error, so `tg daily` is safe to run on a fresh install.
func TestDailyEmptyMonth(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)
	now := time.Date(2026, 1, 20, 12, 0, 0, 0, time.UTC)
	var buf bytes.Buffer
	if err := cmdDaily(&buf, s, now, time.UTC, dailyDefaultTarget, false, false); err != nil {
		t.Fatalf("daily: %v", err)
	}
	if got := buf.String(); got != "No entries this month.\n" {
		t.Errorf("daily = %q, want the empty-month line", got)
	}
}

// TestDailyJSON pins the machine-readable shape: one object per listed day with
// its date, tracked seconds and signed overtime, plus the month totals and the
// per-day target the overtimes were measured against.
func TestDailyJSON(t *testing.T) {
	s := newStore(t)
	seedDailyMonth(t, s, map[int]time.Duration{
		5: 8*time.Hour + 30*time.Minute,
		6: 7*time.Hour + 15*time.Minute,
	})
	now := time.Date(2026, 1, 20, 12, 0, 0, 0, time.UTC)
	var buf bytes.Buffer
	if err := cmdDaily(&buf, s, now, time.UTC, dailyDefaultTarget, true, false); err != nil {
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
	s := newStore(t)
	now, loc := seedSampleDay(t, s)
	var buf bytes.Buffer
	if err := cmdCurrent(&buf, s, now, loc, false); err != nil {
		t.Fatalf("current: %v", err)
	}
	assertGolden(t, "current.txt", buf.String())
}

// TestCurrentCommandLastEntryAndGap covers the no-timer path: with nothing
// running, status reports the newest finished entry, the idle gap since it
// stopped, and today's total (both entries, 1h15m + 0h30m).
func TestCurrentCommandLastEntryAndGap(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	first := time.Date(2026, 1, 2, 9, 15, 0, 0, time.UTC)
	firstStop := time.Date(2026, 1, 2, 10, 30, 0, 0, time.UTC)
	last := time.Date(2026, 1, 2, 10, 30, 0, 0, time.UTC)
	lastStop := time.Date(2026, 1, 2, 11, 0, 0, 0, time.UTC)
	for _, e := range []store.Entry{
		{WorkspaceID: 1, ProjectID: p(1), TaskID: p(10), Start: first, Stop: &firstStop, Duration: 4500, UpdatedAt: firstStop},
		{WorkspaceID: 1, ProjectID: p(1), TaskID: p(12), Start: last, Stop: &lastStop, Duration: 1800, UpdatedAt: lastStop},
	} {
		if _, err := s.CreateEntry(e); err != nil {
			t.Fatal(err)
		}
	}

	now := time.Date(2026, 1, 2, 11, 25, 0, 0, time.UTC)
	var buf bytes.Buffer
	if err := cmdCurrent(&buf, s, now, time.UTC, false); err != nil {
		t.Fatalf("current: %v", err)
	}
	want := "Code review [Backend] 10:30-11:00 (gap 0h25m) Today: 1h45m\n"
	if buf.String() != want {
		t.Errorf("status = %q, want %q", buf.String(), want)
	}

	// The JSON shape carries the same facts.
	buf.Reset()
	if err := cmdCurrent(&buf, s, now, time.UTC, true); err != nil {
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

// TestCurrentCommandIgnoresFutureEntry covers the shared last-entry resolution
// from status's side: an entry that starts later today has not happened yet, so
// the line still reports the last thing actually tracked (and the gap since it
// stopped). The day total is the whole day's tracked time and does include it.
func TestCurrentCommandIgnoresFutureEntry(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	start := time.Date(2026, 1, 2, 9, 15, 0, 0, time.UTC)
	stop := time.Date(2026, 1, 2, 10, 30, 0, 0, time.UTC)
	future := time.Date(2026, 1, 2, 18, 0, 0, 0, time.UTC)
	futureStop := time.Date(2026, 1, 2, 19, 0, 0, 0, time.UTC)
	for _, e := range []store.Entry{
		{WorkspaceID: 1, ProjectID: p(1), TaskID: p(10), Start: start, Stop: &stop, Duration: 4500, UpdatedAt: stop},
		{WorkspaceID: 1, ProjectID: p(1), TaskID: p(12), Start: future, Stop: &futureStop, Duration: 3600, UpdatedAt: stop},
	} {
		if _, err := s.CreateEntry(e); err != nil {
			t.Fatal(err)
		}
	}

	now := time.Date(2026, 1, 2, 11, 25, 0, 0, time.UTC)
	var buf bytes.Buffer
	if err := cmdCurrent(&buf, s, now, time.UTC, false); err != nil {
		t.Fatalf("current: %v", err)
	}
	want := "Fix login bug [Backend] 09:15-10:30 (gap 0h55m) Today: 2h15m\n"
	if buf.String() != want {
		t.Errorf("status = %q, want %q", buf.String(), want)
	}
}

// TestCurrentCommandIgnoresYesterday pins the day scope: a new day starts with
// no last entry rather than reporting yesterday's, so status says so instead of
// showing a stale line with an overnight gap.
func TestCurrentCommandIgnoresYesterday(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	start := time.Date(2026, 1, 1, 16, 0, 0, 0, time.UTC)
	stop := time.Date(2026, 1, 1, 17, 0, 0, 0, time.UTC)
	if _, err := s.CreateEntry(store.Entry{
		WorkspaceID: 1, ProjectID: p(1), TaskID: p(10),
		Start: start, Stop: &stop, Duration: 3600, UpdatedAt: stop,
	}); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 1, 2, 9, 30, 0, 0, time.UTC)
	var buf bytes.Buffer
	if err := cmdCurrent(&buf, s, now, time.UTC, false); err != nil {
		t.Fatalf("current: %v", err)
	}
	if want := "No entries. Today: 0h00m\n"; buf.String() != want {
		t.Errorf("status = %q, want %q", buf.String(), want)
	}
}

// TestCurrentCommandEmptyStore verifies status still reports a day total when
// nothing has ever been tracked.
func TestCurrentCommandEmptyStore(t *testing.T) {
	s := newStore(t)
	var buf bytes.Buffer
	if err := cmdCurrent(&buf, s, testStart, time.UTC, false); err != nil {
		t.Fatalf("current: %v", err)
	}
	if want := "No entries. Today: 0h00m\n"; buf.String() != want {
		t.Errorf("status = %q, want %q", buf.String(), want)
	}
}

// TestCurrentCommandRunningWinsOverNewerEntry verifies a pulled running entry is
// reported as running even when a finished entry started later.
func TestCurrentCommandRunningWinsOverNewerEntry(t *testing.T) {
	s := newStore(t)
	seedCatalog(t, s)

	seedRunning(t, s, 12, testStart) // 09:00, still running
	later := testStart.Add(-2 * time.Hour)
	laterStop := testStart.Add(-time.Hour)
	if _, err := s.CreateEntry(store.Entry{
		WorkspaceID: 1, ProjectID: p(1), TaskID: p(10),
		Start: later, Stop: &laterStop, Duration: 3600, UpdatedAt: laterStop,
	}); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	now := testStart.Add(30 * time.Minute)
	if err := cmdCurrent(&buf, s, now, time.UTC, false); err != nil {
		t.Fatalf("current: %v", err)
	}
	want := "run Code review [Backend] (0h30m) Today: 1h30m\n"
	if buf.String() != want {
		t.Errorf("status = %q, want %q", buf.String(), want)
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
	loc := dstLoc(t)
	s := newStoreIn(t, loc)
	seedCatalog(t, s)

	today := time.Date(2026, 3, 29, 10, 0, 0, 0, loc)
	todayStop := today.Add(time.Hour)
	// 00:30 on the 30th: inside midnight+24h (01:00), outside the calendar day.
	tomorrow := time.Date(2026, 3, 30, 0, 30, 0, 0, loc)
	tomorrowStop := tomorrow.Add(30 * time.Minute)
	for _, e := range []store.Entry{
		{WorkspaceID: 1, ProjectID: p(1), TaskID: p(10), Start: today, Stop: &todayStop, Duration: 3600, UpdatedAt: todayStop},
		{WorkspaceID: 1, ProjectID: p(1), TaskID: p(12), Start: tomorrow, Stop: &tomorrowStop, Duration: 1800, UpdatedAt: tomorrowStop},
	} {
		if _, err := s.CreateEntry(e); err != nil {
			t.Fatal(err)
		}
	}

	now := time.Date(2026, 3, 29, 12, 0, 0, 0, loc)
	var buf bytes.Buffer
	if err := cmdToday(&buf, s, now, loc, 1, true, false); err != nil {
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
	if err := cmdCurrent(&buf, s, now, loc, true); err != nil {
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
	loc := dstLoc(t)
	s := newStoreIn(t, loc)
	seedCatalog(t, s)

	late := time.Date(2026, 10, 25, 23, 15, 0, 0, loc)
	lateStop := late.Add(30 * time.Minute)
	if _, err := s.CreateEntry(store.Entry{
		WorkspaceID: 1, ProjectID: p(1), TaskID: p(10),
		Start: late, Stop: &lateStop, Duration: 1800, UpdatedAt: lateStop,
	}); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 10, 25, 23, 50, 0, 0, loc)
	var buf bytes.Buffer
	if err := cmdToday(&buf, s, now, loc, 1, true, false); err != nil {
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
	if err := cmdCurrent(&buf, s, now, loc, true); err != nil {
		t.Fatalf("current: %v", err)
	}
	if !strings.Contains(buf.String(), `"day_total_seconds":1800`) {
		t.Errorf("status json = %s, want day_total_seconds 1800", buf.String())
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
	err := cmdAuth(&buf,
		func() (string, error) { return "tok123", nil },
		func(token string) *api.Client {
			return api.New(token, api.WithBaseURL(srv.URL), api.WithHTTPClient(srv.Client()))
		})
	if err != nil {
		t.Fatalf("auth: %v", err)
	}
	if !strings.Contains(buf.String(), "Authenticated as Test User") {
		t.Errorf("output = %q", buf.String())
	}

	path, _ := config.Path()
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
	err := cmdAuth(&buf,
		func() (string, error) { return "bad", nil },
		func(token string) *api.Client {
			return api.New(token, api.WithBaseURL(srv.URL), api.WithHTTPClient(srv.Client()))
		})
	if err == nil {
		t.Fatal("expected an error for 403")
	}

	path, _ := config.Path()
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("config.json should not exist, stat err = %v", statErr)
	}
}
