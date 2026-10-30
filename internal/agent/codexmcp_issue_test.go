package agent

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestCodexTOMLServerForms(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	content := "mcp_servers.one = { command = 'x' }\nmcp_servers.two.command = 'x'\ntext = \"\"\"\n[mcp_servers.fake]\n\"\"\"\ntext2 = '''\n[mcp_servers.fake2]\n'''\n[mcp_servers]\nthree = { command = 'x' }\nfour.command = 'x'\n[mcp_servers.five.env]\nX = 'x'\n"
	if err := os.WriteFile(p, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	got := codexConfigServers(p)
	for _, n := range []string{"one", "two", "three", "four", "five"} {
		if !slices.Contains(got, n) {
			t.Errorf("missing %s: %v", n, got)
		}
	}
	for _, n := range []string{"fake", "fake2"} {
		if slices.Contains(got, n) {
			t.Errorf("spurious %s: %v", n, got)
		}
	}
}

func TestCodexMCPProjectAndCollision(t *testing.T) {
	codexHome(t, "[mcp_servers.home]\ncommand='x'\n")
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".codex"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".codex", "config.toml"), []byte("[mcp_servers.project]\ncommand='x'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	task := Task{Dir: dir}
	if got := strings.Join(codexMCPArgs(task), " "); !strings.Contains(got, "mcp_servers.project.enabled=false") {
		t.Errorf("project server not disabled: %s", got)
	}
	for _, n := range []string{"home", "project"} {
		task.MCP = []string{mcpFile(t, `"`+n+`":{"command":"x"}`)}
		if _, err := codexMCPEnv(task); err == nil || !strings.Contains(err.Error(), n) {
			t.Errorf("collision %s: %v", n, err)
		}
	}
	lean := false
	task.Lean = &lean
	if _, err := codexMCPEnv(task); err == nil || !strings.Contains(err.Error(), "project") {
		t.Errorf("non-lean collision: %v", err)
	}
}

func TestCodexMCPBearerAndSSE(t *testing.T) {
	codexHome(t, "")
	task := Task{Options: Options{MCP: []string{mcpFile(t, `"remote":{"url":"https://example.test","headers":{"Authorization":"Bearer ${TEST_TOKEN}"}}`)}}}
	if _, err := codexMCPEnv(task); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(codexMCPArgs(task), " "); !strings.Contains(got, `bearer_token_env_var="TEST_TOKEN"`) {
		t.Errorf("missing bearer mapping: %s", got)
	}
	for _, header := range []string{`"Authorization":"Bearer literal"`, `"X-Extra":"value"`, `"Authorization":"Bearer ${TEST_TOKEN}","X-Extra":"value"`} {
		task.MCP = []string{mcpFile(t, `"remote":{"url":"https://example.test","headers":{`+header+`}}`)}
		if _, err := codexMCPEnv(task); err == nil || !strings.Contains(err.Error(), "remote") || !strings.Contains(err.Error(), "headers") {
			t.Errorf("unsupported header: %v", err)
		}
	}
	task.MCP = []string{mcpFile(t, `"remote":{"url":"https://example.test","type":"sse"}`)}
	if _, err := codexMCPEnv(task); err == nil || !strings.Contains(err.Error(), "streamable HTTP") {
		t.Errorf("sse: %v", err)
	}
}

func TestCodexBatchConfigValues(t *testing.T) {
	for _, bin := range []string{"codex.cmd", "codex.bat"} {
		for _, value := range []string{`x="a"`, "x=a b", "x=a&b", "x=a|b", "x=a<b", "x=a>b", "x=a^b", "x=%NAME%"} {
			if err := codexBatchArgsError(bin, []string{"-c", value}); err == nil || !strings.Contains(err.Error(), "exe") {
				t.Errorf("%s %s: %v", bin, value, err)
			}
		}
		if err := codexBatchArgsError(bin, []string{"-c", "x=false"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := codexBatchArgsError("codex.exe", []string{"-c", `x="a & b"`}); err != nil {
		t.Fatal(err)
	}
}
