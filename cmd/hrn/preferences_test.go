package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/prefs"
	"github.com/cristiangirlea/harness/internal/runlog"
)

func TestBackendsShowsSetsAndResetsPreferences(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	orig, _ := agent.Get("claude")
	agent.Register(recorder{name: "claude"})
	t.Cleanup(func() { agent.Register(orig) })
	for _, args := range [][]string{{"set", "--only", "claude,codex", "--default", "claude"}, {}} {
		out, err := captureStdout(t, func() error { return cmdBackends(args) })
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(out), "\n") {
			if strings.Contains(line, "claude") && (!strings.Contains(line, "enabled") || !strings.Contains(line, "(default)")) {
				t.Errorf("claude: %s", line)
			}
			if strings.Contains(line, "gemini") && !strings.Contains(line, "disabled") {
				t.Errorf("gemini: %s", line)
			}
		}
	}
	p, err := prefs.Load(runlog.Home())
	if err != nil || p.Default() != "claude" || p.Enabled("api") {
		t.Fatalf("saved %+v, %v", p, err)
	}
	for _, args := range [][]string{{"set"}, {"set", "--only", ""}, {"set", "--only", "unknown"}, {"set", "--only", "claude", "--default", "codex"}, {"set", "--only", "claude", "extra"}, {"reset", "extra"}, {"oops"}} {
		if err := cmdBackends(args); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := captureStdout(t, func() error { return cmdBackends([]string{"reset"}) }); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(runlog.Home(), "backends.json")); !os.IsNotExist(err) {
		t.Fatalf("reset: %v", err)
	}
}

func TestRunRefusesADisabledBackendBeforeRunning(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	agent.Register(recorder{name: "prefs-disabled"})
	if err := prefs.Save(runlog.Home(), prefs.Prefs{EnabledNames: []string{"claude"}}); err != nil {
		t.Fatal(err)
	}
	code, err := cmdRun([]string{"-b", "prefs-disabled", "hello"})
	want := "run: backend prefs-disabled is disabled; enabled: claude (hrn backends set)"
	if code != 2 || err == nil || err.Error() != want {
		t.Fatalf("got %d, %v", code, err)
	}
	records, err := runlog.List(0)
	if err != nil || len(records) != 0 {
		t.Fatalf("records: %v, %v", records, err)
	}
}

func TestRunRefusesMalformedPreferences(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	if err := prefs.Save(runlog.Home(), prefs.Prefs{EnabledNames: []string{"unknown-preference"}}); err != nil {
		t.Fatal(err)
	}
	code, err := cmdRun([]string{"-b", "unknown-preference", "hello"})
	if code != 2 || err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("got %d, %v", code, err)
	}
}
