package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/cristiangirlea/harness/internal/agent"
)

const (
	dim   = "\x1b[2m"
	red   = "\x1b[31m"
	cyan  = "\x1b[36m"
	bold  = "\x1b[1m"
	reset = "\x1b[0m"
)

// renderer prints events for a human. It tracks whether the last output ended
// mid-line so tool lines always start on a fresh line.
type renderer struct {
	mu        sync.Mutex
	out       io.Writer
	color     bool
	thinking  bool
	midLine   bool
	inThought bool
}

func newRenderer(showThinking bool) *renderer {
	return &renderer{out: os.Stdout, color: isTerminal(os.Stdout) && os.Getenv("NO_COLOR") == "", thinking: showThinking}
}

func (r *renderer) c(code, s string) string {
	if !r.color {
		return s
	}
	return code + s + reset
}

func (r *renderer) write(s string) {
	if s == "" {
		return
	}
	fmt.Fprint(r.out, s)
	r.midLine = !strings.HasSuffix(s, "\n")
}

func (r *renderer) newline() {
	if r.midLine {
		r.write("\n")
	}
}

func (r *renderer) Event(e agent.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e.Kind != agent.EventThinking && r.inThought {
		r.newline()
		r.inThought = false
	}
	switch e.Kind {
	case agent.EventText:
		if e.Streamed {
			r.newline()
			return
		}
		r.write(e.Text)
		if !e.Partial {
			r.newline()
		}
	case agent.EventThinking:
		if !r.thinking || e.Streamed {
			return
		}
		if !r.inThought {
			r.newline()
			r.write(r.c(dim, "∴ "))
			r.inThought = true
		}
		r.write(r.c(dim, e.Text))
		if !e.Partial {
			r.newline()
			r.inThought = false
		}
	case agent.EventToolCall:
		r.newline()
		summary := e.Text
		if summary == "" && len(e.Input) > 0 {
			summary = compactJSON(e.Input, 120)
		}
		r.write(r.c(cyan, "→ "+e.Tool) + " " + oneLine(summary, 160) + "\n")
	case agent.EventToolResult:
		r.newline()
		first := oneLine(e.Text, 140)
		lines := strings.Count(strings.TrimRight(e.Text, "\n"), "\n") + 1
		if lines > 1 {
			first += fmt.Sprintf(" (+%d lines)", lines-1)
		}
		if e.IsError {
			r.write(r.c(red, "  ✗ "+first) + "\n")
		} else {
			r.write(r.c(dim, "  ✓ "+first) + "\n")
		}
	case agent.EventInfo:
		r.newline()
		r.write(r.c(dim, "· "+oneLine(e.Text, 200)) + "\n")
	case agent.EventError:
		r.newline()
		r.write(r.c(red, "error: "+e.Text) + "\n")
	}
}

func (r *renderer) Summary(runID string, res agent.Result) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.newline()
	cost := ""
	if res.CostUSD > 0 {
		approx := ""
		if res.CostEstimate {
			approx = "~"
		}
		cost = fmt.Sprintf(" · %s$%.4f", approx, res.CostUSD)
	}
	status := string(res.Status)
	if res.Status != agent.StatusOK {
		status = r.c(red, status)
	}
	r.write(r.c(dim, fmt.Sprintf("── %s · %s · %s · %d turns · %s in / %s out (cache %s read)%s · %s ──",
		status, res.Backend, res.Model, res.Turns, human(res.InputTokens), human(res.OutputTokens), human(res.CacheRead),
		cost, res.Duration.Round(100_000_000))) + "\n")
	r.write(r.c(dim, "run "+runID) + "\n")
}

func oneLine(s string, n int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len([]rune(s)) > n {
		s = string([]rune(s)[:n]) + "…"
	}
	return s
}

func compactJSON(raw json.RawMessage, n int) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return oneLine(string(raw), n)
	}
	b, _ := json.Marshal(v)
	return oneLine(string(b), n)
}

func human(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
