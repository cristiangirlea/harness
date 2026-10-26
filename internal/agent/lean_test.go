package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
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

func TestClaudeNoSkillsPassesTheSwitchOnceAndBeatsSkills(t *testing.T) {
	for _, o := range []Options{{NoSkills: true}, {NoSkills: true, Skills: Bool(true)}, {NoSkills: true, Lean: Bool(false)}} {
		args := NewClaudeBackend().args(Task{Options: o}, "")
		n := 0
		for _, a := range args {
			if a == "--disable-slash-commands" {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%+v: --disable-slash-commands passed %d times: %v", o, n, args)
		}
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
		" -c mcp_servers.lit.enabled=false" +
		" -c mcp_servers.github.enabled=true" +
		` -c mcp_servers.github.command="npx"` +
		` -c mcp_servers.github.args=["-y","@x/gh"]` +
		` -c mcp_servers.github.env_vars=["GH_LEVEL"]` +
		" -c mcp_servers.docs.enabled=true" +
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
	token, set := os.LookupEnv("GITHUB_TOKEN")
	fmt.Printf("GITHUB_TOKEN set=%v %s\n", set, token)
	fmt.Println("GIT_CONFIG_COUNT=" + os.Getenv("GIT_CONFIG_COUNT"))
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

func TestCodexMCPEnvIsGuardedLikeTheRestOfTheEnvironment(t *testing.T) {
	codexHome(t, "")
	t.Setenv("HRN_CODEX_ENV_HELPER", "1")
	unsetenv(t, "GIT_CONFIG_COUNT", "GITHUB_TOKEN")
	t.Setenv("HRN_TEST_WITHHELD", "withheld-token-value")
	file := mcpFile(t, `"github":{"command":"npx","env":{"GITHUB_TOKEN":"${HRN_TEST_WITHHELD}","GIT_CONFIG_COUNT":"0"}}`)
	b := NewCodexBackend()
	b.name, b.candidates = os.Args[0], nil
	b.args = func(Task, string) []string { return []string{"-test.run=^TestHelperCodexMCPEnv$", "--"} }
	b.parse = nil
	res := b.Run(context.Background(), Task{Prompt: "go", Dir: t.TempDir(), Options: Options{MCP: []string{file}}}, func(Event) {})
	if res.Status != StatusOK {
		t.Fatalf("run: %+v", res)
	}
	if strings.Contains(res.Final, "withheld-token-value") || !strings.Contains(res.Final, "GITHUB_TOKEN set=false") {
		t.Errorf("a secret-named server variable must be withheld: %q", res.Final)
	}
	if !regexp.MustCompile(`(?m)^GIT_CONFIG_COUNT=6\r?$`).MatchString(res.Final) {
		t.Errorf("the guard's GIT_CONFIG_COUNT must win over a server's: %q", res.Final)
	}
}

func TestCodexKeepsExpandedMCPValuesOffTheStartLine(t *testing.T) {
	codexHome(t, "")
	t.Setenv("HRN_CODEX_ENV_HELPER", "1")
	t.Setenv("HRN_TEST_EXPANDED", "expanded-secret")
	file := mcpFile(t, `"tool":{"command":"${HRN_TEST_EXPANDED}","args":["--k=${HRN_TEST_EXPANDED}"],"env":{"E":"1"}},`+
		`"web":{"url":"https://example.test/${HRN_TEST_EXPANDED}"}`)
	b := NewCodexBackend()
	b.name, b.candidates = os.Args[0], nil
	b.args = func(t Task, _ string) []string {
		return append([]string{"-test.run=^TestHelperCodexMCPEnv$", "--"}, codexMCPArgs(t)...)
	}
	b.parse = nil
	var events []string
	res := b.Run(context.Background(), Task{Prompt: "go", Dir: t.TempDir(), Options: Options{MCP: []string{file}}}, func(e Event) {
		events = append(events, e.Text)
	})
	if res.Status != StatusOK {
		t.Fatalf("run: %+v", res)
	}
	start := ""
	for _, text := range events {
		if strings.Contains(text, "expanded-secret") {
			t.Errorf("an event shows an expanded value: %q", text)
		}
		if strings.Contains(text, "mcp_servers.tool.command=") {
			start = text
		}
	}
	for _, field := range []string{"tool.command", "tool.args", "tool.env_vars", "web.url"} {
		if !strings.Contains(start, "mcp_servers."+field+"=<hidden>") {
			t.Errorf("the start line must name %s with its value hidden: %q", field, start)
		}
	}
	if !strings.Contains(start, "mcp_servers.tool.enabled=true") {
		t.Errorf("enabled carries no secret and stays shown: %q", start)
	}
}

// unsetenv removes names for the test and restores them after.
func unsetenv(t *testing.T, names ...string) {
	t.Helper()
	for _, n := range names {
		t.Setenv(n, "")
		os.Unsetenv(n)
	}
}

func TestCodexRefusesAServerEnvThatWouldChangeCodexsOwn(t *testing.T) {
	t.Setenv("HRN_TEST_SHARED", "hrn-value")
	for servers, refused := range map[string]bool{
		`"a":{"command":"x","env":{"HRN_TEST_SHARED":"other"}}`:              true,
		`"a":{"command":"x","env":{"HRN_TEST_SHARED":"hrn-value"}}`:          false,
		`"a":{"command":"x","env":{"HRN_TEST_SHARED":"${HRN_TEST_SHARED}"}}`: false,
	} {
		_, err := codexMCPEnv(Task{Options: Options{MCP: []string{mcpFile(t, servers)}}})
		if got := err != nil && strings.Contains(err.Error(), "HRN_TEST_SHARED is already set"); got != refused {
			t.Errorf("%s: refused = %v, want %v (err %v)", servers, got, refused, err)
		}
	}
}

func TestCodexRefusesMCPServerCodexHomeBeforeStarting(t *testing.T) {
	for _, key := range []string{"CODEX_HOME", "codex_home"} {
		if key != "CODEX_HOME" && runtime.GOOS != "windows" {
			continue
		}
		for _, tc := range []struct {
			name, home string
		}{
			{"unset", ""},
			{"empty", ""},
			{"same", "server-home-value"},
			{"different", "hrn-home-value"},
		} {
			t.Run(key+"/"+tc.name, func(t *testing.T) {
				if tc.name == "unset" {
					unsetenv(t, "CODEX_HOME")
				} else {
					t.Setenv("CODEX_HOME", tc.home)
				}
				t.Setenv("HRN_CODEX_ENV_HELPER", "1")
				file := mcpFile(t, fmt.Sprintf(`"home-server":{"command":"x","env":{%q:"server-home-value"}}`, key))
				b := NewCodexBackend()
				b.name, b.candidates = os.Args[0], nil
				b.args = func(Task, string) []string { return []string{"-test.run=^TestHelperCodexMCPEnv$", "--"} }
				b.parse = nil
				var events []Event
				res := b.Run(context.Background(), Task{Prompt: "go", Dir: t.TempDir(), Options: Options{MCP: []string{file}}}, func(e Event) {
					events = append(events, e)
				})
				if res.Status != StatusError || !strings.Contains(res.Error, "home-server") || !strings.Contains(res.Error, key) || !strings.Contains(res.Error, "not allowed") {
					t.Errorf("want a refusal naming the server and variable, got %+v", res)
				}
				if res.Final != "" || len(events) != 1 || events[0].Kind != EventError {
					t.Errorf("codex must be refused before its start line or child output: result %+v, events %+v", res, events)
				}
				for _, value := range []string{"server-home-value", "hrn-home-value"} {
					if strings.Contains(res.Error, value) || slices.ContainsFunc(events, func(e Event) bool { return strings.Contains(e.Text, value) }) {
						t.Errorf("a diagnostic shows an environment value: result %+v, events %+v", res, events)
					}
				}
			})
		}
	}
}

func TestCodexRefusesAServerEnvThatHrnKeepsFromAgents(t *testing.T) {
	t.Setenv("HRN_DATABASE_URL", "postgres://hrn")
	for _, servers := range []string{
		`"a":{"command":"x","env":{"HRN_DATABASE_URL":"${HRN_DATABASE_URL}"}}`,
		`"a":{"command":"x","env":{"HRN_REMOTE_TOKEN":"t"}}`,
		`"a":{"command":"x","env":{"GIT_DIR":"/somewhere"}}`,
		`"a":{"command":"x","env":{"CLAUDECODE":"1"}}`,
	} {
		_, err := codexMCPEnv(Task{Options: Options{MCP: []string{mcpFile(t, servers)}}})
		if err == nil || !strings.Contains(err.Error(), "out of an agent's environment") {
			t.Errorf("%s: want a refusal, got %v", servers, err)
		}
	}
}

func TestCodexNeverGetsHrnsSecretsThroughAnExpandedValue(t *testing.T) {
	codexHome(t, "")
	t.Setenv("HRN_DATABASE_URL", "postgres://secret")
	file := mcpFile(t, `"a":{"command":"x","args":["${HRN_DATABASE_URL}"],"env":{"DB":"${HRN_DATABASE_URL}"}},"r":{"url":"https://example.test/${HRN_DATABASE_URL}"}`)
	task := Task{Options: Options{MCP: []string{file}}}
	env, err := codexMCPEnv(task)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(append(env, codexMCPArgs(task)...), " "); strings.Contains(got, "secret") {
		t.Errorf("an expanded secret reached codex: %s", got)
	}
}

func TestCodexReadsARelativeCodexHomeFromTheRunsDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "home"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "home", "config.toml"), []byte("[mcp_servers.docs]\ncommand = \"x\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", "home")
	if got, want := strings.Join(codexMCPArgs(Task{Dir: dir}), " "), "-c mcp_servers.docs.enabled=false"; got != want {
		t.Errorf("args = %q, want %q", got, want)
	}
}

func TestCodexRefusesTwoServersThatSetOneWindowsVariableDifferently(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("environment names are case-sensitive here")
	}
	file := mcpFile(t, `"a":{"command":"x","env":{"HRN_TEST_CASE":"1"}},"b":{"command":"y","env":{"hrn_test_case":"2"}}`)
	if _, err := codexMCPEnv(Task{Options: Options{MCP: []string{file}}}); err == nil || !strings.Contains(err.Error(), "differently") {
		t.Errorf("want a refusal, got %v", err)
	}
}

