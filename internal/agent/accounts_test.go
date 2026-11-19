package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAccountHomeReplacesTheChildEnvironment(t *testing.T) {
	t.Setenv("HRN_CLI_HELPER", "env")
	t.Setenv("PATH", t.TempDir())
	for backend, key := range map[string]string{"claude": "CLAUDE_CONFIG_DIR", "codex": "CODEX_HOME"} {
		t.Run(backend, func(t *testing.T) {
			t.Setenv("HRN_CLI_HELPER_VARS", key)
			t.Setenv(key, "caller")
			home := t.TempDir()
			for _, dangerous := range []bool{false, true} {
				for _, inherit := range []bool{false, true} {
					task := Task{Dir: t.TempDir(), Options: Options{InheritEnv: inherit, AllowDangerous: dangerous}, ChildEnv: []string{key + "=child"}}
					b := helperCLI(parseClaudeStream)
					b.name = backend
					b.candidates = func() []string { return []string{os.Args[0]} }
					b.env = func(Task) ([]string, error) { return []string{key + "=extra"}, nil }
					var accountEvents int
					baseline := b.Run(t.Context(), task, func(e Event) {
						if strings.HasPrefix(e.Text, "account:") {
							t.Error(e.Text)
						}
					})
					if baseline.Status != StatusOK || !strings.Contains(baseline.Final, key+"=child") || baseline.Account != "" {
						t.Fatalf("baseline: %+v", baseline)
					}
					b.Account, b.AccountHome = "team-a", home
					res := b.Run(t.Context(), task, func(e Event) {
						if e.Kind == EventInfo && e.Text == "account: team-a" {
							accountEvents++
						}
						if strings.Contains(e.Text, home) {
							t.Error("event exposed the home")
						}
					})
					if accountEvents != 1 || res.Status != StatusOK || res.Account != "team-a" || strings.Contains(res.Final, home) || !strings.Contains(res.Final, key+"=[account home]") {
						t.Fatalf("account run: %+v, events: %d", res, accountEvents)
					}
					b.AccountHome = filepath.Join(home, "missing")
					res = b.Run(t.Context(), task, func(Event) {})
					if res.Status != StatusError || res.Error != "account team-a: its config home is missing" {
						t.Fatalf("missing home: %+v", res)
					}
				}
			}
		})
	}
}

func TestSetupAndCodexLookupsUseTheSelectedHome(t *testing.T) {
	isolateSetupHome(t)
	caller, home, dir := t.TempDir(), filepath.Join(t.TempDir(), "selected-account-home"), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", caller)
	t.Setenv("CODEX_HOME", caller)
	writeTree(t, caller, map[string]string{
		"AGENTS.md":    "caller",
		"config.toml":  "[mcp_servers.caller]",
		".claude.json": `{"mcpServers":{"caller":{"command":"c"}}}`,
	})
	writeTree(t, home, map[string]string{
		"AGENTS.override.md": "account",
		"config.toml":        "[mcp_servers.account]",
		".claude.json":       `{"mcpServers":{"account":{"command":"a"}}}`,
	})
	claude := NewClaudeBackend()
	claude.Account, claude.AccountHome = "team-a", home
	saved := Task{Dir: dir}
	if got := ResolveBackendSetup(claude, &saved).MCP; len(got) != 1 || got[0] != "account" {
		t.Fatalf("claude setup: %v", got)
	}
	record, err := json.Marshal(saved)
	if err != nil || !strings.Contains(string(record), `"account":"team-a"`) || saved.AccountHome != home ||
		strings.Contains(string(record), "account_home") || strings.Contains(string(record), filepath.Base(home)) {
		t.Fatal("task record must contain the account name and omit its home")
	}
	codex := NewCodexBackend()
	codex.Account, codex.AccountHome = "team-a", home
	files := ResolveBackendSetup(codex, &Task{Dir: dir}).Files
	if files["$CODEX_HOME/AGENTS.override.md"] == "" || files["$CODEX_HOME/AGENTS.md"] != "" {
		t.Fatalf("codex setup: %v", files)
	}
	writeTree(t, dir, map[string]string{".git": ""})
	task := Task{Dir: dir, AccountHome: home}
	for _, scan := range []func(Task) []string{codexLoadedConfigServers, codexAllConfigServers} {
		if got := scan(task); len(got) != 1 || got[0] != "account" {
			t.Fatalf("codex MCP: %v", got)
		}
	}
	if got, ok := codexMCPLookup(task)("CODEX_HOME"); !ok || got != home {
		t.Error("codex MCP expansion used the caller home")
	}
	now := time.Now()
	placeRollout(t, home, now, same)
	res := Result{Session: recordedCodexThread}
	codexReportModel(task, now, &res, func(Event) {}, 0)
	if !res.ModelReported {
		t.Fatalf("codex account rollout was not found: %+v", res)
	}
}
