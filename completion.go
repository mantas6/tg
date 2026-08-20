package main

import (
	"errors"
	"fmt"
	"io"
)

// cmdCompletion writes the shell completion script for the requested shell.
// Only zsh is supported; anything else is a usage error.
func cmdCompletion(w io.Writer, shell string) error {
	if shell != "zsh" {
		return errors.New("usage: tg completion zsh")
	}
	fmt.Fprint(w, zshCompletion)
	return nil
}

// zshCompletion is the zsh completion script emitted by `tg completion zsh`.
// It is written to work both ways: dropped on $fpath as a #compdef file, or
// eval'd/sourced after compinit (the trailing guard calls compdef itself).
// `tg add` completion pulls task names from `tg tasks --json`, which reads
// the local SQLite cache and honours TOGGL_PROJECT_ID scoping.
//
// Every fragment-taking command (`add`, `grep`, `total`, `update`, `pull`) also
// offers -1/--first, the "use the first match" flag (see bindFirstFlag).
const zshCompletion = `#compdef tg

# Complete task names for ` + "`tg add`" + ` from the local catalog cache.
__tg_tasks() {
  local json name
  local MATCH MBEGIN MEND
  local -a match mbegin mend names
  json="$(tg tasks --json 2>/dev/null)" || return 1
  while [[ $json =~ '"name":"((\\.|[^"\\])*)"' ]]; do
    name=$match[1]
    json=${json[MEND+1,-1]}
    name=${name//'\"'/'"'}
    name=${name//'\\'/'\'}
    names+=("$name")
  done
  (( $#names )) || return 1
  local expl
  _wanted tasks expl 'task' compadd -a names
}

# Complete project names for the project fragment taken by ` + "`tg update`" + `
# and ` + "`tg pull`" + `, from the same local catalog cache.
__tg_project_names() {
  local json name
  local MATCH MBEGIN MEND
  local -a match mbegin mend names
  json="$(tg projects --json 2>/dev/null)" || return 1
  while [[ $json =~ '"name":"((\\.|[^"\\])*)"' ]]; do
    name=$match[1]
    json=${json[MEND+1,-1]}
    name=${name//'\"'/'"'}
    name=${name//'\\'/'\'}
    names+=("$name")
  done
  (( $#names )) || return 1
  local expl
  _wanted projects expl 'project' compadd -a names
}

# Complete the subcommands of ` + "`tg projects`" + `.
__tg_projects_cmds() {
  local -a subcmds
  subcmds=('update:sync all workspace projects')
  _describe -t commands 'tg projects command' subcmds
}

_tg() {
  local curcontext="$curcontext" state line
  typeset -A opt_args

  _arguments -C \
    '1:command:->cmds' \
    '*::arg:->args'

  case $state in
    cmds)
      local -a commands
      commands=(
        'auth:verify a Toggl API token and store config'
        'add:add a finished entry from a timesign (9-:30, +:20, 1:30)'
        'mod:retime (+30, -30, 9-10:30) or rename an entry (default: the last one)'
        'del:delete an entry by its number from tg ls'
        "current:show the last entry, idle gap and today's total"
        "status:show the last entry, idle gap and today's total"
        "today:show today's entries"
        "list:show today's entries"
        "ls:show today's entries"
        "daily:this month's tracked time per day and overtime"
        'tasks:list cached tasks'
        'grep:list cached tasks matching a fragment'
        'projects:list cached projects with ids (projects update syncs them)'
        "update:refresh one project's tasks and pull its recent entries"
        'push:send local changes to Toggl'
        "pull:fetch today's remote changes (-a for this month)"
        'total:total tracked hours per task, one group per fragment (Reports API)'
        'completion:print a shell completion script'
        'help:show usage'
      )
      _describe -t commands 'tg command' commands
      ;;
    args)
      case $words[1] in
        add)
          # First positional is the timesign (absolute 9-:30, relative
          # +:20, or a bare duration 1:30 continuing the last entry);
          # later args are task names. --desc/--description set the
          # entry description, -1/--first resolves an ambiguous fragment.
          _arguments \
            '--desc[entry description]:description:' \
            '--description[entry description (alias of --desc)]:description:' \
            '-1[use the first match on an ambiguous fragment]' \
            '--first[use the first match on an ambiguous fragment (alias of -1)]' \
            '1:timesign:' \
            '*:task fragment:__tg_tasks'
          ;;
        mod)
          # Positionals are an entry number from tg ls (optional, defaults to
          # the last entry) and a timesign, in either order; both are transient
          # so only the slots are described, naming the forms mod accepts:
          # absolute (9-10:30) or signed (+30 later, -30 earlier).
          # --desc/--description retitle the entry.
          _arguments \
            '--desc[new entry description]:description:' \
            '--description[new entry description (alias of --desc)]:description:' \
            '1:entry number or timesign (9-10:30, +30, -30):' \
            '2:entry number or timesign (9-10:30, +30, -30):'
          ;;
        del)
          _arguments '1:entry number from tg ls:'
          ;;
        current|status|push)
          _arguments '--json[emit JSON]'
          ;;
        today|list|ls)
          _arguments '--json[emit JSON]' '--days[number of days to look back]:days:'
          ;;
        daily)
          _arguments '--json[emit JSON]' \
            '--target[target hours worked per day (default 8)]:hours:' \
            '-t[target hours worked per day (alias of --target)]:hours:'
          ;;
        tasks)
          _arguments '--all[include inactive tasks]' '--json[emit JSON]'
          ;;
        grep)
          _arguments '--all[include inactive tasks]' '--json[emit JSON]' \
            '-1[list only the first match]' \
            '--first[list only the first match (alias of -1)]' \
            '*:task fragment:__tg_tasks'
          ;;
        projects)
          # Bare ` + "`tg projects`" + ` lists the cache; the lone subcommand
          # ` + "`tg projects update`" + ` syncs it and takes the same flags.
          if [[ $words[2] == update ]]; then
            _arguments '--all[include inactive projects]' '--json[emit JSON]'
          else
            _arguments '--all[include inactive projects]' '--json[emit JSON]' \
              '1:subcommand:__tg_projects_cmds'
          fi
          ;;
        update)
          # The project is named either positionally or with --project/-p;
          # both take the same case-insensitive name fragment.
          _arguments '--all[include inactive tasks]' '--json[emit JSON]' \
            '--days[pull entries from the last N days]:days:' \
            '-n[pull entries from the last N days (alias of --days)]:days:' \
            '--project[project name fragment to update]:project fragment:__tg_project_names' \
            '-p[project name fragment (alias of --project)]:project fragment:__tg_project_names' \
            '-1[use the first match on an ambiguous fragment]' \
            '--first[use the first match on an ambiguous fragment (alias of -1)]' \
            '*:project fragment:__tg_project_names'
          ;;
        pull)
          _arguments '--all[pull this month, not just today]' \
            '-a[pull this month, not just today (alias of --all)]' \
            '--since[pull entries modified since DATE]:date (YYYY-MM-DD):' \
            '--json[emit JSON]' \
            '-1[use the first match on an ambiguous fragment]' \
            '--first[use the first match on an ambiguous fragment (alias of -1)]' \
            '*:project fragment:__tg_project_names'
          ;;
        total)
          # Each positional is its own task fragment, so several may be given.
          _arguments '--since[total entries since DATE (default 3 months ago)]:date (YYYY-MM-DD):' \
            '--json[emit JSON]' \
            '-1[total only the first match of each fragment]' \
            '--first[total only the first match of each fragment (alias of -1)]' \
            '*:task fragment:__tg_tasks'
          ;;
        completion)
          local -a shells
          shells=('zsh:zsh completion script')
          _describe -t shells 'shell' shells
          ;;
      esac
      ;;
  esac
}

# Register directly when eval'd/sourced after compinit; as a #compdef fpath
# file zsh calls us with the completion words instead.
if [ "$funcstack[1]" = "_tg" ]; then
  _tg "$@"
else
  compdef _tg tg
fi
`
