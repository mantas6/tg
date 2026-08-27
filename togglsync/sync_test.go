package togglsync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mantas6/tg/api"
	"github.com/mantas6/tg/store"
)

// ctx is the context every sync and store call in these tests runs under.
var ctx = context.Background()

func ptrInt(v int64) *int64 { return &v }

func ptrTime(v time.Time) *time.Time { return &v }

// ts parses a fixture timestamp. It takes the test rather than panicking on a
// malformed one, so a typo in a fixture is reported as the test failure it is
// instead of taking the whole package's run down with a panic.
func ts(t *testing.T, s string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse fixture timestamp %q: %v", s, err)
	}
	return parsed
}

// mustCreate inserts a fixture entry and returns its local id.
func mustCreate(t *testing.T, st *store.Store, e store.Entry) int64 {
	t.Helper()
	id, err := st.CreateEntry(ctx, e)
	if err != nil {
		t.Fatalf("create entry: %v", err)
	}
	return id
}

// --- checked store reads ------------------------------------------------------
//
// These wrap the reads the assertions are made against: dropping their errors
// (as these tests used to) turns a failing query into a nil entry, and the
// assertion then blames the sync logic instead of the read that broke.

func mustEntryByRemoteID(t *testing.T, st *store.Store, remoteID int64) *store.Entry {
	t.Helper()
	e, err := st.EntryByRemoteID(ctx, remoteID)
	if err != nil {
		t.Fatalf("EntryByRemoteID(%d): %v", remoteID, err)
	}
	return e
}

func mustDirty(t *testing.T, st *store.Store) []store.Entry {
	t.Helper()
	dirty, err := st.DirtyEntries(ctx)
	if err != nil {
		t.Fatalf("DirtyEntries: %v", err)
	}
	return dirty
}

func mustMeta(t *testing.T, st *store.Store, key string) (string, bool) {
	t.Helper()
	v, ok, err := st.Meta(ctx, key)
	if err != nil {
		t.Fatalf("Meta(%q): %v", key, err)
	}
	return v, ok
}

// decodeBody unmarshals a JSON request body captured by a stub handler. It runs
// on the server's goroutine, so it reports a malformed body with t.Errorf
// (t.Fatalf may only be called from the test's own goroutine).
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

// setup opens a throwaway store pinned to UTC (so the per-day entry numbering
// lines up with the UTC timestamps the fixtures use) plus a stub Toggl server.
func setup(t *testing.T, handler http.HandlerFunc) (*store.Store, *api.Client) {
	t.Helper()
	st, err := store.OpenIn(ctx, filepath.Join(t.TempDir(), "tg.db"), time.UTC)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := api.New("tok", api.WithBaseURL(srv.URL), api.WithHTTPClient(srv.Client()))
	return st, c
}

func TestPushCreate(t *testing.T) {
	t.Parallel()
	var gotMethod string
	st, c := setup(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		w.Write([]byte(`{"id":555,"at":"2026-01-02T10:00:00Z"}`))
	})

	start := ts(t, "2026-01-02T09:00:00Z")
	stop := start.Add(5 * time.Minute)
	id := mustCreate(t, st, store.Entry{
		WorkspaceID: 1, TaskID: ptrInt(7), Start: start, Stop: &stop,
		Duration: 300, UpdatedAt: stop, Dirty: true,
	})

	res, err := Push(ctx, st, c, time.Now())
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if res.Created != 1 {
		t.Errorf("created = %d, want 1", res.Created)
	}
	got := mustEntryByRemoteID(t, st, 555)
	if got == nil || got.ID != id || got.Dirty {
		t.Fatalf("after create: %+v", got)
	}
	if got.RemoteID == nil || *got.RemoteID != 555 {
		t.Errorf("remote_id = %v, want 555", got.RemoteID)
	}
}

func TestPushUpdate(t *testing.T) {
	t.Parallel()
	var gotMethod, gotPath string
	st, c := setup(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.Write([]byte(`{"id":555,"at":"2026-01-02T11:00:00Z"}`))
	})

	start := ts(t, "2026-01-02T09:00:00Z")
	mustCreate(t, st, store.Entry{
		RemoteID: ptrInt(555), WorkspaceID: 1, Start: start,
		Duration: 600, UpdatedAt: start.Add(time.Hour), Dirty: true,
	})

	res, err := Push(ctx, st, c, time.Now())
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if gotMethod != http.MethodPut || gotPath != "/workspaces/1/time_entries/555" {
		t.Errorf("update -> %s %s", gotMethod, gotPath)
	}
	if res.Updated != 1 {
		t.Errorf("updated = %d, want 1", res.Updated)
	}
}

