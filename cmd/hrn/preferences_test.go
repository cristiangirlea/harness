package main

import (
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/prefs"
	"github.com/cristiangirlea/harness/internal/runlog"
)

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
