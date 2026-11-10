package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cristiangirlea/harness/internal/envpolicy"
	"github.com/cristiangirlea/harness/internal/gitenv"
	"github.com/cristiangirlea/harness/internal/nowindow"
	"github.com/cristiangirlea/harness/internal/tools"
)

const gateUnrouted = "gate: stopped: "

// GateStopped starts the error of a gated run hrn stopped because a command may have run ungated.
const GateStopped = gateUnrouted

// An allow list, since new code-running tools appear; Monitor, REPL, Workflow and Skill run code the hook does not rewrite.
const claudeGateTools = "Bash,PowerShell,Read,Edit,Write,NotebookEdit,Glob,Grep,LSP,WebFetch,WebSearch,Agent," +
	"TodoWrite,TaskCreate,TaskGet,TaskList,TaskUpdate,TaskOutput,TaskStop,ToolSearch"

var (
	claudeHookedTools   = []string{"Bash", "PowerShell"}
	claudeUnroutedTools = []string{"Monitor", "REPL", "Workflow", "Skill"}
)

// claudeGateRoute starts claude with the gate's hook for one run. The hook's allow skips claude's own
// mode check (a command the gate allows ran in plan mode), so only yolo, which has none, keeps its meaning.
func claudeGateRoute(ctx context.Context, t Task, gate, policy string) ([]string, func(), error) {
	if t.Mode != ModeYolo {
		return nil, nil, fmt.Errorf("gate: on the claude backend it needs --mode yolo: the hook's allow skips Claude Code's own permission mode, so plan, default and acceptEdits would run what they should not (mode %q)", t.Mode)
	}
	if err := checkClaudeGateHook(ctx, t, gate, policy); err != nil {
		return nil, nil, err
	}
	settings, err := claudeGateSettings(gate, policy, t.AllowDangerous)
	if err != nil {
		return nil, nil, err
	}
	// A file, since JSON on a command line would not survive claude's cmd.exe shim.
	f, err := os.CreateTemp("", "hrn-gate-*.json")
	if err != nil {
		return nil, nil, fmt.Errorf("gate: %w", err)
	}
	done := func() { _ = os.Remove(f.Name()) }
	_, err = f.Write(settings)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		done()
		return nil, nil, fmt.Errorf("gate: %w", err)
	}
	args := []string{"--settings", f.Name(), "--include-hook-events"}
	if !t.NoTools {
		args = append(args, "--tools", claudeGateTools)
	}
	return args, done, nil
}

// checkClaudeGateHook asks the hook once, since a binary from before the hook answers the protocol
// handshake and its run would only stop at the first command, with the plan already spent.
func checkClaudeGateHook(ctx context.Context, t Task, gate, policy string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, gate, "hook", "claude-code", "--policy", policy)
	cmd.Dir = t.Dir
	cmd.Env = envpolicy.Filter(gitenv.WithoutRepoVars(append(os.Environ(), t.ChildEnv...)))
	cmd.Stdin = strings.NewReader(`{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"git status","timeout":1000}}`)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = 5 * time.Second
	nowindow.Set(cmd)
	err := cmd.Run()
	a := readGateAnswer("Bash", stdout.String(), gate, policy)
	switch {
	case err != nil || !a.answered:
		return fmt.Errorf("gate: %s has no Claude Code hook this hrn can use (%v: %s); it may be older than this hrn", gate, err, strings.TrimSpace(stderr.String()))
	// A refusal of the probe shows no rewrite, so it says nothing about the timeout.
	case !a.denied && !a.timed:
		return fmt.Errorf("gate: the Claude Code hook of %s does not carry a call's timeout; it is older than this hrn", gate)
	}
	return nil
}

