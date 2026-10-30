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

func TestCodexTOMLScannerEdges(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	content := "# [mcp_servers.comment]\r\n\"mcp_servers\".quoted = {}\r\nmcp_servers.'literal' = {}\r\nvalues = [\r\n\"[mcp_servers.fake]\",\r\n]\r\ntext = \"\"\"\r\n\\\"\"\"\r\n[mcp_servers.fake2]\r\n\"\"\"\r\n[mcp_servers]\r\nreal = {}\r\n[[other]]\r\nwrong = {}\r\n"
	if err := os.WriteFile(p, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	got := codexConfigServers(p)
	for _, n := range []string{"quoted", "literal", "real"} {
		if !slices.Contains(got, n) {
			t.Errorf("missing %s: %v", n, got)
		}
	}
	for _, n := range []string{"comment", "fake", "fake2", "wrong"} {
		if slices.Contains(got, n) {
			t.Errorf("spurious %s: %v", n, got)
		}
	}
}

func TestCodexMCPProjectAndCollision(t *testing.T) {
	dir := t.TempDir()
	codexHome(t, "[mcp_servers.home]\ncommand='x'\n")
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, ".codex"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".codex", "config.toml"), []byte("[mcp_servers.project]\ncommand='x'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(dir, "nested")
	if err := os.MkdirAll(filepath.Join(nested, ".codex"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, ".codex", "config.toml"), []byte("[mcp_servers.nested]\ncommand='x'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	task := Task{Dir: nested}
	if got := strings.Join(codexMCPArgs(task), " "); strings.Contains(got, "mcp_servers.project.enabled=false") {
		t.Errorf("untrusted project disabled: %s", got)
	}
	untrustedFile := mcpFile(t, `"project":{"command":"x"}`)
	task.MCP = []string{untrustedFile}
	if _, err := codexMCPEnv(task); err == nil || !strings.Contains(err.Error(), untrustedFile) {
		t.Errorf("untrusted collision: %v", err)
	}
	task.MCP = nil
	config := "[mcp_servers.home]\ncommand='x'\n[projects." + tomlString(dir) + "]\ntrust_level='trusted'\n"
	if err := os.WriteFile(codexConfigPath(nested), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(codexMCPArgs(task), " "); !strings.Contains(got, "mcp_servers.project.enabled=false") {
		t.Errorf("project server not disabled: %s", got)
	}
	if got := strings.Join(codexMCPArgs(task), " "); !strings.Contains(got, "mcp_servers.nested.enabled=false") {
		t.Errorf("nested server not disabled: %s", got)
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
	t.Setenv("TEST_TOKEN", "example")
	task := Task{Options: Options{AllowDangerous: true, MCP: []string{mcpFile(t, `"remote":{"url":"https://example.test","headers":{"authorization":"Bearer ${TEST_TOKEN}"}}`)}}}
	if _, err := codexMCPEnv(task); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(codexMCPArgs(task), " "); !strings.Contains(got, `bearer_token_env_var="TEST_TOKEN"`) {
		t.Errorf("missing bearer mapping: %s", got)
	}
	task.AllowDangerous = false
	if _, err := codexMCPEnv(task); err == nil || !strings.Contains(err.Error(), "--inherit-env") {
		t.Errorf("missing bearer: %v", err)
	}
	task.MCP = []string{mcpFile(t, `"remote":{"url":"https://example.test","headers":{"Authorization":"Bearer ${ANTHROPIC_API_KEY}"}}`)}
	if _, err := codexMCPEnv(task); err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Errorf("forbidden bearer: %v", err)
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

func TestCodexMCPHomeCollisionSkipped(t *testing.T) {
	codexHome(t, "[mcp_servers.shared]\ncommand='x'\n")
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	path := filepath.Join(home, "mcp.json")
	if err := os.WriteFile(path, []byte(`{"mcpServers":{"shared":{"command":"x"},"other":{"command":"y"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	task := Task{Options: Options{MCP: []string{path}}}
	servers, _, skipped, err := codexMCPConfigs(task)
	if err != nil || len(servers) != 1 || servers[0].Name != "other" || !slices.Contains(skipped, "shared") {
		t.Fatalf("servers %v skipped %v err %v", servers, skipped, err)
	}
	explicit := mcpFile(t, `"shared":{"command":"z"}`)
	task.MCP = append(task.MCP, explicit)
	if _, err := codexMCPEnv(task); err == nil || !strings.Contains(err.Error(), explicit) {
		t.Errorf("explicit collision: %v", err)
	}
}

func TestCodexBatchConfigValues(t *testing.T) {
	for _, bin := range []string{"codex.cmd", "codex.bat"} {
		for _, value := range []string{"x=a&b", "x=a|b", "x=a<b", "x=a>b", "x=a^b", "x=%NAME%", "x=!", "x=(", "x=)"} {
			if err := codexBatchArgsError(bin, []string{"-c", value}); err == nil || !strings.Contains(err.Error(), "exe") {
				t.Errorf("%s %s: %v", bin, value, err)
			}
		}
		for _, value := range []string{"x=false", `x="a"`, "x=a b"} {
			if err := codexBatchArgsError(bin, []string{"-c", value}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := codexBatchArgsError("codex.exe", []string{"-c", `x="a & b"`}); err != nil {
		t.Fatal(err)
	}
}
