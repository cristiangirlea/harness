package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/tools"
)

const gatePlaceholder = "E:/g/adanext-exec.exe"

const (
	gateAllowLine = `{"type":"system","subtype":"hook_response","hook_name":"PreToolUse:Bash","hook_event":"PreToolUse","exit_code":0,"outcome":"success","stdout":"{\"hookSpecificOutput\":{\"hookEventName\":\"PreToolUse\",\"permissionDecision\":\"allow\",\"permissionDecisionReason\":\"adanext-exec allows it\",\"updatedInput\":{\"command\":\"'E:/g/adanext-exec.exe' 'run' '--hook-build' 'ab' '--shell' 'bash' '--script-b64' 'ZWNobyBoaQ'\"}}}\n"}`
	gateDenyLine  = `{"type":"system","subtype":"hook_response","hook_name":"PreToolUse:Bash","hook_event":"PreToolUse","exit_code":0,"outcome":"success","stdout":"{\"hookSpecificOutput\":{\"hookEventName\":\"PreToolUse\",\"permissionDecision\":\"deny\",\"permissionDecisionReason\":\"adanext-exec: refused under rule \\\"git-push\\\"\"}}\n"}`
	guardDenyLine = `{"type":"system","subtype":"hook_response","hook_name":"PreToolUse:Bash","hook_event":"PreToolUse","exit_code":0,"outcome":"success","stdout":"{\"hookSpecificOutput\":{\"hookEventName\":\"PreToolUse\",\"permissionDecision\":\"deny\",\"permissionDecisionReason\":\"blocked by the hrn guard (gh-publish): only the operator can allow this\"}}\n"}`
	psAllowLine   = `{"type":"system","subtype":"hook_response","hook_name":"PreToolUse:PowerShell","hook_event":"PreToolUse","exit_code":0,"outcome":"success","stdout":"{\"hookSpecificOutput\":{\"hookEventName\":\"PreToolUse\",\"permissionDecision\":\"allow\",\"permissionDecisionReason\":\"adanext-exec allows it\",\"updatedInput\":{\"command\":\"& 'E:/g/adanext-exec.exe' 'run' '--hook-build' 'ab' '--shell' 'pwsh' '--script-b64' 'ZWNobyBoaQ'\"}}}\n"}`
	otherHook     = `{"type":"system","subtype":"hook_response","hook_name":"PreToolUse:Bash","hook_event":"PreToolUse","exit_code":0,"outcome":"success","stdout":""}`
	otherHookJSON = `{"type":"system","subtype":"hook_response","hook_name":"PreToolUse:Bash","hook_event":"PreToolUse","exit_code":0,"outcome":"success","stdout":"{\"hookSpecificOutput\":{\"hookEventName\":\"PreToolUse\",\"permissionDecision\":\"allow\",\"permissionDecisionReason\":\"mine\"}}"}`
	failedHook    = `{"type":"system","subtype":"hook_response","hook_name":"PreToolUse:Bash","hook_event":"PreToolUse","exit_code":2,"outcome":"error","stdout":"","stderr":"adanext-exec: not found"}`
	otherRewrite  = `{"type":"system","subtype":"hook_response","hook_name":"PreToolUse:Bash","hook_event":"PreToolUse","exit_code":0,"outcome":"success","stdout":"{\"hookSpecificOutput\":{\"hookEventName\":\"PreToolUse\",\"permissionDecision\":\"allow\",\"updatedInput\":{\"command\":\"echo mine\"}}}"}`
	gateDone      = `{"type":"result","subtype":"success","num_turns":1,"result":"done"}`
)

func subagentUse(id, name string) string {
	return fmt.Sprintf(`{"type":"assistant","parent_tool_use_id":"toolu_p","message":{"id":"m-%s","content":[{"type":"tool_use","id":%q,"name":%q,"input":{"command":"echo hi"}}]}}`, id, id, name)
}

func gateUse(id, name string) string {
	return fmt.Sprintf(`{"type":"assistant","parent_tool_use_id":null,"message":{"id":"m-%s","content":[{"type":"tool_use","id":%q,"name":%q,"input":{"command":"echo hi"}}]}}`, id, id, name)
}

