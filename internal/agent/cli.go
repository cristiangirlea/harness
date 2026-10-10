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
}

func (c *CLIBackend) Name() string { return c.name }

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
		cmd.Env, withheld = tools.GuardEnv(cmd.Env, guardDir)
	}
	if c.promptStdin {
		cmd.Stdin = strings.NewReader(task.Prompt)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fail(StatusError, err)
	}
	emit(Event{Time: time.Now(), Kind: EventInfo, Text: fmt.Sprintf("%s %s", filepath.Base(bin), strings.Join(shownArgs(cmd.Args[1:]), " "))})
	if limits := unenforced(task); len(limits) > 0 {
		emit(Event{Time: time.Now(), Kind: EventInfo,
			Text: fmt.Sprintf("%s: not enforced by the %s backend", strings.Join(limits, ", "), c.name)})
	}
	if guarded {
		text := "guard: git cannot push; gh, cloud and container logins are out of reach; tokens and keys are withheld"
		if len(withheld) > 0 {
			// A CLI that logs in with one of these fails under the guard;
			// this line says why.
			text += " (" + strings.Join(withheld, ", ") + ")"
		}
		emit(Event{Time: time.Now(), Kind: EventInfo, Text: text + "; --allow-dangerous lifts it"})
	} else {
		emit(Event{Time: time.Now(), Kind: EventInfo,
			Text: fmt.Sprintf("guard: lifted by --allow-dangerous; the %s CLI can push, merge and deploy with the operator's credentials", c.name)})
	}
	if err := cmd.Start(); err != nil {
		return fail(StatusError, err)
	}

	var all []byte
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	var plain strings.Builder
	for sc.Scan() {
		line := sc.Bytes()
		if c.finalize != nil {
			all = append(append(all, line...), '\n')
			continue
		}
		if c.parse != nil && c.parse(line, emit, &res) {
			continue
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
		return fail(stopStatus(ctx), ctx.Err())
	case waitErr != nil:
		// Prefer what the CLI said over the bare exit status: stderr first,
		// then a structured error the stream already reported. A status the
		// stream set (max_turns, refused) survives the non-zero exit too.
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
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
