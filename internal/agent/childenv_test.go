package agent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// An agent CLI starts from the variables it needs and no others: the system's,
// its own login places, the toolchains', the proxies' and hrn's own. Secret
// names stay out even when they look like one of those, except the CLI's own
// plan login.
func TestTheChildEnvironmentKeepsOnlyTheAllowlist(t *testing.T) {
	common := []string{
		"PATH=/bin", "HOME=/home/op", "USER=op", "LOGNAME=op", "SHELL=/bin/bash", "TERM=xterm", "COLORTERM=truecolor",
		"LANG=C.UTF-8", "LC_ALL=C", "LC_CTYPE=C", "TMPDIR=/tmp", "TMP=/tmp", "TEMP=/tmp",
		"HTTPS_PROXY=http://proxy", "http_proxy=http://proxy", "NO_PROXY=localhost", "SSL_CERT_FILE=/ca.pem",
		"CLAUDE_CONFIG_DIR=/c", "CODEX_HOME=/x", "GCM_CREDENTIAL_STORE=dpapi", "GOPATH=/go", "JAVA_HOME=/jdk", "XDG_CONFIG_HOME=/cfg", "HRN_HOME=/h",
	}
	dropped := []string{
		"GH_TOKEN=ghp", "GH_HOST=example.com", "GITHUB_TOKEN=ghs", "GIT_ASKPASS=/askpass", "GIT_CONFIG_GLOBAL=/gitconfig",
		"SSH_AUTH_SOCK=/agent", "AWS_ACCESS_KEY_ID=AKIA", "ANTHROPIC_API_KEY=sk", "OPENAI_API_KEY=sk", "NPM_TOKEN=n",
		"DB_PASSWORD=p", "MY_SECRET=s", "SERVICE_CREDENTIALS=c", "DEPLOY_PRIVATE_KEY=k", "HRN_TENANT_TOKEN=t",
		"OPERATOR_SETTING=x", "EDITOR=vim", "CLAUDECODE=1", "GOOGLE_API_KEY=g",
		"HRN_TEST_DATABASE_URL=postgres://u:p@db", "HRN_SERVER_URL=https://u:p@hrn", "HRN_TLS_KEY_FILE=/k.pem",
	}
	windows := []string{
		"SystemRoot=C:\\Windows", "ComSpec=C:\\Windows\\system32\\cmd.exe", "PATHEXT=.COM;.EXE", "APPDATA=C:\\a",
		"LOCALAPPDATA=C:\\l", "USERPROFILE=C:\\u", "windir=C:\\Windows", "ProgramFiles=C:\\p", "SystemDrive=C:",
		"USERNAME=op", "CLAUDE_CODE_GIT_BASH_PATH=C:\\git\\bash.exe",
	}
	for _, tc := range []struct {
		goos    string
		in      []string
		kept    []string
		dropped int
	}{
		{"linux", concat(common, dropped, windows), append(slices.Clone(common), "USERNAME=op"), len(dropped) + len(windows) - 1},
		{"windows", concat(common, dropped, windows), concat(common, windows), len(dropped)},
	} {
		t.Run(tc.goos, func(t *testing.T) {
			got, n, secrets := sanitizedEnv(tc.in, tc.goos)
			if !sameSet(got, tc.kept) {
				t.Errorf("kept:\n got %v\nwant %v", got, tc.kept)
			}
			if n != tc.dropped {
				t.Errorf("dropped %d, want %d", n, tc.dropped)
			}
			// The secret-looking ones are named, for the guard's line.
			want := []string{"ANTHROPIC_API_KEY", "AWS_ACCESS_KEY_ID", "DB_PASSWORD", "DEPLOY_PRIVATE_KEY", "GH_TOKEN", "GITHUB_TOKEN",
				"GIT_ASKPASS", "GOOGLE_API_KEY", "HRN_TENANT_TOKEN", "MY_SECRET", "NPM_TOKEN", "OPENAI_API_KEY",
				"SERVICE_CREDENTIALS", "SSH_AUTH_SOCK"}
			if !slices.Equal(secrets, want) {
				t.Errorf("secret names:\n got %v\nwant %v", secrets, want)
			}
		})
	}
	// The CLI's own plan login passes though its name says token.
	if got, _, _ := sanitizedEnv([]string{"CLAUDE_CODE_OAUTH_TOKEN=plan", "GH_TOKEN=x"}, "linux", "CLAUDE_CODE_OAUTH_TOKEN"); !slices.Equal(got, []string{"CLAUDE_CODE_OAUTH_TOKEN=plan"}) {
		t.Errorf("the plan login must pass to its own CLI: %v", got)
	}
	// Windows names are case-insensitive; the original spelling is kept.
	if got, _, _ := sanitizedEnv([]string{"Path=C:\\bin", "gh_token=x"}, "windows"); !slices.Equal(got, []string{"Path=C:\\bin"}) {
		t.Errorf("names must match in any case: %v", got)
	}
}

