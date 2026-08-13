// Package togglsync reconciles local time entries with Toggl using a two-way
// last-writer-wins (LWW) strategy keyed on each entry's updated_at vs the
// remote at. Pull brings remote changes down; Push sends local changes up.
// Correct convergence is achieved by running Pull then Push.
//
// The package is named togglsync rather than sync so it does not shadow the
// standard library's sync package in files that need both.
package togglsync

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mantas6/tg/api"
	"github.com/mantas6/tg/store"
)

// PullResult summarizes what a pull reconciled.
type PullResult struct {
	Inserted int `json:"inserted"`
	Updated  int `json:"updated"`
	Deleted  int `json:"deleted"`
	Skipped  int `json:"skipped"` // local-newer entries left for push
}

// PushResult summarizes what a push sent. Failed lists the entries Toggl refused
// (see Push): they are counted nowhere else and are still dirty, so the counts
// plus Failed account for every entry the push looked at.
type PushResult struct {
	Created int           `json:"created"`
	Updated int           `json:"updated"`
	Deleted int           `json:"deleted"`
	Failed  []PushFailure `json:"failed,omitempty"`
}

// PushFailure records one entry a push could not send. EntryID is the local row
// id and RemoteID the Toggl id when the entry has one, so a rejected entry can
// be found again; Err is the server's (or the client's) complaint.
type PushFailure struct {
	EntryID  int64  `json:"entry_id"`
	RemoteID *int64 `json:"remote_id,omitempty"`
	Err      string `json:"error"`
}

// PushError is the error a push with per-entry failures returns: the push itself
// ran to completion (every other entry was sent), so the error reports what was
// left behind rather than aborting the queue.
type PushError struct {
	Failures []PushFailure
}

func (e *PushError) Error() string {
	if len(e.Failures) == 1 {
		return fmt.Sprintf("1 entry could not be pushed: %s", e.Failures[0].describe())
	}
	parts := make([]string, 0, len(e.Failures))
	for _, f := range e.Failures {
		parts = append(parts, f.describe())
	}
	return fmt.Sprintf("%d entries could not be pushed: %s",
		len(e.Failures), strings.Join(parts, "; "))
}

// describe renders one failure for a PushError message.
func (f PushFailure) describe() string {
	if f.RemoteID != nil {
		return fmt.Sprintf("entry %d (remote %d): %s", f.EntryID, *f.RemoteID, f.Err)
	}
	return fmt.Sprintf("entry %d: %s", f.EntryID, f.Err)
}

// Pull fetches remote entries modified since `since`, applies LWW against local
// state, and may advance last_pull to `now`. A non-nil projectID scopes the pull
// to a single project: entries belonging to other projects (or to none) are
// ignored, and last_pull is left untouched so a later full pull still
// reconciles those other projects. Callers pass nil to reconcile every project.
//
// The watermark is likewise left untouched when the WINDOW is partial, i.e.
// when `since` starts after the recorded watermark (see canAdvanceWatermark):
// `tg pull` defaults to today's window, and moving last_pull to `now` would
// claim coverage of the gap between the old watermark and `since` that this
// pull never looked at.
//
// Everything the pull writes — the per-entry reconciliation, the catalog
// self-healing and the watermark — happens in ONE transaction, so a failure
// part-way through the remote list leaves the store exactly as it was instead of
// half-reconciled with a watermark that no longer describes it. The returned
// result is therefore zero when an error is returned: nothing was applied.
func Pull(ctx context.Context, st *store.Store, c *api.Client, projectID *int64, since, now time.Time) (PullResult, error) {
	remotes, err := c.List(ctx, since)
	if err != nil {
		return PullResult{}, err
	}

	var res PullResult
	if err := st.WithTx(ctx, func(tx *store.Store) error {
		var err error
		res, err = apply(ctx, tx, remotes, projectID, since, now)
		return err
	}); err != nil {
		return PullResult{}, err
	}
	return res, nil
}

