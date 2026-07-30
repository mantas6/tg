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
		"auth:", "add:", "current:", "status:",
		"today:", "list:", "ls:", "tasks:", "projects:",
		"update:", "push:", "pull:", "completion:", "help:",
	} {
		if !strings.Contains(out, marker) {
			t.Errorf("completion script missing %q", marker)
		}
	}
	// start/stop are gone: the script must not offer them any more.
	for _, gone := range []string{"'start:", "'stop:", "        start)"} {
		if strings.Contains(out, gone) {
			t.Errorf("completion script still offers %q", gone)
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