func TestPushDelete(t *testing.T) {
	t.Parallel()
	var gotMethod string
	st, c := setup(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		w.WriteHeader(http.StatusOK)
	})

	start := ts(t, "2026-01-02T09:00:00Z")
	mustCreate(t, st, store.Entry{
		RemoteID: ptrInt(777), WorkspaceID: 1, Start: start,
		Duration: 300, UpdatedAt: start, Dirty: true, Deleted: true,
	})

	res, err := Push(ctx, st, c, time.Now())
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("method = %s, want DELETE", gotMethod)
	}
	if res.Deleted != 1 {
		t.Errorf("deleted = %d, want 1", res.Deleted)
	}
	if got := mustEntryByRemoteID(t, st, 777); got != nil {
		t.Errorf("row should be gone, got %+v", got)
	}
}

func TestPushDeleteNeverPushed(t *testing.T) {
	t.Parallel()
	called := false
	st, c := setup(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	start := ts(t, "2026-01-02T09:00:00Z")
	id := mustCreate(t, st, store.Entry{
		WorkspaceID: 1, Start: start, Duration: 300, UpdatedAt: start, Dirty: true, Deleted: true,
	})
	if _, err := Push(ctx, st, c, time.Now()); err != nil {
		t.Fatalf("push: %v", err)
	}
	if called {
		t.Error("should not call API for a never-pushed deletion")
	}
	if got := mustEntryByRemoteID(t, st, 0); got != nil {
		t.Error("row should be dropped")
	}
	// And the local row by id is gone.
	dirty := mustDirty(t, st)
	for _, e := range dirty {
		if e.ID == id {
			t.Error("deleted row still present")
		}
	}
}

func TestPullInsert(t *testing.T) {
	t.Parallel()
	st, c := setup(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"id":900,"workspace_id":1,"description":"Imported",
		  "start":"2026-01-02T09:00:00Z","stop":"2026-01-02T09:30:00Z",
		  "duration":1800,"at":"2026-01-02T09:30:00Z"}]`))
	})
	now := ts(t, "2026-01-02T12:00:00Z")
	res, err := Pull(ctx, st, c, nil, ts(t, "2026-01-01T00:00:00Z"), now)
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if res.Inserted != 1 {
		t.Errorf("inserted = %d, want 1", res.Inserted)
	}
	got := mustEntryByRemoteID(t, st, 900)
	if got == nil || got.Description != "Imported" || got.Dirty {
		t.Fatalf("inserted entry = %+v", got)
	}
}

// TestPullNumbersInsertedEntries pins how pulled entries join the local
// per-day numbering: they are numbered like any other insert, in the order the
// pull writes them, and each calendar day starts again at 1. A local entry
// written first keeps the number it already had, so a pull never renumbers what
// `tg ls` has already shown.
func TestPullNumbersInsertedEntries(t *testing.T) {
	t.Parallel()
	st, c := setup(t, func(w http.ResponseWriter, r *http.Request) {
		// Deliberately not in start order: insertion order is what numbers them.
		w.Write([]byte(`[{"id":901,"workspace_id":1,"description":"second",
		  "start":"2026-01-02T14:00:00Z","stop":"2026-01-02T15:00:00Z",
		  "duration":3600,"at":"2026-01-02T15:00:00Z"},
		 {"id":902,"workspace_id":1,"description":"third",
		  "start":"2026-01-02T11:00:00Z","stop":"2026-01-02T12:00:00Z",
		  "duration":3600,"at":"2026-01-02T12:00:00Z"},
		 {"id":903,"workspace_id":1,"description":"next day",
		  "start":"2026-01-03T09:00:00Z","stop":"2026-01-03T10:00:00Z",
		  "duration":3600,"at":"2026-01-03T10:00:00Z"}]`))
	})

	// A purely local entry already occupies number 1 on 2026-01-02.
	local := ts(t, "2026-01-02T09:00:00Z")
	localStop := local.Add(time.Hour)
	if _, err := st.CreateEntry(ctx, store.Entry{
		WorkspaceID: 1, Start: local, Stop: &localStop,
		Duration: 3600, UpdatedAt: localStop, Dirty: true,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := Pull(ctx, st, c, nil, ts(t, "2026-01-01T00:00:00Z"), ts(t, "2026-01-03T12:00:00Z")); err != nil {
		t.Fatalf("pull: %v", err)
	}

	for _, tc := range []struct {
		remoteID int64
		wantSeq  int
	}{{901, 2}, {902, 3}, {903, 1}} {
		got, err := st.EntryByRemoteID(ctx, tc.remoteID)
		if err != nil || got == nil {
			t.Fatalf("EntryByRemoteID(%d) = %+v err=%v", tc.remoteID, got, err)
		}
		if got.Seq != tc.wantSeq {
			t.Errorf("entry %d seq = %d, want %d", tc.remoteID, got.Seq, tc.wantSeq)
		}
	}

	// Numbers address the entries they were given to.
	got, err := st.EntryByNum(ctx, 2, ts(t, "2026-01-02T20:00:00Z"))
	if err != nil || got.Description != "second" {
		t.Errorf("EntryByNum(2) = %+v err=%v, want the first pulled entry", got, err)
	}
}

func TestPullMapsBillable(t *testing.T) {
	t.Parallel()
	st, c := setup(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"id":910,"workspace_id":1,"description":"Billed",
		  "start":"2026-01-02T09:00:00Z","stop":"2026-01-02T09:30:00Z",
		  "duration":1800,"billable":true,"at":"2026-01-02T09:30:00Z"}]`))
	})
	if _, err := Pull(ctx, st, c, nil, ts(t, "2026-01-01T00:00:00Z"), ts(t, "2026-01-02T12:00:00Z")); err != nil {
		t.Fatalf("pull: %v", err)
	}
	got := mustEntryByRemoteID(t, st, 910)
	if got == nil || !got.Billable {
		t.Fatalf("entry = %+v, want Billable=true", got)
	}
}

