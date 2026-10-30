// Package limits replaces usage-limited CLIs without metered billing.
package limits

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/followup"
	"github.com/cristiangirlea/harness/internal/runlog"
)

// Partner prefers ready replacements, then installed ones for diagnostics, or "".
// Without a preference, claude and codex stand in for each other.
func Partner(backend string, now time.Time, spent Spent) string {
	p, err := agent.BackendPreferences(runlog.Home())
	if err != nil {
		return ""
	}
	if len(p.EnabledNames) > 0 {
		first := ""
		for _, name := range p.Filter(agent.Names()) {
			// Automatic replacements must not bill per token.
			if name == backend || name == "api" {
				continue
			}
			b, _ := agent.Get(name)
			if _, err := b.Detect(); err != nil {
				continue
			}
			if ok, _ := Ready(name, now, spent); ok {
				return name
			}
			if first == "" {
				first = name
			}
		}
		return first
	}
	switch backend {
	case "claude":
		return "codex"
	case "codex":
		return "claude"
	}
	return ""
}

// Atomic writes prevent partial reads; lost updates cost a limited start.
const fileName = "limits.json"

var mu sync.RWMutex

type entry struct {
	Until  time.Time `json:"until"`
	Reason string    `json:"reason,omitempty"`
}

func path() string { return filepath.Join(runlog.Home(), fileName) }

// Retry Windows sharing conflicts.
func retry(f func() error) error {
	var err error
	for i := 0; i < 20; i++ {
		if err = f(); err == nil || errors.Is(err, fs.ErrNotExist) {
			return err
		}
		time.Sleep(25 * time.Millisecond)
	}
	return err
}

// Caller holds mu; unreadable state costs a limited start.
func load() map[string]entry {
	var data []byte
	if retry(func() (err error) { data, err = os.ReadFile(path()); return err }) != nil {
		return map[string]entry{}
	}
	var m map[string]entry
	if json.Unmarshal(data, &m) != nil || m == nil {
		return map[string]entry{}
	}
	return m
}

// Note persists a known limit reset.
func Note(backend string, res agent.Result) error {
	if res.Status != agent.StatusLimited || res.LimitResets.IsZero() {
		return nil
	}
	// Unpaired backends may gain a partner later.
	if _, err := agent.BackendPreferences(runlog.Home()); err != nil {
		return err
	}
	mu.Lock()
	defer mu.Unlock()
	m := load()
	now := time.Now()
	for name, e := range m {
		if !e.Until.After(now) {
			delete(m, name)
		}
	}
	m[backend] = entry{Until: res.LimitResets, Reason: res.Error}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(path(), data)
}

func writeFile(name string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(name), fileName+".*")
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = retry(func() error { return os.Rename(f.Name(), name) })
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}

// Until returns a reset or zero, bounding implausibly long diversions.
func Until(backend string, now time.Time) time.Time {
	mu.RLock()
	m := load()
	mu.RUnlock()
	if e, ok := m[backend]; ok && e.Until.After(now) && !e.Until.After(now.Add(agent.MaxLimitWindow)) {
		return e.Until
	}
	return time.Time{}
}

// Status describes a limit or returns "", naming the file for early-reset cleanup.
func Status(backend string, now time.Time) string {
	until := Until(backend, now)
	if until.IsZero() {
		return ""
	}
	return fmt.Sprintf("at its usage limit until %s, as remembered in %s (delete it if the limit has lifted)", when(until), path())
}

// Spent tracks task-local limits without known resets too.
type Spent map[string]bool

func (s Spent) out(backend string, now time.Time) string {
	if s[backend] {
		return backend + " reached its usage limit earlier in this task"
	}
	if until := Until(backend, now); !until.IsZero() {
		return fmt.Sprintf("%s is at its usage limit until %s", backend, when(until))
	}
	return ""
}

// Credentials override plan billing; older codex uses OPENAI_API_KEY too.
var metered = map[string][]string{
	"claude": {"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY"},
	"codex":  {"CODEX_API_KEY", "OPENAI_API_KEY"},
}

// MeteredEnv names overrides to plan billing.
func MeteredEnv(backend string) []string { return slices.Clone(metered[backend]) }

