// Package store is the local-first persistence layer for tg. It wraps a
// SQLite database holding tracked time entries plus a read-only mirror of the
// Toggl task/project catalog. All times are stored as RFC3339 UTC strings;
// durations are integer seconds (-1 marks a running entry).
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Meta keys persisted in the meta table.
const (
	MetaSchemaVersion = "schema_version"
	MetaLastPull      = "last_pull"
)

// Layouts the store renders calendar days with in its error messages. Stored
// times are always RFC3339 UTC (see the package doc); these two are for humans
// only, so a refused edit names the day (and, where it helps, the time) the way
// the CLI prints it.
const (
	dateLayout     = "2006-01-02"
	dateTimeLayout = "2006-01-02 15:04"
)

// Store is a handle to the SQLite database. loc is the calendar the store
// reckons days in: it decides which day an entry's start belongs to, and hence
// which per-day numbering (see Entry.Seq) it takes part in. It is time.Local
// for the CLI and pinned explicitly by tests.
//
// ex is what every statement runs against: the *sql.DB normally, and the
// *sql.Tx of the transaction in progress inside WithTx — which is the whole
// trick that lets one method body serve both cases.
type Store struct {
	db  *sql.DB
	ex  execer
	loc *time.Location
}

// execer is the part of *sql.DB and *sql.Tx the store's statements use. Only the
// context-taking variants are listed, so a cancelled context (Ctrl-C, see main)
// stops a query rather than being ignored.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// WithTx runs fn inside a single transaction, committing when fn returns nil and
// rolling back on any error, so a multi-statement write either lands whole or
// not at all. fn receives a *Store bound to the transaction: every call it makes
// on that store joins the transaction, and the receiver it was called on must
// not be used inside fn (its statements would run outside).
//
// A nested call — a method that wraps itself in WithTx, invoked from inside
// another WithTx — reuses the transaction already in progress rather than
// starting one (SQLite has no nested transactions). That is what lets
// UpdateFromRemote be atomic on its own and still take part in the larger
// transaction a pull wraps around it, instead of committing halfway through it.
func (s *Store) WithTx(ctx context.Context, fn func(tx *Store) error) error {
	if _, ok := s.ex.(*sql.Tx); ok {
		return fn(s)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	if err := fn(&Store{db: s.db, ex: tx, loc: s.loc}); err != nil {
		// The rollback's own error is subordinate to what actually went
		// wrong, so fn's error is the one reported. It is also passed through
		// unwrapped: fn's errors already name the operation that failed (and
		// carry the store's sentinels, which callers test with errors.Is), so
		// a "transaction:" prefix would add nothing but noise.
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// Entry is a tracked time entry. RemoteID/ProjectID/TaskID/Stop/SyncedAt are
// nil when unset. TaskName and ProjectName are populated from a catalog join on
// read and are not persisted directly.
type Entry struct {
	ID          int64
	RemoteID    *int64
	WorkspaceID int64
	ProjectID   *int64
	TaskID      *int64
	Description string
	Start       time.Time
	Stop        *time.Time
	Duration    int64 // seconds; -1 while running
	Billable    bool
	Seq         int // per-day entry number shown by `tg ls`; see CreateEntry
	UpdatedAt   time.Time
	SyncedAt    *time.Time
	Dirty       bool
	Deleted     bool

	TaskName     string // joined, display only
	ProjectName  string // joined, display only
	ProjectColor string // joined, display only; "#RRGGBB" hex
}

// Running reports whether the entry is currently running: it has no stop time
// yet, or it carries Toggl's negative-duration running marker (see the package
// doc). Either signal on its own is enough, since a pull can bring down a row
// that only has one of them.
//
// This is the single definition of "running" in tg: runningWhere is its SQL
// twin, and every query that needs the predicate is built from it, so the Go
// and SQL answers can never drift apart.
func (e Entry) Running() bool { return e.Duration < 0 || e.Stop == nil }

// runningWhere is Entry.Running as a SQL predicate over the `e` alias of
// entrySelect. Keep the two in lockstep.
const runningWhere = "(e.stop IS NULL OR e.duration < 0)"

// Project mirrors a Toggl project. Billable is carried through to entries
// created against the project so workspaces that forbid non-billable entries in
// billable projects accept them.
type Project struct {
	ID          int64
	WorkspaceID int64
	Name        string
	Color       string
	ClientName  string
	Active      bool
	Billable    bool
	At          string
}

// Task mirrors a Toggl task. ProjectName is populated from a catalog join on
// read (display only) and is not persisted directly.
type Task struct {
	ID          int64
	WorkspaceID int64
	ProjectID   int64
	Name        string
	Active      bool
	At          string

	ProjectName string // joined, display only
}

// Open opens (creating if needed) the SQLite database at path and applies the
// schema, reckoning calendar days in time.Local. WAL + a busy timeout keep
// concurrent CLI invocations well behaved.
func Open(ctx context.Context, path string) (*Store, error) {
	return OpenIn(ctx, path, time.Local)
}

// OpenIn is Open with an explicit calendar (see Store.loc): entry days — and
// therefore the per-day numbering `tg ls` shows — are computed in loc. A nil
// loc means time.Local. Callers must pass the same location they render with,
// or a listing's numbers will not match the ones `mod`/`del` resolve.
//
// ctx bounds the schema migration run here; later calls carry their own.
func OpenIn(ctx context.Context, path string, loc *time.Location) (*Store, error) {
	if loc == nil {
		loc = time.Local
	}
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database %s: %w", path, err)
	}
	s := &Store{db: db, ex: db, loc: loc}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate database %s: %w", path, err)
	}
	return s, nil
}

// Close releases the underlying database handle.
func (s *Store) Close() error { return s.db.Close() }

// Location returns the calendar the store reckons entry days in (see OpenIn).
func (s *Store) Location() *time.Location {
	if s.loc == nil {
		return time.Local
	}
	return s.loc
}

// --- time helpers -----------------------------------------------------------

func fmtTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func parseTime(s string) (time.Time, error) { return time.Parse(time.RFC3339, s) }

func nullTime(p *time.Time) any {
	if p == nil {
		return nil
	}
	return fmtTime(*p)
}

func nullInt(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

// --- entry reads -------------------------------------------------------------

// entrySelect lists every entry column plus the joined task/project display
// fields (task name, project name and color).
const entrySelect = `
SELECT e.id, e.remote_id, e.workspace_id, e.project_id, e.task_id,
       e.description, e.start, e.stop, e.duration, e.billable, e.seq,
       e.updated_at, e.synced_at, e.dirty, e.deleted, t.name, p.name, p.color
FROM entries e
LEFT JOIN tasks t ON t.id = e.task_id
LEFT JOIN projects p ON p.id = e.project_id
`

func scanEntry(sc interface{ Scan(...any) error }) (Entry, error) {
	var (
		e         Entry
		remoteID  sql.NullInt64
		projectID sql.NullInt64
		taskID    sql.NullInt64
		start     string
		stop      sql.NullString
		updatedAt string
		syncedAt  sql.NullString
		taskName  sql.NullString
		projName  sql.NullString
		projColor sql.NullString
	)
	if err := sc.Scan(&e.ID, &remoteID, &e.WorkspaceID, &projectID, &taskID,
		&e.Description, &start, &stop, &e.Duration, &e.Billable, &e.Seq, &updatedAt,
		&syncedAt, &e.Dirty, &e.Deleted, &taskName, &projName, &projColor); err != nil {
		return Entry{}, err
	}
	if remoteID.Valid {
		e.RemoteID = &remoteID.Int64
	}
	if projectID.Valid {
		e.ProjectID = &projectID.Int64
	}
	if taskID.Valid {
		e.TaskID = &taskID.Int64
	}
	// The stored timestamps are named in the parse failures: a row with a
	// malformed time is a stored-data problem, so which column of which entry
	// holds it is the whole diagnosis.
	var err error
	if e.Start, err = parseTime(start); err != nil {
		return Entry{}, fmt.Errorf("entry %d: parse start: %w", e.ID, err)
	}
	if stop.Valid {
		t, err := parseTime(stop.String)
		if err != nil {
			return Entry{}, fmt.Errorf("entry %d: parse stop: %w", e.ID, err)
		}
		e.Stop = &t
	}
	if e.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return Entry{}, fmt.Errorf("entry %d: parse updated_at: %w", e.ID, err)
	}
	if syncedAt.Valid {
		t, err := parseTime(syncedAt.String)
		if err != nil {
			return Entry{}, fmt.Errorf("entry %d: parse synced_at: %w", e.ID, err)
		}
		e.SyncedAt = &t
	}
	e.TaskName = taskName.String
	e.ProjectName = projName.String
	e.ProjectColor = projColor.String
	return e, nil
}

// Running returns the single running entry (see Entry.Running; deleted rows are
// ignored) or nil. Should the store hold more than one — running rows only
// arrive from pulls, which cannot rule that out — the newest by start wins.
func (s *Store) Running(ctx context.Context) (*Entry, error) {
	row := s.ex.QueryRowContext(ctx, entrySelect+
		" WHERE "+runningWhere+" AND e.deleted = 0 ORDER BY e.start DESC LIMIT 1")
	e, err := scanEntry(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("running entry: %w", err)
	}
	return &e, nil
}

// LastEntry is the one place tg resolves what "the last entry" means: both
// `tg status` and a bare `tg mod` (no entry number) take their subject from
// here. It returns the most recently started non-deleted entry that has already
// begun on now's calendar day — reckoned in the store's location, see OpenIn —
// or nil when that day holds none.
//
// Two filters are what make it the last thing actually tracked rather than the
// newest row in the table:
//
//   - Today only. An entry from an earlier day is never returned, so a new day
//     starts without a last entry instead of reaching back into history. That
//     also keeps the implicit target of `tg mod` inside the only day an edit may
//     touch (see CheckEditableDay), so the two can never disagree.
//   - No future starts. An entry booked for later today has not happened yet, so
//     it is skipped by its START datetime: with something starting three hours
//     from now, the last entry is still the one before it. Its stop is
//     irrelevant, only where it begins.
//
// A running entry is an ordinary candidate here (it started in the past);
// `tg status` separately prefers one over a newer finished entry (see Running).
func (s *Store) LastEntry(ctx context.Context, now time.Time) (*Entry, error) {
	from := dayStart(now, s.Location())
	row := s.ex.QueryRowContext(ctx, entrySelect+`
WHERE e.deleted = 0 AND e.start >= ? AND e.start <= ?
ORDER BY e.start DESC, e.id DESC LIMIT 1`, fmtTime(from), fmtTime(now))
	e, err := scanEntry(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("last entry: %w", err)
	}
	return &e, nil
}

// EntriesBetween returns non-deleted entries with start in [from, to), ordered
// by start ascending.
func (s *Store) EntriesBetween(ctx context.Context, from, to time.Time) ([]Entry, error) {
	rows, err := s.ex.QueryContext(ctx, entrySelect+
		" WHERE e.deleted = 0 AND e.start >= ? AND e.start < ? ORDER BY e.start ASC",
		fmtTime(from), fmtTime(to))
	if err != nil {
		return nil, fmt.Errorf("list entries: %w", err)
	}
	defer rows.Close()
	return collectEntries(rows)
}

// FindOverlapping returns the non-deleted entries whose tracked interval
// intersects the half-open range [start, stop), ordered by start ascending. It
// backs the overlap guard in `tg add`: an empty result means the range is free.
//
// Intervals are half-open, so entries that merely touch the range do not
// overlap it (an entry ending exactly at start, or starting exactly at stop, is
// fine). A running entry (see Entry.Running) has no end yet, so it is treated
// as open-ended and overlaps whenever it began before stop.
func (s *Store) FindOverlapping(ctx context.Context, start, stop time.Time) ([]Entry, error) {
	return s.FindOverlappingExcluding(ctx, start, stop, 0)
}

// FindOverlappingExcluding is FindOverlapping with one entry left out of the
// search by local id. It backs the overlap guard in `tg mod`, which must not
// see the entry it is retiming as a conflict with itself. Ids start at 1, so
// excludeID 0 excludes nothing (that is what FindOverlapping passes).
func (s *Store) FindOverlappingExcluding(ctx context.Context, start, stop time.Time, excludeID int64) ([]Entry, error) {
	rows, err := s.ex.QueryContext(ctx, entrySelect+`
WHERE e.deleted = 0
  AND e.id <> ?
  AND e.start < ?
  AND (`+runningWhere+` OR e.stop > ?)
ORDER BY e.start ASC`,
		excludeID, fmtTime(stop), fmtTime(start))
	if err != nil {
		return nil, fmt.Errorf("find overlapping entries: %w", err)
	}
	defer rows.Close()
	return collectEntries(rows)
}

// DirtyEntries returns every entry with unsynced local changes, oldest first.
func (s *Store) DirtyEntries(ctx context.Context) ([]Entry, error) {
	rows, err := s.ex.QueryContext(ctx, entrySelect+" WHERE e.dirty = 1 ORDER BY e.start ASC")
	if err != nil {
		return nil, fmt.Errorf("list dirty entries: %w", err)
	}
	defer rows.Close()
	return collectEntries(rows)
}

// EntryByRemoteID returns the entry mirroring the given Toggl id, or nil.
func (s *Store) EntryByRemoteID(ctx context.Context, remoteID int64) (*Entry, error) {
	row := s.ex.QueryRowContext(ctx, entrySelect+" WHERE e.remote_id = ?", remoteID)
	e, err := scanEntry(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("entry with remote id %d: %w", remoteID, err)
	}
	return &e, nil
}

func collectEntries(rows *sql.Rows) ([]Entry, error) {
	var out []Entry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read entries: %w", err)
	}
	return out, nil
}

