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
	"strings"
	"time"

	"github.com/cristiangirlea/harness/internal/tools"
)

// This file holds the machinery every agent CLI shares: finding the binary,
// running it, reading its stdout and turning a non-zero exit into a Result.
// Each backend's arguments and stream format live in its own file, claude.go,
// codex.go, gemini.go and opencode.go.

// CLIBackend drives an installed agent CLI non-interactively, so the work is
// billed to that CLI's own subscription login rather than per API token.
type CLIBackend struct {
	name string
	// candidates lists extra install locations checked after PATH.
	candidates func() []string
	// args builds the argument list. promptFile is set when the prompt is
	// passed as an attachment instead of on stdin.
	args        func(t Task, promptFile string) []string
	promptStdin bool
	// parse handles one stdout line. Nil means stdout is plain text.
	parse func(line []byte, emit func(Event), res *Result) bool
	// finalize runs after exit with all of stdout, for single-JSON outputs.
	finalize func(stdout []byte, emit func(Event), res *Result)
	// forks is set when args honours Task.ForkSession and parse records
	// Result.Session.
	forks bool
	// loginEnv names the secret-looking variables the CLI logs in with on a
	// plan; the guard passes them on to this CLI and no other.
	loginEnv []string
	// fileRules is set when args hands the guard's secret files to the CLI's
	// own permission rules; the other CLIs can read and write them.
	fileRules bool
	// mapsEffort is set when args passes Options.Effort to the CLI.
	mapsEffort bool
	// These fields are opt-in: advertise only explicit adapter mappings.
	mapsModes    []Mode
	mapsModel    bool
	mapsMaxTurns bool
	mapsMCP      bool
	// mapsLean is set when args honours Options.Lean. It is not a capability:
	// the exec protocol's start message has no lean field.
	mapsLean bool
	// limits is set when the CLI bills to a plan and says in words that its
	// usage limit was reached (usageLimit knows the words): such a run ends
	// limited rather than in an error.
	limits bool
}

func (c *CLIBackend) Name() string { return c.name }

// AppliesLean reports whether this CLI honours Options.Lean (see AppliesLean).
func (c *CLIBackend) AppliesLean() bool { return c.mapsLean }

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
	// Run itself enforces timeout for every CLI adapter.
	caps.Options = append(caps.Options, "timeout")
	if c.mapsMaxTurns {
		caps.Options = append(caps.Options, "max_turns")
	}
	if c.mapsMCP {
		caps.Options = append(caps.Options, "mcp")
	}
	if c.forks {
		caps.Sessions = "fork"
	}
	return caps
}

// ForksSessions reports whether the CLI can start from a copy of an earlier
// run's session.
func (c *CLIBackend) ForksSessions() bool { return c.forks }