func TestPushSendsBillable(t *testing.T) {
	t.Parallel()
	var body map[string]any
	st, c := setup(t, func(w http.ResponseWriter, r *http.Request) {
		body = decodeBody(t, r)
		w.Write([]byte(`{"id":556,"at":"2026-01-02T10:00:00Z"}`))
	})
	start := ts(t, "2026-01-02T09:00:00Z")
	stop := start.Add(5 * time.Minute)
	mustCreate(t, st, store.Entry{
		WorkspaceID: 1, ProjectID: ptrInt(3), Start: start, Stop: &stop,
		Duration: 300, Billable: true, UpdatedAt: stop, Dirty: true,
	})
	if _, err := Push(ctx, st, c, time.Now()); err != nil {
		t.Fatalf("push: %v", err)
	}
	if body["billable"] != true {
		t.Errorf("billable = %v, want true", body["billable"])
	}
}

func TestPullLWWRemoteNewer(t *testing.T) {
	t.Parallel()
	st, c := setup(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"id":900,"workspace_id":1,"description":"new",
		  "start":"2026-01-02T09:00:00Z","duration":-1,"at":"2026-01-02T10:00:00Z"}]`))
	})
	mustCreate(t, st, store.Entry{
		RemoteID: ptrInt(900), WorkspaceID: 1, Description: "old",
		Start: ts(t, "2026-01-02T09:00:00Z"), Duration: -1,
		UpdatedAt: ts(t, "2026-01-02T09:00:00Z"), Dirty: false,
	})

	res, err := Pull(ctx, st, c, nil, ts(t, "2026-01-01T00:00:00Z"), ts(t, "2026-01-02T12:00:00Z"))
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if res.Updated != 1 {
		t.Errorf("updated = %d, want 1", res.Updated)
	}
	got := mustEntryByRemoteID(t, st, 900)
	if got.Description != "new" {
		t.Errorf("description = %q, want new", got.Description)
	}
}

func TestPullLWWLocalNewer(t *testing.T) {
	t.Parallel()
	st, c := setup(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"id":901,"workspace_id":1,"description":"remote",
		  "start":"2026-01-02T09:00:00Z","duration":-1,"at":"2026-01-02T10:00:00Z"}]`))
	})
	mustCreate(t, st, store.Entry{
		RemoteID: ptrInt(901), WorkspaceID: 1, Description: "local",
		Start: ts(t, "2026-01-02T09:00:00Z"), Duration: -1,
		UpdatedAt: ts(t, "2026-01-02T11:00:00Z"), Dirty: true,
	})

	res, err := Pull(ctx, st, c, nil, ts(t, "2026-01-01T00:00:00Z"), ts(t, "2026-01-02T12:00:00Z"))
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if res.Skipped != 1 {
		t.Errorf("skipped = %d, want 1", res.Skipped)
	}
	got := mustEntryByRemoteID(t, st, 901)
	if got.Description != "local" {
		t.Errorf("description = %q, want local (kept)", got.Description)
	}
}

