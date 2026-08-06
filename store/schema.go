package store

import (
	"database/sql"
	"time"
)

// schemaVersion is the current logical schema version, recorded in meta.
// v2 added the billable flag to entries and projects.
// v3 added entry_refs, the display-order numbering `tg ls` published so later
// commands could address an entry as `tg mod 2` / `tg del 3`.
// v4 replaced that ephemeral mapping with entries.seq: a per-day number handed
// out at insert time, restarting at 1 on every calendar day and never reused,
// so a listing's numbers survive later listings and deletions leave gaps
// instead of renumbering their neighbours. entry_refs is dropped.
const schemaVersion = "4"

// schemaSQL creates every table and index. It is idempotent (IF NOT EXISTS),
// so applying it repeatedly is safe and forms the basis of migration.
const schemaSQL = `
CREATE TABLE IF NOT EXISTS entries (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  remote_id    INTEGER UNIQUE,
  workspace_id INTEGER NOT NULL,
  project_id   INTEGER,
  task_id      INTEGER,
  description  TEXT NOT NULL DEFAULT '',
  start        TEXT NOT NULL,
  stop         TEXT,
  duration     INTEGER NOT NULL,
  billable     INTEGER NOT NULL DEFAULT 0,
  seq          INTEGER NOT NULL DEFAULT 0,
  updated_at   TEXT NOT NULL,
  synced_at    TEXT,
  dirty        INTEGER NOT NULL DEFAULT 1,
  deleted      INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_entries_start ON entries(start);

CREATE TABLE IF NOT EXISTS projects (
  id           INTEGER PRIMARY KEY,
  workspace_id INTEGER NOT NULL,
  name         TEXT NOT NULL,
  color        TEXT,
  client_name  TEXT,
  active       INTEGER NOT NULL DEFAULT 1,
  billable     INTEGER NOT NULL DEFAULT 0,
  at           TEXT
);

CREATE TABLE IF NOT EXISTS tasks (
  id           INTEGER PRIMARY KEY,
  workspace_id INTEGER NOT NULL,
  project_id   INTEGER NOT NULL,
  name         TEXT NOT NULL,
  active       INTEGER NOT NULL DEFAULT 1,
  at           TEXT
);

CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT);
`

// dropTables holds tables retired by a later schema version. They are dropped
// on every migrate so an upgraded database does not keep dead state around.
// entry_refs (v3) was the rewritten-on-every-listing number->entry mapping that
// entries.seq (v4) replaces.
var dropTables = []string{"entry_refs"}

// addColumns holds columns introduced after the initial schema. Fresh databases
// get them from schemaSQL; pre-existing ones are upgraded in place by migrate.
var addColumns = []struct{ table, column, ddl string }{
	{"entries", "billable", "ALTER TABLE entries ADD COLUMN billable INTEGER NOT NULL DEFAULT 0"},
	{"projects", "billable", "ALTER TABLE projects ADD COLUMN billable INTEGER NOT NULL DEFAULT 0"},
	{"entries", "seq", "ALTER TABLE entries ADD COLUMN seq INTEGER NOT NULL DEFAULT 0"},
}

// migrate applies the schema, back-fills any columns added in later versions on
// pre-existing databases, numbers any entry that predates entries.seq, drops
// retired tables, and records the schema version. It is safe to run on every
// Open (idempotent).
func (s *Store) migrate() error {
	if _, err := s.db.Exec(schemaSQL); err != nil {
		return err
	}
	for _, c := range addColumns {
		has, err := s.hasColumn(c.table, c.column)
		if err != nil {
			return err
		}
		if has {
			continue
		}
		if _, err := s.db.Exec(c.ddl); err != nil {
			return err
		}
	}
	for _, t := range dropTables {
		if _, err := s.db.Exec("DROP TABLE IF EXISTS " + t); err != nil {
			return err
		}
	}
	if err := s.backfillSeq(); err != nil {
		return err
	}
	return s.SetMeta(MetaSchemaVersion, schemaVersion)
}

// backfillSeq hands a per-day number to every entry that has none (seq 0),
// which is what rows written before v4 look like. Rows are numbered oldest id
// first, so the pre-existing insertion order is what the numbering reproduces,
// and each day starts again from 1. It is a no-op once every row is numbered,
// so running it on every Open costs a single indexless scan of an already
// migrated table.
func (s *Store) backfillSeq() error {
	rows, err := s.db.Query("SELECT id, start FROM entries WHERE seq = 0 ORDER BY id")
	if err != nil {
		return err
	}
	type pending struct {
		id    int64
		start time.Time
	}
	var todo []pending
	for rows.Next() {
		var (
			id    int64
			start string
		)
		if err := rows.Scan(&id, &start); err != nil {
			rows.Close()
			return err
		}
		t, err := parseTime(start)
		if err != nil {
			rows.Close()
			return err
		}
		todo = append(todo, pending{id, t})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	for _, p := range todo {
		if err := s.assignSeq(p.id, p.start); err != nil {
			return err
		}
	}
	return nil
}

// hasColumn reports whether table already has the named column. SQLite lacks
// ADD COLUMN IF NOT EXISTS, so migrations probe the schema first. The table name
// is a trusted constant (never user input), so interpolating it is safe.
func (s *Store) hasColumn(table, column string) (bool, error) {
	rows, err := s.db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid     int
			name    string
			ctype   string
			notnull int
			dflt    sql.NullString
			pk      int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}