// Return names only; never expose credentials.
func meteredBy(backend string) string {
	for _, name := range metered[backend] {
		v := os.Getenv(name)
		if v == "" || strings.HasPrefix(name, "CLAUDE_CODE_USE_") && (v == "0" || strings.EqualFold(v, "false")) {
			continue
		}
		return name
	}
	return ""
}

// Ready requires plan billing for unrequested replacements.
func Ready(backend string, now time.Time, spent Spent) (bool, string) {
	if backend == "api" {
		return false, "api would bill per token, not to a plan"
	}
	b, err := agent.Get(backend)
	if err != nil {
		return false, err.Error()
	}
	if _, err := b.Detect(); err != nil {
		return false, backend + " is not installed"
	}
	if name := meteredBy(backend); name != "" {
		return false, fmt.Sprintf("%s would bill per token, not to a plan, since %s is set", backend, name)
	}
	if why := spent.out(backend, now); why != "" {
		return false, why
	}
	return true, ""
}

func when(t time.Time) string { return t.Local().Format("Jan 2 15:04") }

// Pick returns backend unless it is limited and its partner is ready; bothOut means pick stays backend.
func Pick(backend string, now time.Time, spent Spent) (pick, why string, bothOut bool) {
	other := Partner(backend, now, spent)
	gone := spent.out(backend, now)
	if gone == "" || other == "" {
		return backend, "", false
	}
	if ok, reason := Ready(other, now, spent); !ok {
		return backend, gone + "; " + reason, spent.out(other, now) != ""
	}
	return other, fmt.Sprintf("%s; starting on %s instead", gone, other), false
}

// Choose returns backend as typed (even empty) unless a limit moves it; a continued run is planned after.
func Choose(backend, continueID string) (string, string) {
	meant := backend
	if meant == "" && continueID != "" {
		if prev, err := runlog.Load(continueID); err == nil {
			meant = prev.Backend
		}
	}
	if meant == "" {
		meant = agent.DefaultBackendNameIn(runlog.Home())
	}
	pick, why, _ := Pick(meant, time.Now(), nil)
	if pick != meant {
		return pick, why
	}
	return backend, why
}

const carryOnPrompt = "The turn above stopped before it finished: %s reached its plan's usage limit. " +
	"Carry on with that turn's request from where it stopped. Whatever it changed is already in this " +
	"workspace: check it, then finish the task, and answer as that turn would have."

// CarryOn plans a limited run's replacement.
type CarryOn struct {
	From, To string
	// Task must be recorded before Plan.Apply adds the transcript.
	Task agent.Task
	Plan followup.Plan

	Why string
}

// After persists a limit and plans a switch, or returns nil.
func After(prevID, backend string, prev agent.Task, res agent.Result) (next *CarryOn, why string, err error) {
	if res.Status != agent.StatusLimited {
		return nil, "", nil
	}
	if err := Note(backend, res); err != nil {
		// Persistence failure costs only a future limited start.
		why = "the limit was not remembered: " + err.Error() + "; "
	}
	other := Partner(backend, time.Now(), nil)
	if prev.NoSwitch || other == "" {
		return nil, "", nil
	}
	if ok, reason := Ready(other, time.Now(), nil); !ok {
		return nil, why + reason + ", so the run stays " + string(agent.StatusLimited), nil
	}
	plan, err := followup.Make(followup.Request{RunID: prevID, Backend: other, Dir: prev.Dir, Retrieval: prev.Retrieval})
	if err != nil {
		return nil, "", fmt.Errorf("carrying run %s on %s: %w", prevID, other, err)
	}
	task := agent.Task{Prompt: fmt.Sprintf(carryOnPrompt, backend), Purpose: prev.Purpose, Dir: plan.Dir, Options: prev.Options, Approve: prev.Approve}
	task.Model = plan.Model
	msg := fmt.Sprintf("%s reached its usage limit", backend)
	if !res.LimitResets.IsZero() {
		msg += " (until " + when(res.LimitResets) + ")"
	}
	return &CarryOn{From: backend, To: other, Task: task, Plan: plan,
		Why: fmt.Sprintf("%s%s; %s carries run %s on", why, msg, other, prevID)}, "", nil
}
