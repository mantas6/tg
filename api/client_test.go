package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestClient spins up an httptest.Server using handler and returns a Client
// pointed at it.
func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return New("mytoken", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
}

// newTestClientReports spins up an httptest.Server and returns a Client whose
// Reports API base URL points at it (used for the reports endpoints).
func newTestClientReports(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return New("mytoken", WithReportsBaseURL(srv.URL), WithHTTPClient(srv.Client()))
}

// ctx is the context every client call in these tests runs under.
var ctx = context.Background()

func ptrInt(v int64) *int64 { return &v }

// decodeBody unmarshals a JSON request body captured by one of the stub handlers
// above. It is called from the server's goroutine, so it reports failures with
// t.Errorf rather than t.Fatalf (which may only be called from the test's own
// goroutine) — a malformed body then fails the test instead of quietly leaving
// the captured map empty and every assertion on it comparing against nil.
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

func TestBasicAuthHeader(t *testing.T) {
	t.Parallel()
	var gotAuth string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte(`{}`))
	})
	if _, err := c.Me(ctx); err != nil {
		t.Fatalf("Me: %v", err)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("mytoken:api_token"))
	if gotAuth != want {
		t.Errorf("Authorization = %q, want %q", gotAuth, want)
	}
}

func TestMeParsesDefaultWorkspace(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/me" {
			t.Errorf("path = %q, want /me", r.URL.Path)
		}
		w.Write([]byte(`{"id":99,"default_workspace_id":12345,"fullname":"A B"}`))
	})
	me, err := c.Me(ctx)
	if err != nil {
		t.Fatalf("Me: %v", err)
	}
	if me.DefaultWorkspaceID != 12345 {
		t.Errorf("default workspace = %d, want 12345", me.DefaultWorkspaceID)
	}
}

func TestCurrentHandlesNull(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/me/time_entries/current" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Write([]byte(`null`))
	})
	te, err := c.Current(ctx)
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if te != nil {
		t.Errorf("Current = %+v, want nil", te)
	}
}

func TestCurrentReturnsEntry(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":7,"workspace_id":1,"duration":-1,"start":"2026-01-02T09:00:00Z"}`))
	})
	te, err := c.Current(ctx)
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if te == nil || te.ID != 7 {
		t.Fatalf("Current = %+v, want id 7", te)
	}
}

func TestListQuery(t *testing.T) {
	t.Parallel()
	var gotQuery string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Write([]byte(`[]`))
	})
	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := c.List(ctx, since); err != nil {
		t.Fatalf("List: %v", err)
	}
	if !strings.Contains(gotQuery, fmt.Sprintf("since=%d", since.Unix())) {
		t.Errorf("query %q missing since=%d", gotQuery, since.Unix())
	}
	if !strings.Contains(gotQuery, "meta=true") {
		t.Errorf("query %q missing meta=true", gotQuery)
	}
}

func TestCreateBody(t *testing.T) {
	t.Parallel()
	var body map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/workspaces/1/time_entries" {
			t.Errorf("path = %q", r.URL.Path)
		}
		body = decodeBody(t, r)
		w.Write([]byte(`{"id":555,"workspace_id":1,"at":"2026-01-02T09:00:00Z"}`))
	})

	out, err := c.Create(ctx, TimeEntry{
		WorkspaceID: 1, ProjectID: ptrInt(20), TaskID: ptrInt(30),
		Start: "2026-01-02T09:00:00Z", Duration: -1, Billable: true,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if out.ID != 555 {
		t.Errorf("returned id = %d, want 555", out.ID)
	}
	if body["workspace_id"] != float64(1) {
		t.Errorf("workspace_id = %v", body["workspace_id"])
	}
	if body["start"] != "2026-01-02T09:00:00Z" {
		t.Errorf("start = %v", body["start"])
	}
	if body["duration"] != float64(-1) {
		t.Errorf("duration = %v, want -1", body["duration"])
	}
	if body["created_with"] != "tg" {
		t.Errorf("created_with = %v, want tg", body["created_with"])
	}
	if body["task_id"] != float64(30) {
		t.Errorf("task_id = %v, want 30", body["task_id"])
	}
	if body["project_id"] != float64(20) {
		t.Errorf("project_id = %v, want 20", body["project_id"])
	}
	if body["billable"] != true {
		t.Errorf("billable = %v, want true", body["billable"])
	}
}

func TestUpdateMethodPath(t *testing.T) {
	t.Parallel()
	var method, path string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		w.Write([]byte(`{"id":42}`))
	})
	if _, err := c.Update(ctx, TimeEntry{ID: 42, WorkspaceID: 1, Start: "2026-01-02T09:00:00Z", Duration: 300}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if method != http.MethodPut || path != "/workspaces/1/time_entries/42" {
		t.Errorf("Update -> %s %s, want PUT /workspaces/1/time_entries/42", method, path)
	}
}

