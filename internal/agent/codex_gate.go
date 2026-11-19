package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"

	"github.com/cristiangirlea/harness/internal/tools"
)

const codexGateServer = "hrn_gate"

// run_command's longest timeout and the gate's grace, past codex's 60-second default for an MCP call.
const codexGateToolTimeoutSec = "660"

// Every feature seen to offer a tool that runs commands outside the gate. code_mode_host stays on:
// MCP tools are reached only through it, and its script can call tools but start no process.
var codexGateDisabled = []string{"shell_tool", "unified_exec", "computer_use", "browser_use", "apps", "plugins"}

// The items a routed run may stream; any other could be a new tool that runs code, so it stops the run.
var codexKnownItems = []string{"agent_message", "reasoning", "mcp_tool_call", "file_change", "web_search", "todo_list", "error"}

var plainEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// codexGateRoute turns codex's shell tools off and gives it hrn's gate-backed run_command. Only in
// yolo, which has no sandbox to lose: the tool's server runs outside codex's sandbox.
func codexGateRoute(_ context.Context, t Task, gate, policy string) ([]string, func(), error) {
	if t.Mode != ModeYolo {
		return nil, nil, fmt.Errorf("gate: on the codex backend it needs --mode yolo: the gate's tool runs outside codex's sandbox, which the other modes rely on (mode %q)", t.Mode)
	}
	// A configured server of the same name would be merged with this one, not replaced.
	servers, remotes, _, err := codexMCPConfigs(t)
	if err != nil {
		return nil, nil, fmt.Errorf("gate: %w", err)
	}
	names := codexAllConfigServers(t)
	for _, s := range servers {
		names = append(names, s.Name)
	}
	for _, r := range remotes {
		names = append(names, r.Name)
	}
	if slices.Contains(names, codexGateServer) {
		return nil, nil, fmt.Errorf("gate: an MCP server named %s is configured; rename it to use --gate on codex", codexGateServer)
	}
	self, err := os.Executable()
	if err != nil {
		return nil, nil, fmt.Errorf("gate: find hrn for the command tool: %w", err)
	}
	// codex starts the server in the run's directory, where a relative path would resolve twice.
	dir, err := filepath.Abs(t.Dir)
	if err != nil {
		return nil, nil, fmt.Errorf("gate: %w", err)
	}
	sum, err := tools.GatePolicySum(policy)
	if err != nil {
		return nil, nil, fmt.Errorf("gate: %w", err)
	}
	server := []string{"gate-tools", "--dir", dir, "--gate", gate, "--policy", policy, "--policy-sha256", sum}
	if t.RunDir != "" {
		server = append(server, "--run-dir", t.RunDir)
	}
	if t.AllowDangerous {
		server = append(server, "--allow-dangerous")
	}
	if t.ShapeTools {
		server = append(server, "--shape-tools")
	}
	var args []string
	for _, feature := range codexGateDisabled {
		args = append(args, "--disable", feature)
	}
	for _, c := range [][2]string{
		{"enabled", "true"},
		{"command", tomlString(self)},
		{"args", tomlArray(server)},
		{"env_vars", tomlArray(codexGateEnvNames(t))},
		{"startup_timeout_sec", "30"},
		{"tool_timeout_sec", codexGateToolTimeoutSec},
	} {
		args = append(args, "-c", "mcp_servers."+codexGateServer+"."+c[0]+"="+c[1])
	}
	return args, func() {}, nil
}

// codexGateEnvNames names what codex hands the tool's server, since it starts servers with almost no
// environment and commands need the one they would have had. The server applies the guard again.
func codexGateEnvNames(t Task) []string {
	env := withoutEnv(agentEnv(os.Environ()), "ANTHROPIC_API_KEY")
	if !t.InheritEnv {
		env, _, _ = sanitizedEnv(env, runtime.GOOS)
	}
	var names []string
	for _, kv := range append(env, t.ChildEnv...) {
		// Plain names only: a cmd.exe shim refuses -c values with parentheses, as in ProgramFiles(x86).
		if name, _, _ := strings.Cut(kv, "="); plainEnvName.MatchString(name) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// codexGateArgsAt keeps options ahead of a fork subcommand and the prompt's "-".
func codexGateArgsAt(args []string) int {
	at := len(args) - 1
	if at >= 2 && args[at-2] == "fork" {
		at -= 2
	}
	return at
}

func newCodexGateParse(_, _ string) func(line []byte, emit func(Event), res *Result) bool {
	return func(line []byte, emit func(Event), res *Result) bool {
		parsed := parseCodexLine(line, emit, res, true)
		seeCodexGateItem(line, res)
		return parsed
	}
}

type codexMCPItem struct {
	Type      string          `json:"type"`
	Server    string          `json:"server"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
	Status    string          `json:"status"`
	Result    *struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	} `json:"result"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// codexToolItem is a stream item for a tool hrn counts besides the shell.
type codexToolItem struct {
	codexMCPItem
	Changes []struct {
		Path string `json:"path"`
		Kind string `json:"kind"`
	} `json:"changes"`
}

func readCodexToolItem(line []byte) codexToolItem {
	var l struct {
		Item codexToolItem `json:"item"`
	}
	// A field of an unexpected shape stays empty; the others are still read.
	_ = json.Unmarshal(line, &l)
	return l.Item
}

// seeCodexGateItem stops a run whose command got past the gate's tool, reading only the item's type.
func seeCodexGateItem(line []byte, res *Result) {
	var l struct {
		Type string          `json:"type"`
		Item json.RawMessage `json:"item"`
	}
	if json.Unmarshal(line, &l) != nil || !strings.HasPrefix(l.Type, "item.") {
		return
	}
	var item struct {
		Type string `json:"type"`
	}
	// An item whose type cannot be read is as unknown as a new one.
	_ = json.Unmarshal(l.Item, &item)
	if item.Type == "command_execution" {
		res.Error = gateUnrouted + "codex ran a command through its own shell tool, outside the gate"
		return
	}
	if !slices.Contains(codexKnownItems, item.Type) {
		res.Error = gateUnrouted + fmt.Sprintf("codex used a tool hrn does not know (item %q), which may run outside the gate", item.Type)
	}
}

// codexMCPToolName names MCP tools as Claude Code does; the gate's own tools keep their short names.
func codexMCPToolName(item codexMCPItem, gated bool) string {
	if gated && item.Server == codexGateServer {
		return codexGateToolName(item.Tool)
	}
	return "mcp__" + item.Server + "__" + item.Tool
}

func codexGateToolName(tool string) string {
	if tool == "run_command" {
		return "shell"
	}
	return tool
}

func codexGateCallText(item codexMCPItem) string {
	var args struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(item.Arguments, &args) == nil && args.Command != "" {
		return args.Command
	}
	return string(item.Arguments)
}

func codexGateResultText(item codexMCPItem) string {
	var parts []string
	if item.Result != nil {
		for _, c := range item.Result.Content {
			parts = append(parts, c.Text)
		}
	}
	if item.Error != nil {
		parts = append(parts, item.Error.Message)
	}
	return strings.Join(parts, "\n")
}
