package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/limits"
	"github.com/cristiangirlea/harness/internal/prefs"
	"github.com/cristiangirlea/harness/internal/prefs/prefstest"
	"github.com/cristiangirlea/harness/internal/purpose"
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

func TestBackendsSetsListsAndClearsBillingPolicy(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	off := false
	if err := prefs.Save(runlog.Home(), prefs.Prefs{WorkMapGitHub: &off}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args    []string
		billing string
		ceiling float64
		line    string
	}{
		{[]string{"set", "--only", "claude,api", "--billing", "metered", "--max-cost", "2.5"}, "metered", 2.5, "billing: metered, api runs capped at $2.50"},
		{[]string{"set", "--only", "claude,api"}, "", 0, ""},
		{[]string{"set", "--only", "claude,api", "--billing", "metered"}, "metered", 0, "billing: metered, no api ceiling (api is not an automatic partner)"},
		{[]string{"set", "--only", "claude,api", "--billing", "plan"}, "plan", 0, ""},
		{[]string{"set", "--only", "claude,api", "--billing", "metered", "--max-cost", "2.5"}, "metered", 2.5, "billing: metered, api runs capped at $2.50"},
		{[]string{"reset"}, "", 0, ""},
	} {
		out, err := captureStdout(t, func() error { return cmdBackends(tc.args) })
		if err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		p, err := prefs.Load(runlog.Home())
		if err != nil || p.Billing != tc.billing || p.BillingMaxCostUSD != tc.ceiling || p.WorkMapGitHub == nil || *p.WorkMapGitHub {
			t.Fatalf("%v: preferences %+v, %v", tc.args, p, err)
		}
		first, _, _ := strings.Cut(string(out), "\n")
		if tc.line != "" && first != tc.line || tc.line == "" && strings.Contains(string(out), "billing:") {
			t.Errorf("%v: listing %q", tc.args, out)
		}
	}
	for _, args := range [][]string{
		{"set", "--only", "claude,api", "--max-cost", "2.5"},
		{"set", "--only", "claude,api", "--billing", "plan", "--max-cost", "0"},
	} {
		if err := cmdBackends(args); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
}

func TestBackendsRejectsZeroBillingCeilingWithoutWriting(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	_, err := captureStdout(t, func() error {
		return cmdBackends([]string{"set", "--only", "claude", "--billing", "metered", "--max-cost", "0"})
	})
	if err == nil || err.Error() != "backends set: --max-cost wants a positive amount" {
		t.Errorf("got %v", err)
	}
	if _, err := prefs.Raw(runlog.Home()); !os.IsNotExist(err) {
		t.Errorf("preferences should not have been written: %v", err)
	}
}

func TestRunRefusesADisabledBackendBeforeRunning(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	orig, _ := agent.Get("codex")
	agent.Register(recorder{name: "codex"})
	t.Cleanup(func() { agent.Register(orig) })
	if err := prefs.Save(runlog.Home(), prefs.Prefs{EnabledNames: []string{"claude"}}); err != nil {
		t.Fatal(err)
	}
	code, err := cmdRun([]string{"-b", "codex", "hello"})
	want := "run: backend codex is disabled; enabled: claude (hrn backends set)"
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

func TestContinuingOnADisabledBackendIsRefusedBeforeReadingStdin(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	orig, _ := agent.Get("codex")
	agent.Register(recorder{name: "codex"})
	t.Cleanup(func() { agent.Register(orig) })
	w, err := runlog.Create("codex", agent.Task{Prompt: "first", Dir: t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.Dir(), "messages.json"), []byte(`{"model":"stub"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(agent.Result{Backend: "codex", Status: agent.StatusOK}); err != nil {
		t.Fatal(err)
	}
	if err := prefs.Save(runlog.Home(), prefs.Prefs{EnabledNames: []string{"claude"}}); err != nil {
		t.Fatal(err)
	}
	stdin, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	if _, err := stdin.WriteString("the prompt"); err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = stdin
	defer func() { os.Stdin = old }()
	code, err := cmdRun([]string{"--continue", w.ID()})
	if code != 2 || err == nil || !strings.Contains(err.Error(), "backend codex is disabled") {
		t.Fatalf("got %d, %v", code, err)
	}
	if offset, _ := stdin.Seek(0, 1); offset != 0 {
		t.Fatalf("read %d bytes of the prompt before refusing", offset)
	}
}

func TestRouteSetWarnsAboutADisabledBackend(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled []string
		warned  bool
	}{
		{"disabled", []string{"claude"}, true},
		{"enabled", []string{"codex"}, false},
		{"no preference", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HRN_HOME", t.TempDir())
			if tc.enabled != nil {
				if err := prefs.Save(runlog.Home(), prefs.Prefs{EnabledNames: tc.enabled}); err != nil {
					t.Fatal(err)
				}
			}
			f, err := os.CreateTemp(t.TempDir(), "stderr")
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			old := os.Stderr
			os.Stderr = f
			_, code, err := routeOutput(t, "set", "review", "-b", "codex")
			os.Stderr = old
			if code != 0 || err != nil {
				t.Fatalf("got %d, %v", code, err)
			}
			data, err := os.ReadFile(f.Name())
			if err != nil {
				t.Fatal(err)
			}
			want := "hrn: warning: backend codex is disabled; runs ignore this row until it is enabled (hrn backends set)"
			if got := strings.Contains(string(data), want); got != tc.warned {
				t.Fatalf("stderr: %q", data)
			}
		})
	}
}

func TestBackendsRecoversBrokenPreferences(t *testing.T) {
	for _, action := range []string{"reset", "set"} {
		t.Run(action, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HRN_HOME", home)
			if err := os.WriteFile(filepath.Join(home, "backends.json"), []byte("{"), 0600); err != nil {
				t.Fatal(err)
			}
			args := []string{action}
			if action == "set" {
				args = append(args, "--only", "codex")
			}
			if _, err := captureStdout(t, func() error { return cmdBackends(args) }); err != nil {
				t.Fatal(err)
			}
			p, err := prefs.Load(home)
			if err != nil || (action == "set" && (len(p.EnabledNames) != 1 || p.EnabledNames[0] != "codex")) {
				t.Fatalf("%+v %v", p, err)
			}
		})
	}
}

func TestBackendsResetKeepsTheWorkMapGitHubSetting(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	off := false
	if err := prefs.Save(runlog.Home(), prefs.Prefs{WorkMapGitHub: &off, WorkMapSessions: &off, EnabledNames: []string{"claude"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := captureStdout(t, func() error { return cmdBackends([]string{"reset"}) }); err != nil {
		t.Fatal(err)
	}
	p, err := prefs.Load(runlog.Home())
	if err != nil || p.WorkMapGitHub == nil || *p.WorkMapGitHub || p.WorkMapSessions == nil || *p.WorkMapSessions || len(p.EnabledNames) != 0 {
		t.Fatalf("reset: %+v %v", p, err)
	}
}

func TestBackendsResetKeepsOnlyPluginPolicyOrRemovesTheFile(t *testing.T) {
	for _, tc := range []struct{ name, plugins string }{
		{"with plugin policy", `"plugins":{"install":"off","sources":{"echo":"owner/hrn-echo"},"allow":["owner/*"]}`},
		{"backend fields only", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HRN_HOME", home)
			prefstest.OrgPath(t)
			path := filepath.Join(home, "backends.json")
			input := `"enabled":["claude"],"default":"claude","billing":"plan"`
			if tc.plugins != "" {
				input += "," + tc.plugins
			}
			prefstest.Write(t, path, "{"+input+"}")
			if _, err := captureStdout(t, func() error { return cmdBackends([]string{"reset"}) }); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if tc.plugins == "" {
				if !os.IsNotExist(err) {
					t.Fatalf("reset should remove %s: read error = %v", path, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if want := "{" + tc.plugins + "}"; !jsonEqual(data, want) {
				t.Fatalf("reset = %s, want %s", data, want)
			}
		})
	}
}

func TestBackendsSetsShowsAndResetsTheLocalAPI(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	off := false
	if err := prefs.Save(runlog.Home(), prefs.Prefs{WorkMapGitHub: &off}); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"set", "--only", "claude,codex", "--local-api", "http://127.0.0.1:8081"},
		{"set", "--only", "claude,api", "--local-api", "https://api.anthropic.com"},
	} {
		if _, err := captureStdout(t, func() error { return cmdBackends(args) }); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
	out, err := captureStdout(t, func() error {
		return cmdBackends([]string{"set", "--only", "claude,codex,api", "--local-api", "http://127.0.0.1:8081"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "api       enabled: local endpoint http://127.0.0.1:8081, the last carry-on partner") {
		t.Fatalf("listing:\n%s", out)
	}
	t.Setenv("ANTHROPIC_API_KEY", "sk-test-value")
	out, err = captureStdout(t, func() error { return cmdBackends(nil) })
	if err != nil || !strings.Contains(string(out), "not used while ANTHROPIC_API_KEY is set") || strings.Contains(string(out), "sk-test-value") {
		t.Fatalf("listing with a key:\n%s %v", out, err)
	}
	if _, err := captureStdout(t, func() error { return cmdBackends([]string{"set", "--only", "claude,api"}) }); err != nil {
		t.Fatal(err)
	}
	if p, _ := prefs.Load(runlog.Home()); p.LocalAPI != "" {
		t.Fatal("set without --local-api kept the endpoint")
	}
	if _, err := captureStdout(t, func() error {
		return cmdBackends([]string{"set", "--only", "claude,api", "--local-api", "http://localhost:8081"})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := captureStdout(t, func() error { return cmdBackends([]string{"reset"}) }); err != nil {
		t.Fatal(err)
	}
	if p, err := prefs.Load(runlog.Home()); err != nil || p.LocalAPI != "" || p.WorkMapGitHub == nil {
		t.Fatalf("reset: %+v %v", p, err)
	}
}

func TestBackendsSetsListsAndClearsProvider(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	off := false
	if err := prefs.Save(home, prefs.Prefs{WorkMapGitHub: &off}); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		out, err := captureStdout(t, func() error { return cmdBackends(args) })
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	set := []string{"set", "--only", "claude", "--billing", "metered", "--provider", "claude=bedrock"}
	for _, clear := range [][]string{{"set", "--only", "claude", "--billing", "metered"}, {"reset"}} {
		if out := run(set...); !strings.Contains(out, "(provider: bedrock)") {
			t.Fatalf("set listing: %q", out)
		}
		if out := run(); !strings.Contains(out, "(provider: bedrock)") {
			t.Fatalf("saved listing: %q", out)
		}
		before, err := prefs.Raw(home)
		if err != nil {
			t.Fatal(err)
		}
		err = cmdBackends([]string{"set", "--only", "claude", "--provider", "claude=bedrock"})
		if err == nil || err.Error() != "providers: claude target bedrock bills per token and needs billing metered" {
			t.Fatalf("want billing validation error, got %v", err)
		}
		for _, provider := range []string{"", "claude", "codex=plan", "claude=unknown"} {
			if err := cmdBackends([]string{"set", "--only", "claude", "--billing", "metered", "--provider", provider}); err == nil {
				t.Errorf("accepted --provider %q", provider)
			}
		}
		if after, err := prefs.Raw(home); err != nil || string(after) != string(before) {
			t.Fatalf("invalid set changed preferences: %s, %v", after, err)
		}
		if out := run(clear...); strings.Contains(out, "(provider:") {
			t.Fatalf("cleared listing: %q", out)
		}
		if p, err := prefs.Load(home); err != nil || p.WorkMapGitHub == nil {
			t.Fatalf("clear lost work map setting: %+v, %v", p, err)
		}
		if raw, err := prefs.Raw(home); err != nil || strings.Contains(string(raw), "providers") {
			t.Fatalf("clear left providers in preferences: %s, %v", raw, err)
		}
	}
	if out := run("set", "--only", "claude", "--provider", "claude=plan"); strings.Contains(out, "(provider:") {
		t.Fatalf("plan listing: %q", out)
	}
}

func TestBackendsAccountFlagsAndRunRefusal(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	orig, _ := agent.Get("claude")
	agent.Register(recorder{name: "claude"})
	t.Cleanup(func() { agent.Register(orig) })
	home := t.TempDir()
	for _, want := range []string{"has no named accounts", "unknown claude account"} {
		if want == "unknown claude account" {
			out, err := captureStdout(t, func() error {
				return cmdBackends([]string{"set", "--only", "claude", "--account", "claude=team-a:" + home, "--account", "claude=team-b:" + home, "--account-reserve", "0"})
			})
			if err != nil || !strings.Contains(string(out), "    team-a: usage unknown\n    team-b: usage unknown\n") || strings.Contains(string(out), home) {
				t.Fatalf("listing: %s, %v", out, err)
			}
			p, err := prefs.Load(runlog.Home())
			if err != nil || p.Reserve() != 0 || len(p.AccountsFor("claude")) != 2 {
				t.Fatalf("saved: %+v, %v", p, err)
			}
		}
		code, err := cmdRun([]string{"--backend", "claude", "--account", "missing", "hello"})
		if code != 2 || err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("run = %d, %v; want %s", code, err, want)
		}
	}

	if err := limits.NoteAccount("claude", "team-b", agent.Result{Status: agent.StatusLimited, LimitResets: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	out, err := captureStdout(t, func() error { return cmdBackends(nil) })
	if err != nil || !strings.Contains(string(out), "    team-b: usage unknown, at its usage limit until ") || strings.Contains(string(out), home) {
		t.Fatalf("limit listing: %s, %v", out, err)
	}
	account, backend, err := parseBackendAccount(`claude=team-a:C:\x`)
	if err != nil || backend != "claude" || account.Name != "team-a" || account.Home != `C:\x` {
		t.Fatalf("drive path: %+v, %s, %v", account, backend, err)
	}
	records, err := runlog.List(0)
	if err != nil || len(records) != 0 {
		t.Fatalf("recorded a refused run: %v, %v", records, err)
	}
	for _, clear := range [][]string{{"set", "--only", "claude"}, {"reset"}} {
		set := []string{"set", "--only", "claude", "--account", "claude=team-a:" + home, "--account-reserve", "25"}
		if _, err := captureStdout(t, func() error { return cmdBackends(set) }); err != nil {
			t.Fatal(err)
		}
		if _, err := captureStdout(t, func() error { return cmdBackends(clear) }); err != nil {
			t.Fatal(err)
		}
		p, err := prefs.Load(runlog.Home())
		if err != nil || len(p.Accounts) != 0 || p.AccountReservePercent != nil {
			t.Fatalf("clear: %+v, %v", p, err)
		}
	}
}

func TestBackendsOrganisationSetAndReset(t *testing.T) {
	t.Setenv("HRN_HOME", prefstest.TempDir(t))
	path := prefstest.OrgPath(t)
	for _, tc := range []struct {
		field, doc string
		args       []string
	}{
		{"enabled", `"enabled":["api","claude"]`, nil},
		{"default", `"enabled":["api","claude"],"default":"api"`, []string{"--default", "claude"}},
		{"default", `"enabled":["api","claude"],"default":"api"`, []string{"--default", ""}},
		{"billing", `"billing":"plan"`, []string{"--billing", "metered"}},
		{"providers", `"billing":"metered"`, []string{"--billing", "metered", "--provider", "claude=vertex"}},
		{"accounts", `"billing":"plan"`, []string{"--account", "claude=work:" + prefstest.TempDir(t)}},
		{"local_api", `"local_api":"http://localhost:2000"`, []string{"--local-api", "http://localhost:3000"}},
	} {
		t.Run(tc.field, func(t *testing.T) {
			prefstest.Write(t, path, fmt.Sprintf(`{%s,"locked":[%q]}`, tc.doc, tc.field))
			err := cmdBackends(append([]string{"set", "--only", "claude,api"}, tc.args...))
			if err == nil || err.Error() != "backends set: "+tc.field+" is set by the organisation" {
				t.Fatalf("set error = %v", err)
			}
			if _, err := prefs.Raw(runlog.Home()); !os.IsNotExist(err) {
				t.Fatalf("user file changed: %v", err)
			}
		})
	}
	prefstest.Write(t, path, `{"enabled":["claude","api"],"locked":["enabled"]}`)
	if err := cmdBackends([]string{"set", "--only", "claude,api"}); err != nil {
		t.Fatal(err)
	}
	prefstest.Write(t, path, `{"billing":"plan","locked":["billing"]}`)
	err := cmdBackends([]string{"set", "--only", "claude,api", "--provider", "claude=vertex"})
	if err == nil || !strings.Contains(err.Error(), "(after the organisation's settings)") {
		t.Fatalf("merged error = %v", err)
	}
	prefstest.Write(t, path, "{")
	if err := cmdBackends([]string{"reset"}); err == nil || err.Error() != "the organisation's backends.json: malformed JSON" {
		t.Fatalf("reset error = %v", err)
	}
	if _, err := prefs.Raw(runlog.Home()); !os.IsNotExist(err) {
		t.Fatalf("reset left user file: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestBackendsOrganisationNoticeAndListing(t *testing.T) {
	backendNoticeOnce = sync.Once{}
	t.Cleanup(func() { backendNoticeOnce = sync.Once{} })
	t.Setenv("HRN_HOME", prefstest.TempDir(t))
	path := prefstest.OrgPath(t)
	prefstest.Write(t, path, `{"enabled":["claude"],"billing":"plan","locked":["billing","local_api"]}`)
	prefstest.Write(t, filepath.Join(runlog.Home(), "backends.json"), `{"billing":"metered","local_api":"http://localhost:1234"}`)
	stderr, err := os.CreateTemp(prefstest.TempDir(t), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer stderr.Close()
	old := os.Stderr
	os.Stderr = stderr
	defer func() { os.Stderr = old }()
	out, err := captureStdout(t, func() error {
		for range 2 {
			if err := cmdBackends(nil); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"  enabled: set by the organisation\n", "  billing: set by the organisation (locked)\n", "  local_api: set by the organisation (locked)\n"} {
		if !strings.Contains(string(out), line) {
			t.Errorf("missing %q", line)
		}
	}
	got, err := os.ReadFile(stderr.Name())
	want := "backends: billing, local_api set by the organisation; your backends.json values for them are ignored\n"
	if err != nil || strings.Count(string(got), want) != 1 {
		t.Fatalf("stderr = %q, %v", got, err)
	}
}

func TestRouteSetSurfacesUnreadableOrganisationPreferences(t *testing.T) {
	t.Setenv("HRN_HOME", prefstest.TempDir(t))
	prefstest.Write(t, prefstest.OrgPath(t), "{")
	if code, err := cmdRoute([]string{"set", purpose.Categories[0], "-b", "claude"}); code != 2 || err == nil || !strings.Contains(err.Error(), "the organisation's backends.json") {
		t.Fatalf("route set = %d, %v", code, err)
	}
}

func TestBackendsSetDefaultWithOrganisationEnabled(t *testing.T) {
	home := prefstest.TempDir(t)
	t.Setenv("HRN_HOME", home)
	policy := prefstest.OrgPath(t)
	args := []string{"set", "--default", "claude"}
	if err := cmdBackends(args); err == nil || !strings.HasPrefix(err.Error(), "usage: hrn backends set --only ") {
		t.Fatalf("missing --only: %v", err)
	}
	prefstest.Write(t, policy, `{"enabled":["claude"],"locked":["enabled"]}`)
	if _, err := captureStdout(t, func() error { return cmdBackends(args) }); err != nil {
		t.Fatal(err)
	}
	raw, err := prefs.Raw(home)
	if err != nil || strings.Contains(string(raw), "enabled") || !strings.Contains(string(raw), `"default": "claude"`) {
		t.Fatalf("user file = %s, %v", raw, err)
	}
	args[2] = "codex"
	if err := cmdBackends(args); err == nil || err.Error() != "backend preferences: default backend is not in enabled (after the organisation's settings)" {
		t.Fatalf("excluded default: %v", err)
	}
}

func TestBackendsResetPreservesPersonalSettingsWhenPolicyBreaks(t *testing.T) {
	home := prefstest.TempDir(t)
	t.Setenv("HRN_HOME", home)
	policy := prefstest.OrgPath(t)
	prefstest.Write(t, filepath.Join(home, "backends.json"), `{"default":"claude","work_map_github":true}`)
	prefstest.Write(t, policy, "{")
	if err := cmdBackends([]string{"reset"}); err == nil || err.Error() != "the organisation's backends.json: malformed JSON" {
		t.Fatalf("reset error = %v", err)
	}
	p, err := prefs.Load(home)
	if err != nil || p.Default() != "" || p.WorkMapGitHub == nil || !*p.WorkMapGitHub {
		t.Fatalf("reset did not preserve personal settings: %+v, %v", p, err)
	}
}

func TestRouteSetSkipsMalformedUserPreferences(t *testing.T) {
	home := prefstest.TempDir(t)
	t.Setenv("HRN_HOME", home)
	prefstest.OrgPath(t)
	prefstest.Write(t, filepath.Join(home, "backends.json"), "{")
	if code, err := cmdRoute([]string{"set", purpose.Categories[0], "-b", "claude"}); code != 0 || err != nil {
		t.Fatalf("route set = %d, %v", code, err)
	}
}

func TestBatchAndEvalRejectMalformedOrganisationPreferences(t *testing.T) {
	t.Setenv("HRN_HOME", prefstest.TempDir(t))
	prefstest.Write(t, prefstest.OrgPath(t), "{")
	for _, tc := range []struct {
		name string
		run  func([]string) (int, error)
	}{
		{"batch", cmdBatch},
		{"eval", cmdEval},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := filepath.Join(prefstest.TempDir(t), "spec.json")
			prefstest.Write(t, spec, `{"tasks":[{"prompt":"test"}]}`)
			if code, err := tc.run([]string{spec}); code != 2 || err == nil || err.Error() != "the organisation's backends.json: malformed JSON" {
				t.Fatalf("command = %d, %v", code, err)
			}
		})
	}
}
