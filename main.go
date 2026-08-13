// Command tg is a local-first time tracker that records entries in SQLite and
// synchronizes them with Toggl Track on demand. See PLAN.md for the full design.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/mantas6/tg/api"
	"github.com/mantas6/tg/config"
	"github.com/mantas6/tg/store"
)

func main() {
	// One context per invocation, cancelled on Ctrl-C (SIGINT) or SIGTERM. It
	// is threaded through the run*/cmd* layers into the HTTP client and every
	// SQLite statement, so an interrupt stops a hanging sync or query instead
	// of being noticed only after it finished. NotifyContext also restores the
	// default signal behavior on stop, so a second Ctrl-C still kills tg
	// outright if the first one somehow does not land.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if len(os.Args) < 2 {
		printUsage(os.Stderr)
		os.Exit(1)
	}
	if err := run(ctx, os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "tg: "+err.Error())
		os.Exit(1)
	}
}

func run(ctx context.Context, cmd string, args []string) error {
	switch cmd {
	case "auth":
		return runAuth(ctx, args)
	case "add":
		return runAdd(ctx, args)
	case "mod":
		return runMod(ctx, args)
	case "del":
		return runDel(ctx, args)
	case "current", "status":
		return runCurrent(ctx, args)
	case "today", "list", "ls":
		return runToday(ctx, args)
	case "daily":
		return runDaily(ctx, args)
	case "tasks":
		return runTasks(ctx, args)
	case "grep":
		return runGrep(ctx, args)
	case "projects":
		return runProjects(ctx, args)
	case "update":
		return runUpdate(ctx, args)
	case "push":
		return runPush(ctx, args)
	case "pull":
		return runPull(ctx, args)
	case "total":
		return runTotal(ctx, args)
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

func runAdd(ctx context.Context, args []string) error {
	fs := newFlagSet("add")
	// --desc and --description are aliases bound to the same variable, so
	// either spelling sets the entry's description (empty leaves it blank).
	var desc string
	fs.StringVar(&desc, "desc", "", "entry description")
	fs.StringVar(&desc, "description", "", "entry description (alias of --desc)")
	var first bool
	bindFirstFlag(fs, &first, "task or project")
	// Flags may follow the timesign and the fragment (`tg add 9-10 login -1`),
	// so positionals are peeled off the same way `tg mod` does it. No timesign
	// form starts with "-" (an absolute one needs a digit first), so nothing
	// positional is mistaken for a flag.
	rest, err := parseArgsAndFlags(fs, args)
	if err != nil {
		return err
	}
	// First positional arg is the timesign. After it, two fragments mean
	// `<project> <task>` (the first scopes to a project, overriding
	// TOGGL_PROJECT_ID); one means `<task>` scoped by env.
	if len(rest) < 2 {
		return errors.New("usage: tg add <timesign> [project] <task-fragment>")
	}
	timesign := rest[0]
	rest = rest[1:]
	projectID, err := projectIDFromEnv()
	if err != nil {
		return err
	}
	// The project-name form is resolved against the cached catalog, so it needs
	// the store and happens inside withEnv.
	return withEnv(ctx, func(env *cmdEnv) error {
		pid, fragment := projectID, strings.Join(rest, " ")
		if len(rest) == 2 {
			resolved, err := resolveAddProject(env.ctx, env.st, rest[0], first)
			if err != nil {
				return err
			}
			pid, fragment = resolved, rest[1]
		}
		return cmdAdd(env, pid, first, timesign, fragment, desc)
	})
}

func runMod(ctx context.Context, args []string) error {
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
	return withEnv(ctx, func(env *cmdEnv) error {
		return cmdMod(env, ref, timesign, desc, setDesc)
	})
}

func runDel(ctx context.Context, args []string) error {
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
	return withEnv(ctx, func(env *cmdEnv) error { return cmdDel(env, ref) })
}

func runCurrent(ctx context.Context, args []string) error {
	fs := newFlagSet("current")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return withEnv(ctx, func(env *cmdEnv) error { return cmdCurrent(env, *jsonOut) })
}

func runToday(ctx context.Context, args []string) error {
	fs := newFlagSet("today")
	jsonOut := fs.Bool("json", false, "emit JSON")
	days := fs.Int("days", 1, "number of days to look back")
	if err := fs.Parse(args); err != nil {
		return err
	}
	color := term.IsTerminal(int(os.Stdout.Fd()))
	return withEnv(ctx, func(env *cmdEnv) error {
		return cmdToday(env, *days, *jsonOut, color)
	})
}

// dailyDefaultTarget is `tg daily`'s default target: 8 hours worked per day.
const dailyDefaultTarget = 8

func runDaily(ctx context.Context, args []string) error {
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
	color := term.IsTerminal(int(os.Stdout.Fd()))
	return withEnv(ctx, func(env *cmdEnv) error {
		return cmdDaily(env, target, *jsonOut, color)
	})
}

func runTasks(ctx context.Context, args []string) error {
	fs := newFlagSet("tasks")
	all := fs.Bool("all", false, "include inactive tasks")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	projectID, err := projectIDFromEnv()
	if err != nil {
		return err
	}
	return withEnv(ctx, func(env *cmdEnv) error {
		return cmdTasks(env, *all, projectID, *jsonOut)
	})
}

func runGrep(ctx context.Context, args []string) error {
	fs := newFlagSet("grep")
	all := fs.Bool("all", false, "include inactive tasks")
	jsonOut := fs.Bool("json", false, "emit JSON")
	var first bool
	bindFirstFlag(fs, &first, "task")
	// Flags may follow the fragment (`tg grep login --json`), so positionals
	// are peeled off the same way `tg mod` does it.
	rest, err := parseArgsAndFlags(fs, args)
	if err != nil {
		return err
	}
	projectID, err := projectIDFromEnv()
	if err != nil {
		return err
	}
	// All positionals form ONE fragment, exactly like `tg add`/`tg total`.
	fragment := strings.Join(rest, " ")
	return withEnv(ctx, func(env *cmdEnv) error {
		return cmdGrep(env, *all, projectID, first, fragment, *jsonOut)
	})
}

func runProjects(ctx context.Context, args []string) error {
	// `projects` owns one subcommand: `tg projects update` syncs the catalog
	// from Toggl, while a bare `tg projects` lists what is already cached. The
	// subcommand word must come first (flags after it belong to it).
	if len(args) > 0 && args[0] == "update" {
		return runProjectsUpdate(ctx, args[1:])
	}
	fs := newFlagSet("projects")
	all := fs.Bool("all", false, "include inactive projects")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return withEnv(ctx, func(env *cmdEnv) error {
		return cmdProjects(env, *all, *jsonOut)
	})
}

func runProjectsUpdate(ctx context.Context, args []string) error {
	fs := newFlagSet("projects update")
	all := fs.Bool("all", false, "include inactive projects")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return withEnv(ctx, func(env *cmdEnv) error {
		return cmdUpdateProjects(env, *all, *jsonOut)
	})
}

func runUpdate(ctx context.Context, args []string) error {
	fs := newFlagSet("update")
	all := fs.Bool("all", false, "include inactive tasks")
	jsonOut := fs.Bool("json", false, "emit JSON")
	// --days and -n are aliases bound to the same variable: how many days back
	// the time-entry pull reaches. The default is one day (see
	// resolveUpdateSince).
	var days int
	fs.IntVar(&days, "days", updateDefaultDays, "pull entries from the last N days")
	fs.IntVar(&days, "n", updateDefaultDays, "pull entries from the last N days (alias of --days)")
	// --project and -p are aliases naming the project by fragment, exactly
	// like the positional form (`tg update backend` == `tg update -p backend`).
	var project string
	fs.StringVar(&project, "project", "", "project name fragment to update")
	fs.StringVar(&project, "p", "", "project name fragment to update (alias of --project)")
	var first bool
	bindFirstFlag(fs, &first, "project")
	// Flags may follow the project fragment (`tg update backend -n 3`), so
	// positionals are peeled off the same way `tg grep` does it.
	rest, err := parseArgsAndFlags(fs, args)
	if err != nil {
		return err
	}
	fragment, err := updateProjectFragment(project, rest)
	if err != nil {
		return err
	}
	projectID, err := projectIDFromEnv()
	if err != nil {
		return err
	}
	return withEnv(ctx, func(env *cmdEnv) error {
		since := resolveUpdateSince(days, env.now, env.loc)
		return cmdUpdate(env, projectID, first, fragment, since, *all, *jsonOut)
	})
}

func runPush(ctx context.Context, args []string) error {
	fs := newFlagSet("push")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return withEnv(ctx, func(env *cmdEnv) error { return cmdPush(env, *jsonOut) })
}

func runPull(ctx context.Context, args []string) error {
	fs := newFlagSet("pull")
	jsonOut := fs.Bool("json", false, "emit JSON")
	sinceFlag := fs.String("since", "", "pull entries modified since DATE (YYYY-MM-DD)")
	// --all and -a are aliases bound to the same variable: they widen the
	// default today-only window to the whole current month (see
	// resolvePullSince).
	var all bool
	fs.BoolVar(&all, "all", false, "pull this month's entries instead of only today's")
	fs.BoolVar(&all, "a", false, "pull this month's entries (alias of --all)")
	var first bool
	bindFirstFlag(fs, &first, "project")
	// Flags may follow the project fragment (`tg pull backend -a`), so
	// positionals are peeled off the same way `tg update` does it.
	rest, err := parseArgsAndFlags(fs, args)
	if err != nil {
		return err
	}
	fragment := strings.Join(rest, " ")
	return withEnv(ctx, func(env *cmdEnv) error {
		since, err := resolvePullSince(*sinceFlag, all, env.now, env.loc)
		if err != nil {
			return err
		}
		// pull deliberately ignores TOGGL_PROJECT_ID (unlike add/tasks/update):
		// it always reconciles every project. Scoping happens only via an
		// explicit <project> argument, so the env project id is never passed
		// through here.
		return cmdPull(env, first, fragment, since, *jsonOut)
	})
}

func runTotal(ctx context.Context, args []string) error {
	fs := newFlagSet("total")
	jsonOut := fs.Bool("json", false, "emit JSON")
	sinceFlag := fs.String("since", "", "total entries since DATE (YYYY-MM-DD); default 3 months ago")
	var first bool
	bindFirstFlag(fs, &first, "task")
	// Flags may follow the fragment (`tg total login --json`), so positionals
	// are peeled off the same way `tg grep` does it.
	rest, err := parseArgsAndFlags(fs, args)
	if err != nil {
		return err
	}
	// All positionals form ONE fragment, exactly like `tg add` (so
	// `tg total code review` searches for "code review").
	fragment := strings.Join(rest, " ")
	// The store is opened even though the totals come from the Reports API: it
	// is what resolves the reported task ids to names and what fragments are
	// matched against (see cmdTotal).
	return withEnv(ctx, func(env *cmdEnv) error {
		since, err := resolveTotalSince(*sinceFlag, env.now, env.loc)
		if err != nil {
			return err
		}
		return cmdTotal(env, first, fragment, since, *jsonOut)
	})
}

func runAuth(ctx context.Context, args []string) error {
	fs := newFlagSet("auth")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// auth is the one command that needs no store and no stored config: it is
	// what writes the config, so it builds its client from the typed token.
	return cmdAuth(ctx, os.Stdout, tokenSource(fs.Args()), func(token string) *api.Client {
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

// withEnv is the prologue every command shares: it loads the stored
// credentials (if any), opens the SQLite database, hands the resulting cmdEnv to
// fn and closes the database again afterwards. It replaces the
// config-then-store-then-defer-close block that each run* function used to
// repeat, so the commands differ only in their own flags and arguments.
//
// A missing config is deliberately NOT an error here. tg is local-first: adding,
// editing, deleting and listing entries all work without credentials (the edits
// stay dirty for a later `tg push`), so only the commands that genuinely need
// the API say so, by asking for the client (see cmdEnv.client) — which then
// reports the very same "run `tg auth`" error a missing config always did.
//
// The clock and calendar are pinned once per invocation, so every part of a
// command sees the same "now" rather than sampling it repeatedly.
func withEnv(ctx context.Context, fn func(env *cmdEnv) error) error {
	cfg, err := optionalConfig()
	if err != nil {
		return err
	}
	st, err := openStore(ctx)
	if err != nil {
		return err
	}
	defer st.Close()

	env := &cmdEnv{ctx: ctx, w: os.Stdout, st: st, now: time.Now(), loc: time.Local}
	if cfg != nil {
		env.c = api.New(cfg.APIToken)
		env.workspaceID = cfg.WorkspaceID
	}
	return fn(env)
}

// openStore ensures the state directory exists and opens the SQLite database.
func openStore(ctx context.Context) (*store.Store, error) {
	if _, err := config.EnsureDir(); err != nil {
		return nil, err
	}
	path, err := config.DBPath()
	if err != nil {
		return nil, err
	}
	return store.Open(ctx, path)
}

// optionalConfig loads the stored config, reporting "no config yet" as a nil
// config rather than as an error: that is what makes tg usable before (or
// without) `tg auth`, since only the commands that talk to Toggl need it.
func optionalConfig() (*config.Config, error) {
	cfg, err := config.Load()
	if errors.Is(err, config.ErrNotConfigured) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return cfg, nil
}

// bindFirstFlag binds the `-1` flag (long spelling `--first`) shared by every
// command that resolves a name fragment: when the fragment matches more than
// one candidate, take the FIRST one instead of failing with the candidate list.
// That is what makes two identically named tasks in different projects usable
// without exporting TOGGL_PROJECT_ID or renaming anything.
//
// The name is the digit "1", so the flag package parses a bare `-1` as this
// boolean rather than treating it as an unknown flag or a positional: boolean
// flags need no value, and `-1`, `--1`, `-first` and `--first` all set it.
// Since it is a declared flag it also survives parseArgsAndFlags, so it may be
// written before or after the fragment. subject names what the fragment
// selects ("task", "project", ...) and only shapes the usage line.
func bindFirstFlag(fs *flag.FlagSet, first *bool, subject string) {
	usage := "on an ambiguous " + subject + " fragment, use the first match instead of failing"
	fs.BoolVar(first, "1", false, usage)
	fs.BoolVar(first, "first", false, usage+" (alias of -1)")
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

// projectIDFromEnv parses TOGGL_PROJECT_ID, returning nil when it is unset (or
// empty, which is how a shell spells "unset" for an exported variable).
//
// A value that is not a project id is an error rather than a silent nil: the
// variable is what scopes `add`, `tasks`, `grep` and `update` to one project, so
// treating a typo as "unset" would quietly widen the scope — and file entries
// added under it against no project at all.
func projectIDFromEnv() (*int64, error) {
	v := strings.TrimSpace(os.Getenv("TOGGL_PROJECT_ID"))
	if v == "" {
		return nil, nil
	}
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid TOGGL_PROJECT_ID %q: want a numeric project id", v)
	}
	return &id, nil
}

// resolvePullSince determines `tg pull`'s window start. An explicit --since
// date wins; otherwise --all/-a widens the window to the first day of the
// current month and the default is the start of today. Both defaults are
// day-aligned in loc (like resolveUpdateSince) so they mean "today" and "this
// month" in calendar terms rather than a rolling cut.
//
// There is no explicit window END: togglsync.Pull asks Toggl for everything modified
// at or after `since`, and nothing can be modified after `now`, so today's
// window ends at now and the current month's window ends at the month's end as
// it is reached.
func resolvePullSince(sinceFlag string, all bool, now time.Time, loc *time.Location) (time.Time, error) {
	if sinceFlag != "" {
		return parseSinceFlag(sinceFlag, loc)
	}
	if all {
		return startOfMonth(now, loc), nil
	}
	return startOfDay(now, loc), nil
}

// updateDefaultDays is `tg update`'s default entry window: one day back.
const updateDefaultDays = 1

// updateProjectFragment folds `tg update`'s two equivalent ways of naming a
// project — the --project/-p flag and the positional arguments — into the one
// fragment resolveUpdateProject matches against the cached catalog. Positionals
// are joined with spaces so a multi-word name works unquoted, mirroring
// `tg add`/`tg grep`.
//
// Supplying both spellings is a usage error rather than a silent precedence
// rule: `tg update -p backend payments` almost certainly means the user
// mistyped, and picking one of the two projects would be a surprising way to
// resolve that. An empty result is left to resolveUpdateProject, which turns it
// into the "requires a project" error (or accepts it when TOGGL_PROJECT_ID is
// set).
func updateProjectFragment(flagValue string, positional []string) (string, error) {
	flagValue = strings.TrimSpace(flagValue)
	pos := strings.TrimSpace(strings.Join(positional, " "))
	if flagValue != "" && pos != "" {
		return "", fmt.Errorf("project given twice: --project %q and %q; pass it once", flagValue, pos)
	}
	if flagValue != "" {
		return flagValue, nil
	}
	return pos, nil
}

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
		return parseSinceFlag(sinceFlag, loc)
	}
	return now.AddDate(0, -3, 0), nil
}

// parseSinceFlag parses a --since value, the one date format tg accepts on the
// command line (YYYY-MM-DD), as midnight of that day in loc. It is shared by
// `pull` and `total` so both spell the format and the complaint identically.
func parseSinceFlag(value string, loc *time.Location) (time.Time, error) {
	t, err := time.ParseInLocation("2006-01-02", value, loc)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid --since %q (want YYYY-MM-DD)", value)
	}
	return t, nil
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
	fmt.Fprintln(w, "  add <timesign> [project] <task>  add a finished entry [--desc TEXT] [-1]")
	fmt.Fprintln(w, "  mod [num] [timesign]      retime/rename an entry (default: last) [--desc TEXT]")
	fmt.Fprintln(w, "  del <num>                 delete the entry numbered by `tg ls`")
	fmt.Fprintln(w, "  current | status          last entry, gap, day total        [--json]")
	fmt.Fprintln(w, "  today   | list | ls       show today's entries     [--days N] [--json]")
	fmt.Fprintln(w, "  daily                     this month's time per day and overtime")
	fmt.Fprintln(w, "                            vs a daily target      [-t HOURS] [--json]")
	fmt.Fprintln(w, "  tasks                     list cached tasks                 [--all] [--json]")
	fmt.Fprintln(w, "  grep <fragment>           list cached tasks matching it [--all] [--json] [-1]")
	fmt.Fprintln(w, "  projects                  list cached projects with ids     [--all] [--json]")
	fmt.Fprintln(w, "  projects update           sync all workspace projects       [--all] [--json]")
	fmt.Fprintln(w, "  update [project]          refresh a project's tasks and pull its recent")
	fmt.Fprintln(w, "                            entries [-p FRAGMENT] [--days N] [--all] [--json] [-1]")
	fmt.Fprintln(w, "  push                      send local changes to Toggl       [--json]")
	fmt.Fprintln(w, "  pull [project]            fetch today's changes; all projects, or one")
	fmt.Fprintln(w, "                            [-a|--all this month] [--since DATE] [--json] [-1]")
	fmt.Fprintln(w, "  total [task]              total tracked hours per task; last 3 months [--since DATE] [--json] [-1]")
	fmt.Fprintln(w, "  completion zsh            print the zsh completion script")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "timesign: absolute 9-:30, 10-11, 10:30-11:15 (today), relative +:20,")
	fmt.Fprintln(w, "      +1, +1:20 (that long, ending at the last 5m mark), or a bare")
	fmt.Fprintln(w, "      duration 1:30, :45 (that long, starting where the last entry")
	fmt.Fprintln(w, "      ended; `add` only). Full spec: docs/timesig.md")
	fmt.Fprintln(w, "mod:  numbers are the per-day ones shown by `tg ls` (assigned when an")
	fmt.Fprintln(w, "      entry is added, never reused); without one the last entry is")
	fmt.Fprintln(w, "      modified: today's newest already-started entry, the same one")
	fmt.Fprintln(w, "      `tg status` shows. An absolute timesign sets the range on the")
	fmt.Fprintln(w, "      entry's own day; a relative one EXTENDS the entry, keeping the")
	fmt.Fprintln(w, "      start (`tg mod +30` pushes the end 30m later); unlike other")
	fmt.Fprintln(w, "      relative timesigns, a number without `:` is minutes for `mod`.")
	fmt.Fprintln(w, "-1:   `add`/`grep`/`total`/`update`/`pull` match tasks and projects by")
	fmt.Fprintln(w, "      name fragment; a fragment matching several of them normally")
	fmt.Fprintln(w, "      fails with the candidates listed. `-1` (alias `--first`) takes")
	fmt.Fprintln(w, "      the first candidate instead, which is how two tasks sharing a")
	fmt.Fprintln(w, "      name in different projects are told apart.")
	fmt.Fprintln(w, "sync: run `tg pull` then `tg push` for correct last-writer-wins.")
	fmt.Fprintln(w, "env:  TOGGL_PROJECT_ID scopes `add`/`tasks`/`grep`/`update` to one project")
	fmt.Fprintln(w, "      (and sets the project on entries created by `add`). `pull`")
	fmt.Fprintln(w, "      ignores it and always reconciles every project; pass a")
	fmt.Fprintln(w, "      <project> name to `pull` to scope it explicitly. When unset,")
	fmt.Fprintln(w, "      `update` needs a <project> fragment (positional or -p) matching")
	fmt.Fprintln(w, "      exactly one cached project, and `add` accepts")
	fmt.Fprintln(w, "      `<timesign> <project> <task>` to scope by project name.")
}
