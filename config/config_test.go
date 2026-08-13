package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// These tests all set XDG_STATE_HOME (that is what points the package at a
// throwaway state directory), so none of them can run in parallel: t.Setenv
// mutates the whole process's environment.

func TestLoadNotConfigured(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if _, err := Load(); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("Load on empty dir = %v, want ErrNotConfigured", err)
	}
}

// TestLoadCorrupted pins the difference between "no config yet" and "the config
// is broken": a config.json that is not valid JSON must fail loudly rather than
// masquerade as ErrNotConfigured, which callers translate into "run `tg auth`"
// and which would therefore hide the real problem (and invite overwriting the
// file). See the main package's TestWithEnvCorruptedConfig for the command-level
// half of this.
func TestLoadCorrupted(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", base)
	if _, err := EnsureDir(); err != nil {
		t.Fatalf("ensure dir: %v", err)
	}
	path, err := Path()
	if err != nil {
		t.Fatalf("path: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"api_token": `), 0o600); err != nil {
		t.Fatalf("write truncated config: %v", err)
	}

	cfg, err := Load()
	if err == nil {
		t.Fatalf("Load on a corrupted config = %+v, want an error", cfg)
	}
	if errors.Is(err, ErrNotConfigured) {
		t.Errorf("err = %v, want a parse error rather than ErrNotConfigured", err)
	}
	if cfg != nil {
		t.Errorf("Load = %+v, want no config alongside the error", cfg)
	}

	// A config.json holding valid JSON of the wrong shape is rejected too: the
	// token would silently come out empty otherwise.
	if err := os.WriteFile(path, []byte(`["not", "an", "object"]`), 0o600); err != nil {
		t.Fatalf("write wrong-shape config: %v", err)
	}
	if cfg, err := Load(); err == nil {
		t.Errorf("Load on a JSON array = %+v, want an error", cfg)
	}
}

// TestLoadUnreadable covers the other way a present config can fail: it exists
// but cannot be read, which is not "not configured" either.
func TestLoadUnreadable(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", base)
	// A directory where config.json should be: reading it fails with something
	// other than os.ErrNotExist.
	if err := os.MkdirAll(filepath.Join(base, "tg", "config.json"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	_, err := Load()
	if err == nil {
		t.Fatal("Load with an unreadable config = nil error, want an error")
	}
	if errors.Is(err, ErrNotConfigured) {
		t.Errorf("err = %v, want a read error rather than ErrNotConfigured", err)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", base)

	c := &Config{APIToken: "secret-token", WorkspaceID: 12345}
	if err := c.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	path, err := Path()
	if err != nil {
		t.Fatalf("path: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("config.json perm = %o, want 600", perm)
	}

	got, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.APIToken != c.APIToken || got.WorkspaceID != c.WorkspaceID {
		t.Errorf("round-trip = %+v, want %+v", got, c)
	}
}

func TestDirHonorsXDG(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/custom/state")
	dir, err := Dir()
	if err != nil {
		t.Fatalf("dir: %v", err)
	}
	if dir != "/custom/state/tg" {
		t.Errorf("Dir() = %q, want /custom/state/tg", dir)
	}
}
