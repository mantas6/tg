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
*timesign* — an absolute `START-STOP` range on today's clock, a relative
`+DURATION` span counted back from now, or a bare `DURATION` that continues from
the last entry. (`tg mod` reads the same notation against an existing entry and
adds a fourth form, `-DURATION`, to take time back off it; see
[Fixing and removing entries](#fixing-and-removing-entries).)

```sh
tg add 9-:30 <task>         # 09:00-09:30
tg add 10-11 <task>         # 10:00-11:00
tg add 10:30-11 <task>      # 10:30-11:00
tg add @:30 <task>          # from now (floored to the last 5m mark) until :30
tg add @-16 <task>          # from now until 16:00
tg add +:20 <task>          # the last 20 minutes
tg add +1:20 <task>         # the last 1h20m
tg add 1:30 <task>          # 1h30m starting when the last entry ended
tg add :45 <task>           # 45m starting when the last entry ended
```

For absolute ranges each side is `H` or `H:MM`; the stop side may also be `:MM`,
inheriting the start hour. Hours are 0-23, minutes 0-59, and the stop must be
after the start. Either side may be `@`, which is now floored *down* to the
preceding 5-minute mark (14:23 -> 14:20); as the start it needs no dash, so
`@:30` reads "from now until :30". A relative timesign ends at that same mark
and starts that duration earlier. The full grammar, rounding rules, and error
cases are in [docs/timesig.md](docs/timesig.md).

A **bare duration** (no `+`, no `-`) leaves the start out: the entry begins where
the last one ended and runs that long, which is how you log a day of back-to-back
blocks. The "last entry" is the one `tg status` reports — today's newest entry
that has already started — so `tg add 1:30 <task>` after an entry ending at 10:00
records 10:00-11:30. With nothing tracked yet today there is nothing to continue
from, and a still-running entry has no end to start at; both are refused with an
error pointing at the other two forms rather than guessing a start.

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

Flags may come before or after the positional arguments, so
`tg add 9-:30 login --desc "…"` works too. `--date` books the entry on another
day; see [Another day: `--date`](#another-day---date).

### Ambiguous fragments and `-1`

Tasks and projects are named by **fragment**: a case-insensitive substring of
the name, with an exact name winning over mere substrings. When a fragment
matches several of them, the command refuses to guess and lists the candidates:

```
$ tg add 9-:30 "code review"
tg: multiple tasks match "code review":
  Code review [Backend]
  Code review [Payments]
pass -1 to use the first match
```

Refine the fragment when you can — but two tasks that *share* a name in
different projects cannot be told apart that way. Pass **`-1`** (alias
`--first`) to take the first candidate, the one listed at the top:

```sh
tg add 9-:30 "code review" -1     # records against Code review [Backend]
tg grep "code review" -1          # only the first of the matching tasks
```

`-1` is accepted by every command that resolves a fragment — `add`, `grep`,
`total`, `update` and `pull` — before or after the fragment, and it applies to
project fragments as well as task ones (`tg pull back -1`, `tg update back -1`).
It only ever resolves *ambiguity*: a fragment matching nothing still fails, and
on a fragment that already matches one candidate the flag changes nothing.
`grep` and `total` normally report every match, so there `-1` narrows the output
to the first one instead — for `total`, the first match of *each* of its
fragments (`grep` orders by project then name and gives an exact name no
precedence, so its first line is not always the task `add -1` picks).

### Another day: `--date`

`add` and `mod` work on **today**. Pass `--date YYYY-MM-DD` to point either of
them at a different day — for time you already know you will spend, or to fix
something you booked ahead earlier:

```sh
tg add --date 2026-01-05 9-10 <task>     # 09:00-10:00 on the 5th
tg add --date 2026-01-05 :30 <task>      # continues the 5th's last entry
tg mod --date 2026-01-05 +30             # extend that day's last entry
tg mod --date 2026-01-05 2 9-10:30       # retime entry 2 of the 5th
```

The date must be **today or later**. A day that is over is history: tg never
rewrites it (the same rule `tg mod` enforces on an old entry, see below), so a
past `--date` is refused outright rather than producing an entry no later
command could touch. "Today" is a calendar day in your local zone, not a
24-hour window.

On the day named, everything a command reckons per day is that day's:

- an **absolute** timesign resolves on it (`9-10` is 09:00-10:00 there);
- a **bare duration** continues from *that* day's last entry, so booking a
  block of back-to-back entries on a future day works exactly as it does today
  — and on a day with nothing on it yet there is nothing to continue from,
  which is refused;
- `mod`'s entry **numbers** and its default "last entry" are that day's too, so
  `tg mod --date … 2` and a bare `tg mod --date …` address what you booked
  there, not what you tracked today. Today's numbers do not reach it, and its
  numbers do not reach today.

A **relative** timesign (`+:20`) and the **`@`** now-token are the forms `--date`
cannot take: `+:20` means "the last 20 minutes", counted back from the current
5-minute mark, and `@` *is* that mark — another day has no such mark. `tg add
--date … +:20` and `tg add --date … @:30` are therefore refused, naming the
forms that do work. (`tg mod`'s `+30`/`-30` are fine: they only move an existing
entry's end by that much.)

Confirmation lines name the day whenever it is not today, since the clock times
alone no longer identify the entry:

```
$ tg add --date 2026-01-05 9-10 login
Added: Fix login bug [Backend]  09:00-10:00 (1h00m) on 2026-01-05
```

Entries booked ahead behave like any other: they are pushed to Toggl
immediately (best-effort), they take part in the overlap check *on their own
day* — an entry at 09:00 today is no conflict for one at 09:00 next week — and
they are excluded from `tg status`'s "last entry" and day total until their day
arrives.

Two commands do **not** take `--date` yet: `tg ls` lists today and days *behind*
it, and `tg del` addresses today's numbers only. Until a booked day comes
around, its entries are therefore visible (and removable) in the Toggl web app
rather than through tg — `tg mod --date` can still retime and re-describe them.

### Fixing and removing entries

`mod` edits an entry that already exists and `del` removes one. Both address
entries by the small per-day numbers printed by `tg ls` (see below), resolved on
today's day (`mod` also takes `--date` to resolve them on a later one); `mod`
also defaults to *the last entry* when no number is given — the same one
`tg status` reports, resolved the same way: today's newest entry that has
already started (see below).

```sh
tg mod +30                       # the last entry ran 30 minutes longer
tg mod -10                       # ...or 10 minutes shorter
tg mod 2 +1:15                   # push entry 2's end 1h15m later
tg mod 2 -1:20                   # pull entry 2's end 1h20m back
tg mod 2 9-10:30                 # set entry 2's range explicitly
tg mod --desc "reset password"   # only change the last entry's description
tg mod 2 +:45 --desc "rebased"   # extend and re-describe entry 2
tg del 3                         # remove entry 3
```

A timesign is read relative to the entry, not to today:

- an **absolute** range (`9-10:30`) sets start and stop on the entry's *own*
  calendar day, so an edit never drags an entry onto another day;
- a **signed** timesign moves the entry's *end* by that duration, keeping the
  start: `+30` *extends* it (an entry ending at 14:00 ends at 14:30) and `-30`
  *shortens* it (that entry ends at 13:30 instead). An all-digit duration
  without `:` is minutes for `mod`, so `+20`/`-20` are 20 minutes; `+1:20` and
  `-1:20` are 1h20m. Neither re-anchors the entry to now the way `tg add` does,
  and neither is an absolute length — repeating one keeps moving the end, and
  `+30 -30` is a round trip. A still-running entry has no end to move, so both
  refuse it; retime it with an absolute range instead.

Subtracting is capped by the entry itself: taking off its whole length (or more)
would leave nothing, so it is refused rather than producing an empty or
inside-out entry (`tg del` removes an entry, and an absolute range retimes one
outright).

`mod` does not take `add`'s bare duration form: it never moves an entry's start,
and `+DURATION`/`-DURATION` already say "this ran longer/shorter". Note that a
negative timesign is *not* the `-1` first-match flag other commands take: `mod`
has no such flag, so `tg mod -1` takes one minute off the last entry.

**A day that is over can never be modified.** Once an entry's calendar day has
passed it is history: `tg mod` refuses it outright ("refusing to update an entry
older than today") and nothing is written locally or sent to Toggl. The check is
enforced again in the storage layer, so no command can rewrite a past day, and
it is judged against the real clock — never against `--date`, which is why that
flag cannot name a past day either. Use the Toggl web app for genuine
corrections to older days.

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
tg grep login -1           # only the first match
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
tg total login docs                   # two fragments: one group each
tg total "code review"                # one fragment: "code review"
tg total                              # every task with tracked time
tg total --since 2025-01-01 login     # from 2025-01-01 through today
tg total write -1                     # only the first matching task
```

Each argument is its **own** task-name fragment (unlike `tg add`, which joins
them), matched against the cached tasks the same case-insensitive way as
`tg add` (exact name wins over substrings), so run `tg update` if the catalog is
stale, and quote a multi-word name to keep it one search. A fragment matching
several tasks totals all of them; `-1` narrows every fragment to its first
match. Without a fragment every task with tracked time is listed, including
tasks missing from the local catalog (shown as `task #<id>` when the API gives
no title); those cannot be reached by a fragment.

With several fragments each gets a header line carrying its own total, its tasks
indented below it, and the footer sums them:

```
$ tg total write login
write  1h15m
  Write docs   0h15m  [Backend]
  Write tests  1h00m  [Backend]
login  1h15m
  Fix login bug  1h15m  [Backend]
----------------------------------------
Total: 2h30m
```

Fragments are independent searches, so overlapping ones (`tg total write docs`)
list the shared task under both headers; the footer still counts it once, and so
stays the tracked time for the range rather than the sum of the headers. Every
fragment must find tracked time: one matching no cached task, or only tasks with
nothing tracked in the range, fails the whole command instead of quietly
dropping out of the report. `--json` adds a `fragments` array (each with its own
`tasks` and `total_seconds`) next to the usual distinct `tasks` and
`total_seconds`, and is unchanged for a single fragment.

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
function, so they can never disagree about what they are talking about. (A bare
`tg mod --date` resolves it the same way on the day it names, which is the only
thing that moves it off today.) Two things are filtered out:

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

The current (running) entry is marked with a `<` right after its number
(`2<` above where `2` would sit), so the one you are on stands out at a glance.
The marker takes the place of a padding space, so no other row shifts.

`--days N` looks further back. Since every day has its own 1..N, a multi-day
listing groups the entries under a date header; `mod`/`del` address today's
numbers. `--json` emits the same data with `num` on each entry.

### Daily totals and overtime

`daily` zooms out to the month: one line per day with everything you tracked on
it and how far that is from a daily target, then a footer with the month's
total and its overall overtime.

```sh
$ tg daily
Mon 2026-01-05  8h30m   +0:30
Tue 2026-01-06  7h15m   -0:45
Wed 2026-01-07  8h00m*  +0:00
----------------------------------------
Total: 23h45m  -0:15  (3 days x 8h00m)   (* running)
```

The window is the **whole current calendar month**, from the 1st to the last
day, regardless of where in the month you run it. The target is `-t`/`--target`
in hours, defaulting to **8**, and fractional targets work (`-t 7.5`). The third
column is that day's tracked time minus the target, always signed, as
`h:mm` — `+0:30` is half an hour over, `-0:45` is three quarters of an hour
short.

```sh
tg daily              # this month against an 8h/day target
tg daily -t 6         # ...against 6h/day
tg daily --target 7.5 # half-hour targets are fine
tg daily -n           # exclude today (alias --no-today)
tg daily --json       # machine-readable
```

`-n`/`--no-today` drops today's row from the listing **and** the footer's
totals, so the report covers only the days that are already over — handy while
today is still in progress and its half-finished figure would otherwise skew
the overtime. Days booked *ahead* are kept, since `-n` removes only today
itself, not the future.

**Only days you actually tracked something get a line**, and the footer's target
is the daily target multiplied by the number of *listed* days — so weekends and
days off never accumulate a deficit, and the total overtime answers "am I ahead
or behind on the days I worked?". A day is summed from the entries that *start*
on it (an entry crossing midnight counts entirely towards the day it began), and
a still-running entry contributes its elapsed time so far exactly as `tg ls` and
`tg status` count it, marked with `*`.

Days *after today* (time booked ahead) are greyed out, so planned days are easy
to tell apart from worked ones. Like the project colors in `ls`, the dimming is
only emitted when the output is a terminal — piped or redirected output stays
plain, and `--json` never carries styling.

Everything comes from the local store, so run `tg pull` first if days tracked
elsewhere are missing. `--json` returns
`{"days":[{"date":"2026-01-05","duration_seconds":30600,"overtime_seconds":1800,"running":false},...],"total_seconds":...,"target_seconds":28800,"overtime_seconds":...}`.

## Usage

```
usage: tg <command> [flags]

commands:
  auth [token]              verify a Toggl API token and store config
  add <timesign> [project] <task>  add a finished entry
                            [--desc TEXT] [--date DATE] [-1]
  mod [num] [timesign]      retime/rename an entry (default: last), e.g.
                            +30 / -30 minutes  [--desc TEXT] [--date DATE]
  del <num>                 delete the entry numbered by `tg ls`
  current | status          last entry, gap, day total        [--json]
  today   | list | ls       show today's entries     [--days N] [--json]
  daily                     this month's time per day and overtime
                            vs a daily target [-t HOURS] [-n] [--json]
  tasks                     list cached tasks                 [--all] [--json]
  grep <fragment>           list cached tasks matching it [--all] [--json] [-1]
  projects                  list cached projects with ids     [--all] [--json]
  projects update           sync all workspace projects       [--all] [--json]
  update [project]          refresh a project's tasks and pull its recent
                            entries [-p FRAGMENT] [--days N] [--all] [--json] [-1]
  push                      send local changes to Toggl       [--json]
  pull [project]            fetch today's changes; all projects, or one
                            [-a|--all this month] [--since DATE] [--json] [-1]
  total [task...]           total tracked hours per task, one group per named
                            task; last 3 months [--since DATE] [--json] [-1]
  completion zsh            print the zsh completion script

timesign: absolute 9-:30, 10-11, 10:30-11:15 (today); `@` in a range is
      now floored to the last 5m mark, and `@:30` (no dash) is from now
      until :30. relative +:20, +1, +1:20 (that long, ending at the last
      5m mark), negative -:20, -1:20 (that much LESS; `mod` only), or a
      bare duration 1:30, :45 (that long, starting where the last entry
      ended; `add` only). Full spec: docs/timesig.md
mod:  numbers are the per-day ones shown by `tg ls`; without one the
      last entry is modified: today's newest already-started entry, the
      same one `tg status` shows. A day that is OVER can never be
      modified (see `date`).
      An absolute timesign sets the range on the entry's own day; a
      signed one moves its END, keeping the start: `tg mod +30` pushes
      the end 30m later, `tg mod -30` pulls it 30m back (never past the
      start); a number without `:` is minutes for `mod`.
date: `add` and `mod` work on today; `--date YYYY-MM-DD` moves them to
      another day, which must be today or later (a day that is over is
      history and is refused). On a moved day the entry numbers, the
      "last entry" and an absolute timesign are all that day's; a
      relative or `@` timesign has no `now` there, so `add` refuses one
      (`mod` still takes +30/-30, which only move an end).
-1:   `add`/`grep`/`total`/`update`/`pull` match tasks and projects by
      name fragment; a fragment matching several of them normally
      fails with the candidates listed. `-1` (alias `--first`) takes
      the first candidate instead, which is how two tasks sharing a
      name in different projects are told apart.
```

### Syncing

Run `tg pull` then `tg push` for correct last-writer-wins behavior:

```sh
tg pull    # fetch today's remote changes into the local store
tg push    # send local changes to Toggl
```

`tg pull` reconciles a **time window**, which defaults to **today** (from
midnight, local time). Widen it to the whole current calendar month — from the
1st to the month's end — with `-a`/`--all`, or pick an explicit start with
`--since DATE`, which wins over both:

```sh
tg pull                # today's changes, every project
tg pull -a             # everything changed this month
tg pull backend -a     # ...scoped to one project
tg pull back -1        # ...taking the first project the fragment matches
tg pull --since 2026-01-01
```

Only entries **modified** inside the window are fetched, so the usual
today-sized pull stays cheap. A window that does not reach back to the
`last_pull` watermark is partial and leaves it untouched, so nothing that
changed in the meantime is ever marked as reconciled.

`tg push` runs automatically (best-effort) when you `tg add`, so the entry shows
up in the Toggl web app immediately. If the network is unavailable, the entry
stays local and dirty until the next `tg push`.

An entry Toggl refuses is skipped rather than fatal: the rest of the dirty queue
is still sent, the rejected entry stays dirty, and `tg push` reports it (and
exits non-zero). One permanently rejected entry can therefore not block the
entries behind it. Under `--json` the rejections are listed in the result's
`failed` array.

`tg update [project]` is the per-project refresh: it fetches the project's task
list *and* pulls its recent time entries in one go. It does not sync the project
catalog itself — use `tg projects update` for that.

```sh
tg update backend            # tasks + entries touched since yesterday
tg update -p backend         # same, naming the project with a flag
tg update backend -n 7       # reach a week back instead
tg update -n 0 backend       # today only
tg update code review        # a multi-word fragment needs no quoting
```

The project is named by **fragment**, the same case-insensitive substring match
`add`, `grep` and `pull` use: `pay` finds `Payments`. Pass it either as the
positional argument or with `--project`/`-p` — the two are equivalent, so pick
whichever reads better; passing both at once is an error. An exact
(case-insensitive) name always wins over mere substrings, and a fragment that
still matches several projects fails with the candidates listed by name and id:

```
$ tg update back
tg: multiple projects match "back":
  Backend (1)
  Backend API (3)
pass -1 to use the first match
```

Refine the fragment (`tg update "backend api"`), pass `-1` to take the first
candidate listed, or export `TOGGL_PROJECT_ID`, which takes precedence and also
lets you update a project that is not cached yet.

The entry window defaults to **one day back** and is set by `--days`/`-n`, a
count of calendar days: the window starts at midnight that many days before
today (so `-n 1` means "since yesterday morning" whatever time you run it, and
`-n 0` means today only). Flags may come before or after the project name.
Because the pull is scoped to a single project, it is partial: it never advances
the `last_pull` watermark, so a later `tg pull` still reconciles every other
project. The command stays quiet on success; use `--json` for the project name,
task count and pull counters.

### Environment

- `TOGGL_PROJECT_ID` scopes `add`, `tasks`, `grep`, and `update` to one project (and
  sets the project on entries created by `add`). A value that is not a numeric
  project id is an error, not a silent "unset", so a typo cannot quietly widen
  the scope. `pull` ignores it and always
  reconciles every project; pass a `<project>` name to `pull` to scope it
  explicitly. When unset, `update` requires a `<project>` fragment (positional
  or `--project`/`-p`) that matches exactly one cached project — or several with
  `-1`, which takes the first — and `add` accepts
  `<timesign> <project> <task>` to scope by project name.
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
