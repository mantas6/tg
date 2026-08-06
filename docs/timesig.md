# Time signature ("timesign") spec

A *timesign* is the compact wall-clock notation `tg` accepts wherever a command
needs a finished time range (`tg add`, `tg mod`). Three forms exist:

| Form     | Shape         | Example  | Meaning                                    |
| -------- | ------------- | -------- | ------------------------------------------ |
| absolute | `START-STOP`  | `9-:30`  | 09:00-09:30 today                          |
| relative | `+DURATION`   | `+:20`   | the last 20 minutes, ending at the current 5-minute mark |
| duration | `DURATION`    | `1:30`   | a bare 1h30m length; the command supplies the start |

The form is decided by punctuation, after trimming surrounding whitespace:

- it is **relative** if and only if it starts with `+`;
- otherwise it is a **duration** if it is non-empty and contains no `-`;
- everything else (including an empty timesign) is parsed as **absolute**.

So `1:30` is a duration, `+1:30` a relative span and `1:30-3` an absolute range.
The absolute and relative forms resolve to a time range on their own. A bare
duration does not, so it is only accepted where the command can anchor it —
currently just `tg add`, which starts it at the last entry's end.

The reference implementation is the `timesig` package (`timesig/timesig.go`);
this document and that package must stay in sync.

## Common lexical rules

```
H   = 1*DIGIT   ; 0-23
MM  = 1*DIGIT   ; 0-59
```

- Only ASCII digits are accepted. Signs (`-9`, `+9`), spaces inside a number,
  and any other decoration are errors.
- Leading zeros are fine: `09`, `01:05`.
- Whitespace around the whole timesign, around the `-` separator, and after the
  leading `+` is trimmed. Whitespace *inside* a number is not.
- Seconds cannot be expressed; all resolved times have zero seconds.

## Absolute form

```
timesign = START "-" STOP
START    = H | H ":" MM
STOP     = H | H ":" MM | ":" MM
```

Both sides resolve to wall-clock times on **now's calendar day** in the active
location (`TZ`). Rules:

- A missing minute component defaults to `00`: `10-11` is 10:00-11:00.
- A minutes-only `STOP` (`:MM`) inherits `START`'s hour: `9-:30` is 09:00-09:30.
- A minutes-only `START` is **not** allowed: `:30-10` is an error.
- `STOP` must be strictly after `START`.
- No day arithmetic: absolute timesigns cannot cross midnight. Use two entries.
- No rounding: the times are taken exactly as written (`9:07-9:11` is honored).

Examples:

| Input        | Start | Stop  | Duration |
| ------------ | ----- | ----- | -------- |
| `9-:30`      | 09:00 | 09:30 | 30m      |
| `10-11`      | 10:00 | 11:00 | 1h       |
| `10:30-11`   | 10:30 | 11:00 | 30m      |
| `9:15-9:45`  | 09:15 | 09:45 | 30m      |
| `0-:01`      | 00:00 | 00:01 | 1m       |
| `23-23:59`   | 23:00 | 23:59 | 59m      |
| ` 9 - :30 `  | 09:00 | 09:30 | 30m      |

## Relative form

```
timesign = "+" DURATION
DURATION = H | H ":" MM | ":" MM
```

`H` is a count of hours (0-23) and `MM` a count of minutes (0-59), so the
longest expressible span is `+23:59`. The span is anchored to now:

- **Stop** = now floored **down** to the preceding 5-minute wall-clock mark
  (`00, 05, 10, ..., 55`), seconds zeroed. A time already on a mark is
  unchanged. Flooring never rounds up: 14:59 -> 14:55.
- **Start** = that stop minus `DURATION`.
- The duration must be **greater than zero**.
- Start may fall on the previous day; crossing midnight backwards is allowed.
- Flooring uses the active location's wall clock, so the mark is local, not UTC.

Examples (assuming now = 15:07 unless noted):

| Input   | now   | Start          | Stop  | Duration |
| ------- | ----- | -------------- | ----- | -------- |
| `+:20`  | 15:07 | 14:45          | 15:05 | 20m      |
| `+:05`  | 15:07 | 15:00          | 15:05 | 5m       |
| `+1`    | 15:07 | 14:05          | 15:05 | 1h       |
| `+2`    | 15:07 | 13:05          | 15:05 | 2h       |
| `+1:20` | 15:07 | 13:45          | 15:05 | 1h20m    |
| `+:20`  | 14:23 | 14:00          | 14:20 | 20m      |
| `+:20`  | 14:00 | 13:40          | 14:00 | 20m      |
| `+2`    | 00:30 | 22:30 prev day | 00:30 | 2h       |

Relative timesigns always floor (never round to the *nearest* mark), so a freshly
added entry can never claim time in the future.

## Duration form

```
timesign = DURATION
DURATION = H | H ":" MM | ":" MM
```

The same `DURATION` production as the relative form, without the `+`, so `1:30`
is 1h30m, `:45` is 45 minutes, `2` is two hours and the longest expressible span
is again `23:59`. The duration must be greater than zero.

This form carries **no anchor**: it resolves to a length only, and the command
decides where that length starts (see `tg add` below). Neither the current time
nor the active location affects the parse.

Examples:

| Input   | Duration |
| ------- | -------- |
| `1:30`  | 1h30m    |
| `:45`   | 45m      |
| `2`     | 2h       |
| `01:05` | 1h5m     |
| `23:59` | 23h59m   |

## Interpretation per command

The parser above is shared, but a command decides which reference instant a
timesign is resolved against, and `tg mod` deliberately reads the relative form
differently from `tg add`.