func gateUseCommand(id, command string) string {
	return fmt.Sprintf(`{"type":"assistant","parent_tool_use_id":null,"message":{"id":"m-%s","content":[{"type":"tool_use","id":%q,"name":"Bash","input":{"command":%q}}]}}`, id, id, command)
}

func gateResult(id string, isError bool) string {
	return fmt.Sprintf(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":%q,"content":"x","is_error":%v}]}}`, id, isError)
}

// gateStreams are what the stand-in CLI prints; a stream a run must stop on then waits, to prove it was stopped.
var gateStreams = map[string][]string{
	"routed":       {gateUse("a", "Bash"), otherHook, gateAllowLine, gateResult("a", false), gateDone},
	"refused":      {gateUse("a", "Bash"), gateDenyLine, gateResult("a", true), gateDone},
	"guarded":      {gateUse("a", "Bash"), guardDenyLine, gateAllowLine, gateResult("a", true), gateDone},
	"bothrefuse":   {gateUse("a", "Bash"), gateDenyLine, guardDenyLine, gateResult("a", true), gateDone},
	"subagent":     {subagentUse("a", "Bash"), gateAllowLine, gateResult("a", false), gateDone},
	"subnoevent":   {subagentUse("a", "Bash"), gateResult("a", false)},
	"banked":       {gateUse("a", "Bash"), gateDenyLine, guardDenyLine, gateResult("a", true), gateUse("b", "Bash"), gateResult("b", false)},
	"early":        {gateAllowLine, gateUse("a", "Bash"), gateResult("a", false)},
	"guardonly":    {gateUse("a", "Bash"), guardDenyLine, gateResult("a", true)},
	"rewritten":    {gateUse("a", "Bash"), gateAllowLine, otherRewrite, gateResult("a", false)},
	"duplicate":    {gateUse("a", "Bash"), gateUseCommand("b", "echo other"), gateAllowLine, gateAllowLine, gateResult("a", false), gateResult("b", false)},
	"sametext":     {gateUse("a", "Bash"), gateUse("b", "Bash"), gateAllowLine, gateAllowLine, gateResult("a", false), gateResult("b", false), gateDone},
	"reordered":    {gateUse("a", "Bash"), gateUse("b", "Bash"), gateAllowLine, gateResult("b", false), gateAllowLine, gateResult("a", false), gateDone},
	"refusalfirst": {gateUse("a", "Bash"), gateUseCommand("b", "git push"), gateDenyLine, gateAllowLine, gateResult("b", true), gateResult("a", false), gateDone},
	"trailing":     {gateUseCommand("a", "echo hi\n"), gateAllowLine, gateResult("a", false), gateDone},
	"otherexe":     {gateUse("a", "Bash"), strings.ReplaceAll(gateAllowLine, gatePlaceholder, "E:/g/other.exe"), gateResult("a", false)},
	"nonzero":      {gateUse("a", "Bash"), strings.Replace(gateAllowLine, `"exit_code":0`, `"exit_code":1`, 1), gateResult("a", false)},
	"workflow":     {gateUse("a", "Workflow")},
	"skill":        {gateUse("a", "Skill")},
	"powershell":   {gateUse("a", "PowerShell"), psAllowLine, gateResult("a", false), gateDone},
	"parallel":     {gateUse("a", "Bash"), gateUse("b", "Bash"), gateAllowLine, gateAllowLine, gateResult("a", false), gateResult("b", false), gateDone},
	"replayed":     {gateUse("a", "Bash"), gateUse("a", "Bash"), gateAllowLine, gateResult("a", false), gateResult("a", false), gateDone},
	"noevent":      {gateUse("a", "Bash"), gateResult("a", false)},
	"unhooked":     {gateUse("a", "Bash"), gateResult("a", false), gateDone},
	"otherhook":    {gateUse("a", "Bash"), otherHook, gateResult("a", false)},
	"otherjson":    {gateUse("a", "Bash"), otherHookJSON, gateResult("a", false)},
	"hookfailed":   {gateUse("a", "Bash"), failedHook, gateResult("a", true)},
	"wrongtool":    {gateUse("a", "PowerShell"), gateAllowLine, gateResult("a", false)},
	"oneofparall":  {gateUse("a", "Bash"), gateUse("b", "Bash"), gateAllowLine, gateResult("a", false), gateResult("b", false)},
	"afterresult":  {`{"type":"result","subtype":"error_during_execution","is_error":true,"num_turns":1,"result":"x"}`, gateUse("a", "Bash"), gateResult("a", false)},
	"monitor":      {gateUse("a", "Monitor")},
	"repl":         {gateUse("a", "REPL")},
	"ended":        {gateUse("a", "Bash"), gateDone},
}

// TestHelperCLIGate is not a test: the test binary acts as the claude CLI, records what it was
// started with and prints a stream.
func TestHelperCLIGate(t *testing.T) {
	stream := os.Getenv("HRN_GATE_STREAM")
	if stream == "" {
		t.Skip("helper")
	}
	args := os.Args[slices.Index(os.Args, "--")+1:]
	record := map[string]any{"args": args}
	if i := slices.Index(args, "--settings"); i >= 0 {
		data, err := os.ReadFile(args[i+1])
		record["settings"], record["settings_path"], record["settings_error"] = string(data), args[i+1], fmt.Sprint(err)
	}
	line, _ := json.Marshal(record)
	_ = os.WriteFile(os.Getenv("HRN_GATE_LOG"), line, 0o644)
	lines, known := gateStreams[stream]
	if !known {
		os.Exit(3)
	}
	for _, l := range lines {
		fmt.Println(strings.ReplaceAll(l, gatePlaceholder, os.Getenv("HRN_GATE_PATH")))
	}
	if !strings.Contains(lines[len(lines)-1], `"type":"result"`) && stream != "ended" {
		time.Sleep(30 * time.Second)
	}
	os.Exit(0)
}

type gateRecord struct {
	Args          []string `json:"args"`
	Settings      string   `json:"settings"`
	SettingsPath  string   `json:"settings_path"`
	SettingsError string   `json:"settings_error"`
}

// runGatedClaude runs the claude adapter with the test binary as the CLI and returns what it was started with.
func runGatedClaude(t *testing.T, stream string, o Options) (Result, []Event, *gateRecord) {
	t.Helper()
	return runGatedClaudeTask(t, stream, Task{Options: o})
}

func runGatedClaudeTask(t *testing.T, stream string, task Task) (Result, []Event, *gateRecord) {
	t.Helper()
	log := filepath.Join(t.TempDir(), "started.json")
	t.Setenv("HRN_GATE_STREAM", stream)
	t.Setenv("HRN_GATE_LOG", log)
	if gate, err := exec.LookPath(tools.GateName); err == nil {
		t.Setenv("HRN_GATE_PATH", filepath.ToSlash(gate))
	}
	b := NewClaudeBackend()
	real := b.args
	b.name, b.candidates = os.Args[0], nil
	b.args = func(task Task, prompt string) []string {
		return append([]string{"-test.run=^TestHelperCLIGate$", "--"}, real(task, prompt)...)
	}
	task.Prompt, task.Dir = "go", t.TempDir()
	var events []Event
	res := b.Run(context.Background(), task, func(e Event) { events = append(events, e) })
	data, err := os.ReadFile(log)
	if err != nil {
		return res, events, nil
	}
	var rec gateRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	return res, events, &rec
}

func argAfter(args []string, flag string) (string, bool) {
	if i := slices.Index(args, flag); i >= 0 && i+1 < len(args) {
		return args[i+1], true
	}
	return "", false
}

type gateSettings struct {
	Hooks map[string][]struct {
		Matcher string `json:"matcher"`
		Hooks   []struct {
			Type    string `json:"type"`
			Command string `json:"command"`
		} `json:"hooks"`
	} `json:"hooks"`
}

func TestGatedClaudeIsStartedWithTheHookTheToolListAndHookEvents(t *testing.T) {
	installFakeGate(t)
	res, events, rec := runGatedClaude(t, "routed", Options{Mode: ModeYolo, Gate: true})
	if res.Status != StatusOK {
		t.Fatalf("result = %s %q", res.Status, res.Error)
	}
	if rec == nil {
		t.Fatal("the CLI did not start")
	}
	if !slices.Contains(rec.Args, "--include-hook-events") {
		t.Errorf("no --include-hook-events in %v", rec.Args)
	}
	list, ok := argAfter(rec.Args, "--tools")
	if !ok {
		t.Fatalf("no --tools in %v", rec.Args)
	}
	named := strings.Split(list, ",")
	for _, want := range []string{"Bash", "PowerShell", "Read", "Edit", "Write", "Grep", "Glob", "Agent"} {
		if !slices.Contains(named, want) {
			t.Errorf("--tools %q leaves out %s", list, want)
		}
	}
	for _, off := range []string{"Monitor", "REPL", "Skill", "Workflow"} {
		if slices.Contains(named, off) {
			t.Errorf("--tools %q names %s, which runs code the hook does not see", list, off)
		}
	}
	if !slices.Contains(rec.Args, "--disallowedTools") {
		t.Error("the run lost hrn's deny rules")
	}
	for _, narrowing := range []string{"--setting-sources", "--bare", "--safe"} {
		if slices.Contains(rec.Args, narrowing) {
			t.Errorf("%s would drop the user's own settings: %v", narrowing, rec.Args)
		}
	}

	var s gateSettings
	if err := json.Unmarshal([]byte(rec.Settings), &s); err != nil {
		t.Fatalf("settings %q (%s): %v", rec.Settings, rec.SettingsError, err)
	}
	var whole map[string]json.RawMessage
	_ = json.Unmarshal([]byte(rec.Settings), &whole)
	if len(whole) != 1 || whole["hooks"] == nil {
		t.Errorf("the settings say more than the hook, and could override the user's: %s", rec.Settings)
	}
	pre := s.Hooks["PreToolUse"]
	if len(s.Hooks) != 1 || len(pre) != 1 || pre[0].Matcher != "Bash|PowerShell" || len(pre[0].Hooks) != 2 {
		t.Fatalf("settings = %s", rec.Settings)
	}
	gate, guard := pre[0].Hooks[0].Command, pre[0].Hooks[1].Command
	if !strings.Contains(gate, "adanext-exec") || !strings.HasSuffix(gate, "' hook claude-code || exit 2") || strings.Contains(gate, `\`) {
		t.Errorf("the gate's hook = %q, want its slashed path, quoted, failing closed", gate)
	}
	if !strings.HasSuffix(guard, "' guard-hook || exit 2") {
		t.Errorf("the guard's hook = %q", guard)
	}
	if _, err := os.Stat(rec.SettingsPath); !os.IsNotExist(err) {
		t.Errorf("the settings file outlived the run: %v", err)
	}
	said := false
	for _, e := range events {
		said = said || (e.Kind == EventInfo && strings.HasPrefix(e.Text, "gate: Bash and PowerShell commands run through ") &&
			strings.HasSuffix(e.Text, "; MCP servers' tools are not routed"))
	}
	if !said {
		t.Error("the run does not record which gate its commands ran through")
	}
}

func TestGatedClaudeWithAllowDangerousHasNoGuardHookButKeepsTheGate(t *testing.T) {
	installFakeGate(t)
	res, _, rec := runGatedClaude(t, "routed", Options{Mode: ModeYolo, Gate: true, AllowDangerous: true})
	if res.Status != StatusOK || rec == nil {
		t.Fatalf("result = %s %q", res.Status, res.Error)
	}
	if strings.Contains(rec.Settings, "guard-hook") || !strings.Contains(rec.Settings, "hook claude-code") {
		t.Errorf("settings = %s", rec.Settings)
	}
}

func TestGatedClaudeKeepsNoToolsEmpty(t *testing.T) {
	installFakeGate(t)
	_, _, rec := runGatedClaudeTask(t, "routed", Task{NoTools: true, Options: Options{Mode: ModeYolo, Gate: true}})
	if rec == nil {
		t.Fatal("the CLI did not start")
	}
	n := 0
	for i, a := range rec.Args {
		if a == "--tools" {
			n++
			if rec.Args[i+1] != "" {
				t.Errorf("--tools %q gives back tools the run turned off", rec.Args[i+1])
			}
		}
	}
	if n != 1 {
		t.Errorf("%d --tools flags in %v", n, rec.Args)
	}
}

func TestGatedClaudeRunsThatTheGateAnsweredFinish(t *testing.T) {
	installFakeGate(t)
	for _, stream := range []string{"routed", "refused", "guarded", "bothrefuse", "subagent", "powershell", "parallel", "replayed", "sametext", "reordered", "refusalfirst", "trailing"} {
		t.Run(stream, func(t *testing.T) {
			res, _, _ := runGatedClaude(t, stream, Options{Mode: ModeYolo, Gate: true})
			if res.Status != StatusOK || res.Final != "done" {
				t.Fatalf("result = %s %q final %q", res.Status, res.Error, res.Final)
			}
		})
	}
}

func TestGatedClaudeRefusedCommandReachesTheRecordAsAnError(t *testing.T) {
	installFakeGate(t)
	_, events, _ := runGatedClaude(t, "refused", Options{Mode: ModeYolo, Gate: true})
	for _, e := range events {
		if e.Kind == EventToolResult && e.IsError {
			return
		}
	}
	t.Fatal("no failed tool result in the record")
}

func TestGatedClaudeStopsARunWhoseCommandTheGateDidNotAnswer(t *testing.T) {
	installFakeGate(t)
	for _, stream := range []string{"noevent", "subnoevent", "otherhook", "otherjson", "hookfailed", "wrongtool", "oneofparall", "banked", "early", "guardonly", "rewritten", "duplicate", "otherexe", "nonzero", "afterresult", "monitor", "repl", "workflow", "skill"} {
		t.Run(stream, func(t *testing.T) {
			started := time.Now()
			res, _, _ := runGatedClaude(t, stream, Options{Mode: ModeYolo, Gate: true})
			if res.Status != StatusError || !strings.Contains(res.Error, "ungated") {
				t.Fatalf("result = %s %q, want the run stopped", res.Status, res.Error)
			}
			if took := time.Since(started); took > 20*time.Second {
				t.Fatalf("the run was not stopped: it took %s", took)
			}
		})
	}
}

func TestGatedClaudeRunThatEndsWithAnUnansweredCallFails(t *testing.T) {
	installFakeGate(t)
	res, _, _ := runGatedClaude(t, "ended", Options{Mode: ModeYolo, Gate: true})
	if res.Status != StatusError || !strings.Contains(res.Error, "without the gate's answer") {
		t.Fatalf("result = %s %q", res.Status, res.Error)
	}
}

func TestGatedClaudeIsRefusedOutsideYolo(t *testing.T) {
	installFakeGate(t)
	for _, mode := range []Mode{ModeReadOnly, ModeAsk, ModeAuto, ""} {
		res, _, rec := runGatedClaude(t, "routed", Options{Mode: mode, Gate: true})
		if res.Status != StatusError || !strings.Contains(res.Error, "--mode yolo") {
			t.Errorf("mode %q: result = %s %q, want refused", mode, res.Status, res.Error)
		}
		if rec != nil {
			t.Errorf("mode %q: the CLI started", mode)
		}
	}
}

func TestGatedClaudeDoesNotStartWithoutTheGate(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	res, _, rec := runGatedClaude(t, "routed", Options{Mode: ModeYolo, Gate: true})
	if res.Status != StatusError || !strings.Contains(res.Error, "adanext-exec is not in PATH") {
		t.Fatalf("result = %s %q", res.Status, res.Error)
	}
	if rec != nil {
		t.Fatal("the CLI started without the gate")
	}
}

func TestGatedClaudeDoesNotStartWithAnOlderGate(t *testing.T) {
	dir := installFakeGate(t)
	if err := os.WriteFile(filepath.Join(dir, "old"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	res, _, rec := runGatedClaude(t, "routed", Options{Mode: ModeYolo, Gate: true})
	if res.Status != StatusError || !strings.Contains(res.Error, "handshake") {
		t.Fatalf("result = %s %q", res.Status, res.Error)
	}
	if rec != nil {
		t.Fatal("the CLI started with an older gate")
	}
}

func TestGatedClaudeDoesNotStartWithAGateThatHasNoHook(t *testing.T) {
	dir := installFakeGate(t)
	if err := os.WriteFile(filepath.Join(dir, "nohook"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	res, _, rec := runGatedClaude(t, "routed", Options{Mode: ModeYolo, Gate: true})
	if res.Status != StatusError || !strings.Contains(res.Error, "has no Claude Code hook") {
		t.Fatalf("result = %s %q", res.Status, res.Error)
	}
	if rec != nil {
		t.Fatal("the CLI started with a gate that has no hook")
	}
}

func TestReadGateAnswerKnowsTheGatesOwnRewrite(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a'b c")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	file := func(name string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, nil, 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	gate, curly, other := file("adanext-exec"), file("ad\u2019anext-exec"), file("other")
	answer := func(decision, command string) string {
		b, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{
			"permissionDecision": decision, "updatedInput": map[string]string{"command": command},
		}})
		return string(b)
	}
	bash := func(exe string) string {
		return tools.PosixQuote(filepath.ToSlash(exe)) + ` 'run' '--hook-build' 'ab12' '--shell' 'bash' '--approved=rule' '--agent' 'claude-code' '--session' 's-1' '--script-b64' 'ZWNobw'`
	}
	ps := func(exe string) string {
		quoted := strings.NewReplacer(`'`, `''`, "\u2019", "\u2019\u2019").Replace(exe)
		return `& '` + quoted + `' 'run' '--hook-build' 'ab' '--shell' 'pwsh' '--script-b64' 'ZWNobw'`
	}
	refusal := func(reason string) string {
		return fmt.Sprintf(`{"hookSpecificOutput":{"permissionDecision":"deny","permissionDecisionReason":%q}}`, reason)
	}
	for _, tc := range []struct {
		name, tool, gate, stdout string
		want                     gateAnswerRead
	}{
		{"the gate's rewrite", "Bash", gate, answer("allow", bash(gate)), gateAnswerRead{answered: true, command: "echo"}},
		{"an ask", "Bash", gate, answer("ask", bash(gate)), gateAnswerRead{answered: true, command: "echo"}},
		{"powershell", "PowerShell", gate, answer("allow", ps(gate)), gateAnswerRead{answered: true, command: "echo"}},
		{"a doubled typographic quote in the path", "PowerShell", curly, answer("allow", ps(curly)), gateAnswerRead{answered: true, command: "echo"}},
		{"another executable", "Bash", gate, answer("allow", bash(other)), gateAnswerRead{rewritten: true}},
		{"another executable on powershell", "PowerShell", gate, answer("allow", ps(other)), gateAnswerRead{rewritten: true}},
		{"a missing executable", "Bash", gate, answer("allow", bash(filepath.Join(dir, "none"))), gateAnswerRead{rewritten: true}},
		{"commands after it", "Bash", gate, answer("allow", bash(gate)+`; evil`), gateAnswerRead{rewritten: true}},
		{"the words in another command", "Bash", gate, answer("allow", `echo 'run' '--hook-build' 'a' '--shell' 'bash' '--script-b64' 'aa'; evil`), gateAnswerRead{rewritten: true}},
		{"a typographic quote in powershell", "PowerShell", gate, answer("allow", "& 'a\u2019; evil #' 'run' '--hook-build' 'x' '--shell' 'pwsh' '--script-b64' 'aa'"), gateAnswerRead{rewritten: true}},
		{"the bash form on powershell", "PowerShell", gate, answer("allow", bash(gate)), gateAnswerRead{rewritten: true}},
		{"a bash quote in the powershell form", "PowerShell", gate, answer("allow", `& 'a'\''; evil #'\''b' 'run' '--hook-build' 'x' '--shell' 'pwsh' '--script-b64' 'aa'`), gateAnswerRead{rewritten: true}},
		{"another hook's rewrite", "Bash", gate, answer("allow", `echo mine`), gateAnswerRead{rewritten: true}},
		{"the gate's refusal", "Bash", gate, refusal("adanext-exec: refused"), gateAnswerRead{answered: true, denied: true}},
		{"the guard's refusal", "Bash", gate, refusal("blocked by the hrn guard (x)"), gateAnswerRead{denied: true}},
		{"another hook's allow", "Bash", gate, `{"hookSpecificOutput":{"permissionDecision":"allow"}}`, gateAnswerRead{}},
		{"nothing", "Bash", gate, ``, gateAnswerRead{}},
		{"not json", "Bash", gate, `adanext-exec: oops`, gateAnswerRead{}},
	} {
		if got := readGateAnswer(tc.tool, tc.stdout, tc.gate); got != tc.want {
			t.Errorf("%s: got %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func TestGateHookCommandQuotesTheGatePath(t *testing.T) {
	data, err := claudeGateSettings(`C:/a'b c/adanext-exec.exe`, false)
	if err != nil {
		t.Fatal(err)
	}
	var s gateSettings
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	got := s.Hooks["PreToolUse"][0].Hooks[0].Command
	if want := `'C:/a'\''b c/adanext-exec.exe' hook claude-code || exit 2`; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
	self, _ := os.Executable()
	guard := s.Hooks["PreToolUse"][0].Hooks[1].Command
	if want := "'" + filepath.ToSlash(self) + "' guard-hook || exit 2"; guard != want {
		t.Fatalf("got %s, want %s", guard, want)
	}
}

func TestGuardHookRuleRefusesWhatTheDenyRulesRefused(t *testing.T) {
	for _, command := range []string{
		"gh repo edit --description x", "gh api /x -X POST", "gh api /x --method POST", "env -u SOME_VAR ls",
		"git -c a.pushurl=b fetch", "unset GH_TOKEN", "cd x && gh repo edit --homepage y", "git status; env -u A b",
		"git push", "git push origin x",
		"timeout 5 gh repo edit --description x", "A=1 gh repo edit --description x", "A=1 B=2 nohup env -u X y",
		"echo $(gh api /x -X POST)", "echo `env -u X y`", "(env -u X y)", "{ gh repo edit x; }",
		"if true; then gh repo edit x; fi", "while true; do env -u X y; done", "nice -n 5 timeout 9 unset GH_TOKEN",
		"gh api repos/{owner}/{repo}/issues -X POST", "gh api ${URL} -X POST",
		"gh api repos/o/r/issues -f 'title=a|b' -X POST", "gh api repos/o/r/issues -f 'title=a;b' --method POST",
		`gh api repos/o/r/issues -f "title=a&b" -X POST`,
		`gh api "repos/$(gh repo view --json nameWithOwner -q .nameWithOwner)/issues" --method POST`,
		"time gh repo edit x", "stdbuf -o0 gh repo edit x", "command gh repo edit x", "builtin unset GH_TOKEN",
		"noglob gh repo edit x", "xargs gh repo edit x", "if a; then b; elif c; then gh repo edit x; fi",
		"if a; then b; else gh repo edit x; fi", "until a; do env -u X y; done", "! gh repo edit x",
	} {
		if GuardHookRule(command) == "" {
			t.Errorf("%q was not refused", command)
		}
	}
	for _, command := range []string{"git status", "gh pr list", "echo gh repo edit x", "go test ./...", "ls -la | grep env", "timeout 5 go test ./...", "echo ${A}"} {
		if rule := GuardHookRule(command); rule != "" {
			t.Errorf("%q was refused by %q", command, rule)
		}
	}
}

func TestClaudeWithoutTheGateIsStartedAndReadAsBefore(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for _, stream := range []string{"unhooked", "routed"} {
		res, _, rec := runGatedClaude(t, stream, Options{Mode: ModeYolo})
		if res.Status != StatusOK {
			t.Fatalf("%s: a run without the gate was judged by it: %s %q", stream, res.Status, res.Error)
		}
		if rec == nil {
			t.Fatal("the CLI did not start")
		}
		for _, flag := range []string{"--settings", "--include-hook-events", "--tools"} {
			if slices.Contains(rec.Args, flag) {
				t.Errorf("%s in a run without the gate: %v", flag, rec.Args)
			}
		}
	}
}