func TestStopMethodPath(t *testing.T) {
	t.Parallel()
	var method, path string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		w.Write([]byte(`{"id":42}`))
	})
	if _, err := c.Stop(ctx, 1, 42); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if method != http.MethodPatch || path != "/workspaces/1/time_entries/42/stop" {
		t.Errorf("Stop -> %s %s, want PATCH .../42/stop", method, path)
	}
}

func TestDeleteMethodPath(t *testing.T) {
	t.Parallel()
	var method, path string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		w.WriteHeader(http.StatusOK)
	})
	if err := c.Delete(ctx, 1, 42); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if method != http.MethodDelete || path != "/workspaces/1/time_entries/42" {
		t.Errorf("Delete -> %s %s, want DELETE /workspaces/1/time_entries/42", method, path)
	}
}

func TestProjectsPagination(t *testing.T) {
	t.Parallel()
	// First page returns a full batch (perPage items); second page is short,
	// terminating the loop.
	var pages []string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		pages = append(pages, r.URL.Query().Get("page"))
		if r.URL.Query().Get("active") != "true" {
			t.Errorf("active = %q, want true", r.URL.Query().Get("active"))
		}
		page := r.URL.Query().Get("page")
		w.Write([]byte(pageProjects(page)))
	})
	projects, err := c.Projects(ctx, 1, false)
	if err != nil {
		t.Fatalf("Projects: %v", err)
	}
	if len(projects) != perPage+2 {
		t.Errorf("projects = %d, want %d", len(projects), perPage+2)
	}
	if len(pages) != 2 || pages[0] != "1" || pages[1] != "2" {
		t.Errorf("requested pages = %v, want [1 2]", pages)
	}
}

func TestTasksActiveBoth(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("active"); got != "both" {
			t.Errorf("active = %q, want both", got)
		}
		if r.URL.Path != "/workspaces/1/tasks" {
			t.Errorf("path = %q", r.URL.Path)
		}
		// The tasks endpoint wraps results in a paginated envelope.
		w.Write([]byte(`{"data":[{"id":1,"name":"T"}],"total_count":1,"per_page":200}`))
	})
	tasks, err := c.Tasks(ctx, 1, true)
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Errorf("tasks = %d, want 1", len(tasks))
	}
	if tasks[0].Name != "T" {
		t.Errorf("task name = %q, want T", tasks[0].Name)
	}
}

func TestTasksPaginationEnvelope(t *testing.T) {
	t.Parallel()
	// First page is a full batch (perPage items) inside the envelope; the
	// second page is short, terminating the walk.
	var pages []string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		pages = append(pages, page)
		w.Write([]byte(pageTasks(page)))
	})
	tasks, err := c.Tasks(ctx, 1, false)
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if len(tasks) != perPage+2 {
		t.Errorf("tasks = %d, want %d", len(tasks), perPage+2)
	}
	if len(pages) != 2 || pages[0] != "1" || pages[1] != "2" {
		t.Errorf("requested pages = %v, want [1 2]", pages)
	}
}

