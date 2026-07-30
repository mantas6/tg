# Time signature ("timesign") spec

A *timesign* is the compact wall-clock notation `tg` accepts wherever a command
needs a finished time range (today: `tg add`; later also `tg mod`). Two forms
exist:

| Form     | Shape         | Example  | Meaning                                    |
| -------- | ------------- | -------- | ------------------------------------------ |
| absolute | `START-STOP`  | `9-:30`  | 09:00-09:30 today                          |
| relative | `+DURATION`   | `+:20`   | the last 20 minutes, ending at the current 5-minute mark |

A timesign is relative if and only if it starts with `+` (after trimming
surrounding whitespace). Everything else is parsed as absolute.

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

Note the asymmetry with `tg start`/`tg stop`, which snap to the *nearest*
5-minute mark (rounding up on ties). Relative timesigns always floor, so a
freshly added entry can never claim time in the future.

## Error cases

Both forms report a descriptive error rather than silently guessing. The parser
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
func Parse(s string, now time.Time, loc *time.Location) (Span, error)
func ParseAbsolute(s string, now time.Time, loc *time.Location) (Span, error)
func ParseRelative(s string, now time.Time, loc *time.Location) (Span, error)
func Floor5(t time.Time) time.Time
```

`Parse` dispatches on the `+` prefix; `ParseAbsolute` and `ParseRelative` are
exported for callers that accept only one form. `IsRelative` inspects the prefix
only — it is a classifier, not a validator, so `IsRelative("+bad")` is true
while `Parse("+bad", ...)` errors.