var sessionID = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._-]*$`)

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
	// The id comes from a record on disk and becomes an argument, where one
	// starting with a dash would be read as a flag. Both CLIs issue UUIDs.
	if task.ForkSession != "" && !sessionID.MatchString(task.ForkSession) {
		return fail(StatusError, fmt.Errorf("session id %q is not one the %s CLI issues", task.ForkSession, c.name))
	}

	bin, err := c.Detect()
	if err != nil {
		return fail(StatusError, err)
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
	// A nested Claude Code session refuses to start when it thinks it is
	// inside another one.
	cmd.Env = withoutEnv(os.Environ(), "CLAUDECODE")
	guarded := !task.AllowDangerous
	var withheld []string
	if guarded {
		// The logins tools keep on disk are looked for in an empty directory
		// of the run's own.
		guardDir, err := os.MkdirTemp("", "hrn-guard-*")
		if err != nil {
			return fail(StatusError, err)
		}
		defer os.RemoveAll(guardDir)
		cmd.Env, withheld = tools.GuardEnv(cmd.Env, guardDir, c.loginEnv...)
	}
	if c.promptStdin {
		cmd.Stdin = strings.NewReader(task.Prompt)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	cmd.WaitDelay = 5 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fail(StatusError, err)
	}
	emit(Event{Time: time.Now(), Kind: EventInfo, Text: fmt.Sprintf("%s %s", filepath.Base(bin), strings.Join(shownArgs(cmd.Args[1:]), " "))})
	if limits := unenforced(task, c.mapsEffort, c.mapsLean); len(limits) > 0 {
		emit(Event{Time: time.Now(), Kind: EventInfo,
			Text: fmt.Sprintf("%s: not enforced by the %s backend", strings.Join(limits, ", "), c.name)})
	}
	if guarded {
		emit(Event{Time: time.Now(), Kind: EventInfo, Text: c.guardLine(withheld)})
	} else {
		emit(Event{Time: time.Now(), Kind: EventInfo,
			Text: fmt.Sprintf("guard: lifted by --allow-dangerous; the %s CLI can push, merge and deploy with the operator's credentials", c.name)})
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
	// A surviving descendant can hold StdoutPipe open before Wait is reached.
	// Bound that read on cancellation as well as exec's stderr copy in Wait.
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
	for sc.Scan() {
		line := sc.Bytes()
		if c.finalize != nil {
			all = append(append(all, line...), '\n')
			continue
		}
		if c.parse != nil {
			parsed := c.parse(line, emit, &res)
			if res.Session != "" && res.Session != session {
				session = res.Session
				emit(Event{Time: time.Now(), Kind: EventSession, Text: session})
			}
			if parsed {
				continue
			}
		}
		// Unparsed output is shown as text so nothing is silently lost.
		text := string(line) + "\n"
		plain.WriteString(text)
		emit(Event{Kind: EventText, Text: text, Partial: true})
	}
	_, _ = io.Copy(io.Discard, stdout)
	waitErr := cmd.Wait()

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
	case waitErr != nil:
		// Prefer what the CLI said over the bare exit status: stderr first,
		// then a structured error the stream already reported. A status the
		// stream set (max_turns, refused) survives the non-zero exit too. A
		// usage limit the stream reported wins over stderr, which then holds
		// only the CLI's chatter around it.
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
		return fail(status, errors.New(lastLines(msg, 20)))
	case res.Status == "":
		res.Status = StatusOK
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
	// exec may have reaped a CLI that exited just before cancellation
	// and stopped its watcher without ever invoking cmd.Cancel.
	// Without containment, cmd.Cancel owns the best-effort fallback. WaitGone
	// supplies the single explanation that cleanup could not be verified.
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

func withoutEnv(env []string, key string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if !strings.HasPrefix(strings.ToUpper(kv), key+"=") {
			out = append(out, kv)
		}
	}
	return out
}

func lastLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func appData(parts ...string) string {
	return filepath.Join(append([]string{os.Getenv("APPDATA")}, parts...)...)
}

func npmShim(name string) []string {
	if runtime.GOOS != "windows" {
		return nil
	}
	return []string{appData("npm", name+".cmd")}
}

func init() {
	Register(NewAPIBackend())
	Register(NewClaudeBackend())
	Register(NewCodexBackend())
	Register(NewGeminiBackend())
	Register(NewOpencodeBackend())
}

// guardLine is what a guarded run says about the guard, with what this CLI
// leaves open. It says "pointed", not "cannot": an agent set on undoing the
// guarded environment can, and the README says how.
func (c *CLIBackend) guardLine(withheld []string) string {
	text := "guard: git pushes are pointed at a dead end; gh, npm, cloud and container logins are out of reach; tokens and keys are withheld"
	if len(withheld) > 0 {
		// A CLI that logs in with one of these fails under the guard; this
		// line says why.
		text += " (" + strings.Join(withheld, ", ") + ")"
	}
	if c.fileRules {
		text += "; the CLI's own rules refuse publishing commands and secret files"
	} else {
		text += fmt.Sprintf("; unguarded on the %s backend: reading and writing secret files (.env, keys, .private), the wrangler and cargo logins, and gh's login in the system's credential store (gh auth token --user)", c.name)
	}
	return text + "; --allow-dangerous lifts it"
}

// shownArgs is a command line as a run's info line shows it: the guard's
// deny rules, dozens of them, are counted rather than listed.
func shownArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
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
