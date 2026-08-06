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
		"\"today:", "\"list:", "\"ls:", "'tasks:", "'grep:", "'projects:",
		"\"update:", "'push:", "\"pull:", "'total:",
		"'completion:", "'help:",
		// `projects` has a subcommand of its own.
		"__tg_projects_cmds", "'update:sync all workspace projects'",
		// Per-command argument handling for the commands with flags.
		"        add)", "        mod)", "        del)",
		"        current|status|push)", "        today|list|ls)",
		"        tasks)", "        grep)", "        projects)", "        update)",
		"        pull)", "        total)",
		"        completion)",
		"--desc[", "--description[", "--json[", "--all[", "--since[", "--days[",
		// `pull` widens its default today-only window with -a/--all.
		"--all[pull this month", "-a[pull this month",
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
		"ls", "tasks", "grep", "projects", "update", "push",
		"pull", "total", "completion", "help",
	} {
		if !strings.Contains(out, "'"+cmd+":") && !strings.Contains(out, `"`+cmd+":") {
			t.Errorf("completion script does not offer command %q", cmd)
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