// The child sees neither a planted token nor an unlisted variable, also with
// the guard lifted, and the run says how many variables it dropped without
// naming them. --inherit-env passes the operator's environment as it is.
func TestACLIChildDoesNotSeeThePlantedToken(t *testing.T) {
	t.Setenv("HRN_CLI_HELPER", "env")
	t.Setenv("GH_TOKEN", "ghp_planted")
	t.Setenv("OPERATOR_SETTING", "operator-value")
	run := func(opts Options) (map[string]string, []string) {
		var info []string
		res := helperCLI(parseClaudeStream).Run(context.Background(), Task{Prompt: "go", Dir: t.TempDir(), Options: opts}, func(e Event) {
			if e.Kind == EventInfo {
				info = append(info, e.Text)
			}
		})
		got := map[string]string{}
		for _, kv := range strings.Split(res.Final, ";") {
			k, v, _ := strings.Cut(kv, "=")
			got[k] = v
		}
		return got, info
	}
	got, info := run(Options{Mode: ModeYolo, AllowDangerous: true})
	if got["GH_TOKEN"] != "" || got["OPERATOR_SETTING"] != "" || got["PATH_SET"] != "true" {
		t.Fatalf("the sanitized CLI saw %v; want no GH_TOKEN, no OPERATOR_SETTING and a PATH", got)
	}
	line, guard := "", ""
	for _, l := range info {
		switch {
		case strings.HasPrefix(l, "environment:"):
			line = l
		case strings.HasPrefix(l, "guard:"):
			guard = l
		}
		// Never a value, and never an ordinary variable's name.
		if strings.Contains(l, "OPERATOR_SETTING") || strings.Contains(l, "ghp_planted") || strings.Contains(l, "operator-value") {
			t.Errorf("an info line names a dropped variable or shows a value: %q", l)
		}
	}
	if !strings.Contains(line, "sanitized") || !strings.Contains(line, "variables dropped") || strings.Contains(line, "GH_TOKEN") {
		t.Errorf("the run must say its environment was sanitized and how many variables were dropped, naming none: %q", info)
	}
	// With the guard lifted only a count is said, so a CLI that logs in with a
	// key says why it cannot, and how to let it, without naming anything.
	if strings.Contains(guard, "GH_TOKEN") || !strings.Contains(guard, "secret-looking variables still left out") || !strings.Contains(guard, "--inherit-env") {
		t.Errorf("with the guard lifted the run must count, not name, the tokens the sanitized environment leaves out: %q", guard)
	}

	got, info = run(Options{Mode: ModeYolo, AllowDangerous: true, InheritEnv: true})
	if got["GH_TOKEN"] != "ghp_planted" || got["OPERATOR_SETTING"] != "operator-value" {
		t.Fatalf("with --inherit-env the CLI saw %v; want the operator's environment", got)
	}
	if !slices.ContainsFunc(info, func(l string) bool { return strings.HasPrefix(l, "environment: inherited") }) {
		t.Errorf("the run must say its environment was inherited: %q", info)
	}
}

