package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/cristiangirlea/harness/internal/gitenv"
	"github.com/cristiangirlea/harness/internal/retrieval"
	"github.com/cristiangirlea/harness/internal/tools"
)

// CLIBackend runs an installed CLI using its own login.
type CLIBackend struct {
	name string
	// PATH takes precedence over bundled installations.
	candidates func() []string
	// promptFile replaces stdin for attachment-based CLIs.
	args        func(t Task, promptFile string) []string
	promptStdin bool
	// env keeps values off command lines; the guard still filters them.
	env         func(t Task) ([]string, error)
	mcpWarnings func(Task) []string
	// Some CLIs require structured stdin even in print mode.
	encodePrompt func(string) string
	// A nil parser treats stdout as plain text.
	parse func(line []byte, emit func(Event), res *Result) bool
	// newParse isolates parser state per run.
	newParse func() func(line []byte, emit func(Event), res *Result) bool
	// Stop promptly when stream events declare a terminal condition.
	stopWhen func(*Result) bool
	// Single-JSON CLIs need the complete stdout document.
	finalize func(stdout []byte, emit func(Event), res *Result)
	// Resolve pending errors after clean exit without duplicating events.
	end func(emit func(Event), res *Result, errored bool)
	// exited reads what a CLI records outside its stream, once the process is gone.
	exited func(t Task, started time.Time, res *Result)
	// forks requires session argument and result support.
	forks bool
	// In-place resume must not be advertised as a session copy.
	resumesInPlace bool
	// Pass login secrets only to their owning CLI.
	loginEnv []string
	// ownEnv passes the allowlist for this CLI only: where it keeps its state
	// and which account it signs in with. The guard still applies.
	ownEnv []string
	// meteredEnv never reaches the CLI, not even with --allow-dangerous and --inherit-env: it would bill per token.
	meteredEnv []string
	// fileRules requires native CLI secret-file protection.
	fileRules bool
	// Advertise effort only when the adapter enforces it.
	mapsEffort bool
	// Advertise only explicitly mapped capabilities.
	mapsModes    []Mode
	mapsModel    bool
	mapsMaxTurns bool
	mapsMCP      bool
	// The exec start protocol has no lean field.
	mapsLean bool
	// Compaction support is separate from exec capabilities.
	mapsCompact bool
	// System-prompt and skill support require explicit argument mappings.
	mapsSystemPrompt bool
	mapsNoSkills     bool
	// Only subscription-backed CLIs report plan limits.
	limits bool
}

func (c *CLIBackend) Name() string { return c.name }

// AppliesLean reports whether this CLI honours Options.Lean (see AppliesLean).
func (c *CLIBackend) AppliesLean() bool { return c.mapsLean }

// AppliesSystemPrompt reports whether this CLI honours Options.AppendSystemPrompt.
func (c *CLIBackend) AppliesSystemPrompt() bool { return c.mapsSystemPrompt }

// AppliesNoSkills reports whether this CLI honours Options.NoSkills.
func (c *CLIBackend) AppliesNoSkills() bool { return c.mapsNoSkills }

func (c *CLIBackend) Capabilities() Capabilities {
	caps := Capabilities{Modes: append([]Mode{}, c.mapsModes...), Efforts: []string{}, Sessions: "none", Options: []string{}}
	if c.mapsModel {
		caps.Options = append(caps.Options, "model")
	}
	if len(c.mapsModes) > 0 {
		caps.Options = append(caps.Options, "mode")
	}
	if c.mapsEffort {
		caps.Efforts = append(caps.Efforts, EffortLevels...)
		caps.Options = append(caps.Options, "effort")
	}
	caps.Options = append(caps.Options, "timeout")
	if c.mapsMaxTurns {
		caps.Options = append(caps.Options, "max_turns")
	}
	if c.mapsMCP {
		caps.Options = append(caps.Options, "mcp")
	}
	if c.forks {
		caps.Sessions = "fork"
		if c.resumesInPlace {
			caps.Sessions = "resume"
		}
	}
	return caps
}

