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
	"github.com/mantas6/tg/timesig"
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
	f := bindAddFlags(fs)
	// Flags may follow the timesign and the fragment (`tg add 9-10 login -1`),
	// so positionals are peeled off the same way `tg mod` does it. None of the
	// forms `add` accepts starts with "-" (an absolute one needs a digit
	// first), so nothing positional is mistaken for a flag — and unlike
	// `tg mod -30`, `tg add ... -1` stays the --first flag.
	rest, err := parseArgsAndFlags(fs, args)
	if err != nil {
		return err
	}
	// First positional arg is the timesign. After it, two fragments mean
	// `<project> <task>` (the first scopes to a project, overriding
	// TOGGL_PROJECT_ID); one means `<task>` scoped by env.
	if len(rest) < 2 {
		return errors.New(addUsage)
	}
	timesign := rest[0]
	rest = rest[1:]
	projectID, err := projectIDFromEnv()
	if err != nil {
		return err
	}
	// The project-name form is resolved against the cached catalog, so it needs
	// the store and happens inside withEnv. --date is resolved there too: it is
	// judged against the invocation's pinned clock, like `pull`'s and `total`'s
	// --since (see resolveDateFlag).
	return withEnv(ctx, func(env *cmdEnv) error {
		day, err := resolveDateFlag(f.date, env.now, env.loc)
		if err != nil {
			return err
		}
		pid, fragment := projectID, strings.Join(rest, " ")
		if len(rest) == 2 {
			resolved, err := resolveAddProject(env.ctx, env.st, rest[0], f.first)
			if err != nil {
				return err
			}
			pid, fragment = resolved, rest[1]
		}
		return cmdAdd(env.on(day), pid, f.first, timesign, fragment, f.desc)
	})
}