// --- entry writes ------------------------------------------------------------

// nextSeqExpr is the scalar subquery that hands out the next per-day entry
// number: one past the highest number already used on the entry's calendar day,
// so numbering starts at 1 each day and only ever grows. Deleted rows are
// deliberately NOT filtered out, which is what keeps a number from being reused
// after `tg del` (the day's numbering keeps its gaps). Its three placeholders
// are the day's [from, to) bounds and the id to exclude (0 when inserting, so
// nothing is excluded).
const nextSeqExpr = `(SELECT COALESCE(MAX(seq), 0) + 1 FROM entries
                       WHERE start >= ? AND start < ? AND id <> ?)`

// dayBounds returns the half-open [midnight, next midnight) range of t's
// calendar day in loc.
func dayBounds(t time.Time, loc *time.Location) (from, to time.Time) {
	from = dayStart(t, loc)
	return from, from.AddDate(0, 0, 1)
}

// assignSeq (re)numbers an existing entry within the calendar day of start,
// giving it the next free per-day number. It is used to back-fill pre-v4 rows
// and to renumber an entry a remote update moved to another day; the row itself
// is excluded from the maximum so it cannot bump itself.
func (s *Store) assignSeq(ctx context.Context, id int64, start time.Time) error {
	from, to := dayBounds(start, s.Location())
	if _, err := s.ex.ExecContext(ctx, "UPDATE entries SET seq = "+nextSeqExpr+" WHERE id = ?",
		fmtTime(from), fmtTime(to), id, id); err != nil {
		return fmt.Errorf("renumber entry %d: %w", id, err)
	}
	return nil
}