// ForksSessions reports support, including in-place resume.
func (c *CLIBackend) ForksSessions() bool { return c.forks }

var sessionID = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._-]*$`)

var modelID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+\[\]-]{0,127}$`)

// ValidModel rejects flags and unsafe cmd.exe syntax.
func ValidModel(model string) bool { return modelID.MatchString(model) }

// ForksSessions reports whether b honours Task.ForkSession.
func ForksSessions(b Backend) bool {
	f, ok := b.(interface{ ForksSessions() bool })
	return ok && f.ForksSessions()
}

func (c *CLIBackend) Detect() (string, error) {
	if p, err := exec.LookPath(c.name); err == nil {
		return p, nil
	}
	if c.candidates != nil {
		for _, p := range c.candidates() {
			// Relative candidates could select workspace-controlled binaries.
			if !filepath.IsAbs(p) {
				continue
			}
			if _, err := os.Stat(p); err == nil {
				return p, nil
			}
		}
	}
	return "", fmt.Errorf("%s not found on PATH or in known install locations", c.name)
}

func (c *CLIBackend) Run(ctx context.Context, task Task, emit func(Event)) Result {
	res := c.run(ctx, task, emit)
	if c.limits && res.Status == StatusError {
		if limited, resets := usageLimit(res.Error, time.Now()); limited {
			res.Status, res.LimitResets = StatusLimited, resets
		}
	}
	return res
}