func TestCodexRefusesARemoteServerWhoseHeadersOrTransportWouldBeDropped(t *testing.T) {
	for servers, refused := range map[string]bool{
		`"r":{"url":"https://example.test","headers":{"Authorization":"Bearer x"}}`: true,
		`"r":{"type":"sse","url":"https://example.test"}`:                           true,
		`"r":{"type":"http","url":"https://example.test"}`:                          false,
		`"r":{"url":"https://example.test"}`:                                        false,
	} {
		_, err := codexMCPEnv(Task{Options: Options{MCP: []string{mcpFile(t, servers)}}})
		if got := err != nil && strings.Contains(err.Error(), "would be dropped"); got != refused {
			t.Errorf("%s: refused = %v, want %v (err %v)", servers, got, refused, err)
		}
	}
}

func TestCodexSkipsConfigServersWhoseNameIsNotABareKey(t *testing.T) {
	codexHome(t, "[mcp_servers.\"my server\"]\ncommand = \"x\"\n[mcp_servers.ok]\ncommand = \"y\"\n")
	got := strings.Join(codexMCPArgs(Task{}), " ")
	if want := "-c mcp_servers.ok.enabled=false"; got != want {
		t.Errorf("args = %q, want %q", got, want)
	}
}

func TestCodexRefusesARunServerWhoseNameIsNotABareKey(t *testing.T) {
	for _, servers := range []string{`"my server":{"command":"x"}`, `"a.b":{"url":"https://example.test"}`} {
		file := mcpFile(t, servers)
		if _, err := codexMCPEnv(Task{Options: Options{MCP: []string{file}}}); err == nil || !strings.Contains(err.Error(), "letters, digits") {
			t.Errorf("%s: want a refusal naming the allowed characters, got %v", servers, err)
		}
	}
}