// insertStmt accumulates an INSERT one column at a time, keeping each column's
// value expression AND the arguments that expression consumes together.
//
// That pairing is the point. Written out by hand, the entries insert splices a
// multi-placeholder subquery (nextSeqExpr) into the middle of its VALUES list,
// so its arguments sit in the middle of a flat argument list too: adding a
// column, or moving one, silently shifts every later argument onto the wrong
// placeholder — a bug SQLite cannot catch, since the types still line up. Here a
// column is added with its arguments in one call, so the correspondence is
// structural rather than something the reader has to count out.
type insertStmt struct {
	table string
	cols  []string
	vals  []string
	args  []any
}

// set adds a column filled from one bound argument.
func (i *insertStmt) set(col string, arg any) { i.setExpr(col, "?", arg) }

// setExpr adds a column filled from a SQL expression, together with the
// arguments its own placeholders consume (none, for a constant expression).
func (i *insertStmt) setExpr(col, expr string, args ...any) {
	i.cols = append(i.cols, col)
	i.vals = append(i.vals, expr)
	i.args = append(i.args, args...)
}

// exec renders the statement and runs it against ex.
func (i *insertStmt) exec(ctx context.Context, ex execer) (sql.Result, error) {
	q := "INSERT INTO " + i.table + " (" + strings.Join(i.cols, ", ") + ")\n" +
		"VALUES (" + strings.Join(i.vals, ", ") + ")"
	return ex.ExecContext(ctx, q, i.args...)
}