// TestPullLWWTieKeepsDirtyLocal is the regression test for the tie-break: the
// remote `at` and the local updated_at are equal (Toggl's second-granular clock
// is what MarkSynced stored, so an edit made in the same second collides with
// it) and the local entry has unsynced changes. The remote used to win the tie
// and drop the edit before `tg push` ever ran.
func TestPullLWWTieKeepsDirtyLocal(t *testing.T) {
	t.Parallel()
	st, c := setup(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"id":905,"workspace_id":1,"description":"remote",
		  "start":"2026-01-02T09:00:00Z","stop":"2026-01-02T09:30:00Z",
		  "duration":1800,"at":"2026-01-02T10:00:00Z"}]`))
	})
	at := ts(t, "2026-01-02T10:00:00Z")
	mustCreate(t, st, store.Entry{
		RemoteID: ptrInt(905), WorkspaceID: 1, Description: "local edit",
		Start: ts(t, "2026-01-02T09:00:00Z"), Stop: ptrTime(ts(t, "2026-01-02T09:45:00Z")),
		Duration: 2700, UpdatedAt: at, SyncedAt: &at, Dirty: true,
	})

	res, err := Pull(ctx, st, c, nil, ts(t, "2026-01-01T00:00:00Z"), ts(t, "2026-01-02T12:00:00Z"))
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if res.Updated != 0 || res.Skipped != 1 {
		t.Errorf("res = %+v, want the dirty entry skipped, not updated", res)
	}
	got := mustEntryByRemoteID(t, st, 905)
	if got == nil || got.Description != "local edit" || got.Duration != 2700 {
		t.Fatalf("entry = %+v, want the local edit kept", got)
	}
	// And it is still dirty, so the very next push sends it.
	if !got.Dirty {
		t.Error("entry should stay dirty for the next push")
	}
}

// TestPullLWWTieOverwritesCleanLocal is the other half of the tie-break: a clean
// entry holds nothing the server does not, so a tie may re-apply remote state.
// That is what keeps `tg pull` right after `tg push` idempotent instead of
// leaving the entry looking locally newer forever.
func TestPullLWWTieOverwritesCleanLocal(t *testing.T) {
	t.Parallel()
	st, c := setup(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"id":906,"workspace_id":1,"description":"remote",
		  "start":"2026-01-02T09:00:00Z","stop":"2026-01-02T09:30:00Z",
		  "duration":1800,"at":"2026-01-02T10:00:00Z"}]`))
	})
	at := ts(t, "2026-01-02T10:00:00Z")
	mustCreate(t, st, store.Entry{
		RemoteID: ptrInt(906), WorkspaceID: 1, Description: "stale",
		Start: ts(t, "2026-01-02T09:00:00Z"), Stop: ptrTime(ts(t, "2026-01-02T09:30:00Z")),
		Duration: 1800, UpdatedAt: at, SyncedAt: &at, Dirty: false,
	})

	res, err := Pull(ctx, st, c, nil, ts(t, "2026-01-01T00:00:00Z"), ts(t, "2026-01-02T12:00:00Z"))
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if res.Updated != 1 {
		t.Errorf("res = %+v, want the clean entry updated", res)
	}
	if got := mustEntryByRemoteID(t, st, 906); got == nil || got.Description != "remote" {
		t.Errorf("entry = %+v, want remote state", got)
	}
}

func TestPullRemoteDeleted(t *testing.T) {
	t.Parallel()
	st, c := setup(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"id":902,"workspace_id":1,"start":"2026-01-02T09:00:00Z",
		  "duration":300,"at":"2026-01-02T10:00:00Z","server_deleted_at":"2026-01-02T10:00:00Z"}]`))
	})
	mustCreate(t, st, store.Entry{
		RemoteID: ptrInt(902), WorkspaceID: 1,
		Start: ts(t, "2026-01-02T09:00:00Z"), Duration: 300,
		UpdatedAt: ts(t, "2026-01-02T09:00:00Z"),
	})

	res, err := Pull(ctx, st, c, nil, ts(t, "2026-01-01T00:00:00Z"), ts(t, "2026-01-02T12:00:00Z"))
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if res.Deleted != 1 {
		t.Errorf("deleted = %d, want 1", res.Deleted)
	}
	if got := mustEntryByRemoteID(t, st, 902); got != nil {
		t.Errorf("entry should be deleted, got %+v", got)
	}
}

func TestPullSkipsRemoteDeletedWithNoLocal(t *testing.T) {
	t.Parallel()
	st, c := setup(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"id":903,"workspace_id":1,"start":"2026-01-02T09:00:00Z",
		  "duration":300,"at":"2026-01-02T10:00:00Z","server_deleted_at":"2026-01-02T10:00:00Z"}]`))
	})
	res, err := Pull(ctx, st, c, nil, ts(t, "2026-01-01T00:00:00Z"), ts(t, "2026-01-02T12:00:00Z"))
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if res.Inserted != 0 || res.Skipped != 1 {
		t.Errorf("res = %+v, want skipped deletion", res)
	}
}