// apply is Pull's body, run inside the pull's transaction (tx).
func apply(ctx context.Context, tx *store.Store, remotes []api.TimeEntry, projectID *int64, since, now time.Time) (PullResult, error) {
	var res PullResult
	for _, r := range remotes {
		// A cancelled context (Ctrl-C) stops the loop promptly; the
		// transaction is rolled back by WithTx.
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if projectID != nil && (r.ProjectID == nil || *r.ProjectID != *projectID) {
			continue
		}

		local, err := tx.EntryByRemoteID(ctx, r.ID)
		if err != nil {
			return res, err
		}

		mapped, err := toStoreEntry(r)
		if err != nil {
			return res, err
		}

		// Self-heal the catalog from the meta-enriched payload so the entry's
		// project/task titles resolve on display even if the local catalog is
		// stale or was never populated (e.g. before the first `tg update`).
		if err := healCatalog(ctx, tx, r); err != nil {
			return res, err
		}

		switch {
		case local == nil:
			if r.Deleted() {
				res.Skipped++
				continue
			}
			if _, err := tx.CreateEntry(ctx, mapped); err != nil {
				return res, err
			}
			res.Inserted++

		case remoteWins(mapped, *local):
			if r.Deleted() {
				if err := tx.DeleteByRemoteID(ctx, r.ID); err != nil {
					return res, err
				}
				res.Deleted++
				continue
			}
			switch err := tx.UpdateFromRemote(ctx, mapped); {
			case err == nil:
				res.Updated++
			case errors.Is(err, store.ErrEntryNotFound):
				// The mirror read above vanished before this write (only a
				// concurrent tg dropping the row can do that). The remote entry
				// is still authoritative, so re-create it rather than counting
				// an update that never landed.
				if _, err := tx.CreateEntry(ctx, mapped); err != nil {
					return res, err
				}
				res.Inserted++
			default:
				return res, err
			}

		default: // local is newer; keep it for push
			res.Skipped++
		}
	}

	// Only advance the watermark on a full pull; a project-scoped pull is
	// partial and must not hide other projects' changes from a later full
	// pull, and a window that starts after the watermark must not hide the
	// changes made in between.
	if projectID == nil {
		ok, err := canAdvanceWatermark(ctx, tx, since)
		if err != nil {
			return res, err
		}
		if ok {
			if err := tx.SetMeta(ctx, store.MetaLastPull, now.UTC().Format(time.RFC3339)); err != nil {
				return res, err
			}
		}
	}
	return res, nil
}

// remoteWins decides the LWW comparison behind Pull: it reports whether the
// remote version of an entry may overwrite the local one.
//
// The clocks it compares are only second-accurate — Toggl's `at` has no
// sub-second part and store.MarkSynced copies it onto updated_at — so ties are
// common rather than exotic: an edit made in the same second the entry was last
// synced carries the very timestamp the push just wrote. A tie is therefore not
// evidence that the remote is newer, and letting it win would drop a local edit
// before `tg push` ever saw it. So a dirty entry (one with unsynced local
// changes) only yields to a STRICTLY newer remote and is otherwise kept for the
// next push.
//
// A clean entry has nothing to lose: its state came from the server, so a tie
// re-applies what it already holds and the remote is allowed to win, which keeps
// a pull idempotent right after a push.
func remoteWins(remote, local store.Entry) bool {
	if local.Dirty {
		return remote.UpdatedAt.After(local.UpdatedAt)
	}
	return !remote.UpdatedAt.Before(local.UpdatedAt)
}

// canAdvanceWatermark reports whether a pull whose window starts at `since` may
// move last_pull forward without leaving an unreconciled gap behind it. That is
// true only when the window reaches back to (at least) the recorded watermark,
// so the two windows chain: "everything modified since last_pull is
// reconciled" stays a true statement.
//
// An absent (or unparsable) watermark bootstraps to true: there is no coverage
// claim yet that this pull could invalidate.
func canAdvanceWatermark(ctx context.Context, st *store.Store, since time.Time) (bool, error) {
	v, ok, err := st.GetMeta(ctx, store.MetaLastPull)
	if err != nil {
		return false, err
	}
	if !ok {
		return true, nil
	}
	prev, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return true, nil
	}
	return !since.After(prev), nil
}

// Push sends every dirty local entry to Toggl: deletions are DELETEd then
// dropped, new entries are POSTed, existing entries are PUT. now is the fallback
// clock used if the server omits an `at` timestamp; a server timestamp that is
// present but malformed is treated as a failure for that entry (see remoteAt)
// rather than recording a made-up clock.
//
// An entry Toggl refuses is SKIPPED, not fatal: it is recorded in
// PushResult.Failed, left dirty for a later attempt, and the remaining dirty
// entries are still sent. A permanently rejected entry therefore cannot wedge
// the queue behind it — which is what a fail-fast push did, since the same
// entry was retried first every single time. When anything failed, the returned
// error is a *PushError summarizing the failures; the result still reports
// everything that did get through.
//
// A cancelled context is different: it aborts the loop at once and is returned
// as-is, since nothing suggests the next entry would fare better.
func Push(ctx context.Context, st *store.Store, c *api.Client, now time.Time) (PushResult, error) {
	var res PushResult
	dirty, err := st.DirtyEntries(ctx)
	if err != nil {
		return res, err
	}

	for _, e := range dirty {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if err := pushEntry(ctx, st, c, e, now, &res); err != nil {
			if ctx.Err() != nil {
				return res, ctx.Err()
			}
			res.Failed = append(res.Failed, PushFailure{
				EntryID: e.ID, RemoteID: e.RemoteID, Err: err.Error(),
			})
		}
	}
	if len(res.Failed) > 0 {
		return res, &PushError{Failures: res.Failed}
	}
	return res, nil
}