// CreateEntry inserts a new entry and returns its local id. The caller sets all
// fields (Start/UpdatedAt/Dirty/Duration etc.); Duration -1 marks it running.
//
// The entry's per-day number (Entry.Seq) is assigned here, not by the caller: it
// is one past the highest number handed out on the entry's calendar day, so
// numbers reflect insertion order, restart at 1 every day, and are never reused
// (see nextSeqExpr). Entries arriving from a pull are numbered the same way, in
// the order the pull inserts them. Any Seq set on e is ignored.
func (s *Store) CreateEntry(ctx context.Context, e Entry) (int64, error) {
	from, to := dayBounds(e.Start, s.Location())
	ins := insertStmt{table: "entries"}
	ins.set("remote_id", nullInt(e.RemoteID))
	ins.set("workspace_id", e.WorkspaceID)
	ins.set("project_id", nullInt(e.ProjectID))
	ins.set("task_id", nullInt(e.TaskID))
	ins.set("description", e.Description)
	ins.set("start", fmtTime(e.Start))
	ins.set("stop", nullTime(e.Stop))
	ins.set("duration", e.Duration)
	ins.set("billable", boolToInt(e.Billable))
	// seq is computed by SQL rather than supplied, so the subquery's three
	// arguments (the day's bounds and the id to exclude — nothing, on an
	// insert) are attached to the column they belong to.
	ins.setExpr("seq", nextSeqExpr, fmtTime(from), fmtTime(to), int64(0))
	ins.set("updated_at", fmtTime(e.UpdatedAt))
	ins.set("synced_at", nullTime(e.SyncedAt))
	ins.set("dirty", boolToInt(e.Dirty))
	ins.set("deleted", boolToInt(e.Deleted))

	res, err := ins.exec(ctx, s.ex)
	if err != nil {
		return 0, fmt.Errorf("create entry: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("create entry: %w", err)
	}
	return id, nil
}

// ErrEntryTooOld reports a refusal to edit an entry that belongs to a calendar
// day before today: tg only ever rewrites the day being tracked, so history is
// never silently rewritten (see CheckEditableDay and UpdateEntry).
var ErrEntryTooOld = errors.New("refusing to update an entry older than today")

// CheckEditableDay is the failsafe behind every entry edit: it rejects any
// start that falls on a calendar day before now's day in loc, wrapping
// ErrEntryTooOld. It is applied both to where an entry currently sits and to
// where an edit would move it, so neither an old entry can be touched nor a
// current one pushed back into the past. A nil loc means time.Local.
func CheckEditableDay(start, now time.Time, loc *time.Location) error {
	if loc == nil {
		loc = time.Local
	}
	today := dayStart(now, loc)
	if start.Before(today) {
		return fmt.Errorf("%w: %s is before today (%s); only today's entries can be modified",
			ErrEntryTooOld, start.In(loc).Format(dateTimeLayout), today.Format(dateLayout))
	}
	return nil
}

// dayStart returns midnight of t's calendar day in loc.
func dayStart(t time.Time, loc *time.Location) time.Time {
	t = t.In(loc)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
}

// UpdateEntry writes the locally editable fields of an entry — description,
// start, stop and duration — back to the row identified by e.ID, then records
// e.UpdatedAt as the LWW clock and marks the row dirty so a later `tg push`
// sends the change. It backs `tg mod`; remote_id, synced_at and the deleted
// flag are deliberately left alone, so an already-synced entry keeps its remote
// identity and is PUT rather than re-created. A missing id is an error.
//
// This is the single local edit path, so it is also where the "today only"
// failsafe lives: the stored start and the incoming start are both checked
// against now (in loc) and an entry from an earlier day is refused with an
// error wrapping ErrEntryTooOld, before anything is written.
//
// The read of the stored start, the update and the possible renumbering run in
// one transaction, so the row the day check was made against is the row that is
// written, and a failure at the renumbering step cannot leave the entry retimed
// but numbered into the day it just left.
func (s *Store) UpdateEntry(ctx context.Context, e Entry, now time.Time, loc *time.Location) error {
	return s.WithTx(ctx, func(tx *Store) error {
		stored, err := tx.entryStart(ctx, e.ID)
		if err != nil {
			return err
		}
		if err := CheckEditableDay(stored, now, loc); err != nil {
			return err
		}
		if err := CheckEditableDay(e.Start, now, loc); err != nil {
			return err
		}
		res, err := tx.ex.ExecContext(ctx, `
UPDATE entries SET description = ?, start = ?, stop = ?, duration = ?,
  updated_at = ?, dirty = 1 WHERE id = ?`,
			e.Description, fmtTime(e.Start), nullTime(e.Stop), e.Duration,
			fmtTime(e.UpdatedAt), e.ID)
		if err != nil {
			return fmt.Errorf("update entry %d: %w", e.ID, err)
		}
		// checkAffected's error already names the entry and carries
		// ErrEntryNotFound, so it is returned as it is rather than prefixed
		// with the operation a second time.
		if err := checkAffected(res, e.ID); err != nil {
			return err
		}
		// Retiming keeps an entry on its own day today (a relative timesign
		// keeps the start and an absolute one is resolved on the entry's day),
		// so this is a failsafe rather than a routine path: should an edit ever
		// land the entry on another day, it joins that day's numbering instead
		// of keeping a number nothing on the new day would show.
		if !sameDay(stored, e.Start, tx.Location()) {
			return tx.assignSeq(ctx, e.ID, e.Start)
		}
		return nil
	})
}

// entryStart reads the persisted start of an entry by local id, so an edit can
// be judged by where the entry actually sits rather than by the (possibly
// already rewritten) copy the caller hands in. A missing id is an error.
func (s *Store) entryStart(ctx context.Context, id int64) (time.Time, error) {
	var start string
	err := s.ex.QueryRowContext(ctx, "SELECT start FROM entries WHERE id = ?", id).Scan(&start)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, fmt.Errorf("%w: no entry with id %d", ErrEntryNotFound, id)
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("read start of entry %d: %w", id, err)
	}
	t, err := parseTime(start)
	if err != nil {
		return time.Time{}, fmt.Errorf("entry %d: parse start: %w", id, err)
	}
	return t, nil
}

