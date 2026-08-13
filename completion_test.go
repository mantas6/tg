package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestCompletionZsh(t *testing.T) {
	var buf bytes.Buffer
	if err := cmdCompletion(&buf, "zsh"); err != nil {
		t.Fatalf("completion: %v", err)
	}
	out := buf.String()
	for _, marker := range []string{
		"#compdef tg",
		"_tg()",
		"__tg_tasks",
		"compdef _tg tg",
		"tg tasks --json",
		// Every command and alias accepted by run() must be offered.
		"'auth:", "'add:", "'mod:", "'del:", "\"current:", "\"status:",
		"\"today:", "\"list:", "\"ls:", "\"daily:", "'tasks:", "'grep:", "'projects:",
		"\"update:", "'push:", "\"pull:", "'total:",
		"'completion:", "'help:",
		// `projects` has a subcommand of its own.
		"__tg_projects_cmds", "'update:sync all workspace projects'",
		// Per-command argument handling for the commands with flags.
		"        add)", "        mod)", "        del)",
		"        current|status|push)", "        today|list|ls)", "        daily)",
		"        tasks)", "        grep)", "        projects)", "        update)",
		"        pull)", "        total)",
		"        completion)",
		"--desc[", "--description[", "--json[", "--all[", "--since[", "--days[",
		// `pull` widens its default today-only window with -a/--all.
		"--all[pull this month", "-a[pull this month",
		// `daily` measures each day against -t/--target.
		"--target[target hours", "-t[target hours",
		// Every fragment-taking command offers -1/--first.
		"-1[", "--first[",
	} {
		if !strings.Contains(out, marker) {
			t.Errorf("completion script missing %q", marker)
		}
	}
	// start/stop and the old update-projects spelling are gone: the script must
	// not offer them any more.
	for _, gone := range []string{
		"'start:", "'stop:", "        start)", "        stop)",
		"update-projects",
	} {
		if strings.Contains(out, gone) {
			t.Errorf("completion script still offers %q", gone)
		}
	}
}

// TestCompletionCoversDispatch pins the completion script to run()'s dispatch
// switch: every command word there must appear in the script's command list.
func TestCompletionCoversDispatch(t *testing.T) {
	var buf bytes.Buffer
	if err := cmdCompletion(&buf, "zsh"); err != nil {
		t.Fatalf("completion: %v", err)
	}
	out := buf.String()
	for _, cmd := range []string{
		"auth", "add", "mod", "del", "current", "status", "today", "list",
		"ls", "daily", "tasks", "grep", "projects", "update", "push",
		"pull", "total", "completion", "help",
	} {
		if !strings.Contains(out, "'"+cmd+":") && !strings.Contains(out, `"`+cmd+":") {
			t.Errorf("completion script does not offer command %q", cmd)
		}
	}
}

// TestCompletionOffersFirstFlag pins -1/--first to the commands that resolve a
// name fragment (see bindFirstFlag): each of their `_arguments` blocks must
// offer both spellings, and the commands without a fragment must not.
func TestCompletionOffersFirstFlag(t *testing.T) {
	var buf bytes.Buffer
	if err := cmdCompletion(&buf, "zsh"); err != nil {
		t.Fatalf("completion: %v", err)
	}
	// The per-command blocks are the "        <words>)" labels of the case
	// statement, so splitting on them isolates each command's flag list.
	blocks := map[string]string{}
	var label string
	for _, line := range strings.Split(buf.String(), "\n") {
		if trimmed := strings.TrimPrefix(line, "        "); trimmed != line &&
			strings.HasSuffix(trimmed, ")") && !strings.Contains(trimmed, " ") {
			label = strings.TrimSuffix(trimmed, ")")
			continue
		}
		if label != "" {
			blocks[label] += line + "\n"
		}
	}
	for _, cmd := range []string{"add", "grep", "update", "pull", "total"} {
		block, ok := blocks[cmd]
		if !ok {
			t.Fatalf("completion script has no %q block", cmd)
		}
		for _, want := range []string{"'-1[", "'--first["} {
			if !strings.Contains(block, want) {
				t.Errorf("%s block missing %q:\n%s", cmd, want, block)
			}
		}
	}
	for _, cmd := range []string{"mod", "del", "tasks", "daily"} {
		if strings.Contains(blocks[cmd], "'-1[") {
			t.Errorf("%s takes no fragment, it should not offer -1:\n%s", cmd, blocks[cmd])
		}
	}
}

func TestCompletionUnsupportedShell(t *testing.T) {
	for _, shell := range []string{"", "bash", "fish"} {
		var buf bytes.Buffer
		if err := cmdCompletion(&buf, shell); err == nil {
			t.Errorf("completion %q: expected error", shell)
		}
	}
}