// TestPullSelfHealsCatalog verifies that a pulled, task-based entry resolves
// its project/task titles via the catalog even when nothing was seeded, by
// self-healing from the meta=true names in the payload.
func TestPullSelfHealsCatalog(t *testing.T) {
	t.Parallel()
	st, c := setup(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"id":950,"workspace_id":1,"project_id":5,"task_id":7,
		  "project_name":"Backend","project_color":"#0B83D9",
		  "task_name":"Fix login bug","description":"",
		  "start":"2026-01-02T09:00:00Z","stop":"2026-01-02T09:30:00Z",
		  "duration":1800,"at":"2026-01-02T09:30:00Z"}]`))
	})

	if _, err := Pull(ctx, st, c, nil, ts(t, "2026-01-01T00:00:00Z"), ts(t, "2026-01-02T12:00:00Z")); err != nil {
		t.Fatalf("pull: %v", err)
	}

	entries, err := st.EntriesBetween(ctx, ts(t, "2026-01-02T00:00:00Z"), ts(t, "2026-01-03T00:00:00Z"))
	if err != nil {
		t.Fatalf("entries: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if entries[0].TaskName != "Fix login bug" {
		t.Errorf("task name = %q, want %q", entries[0].TaskName, "Fix login bug")
	}
	if entries[0].ProjectName != "Backend" {
		t.Errorf("project name = %q, want %q", entries[0].ProjectName, "Backend")
	}
	// The meta=true project color is healed too, so `tg ls` renders its block.
	if entries[0].ProjectColor != "#0B83D9" {
		t.Errorf("project color = %q, want %q", entries[0].ProjectColor, "#0B83D9")
	}

	// And the healed task is discoverable for `start`.
	tasks, err := st.FindTasksByFragment(ctx, "login", nil)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID != 7 {
		t.Errorf("FindTasksByFragment = %+v, want task 7", tasks)
	}
}

// TestPullProjectScope verifies a project-scoped pull only reconciles entries
// for that project and, being partial, does not advance the last_pull watermark.
func TestPullProjectScope(t *testing.T) {
	t.Parallel()
	st, c := setup(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[
		  {"id":1,"workspace_id":1,"project_id":5,"description":"mine",
		   "start":"2026-01-02T09:00:00Z","stop":"2026-01-02T09:30:00Z",
		   "duration":1800,"at":"2026-01-02T09:30:00Z"},
		  {"id":2,"workspace_id":1,"project_id":9,"description":"other",
		   "start":"2026-01-02T10:00:00Z","stop":"2026-01-02T10:30:00Z",
		   "duration":1800,"at":"2026-01-02T10:30:00Z"},
		  {"id":3,"workspace_id":1,"description":"noproject",
		   "start":"2026-01-02T11:00:00Z","stop":"2026-01-02T11:30:00Z",
		   "duration":1800,"at":"2026-01-02T11:30:00Z"}]`))
	})

	pid := int64(5)
	now := ts(t, "2026-01-02T12:00:00Z")
	res, err := Pull(ctx, st, c, &pid, ts(t, "2026-01-01T00:00:00Z"), now)
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if res.Inserted != 1 {
		t.Errorf("inserted = %d, want 1 (only project 5)", res.Inserted)
	}
	if got := mustEntryByRemoteID(t, st, 1); got == nil {
		t.Error("entry for project 5 should have been inserted")
	}
	if got := mustEntryByRemoteID(t, st, 2); got != nil {
		t.Error("entry for other project should be ignored")
	}
	if got := mustEntryByRemoteID(t, st, 3); got != nil {
		t.Error("entry with no project should be ignored")
	}
	// A scoped pull is partial: the watermark must not advance.
	if _, ok := mustMeta(t, st, store.MetaLastPull); ok {
		t.Error("scoped pull should not advance last_pull")
	}
}

func TestPullAdvancesLastPull(t *testing.T) {
	t.Parallel()
	st, c := setup(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[]`))
	})
	now := ts(t, "2026-01-02T12:00:00Z")
	if _, err := Pull(ctx, st, c, nil, ts(t, "2026-01-01T00:00:00Z"), now); err != nil {
		t.Fatalf("pull: %v", err)
	}
	v, ok := mustMeta(t, st, store.MetaLastPull)
	if !ok || v != "2026-01-02T12:00:00Z" {
		t.Errorf("last_pull = %q ok=%v, want now", v, ok)
	}
}

// TestPullChainedWindowAdvancesLastPull verifies a bounded window still
// advances the watermark when it reaches back to it: `tg pull` run twice in a
// day pulls "today", whose start precedes the watermark left by the first run,
// so the two windows chain and no change can slip through the seam.
func TestPullChainedWindowAdvancesLastPull(t *testing.T) {
	t.Parallel()
	st, c := setup(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[]`))
	})
	if err := st.SetMeta(ctx, store.MetaLastPull, "2026-01-02T06:00:00Z"); err != nil {
		t.Fatalf("seed watermark: %v", err)
	}
	now := ts(t, "2026-01-02T12:00:00Z")
	if _, err := Pull(ctx, st, c, nil, ts(t, "2026-01-02T00:00:00Z"), now); err != nil {
		t.Fatalf("pull: %v", err)
	}
	v, _ := mustMeta(t, st, store.MetaLastPull)
	if v != "2026-01-02T12:00:00Z" {
		t.Errorf("last_pull = %q, want now (window reaches back past the watermark)", v)
	}
}

// TestPullPartialWindowKeepsLastPull verifies a window that starts AFTER the
// watermark is partial and leaves it alone: `tg pull` (today only) run for the
// first time in days never looked at what changed in between, so claiming
// coverage up to now would hide those changes from a later wider pull.
func TestPullPartialWindowKeepsLastPull(t *testing.T) {
	t.Parallel()
	st, c := setup(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[]`))
	})
	if err := st.SetMeta(ctx, store.MetaLastPull, "2025-12-28T09:00:00Z"); err != nil {
		t.Fatalf("seed watermark: %v", err)
	}
	now := ts(t, "2026-01-02T12:00:00Z")
	if _, err := Pull(ctx, st, c, nil, ts(t, "2026-01-02T00:00:00Z"), now); err != nil {
		t.Fatalf("pull: %v", err)
	}
	v, _ := mustMeta(t, st, store.MetaLastPull)
	if v != "2025-12-28T09:00:00Z" {
		t.Errorf("last_pull = %q, want the untouched watermark", v)
	}
}

// TestPullUnparsableWatermarkAdvances verifies a corrupt watermark does not
// wedge the pull path: there is no coverage claim worth protecting, so it is
// overwritten with a well-formed one.
func TestPullUnparsableWatermarkAdvances(t *testing.T) {
	t.Parallel()
	st, c := setup(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[]`))
	})
	if err := st.SetMeta(ctx, store.MetaLastPull, "not-a-timestamp"); err != nil {
		t.Fatalf("seed watermark: %v", err)
	}
	now := ts(t, "2026-01-02T12:00:00Z")
	if _, err := Pull(ctx, st, c, nil, ts(t, "2026-01-02T00:00:00Z"), now); err != nil {
		t.Fatalf("pull: %v", err)
	}
	v, _ := mustMeta(t, st, store.MetaLastPull)
	if v != "2026-01-02T12:00:00Z" {
		t.Errorf("last_pull = %q, want now", v)
	}
}