// SoftDeleteEntry marks an entry deleted and dirty, keeping the row so the
// deletion can be pushed: togglsync.Push DELETEs it remotely (when it has a remote
// id) and only then drops the row. Every read path filters deleted rows out, so
// the entry disappears from listings immediately. It backs `tg del`; at becomes
// the entry's LWW clock. A missing id is an error.
func (s *Store) SoftDeleteEntry(ctx context.Context, id int64, at time.Time) error {
	res, err := s.ex.ExecContext(ctx,
		"UPDATE entries SET deleted = 1, dirty = 1, updated_at = ? WHERE id = ?",
		fmtTime(at), id)
	if err != nil {
		return fmt.Errorf("soft-delete entry %d: %w", id, err)
	}
	return checkAffected(res, id)
}

// checkAffected turns an UPDATE that matched no row into an error wrapping
// ErrEntryNotFound, so a stale entry id fails loudly instead of silently doing
// nothing.
//
// A driver that cannot report the count is an error too, not a pass: the whole
// point of the check is to know whether the write landed, and answering "it did"
// when that is precisely what could not be established is how a silent no-op
// gets through. (The sqlite driver tg uses always reports it, so this is a
// failsafe rather than a path in practice.)
func checkAffected(res sql.Result, id int64) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected for entry %d: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: no entry with id %d", ErrEntryNotFound, id)
	}
	return nil
}

// MarkSynced records a successful push: stores the remote id, clears dirty, and
// aligns updated_at/synced_at to the remote clock so a later pull is a no-op.
func (s *Store) MarkSynced(ctx context.Context, id, remoteID int64, at time.Time) error {
	if _, err := s.ex.ExecContext(ctx, `
UPDATE entries SET remote_id = ?, synced_at = ?, updated_at = ?, dirty = 0 WHERE id = ?`,
		remoteID, fmtTime(at), fmtTime(at), id); err != nil {
		return fmt.Errorf("mark entry %d synced: %w", id, err)
	}
	return nil
}

// ErrEntryNotFound reports that an entry a write was aimed at does not exist,
// so the write matched nothing. Callers that can carry on (togglsync.Pull re-creates
// a mirror that vanished under it) should test for it with errors.Is; the point
// of the sentinel is that a lookup miss fails loudly instead of passing for a
// successful update.
var ErrEntryNotFound = errors.New("entry not found")

