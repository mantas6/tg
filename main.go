// Command tg is a local-first time tracker that records entries in SQLite and
// synchronizes them with Toggl Track on demand. See PLAN.md for the full design.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/mantas6/tg/api"
	"github.com/mantas6/tg/config"
	"github.com/mantas6/tg/store"
)

func main() {
	if len(os.Args) < 2 {
		printUsage(os.Stderr)
		os.Exit(1)
	}
	if err := run(os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "tg: "+err.Error())
		os.Exit(1)
	}
}

func run(cmd string, args []string) error {
	switch cmd {
	case "auth":
		return runAuth(args)
	case "add":
		return runAdd(args)
	case "mod":
		return runMod(args)
	case "del":
		return runDel(args)
	case "current", "status":
		return runCurrent(args)
	case "today", "list", "ls":
		return runToday(args)
	case "daily":
		return runDaily(args)
	case "tasks":
		return runTasks(args)
	case "grep":
		return runGrep(args)
	case "projects":
		return runProjects(args)
	case "update":
		return runUpdate(args)
	case "push":
		return runPush(args)
	case "pull":
		return runPull(args)
	case "total":
		return runTotal(args)
	case "completion":
		return runCompletion(args)
	case "help", "-h", "--help":
		printUsage(os.Stdout)
		return nil
	default:
		printUsage(os.Stderr)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// --- command wiring ----------------------------------------------------------

func runAdd(args []string) error {
	fs := newFlagSet("add")
	// --desc and --description are aliases bound to the same variable, so
	// either spelling sets the entry's description (empty leaves it blank).
	var desc string
	fs.StringVar(&desc, "desc", "", "entry description")
	fs.StringVar(&desc, "description", "", "entry description (alias of --desc)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()

	// First positional arg is the timesign. After it, two fragments mean
	// `<project> <task>` (the first scopes to a project, overriding
	// TOGGL_PROJECT_ID); one means `<task>` scoped by env.
	rest := fs.Args()
	if len(rest) < 2 {
		return errors.New("usage: tg add <timesign> [project] <task-fragment>")
	}
	timesign := rest[0]
	rest = rest[1:]
	projectID := projectIDFromEnv()
	fragment := strings.Join(rest, " ")
	if len(rest) == 2 {
		pid, err := resolveAddProject(st, rest[0])
		if err != nil {
			return err
		}
		projectID = pid
		fragment = rest[1]
	}
	return cmdAdd(os.Stdout, st, api.New(cfg.APIToken), cfg.WorkspaceID, projectID, timesign, fragment, desc, time.Now(), time.Local)
}

func runMod(args []string) error {
	fs := newFlagSet("mod")
	// Same --desc/--description alias pair as `add`; here an explicitly empty
	// value clears the description, which is why the flag's presence is tracked
	// separately (see setDesc below).
	var desc string
	fs.StringVar(&desc, "desc", "", "new entry description")
	fs.StringVar(&desc, "description", "", "new entry description (alias of --desc)")
	rest, err := parseArgsAndFlags(fs, args)
	if err != nil {
		return err
	}
	setDesc := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "desc" || f.Name == "description" {
			setDesc = true
		}
	})

	ref, timesign, err := parseModArgs(rest)
	if err != nil {
		return err
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	c, err := optionalClient()
	if err != nil {
		return err
	}
	return cmdMod(os.Stdout, st, c, ref, timesign, desc, setDesc, time.Now(), time.Local)
}

func runDel(args []string) error {
	fs := newFlagSet("del")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return errors.New("usage: tg del <entry-number>")
	}
	ref, err := parseEntryRef(rest[0])
	if err != nil {
		return err
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	c, err := optionalClient()
	if err != nil {
		return err
	}
	return cmdDel(os.Stdout, st, c, ref, time.Now(), time.Local)
}

func runCurrent(args []string) error {
	fs := newFlagSet("current")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	return cmdCurrent(os.Stdout, st, time.Now(), time.Local, *jsonOut)
}

func runToday(args []string) error {
	fs := newFlagSet("today")
	jsonOut := fs.Bool("json", false, "emit JSON")
	days := fs.Int("days", 1, "number of days to look back")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	color := term.IsTerminal(int(os.Stdout.Fd()))
	return cmdToday(os.Stdout, st, time.Now(), time.Local, *days, *jsonOut, color)
}

// dailyDefaultTarget is `tg daily`'s default target: 8 hours worked per day.
const dailyDefaultTarget = 8

func runDaily(args []string) error {
	fs := newFlagSet("daily")
	jsonOut := fs.Bool("json", false, "emit JSON")
	// --target and -t are aliases bound to the same variable: the target hours
	// worked per day that each listed day's overtime is measured against. It is
	// a float so half days (`-t 7.5`) work.
	var target float64
	fs.Float64Var(&target, "target", dailyDefaultTarget, "target hours worked per day")
	fs.Float64Var(&target, "t", dailyDefaultTarget, "target hours worked per day (alias of --target)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	return cmdDaily(os.Stdout, st, time.Now(), time.Local, target, *jsonOut)
}

func runTasks(args []string) error {
	fs := newFlagSet("tasks")
	all := fs.Bool("all", false, "include inactive tasks")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	return cmdTasks(os.Stdout, st, *all, projectIDFromEnv(), *jsonOut)
}

func runGrep(args []string) error {
	fs := newFlagSet("grep")
	all := fs.Bool("all", false, "include inactive tasks")
	jsonOut := fs.Bool("json", false, "emit JSON")
	// Flags may follow the fragment (`tg grep login --json`), so positionals
	// are peeled off the same way `tg mod` does it.
	rest, err := parseArgsAndFlags(fs, args)
	if err != nil {
		return err
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	// All positionals form ONE fragment, exactly like `tg add`/`tg total`.
	fragment := strings.Join(rest, " ")
	return cmdGrep(os.Stdout, st, *all, projectIDFromEnv(), fragment, *jsonOut)
}

func runProjects(args []string) error {
	// `projects` owns one subcommand: `tg projects update` syncs the catalog
	// from Toggl, while a bare `tg projects` lists what is already cached. The
	// subcommand word must come first (flags after it belong to it).
	if len(args) > 0 && args[0] == "update" {
		return runProjectsUpdate(args[1:])
	}
	fs := newFlagSet("projects")
	all := fs.Bool("all", false, "include inactive projects")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	return cmdProjects(os.Stdout, st, *all, *jsonOut)
}

func runProjectsUpdate(args []string) error {
	fs := newFlagSet("projects update")
	all := fs.Bool("all", false, "include inactive projects")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	return cmdUpdateProjects(os.Stdout, st, api.New(cfg.APIToken), cfg.WorkspaceID, *all, *jsonOut)
}

func runUpdate(args []string) error {
	fs := newFlagSet("update")
	all := fs.Bool("all", false, "include inactive tasks")
	jsonOut := fs.Bool("json", false, "emit JSON")
	// --days and -n are aliases bound to the same variable: how many days back
	// the time-entry pull reaches. The default is one day (see
	// resolveUpdateSince).
	var days int
	fs.IntVar(&days, "days", updateDefaultDays, "pull entries from the last N days")
	fs.IntVar(&days, "n", updateDefaultDays, "pull entries from the last N days (alias of --days)")
	// Flags may follow the project fragment (`tg update backend -n 3`), so
	// positionals are peeled off the same way `tg grep` does it.
	rest, err := parseArgsAndFlags(fs, args)
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	now := time.Now()
	since := resolveUpdateSince(days, now, time.Local)
	fragment := strings.Join(rest, " ")
	return cmdUpdate(os.Stdout, st, api.New(cfg.APIToken), cfg.WorkspaceID, projectIDFromEnv(), fragment, since, now, *all, *jsonOut)
}

func runPush(args []string) error {
	fs := newFlagSet("push")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	return cmdPush(os.Stdout, st, api.New(cfg.APIToken), time.Now(), *jsonOut)
}

func runPull(args []string) error {
	fs := newFlagSet("pull")
	jsonOut := fs.Bool("json", false, "emit JSON")
	sinceFlag := fs.String("since", "", "pull entries modified since DATE (YYYY-MM-DD)")
	// --all and -a are aliases bound to the same variable: they widen the
	// default today-only window to the whole current month (see
	// resolvePullSince).
	var all bool
	fs.BoolVar(&all, "all", false, "pull this month's entries instead of only today's")
	fs.BoolVar(&all, "a", false, "pull this month's entries (alias of --all)")
	// Flags may follow the project fragment (`tg pull backend -a`), so
	// positionals are peeled off the same way `tg update` does it.
	rest, err := parseArgsAndFlags(fs, args)
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()

	now := time.Now()
	since, err := resolvePullSince(*sinceFlag, all, now, time.Local)
	if err != nil {
		return err
	}
	fragment := strings.Join(rest, " ")
	// pull deliberately ignores TOGGL_PROJECT_ID (unlike add/tasks/update):
	// it always reconciles every project. Scoping happens only via an explicit
	// <project> argument, so the env project id is never passed through here.
	return cmdPull(os.Stdout, st, api.New(cfg.APIToken), fragment, since, now, *jsonOut)
}

func runTotal(args []string) error {
	fs := newFlagSet("total")
	jsonOut := fs.Bool("json", false, "emit JSON")
	sinceFlag := fs.String("since", "", "total entries since DATE (YYYY-MM-DD); default 3 months ago")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	now := time.Now()
	since, err := resolveTotalSince(*sinceFlag, now, time.Local)
	if err != nil {
		return err
	}
	// The store is needed even though the totals come from the Reports API: it
	// is what resolves the reported task ids to names and what fragments are
	// matched against (see cmdTotal).
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	// All positionals form ONE fragment, exactly like `tg add` (so
	// `tg total code review` searches for "code review").
	fragment := strings.Join(fs.Args(), " ")
	return cmdTotal(os.Stdout, st, api.New(cfg.APIToken), cfg.WorkspaceID, fragment, since, now, time.Local, *jsonOut)
}

func runAuth(args []string) error {
	fs := newFlagSet("auth")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return cmdAuth(os.Stdout, tokenSource(fs.Args()), func(token string) *api.Client {
		return api.New(token)
	})
}

func runCompletion(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: tg completion zsh")
	}
	return cmdCompletion(os.Stdout, args[0])
}

// --- helpers -----------------------------------------------------------------

// openStore ensures the state directory exists and opens the SQLite database.
func openStore() (*store.Store, error) {
	if _, err := config.EnsureDir(); err != nil {
		return nil, err
	}
	path, err := config.DBPath()
	if err != nil {
		return nil, err
	}
	return store.Open(path)
}

// optionalClient builds an API client from the stored config for the
// best-effort pushes done by the mutating commands. A missing config is not an
// error: the local edit still applies and stays dirty for a later `tg push`,
// which keeps `mod`/`del` usable before `tg auth`.
func optionalClient() (*api.Client, error) {
	cfg, err := config.Load()
	if errors.Is(err, config.ErrNotConfigured) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return api.New(cfg.APIToken), nil
}

// parseArgsAndFlags parses args with fs and returns the positional arguments,
// tolerating flags that appear AFTER a positional. The flag package stops at the
// first non-flag argument, which would make `tg mod 2 --desc x` treat --desc as
// a positional; parsing is therefore resumed after each positional is peeled
// off. Flag values set by earlier rounds are retained, so the order in which
// flags and positionals are mixed does not matter.
func parseArgsAndFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			return nil, err
		}
		rest = fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		rest = rest[1:]
	}
}