func TestCodexEnablesARunServerTheConfigDisables(t *testing.T) {
	codexHome(t, "[mcp_servers.github]\ncommand = \"npx\"\nenabled = false\n")
	file := mcpFile(t, `"github":{"command":"npx"},"docs":{"url":"https://example.test/mcp"}`)
	got := strings.Join(codexMCPArgs(Task{Options: Options{MCP: []string{file}}}), " ")
	for _, want := range []string{"mcp_servers.github.enabled=true", "mcp_servers.docs.enabled=true"} {
		if !strings.Contains(got, want) {
			t.Errorf("args lack %s: %q", want, got)
		}
	}
	if strings.Contains(got, "enabled=false") {
		t.Errorf("a run server must not be disabled: %q", got)
	}
}

func TestCodexGivesAnEmptyArgsListSoTheConfigsArgsDoNotMergeIn(t *testing.T) {
	codexHome(t, "")
	file := mcpFile(t, `"github":{"command":"npx","args":[]}`)
	got := strings.Join(codexMCPArgs(Task{Options: Options{MCP: []string{file}}}), " ")
	if !strings.Contains(got, "-c mcp_servers.github.args=[]") {
		t.Errorf("an empty args must still be set: %q", got)
	}
}

func TestCodexEscapesAWindowsCommandPath(t *testing.T) {
	codexHome(t, "")
	file := mcpFile(t, `"gh":{"command":"C:\\Tools\\gh.exe","args":["D:\\x"]}`)
	got := strings.Join(codexMCPArgs(Task{Options: Options{MCP: []string{file}}}), " ")
	for _, want := range []string{`mcp_servers.gh.command="C:\\Tools\\gh.exe"`, `mcp_servers.gh.args=["D:\\x"]`} {
		if !strings.Contains(got, want) {
			t.Errorf("args lack %s: %q", want, got)
		}
	}
}
