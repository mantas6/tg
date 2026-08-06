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
tg projects update          # sync all workspace projects
tg update <project>         # fetch one project's tasks + its recent entries
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

### Fixing and removing entries

`mod` edits an entry that already exists and `del` removes one. Both address
entries by the small per-day numbers printed by `tg ls` (see below), resolved on
today's day; `mod` also defaults to *the last entry* when no number is given —
the same one `tg status` reports, resolved the same way: today's newest entry
that has already started (see below).

```sh
tg mod +:30                      # the last entry ran 30 minutes longer
tg mod 2 +1:15                   # push entry 2's end 1h15m later
tg mod 2 9-10:30                 # set entry 2's range explicitly
tg mod --desc "reset password"   # only change the last entry's description
tg mod 2 +:45 --desc "rebased"   # extend and re-describe entry 2
tg del 3                         # remove entry 3
```

A timesign is read relative to the entry, not to today:

- an **absolute** range (`9-10:30`) sets start and stop on the entry's *own*
  calendar day, so an edit never drags an entry onto another day;
- a **relative** timesign (`+:30`) *extends* the entry by that duration: the
  start is kept and the stop moves to stop + duration, so an entry ending at
  14:00 ends at 14:30 after `tg mod +:30`. It does **not** re-anchor the entry
  to now the way `tg add` does, and it is not an absolute length — repeating it
  keeps adding time. A still-running entry has no end to extend, so `mod +`
  refuses it; retime it with an absolute range instead.

