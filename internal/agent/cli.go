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
	"runtime"
	"strings"
	"time"
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
}

func (c *CLIBackend) Name() string { return c.name }

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
	if c.promptStdin {
		cmd.Stdin = strings.NewReader(task.Prompt)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fail(StatusError, err)
	}
	emit(Event{Time: time.Now(), Kind: EventInfo, Text: fmt.Sprintf("%s %s", filepath.Base(bin), strings.Join(cmd.Args[1:], " "))})
	if limits := unenforced(task); len(limits) > 0 {
		emit(Event{Time: time.Now(), Kind: EventInfo,
			Text: fmt.Sprintf("%s: not enforced by the %s backend", strings.Join(limits, ", "), c.name)})
	}
	if task.Mode == ModeYolo {
		// The command guard lives in the api backend's own run_command, so a
		// CLI backend running unattended has only its own permission system.
		emit(Event{Time: time.Now(), Kind: EventInfo,
			Text: fmt.Sprintf("command guard: not applied by the %s backend; its own permissions decide", c.name)})
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