// TestRemoteAt covers the clock a push records: an absent `at` falls back to
// now, a well-formed one is parsed, and a malformed one is an error instead of
// silently becoming now (which would store a local clock as if the server had
// sent it and skew every later LWW comparison).
func TestRemoteAt(t *testing.T) {
	t.Parallel()
	now := ts(t, "2026-01-02T12:00:00Z")

	got, err := remoteAt("", now)
	if err != nil || !got.Equal(now) {
		t.Errorf("remoteAt(\"\") = %v err=%v, want now", got, err)
	}
	got, err = remoteAt("2026-01-02T10:00:00Z", now)
	if err != nil || !got.Equal(ts(t, "2026-01-02T10:00:00Z")) {
		t.Errorf("remoteAt(valid) = %v err=%v, want the parsed remote at", got, err)
	}
	for _, bad := range []string{"not-a-timestamp", "2026-01-02 10:00:00", "1767355200"} {
		if got, err := remoteAt(bad, now); err == nil {
			t.Errorf("remoteAt(%q) = %v, want an error", bad, got)
		}
	}
}

// TestPushInvalidRemoteAt verifies the malformed timestamp surfaces from Push
// rather than being papered over: the entry keeps its own clock and stays dirty,
// so nothing converges on a fabricated `at`.
func TestPushInvalidRemoteAt(t *testing.T) {
	t.Parallel()
	st, c := setup(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":557,"at":"yesterday"}`))
	})
	start := ts(t, "2026-01-02T09:00:00Z")
	stop := start.Add(5 * time.Minute)
	id := mustCreate(t, st, store.Entry{
		WorkspaceID: 1, Start: start, Stop: &stop, Duration: 300,
		UpdatedAt: stop, Dirty: true,
	})

	res, err := Push(ctx, st, c, ts(t, "2026-01-02T12:00:00Z"))
	if err == nil {
		t.Fatalf("push = %+v, want an error for the malformed remote at", res)
	}
	if res.Created != 0 {
		t.Errorf("created = %d, want 0", res.Created)
	}
	dirty := mustDirty(t, st)
	if len(dirty) != 1 || dirty[0].ID != id {
		t.Fatalf("dirty = %+v, want the entry left for a later push", dirty)
	}
	if !dirty[0].UpdatedAt.Equal(stop) {
		t.Errorf("updated_at = %v, want the untouched local clock %v", dirty[0].UpdatedAt, stop)
	}
}