// claudeGateSettings holds hooks and nothing else, since claude merges --settings over the user's own.
// hrn's guard is a hook of its own because a rewritten command no longer matches its deny rules.
func claudeGateSettings(gate, policy string, allowDangerous bool) ([]byte, error) {
	type hook struct {
		Type    string `json:"type"`
		Command string `json:"command"`
	}
	type matcher struct {
		Matcher string `json:"matcher"`
		Hooks   []hook `json:"hooks"`
	}
	// Claude runs a command whose hook fails with any code but 2.
	command := func(exe, args string) hook {
		return hook{"command", tools.PosixQuote(filepath.ToSlash(exe)) + " " + args + " || exit 2"}
	}
	hooks := []hook{command(gate, "hook claude-code --policy "+tools.PosixQuote(filepath.ToSlash(policy)))}
	if !allowDangerous {
		self, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("gate: find hrn for the guard's hook: %w", err)
		}
		hooks = append(hooks, command(self, "guard-hook"))
	}
	return json.Marshal(map[string]map[string][]matcher{
		"hooks": {"PreToolUse": {{Matcher: strings.Join(claudeHookedTools, "|"), Hooks: hooks}}},
	})
}

// claudeGateCheck holds command calls to the gate's answers, since a hook that did not load,
// failed or timed out lets a command run ungated without a word. Hook events carry no tool id, so
// answers are counted per command text, and a refusal, which names no command, per tool.
type claudeGateCheck struct {
	gate, policy             string
	pending                  map[string]string
	keys                     []string
	called, answered, closed map[string]int
	toolCalls, refused       map[string]int
}

func gateCallKey(tool, command string) string { return tool + "\x00" + strings.TrimSpace(command) }

// newClaudeGateParse reads the stream as usual and also stops a run on a command the gate did not answer.
func newClaudeGateParse(gate, policy string) func([]byte, func(Event), *Result) bool {
	parse := newClaudeParse()
	g := &claudeGateCheck{gate: gate, policy: policy, pending: map[string]string{}, called: map[string]int{}, answered: map[string]int{},
		closed: map[string]int{}, toolCalls: map[string]int{}, refused: map[string]int{}}
	return func(line []byte, emit func(Event), res *Result) bool {
		parsed := parse(line, emit, res)
		if line == nil {
			if msg := g.unanswered(); msg != "" {
				emit(Event{Time: time.Now(), Kind: EventError, Text: msg})
				if res.Status == "" || res.Status == StatusOK {
					res.Status, res.Error = StatusError, msg
				}
			}
			return parsed
		}
		// Replaces any earlier error, since the run goes on after a result line while a subagent works.
		if msg := g.see(line); msg != "" {
			res.Error = msg
		}
		return parsed
	}
}

func (g *claudeGateCheck) see(line []byte) string {
	var l struct {
		Type     string `json:"type"`
		Subtype  string `json:"subtype"`
		HookName string `json:"hook_name"`
		ExitCode int    `json:"exit_code"`
		Stdout   string `json:"stdout"`
		Message  struct {
			Content []struct {
				Type      string `json:"type"`
				ID        string `json:"id"`
				Name      string `json:"name"`
				ToolUseID string `json:"tool_use_id"`
				Input     struct {
					Command string `json:"command"`
				} `json:"input"`
			} `json:"content"`
		} `json:"message"`
	}
	var mistyped *json.UnmarshalTypeError
	if err := json.Unmarshal(line, &l); err != nil && !errors.As(err, &mistyped) {
		return ""
	}
	if l.Type == "system" && l.Subtype == "hook_response" {
		tool, ok := strings.CutPrefix(l.HookName, "PreToolUse:")
		if !ok || !slices.Contains(claudeHookedTools, tool) || l.ExitCode != 0 {
			return ""
		}
		a := readGateAnswer(tool, l.Stdout, g.gate, g.policy)
		if a.rewritten {
			// When hooks disagree the last to finish wins, so the command may not be the gate's.
			return fmt.Sprintf("%sanother hook rewrote a %s call, so its command may have run ungated", gateUnrouted, tool)
		}
		// One answer per call: a surplus would cover a later call that had none.
		key := gateCallKey(tool, a.command)
		switch {
		case a.answered && a.denied && g.refused[tool] < g.toolCalls[tool]:
			g.refused[tool]++
		case a.answered && !a.denied && g.answered[key] < g.called[key]:
			g.answered[key]++
		}
		return ""
	}
	for _, c := range l.Message.Content {
		switch {
		case c.Type == "tool_use" && slices.Contains(claudeUnroutedTools, c.Name):
			return gateUnroutedMessage(c.Name)
		case c.Type == "tool_use" && slices.Contains(claudeHookedTools, c.Name):
			if _, seen := g.pending[c.ID]; !seen {
				key := gateCallKey(c.Name, c.Input.Command)
				g.pending[c.ID] = key
				if g.called[key]++; g.called[key] == 1 {
					g.keys = append(g.keys, key)
				}
				g.toolCalls[c.Name]++
			}
		case c.Type == "tool_result":
			key := g.pending[c.ToolUseID]
			if key == "" {
				continue
			}
			g.pending[c.ToolUseID] = ""
			g.closed[key]++
			if tool, _, _ := strings.Cut(key, "\x00"); g.closed[key] > g.answered[key] {
				if g.refused[tool] == 0 {
					return gateUnroutedMessage(tool)
				}
				g.refused[tool]--
				g.answered[key]++
			}
		}
	}
	return ""
}