### `tg add <timesign>`

The plain reading of the two anchored forms: absolute ranges land on **today**,
and a relative span **ends at now** (floored to the preceding 5-minute mark) and
starts `DURATION` earlier. This is the "I just worked that long" case.

A bare duration means "that long, **starting where I left off**": the new entry
starts at the **end of the last entry** and stops `DURATION` later, so
`tg add 1:30 <task>` after an entry ending at 10:00 records 10:00-11:30. The
last entry is resolved exactly as `tg status` reports it and a bare `tg mod`
edits it (today's newest already-started entry), so:

- an entry from an **earlier day** is never the anchor — on a day with nothing
  tracked yet the form is refused ("no entry tracked today to continue from")
  instead of starting at now or reaching back into history;
- a **running** last entry has no end to start from, so it is refused too (the
  same reason `tg mod +DURATION` refuses one);
- an entry booked for **later today** is skipped when picking the anchor, so a
  long enough duration can run into it — the ordinary overlap guard reports
  that, exactly as for the other two forms.

Unlike a relative span, a bare duration is not floored and may end in the
future (an absolute range may too): the anchor is the last stop, not now. For
the same reason it may run past midnight — the start is always today's, so the
entry is still recorded on today, the way a relative span may start on the
previous day.

### `tg mod [num] <timesign>`

`mod` retimes an entry that already exists, so both anchored forms are read
against that entry instead of against today/now:

| Form                | Effect on the entry                                        |
| ------------------- | ---------------------------------------------------------- |
| absolute `9-10:30`  | start and stop are set on the **entry's own calendar day** |
| relative `+20`      | the entry **keeps its start**; stop becomes stop + 20 minutes |

So a relative timesign **extends** the entry, it does not re-anchor it to now
and it is not an absolute length: `tg mod +30` means "that entry ran 30 minutes
longer than recorded", so an entry ending at 14:00 ends at 14:30. Topping up the
entry you just finished is the common correction; re-anchoring would silently
move an entry recorded hours ago, and running `mod +30` twice adds an hour
rather than being a no-op.

A **running** entry has no stop to add to, so `mod +DURATION` refuses it (use an
absolute range to give it a finished span instead).

`mod` only ever edits **today's** entries: an entry whose start falls on an
earlier calendar day is refused before the timesign is even applied, so the
"entry's own calendar day" is in practice today's.

Unlike other uses of relative timesigns, an all-digit duration without `:` is
minutes for `mod`: `mod +20` adds 20 minutes, while `mod +1:20` adds 1h20m.
`+0` and friends are still errors, and the 5-minute flooring is irrelevant here
because only `DURATION` is used.
Absolute ranges still cannot cross midnight, and the resulting span must not
overlap another entry.

`mod` does **not** accept the bare duration form: it has no start to anchor
(`mod` never moves an entry's start), and `+DURATION` already covers "this ran
longer". A bare `1:30` there is reported as a malformed absolute range.

## Error cases

Every form reports a descriptive error rather than silently guessing. The parser
rejects:

Absolute:

- no `-` separator: `9`
- an empty side: `-11`, `9-`, `-`, `` (empty input)
- hour out of range: `24-25`, `9-24`
- minute out of range: `9:60-10`, `9-9:60`
- minutes-only start: `:30-10`
- stop not after start: `9-9`, `10-9`, `9-:00`
- non-numeric components: `ab-cd`, `9:aa-10`

Relative:

- zero duration: `+0`, `+:00`, `+0:00`
- empty duration: `+`, `+ `
- hour out of range: `+24`
- minute out of range: `+1:60`
- non-numeric or malformed components: `+a`, `+:ab`, `+1:`, `+-1`, `++1`,
  `+1:20x`
- a range after the `+`: `+9-10`

Duration:

- zero duration: `0`, `:00`, `0:00`
- empty duration: `` (empty input), ` `
- hour out of range: `24`
- minute out of range: `1:60`
- non-numeric or malformed components: `a`, `:ab`, `1:`, `1:20x`

(An empty input and anything containing `-` never reach the duration parser via
`IsDuration`; they are classified as absolute and rejected there.)

Beyond the parser, `tg add` refuses a bare duration it cannot anchor: no entry
tracked today, or a last entry that is still running.

## API

```go
package timesig

type Kind int
const (
    Absolute Kind = iota
    Relative
)
func (k Kind) String() string

type Span struct {
    Kind  Kind
    Start time.Time
    Stop  time.Time
}
func (s Span) Duration() time.Duration

const RelativePrefix = "+"

func IsRelative(s string) bool
func IsDuration(s string) bool
func Parse(s string, now time.Time, loc *time.Location) (Span, error)
func ParseAbsolute(s string, now time.Time, loc *time.Location) (Span, error)
func ParseRelative(s string, now time.Time, loc *time.Location) (Span, error)
func ParseDuration(s string) (time.Duration, error)
func Floor5(t time.Time) time.Time
```

`Parse` dispatches on the `+` prefix between the two forms that resolve to a
Span; `ParseAbsolute` and `ParseRelative` are exported for callers that accept
only one of them. The bare duration form yields no Span, so it has no Kind and
is parsed by `ParseDuration`, which needs neither `now` nor a location; callers
route to it with `IsDuration` before falling back to `Parse`.

`IsRelative` and `IsDuration` inspect punctuation only — they are classifiers,
not validators, so `IsRelative("+bad")` and `IsDuration("bad")` are both true
while parsing either errors.