func TestProjectByID(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/workspaces/1/projects/42" {
			t.Errorf("path = %q, want /workspaces/1/projects/42", r.URL.Path)
		}
		w.Write([]byte(`{"id":42,"workspace_id":1,"name":"Payments","billable":true,"active":true}`))
	})
	p, err := c.Project(ctx, 1, 42)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if p.ID != 42 || p.Name != "Payments" || !p.Billable {
		t.Errorf("project = %+v, want id 42 Payments billable", p)
	}
}

func TestProjectTasksBareArray(t *testing.T) {
	t.Parallel()
	// The project-scoped tasks endpoint is NOT paginated: it returns every task
	// as a single bare JSON array. ProjectTasks must issue exactly one request
	// and must not send page/per_page (walking pages would loop forever, since
	// the endpoint ignores them and returns the full list for every page).
	var calls int
	var gotActive, gotPage string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/workspaces/1/projects/42/tasks") {
			t.Errorf("path = %q, want project tasks endpoint", r.URL.Path)
		}
		calls++
		gotActive = r.URL.Query().Get("active")
		gotPage = r.URL.Query().Get("page")
		w.Write([]byte(projectTasksBare()))
	})
	tasks, err := c.ProjectTasks(ctx, 1, 42, true)
	if err != nil {
		t.Fatalf("ProjectTasks: %v", err)
	}
	if calls != 1 {
		t.Errorf("requests = %d, want 1 (endpoint is not paginated)", calls)
	}
	if gotPage != "" {
		t.Errorf("page = %q, want no page param (endpoint is not paginated)", gotPage)
	}
	// includeInactive omits the active filter so inactive tasks are returned.
	if gotActive != "" {
		t.Errorf("active = %q, want empty when including inactive", gotActive)
	}
	if len(tasks) != perPage+1 {
		t.Errorf("tasks = %d, want %d", len(tasks), perPage+1)
	}
}

func TestProjectTasksActiveFilter(t *testing.T) {
	t.Parallel()
	var gotActive string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotActive = r.URL.Query().Get("active")
		w.Write([]byte(`[]`))
	})
	if _, err := c.ProjectTasks(ctx, 1, 42, false); err != nil {
		t.Fatalf("ProjectTasks: %v", err)
	}
	if gotActive != "true" {
		t.Errorf("active = %q, want true when excluding inactive", gotActive)
	}
}

// projectTasksBare renders the project-scoped tasks endpoint response: a single
// bare JSON array holding every task (the endpoint is not paginated), here more
// than perPage items to prove ProjectTasks no longer walks pages.
func projectTasksBare() string {
	var items []string
	for i := 0; i < perPage+1; i++ {
		items = append(items, fmt.Sprintf(`{"id":%d,"project_id":42,"name":"T%d","active":true}`, i+1, i+1))
	}
	return "[" + strings.Join(items, ",") + "]"
}