func (g *claudeGateCheck) unanswered() string {
	for _, key := range g.keys {
		tool, _, _ := strings.Cut(key, "\x00")
		need := g.called[key] - g.answered[key]
		if need > g.refused[tool] {
			return gateUnroutedMessage(tool)
		}
		g.refused[tool] -= need
	}
	return ""
}

func gateUnroutedMessage(tool string) string {
	return fmt.Sprintf("%sa %s call came without the gate's answer, so its command may have run ungated", gateUnrouted, tool)
}

// gateRewrite is the whole command line the gate's hook writes (docs/protocol.md in adanext-exec).
// PowerShell also ends a string on the typographic single quotes.
var gateRewrite = map[string]*regexp.Regexp{
	"Bash":       regexp.MustCompile(`^'((?:[^']|'\\'')+)' 'run' '--hook-build' '[^']+' '--shell' 'bash'(?: '(?:[^']|'\\'')*')* '--script-b64' '([A-Za-z0-9_-]+)'$`),
	"PowerShell": regexp.MustCompile(`^& '((?:[^'\x{2018}-\x{201b}]|''|\x{2018}\x{2018}|\x{2019}\x{2019}|\x{201a}\x{201a}|\x{201b}\x{201b})+)' 'run' '--hook-build' '[^'\x{2018}-\x{201b}]+' '--shell' '(?:pwsh|powershell)'(?: '(?:[^'\x{2018}-\x{201b}]|''|\x{2018}\x{2018}|\x{2019}\x{2019}|\x{201a}\x{201a}|\x{201b}\x{201b})*')* '--script-b64' '([A-Za-z0-9_-]+)'$`),
}

var gateRewritePolicy = map[string]*regexp.Regexp{
	"Bash":       regexp.MustCompile(` '--policy' '((?:[^']|'\\'')*)'`),
	"PowerShell": regexp.MustCompile(` '--policy' '((?:[^'\x{2018}-\x{201b}]|''|\x{2018}\x{2018}|\x{2019}\x{2019}|\x{201a}\x{201a}|\x{201b}\x{201b})*)'`),
}

// The gate's flag parser takes the last of several, in either dash form or with =, so one is all a rewrite may carry.
var gateRewritePolicyFlag = regexp.MustCompile(` '-{1,2}policy(?:=|')`)

var gateRewriteUnquote = map[string]*strings.Replacer{
	"Bash":       strings.NewReplacer(`'\''`, `'`),
	"PowerShell": strings.NewReplacer(`''`, `'`, "\u2018\u2018", "\u2018", "\u2019\u2019", "\u2019", "\u201a\u201a", "\u201a", "\u201b\u201b", "\u201b"),
}

// gateAnswerRead is what one hook's answer says: the gate's own, with the command an allow carries,
// or another hook's rewrite of the command.
type gateAnswerRead struct {
	answered, denied, rewritten, timed bool
	command                            string
}