// TestRoundTrip pushes a fresh local entry, then pulls the server's view of it
// back and asserts convergence (clean, single consistent row).
func TestRoundTrip(t *testing.T) {
	t.Parallel()
	created := false
	st, c := setup(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			created = true
			w.Write([]byte(`{"id":1000,"at":"2026-01-02T10:00:00Z"}`))
		case r.Method == http.MethodGet:
			w.Write([]byte(`[{"id":1000,"workspace_id":1,"description":"",
			  "start":"2026-01-02T09:00:00Z","stop":"2026-01-02T09:05:00Z",
			  "duration":300,"at":"2026-01-02T10:00:00Z"}]`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})

	start := ts(t, "2026-01-02T09:00:00Z")
	stop := start.Add(5 * time.Minute)
	mustCreate(t, st, store.Entry{
		WorkspaceID: 1, Start: start, Stop: &stop, Duration: 300,
		UpdatedAt: stop, Dirty: true,
	})

	if _, err := Push(ctx, st, c, ts(t, "2026-01-02T10:00:00Z")); err != nil {
		t.Fatalf("push: %v", err)
	}
	if !created {
		t.Fatal("expected a create call")
	}
	if _, err := Pull(ctx, st, c, nil, ts(t, "2026-01-01T00:00:00Z"), ts(t, "2026-01-02T12:00:00Z")); err != nil {
		t.Fatalf("pull: %v", err)
	}

	// Converged: exactly one clean entry mirroring remote 1000.
	dirty := mustDirty(t, st)
	if len(dirty) != 0 {
		t.Errorf("dirty entries after round-trip = %d, want 0", len(dirty))
	}
	got := mustEntryByRemoteID(t, st, 1000)
	if got == nil || got.Duration != 300 {
		t.Fatalf("converged entry = %+v", got)
	}
}

// TestPushSkipsFailedEntry is the regression test for a poisoned dirty queue: an
// entry Toggl permanently rejects must not stop the entries behind it from being
// pushed. It stays dirty (so it can be fixed or retried) and is reported, while
// everything else goes through.
func TestPushSkipsFailedEntry(t *testing.T) {
	t.Parallel()
	var attempted []string
	st, c := setup(t, func(w http.ResponseWriter, r *http.Request) {
		body := decodeBody(t, r)
		desc, _ := body["description"].(string)
		attempted = append(attempted, desc)
		if desc == "poison" {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":"task not in project"}`))
			return
		}
		// A distinct remote id per create, since they share one local store.
		fmt.Fprintf(w, `{"id":%d,"at":"2026-01-02T10:00:00Z"}`, 600+len(attempted))
	})

	day := ts(t, "2026-01-02T09:00:00Z")
	var ids []int64
	for i, desc := range []string{"first", "poison", "last"} {
		start := day.Add(time.Duration(i) * time.Hour)
		stop := start.Add(30 * time.Minute)
		id, err := st.CreateEntry(ctx, store.Entry{
			WorkspaceID: 1, Description: desc, Start: start, Stop: &stop,
			Duration: 1800, UpdatedAt: stop, Dirty: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}

	res, err := Push(ctx, st, c, ts(t, "2026-01-02T12:00:00Z"))
	// The push reports the failure, but as a summary of what was left behind.
	var perr *PushError
	if !errors.As(err, &perr) {
		t.Fatalf("err = %v, want a *PushError", err)
	}
	if len(perr.Failures) != 1 || perr.Failures[0].EntryID != ids[1] {
		t.Fatalf("failures = %+v, want just the poisoned entry %d", perr.Failures, ids[1])
	}
	if !strings.Contains(err.Error(), "task not in project") {
		t.Errorf("err = %v, want the server's complaint included", err)
	}
	// Every entry was attempted, and the two good ones were created.
	if want := []string{"first", "poison", "last"}; !equalStrings(attempted, want) {
		t.Errorf("attempted = %v, want %v (the failure must not stop the queue)", attempted, want)
	}
	if res.Created != 2 {
		t.Errorf("created = %d, want 2", res.Created)
	}
	if len(res.Failed) != 1 || res.Failed[0].EntryID != ids[1] {
		t.Errorf("res.Failed = %+v, want the poisoned entry", res.Failed)
	}

	// Only the rejected entry is still dirty; it keeps its local clock and has
	// no remote id, so a later push tries it again.
	dirty, err := st.DirtyEntries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(dirty) != 1 || dirty[0].ID != ids[1] {
		t.Fatalf("dirty = %+v, want only the poisoned entry", dirty)
	}
	if dirty[0].RemoteID != nil {
		t.Errorf("remote_id = %v, want none on a rejected entry", dirty[0].RemoteID)
	}
}

// TestPushSkipsFailedDeleteAndUpdate covers the other two push shapes: a
// deletion Toggl refuses keeps its row (still deleted and dirty, so `tg push`
// retries it) and an update that fails keeps the entry dirty, while an unrelated
// entry in the same run is still sent.
func TestPushSkipsFailedDeleteAndUpdate(t *testing.T) {
	t.Parallel()
	st, c := setup(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodDelete, http.MethodPut:
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`boom`))
		default:
			w.Write([]byte(`{"id":700,"at":"2026-01-02T10:00:00Z"}`))
		}
	})

	day := ts(t, "2026-01-02T09:00:00Z")
	delStop := day.Add(30 * time.Minute)
	delID := mustCreate(t, st, store.Entry{
		RemoteID: ptrInt(801), WorkspaceID: 1, Start: day, Stop: &delStop,
		Duration: 1800, UpdatedAt: delStop, Dirty: true, Deleted: true,
	})
	updStart := day.Add(time.Hour)
	updStop := updStart.Add(30 * time.Minute)
	updID := mustCreate(t, st, store.Entry{
		RemoteID: ptrInt(802), WorkspaceID: 1, Start: updStart, Stop: &updStop,
		Duration: 1800, UpdatedAt: updStop, Dirty: true,
	})
	newStart := day.Add(2 * time.Hour)
	newStop := newStart.Add(30 * time.Minute)
	newID := mustCreate(t, st, store.Entry{
		WorkspaceID: 1, Start: newStart, Stop: &newStop,
		Duration: 1800, UpdatedAt: newStop, Dirty: true,
	})

	res, err := Push(ctx, st, c, ts(t, "2026-01-02T12:00:00Z"))
	if err == nil {
		t.Fatalf("push = %+v, want an error reporting the failures", res)
	}
	if res.Created != 1 || res.Deleted != 0 || res.Updated != 0 {
		t.Errorf("res = %+v, want only the new entry created", res)
	}
	if len(res.Failed) != 2 {
		t.Fatalf("failed = %+v, want the delete and the update", res.Failed)
	}
	// The failed delete kept its row (soft-deleted, dirty) instead of being
	// dropped locally while Toggl still holds it.
	dirty := mustDirty(t, st)
	got := map[int64]store.Entry{}
	for _, e := range dirty {
		got[e.ID] = e
	}
	if len(got) != 2 {
		t.Fatalf("dirty = %+v, want the two failed entries", dirty)
	}
	if e, ok := got[delID]; !ok || !e.Deleted {
		t.Errorf("deleted entry = %+v (present %v), want it kept for a retry", e, ok)
	}
	if _, ok := got[updID]; !ok {
		t.Error("failed update should stay dirty")
	}
	if _, ok := got[newID]; ok {
		t.Error("the created entry should be clean")
	}
}