// pushEntry sends a single dirty entry and, on success, records the outcome in
// res. Any error it returns concerns only this entry: the caller keeps the entry
// dirty and carries on with the rest of the queue (see Push).
//
// Local bookkeeping (MarkSynced, DeleteRow) happens only after the remote call
// succeeded, so a failure never leaves an entry looking synced.
func pushEntry(ctx context.Context, st *store.Store, c *api.Client, e store.Entry, now time.Time, res *PushResult) error {
	switch {
	case e.Deleted:
		if e.RemoteID != nil {
			if err := c.Delete(ctx, e.WorkspaceID, *e.RemoteID); err != nil {
				return err
			}
			res.Deleted++
		}
		return st.DeleteRow(ctx, e.ID)

	case e.RemoteID == nil:
		created, err := c.Create(ctx, toAPIEntry(e))
		if err != nil {
			return err
		}
		at, err := remoteAt(created.At, now)
		if err != nil {
			return err
		}
		if err := st.MarkSynced(ctx, e.ID, created.ID, at); err != nil {
			return err
		}
		res.Created++
		return nil

	default:
		updated, err := c.Update(ctx, toAPIEntry(e))
		if err != nil {
			return err
		}
		at, err := remoteAt(updated.At, now)
		if err != nil {
			return err
		}
		if err := st.MarkSynced(ctx, e.ID, updated.ID, at); err != nil {
			return err
		}
		res.Updated++
		return nil
	}
}

// healCatalog upserts the project/task referenced by a (non-deleted) remote
// entry using the names (and project color) from its meta=true payload. Missing
// names or ids are skipped, so this is a no-op when meta is absent. A task is
// only healed when its project id is known, since the catalog requires it.
func healCatalog(ctx context.Context, st *store.Store, r api.TimeEntry) error {
	if r.Deleted() {
		return nil
	}
	if r.ProjectID != nil && r.ProjectName != "" {
		if err := st.UpsertProject(ctx, store.Project{
			ID:          *r.ProjectID,
			WorkspaceID: r.WorkspaceID,
			Name:        r.ProjectName,
			Color:       r.ProjectColor,
			Active:      true,
		}); err != nil {
			return err
		}
	}
	if r.TaskID != nil && r.ProjectID != nil && r.TaskName != "" {
		if err := st.UpsertTask(ctx, store.Task{
			ID:          *r.TaskID,
			WorkspaceID: r.WorkspaceID,
			ProjectID:   *r.ProjectID,
			Name:        r.TaskName,
			Active:      true,
		}); err != nil {
			return err
		}
	}
	return nil
}

// --- conversions -------------------------------------------------------------

// toStoreEntry maps a remote entry to a clean local entry whose LWW clocks
// (updated_at, synced_at) are pinned to the remote `at`.
func toStoreEntry(r api.TimeEntry) (store.Entry, error) {
	start, err := time.Parse(time.RFC3339, r.Start)
	if err != nil {
		return store.Entry{}, err
	}
	at, err := time.Parse(time.RFC3339, r.At)
	if err != nil {
		return store.Entry{}, err
	}
	e := store.Entry{
		RemoteID:    &r.ID,
		WorkspaceID: r.WorkspaceID,
		ProjectID:   r.ProjectID,
		TaskID:      r.TaskID,
		Description: r.Description,
		Start:       start,
		Duration:    r.Duration,
		Billable:    r.Billable,
		UpdatedAt:   at,
		SyncedAt:    &at,
		Dirty:       false,
		Deleted:     r.Deleted(),
	}
	if r.Stop != nil && *r.Stop != "" {
		stop, err := time.Parse(time.RFC3339, *r.Stop)
		if err != nil {
			return store.Entry{}, err
		}
		e.Stop = &stop
	}
	return e, nil
}

// toAPIEntry maps a local entry to an API time entry for create/update.
func toAPIEntry(e store.Entry) api.TimeEntry {
	te := api.TimeEntry{
		WorkspaceID: e.WorkspaceID,
		ProjectID:   e.ProjectID,
		TaskID:      e.TaskID,
		Description: e.Description,
		Start:       e.Start.UTC().Format(time.RFC3339),
		Duration:    e.Duration,
		Billable:    e.Billable,
	}
	if e.RemoteID != nil {
		te.ID = *e.RemoteID
	}
	if e.Stop != nil {
		stop := e.Stop.UTC().Format(time.RFC3339)
		te.Stop = &stop
	}
	return te
}

// remoteAt parses the server's `at` into the clock a push records locally. An
// absent `at` falls back to now: the server reported no timestamp, so the local
// one is the best available and is at least in the same order of events.
//
// A malformed `at` is an error instead. Substituting now for a timestamp the
// server DID send would invent an LWW clock unrelated to the server's, and
// MarkSynced would store it as both updated_at and synced_at — poisoning every
// later comparison against the real remote `at` (see remoteWins), silently and
// in whichever direction the skew happens to point.
func remoteAt(at string, now time.Time) (time.Time, error) {
	if at == "" {
		return now, nil
	}
	t, err := time.Parse(time.RFC3339, at)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid remote timestamp %q: %w", at, err)
	}
	return t, nil
}