// The allowlist touches only the environment: claude keeps every setting
// source, so the operator's CLAUDE.md, rules, plugins and skills still load,
// and its argv is the same with the environment inherited or not.
func TestClaudeKeepsTheOwnersSettingsUnderTheAllowlist(t *testing.T) {
	for _, lean := range []*bool{nil, Bool(true), Bool(false)} {
		opts := Options{Mode: ModeAuto, MCP: []string{"a.json"}, Lean: lean}
		clean := NewClaudeBackend().args(Task{Options: opts}, "")
		if slices.Contains(clean, "--setting-sources") {
			t.Errorf("lean=%v: claude must load every setting source: %v", lean, clean)
		}
		if strict := slices.Contains(clean, "--strict-mcp-config"); strict != (Task{Options: opts}).IsLean() {
			t.Errorf("lean=%v: --strict-mcp-config follows lean alone: %v", lean, clean)
		}
		opts.InheritEnv = true
		if inherit := NewClaudeBackend().args(Task{Options: opts}, ""); !slices.Equal(inherit, clean) {
			t.Errorf("lean=%v: --inherit-env must not change claude's argv:\n got %v\nwant %v", lean, inherit, clean)
		}
	}
}

// agy signs in from disk, but its account, its ADC sign-in and Cloud project
// are chosen by variables only agy gets; the guard still withholds the ADC file.
func TestAgyKeepsItsOwnLoginVariables(t *testing.T) {
	t.Setenv("HRN_CLI_HELPER", "env")
	t.Setenv("AGY_ACCOUNT", "op@example.com")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "/adc.json")
	run := func(b *CLIBackend, opts Options) map[string]string {
		res := b.Run(context.Background(), Task{Prompt: "go", Dir: t.TempDir(), Options: opts}, func(Event) {})
		got := map[string]string{}
		for _, kv := range strings.Split(res.Final, ";") {
			k, v, _ := strings.Cut(kv, "=")
			got[k] = v
		}
		return got
	}
	agy := helperCLI(parseClaudeStream)
	agy.ownEnv = NewAgyBackend().ownEnv
	if got := run(agy, Options{Mode: ModeYolo}); got["AGY_ACCOUNT"] != "op@example.com" || got["GOOGLE_APPLICATION_CREDENTIALS"] != "" {
		t.Errorf("guarded agy saw %v; want its account and no ADC file", got)
	}
	if got := run(agy, Options{Mode: ModeYolo, AllowDangerous: true}); got["AGY_ACCOUNT"] != "op@example.com" || got["GOOGLE_APPLICATION_CREDENTIALS"] != "/adc.json" {
		t.Errorf("agy with the guard lifted saw %v; want its account and ADC file", got)
	}
	if got := run(helperCLI(parseClaudeStream), Options{Mode: ModeYolo, AllowDangerous: true}); got["AGY_ACCOUNT"] != "" || got["GOOGLE_APPLICATION_CREDENTIALS"] != "" {
		t.Errorf("another CLI saw agy's variables: %v", got)
	}
}

// gemini picks its Cloud project and sign-in route by variables only gemini gets.
func TestGeminiKeepsItsOwnLoginVariables(t *testing.T) {
	t.Setenv("HRN_CLI_HELPER", "env")
	vars := []string{"GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_LOCATION", "GOOGLE_GENAI_USE_VERTEXAI", "GOOGLE_GENAI_USE_GCA"}
	t.Setenv("HRN_CLI_HELPER_VARS", strings.Join(vars, ","))
	for _, k := range vars {
		t.Setenv(k, "v-"+k)
	}
	for _, b := range []*CLIBackend{NewGeminiBackend(), NewClaudeBackend(), NewCodexBackend()} {
		h := helperCLI(parseClaudeStream)
		h.ownEnv, h.loginEnv = b.ownEnv, b.loginEnv
		got := helperEnv(t, h, Options{Mode: ModeYolo})
		for _, k := range vars {
			if want := map[bool]string{true: "v-" + k}[b.name == "gemini"]; got[k] != want {
				t.Errorf("%s saw %s=%q, want %q", b.name, k, got[k], want)
			}
		}
	}
}