// TestPushContextCancelAborts verifies cancellation is not treated as a
// per-entry failure to be skipped: the loop stops at once and the context error
// is what surfaces.
func TestPushContextCancelAborts(t *testing.T) {
	t.Parallel()
	var calls int
	cctx, cancel := context.WithCancel(context.Background())
	st, c := setup(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		cancel() // the next entry must not be attempted
		w.Write([]byte(`{"id":900,"at":"2026-01-02T10:00:00Z"}`))
	})

	day := ts(t, "2026-01-02T09:00:00Z")
	for i := 0; i < 3; i++ {
		start := day.Add(time.Duration(i) * time.Hour)
		stop := start.Add(30 * time.Minute)
		if _, err := st.CreateEntry(ctx, store.Entry{
			WorkspaceID: 1, Start: start, Stop: &stop,
			Duration: 1800, UpdatedAt: stop, Dirty: true,
		}); err != nil {
			t.Fatal(err)
		}
	}

	_, err := Push(cctx, st, c, ts(t, "2026-01-02T12:00:00Z"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if calls > 1 {
		t.Errorf("calls = %d, want the loop to stop at the cancel", calls)
	}
}

// TestPullRollsBackOnFailure verifies the pull applies as one transaction: a
// malformed entry half-way through the remote list leaves the store untouched
// rather than half-reconciled, and the watermark is not moved either.
func TestPullRollsBackOnFailure(t *testing.T) {
	t.Parallel()
	st, c := setup(t, func(w http.ResponseWriter, r *http.Request) {
		// The first entry is fine; the second carries an unparsable `at`.
		w.Write([]byte(`[{"id":910,"workspace_id":1,"description":"good",
		  "start":"2026-01-02T09:00:00Z","stop":"2026-01-02T09:30:00Z",
		  "duration":1800,"at":"2026-01-02T09:30:00Z"},
		 {"id":911,"workspace_id":1,"description":"broken",
		  "start":"2026-01-02T10:00:00Z","duration":1800,"at":"yesterday"}]`))
	})

	res, err := Pull(ctx, st, c, nil, ts(t, "2026-01-01T00:00:00Z"), ts(t, "2026-01-02T12:00:00Z"))
	if err == nil {
		t.Fatalf("pull = %+v, want an error for the malformed entry", res)
	}
	if res != (PullResult{}) {
		t.Errorf("res = %+v, want a zero result: nothing was applied", res)
	}
	if got := mustEntryByRemoteID(t, st, 910); got != nil {
		t.Errorf("entry = %+v, want it rolled back with the failed pull", got)
	}
	if _, ok := mustMeta(t, st, store.MetaLastPull); ok {
		t.Error("a failed pull must not advance last_pull")
	}
}

// TestPullContextCancelAborts verifies a cancelled context stops the pull loop
// and rolls back whatever it had applied.
func TestPullContextCancelAborts(t *testing.T) {
	t.Parallel()
	cctx, cancel := context.WithCancel(context.Background())
	st, c := setup(t, func(w http.ResponseWriter, r *http.Request) {
		cancel()
		w.Write([]byte(`[{"id":920,"workspace_id":1,"description":"one",
		  "start":"2026-01-02T09:00:00Z","stop":"2026-01-02T09:30:00Z",
		  "duration":1800,"at":"2026-01-02T09:30:00Z"}]`))
	})

	if _, err := Pull(cctx, st, c, nil, ts(t, "2026-01-01T00:00:00Z"), ts(t, "2026-01-02T12:00:00Z")); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got := mustEntryByRemoteID(t, st, 920); got != nil {
		t.Errorf("entry = %+v, want nothing applied", got)
	}
}

// TestPushErrorMessage pins the summary error's shape: it names the entries and
// carries their errors, so `tg push` says what was left behind.
func TestPushErrorMessage(t *testing.T) {
	t.Parallel()
	one := &PushError{Failures: []PushFailure{{EntryID: 3, Err: "nope"}}}
	if got, want := one.Error(), "1 entry could not be pushed: entry 3: nope"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	two := &PushError{Failures: []PushFailure{
		{EntryID: 3, Err: "nope"},
		{EntryID: 4, RemoteID: ptrInt(77), Err: "boom"},
	}}
	want := "2 entries could not be pushed: entry 3: nope; entry 4 (remote 77): boom"
	if got := two.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func equalStrings(a, b []string) bool {
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
