package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestLeanIsOnUnlessARunTurnsItOff(t *testing.T) {
	for _, tc := range []struct {
		json string
		want *bool
		lean bool
	}{{`{}`, nil, true}, {`{"lean":false}`, Bool(false), false}, {`{"lean":true}`, Bool(true), true}} {
		var task Task
		if err := json.Unmarshal([]byte(tc.json), &task); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(task.Lean, tc.want) || task.IsLean() != tc.lean {
			t.Errorf("%s: Lean = %v, IsLean = %v; want %v, %v", tc.json, task.Lean, task.IsLean(), tc.want, tc.lean)
		}
		b, err := json.Marshal(task.Options)
		if err != nil {
			t.Fatal(err)
		}
		if tc.want == nil && strings.Contains(string(b), "lean") {
			t.Errorf("%s: an unset lean must stay out of the record: %s", tc.json, b)
		}
		if tc.want != nil && !strings.Contains(string(b), fmt.Sprintf(`"lean":%v`, *tc.want)) {
			t.Errorf("%s: the record must keep lean as written: %s", tc.json, b)
		}
	}
}

func TestSkillsAreOffUnlessARunAsksForThem(t *testing.T) {
	var o Options
	if err := json.Unmarshal([]byte(`{}`), &o); err != nil || o.Skills != nil {
		t.Fatalf("old records must leave skills unset: %v %v", o.Skills, err)
	}
	if b, _ := json.Marshal(Options{}); strings.Contains(string(b), "skills") {
		t.Errorf("an unset skills must stay out of the record: %s", b)
	}
	if b, _ := json.Marshal(Options{Skills: Bool(true)}); !strings.Contains(string(b), `"skills":true`) {
		t.Errorf("skills must be recorded: %s", b)
	}
	if p, ok := (Options{}).Schema()["skills"].(map[string]any); !ok || p["type"] != "boolean" {
		t.Errorf("the MCP schema must list skills as a boolean: %v", p)
	}
}

func TestClaudeLoadsNoOtherMCPServersAndNoSkillsByDefault(t *testing.T) {
	args := NewClaudeBackend().args(Task{Options: Options{Mode: ModeAuto}}, "")
	d := slices.Index(args, "--disallowedTools")
	for _, flag := range []string{"--strict-mcp-config", "--disable-slash-commands"} {
		if i := slices.Index(args, flag); i < 0 || i > d {
			t.Errorf("default args must pass %s before the deny rules: %v", flag, args)
		}
	}
	for _, flag := range []string{"--setting-sources", "--bare"} {
		if slices.Contains(args, flag) {
			t.Errorf("the user's settings and login must stay: %s in %v", flag, args)
		}
	}
}

func TestClaudeLeanFalseLoadsEverything(t *testing.T) {
	args := NewClaudeBackend().args(Task{Options: Options{Lean: Bool(false)}}, "")
	for _, flag := range []string{"--strict-mcp-config", "--disable-slash-commands"} {
		if slices.Contains(args, flag) {
			t.Errorf("lean false must not pass %s: %v", flag, args)
		}
	}
}

func TestClaudeSkillsTrueKeepsSkillsButStaysLean(t *testing.T) {
	args := NewClaudeBackend().args(Task{Options: Options{Skills: Bool(true)}}, "")
	if slices.Contains(args, "--disable-slash-commands") || !slices.Contains(args, "--strict-mcp-config") {
		t.Errorf("skills true: want --strict-mcp-config only: %v", args)
	}
}

