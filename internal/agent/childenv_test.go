package agent

import (
	"context"
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
	// The guard's line names the secret-looking ones, as it always has, so a
	// CLI that logs in with a key says why it cannot, and how to let it.
	if !strings.Contains(guard, "GH_TOKEN") || !strings.Contains(guard, "--inherit-env") {
		t.Errorf("with the guard lifted the run must name the tokens the sanitized environment still leaves out: %q", guard)
	}

	got, info = run(Options{Mode: ModeYolo, AllowDangerous: true, InheritEnv: true})
	if got["GH_TOKEN"] != "ghp_planted" || got["OPERATOR_SETTING"] != "operator-value" {
		t.Fatalf("with --inherit-env the CLI saw %v; want the operator's environment", got)
	}
	if !slices.ContainsFunc(info, func(l string) bool { return strings.HasPrefix(l, "environment: inherited") }) {
		t.Errorf("the run must say its environment was inherited: %q", info)
	}
}

// Unless the environment is inherited, claude loads only the repository's
// own settings and only hrn's --mcp-config servers: the operator's hooks,
// plugins, permission rules and MCP servers stay out.
func TestClaudeLoadsOnlyProjectSettingsAndHrnsMCPServers(t *testing.T) {
	opts := Options{Mode: ModeAuto, MCP: []string{"a.json"}}
	clean := NewClaudeBackend().args(Task{Options: opts}, "")
	d := slices.Index(clean, "--disallowedTools")
	s := slices.Index(clean, "--setting-sources")
	m := slices.Index(clean, "--strict-mcp-config")
	if s < 0 || s+1 >= len(clean) || clean[s+1] != "project" || m < 0 {
		t.Fatalf("want --setting-sources project and --strict-mcp-config: %v", clean)
	}
	if d < 0 || s > d || m > d {
		t.Errorf("both flags must come before the deny rules: %v", clean)
	}
	if !slices.Contains(clean, "--mcp-config") {
		t.Errorf("hrn's own MCP configs must still be passed: %v", clean)
	}
	opts.Lean = Bool(true)
	if lean := NewClaudeBackend().args(Task{Options: opts}, ""); countOf(lean, "--strict-mcp-config") != 1 {
		t.Errorf("lean and a sanitized run pass --strict-mcp-config once: %v", lean)
	}
	opts.Lean, opts.InheritEnv = nil, true
	inherit := NewClaudeBackend().args(Task{Options: opts}, "")
	if slices.Contains(inherit, "--setting-sources") || slices.Contains(inherit, "--strict-mcp-config") {
		t.Errorf("--inherit-env keeps every setting source and MCP server: %v", inherit)
	}
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

func countOf(list []string, s string) int {
	n := 0
	for _, v := range list {
		if v == s {
			n++
		}
	}
	return n
}