// UpdateFromRemote overwrites a local entry with remote state (remote wins) and
// marks it clean, aligning the LWW clocks to the remote at. The entry is found
// by its remote id; a remote id that no local row mirrors (or a nil one) is an
// error wrapping ErrEntryNotFound rather than a silent no-op, so a caller
// counting updates never counts a write that never happened.
//
// A remote edit is the only thing that can move an entry to another calendar
// day, which would strand its number in the day it no longer belongs to, so the
// entry is renumbered into its new day when that happens. An entry that stays
// put keeps the number `tg ls` published for it.
//
// The mirror lookup, the overwrite and the renumbering share one transaction
// (joining the caller's when there is one, see WithTx), so the row that was
// looked up is the row that is written and no half-applied update survives a
// failure partway through.
func (s *Store) UpdateFromRemote(ctx context.Context, e Entry) error {
	return s.WithTx(ctx, func(tx *Store) error {
		id, oldStart, err := tx.entryByRemote(ctx, e.RemoteID)
		if err != nil {
			return err
		}
		if id == 0 {
			if e.RemoteID == nil {
				return fmt.Errorf("%w: entry has no remote id", ErrEntryNotFound)
			}
			return fmt.Errorf("%w: no local entry mirrors remote id %d", ErrEntryNotFound, *e.RemoteID)
		}
		res, err := tx.ex.ExecContext(ctx, `
UPDATE entries SET workspace_id = ?, project_id = ?, task_id = ?, description = ?,
  start = ?, stop = ?, duration = ?, billable = ?, updated_at = ?, synced_at = ?,
  dirty = 0, deleted = 0 WHERE id = ?`,
			e.WorkspaceID, nullInt(e.ProjectID), nullInt(e.TaskID), e.Description,
			fmtTime(e.Start), nullTime(e.Stop), e.Duration, boolToInt(e.Billable),
			fmtTime(e.UpdatedAt), nullTime(e.SyncedAt), id)
		if err != nil {
			return fmt.Errorf("update entry %d from remote: %w", id, err)
		}
		if err := checkAffected(res, id); err != nil {
			return err
		}
		if sameDay(oldStart, e.Start, tx.Location()) {
			return nil
		}
		return tx.assignSeq(ctx, id, e.Start)
	})
}

// entryByRemote reads the local id and stored start of the mirror of a remote
// entry. A remote id that is unknown (or nil) yields id 0 and no error; it is
// the caller's job to decide whether that is fatal (see UpdateFromRemote).
func (s *Store) entryByRemote(ctx context.Context, remoteID *int64) (int64, time.Time, error) {
	if remoteID == nil {
		return 0, time.Time{}, nil
	}
	var (
		id    int64
		start string
	)
	err := s.ex.QueryRowContext(ctx, "SELECT id, start FROM entries WHERE remote_id = ?", *remoteID).
		Scan(&id, &start)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, time.Time{}, nil
	}
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("read entry with remote id %d: %w", *remoteID, err)
	}
	t, err := parseTime(start)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("entry %d: parse start: %w", id, err)
	}
	return id, t, nil
}

// sameDay reports whether a and b fall on the same calendar day in loc.
func sameDay(a, b time.Time, loc *time.Location) bool {
	return dayStart(a, loc).Equal(dayStart(b, loc))
}

// DeleteRow hard-deletes a local row (used after a remote delete is confirmed).
func (s *Store) DeleteRow(ctx context.Context, id int64) error {
	if _, err := s.ex.ExecContext(ctx, "DELETE FROM entries WHERE id = ?", id); err != nil {
		return fmt.Errorf("delete entry %d: %w", id, err)
	}
	return nil
}

// DeleteByRemoteID hard-deletes the local mirror of a remote-deleted entry.
func (s *Store) DeleteByRemoteID(ctx context.Context, remoteID int64) error {
	if _, err := s.ex.ExecContext(ctx, "DELETE FROM entries WHERE remote_id = ?", remoteID); err != nil {
		return fmt.Errorf("delete entry with remote id %d: %w", remoteID, err)
	}
	return nil
}

// --- entry numbers -----------------------------------------------------------

// ErrNoEntryNum reports that a local entry number does not resolve to an entry
// on the day it was looked up on: nothing was ever numbered that high that day,
// or the entry has since been deleted (its number stays vacant rather than
// sliding onto its neighbour). Callers should surface the wrapped message,
// which already tells the user to re-run `tg ls`.
var ErrNoEntryNum = errors.New("no such entry number")

// EntryByNum resolves a per-day entry number (the leading column of `tg ls`) to
// its entry on the calendar day containing day, in the store's location. The
// numbers are persistent, so this keeps working across listings and is not
// affected by anything deleted in between: an unused or freed number is an
// error wrapping ErrNoEntryNum rather than a different entry.
func (s *Store) EntryByNum(ctx context.Context, num int, day time.Time) (Entry, error) {
	from, to := dayBounds(day, s.Location())
	row := s.ex.QueryRowContext(ctx, entrySelect+`
WHERE e.deleted = 0 AND e.seq = ? AND e.start >= ? AND e.start < ?
LIMIT 1`, num, fmtTime(from), fmtTime(to))
	e, err := scanEntry(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Entry{}, fmt.Errorf("%w %d on %s; run `tg ls` to list entries",
			ErrNoEntryNum, num, from.Format(dateLayout))
	}
	if err != nil {
		return Entry{}, fmt.Errorf("entry number %d: %w", num, err)
	}
	return e, nil
}

// --- catalog -----------------------------------------------------------------