func runMod(ctx context.Context, args []string) error {
	fs := newFlagSet("mod")
	// Registered before the pre-pass below, which has to know which of these
	// flags consume the argument after them (see splitNegativeTimesigns): that
	// is what keeps `tg mod --date 2026-08-25 -30` reading the date as the
	// flag's value and the "-30" as the timesign.
	f := bindModFlags(fs)
	// A negative timesign ("-30") is a positional argument that looks exactly
	// like an undefined flag, so it is peeled off before the flag package sees
	// it and put back with the other positionals afterwards. parseModArgs takes
	// them in any order, so appending is enough.
	args, negative := splitNegativeTimesigns(fs, args)
	rest, err := parseArgsAndFlags(fs, args)
	if err != nil {
		return err
	}
	setDesc := descWasSet(fs)

	ref, timesign, err := parseModArgs(append(rest, negative...))
	if err != nil {
		return err
	}
	return withEnv(ctx, func(env *cmdEnv) error {
		day, err := resolveDateFlag(f.date, env.now, env.loc)
		if err != nil {
			return err
		}
		return cmdMod(env.on(day), ref, timesign, f.desc, setDesc)
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
	// All positionals form ONE fragment, exactly like `tg add` (unlike
	// `tg total`, whose positionals are one fragment each).
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
	f := bindUpdateFlags(fs)
	// Flags may follow the project fragment (`tg update backend -n 3`), so
	// positionals are peeled off the same way `tg grep` does it.
	rest, err := parseArgsAndFlags(fs, args)
	if err != nil {
		return err
	}
	fragment, err := f.resolveFragment(rest)
	if err != nil {
		return err
	}
	projectID, err := projectIDFromEnv()
	if err != nil {
		return err
	}
	return withEnv(ctx, func(env *cmdEnv) error {
		since := resolveUpdateSince(f.days, env.now, env.loc)
		return cmdUpdate(env, projectID, f.first, fragment, since, f.all, f.jsonOut)
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
	f := bindPullFlags(fs)
	// Flags may follow the project fragment (`tg pull backend -a`), so
	// positionals are peeled off the same way `tg update` does it.
	rest, err := parseArgsAndFlags(fs, args)
	if err != nil {
		return err
	}
	fragment := strings.Join(rest, " ")
	return withEnv(ctx, func(env *cmdEnv) error {
		since, err := resolvePullSince(f.since, f.all, env.now, env.loc)
		if err != nil {
			return err
		}
		// pull deliberately ignores TOGGL_PROJECT_ID (unlike add/tasks/update):
		// it always reconciles every project. Scoping happens only via an
		// explicit <project> argument, so the env project id is never passed
		// through here.
		return cmdPull(env, f.first, fragment, since, f.jsonOut)
	})
}

func runTotal(ctx context.Context, args []string) error {
	fs := newFlagSet("total")
	f := bindTotalFlags(fs)
	// Flags may follow the fragments (`tg total login --json`), so positionals
	// are peeled off the same way `tg grep` does it.
	rest, err := parseArgsAndFlags(fs, args)
	if err != nil {
		return err
	}
	// Each positional is its OWN task-name fragment (`tg total login docs`
	// reports both, one group each), unlike `tg add`, which joins them: a
	// multi-word name is therefore a single quoted argument,
	// `tg total "code review"`.
	// The store is opened even though the totals come from the Reports API: it
	// is what resolves the reported task ids to names and what fragments are
	// matched against (see cmdTotal).
	return withEnv(ctx, func(env *cmdEnv) error {
		since, err := resolveTotalSince(f.since, env.now, env.loc)
		if err != nil {
			return err
		}
		return cmdTotal(env, f.first, rest, since, f.jsonOut)
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
	return withEnvOut(ctx, os.Stdout, fn)
}

// withEnvOut is withEnv with the command's output stream given explicitly. Every
// command writes to stdout, so withEnv fills that in; the parameter exists so
// the prologue itself (config loading, store opening, the assembled cmdEnv) can
// be exercised without a command scribbling on the test's own stdout.
func withEnvOut(ctx context.Context, w io.Writer, fn func(env *cmdEnv) error) error {
	cfg, err := optionalConfig()
	if err != nil {
		return err
	}
	st, err := openStore(ctx)
	if err != nil {
		return err
	}
	defer st.Close()

	// The day a command works on starts out as now's; only `add`/`mod` move it,
	// and only when --date says so (see cmdEnv.day and cmdEnv.on).
	now := time.Now()
	env := &cmdEnv{ctx: ctx, w: w, st: st, now: now, day: now, loc: time.Local}
	if cfg != nil {
		env.c = api.New(cfg.APIToken)
		env.workspaceID = cfg.WorkspaceID
	}
	return fn(env)
}

// openStore ensures the state directory exists and opens the SQLite database.
func openStore(ctx context.Context) (*store.Store, error) {
	if _, err := config.EnsureDir(); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	path, err := config.DBPath()
	if err != nil {
		return nil, fmt.Errorf("locate state directory: %w", err)
	}
	// store.Open already names the database file in its own errors, so it is
	// returned unwrapped rather than prefixed twice.
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
		return nil, fmt.Errorf("load config: %w", err)
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

// bindDescFlag binds --desc and its --description alias to the same variable, so
// either spelling sets an entry's description. `add` and `mod` share it (and so
// share the spelling), which is also what lets descWasSet recognize both names.
func bindDescFlag(fs *flag.FlagSet, desc *string, usage string) {
	fs.StringVar(desc, "desc", "", usage)
	fs.StringVar(desc, "description", "", usage+" (alias of --desc)")
}

// bindDateFlag binds --date, the day `add` and `mod` work on instead of today.
// Both take it (and so spell it identically), since an entry booked for another
// day has to be editable on that day too.
//
// The value is a calendar date in dateLayout (YYYY-MM-DD), the one date format
// tg's interface uses; an empty value means "today". Only today or a later day
// is accepted — see resolveDateFlag, which turns the value into the day and is
// where a past date is refused.
func bindDateFlag(fs *flag.FlagSet, date *string) {
	fs.StringVar(date, "date", "",
		"the day the entry belongs to (YYYY-MM-DD); today or later, default today")
}

// resolveDateFlag turns `add`'s and `mod`'s --date value into the instant the
// command reckons its calendar day by (see cmdEnv.day):
//
//   - absent, or naming now's own day: now itself, so the ordinary case is
//     bit-for-bit the behavior tg had before the flag existed;
//   - naming a later day: the LAST second of that day, so every entry already
//     booked on it counts as "already started" — which is what lets `tg add`
//     continue from it with a bare duration and `tg mod` address it at all
//     (see store.LastEntry and store.EntryByNum).
//
// A PAST day is refused rather than accepted: tg only ever writes the day it is
// tracking (store.CheckEditableDay enforces the same rule on every edit), so
// letting --date name yesterday would only produce a refusal one layer down —
// or, for `add`, an entry no `tg mod` could ever touch again.
//
// The comparison is by calendar day in loc, not by instant: at 23:30 local,
// tomorrow's date is still the future even though it is already "today"
// somewhere else, and the day tg would file the entry under is the local one.
func resolveDateFlag(value string, now time.Time, loc *time.Location) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return now, nil
	}
	parsed, err := time.ParseInLocation(dateLayout, value, loc)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid --date %q (want YYYY-MM-DD): %w", value, err)
	}
	// Both sides are day-aligned in loc, so this compares calendar days even
	// though the operands are instants.
	day, today := startOfDay(parsed, loc), startOfDay(now, loc)
	if day.Before(today) {
		return time.Time{}, fmt.Errorf(
			"--date %s is in the past (today is %s): only today or a later day can be given, since tg never rewrites a day that is over",
			day.Format(dateLayout), today.Format(dateLayout))
	}
	if day.Equal(today) {
		return now, nil
	}
	return endOfDay(day, loc), nil
}

// addFlags holds `tg add`'s flag values; see updateFlags for why registration is
// a function of its own. The timesign and the task fragment are positional and
// stay out of here.
type addFlags struct {
	// desc is the entry's description (--desc/--description), empty when
	// absent, which leaves the description blank.
	desc string
	// first takes the first candidate of an ambiguous fragment (-1/--first).
	first bool
	// date is the day the entry belongs to (--date DATE), empty for today;
	// see resolveDateFlag.
	date string
}

func bindAddFlags(fs *flag.FlagSet) *addFlags {
	f := &addFlags{}
	// --desc and --description are aliases bound to the same variable, so
	// either spelling sets the entry's description (empty leaves it blank).
	bindDescFlag(fs, &f.desc, "entry description")
	bindFirstFlag(fs, &f.first, "task or project")
	bindDateFlag(fs, &f.date)
	return f
}

// modFlags holds `tg mod`'s flag values; see updateFlags for why registration is
// a function of its own. The entry number and the timesign are positional (in
// either order, see parseModArgs) and stay out of here.
//
// There is deliberately no -1/--first flag: `tg mod -1` is a negative timesign
// taking one minute off the entry (see splitNegativeTimesigns).
type modFlags struct {
	// desc is the entry's new description (--desc/--description). An
	// explicitly empty value CLEARS it, so whether the flag appeared at all is
	// a separate question (see descWasSet).
	desc string
	// date is the day whose entry is edited (--date DATE), empty for today;
	// see resolveDateFlag.
	date string
}

func bindModFlags(fs *flag.FlagSet) *modFlags {
	f := &modFlags{}
	// Same --desc/--description alias pair as `add`; here an explicitly empty
	// value clears the description, which is why the flag's presence is tracked
	// separately (see descWasSet).
	bindDescFlag(fs, &f.desc, "new entry description")
	bindDateFlag(fs, &f.date)
	return f
}

// descWasSet reports whether either spelling of the description flag actually
// appeared on the command line. `tg mod` needs the distinction because an
// explicitly empty --desc CLEARS the description, which an unset flag (whose
// value is empty too) must not do.
func descWasSet(fs *flag.FlagSet) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "desc" || f.Name == "description" {
			set = true
		}
	})
	return set
}

// updateFlags holds `tg update`'s flag values. Registration lives in
// bindUpdateFlags rather than inline in runUpdate so the flag names, aliases and
// defaults are one testable thing: a test that re-declared them would keep
// passing after a rename here, which is exactly what it must not do.
type updateFlags struct {
	all     bool
	jsonOut bool
	// days is how many days back the time-entry pull reaches (--days/-n),
	// defaulting to one day; see resolveUpdateSince.
	days int
	// project names the project by fragment (--project/-p), the flag form of
	// the positional argument; see resolveFragment.
	project string
	first   bool
}

func bindUpdateFlags(fs *flag.FlagSet) *updateFlags {
	f := &updateFlags{}
	fs.BoolVar(&f.all, "all", false, "include inactive tasks")
	fs.BoolVar(&f.jsonOut, "json", false, "emit JSON")
	// --days and -n are aliases bound to the same variable, so either spelling
	// sets the window and both share the default.
	fs.IntVar(&f.days, "days", updateDefaultDays, "pull entries from the last N days")
	fs.IntVar(&f.days, "n", updateDefaultDays, "pull entries from the last N days (alias of --days)")
	// --project and -p name the project by fragment, exactly like the
	// positional form (`tg update backend` == `tg update -p backend`).
	fs.StringVar(&f.project, "project", "", "project name fragment to update")
	fs.StringVar(&f.project, "p", "", "project name fragment to update (alias of --project)")
	bindFirstFlag(fs, &f.first, "project")
	return f
}

// resolveFragment folds the flag and positional spellings of `tg update`'s
// project into the one fragment resolveUpdateProject matches against the cached
// catalog (see updateProjectFragment for the precedence rules).
func (f *updateFlags) resolveFragment(positional []string) (string, error) {
	return updateProjectFragment(f.project, positional)
}

// pullFlags holds `tg pull`'s flag values; see updateFlags for why registration
// is a function of its own.
type pullFlags struct {
	jsonOut bool
	// since is the explicit window start (--since DATE), empty when absent;
	// see resolvePullSince.
	since string
	// all widens the default today-only window to the whole current month
	// (--all/-a).
	all   bool
	first bool
}

func bindPullFlags(fs *flag.FlagSet) *pullFlags {
	f := &pullFlags{}
	fs.BoolVar(&f.jsonOut, "json", false, "emit JSON")
	fs.StringVar(&f.since, "since", "", "pull entries modified since DATE (YYYY-MM-DD)")
	// --all and -a are aliases bound to the same variable.
	fs.BoolVar(&f.all, "all", false, "pull this month's entries instead of only today's")
	fs.BoolVar(&f.all, "a", false, "pull this month's entries (alias of --all)")
	bindFirstFlag(fs, &f.first, "project")
	return f
}

// totalFlags holds `tg total`'s flag values; see updateFlags for why
// registration is a function of its own. The task fragments are positional and
// stay out of here: unlike `update`'s project there is no flag spelling for
// them, and there may be several (see cmdTotal).
type totalFlags struct {
	jsonOut bool
	// since is the explicit window start (--since DATE), empty when absent;
	// see resolveTotalSince.
	since string
	first bool
}

func bindTotalFlags(fs *flag.FlagSet) *totalFlags {
	f := &totalFlags{}
	fs.BoolVar(&f.jsonOut, "json", false, "emit JSON")
	fs.StringVar(&f.since, "since", "", "total entries since DATE (YYYY-MM-DD); default 3 months ago")
	bindFirstFlag(fs, &f.first, "task")
	return f
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

// splitNegativeTimesigns separates `tg mod`'s negative timesigns ("-30",
// "-1:20") from the rest of its command line. They are positional arguments that
// begin with "-", so flag.Parse would report them as undefined flags; pulling
// them out first is what lets `tg mod -30` be written the obvious way instead of
// needing `tg mod -- -30`.
//
// Only genuine timesign shapes are taken (see timesig.IsNegative), so real flags
// pass through untouched, and the argument after a flag that consumes one is
// skipped so a description can still be the text "-30" (`tg mod --desc -30`).
// Anything else that starts with "-" is left to the flag package, which reports
// it as the typo it almost certainly is.
func splitNegativeTimesigns(fs *flag.FlagSet, args []string) (rest, negative []string) {
	for i := 0; i < len(args); i++ {
		if timesig.IsNegative(args[i]) {
			negative = append(negative, args[i])
			continue
		}
		rest = append(rest, args[i])
		if flagWantsValue(fs, args[i]) && i+1 < len(args) {
			i++
			rest = append(rest, args[i])
		}
	}
	return rest, negative
}

// flagWantsValue reports whether arg is a flag declared on fs that takes the
// NEXT argument as its value. The joined "--flag=value" form carries its own, and
// a boolean flag never takes one (the flag package spells that with the
// unexported IsBoolFlag interface, which is why it is asserted here rather than
// looked up).
func flagWantsValue(fs *flag.FlagSet, arg string) bool {
	if !strings.HasPrefix(arg, "-") {
		return false
	}
	name := strings.TrimPrefix(strings.TrimPrefix(arg, "-"), "-")
	if name == "" || strings.ContainsRune(name, '=') {
		return false
	}
	f := fs.Lookup(name)
	if f == nil {
		return false
	}
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return !ok || !b.IsBoolFlag()
}

// parseModArgs classifies `tg mod`'s positional arguments. An all-digits
// argument is a local entry number (0 when absent, meaning the last entry);
// anything else is the timesign, since no timesign form is bare digits (an
// absolute one needs an inner `-`, a relative one a leading `+` and a negative
// one a leading `-`). Order does not matter, and neither may be given twice.
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
// command line (dateLayout, i.e. YYYY-MM-DD), as midnight of that day in loc. It
// is shared by `pull` and `total` so both spell the format and the complaint
// identically.
//
// The parse error is wrapped rather than dropped: the message keeps saying what
// tg wants, and what time.ParseInLocation objected to (which component was out
// of range, say) stays reachable for a caller that inspects the error.
func parseSinceFlag(value string, loc *time.Location) (time.Time, error) {
	t, err := time.ParseInLocation(dateLayout, value, loc)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid --since %q (want YYYY-MM-DD): %w", value, err)
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
	fmt.Fprintln(w, "  add <timesign> [project] <task>  add a finished entry")
	fmt.Fprintln(w, "                            [--desc TEXT] [--date DATE] [-1]")
	fmt.Fprintln(w, "  mod [num] [timesign]      retime/rename an entry (default: last), e.g.")
	fmt.Fprintln(w, "                            +30 / -30 minutes  [--desc TEXT] [--date DATE]")
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
	fmt.Fprintln(w, "  total [task...]           total tracked hours per task, one group per named")
	fmt.Fprintln(w, "                            task; last 3 months [--since DATE] [--json] [-1]")
	fmt.Fprintln(w, "  completion zsh            print the zsh completion script")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "timesign: absolute 9-:30, 10-11, 10:30-11:15 (today), relative +:20,")
	fmt.Fprintln(w, "      +1, +1:20 (that long, ending at the last 5m mark), negative")
	fmt.Fprintln(w, "      -:20, -1:20 (that much LESS; `mod` only), or a bare duration")
	fmt.Fprintln(w, "      1:30, :45 (that long, starting where the last entry ended;")
	fmt.Fprintln(w, "      `add` only). Full spec: docs/timesig.md")
	fmt.Fprintln(w, "mod:  numbers are the per-day ones shown by `tg ls` (assigned when an")
	fmt.Fprintln(w, "      entry is added, never reused); without one the last entry is")
	fmt.Fprintln(w, "      modified: today's newest already-started entry, the same one")
	fmt.Fprintln(w, "      `tg status` shows. An absolute timesign sets the range on the")
	fmt.Fprintln(w, "      entry's own day; a signed one moves its END, keeping the start:")
	fmt.Fprintln(w, "      `tg mod +30` pushes the end 30m later, `tg mod -30` pulls it")
	fmt.Fprintln(w, "      30m back (never past the start; use `tg del` to remove an")
	fmt.Fprintln(w, "      entry). Unlike other timesigns, a number without `:` is")
	fmt.Fprintln(w, "      minutes for `mod`.")
	fmt.Fprintln(w, "date: `add` and `mod` work on today; `--date YYYY-MM-DD` moves them to")
	fmt.Fprintln(w, "      another day, which must be today or later (a day that is over")
	fmt.Fprintln(w, "      is history and is refused). On a moved day the entry numbers,")
	fmt.Fprintln(w, "      the \"last entry\" and an absolute timesign are all that day's;")
	fmt.Fprintln(w, "      a relative timesign has no `now` there, so `add` refuses one")
	fmt.Fprintln(w, "      (`mod` still takes +30/-30, which only move an end).")
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