**Only today's entries can be modified.** Once an entry's calendar day is over
it is history: `tg mod` refuses it outright ("refusing to update an entry older
than today") and nothing is written locally or sent to Toggl. The check is
enforced again in the storage layer, so no command can rewrite a past day. Use
the Toggl web app for genuine corrections to older days.

`mod` requires at least one change (a timesign, `--desc`, or both), refuses a new
range that would overlap a *different* entry, and `--desc ""` clears the
description. `del` is a soft delete: the entry disappears from listings at once
and the removal is pushed to Toggl. Both mark the entry dirty and best-effort
push, just like `add`, so a failed or skipped sync just leaves the change for the
next `tg push`. Deleting an entry retires its number rather than renumbering the
rest, so the numbers you just read stay valid. A number that does not resolve
(nothing was numbered that high today, or that entry is gone) is an error
telling you to re-run `tg ls`.

`grep` searches the cached task catalog and lists every task whose name
contains the fragment, case-insensitively. It is the way to find the exact
wording of a task before recording against it (`tg tasks` lists everything;
`grep` narrows it down):

```sh
tg grep login              # every task containing "login"
tg grep code review        # one fragment: "code review"
tg grep --all fix          # include inactive (archived) tasks
tg grep login --json       # machine-readable, same shape as `tg tasks --json`
```

The arguments are joined into a single fragment and the output is the `tasks`
listing restricted to the matches, name plus `[Project]`. Unlike the matching
used by `add` and `total`, an exact name does **not** win over substrings: a
task named `Fix` never hides `Fix login bug`, since the point is to see every
candidate. `TOGGL_PROJECT_ID` scopes the search to one project. A fragment is
required, and grep exits non-zero when nothing matches (run `tg update` if the
catalog is stale).

To see how much time you have tracked against particular tasks, use `total`.
The hours come from the Toggl Reports API, while the task names come from the
local catalog: report rows are joined to cached tasks by task id, because the
API's summary rows often carry ids only. It lists one line per task with its
project plus a grand total. By default it covers the last 3 months (from today
minus three months through today); pass `--since DATE` (YYYY-MM-DD) to override
the start of the range:

```sh
tg total login                        # last 3 months for "login"
tg total code review                  # one fragment: "code review"
tg total                              # every task with tracked time
tg total --since 2025-01-01 login     # from 2025-01-01 through today
```

The arguments are joined into a single task-name fragment, matched against the
cached tasks the same case-insensitive way as `tg add` (exact name wins over
substrings), so run `tg update` if the catalog is stale. Without a fragment
every task with tracked time is listed, including tasks missing from the local
catalog (shown as `task #<id>` when the API gives no title); those cannot be
reached by a fragment.

`status` (alias `current`) is the terse one-glance line: the last entry with its
wall-clock range, the idle gap since it stopped, and today's tracked total.

```sh
$ tg status
Code review [Backend] 10:30-11:00 (gap 0h25m) Today: 6h30m
```

The task name is cut to 60 characters (no ellipsis marker) so the line fits a
status bar. The gap only appears once now has moved past the entry's stop.

**The last entry is always today's**, and it is the single notion of "last
entry" in tg: `tg status` and a bare `tg mod` resolve it through the same
function, so they can never disagree about what they are talking about. Two
things are filtered out:

- **earlier days.** A new day starts with no last entry (`No entries. Today:
  0h00m`) rather than showing yesterday's — which `tg mod` could not edit
  anyway, since only today's entries can be modified.
- **entries that start later today.** Something booked three hours from now has
  not happened yet, so it is skipped by its *start* time; the last entry is
  still the one before it. (It does count towards the day's total, which is the
  whole day's tracked time.)

An entry that is still running in Toggl (pulled by `tg pull`) is reported as
running with its live elapsed time instead, whatever day it began on:

```sh
$ tg status
run Code review [Backend] (0h45m) Today: 2h00m
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
4  11:30-12:00 0h30m  Payment fix       [Payments]
               (gap 0h25m)
----------------------------------------
Total: 2h30m
```

The leading numbers are **persistent**. Each entry is given its number when it
is recorded — including entries that arrive from a `tg pull` — so the numbers
are the order things were added, they restart at 1 on every calendar day, and
they never change. A deleted entry takes its number with it, which is why the
listing above jumps from 2 to 4: numbers are never reused or shifted, so
`tg del 4` keeps meaning the same entry no matter what else you removed first.

Gap rows are not entries and carry no number: they show idle time between two
entries, and the last one shows the idle time since the newest entry stopped
(only within the same day, and never while an entry is running).

`--days N` looks further back. Since every day has its own 1..N, a multi-day
listing groups the entries under a date header; `mod`/`del` address today's
numbers. `--json` emits the same data with `num` on each entry.

## Usage

```
usage: tg <command> [flags]

commands:
  auth [token]              verify a Toggl API token and store config
  add <timesign> [project] <task>  add a finished entry [--desc TEXT]
  mod [num] [timesign]      retime/rename an entry (default: last) [--desc TEXT]
  del <num>                 delete the entry numbered by `tg ls`
  current | status          last entry, gap, day total        [--json]
  today   | list | ls       show today's entries     [--days N] [--json]
  tasks                     list cached tasks                 [--all] [--json]
  grep <fragment>           list cached tasks matching it     [--all] [--json]
  projects                  list cached projects with ids     [--all] [--json]
  projects update           sync all workspace projects       [--all] [--json]
  update <project>          refresh a project's tasks and pull its recent
                            entries                 [--days N] [--all] [--json]
  push                      send local changes to Toggl       [--json]
  pull [project]            fetch changes; all projects, or one [--since DATE] [--json]
  total [task]              total tracked hours per task; last 3 months [--since DATE] [--json]
  completion zsh            print the zsh completion script

timesign: absolute 9-:30, 10-11, 10:30-11:15 (today) or relative
      +:20, +1, +1:20 (that long, ending at the last 5m mark).
      Full spec: docs/timesig.md
mod:  numbers are the per-day ones shown by `tg ls`; without one the
      last entry is modified: today's newest already-started entry, the
      same one `tg status` shows. Only TODAY's entries can be modified.
      An absolute timesign sets the range on the entry's own day; a
      relative one EXTENDS the entry, keeping the start (`tg mod +:30`
      pushes the end 30m later).
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

`tg update <project>` is the per-project refresh: it fetches the project's task
list *and* pulls its recent time entries in one go. It does not sync the project
catalog itself — use `tg projects update` for that.

```sh
tg update backend            # tasks + entries touched since yesterday
tg update backend -n 7       # reach a week back instead
tg update -n 0 backend       # today only
```

The entry window defaults to **one day back** and is set by `--days`/`-n`, a
count of calendar days: the window starts at midnight that many days before
today (so `-n 1` means "since yesterday morning" whatever time you run it, and
`-n 0` means today only). Flags may come before or after the project name.
Because the pull is scoped to a single project, it is partial: it never advances
the `last_pull` watermark, so a later full `tg pull` still reconciles every
other project. The command stays quiet on success; use `--json` for the
project name, task count and pull counters.

### Environment

- `TOGGL_PROJECT_ID` scopes `add`, `tasks`, `grep`, and `update` to one project (and
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