func (c *CLIBackend) run(ctx context.Context, task Task, emit func(Event)) Result {
	started := time.Now()
	ctx, cancel := withTimeout(ctx, task)
	defer cancel()
	res := Result{Backend: c.name, Model: task.Model}
	fail := func(status Status, err error) Result {
		res.Status = status
		res.Error = err.Error()
		emit(Event{Time: time.Now(), Kind: EventError, Text: err.Error()})
		return finish(&res, started)
	}
	if task.ForkSession != "" && !c.forks {
		return fail(StatusError, fmt.Errorf("the %s backend cannot resume a session", c.name))
	}
	// Disk-sourced session IDs must not become CLI flags.
	if task.ForkSession != "" && !sessionID.MatchString(task.ForkSession) {
		return fail(StatusError, fmt.Errorf("session id %q is not one the %s CLI issues", task.ForkSession, c.name))
	}
	if task.Model != "" && !ValidModel(task.Model) {
		return fail(StatusError, fmt.Errorf("model %q is not a model id", task.Model))
	}

	bin, err := c.Detect()
	if err != nil {
		return fail(StatusError, err)
	}
	// cmd.exe treats free-text metacharacters as shell syntax.
	if task.AppendSystemPrompt != "" && c.mapsSystemPrompt && isBatchFile(bin) {
		return fail(StatusError, fmt.Errorf("append_system_prompt cannot be passed safely through %s; install the native %s CLI", filepath.Base(bin), c.name))
	}
	var promptFile string
	if !c.promptStdin {
		f, err := os.CreateTemp("", "hrn-prompt-*.md")
		if err != nil {
			return fail(StatusError, err)
		}
		promptFile = f.Name()
		defer os.Remove(promptFile)
		_, err = f.WriteString(task.Prompt)
		f.Close()
		if err != nil {
			return fail(StatusError, err)
		}
	}

	cmd := exec.CommandContext(ctx, bin, c.args(task, promptFile)...)
	cmd.Dir = task.Dir
	// The api backend's key is for hrn itself; a CLI that found it would bill
	// per token instead of logging in on its own.
	cmd.Env = withoutEnv(agentEnv(os.Environ()), "ANTHROPIC_API_KEY")
	// Named from the operator's environment, before the allowlist would hide them.
	var metered []string
	cmd.Env, metered = withoutEnvNames(cmd.Env, c.meteredEnv)
	envLine := "environment: inherited (--inherit-env)"
	var withheld []string
	if !task.InheritEnv {
		var dropped int
		cmd.Env, dropped, withheld = sanitizedEnv(cmd.Env, runtime.GOOS, slices.Concat(c.loginEnv, c.ownEnv)...)
		envLine = fmt.Sprintf("environment: sanitized, %d variables dropped (--inherit-env keeps them)", dropped)
	}
	// Variables hrn sets for the CLI itself go on after the allowlist; the
	// guard below still filters them.
	if c.env != nil {
		extra, err := c.env(task)
		if err != nil {
			return fail(StatusError, err)
		}
		// The key stays out even when an MCP server's env names it.
		cmd.Env = withoutEnv(append(cmd.Env, extra...), "ANTHROPIC_API_KEY")
	}
	cmd.Env = append(cmd.Env, task.ChildEnv...)
	cmd.Env, _ = withoutEnvNames(cmd.Env, c.meteredEnv)
	guarded := !task.AllowDangerous
	if guarded {
		guardDir, err := os.MkdirTemp("", "hrn-guard-*")
		if err != nil {
			return fail(StatusError, err)
		}
		defer os.RemoveAll(guardDir)
		var guardWithheld []string
		cmd.Env, guardWithheld = tools.GuardEnv(cmd.Env, guardDir, c.loginEnv...)
		withheld = slices.Compact(slices.Sorted(slices.Values(append(withheld, guardWithheld...))))
	}
	if c.promptStdin {
		prompt := task.Prompt
		if c.encodePrompt != nil {
			prompt = c.encodePrompt(prompt)
		}
		cmd.Stdin = strings.NewReader(prompt)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	cmd.WaitDelay = 5 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fail(StatusError, err)
	}
	emit(Event{Time: time.Now(), Kind: EventInfo, Text: fmt.Sprintf("%s %s", filepath.Base(bin), strings.Join(shownArgs(cmd.Args[1:]), " "))})
	if limits := unenforced(task, c); len(limits) > 0 {
		emit(Event{Time: time.Now(), Kind: EventInfo,
			Text: fmt.Sprintf("%s: not enforced by the %s backend", strings.Join(limits, ", "), c.name)})
	}
	if c.mcpWarnings != nil {
		for _, warning := range c.mcpWarnings(task) {
			emit(Event{Time: time.Now(), Kind: EventInfo, Text: warning})
		}
	}
	emit(Event{Time: time.Now(), Kind: EventInfo, Text: envLine})
	if len(metered) > 0 {
		emit(Event{Time: time.Now(), Kind: EventInfo,
			Text: fmt.Sprintf("environment: %s withheld so %s bills its plan login, not per token", strings.Join(metered, ", "), c.name)})
	}
	if guarded {
		emit(Event{Time: time.Now(), Kind: EventInfo, Text: c.guardLine(withheld, task.InheritEnv, len(task.MCP) > 0)})
	} else if task.InheritEnv {
		emit(Event{Time: time.Now(), Kind: EventInfo,
			Text: fmt.Sprintf("guard: lifted by --allow-dangerous; the %s CLI can push, merge and deploy with the operator's credentials", c.name)})
	} else {
		// The tokens the allowlist dropped stay dropped: only the logins
		// tools keep on disk and in the system's store are within reach.
		text := fmt.Sprintf("guard: lifted by --allow-dangerous; the %s CLI can push, merge and deploy with the logins kept on disk and in the system's store", c.name)
		if len(withheld) > 0 {
			text += fmt.Sprintf("; %d secret-looking variables still left out; --inherit-env passes them", len(withheld))
		}
		emit(Event{Time: time.Now(), Kind: EventInfo, Text: text})
	}
	tree, err := tools.StartTree(cmd)
	if err != nil {
		if ctx.Err() != nil {
			return fail(stopStatus(ctx), ctx.Err())
		}
		return fail(StatusError, err)
	}
	defer tree.Close()
	emitCLITreeStartInfo(tree, emit)
	// A surviving descendant can hold stdout open; bound the read on cancel.
	readDone := make(chan struct{})
	defer close(readDone)
	go func() {
		select {
		case <-readDone:
			return
		case <-ctx.Done():
		}
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		select {
		case <-readDone:
		case <-timer.C:
			_ = stdout.Close()
		}
	}()

	var all []byte
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	var plain strings.Builder
	var session string
	parse := c.parse
	if c.newParse != nil {
		parse = c.newParse()
	}
	var errored bool
	var lastErr string
	parseEmit := func(e Event) {
		if e.Kind == EventError {
			errored, lastErr = true, e.Text
		}
		emit(e)
	}
	for sc.Scan() {
		line := sc.Bytes()
		if c.finalize != nil {
			all = append(append(all, line...), '\n')
			continue
		}
		if parse != nil {
			parsed := parse(line, parseEmit, &res)
			if c.stopWhen != nil && c.stopWhen(&res) {
				cancel()
				stopErr := stopCLITree(tree, errors.New(res.Error))
				_ = stdout.Close()
				_ = cmd.Wait()
				return fail(StatusError, stopErr)
			}
			if res.Session != "" && res.Session != session {
				session = res.Session
				emit(Event{Time: time.Now(), Kind: EventSession, Text: session})
			}
			if parsed {
				continue
			}
		}
		text := string(line) + "\n"
		plain.WriteString(text)
		emit(Event{Kind: EventText, Text: text, Partial: true})
	}
	if c.newParse != nil && c.finalize == nil {
		parse(nil, parseEmit, &res) // a parser holding a line back lets it go
	}
	_, _ = io.Copy(io.Discard, stdout)
	waitErr := cmd.Wait()
	if c.exited != nil {
		c.exited(task, started, &res)
	}

	if c.finalize != nil {
		c.finalize(all, emit, &res)
	}
	if res.Final == "" {
		res.Final = strings.TrimSpace(plain.String())
		if res.Final != "" {
			emit(Event{Time: time.Now(), Kind: EventText, Text: res.Final, Streamed: true})
		}
	}
	switch {
	case ctx.Err() != nil:
		return fail(stopStatus(ctx), stopCLITree(tree, ctx.Err()))
	// Prefer stderr, then the stream's error; a stream-set status or usage limit survives the exit.
	case waitErr != nil:
		msg := strings.TrimSpace(stderr.String())
		if limited, _ := usageLimit(res.Error, time.Now()); msg == "" || c.limits && limited {
			msg = res.Error
		}
		if msg == "" {
			msg = waitErr.Error()
		}
		status := StatusError
		if res.Status != "" && res.Status != StatusOK {
			status = res.Status
		}
		if msg = lastLines(msg, 20); msg == lastErr {
			res.Status, res.Error = status, msg
			return finish(&res, started)
		}
		return fail(status, errors.New(msg))
	default:
		if c.end != nil {
			c.end(emit, &res, errored)
		}
		if res.Status == "" {
			res.Status = StatusOK
		}
	}
	return finish(&res, started)
}

