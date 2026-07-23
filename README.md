# tg

`tg` is a local-first command-line time tracker. It records entries in a local
SQLite database and synchronizes them with [Toggl Track](https://toggl.com/track/)
on demand, so tracking works offline and syncs when you choose.

## Features

- Local-first: every action is written to SQLite first and works offline.
- On-demand sync with Toggl Track using last-writer-wins reconciliation.
- Fuzzy task matching — start tracking with just a fragment of a task name.
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

Refresh the local catalog, then start tracking:

```sh
tg update-projects          # sync all workspace projects
tg update <project>         # fetch tasks for one project
tg start <task>             # start tracking the matching task
tg stop                     # stop the running entry
```

To record a finished block of time without using the timer, use `add` with a
`START-STOP` range on today's clock:

```sh
tg add 9-:30 <task>         # 09:00-09:30
tg add 10-11 <task>         # 10:00-11:00
tg add 10:30-11 <task>      # 10:30-11:00
```

Each side is `H` or `H:MM`; the stop side may also be `:MM`, inheriting the
start hour. Hours are 0-23, minutes 0-59, and the stop must be after the start.
Like `start`, `add` accepts `<project> <task>` to scope by project name (or set
`TOGGL_PROJECT_ID`), stores the entry locally marked dirty, and best-effort
pushes it to Toggl.

To see how much time you have tracked against particular tasks, use `total`.
It queries the Toggl Reports API directly (no local cache) for all-time totals
and lists one line per matched task plus a grand total:

```sh
tg total login review      # totals for tasks matching "login" and "review"
```

Each argument is a task-name fragment, matched the same case-insensitive way as
`tg start`. A task matched by more than one fragment is listed once.

## Usage

```
usage: tg <command> [flags]

commands:
  auth [token]              verify a Toggl API token and store config
  start [project] <task>    start tracking the task matching <task>
  add <range> [project] <task>  add a finished entry (e.g. 9-:30, 10:30-11)
  stop                      stop the running entry (snaps to 5m)
  current | status          show the running entry            [--json]
  today   | list | ls       show today's entries     [--days N] [--json]
  tasks                     list cached tasks         [--all] [--json]
  projects                  list cached projects with ids     [--all] [--json]
  update <project>          refresh one project's tasks       [--all] [--json]
  update-projects           sync all workspace projects       [--all] [--json]
  push                      send local changes to Toggl       [--json]
  pull [project]            fetch changes; all projects, or one [--since DATE] [--json]
  total <task>...           total tracked hours per task (Reports API) [--json]
  completion zsh            print the zsh completion script
```

### Syncing

Run `tg pull` then `tg push` for correct last-writer-wins behavior:

```sh
tg pull    # fetch remote changes into the local store
tg push    # send local changes to Toggl
```

`tg push` runs automatically (best-effort) when you `tg start`, so a running
entry shows up in the Toggl web app immediately. If the network is unavailable,
the entry stays local and dirty until the next `tg push`.

### Environment

- `TOGGL_PROJECT_ID` scopes `start`, `tasks`, and `update` to one project (and
  sets the project on entries created by `start`). `pull` ignores it and always
  reconciles every project; pass a `<project>` name to `pull` to scope it
  explicitly. When unset, `update` requires a unique `<project>` name and
  `start` accepts `<project> <task>` to scope by project name.
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

## License

[MIT](LICENSE)
