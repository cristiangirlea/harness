package followup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/runlog"
)

// finished records a run that ended with res, following continues when set.
func finished(t *testing.T, continues string, task agent.Task, res agent.Result) string {
	t.Helper()
	w, err := runlog.CreateContinuing(res.Backend, task, continues)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status == "" {
		res.Status = agent.StatusOK
	}
	if err := w.Close(res); err != nil {
		t.Fatal(err)
	}
	return w.ID()
}

func workspace(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestSameBackendForksItsSession(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	dir := workspace(t)
	id := finished(t, "", agent.Task{Prompt: "p", Dir: dir, Options: agent.Options{Model: "sonnet"}},
		agent.Result{Backend: "claude", Session: "s1", Final: "a"})
	p, err := Make(Request{RunID: id})
	if err != nil {
		t.Fatal(err)
	}
	if p.Backend != "claude" || p.ForkSession != "s1" || p.Resume != nil || len(p.Handoff) != 0 || p.Model != "sonnet" || p.Dir != dir {
		t.Errorf("unexpected plan %+v", p)
	}
}

func TestNothingToPickUpOnTheSameBackendIsRefused(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	dir := workspace(t)
	id := finished(t, "", agent.Task{Prompt: "p", Dir: dir}, agent.Result{Backend: "gemini", Final: "a"})
	if _, err := Make(Request{RunID: id}); err == nil || !strings.Contains(err.Error(), "no saved conversation or session") {
		t.Errorf("gemini saves nothing to continue from, got %v", err)
	}
	// The same run can still be handed to a backend that starts fresh anyway.
	if p, err := Make(Request{RunID: id, Backend: "codex"}); err != nil || len(p.Handoff) != 1 {
		t.Errorf("handing the run to codex: %+v err=%v", p, err)
	}
}

func TestASessionFromAnotherWorkspaceIsNotForked(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	a, b := workspace(t), workspace(t)
	id := finished(t, "", agent.Task{Prompt: "p", Dir: a}, agent.Result{Backend: "claude", Session: "s1"})
	_, err := Make(Request{RunID: id, Dir: b})
	if err == nil || !strings.Contains(err.Error(), a) {
		t.Errorf("the error should say where the session belongs, got %v", err)
	}
}

func TestSwitchingBackendsHandsTheChainOver(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	dir := workspace(t)
	id := finished(t, "", agent.Task{Prompt: "rename add to sum", Dir: dir, Options: agent.Options{Model: "sonnet", Mode: agent.ModeAuto}},
		agent.Result{Backend: "claude", Model: "claude-sonnet-5", Session: "s1", Final: "Renamed it in math.go."})
	p, err := Make(Request{RunID: id, Backend: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Backend != "codex" || p.ForkSession != "" || len(p.Handoff) != 1 || p.Dir != dir {
		t.Errorf("unexpected plan %+v", p)
	}
	if p.Model != "" {
		t.Errorf("claude's model %q must not reach codex", p.Model)
	}

	runDir := t.TempDir()
	task := agent.Task{Prompt: "now add a test"}
	var notes []string
	p.Apply(&task, runDir, func(e agent.Event) { notes = append(notes, e.Text) })
	for _, want := range []string{"claude (claude-sonnet-5)", "--- request\nrename add to sum", "--- answer\nRenamed it in math.go."} {
		if !strings.Contains(task.Prompt, want) {
			t.Errorf("prompt is missing %q:\n%s", want, task.Prompt)
		}
	}
	if !strings.HasSuffix(task.Prompt, "=== now\n\nnow add a test") {
		t.Errorf("the follow-up should come last:\n%s", task.Prompt)
	}
	saved, err := os.ReadFile(filepath.Join(runDir, "handoff.md"))
	if err != nil || !strings.HasPrefix(task.Prompt, string(saved)) {
		t.Errorf("handoff.md should hold what went before the follow-up: err=%v", err)
	}
	if joined := strings.Join(notes, "\n"); !strings.Contains(joined, "starting a new one") || !strings.Contains(joined, "handing 1 earlier turn to codex") {
		t.Errorf("notes should say what was carried over: %q", notes)
	}
}

func TestSwitchingBackForksAndCatchesUp(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	dir := workspace(t)
	first := finished(t, "", agent.Task{Prompt: "one", Dir: dir, Options: agent.Options{Model: "sonnet"}},
		agent.Result{Backend: "claude", Session: "s1", Final: "a1"})
	second := finished(t, first, agent.Task{Prompt: "two", Dir: dir}, agent.Result{Backend: "codex", Session: "t2", Final: "a2"})
	p, err := Make(Request{RunID: second, Backend: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if p.ForkSession != "s1" || len(p.Handoff) != 1 || p.Handoff[0].ID != second || p.Model != "sonnet" {
		t.Errorf("claude should fork its own session and be told only what codex did: %+v", p)
	}
	h := p.handoff()
	if strings.Contains(h, "--- request\none") || !strings.Contains(h, "--- request\ntwo") {
		t.Errorf("only the turn claude has not seen belongs in the transcript:\n%s", h)
	}
	// Codex, continued in turn, forks its own thread and needs no transcript.
	if p, err := Make(Request{RunID: second}); err != nil || p.ForkSession != "t2" || len(p.Handoff) != 0 {
		t.Errorf("codex continuing itself: %+v err=%v", p, err)
	}
}

func TestTheAPIConversationIsResumed(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	dir := workspace(t)
	w, err := runlog.Create("api", agent.Task{Prompt: "p", Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	conv, _ := json.Marshal(agent.Conversation{Model: "claude-opus-5"})
	if err := os.WriteFile(filepath.Join(w.Dir(), "messages.json"), conv, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(agent.Result{Backend: "api", Status: agent.StatusOK}); err != nil {
		t.Fatal(err)
	}
	p, err := Make(Request{RunID: w.ID()})
	if err != nil {
		t.Fatal(err)
	}
	if p.Resume == nil || p.Resume.Model != "claude-opus-5" || p.Model != "claude-opus-5" || len(p.Handoff) != 0 {
		t.Errorf("unexpected plan %+v", p)
	}
	p, err = Make(Request{RunID: w.ID(), Model: "claude-sonnet-5"})
	if err != nil || p.Model != "claude-sonnet-5" || !strings.Contains(strings.Join(p.Notes, "\n"), "drops the earlier thinking") {
		t.Errorf("another model should be warned about: %+v err=%v", p, err)
	}
}

func TestAnUnfinishedRunIsRefused(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	w, err := runlog.Create("claude", agent.Task{Prompt: "p", Dir: workspace(t)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Make(Request{RunID: w.ID(), Backend: "codex"}); err == nil || !strings.Contains(err.Error(), "no result") {
		t.Errorf("a run without a result has nothing to hand over yet, got %v", err)
	}
	// Windows will not remove the temp dir while the transcript is open.
	_ = w.Close(agent.Result{Status: agent.StatusCanceled})
}

func TestALongHandoffKeepsTheLatestTurns(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	dir := workspace(t)
	long := strings.Repeat("é", 5000) // 10 KB, over the per-text cap
	prev := ""
	for i, b := range []string{"gemini", "opencode", "gemini", "opencode", "gemini", "opencode"} {
		prev = finished(t, prev, agent.Task{Prompt: string(rune('A'+i)) + long, Dir: dir}, agent.Result{Backend: b, Final: long})
	}
	p, err := Make(Request{RunID: prev, Backend: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	h := p.handoff()
	if len(h) > maxHandoff+1024 || !utf8.ValidString(h) {
		t.Errorf("handoff is %d bytes (cap %d), valid UTF-8: %v", len(h), maxHandoff, utf8.ValidString(h))
	}
	if !strings.Contains(h, "left out to fit") || !strings.Contains(h, "--- request\nF") || strings.Contains(h, "--- request\nA") {
		t.Errorf("the oldest turns should go first, the latest stay")
	}
}