func emitCLITreeStartInfo(tree interface {
	Contained() bool
	StartWarnings() []string
}, emit func(Event)) {
	if !tree.Contained() {
		emit(Event{Time: time.Now(), Kind: EventInfo, Text: "process tree is not contained; cancellation may leave child processes running"})
	}
	for _, warning := range tree.StartWarnings() {
		emit(Event{Time: time.Now(), Kind: EventInfo, Text: "process tree startup: " + warning})
	}
}

func stopCLITree(tree *tools.Tree, err error) error {
	// Exit may race cancellation without invoking cmd.Cancel.
	if tree.Contained() {
		if killErr := tree.Kill(); killErr != nil {
			err = fmt.Errorf("%w; kill process tree: %v", err, killErr)
		}
	}
	if goneErr := tree.WaitGone(5 * time.Second); goneErr != nil {
		err = fmt.Errorf("%w; %v", err, goneErr)
	}
	return err
}

// Strip harness secrets, git repository overrides, and the nested-session marker.
func agentEnv(env []string) []string {
	return retrieval.ExecutorEnv(gitenv.WithoutRepoVars(withoutEnv(env, "CLAUDECODE")))
}

func withoutEnv(env []string, key string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if !strings.HasPrefix(strings.ToUpper(kv), key+"=") {
			out = append(out, kv)
		}
	}
	return out
}

