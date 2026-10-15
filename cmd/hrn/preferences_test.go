package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestBackendsPrintsTheQuotaUnderEachUsableBackendWhenKnown(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	for _, name := range []string{"claude", "codex"} {
		orig, _ := agent.Get(name)
		agent.Register(recorder{name: name})
		t.Cleanup(func() { agent.Register(orig) })
	}
	now := time.Now()
	day := filepath.Join(codexHome, "sessions", now.Format("2006"), now.Format("01"), now.Format("02"))
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","rate_limits":{"limit_id":"codex","primary":{"used_percent":40,"window_minutes":10080,"resets_at":%d},"secondary":null}}}`+"\n",
		now.UTC().Format(time.RFC3339Nano), now.Add(76*time.Hour+30*time.Minute).Unix())
	if err := os.WriteFile(filepath.Join(day, "rollout-cli.jsonl"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := captureStdout(t, func() error { return cmdBackends(nil) })
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(out), "\n")
	for i, l := range lines {
		if strings.Contains(l, "✓ codex") {
			if i+1 >= len(lines) || strings.TrimSpace(lines[i+1]) != "week: 60% left, resets in 3d 4h" {
				t.Fatalf("after codex: %q", lines[i+1:])
			}
		}
	}
	if strings.Count(string(out), "left") != 1 {
		t.Fatalf("only codex's quota is known: %s", out)
	}
}
