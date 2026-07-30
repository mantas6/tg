# tg

`tg` is a local-first command-line time tracker. It records entries in a local
SQLite database and synchronizes them with [Toggl Track](https://toggl.com/track/)
on demand, so tracking works offline and syncs when you choose.

## Features

- Local-first: every action is written to SQLite first and works offline.
- On-demand sync with Toggl Track using last-writer-wins reconciliation.
- Fuzzy task matching — record time with just a fragment of a task name.
- Cached project/task catalog for fast, offline-friendly lookups.
- Human-readable output with project colors, plus `--json` on most commands.
- zsh completion.

## Installation

Requires Go 1.26+.

```sh
go install github.com/mantas6/tg@latest
```

Or build from source:

```sh
git clone https://github.com/mantas6/tg.git
cd tg
go build -o tg .
```

## Getting started

Authenticate with your Toggl API token (found in your Toggl profile settings):

```sh
tg auth <token>
# or, prompt interactively / read from stdin:
tg auth
```

Refresh the local catalog, then record time:

```sh
tg update-projects          # sync all workspace projects
tg update <project>         # fetch tasks for one project
tg add +:20 <task>          # record the last 20 minutes against the task
tg status                   # last entry, idle gap, today's total
```

There is no timer: time is always recorded as a finished block with `add` and a
*timesign*, either an absolute `START-STOP` range on today's clock or a relative
`+DURATION` span counted back from now.

```sh
tg add 9-:30 <task>         # 09:00-09:30
tg add 10-11 <task>         # 10:00-11:00
tg add 10:30-11 <task>      # 10:30-11:00
tg add +:20 <task>          # the last 20 minutes
tg add +1:20 <task>         # the last 1h20m
```

For absolute ranges each side is `H` or `H:MM`; the stop side may also be `:MM`,
inheriting the start hour. Hours are 0-23, minutes 0-59, and the stop must be
after the start. A relative timesign ends at now rounded *down* to the preceding
5-minute mark (14:23 -> 14:20) and starts that duration earlier. The full
grammar, rounding rules, and error cases are in [docs/timesig.md](docs/timesig.md).

`add` accepts `<project> <task>` to scope by project name (or set
`TOGGL_PROJECT_ID`), stores the entry locally marked dirty, and best-effort
pushes it to Toggl.

Time is exclusive: `add` refuses a span that overlaps an entry you already
tracked and reports the conflict instead of recording it. Back-to-back entries
are fine (one may start exactly when another ends); an entry still running in
Toggl (pulled by `tg pull`) counts as occupying everything from its start
onwards, so only spans ending at or before it are accepted while it runs.

Pass `--desc` (alias `--description`) to set the entry's description, which is
carried through to Toggl on push:

```sh
tg add 9-:30 --desc "reset password flow" <task>
```

To see how much time you have tracked against particular tasks, use `total`.
It queries the Toggl Reports API directly (no local cache) and lists one line
per matched task plus a grand total. By default it covers the last 3 months
(from today minus three months through today); pass `--since DATE` (YYYY-MM-DD)
to override the start of the range:

```sh
tg total login review                 # last 3 months for "login" and "review"
tg total --since 2025-01-01 login     # from 2025-01-01 through today
```

Each argument is a task-name fragment, matched the same case-insensitive way as
`tg add`. A task matched by more than one fragment is listed once.

`status` (alias `current`) is the terse one-glance line: the last entry with its
wall-clock range, the idle gap since it stopped, and today's tracked total.

```sh
$ tg status
last Code review [Backend] 10:30-11:00 (gap 0h25m)
Today: 6h30m
```

The task name is truncated to 30 characters so the line fits a status bar. The
gap only appears once now has moved past the entry's stop, and it deliberately
spans days, so a stale `gap 20h00m` tells you nothing has been tracked since
yesterday. An entry that is still running in Toggl (pulled by `tg pull`) is
reported as running with its live elapsed time instead:

```sh
$ tg status
run Code review [Backend] (0h45m)
Today: 2h00m
```

With `--json` the same facts come back as
`{"running":false,"task":"Code review",...,"gap_seconds":1500,"day_total_seconds":23400}`,
where `elapsed_seconds` is the last entry's length (live while running).

`ls` (aliases `today`, `list`) is the day's table: one line per entry with its
local number, wall-clock range, duration, task and project, filler rows for the
time you did not track, and the day's total.

```sh
$ tg ls
1  09:00-10:00 1h00m  Fix login bug     [Backend]
2  10:00-11:00 1h00m  Code review       [Backend]
               (gap 0h30m)
3  11:30-12:00 0h30m  Payment fix       [Payments]
               (gap 0h25m)
----------------------------------------
Total: 2h30m
```

The leading numbers are local references, renumbered 1..N on every `ls` and
stored in the local database, so the listing you just looked at is what other
commands address. Gap rows are not entries and carry no number: they show idle
time between two entries, and the last one shows the idle time since the newest
entry stopped (only within the same day, and never while an entry is running).
`--days N` looks further back, `--json` emits the same data with `num` on each
entry.

## Usage

```
usage: tg <command> [flags]

commands:
  auth [token]              verify a Toggl API token and store config
  add <timesign> [project] <task>  add a finished entry [--desc TEXT]
  current | status          last entry, gap, day total        [--json]
  today   | list | ls       show today's entries     [--days N] [--json]
  tasks                     list cached tasks         [--all] [--json]
  projects                  list cached projects with ids     [--all] [--json]
  update <project>          refresh one project's tasks       [--all] [--json]
  update-projects           sync all workspace projects       [--all] [--json]
  push                      send local changes to Toggl       [--json]
  pull [project]            fetch changes; all projects, or one [--since DATE] [--json]
  total <task>...           total tracked hours per task; last 3 months [--since DATE] [--json]
  completion zsh            print the zsh completion script

timesign: absolute 9-:30, 10-11, 10:30-11:15 (today) or relative
      +:20, +1, +1:20 (that long, ending at the last 5m mark).
      Full spec: docs/timesig.md
```

### Syncing

Run `tg pull` then `tg push` for correct last-writer-wins behavior:

```sh
tg pull    # fetch remote changes into the local store
tg push    # send local changes to Toggl
```

`tg push` runs automatically (best-effort) when you `tg add`, so the entry shows
up in the Toggl web app immediately. If the network is unavailable, the entry
stays local and dirty until the next `tg push`.

### Environment

- `TOGGL_PROJECT_ID` scopes `add`, `tasks`, and `update` to one project (and
  sets the project on entries created by `add`). `pull` ignores it and always
  reconciles every project; pass a `<project>` name to `pull` to scope it
  explicitly. When unset, `update` requires a unique `<project>` name and
  `add` accepts `<timesign> <project> <task>` to scope by project name.
- `XDG_STATE_HOME` controls where state is stored (`$XDG_STATE_HOME/tg`,
  falling back to `~/.local/state/tg`). This holds `config.json` (mode 0600)
  and the SQLite database.

### Shell completion

```sh
tg completion zsh > "${fpath[1]}/_tg"
```

## Development

```sh
go test ./...
go build ./...
```

These same checks run automatically via GitHub Actions on every push to `main` and pull request.

## License

[MIT](LICENSE)