// withoutEnvNames drops each of keys from env and returns the ones that were set.
func withoutEnvNames(env, keys []string) (out, removed []string) {
	out = env
	for _, key := range keys {
		if kept := withoutEnv(out, key); len(kept) < len(out) {
			removed = append(removed, key)
			out = kept
		}
	}
	return out, removed
}

func lastLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// Relative APPDATA paths could select workspace binaries.
func appData(parts ...string) string {
	root := os.Getenv("APPDATA")
	if !filepath.IsAbs(root) {
		return ""
	}
	return filepath.Join(append([]string{root}, parts...)...)
}

func npmShim(name string) []string {
	if runtime.GOOS != "windows" {
		return nil
	}
	if shim := appData("npm", name+".cmd"); shim != "" {
		return []string{shim}
	}
	return nil
}

func init() {
	Register(NewAPIBackend())
	Register(NewClaudeBackend())
	Register(NewCodexBackend())
	Register(NewGeminiBackend())
	Register(NewOpencodeBackend())
	Register(NewAgyBackend())
}

// guardLine is what a guarded run says about the guard, with what this CLI
// leaves open. The environment guard is reversible by a determined agent; do
// not claim isolation.
func (c *CLIBackend) guardLine(withheld []string, inherited, mcp bool) string {
	text := "guard: git pushes are pointed at a dead end; gh, npm, cloud and container logins are out of reach; tokens and keys are withheld"
	if len(withheld) > 0 {
		text += " (" + strings.Join(withheld, ", ") + ")"
	}
	if mcp && c.mcpWarnings != nil {
		text += "; MCP server secret env such as GITHUB_TOKEN is withheld; servers need --allow-dangerous or a login of their own"
	}
	if c.fileRules {
		text += "; the CLI's own rules refuse publishing commands and secret files"
	} else {
		text += fmt.Sprintf("; unguarded on the %s backend: reading and writing secret files (.env, keys, .private), the wrangler and cargo logins, and gh's login in the system's credential store (gh auth token --user)", c.name)
	}
	if !inherited {
		// The allowlist drops the tokens before the guard sees them.
		return text + "; --allow-dangerous with --inherit-env lifts it"
	}
	return text + "; --allow-dangerous lifts it"
}

// Hide MCP secrets and summarize verbose deny rules.
func shownArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if m := mcpValueArg.FindStringSubmatch(args[i]); m != nil && i > 0 && args[i-1] == "-c" {
			out = append(out, m[1]+"<hidden>")
			continue
		}
		if text, ok := strings.CutPrefix(args[i], appendSystemPromptFlag+"="); ok {
			out = append(out, fmt.Sprintf("%s=<%d bytes>", appendSystemPromptFlag, len(text)))
			continue
		}
		out = append(out, args[i])
		if args[i] != "--disallowedTools" {
			continue
		}
		n := 0
		for i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			i++
			n++
		}
		out = append(out, fmt.Sprintf("<%d guard rules>", n))
	}
	return out
}

var mcpValueArg = regexp.MustCompile(`^(mcp_servers\.[^.=]+\.(?:command|args|url|env_vars)=)`)

func isBatchFile(bin string) bool {
	ext := strings.ToLower(filepath.Ext(bin))
	return ext == ".cmd" || ext == ".bat"
}