func readGateAnswer(tool, stdout, gate, policy string) gateAnswerRead {
	var a struct {
		Specific struct {
			Decision string `json:"permissionDecision"`
			Reason   string `json:"permissionDecisionReason"`
			Input    struct {
				Command string `json:"command"`
			} `json:"updatedInput"`
		} `json:"hookSpecificOutput"`
	}
	if json.Unmarshal([]byte(stdout), &a) != nil {
		return gateAnswerRead{}
	}
	if a.Specific.Decision == "deny" {
		return gateAnswerRead{answered: strings.HasPrefix(a.Specific.Reason, tools.GateName+": "), denied: true}
	}
	if a.Specific.Input.Command == "" {
		return gateAnswerRead{}
	}
	m := gateRewrite[tool].FindStringSubmatch(a.Specific.Input.Command)
	if m == nil || !sameFile(gateRewriteUnquote[tool].Replace(m[1]), gate) {
		return gateAnswerRead{rewritten: true}
	}
	// The user's own gate hook can win the race and judge by another policy.
	p := gateRewritePolicy[tool].FindStringSubmatch(m[0])
	if p == nil || len(gateRewritePolicyFlag.FindAllString(m[0], 2)) != 1 || !sameFile(gateRewriteUnquote[tool].Replace(p[1]), policy) {
		return gateAnswerRead{rewritten: true}
	}
	command, err := base64.RawURLEncoding.DecodeString(m[2])
	if err != nil {
		return gateAnswerRead{rewritten: true}
	}
	return gateAnswerRead{answered: a.Specific.Decision == "allow" || a.Specific.Decision == "ask", command: string(command),
		timed: strings.Contains(m[0], " '--timeout' ")}
}

func sameFile(a, b string) bool {
	x, errX := os.Stat(a)
	y, errY := os.Stat(b)
	return errX == nil && errY == nil && os.SameFile(x, y)
}

var (
	guardRuleOnce     = sync.OnceValue(compileGuardCommands)
	guardRuleKeywords = []string{"if", "then", "elif", "else", "do", "while", "until", "!"}
	guardRuleWrappers = []string{"timeout", "time", "nice", "nohup", "stdbuf", "command", "builtin", "noglob", "xargs"}
	// Tried with the whole line: a split can cut a command from its own later words (gh api repos/{owner} -X POST).
	commandBreaks   = regexp.MustCompile(`&&|\|\||[;|&\n]`)
	commandBlocks   = regexp.MustCompile(`&&|\|\||\$\(|[;|&\n(){}` + "`" + `]`)
	shellAssignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(?:\+)?=`)
)

func compileGuardCommands() map[string]*regexp.Regexp {
	rules := make(map[string]*regexp.Regexp, len(guardCommands))
	for _, c := range guardCommands {
		pattern, trailing := strings.CutSuffix(c, " *")
		text := strings.ReplaceAll(regexp.QuoteMeta(pattern), `\*`, `.*`)
		if trailing {
			text += `(?:\s.*)?`
		}
		rules[c] = regexp.MustCompile(`(?s)^` + text + `$`)
	}
	return rules
}

// GuardHookRule is the rule that refuses a command, for "hrn guard-hook": hrn's guard, then the
// deny rules claude no longer applies to a rewritten command, tried on each command of the line
// past assignments, keywords and wrappers. It reads text, not a shell.
func GuardHookRule(command string) string {
	if rule := tools.Guard(command); rule != "" {
		return rule
	}
	parts := []string{command}
	for _, breaks := range []*regexp.Regexp{commandBreaks, commandBlocks} {
		parts = append(parts, strings.Split(breaks.ReplaceAllString(command, "\n"), "\n")...)
	}
	for _, part := range parts {
		for _, text := range guardRuleTexts(part) {
			for _, c := range guardCommands {
				if guardRuleOnce()[c].MatchString(text) {
					return c
				}
			}
		}
	}
	return ""
}

// guardRuleTexts are the readings of one command the deny rules are tried on.
func guardRuleTexts(part string) []string {
	words := strings.Fields(part)
	for len(words) > 0 && (shellAssignment.MatchString(words[0]) || slices.Contains(guardRuleKeywords, words[0])) {
		words = words[1:]
	}
	if len(words) == 0 {
		return nil
	}
	joined := strings.Join(words, " ")
	texts := []string{joined}
	if slices.Contains(guardRuleWrappers, words[0]) {
		// A wrapper's own options cannot be told from its command, so every word is a start.
		start := 0
		for _, word := range words[:len(words)-1] {
			start += len(word) + 1
			texts = append(texts, joined[start:])
		}
	}
	return texts
}