// parseModArgs classifies `tg mod`'s positional arguments. An all-digits
// argument is a local entry number (0 when absent, meaning the last entry);
// anything else is the timesign, since neither timesign form is bare digits (an
// absolute one needs a `-`, a relative one a leading `+`). Order does not
// matter, and neither may be given twice.
func parseModArgs(args []string) (ref int, timesign string, err error) {
	for _, a := range args {
		if isDigits(a) {
			if ref != 0 {
				return 0, "", fmt.Errorf("unexpected second entry number %q", a)
			}
			if ref, err = parseEntryRef(a); err != nil {
				return 0, "", err
			}
			continue
		}
		if timesign != "" {
			return 0, "", fmt.Errorf("unexpected second timesign %q", a)
		}
		timesign = a
	}
	return ref, timesign, nil
}

// parseEntryRef parses a local entry number (1, 2, 3... as shown by `tg ls`).
func parseEntryRef(s string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 1 {
		return 0, fmt.Errorf("invalid entry number %q; run `tg ls` to list entries", s)
	}
	return n, nil
}

// isDigits reports whether s is a non-empty run of ASCII digits (no sign, no
// separators), which is what distinguishes an entry number from a timesign.
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// projectIDFromEnv parses TOGGL_PROJECT_ID, returning nil when unset/invalid.
func projectIDFromEnv() *int64 {
	v := strings.TrimSpace(os.Getenv("TOGGL_PROJECT_ID"))
	if v == "" {
		return nil
	}
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return nil
	}
	return &id
}

