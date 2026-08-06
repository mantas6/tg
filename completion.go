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
        'add:add a finished entry from a timesign (9-:30, +:20)'
        'mod:retime or rename an entry (default: the last one)'
        'del:delete an entry by its number from tg ls'
        "current:show the last entry, idle gap and today's total"
        "status:show the last entry, idle gap and today's total"
        "today:show today's entries"
        "list:show today's entries"
        "ls:show today's entries"
        'tasks:list cached tasks'
        'grep:list cached tasks matching a fragment'
        'projects:list cached projects with ids (projects update syncs them)'
        "update:refresh one project's tasks and pull its recent entries"
        'push:send local changes to Toggl'
        "pull:fetch today's remote changes (-a for this month)"
        'total:total tracked hours per task (Reports API)'
        'completion:print a shell completion script'
        'help:show usage'
      )
      _describe -t commands 'tg command' commands
      ;;
    args)
      case $words[1] in
        add)
          # First positional is the timesign (absolute 9-:30 or relative
          # +:20); later args are task names. --desc/--description set the
          # entry description.
          _arguments \
            '--desc[entry description]:description:' \
            '--description[entry description (alias of --desc)]:description:' \
            '1:timesign:' \
            '*:task fragment:__tg_tasks'
          ;;
        mod)
          # Positionals are an entry number from tg ls (optional, defaults to
          # the last entry) and a timesign, in either order; both are transient
          # so only the slots are described. --desc/--description retitle it.
          _arguments \
            '--desc[new entry description]:description:' \
            '--description[new entry description (alias of --desc)]:description:' \
            '1:entry number or timesign:' \
            '2:entry number or timesign:'
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
        tasks)
          _arguments '--all[include inactive tasks]' '--json[emit JSON]'
          ;;
        grep)
          _arguments '--all[include inactive tasks]' '--json[emit JSON]' '*:task fragment:__tg_tasks'
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
          _arguments '--all[include inactive tasks]' '--json[emit JSON]' \
            '--days[pull entries from the last N days]:days:' \
            '-n[pull entries from the last N days (alias of --days)]:days:' \
            '*:project fragment:'
          ;;
        pull)
          _arguments '--all[pull this month, not just today]' \
            '-a[pull this month, not just today (alias of --all)]' \
            '--since[pull entries modified since DATE]:date (YYYY-MM-DD):' \
            '--json[emit JSON]' '*:project fragment:'
          ;;
        total)
          _arguments '--since[total entries since DATE (default 3 months ago)]:date (YYYY-MM-DD):' '--json[emit JSON]' '*:task fragment:__tg_tasks'
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