// codexHome writes config to a fresh CODEX_HOME, the directory codex reads it from.
func codexHome(t *testing.T, config string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	if config != "" {
		if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

const codexConfigFixture = `model = "gpt-5"

[mcp_servers.github]
command = "npx"

[mcp_servers.github.env]
GITHUB_TOKEN = "never read"

[mcp_servers."my server"]
command = "x"

[ mcp_servers . 'lit' ] # a comment
url = "https://example.test"

[profiles.fast]
model = "gpt-5-mini"
`

// mcpFile writes a Claude Code .mcp.json with the given servers and returns its path.
func mcpFile(t *testing.T, servers string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(p, []byte(`{"mcpServers":{`+servers+`}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCodexDisablesEachServerInTheUsersConfigByDefault(t *testing.T) {
	codexHome(t, codexConfigFixture)
	got := strings.Join(NewCodexBackend().args(Task{Dir: "/w", Options: Options{Mode: ModeReadOnly}}, ""), " ")
	want := "exec --json --skip-git-repo-check -C /w" +
		" -c mcp_servers.github.enabled=false" +
		` -c mcp_servers."my server".enabled=false` +
		" -c mcp_servers.lit.enabled=false" +
		" --sandbox read-only -"
	if got != want {
		t.Errorf("args = %q\n  want %q", got, want)
	}
}

func TestCodexTranslatesTheRunsMCPServersInsteadOfDisablingThem(t *testing.T) {
	codexHome(t, codexConfigFixture)
	file := mcpFile(t, `"github":{"command":"npx","args":["-y","@x/gh"],"env":{"GH_LEVEL":"value-only-in-env"}},`+
		`"docs":{"type":"http","url":"https://example.test/mcp","headers":{"Authorization":"Bearer no"}}`)
	args := NewCodexBackend().args(Task{Dir: "/w", Options: Options{MCP: []string{file}}}, "")
	got := strings.Join(args, " ")
	want := "exec --json --skip-git-repo-check -C /w" +
		` -c mcp_servers."my server".enabled=false` +
		" -c mcp_servers.lit.enabled=false" +
		` -c mcp_servers.github.command="npx"` +
		` -c mcp_servers.github.args=["-y","@x/gh"]` +
		` -c mcp_servers.github.env_vars=["GH_LEVEL"]` +
		` -c mcp_servers.docs.url="https://example.test/mcp"` +
		" -"
	if got != want {
		t.Errorf("args = %q\n  want %q", got, want)
	}
	if strings.Contains(got, "value-only-in-env") || strings.Contains(got, "Bearer") {
		t.Errorf("env values and headers must stay off the command line: %q", got)
	}
}

func TestCodexWithoutAConfigFileDisablesNothing(t *testing.T) {
	codexHome(t, "")
	got := strings.Join(NewCodexBackend().args(Task{Dir: "/w"}, ""), " ")
	if want := "exec --json --skip-git-repo-check -C /w -"; got != want {
		t.Errorf("args = %q, want %q", got, want)
	}
}

func TestCodexLeanFalseKeepsTheUsersServers(t *testing.T) {
	codexHome(t, codexConfigFixture)
	got := strings.Join(NewCodexBackend().args(Task{Dir: "/w", Options: Options{Lean: Bool(false)}}, ""), " ")
	if want := "exec --json --skip-git-repo-check -C /w -"; got != want {
		t.Errorf("args = %q, want %q", got, want)
	}
}

// TestHelperCodexMCPEnv is not a test: the test binary acts as codex and
// prints the variable a translated MCP server is given.
func TestHelperCodexMCPEnv(t *testing.T) {
	if os.Getenv("HRN_CODEX_ENV_HELPER") != "1" {
		return
	}
	fmt.Println("GH_LEVEL=" + os.Getenv("GH_LEVEL"))
	os.Exit(0)
}

func TestCodexGivesMCPEnvValuesToTheChildOnly(t *testing.T) {
	codexHome(t, "")
	t.Setenv("HRN_CODEX_ENV_HELPER", "1")
	file := mcpFile(t, `"github":{"command":"npx","env":{"GH_LEVEL":"value-only-in-env"}}`)
	b := NewCodexBackend()
	b.name, b.candidates = os.Args[0], nil
	b.args = func(Task, string) []string { return []string{"-test.run=^TestHelperCodexMCPEnv$", "--"} }
	b.parse = nil
	var infos []string
	res := b.Run(context.Background(), Task{Prompt: "go", Dir: t.TempDir(), Options: Options{MCP: []string{file}}}, func(e Event) {
		if e.Kind == EventInfo {
			infos = append(infos, e.Text)
		}
	})
	if res.Status != StatusOK || !strings.Contains(res.Final, "GH_LEVEL=value-only-in-env") {
		t.Fatalf("the child must get the server's env value: %+v", res)
	}
	for _, line := range infos {
		if strings.Contains(line, "value-only-in-env") {
			t.Errorf("an info line shows the env value: %q", line)
		}
	}
}

func TestCodexRefusesTwoServersThatSetOneVariableDifferently(t *testing.T) {
	file := mcpFile(t, `"a":{"command":"x","env":{"K":"1"}},"b":{"command":"y","env":{"K":"2"}}`)
	if _, err := codexMCPEnv(Task{Options: Options{MCP: []string{file}}}); err == nil || !strings.Contains(err.Error(), "K") {
		t.Errorf("want an error naming K, got %v", err)
	}
}