// resolvePullSince determines `tg pull`'s window start. An explicit --since
// date wins; otherwise --all/-a widens the window to the first day of the
// current month and the default is the start of today. Both defaults are
// day-aligned in loc (like resolveUpdateSince) so they mean "today" and "this
// month" in calendar terms rather than a rolling cut.
//
// There is no explicit window END: sync.Pull asks Toggl for everything modified
// at or after `since`, and nothing can be modified after `now`, so today's
// window ends at now and the current month's window ends at the month's end as
// it is reached.
func resolvePullSince(sinceFlag string, all bool, now time.Time, loc *time.Location) (time.Time, error) {
	if sinceFlag != "" {
		t, err := time.ParseInLocation("2006-01-02", sinceFlag, loc)
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid --since %q (want YYYY-MM-DD)", sinceFlag)
		}
		return t, nil
	}
	if all {
		return startOfMonth(now, loc), nil
	}
	return startOfDay(now, loc), nil
}

// updateDefaultDays is `tg update`'s default entry window: one day back.
const updateDefaultDays = 1

// resolveUpdateSince turns `tg update`'s --days/-n count into the start of the
// entry pull window: midnight in loc, days calendar days before now. The window
// is day-aligned rather than a rolling 24h cut so `-n 1` means "since yesterday
// morning" no matter what time it is run. A negative count is clamped to 0
// (today only) instead of erroring, mirroring how cmdToday clamps --days.
func resolveUpdateSince(days int, now time.Time, loc *time.Location) time.Time {
	if days < 0 {
		days = 0
	}
	return startOfDay(now, loc).AddDate(0, 0, -days)
}