func TestSummaryByTask(t *testing.T) {
	t.Parallel()
	var gotMethod, gotPath string
	var body map[string]any
	c := newTestClientReports(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		body = decodeBody(t, r)
		// Two projects; the second repeats task 10, whose seconds must be summed
		// across groups. A sub-group without a task id is dropped, but one that
		// has an id and no title (what the endpoint usually sends) is kept.
		w.Write([]byte(`{"groups":[
		  {"id":1,"sub_groups":[
		    {"id":10,"title":"Fix login bug","seconds":4500},
		    {"id":12,"title":"Code review","seconds":2700},
		    {"id":15,"seconds":1200},
		    {"id":null,"title":"","seconds":600}]},
		  {"id":2,"sub_groups":[
		    {"id":10,"title":"Fix login bug","seconds":1800}]}]}`))
	})

	tasks, err := c.SummaryByTask(ctx, 1, "", "2026-01-02")
	if err != nil {
		t.Fatalf("SummaryByTask: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if gotPath != "/workspace/1/summary/time_entries" {
		t.Errorf("path = %q, want /workspace/1/summary/time_entries", gotPath)
	}
	// An empty start date defaults to the Reports API's earliest allowed date.
	if body["start_date"] != reportAllTimeStart {
		t.Errorf("start_date = %v, want %q", body["start_date"], reportAllTimeStart)
	}
	if body["end_date"] != "2026-01-02" {
		t.Errorf("end_date = %v, want 2026-01-02", body["end_date"])
	}
	if body["grouping"] != "projects" || body["sub_grouping"] != "tasks" {
		t.Errorf("grouping/sub_grouping = %v/%v, want projects/tasks", body["grouping"], body["sub_grouping"])
	}

	// Task 10 (summed across the two groups), task 12 and the titleless task
	// 15; only the task-less sub-group is dropped.
	if len(tasks) != 3 {
		t.Fatalf("tasks = %d (%+v), want 3", len(tasks), tasks)
	}
	byID := map[int64]SummaryTask{}
	for _, tk := range tasks {
		byID[tk.TaskID] = tk
	}
	if got := byID[10]; got.Seconds != 6300 || got.Name != "Fix login bug" {
		t.Errorf("task 10 = %+v, want Fix login bug / 6300s", got)
	}
	if got := byID[12]; got.Seconds != 2700 || got.Name != "Code review" {
		t.Errorf("task 12 = %+v, want Code review / 2700s", got)
	}
	// A titled-less sub-group is kept: its name is resolved from the local
	// catalog by id (see cmdTotal), which is what `tg total` matches against.
	if got, ok := byID[15]; !ok || got.Seconds != 1200 || got.Name != "" {
		t.Errorf("task 15 = %+v (present %v), want an untitled 1200s row", got, ok)
	}
}

