// Package limits keeps work going when a CLI backend reaches its plan's usage
// limit. claude and codex bill to the owner's subscriptions, and each stands in
// for the other: a run that ends limited is carried on by the other one, and
// the time the limit lifts is remembered, so runs meanwhile start on the other
// one directly. No other backend takes part: api is billed per token, and
// gemini and opencode are not the plans this is for.
package limits

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/followup"
	"github.com/cristiangirlea/harness/internal/runlog"
)

// Partner is the backend that takes over from one at its usage limit, or ""
// when none does.
func Partner(backend string) string {
	switch backend {
	case "claude":
		return "codex"
	case "codex":
		return "claude"
	}
	return ""
}

// The file is small and read on every run; a write replaces it whole, so a
// reader never sees half of one. Two runs noting a limit at the same moment
// can lose one note, which costs one more limited run, not a wrong answer.
const fileName = "limits.json"

type entry struct {
	Until  time.Time `json:"until"`
	Reason string    `json:"reason,omitempty"`
}

func path() string { return filepath.Join(runlog.Home(), fileName) }

// load reads the remembered limits. A missing or unreadable file is no limits:
// the worst it costs is one more run that ends limited.
func load() map[string]entry {
	m := map[string]entry{}
	data, err := os.ReadFile(path())
	if err == nil {
		_ = json.Unmarshal(data, &m)
	}
	return m
}

// Note remembers when backend's limit lifts, from a run's result. A result
// that is not limited, or does not say when the limit lifts, changes nothing.
func Note(backend string, res agent.Result) error {
	if res.Status != agent.StatusLimited || res.LimitResets.IsZero() || Partner(backend) == "" {
		return nil
	}
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
		err = os.Rename(f.Name(), name)
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}

// Until is when backend's remembered limit lifts, or zero when none holds at now.
func Until(backend string, now time.Time) time.Time {
	if e, ok := load()[backend]; ok && e.Until.After(now) {
		return e.Until
	}
	return time.Time{}
}

// Status is backend's remembered limit in words, for a list of backends, or ""
// when none holds. A limit can lift early (codex sells credits), and only
// deleting the file tells hrn so, which is why the line names it.
func Status(backend string, now time.Time) string {
	until := Until(backend, now)
	if until.IsZero() {
		return ""
	}
	return fmt.Sprintf("at its usage limit until %s, as remembered in %s (delete it if the limit has lifted)", when(until), path())
}

// Spent names the backends that reached their limit in the caller's own work,
// such as a batch task's earlier attempts. They count as limited whatever the
// file says, which matters when the limit gave no time to remember.
type Spent map[string]bool

// out says why backend cannot start now, or "" when it can as far as limits go.
func (s Spent) out(backend string, now time.Time) string {
	if s[backend] {
		return backend + " reached its usage limit earlier in this task"
	}
	if until := Until(backend, now); !until.IsZero() {
		return fmt.Sprintf("%s is at its usage limit until %s", backend, when(until))
	}
	return ""
}

// Ready reports whether backend can take work over at now, or why not.
func Ready(backend string, now time.Time, spent Spent) (bool, string) {
	b, err := agent.Get(backend)
	if err != nil {
		return false, err.Error()
	}
	if _, err := b.Detect(); err != nil {
		return false, backend + " is not installed"
	}
	if why := spent.out(backend, now); why != "" {
		return false, why
	}
	return true, ""
}

func when(t time.Time) string { return t.Local().Format("Jan 2 15:04") }

// Pick is the backend a run meant for backend starts on: backend itself,
// unless it is limited and its partner can take over. why says what moved it,
// for the run's transcript, and is empty when nothing did. When both are
// limited, pick is backend, why says so, and bothOut is set: a caller with
// other work to do can skip this; one without runs it and lets it say so.
func Pick(backend string, now time.Time, spent Spent) (pick, why string, bothOut bool) {
	other := Partner(backend)
	gone := spent.out(backend, now)
	if gone == "" || other == "" {
		return backend, "", false
	}
	if ok, reason := Ready(other, now, spent); !ok {
		if spent.out(other, now) != "" {
			return backend, gone + "; " + reason, true
		}
		return backend, "", false // not installed: nothing else to start on
	}
	return other, fmt.Sprintf("%s; starting on %s instead", gone, other), false
}

// Choose is Pick for a run as its caller asked for it: backend as typed, empty
// for the default or for a continued run's own, continuing continueID when that
// is set. It returns backend unchanged, and no why, when nothing moves it; a
// continued run is planned after this, on whatever it returns.
func Choose(backend, continueID string) (string, string) {
	meant := backend
	if meant == "" && continueID != "" {
		if prev, err := runlog.Load(continueID); err == nil {
			meant = prev.Backend
		}
	}
	if meant == "" {
		meant = agent.DefaultBackendName()
	}
	if pick, why, _ := Pick(meant, time.Now(), nil); pick != meant {
		return pick, why
	}
	return backend, ""
}

// carryOnPrompt is what the partner is asked, below the transcript of the
// turn that stopped.
const carryOnPrompt = "The turn above stopped before it finished: %s reached its plan's usage limit. " +
	"Carry on with that turn's request from where it stopped. Whatever it changed is already in this " +
	"workspace: check it, then finish the task, and answer as that turn would have."

// CarryOn is the run that takes a limited run on, on the partner backend.
type CarryOn struct {
	From, To string
	// Task is ready for a record that continues the limited run; Plan.Apply
	// then hands the transcript over, as for any follow-up.
	Task agent.Task
	Plan followup.Plan
	// Why is an info line for the new run's transcript.
	Why string
}

// After plans the run that carries limited run prevID on. It also remembers
// the limit, so the next run starts on the partner. next is nil when there is
// nothing to do: the run was not limited, switching is off for it, or its
// backend has no partner (why is empty), or the partner cannot take over
// (why says so).
func After(prevID, backend string, prev agent.Task, res agent.Result) (next *CarryOn, why string, err error) {
	if res.Status != agent.StatusLimited {
		return nil, "", nil
	}
	if err := Note(backend, res); err != nil {
		// Not remembering costs the next run a limited start; this one goes on.
		why = "the limit was not remembered: " + err.Error() + "; "
	}
	other := Partner(backend)
	if prev.NoSwitch || other == "" {
		return nil, "", nil
	}
	if ok, reason := Ready(other, time.Now(), nil); !ok {
		return nil, why + reason + ", so the run stays " + string(agent.StatusLimited), nil
	}
	plan, err := followup.Make(followup.Request{RunID: prevID, Backend: other, Dir: prev.Dir})
	if err != nil {
		return nil, "", fmt.Errorf("carrying run %s on on %s: %w", prevID, other, err)
	}
	task := agent.Task{Prompt: fmt.Sprintf(carryOnPrompt, backend), Dir: plan.Dir, Options: prev.Options, Approve: prev.Approve}
	task.Model = plan.Model
	msg := fmt.Sprintf("%s reached its usage limit", backend)
	if !res.LimitResets.IsZero() {
		msg += " (until " + when(res.LimitResets) + ")"
	}
	return &CarryOn{From: backend, To: other, Task: task, Plan: plan,
		Why: fmt.Sprintf("%s%s; %s carries run %s on", why, msg, other, prevID)}, "", nil
}