// resolveTotalSince determines the `tg total` window start: an explicit --since
// date (parsed in loc, mirroring resolveSince's format and error style), else
// the default three calendar months before now.
func resolveTotalSince(sinceFlag string, now time.Time, loc *time.Location) (time.Time, error) {
	if sinceFlag != "" {
		t, err := time.ParseInLocation("2006-01-02", sinceFlag, loc)
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid --since %q (want YYYY-MM-DD)", sinceFlag)
		}
		return t, nil
	}
	return now.AddDate(0, -3, 0), nil
}

// tokenSource returns a function that yields the API token: an explicit arg, a
// piped (non-TTY) stdin line, or an interactive masked prompt.
func tokenSource(args []string) func() (string, error) {
	return func() (string, error) {
		if len(args) > 0 {
			return args[0], nil
		}
		fd := int(os.Stdin.Fd())
		if !term.IsTerminal(fd) {
			data, err := io.ReadAll(os.Stdin)
			return string(data), err
		}
		fmt.Fprint(os.Stderr, "Toggl API token: ")
		b, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		return string(b), err
	}
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: tg <command> [flags]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "commands:")
	fmt.Fprintln(w, "  auth [token]              verify a Toggl API token and store config")
	fmt.Fprintln(w, "  add <timesign> [project] <task>  add a finished entry [--desc TEXT]")
	fmt.Fprintln(w, "  mod [num] [timesign]      retime/rename an entry (default: last) [--desc TEXT]")
	fmt.Fprintln(w, "  del <num>                 delete the entry numbered by `tg ls`")
	fmt.Fprintln(w, "  current | status          last entry, gap, day total        [--json]")
	fmt.Fprintln(w, "  today   | list | ls       show today's entries     [--days N] [--json]")
	fmt.Fprintln(w, "  daily                     this month's time per day and overtime")
	fmt.Fprintln(w, "                            vs a daily target      [-t HOURS] [--json]")
	fmt.Fprintln(w, "  tasks                     list cached tasks                 [--all] [--json]")
	fmt.Fprintln(w, "  grep <fragment>           list cached tasks matching it     [--all] [--json]")
	fmt.Fprintln(w, "  projects                  list cached projects with ids     [--all] [--json]")
	fmt.Fprintln(w, "  projects update           sync all workspace projects       [--all] [--json]")
	fmt.Fprintln(w, "  update <project>          refresh a project's tasks and pull its recent")
	fmt.Fprintln(w, "                            entries                 [--days N] [--all] [--json]")
	fmt.Fprintln(w, "  push                      send local changes to Toggl       [--json]")
	fmt.Fprintln(w, "  pull [project]            fetch today's changes; all projects, or one")
	fmt.Fprintln(w, "                            [-a|--all this month] [--since DATE] [--json]")
	fmt.Fprintln(w, "  total [task]              total tracked hours per task; last 3 months [--since DATE] [--json]")
	fmt.Fprintln(w, "  completion zsh            print the zsh completion script")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "timesign: absolute 9-:30, 10-11, 10:30-11:15 (today) or relative")
	fmt.Fprintln(w, "      +:20, +1, +1:20 (that long, ending at the last 5m mark).")
	fmt.Fprintln(w, "      Full spec: docs/timesig.md")
	fmt.Fprintln(w, "mod:  numbers are the per-day ones shown by `tg ls` (assigned when an")
	fmt.Fprintln(w, "      entry is added, never reused); without one the last entry is")
	fmt.Fprintln(w, "      modified: today's newest already-started entry, the same one")
	fmt.Fprintln(w, "      `tg status` shows. An absolute timesign sets the range on the")
	fmt.Fprintln(w, "      entry's own day; a relative one EXTENDS the entry, keeping the")
	fmt.Fprintln(w, "      start (`tg mod +:30` pushes the end 30m later).")
	fmt.Fprintln(w, "sync: run `tg pull` then `tg push` for correct last-writer-wins.")
	fmt.Fprintln(w, "env:  TOGGL_PROJECT_ID scopes `add`/`tasks`/`grep`/`update` to one project")
	fmt.Fprintln(w, "      (and sets the project on entries created by `add`). `pull`")
	fmt.Fprintln(w, "      ignores it and always reconciles every project; pass a")
	fmt.Fprintln(w, "      <project> name to `pull` to scope it explicitly. When unset,")
	fmt.Fprintln(w, "      `update` requires a unique <project> name and `add` accepts")
	fmt.Fprintln(w, "      `<timesign> <project> <task>` to scope by project name.")
}