// ReplaceProjects atomically replaces the entire projects mirror.
func (s *Store) ReplaceProjects(ctx context.Context, projects []Project) error {
	return s.WithTx(ctx, func(tx *Store) error {
		if _, err := tx.ex.ExecContext(ctx, "DELETE FROM projects"); err != nil {
			return fmt.Errorf("clear projects: %w", err)
		}
		for _, p := range projects {
			if _, err := tx.ex.ExecContext(ctx, `
INSERT INTO projects (id, workspace_id, name, color, client_name, active, billable, at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				p.ID, p.WorkspaceID, p.Name, p.Color, p.ClientName,
				boolToInt(p.Active), boolToInt(p.Billable), p.At); err != nil {
				return fmt.Errorf("insert project %d: %w", p.ID, err)
			}
		}
		return nil
	})
}

// ReplaceProjectTasks atomically replaces the cached tasks of a single project,
// leaving every other project's tasks untouched. It backs the project-scoped
// `tg update`, which never refreshes the whole workspace.
func (s *Store) ReplaceProjectTasks(ctx context.Context, projectID int64, tasks []Task) error {
	return s.WithTx(ctx, func(tx *Store) error {
		if _, err := tx.ex.ExecContext(ctx, "DELETE FROM tasks WHERE project_id = ?", projectID); err != nil {
			return fmt.Errorf("clear tasks of project %d: %w", projectID, err)
		}
		return tx.insertTasks(ctx, tasks)
	})
}

// ReplaceTasks atomically replaces the entire tasks mirror.
func (s *Store) ReplaceTasks(ctx context.Context, tasks []Task) error {
	return s.WithTx(ctx, func(tx *Store) error {
		if _, err := tx.ex.ExecContext(ctx, "DELETE FROM tasks"); err != nil {
			return fmt.Errorf("clear tasks: %w", err)
		}
		return tx.insertTasks(ctx, tasks)
	})
}

// insertTasks inserts the given catalog tasks, assuming the rows they replace
// have already been deleted by the caller (see ReplaceTasks).
func (s *Store) insertTasks(ctx context.Context, tasks []Task) error {
	for _, t := range tasks {
		if _, err := s.ex.ExecContext(ctx, `
INSERT INTO tasks (id, workspace_id, project_id, name, active, at)
VALUES (?, ?, ?, ?, ?, ?)`,
			t.ID, t.WorkspaceID, t.ProjectID, t.Name, boolToInt(t.Active), t.At); err != nil {
			return fmt.Errorf("insert task %d: %w", t.ID, err)
		}
	}
	return nil
}

// PutProject inserts or fully updates a single project row by id, refreshing
// every display and state field (name, color, client, active, billable, at).
// Unlike UpsertProject (which is a conservative self-heal from meta pulls),
// this is authoritative and backs the project-scoped `tg update`.
func (s *Store) PutProject(ctx context.Context, p Project) error {
	if _, err := s.ex.ExecContext(ctx, `
INSERT INTO projects (id, workspace_id, name, color, client_name, active, billable, at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
  workspace_id = excluded.workspace_id, name = excluded.name, color = excluded.color,
  client_name = excluded.client_name, active = excluded.active,
  billable = excluded.billable, at = excluded.at`,
		p.ID, p.WorkspaceID, p.Name, p.Color, p.ClientName,
		boolToInt(p.Active), boolToInt(p.Billable), p.At); err != nil {
		return fmt.Errorf("put project %d: %w", p.ID, err)
	}
	return nil
}

// UpsertProject inserts or updates a single project row by id, refreshing the
// display fields. It is used to self-heal the catalog from meta-enriched pulls
// so entries always resolve a project name (and color), even before a full
// `tg update`. It deliberately leaves active/at untouched on conflict so an
// authoritative `tg update` is never downgraded. Color is only refreshed when a
// non-empty value is supplied, so a meta payload lacking a color never clobbers
// a color already stored by an authoritative update.
func (s *Store) UpsertProject(ctx context.Context, p Project) error {
	if _, err := s.ex.ExecContext(ctx, `
INSERT INTO projects (id, workspace_id, name, color, client_name, active, billable, at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
  workspace_id = excluded.workspace_id, name = excluded.name,
  color = COALESCE(NULLIF(excluded.color, ''), color)`,
		p.ID, p.WorkspaceID, p.Name, p.Color, p.ClientName,
		boolToInt(p.Active), boolToInt(p.Billable), p.At); err != nil {
		return fmt.Errorf("upsert project %d: %w", p.ID, err)
	}
	return nil
}

// UpsertTask inserts or updates a single task row by id, refreshing the display
// fields (see UpsertProject for the rationale).
func (s *Store) UpsertTask(ctx context.Context, t Task) error {
	if _, err := s.ex.ExecContext(ctx, `
INSERT INTO tasks (id, workspace_id, project_id, name, active, at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
  workspace_id = excluded.workspace_id, project_id = excluded.project_id,
  name = excluded.name`,
		t.ID, t.WorkspaceID, t.ProjectID, t.Name, boolToInt(t.Active), t.At); err != nil {
		return fmt.Errorf("upsert task %d: %w", t.ID, err)
	}
	return nil
}

// activeTasks loads every active task for matching.
func (s *Store) activeTasks(ctx context.Context) ([]Task, error) {
	rows, err := s.ex.QueryContext(ctx,
		"SELECT id, workspace_id, project_id, name, active, at FROM tasks WHERE active = 1")
	if err != nil {
		return nil, fmt.Errorf("list active tasks: %w", err)
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		var t Task
		if err := rows.Scan(&t.ID, &t.WorkspaceID, &t.ProjectID, &t.Name, &t.Active, &t.At); err != nil {
			return nil, fmt.Errorf("scan task: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListProjects returns catalog projects for display, ordered by name. Inactive
// projects are included only when includeInactive is set.
func (s *Store) ListProjects(ctx context.Context, includeInactive bool) ([]Project, error) {
	q := `
SELECT id, workspace_id, name, COALESCE(color, ''), COALESCE(client_name, ''),
       active, billable, COALESCE(at, '')
FROM projects`
	if !includeInactive {
		q += "\nWHERE active = 1"
	}
	q += "\nORDER BY name"

	rows, err := s.ex.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.WorkspaceID, &p.Name, &p.Color,
			&p.ClientName, &p.Active, &p.Billable, &p.At); err != nil {
			return nil, fmt.Errorf("scan project: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ProjectByID returns the cached project with the given id, or nil if it is not
// in the local catalog (e.g. before `tg update`). It is used to carry a
// project's billable flag onto entries created against it.
func (s *Store) ProjectByID(ctx context.Context, id int64) (*Project, error) {
	row := s.ex.QueryRowContext(ctx, `
SELECT id, workspace_id, name, COALESCE(color, ''), COALESCE(client_name, ''),
       active, billable, COALESCE(at, '')
FROM projects WHERE id = ?`, id)
	var p Project
	err := row.Scan(&p.ID, &p.WorkspaceID, &p.Name, &p.Color,
		&p.ClientName, &p.Active, &p.Billable, &p.At)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("project %d: %w", id, err)
	}
	return &p, nil
}

// ListTasks returns catalog tasks for display, with the project name joined and
// ordered by project then task name. Inactive tasks are included only when
// includeInactive is set; a non-nil projectID scopes the listing to one project.
func (s *Store) ListTasks(ctx context.Context, includeInactive bool, projectID *int64) ([]Task, error) {
	q := `
SELECT t.id, t.workspace_id, t.project_id, t.name, t.active, COALESCE(t.at, ''),
       COALESCE(p.name, '')
FROM tasks t
LEFT JOIN projects p ON p.id = t.project_id`
	var conds []string
	var args []any
	if !includeInactive {
		conds = append(conds, "t.active = 1")
	}
	if projectID != nil {
		conds = append(conds, "t.project_id = ?")
		args = append(args, *projectID)
	}
	if len(conds) > 0 {
		q += "\nWHERE " + strings.Join(conds, " AND ")
	}
	q += "\nORDER BY p.name, t.name"

	rows, err := s.ex.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		var t Task
		if err := rows.Scan(&t.ID, &t.WorkspaceID, &t.ProjectID, &t.Name,
			&t.Active, &t.At, &t.ProjectName); err != nil {
			return nil, fmt.Errorf("scan task: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// FindTasksByFragment returns active tasks matching fragment (see matchTasks),
// optionally scoped to a project.
func (s *Store) FindTasksByFragment(ctx context.Context, fragment string, projectID *int64) ([]Task, error) {
	tasks, err := s.activeTasks(ctx)
	if err != nil {
		return nil, err
	}
	return matchTasks(tasks, fragment, projectID), nil
}

// FindProjectsByFragment returns active projects matching fragment (see
// matchProjects).
func (s *Store) FindProjectsByFragment(ctx context.Context, fragment string) ([]Project, error) {
	projects, err := s.ListProjects(ctx, false)
	if err != nil {
		return nil, err
	}
	return matchProjects(projects, fragment), nil
}

// matchProjects mirrors matchTasks for projects: a case-insensitive substring
// match on the project name, with an exact (case-insensitive) full-name match
// taking precedence over mere substrings. Results are sorted by name then id
// for stable candidate listings.
func matchProjects(projects []Project, fragment string) []Project {
	frag := strings.ToLower(strings.TrimSpace(fragment))
	if frag == "" {
		return nil
	}
	var subs, exact []Project
	for _, p := range projects {
		name := strings.ToLower(p.Name)
		if !strings.Contains(name, frag) {
			continue
		}
		subs = append(subs, p)
		if name == frag {
			exact = append(exact, p)
		}
	}
	res := subs
	if len(exact) > 0 {
		res = exact
	}
	sort.Slice(res, func(i, j int) bool {
		if res[i].Name != res[j].Name {
			return res[i].Name < res[j].Name
		}
		return res[i].ID < res[j].ID
	})
	return res
}

// matchTasks is a pure, deterministic matcher: case-insensitive substring on
// the task name, optionally scoped to projectID. An exact (case-insensitive)
// full-title match takes precedence over mere substring matches. Results are
// sorted by name then id for stable candidate listings.
func matchTasks(tasks []Task, fragment string, projectID *int64) []Task {
	frag := strings.ToLower(strings.TrimSpace(fragment))
	if frag == "" {
		return nil
	}
	var subs, exact []Task
	for _, t := range tasks {
		if projectID != nil && t.ProjectID != *projectID {
			continue
		}
		name := strings.ToLower(t.Name)
		if !strings.Contains(name, frag) {
			continue
		}
		subs = append(subs, t)
		if name == frag {
			exact = append(exact, t)
		}
	}
	res := subs
	if len(exact) > 0 {
		res = exact
	}
	sort.Slice(res, func(i, j int) bool {
		if res[i].Name != res[j].Name {
			return res[i].Name < res[j].Name
		}
		return res[i].ID < res[j].ID
	})
	return res
}

// --- meta --------------------------------------------------------------------

// Meta returns the value for key and whether it was present.
func (s *Store) Meta(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := s.ex.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = ?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read meta %q: %w", key, err)
	}
	return v, true, nil
}

// SetMeta upserts a meta key/value pair.
func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	if _, err := s.ex.ExecContext(ctx,
		"INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
		key, value); err != nil {
		return fmt.Errorf("write meta %q: %w", key, err)
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