func TestErrorMapping(t *testing.T) {
	t.Parallel()
	// 401 and 403 are kept apart: only the former means "the token is no good".
	t.Run("unauthorized", func(t *testing.T) {
		t.Parallel()
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`token expired`))
		})
		_, err := c.Me(ctx)
		if !errors.Is(err, ErrUnauthorized) {
			t.Errorf("err = %v, want ErrUnauthorized", err)
		}
		if errors.Is(err, ErrForbidden) {
			t.Errorf("err = %v, should not be ErrForbidden", err)
		}
		// The server's own message is carried through so the failure is
		// diagnosable rather than a bare sentinel.
		if err == nil || !strings.Contains(err.Error(), "token expired") {
			t.Errorf("err = %v, want the response body included", err)
		}
	})
	t.Run("forbidden", func(t *testing.T) {
		t.Parallel()
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`no access to workspace`))
		})
		_, err := c.Me(ctx)
		if !errors.Is(err, ErrForbidden) {
			t.Errorf("err = %v, want ErrForbidden", err)
		}
		if errors.Is(err, ErrUnauthorized) {
			t.Errorf("err = %v, should not be ErrUnauthorized", err)
		}
		if err == nil || !strings.Contains(err.Error(), "no access to workspace") {
			t.Errorf("err = %v, want the response body included", err)
		}
	})
	t.Run("server error", func(t *testing.T) {
		t.Parallel()
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"error":"boom"}`))
		})
		_, err := c.Me(ctx)
		if err == nil || !strings.Contains(err.Error(), "500") {
			t.Errorf("err = %v, want status 500", err)
		}
	})
	t.Run("non-json body", func(t *testing.T) {
		t.Parallel()
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
			w.Write([]byte(`<html>down</html>`))
		})
		_, err := c.Me(ctx)
		if err == nil || !strings.Contains(err.Error(), "down") {
			t.Errorf("err = %v, want body text", err)
		}
	})
}

// TestRetriesRateLimit verifies a 429 is retried and that the retry can
// succeed, so a brief brush with Toggl's limiter is invisible to the caller.
func TestRetriesRateLimit(t *testing.T) {
	t.Parallel()
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(`{"id":99,"default_workspace_id":7}`))
	}))
	defer srv.Close()
	c := New("mytoken", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()),
		WithRetry(3, time.Millisecond))

	me, err := c.Me(ctx)
	if err != nil {
		t.Fatalf("Me: %v", err)
	}
	if me.DefaultWorkspaceID != 7 {
		t.Errorf("workspace = %d, want 7", me.DefaultWorkspaceID)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2 (one retry)", calls)
	}
}

// TestRetryGivesUp verifies the retries are bounded and the final 429 is
// reported with its body, rather than being retried forever.
func TestRetryGivesUp(t *testing.T) {
	t.Parallel()
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`slow down`))
	}))
	defer srv.Close()
	c := New("mytoken", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()),
		WithRetry(3, time.Millisecond))

	_, err := c.Me(ctx)
	if err == nil || !strings.Contains(err.Error(), "429") || !strings.Contains(err.Error(), "slow down") {
		t.Errorf("err = %v, want a 429 error carrying the body", err)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3 attempts", calls)
	}
}

// TestRetryHonorsRetryAfter verifies the server's Retry-After is what shapes the
// wait (and that it is capped): a 0-second header retries immediately.
func TestRetryHonorsRetryAfter(t *testing.T) {
	t.Parallel()
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	// An hour of fixed backoff would hang the test if Retry-After were ignored.
	c := New("mytoken", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()),
		WithRetry(2, time.Hour))
	if _, err := c.Me(ctx); err != nil {
		t.Fatalf("Me: %v", err)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
}

func TestRetryWait(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		header http.Header
		want   time.Duration
	}{
		{"absent", http.Header{}, defaultBackoff},
		{"nil", nil, defaultBackoff},
		{"seconds", http.Header{"Retry-After": []string{"2"}}, 2 * time.Second},
		{"capped", http.Header{"Retry-After": []string{"600"}}, maxBackoff},
		{"http date is ignored", http.Header{"Retry-After": []string{"Wed, 21 Oct 2026 07:28:00 GMT"}}, defaultBackoff},
		{"negative is ignored", http.Header{"Retry-After": []string{"-5"}}, defaultBackoff},
	} {
		if got := retryWait(tc.header, defaultBackoff); got != tc.want {
			t.Errorf("%s: retryWait = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestContextCancelAbortsRequest verifies the context reaches the transport, so
// a Ctrl-C during a slow call returns instead of waiting out the HTTP timeout.
func TestContextCancelAbortsRequest(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	defer close(release)

	c := New("mytoken", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
	cctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	if _, err := c.Me(cctx); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

// TestContextCancelAbortsBackoff verifies a cancelled context cuts the
// rate-limit wait short instead of sleeping through it.
func TestContextCancelAbortsBackoff(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := New("mytoken", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()),
		WithRetry(3, time.Hour))

	cctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	if _, err := c.Me(cctx); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("waited %v, want the backoff cut short by the cancel", elapsed)
	}
}

// pageProjects renders page 1 as a full batch and page 2 as a short final page.
func pageProjects(page string) string {
	var items []string
	switch page {
	case "1":
		for i := 0; i < perPage; i++ {
			items = append(items, fmt.Sprintf(`{"id":%d,"name":"P%d","active":true}`, i+1, i+1))
		}
	case "2":
		items = []string{`{"id":1001,"name":"X","active":true}`, `{"id":1002,"name":"Y","active":true}`}
	}
	return "[" + strings.Join(items, ",") + "]"
}

// pageTasks renders the paginated tasks envelope: page 1 is a full batch and
// page 2 is the short final page.
func pageTasks(page string) string {
	var items []string
	switch page {
	case "1":
		for i := 0; i < perPage; i++ {
			items = append(items, fmt.Sprintf(`{"id":%d,"name":"T%d","active":true}`, i+1, i+1))
		}
	case "2":
		items = []string{`{"id":9001,"name":"X","active":true}`, `{"id":9002,"name":"Y","active":true}`}
	}
	return fmt.Sprintf(`{"data":[%s],"total_count":%d,"per_page":%d}`,
		strings.Join(items, ","), perPage+2, perPage)
}