// The api key stays out of a CLI even when an MCP server's env, added after
// the allowlist, sets it; the server's other variables do reach the CLI.
func TestEnvExtrasReachTheCLIButNotTheAPIKey(t *testing.T) {
	t.Setenv("HRN_CLI_HELPER", "env")
	t.Setenv("HRN_CLI_HELPER_VARS", "MY_SERVER_SETTING")
	cfg := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(cfg, []byte(`{"mcpServers":{"s":{"command":"srv","env":{"MY_SERVER_SETTING":"x","ANTHROPIC_API_KEY":"sk-ant-metered"}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	h := helperCLI(parseClaudeStream)
	h.env = codexMCPEnv
	for _, opts := range []Options{{Mode: ModeYolo, MCP: []string{cfg}}, {Mode: ModeYolo, MCP: []string{cfg}, AllowDangerous: true}} {
		got := helperEnv(t, h, opts)
		if got["MY_SERVER_SETTING"] != "x" || got["ANTHROPIC_API_KEY"] != "" {
			t.Errorf("allow_dangerous=%v: the CLI saw %v; want MY_SERVER_SETTING=x and no ANTHROPIC_API_KEY", opts.AllowDangerous, got)
		}
	}
}

// A proxy URL passes without its user and password, in every element of a
// GOPROXY list; a GCM_ name that looks secret stays out, GCM's settings pass.
func TestAllowedValuesCarryNoCredentials(t *testing.T) {
	in := []string{
		"HTTPS_PROXY=http://u:p@proxy:8080", "http_proxy=http://u@proxy/", "ALL_PROXY=socks5://u:p@proxy",
		"NO_PROXY=localhost", "HTTP_PROXY=u:p@proxy:3128", "GCM_HTTP_PROXY=http://u:p@proxy",
		"GOPROXY=https://u:p@goproxy.example/a@b,direct|https://proxy.golang.org",
		"GCM_CREDENTIAL_STORE=dpapi", "GCM_INTERACTIVE=never", "GCM_PASSWORD=p", "GCM_DEPLOY_TOKEN=t",
	}
	want := []string{
		"HTTPS_PROXY=http://proxy:8080", "http_proxy=http://proxy/", "ALL_PROXY=socks5://proxy",
		"NO_PROXY=localhost", "HTTP_PROXY=proxy:3128", "GCM_HTTP_PROXY=http://proxy",
		"GOPROXY=https://goproxy.example/a@b,direct|https://proxy.golang.org",
		"GCM_CREDENTIAL_STORE=dpapi", "GCM_INTERACTIVE=never",
	}
	got, n, secrets := sanitizedEnv(in, "linux")
	if !slices.Equal(got, want) || n != 2 || !slices.Equal(secrets, []string{"GCM_DEPLOY_TOKEN", "GCM_PASSWORD"}) {
		t.Errorf("got %v, %d dropped, secrets %v\nwant %v, 2 dropped, GCM_DEPLOY_TOKEN and GCM_PASSWORD", got, n, secrets, want)
	}
}

func helperEnv(t *testing.T, b *CLIBackend, opts Options) map[string]string {
	t.Helper()
	res := b.Run(context.Background(), Task{Prompt: "go", Dir: t.TempDir(), Options: opts}, func(Event) {})
	got := map[string]string{}
	for _, kv := range strings.Split(res.Final, ";") {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}
	return got
}

func concat(lists ...[]string) []string {
	var out []string
	for _, l := range lists {
		out = append(out, l...)
	}
	return out
}

func sameSet(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}
