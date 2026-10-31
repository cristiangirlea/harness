package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/runlog"
)

// continued returns the record of the run that continues id.
func continued(t *testing.T, id string) runlog.Record {
	t.Helper()
	recs, err := runlog.List(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range recs {
		if rec.Continues == id {
			return rec
		}
	}
	t.Fatalf("no run continues %s", id)
	return runlog.Record{}
}

// withCommitBlock is prompt behind the default commit block agent.Run puts
// first, with the fix-round line a continue adds.
func withCommitBlock(prompt string, fixRound bool) string {
	b := "# How to commit\n\n" +
		"Reviews are text, commits are changes. Review findings and the reason for a fix go into the review on the pull request, not into commit messages and not into a commit per round or per finding. Before a commit exists, everything found goes into that one commit; no fix-round commits for work that was never pushed. After a commit, a new finding means one more review entry and one more commit for all of it. Every defect a review finds in what this run changes is fixed before the push; nits are dropped.\n" +
		"These commit rules take precedence over a repository instruction such as \"one logical change per commit\" when the two conflict.\n" +
		"Commit your work before you finish, as one clean commit for this task; leave the working tree clean.\n"
	if fixRound {
		b += "This is a fix round: one review, one commit; every fix from this review goes into one commit.\n"
	}
	return b + "\n" + prompt
}

func TestRunFreshStartsANewSessionWithAHandover(t *testing.T) {
	for _, backend := range []string{"claude", "codex", "agy"} {
		t.Run(backend, func(t *testing.T) {
			drain(t)
			t.Setenv("HRN_HOME", t.TempDir())
			old, _ := agent.Get(backend)
			agent.Register(recorder{backend, recorded})
			defer agent.Register(old)
			dir, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			w, err := runlog.Create(backend, agent.Task{Prompt: "implement the parser", Dir: dir, Options: agent.Options{Mode: agent.ModeAuto, Model: "m1"}})
			if err != nil {
				t.Fatal(err)
			}
			if err := w.Close(agent.Result{Backend: backend, Status: agent.StatusOK, Session: "s1", Final: "Parser done."}); err != nil {
				t.Fatal(err)
			}
			code, err := cmdRun([]string{"--continue", w.ID(), "--fresh", "--no-switch", "review it"})
			if code != 0 || err != nil {
				t.Fatalf("code=%d err=%v", code, err)
			}
			task := <-recorded
			if task.ForkSession != "" || task.Resume != nil {
				t.Fatalf("a fresh run picked up a session: fork=%q resume=%v", task.ForkSession, task.Resume)
			}
			if task.Mode != agent.ModeAuto || task.Model != "m1" || task.Dir != dir {
				t.Errorf("mode, model and workspace should be inherited: %+v", task.Options)
			}
			if !strings.Contains(task.Prompt, "> implement the parser") || !strings.Contains(task.Prompt, "> Parser done.") || !strings.HasSuffix(task.Prompt, "=== now\n\nreview it") {
				t.Errorf("prompt should be the hand-over, then the request:\n%s", task.Prompt)
			}
			rec := continued(t, w.ID())
			if rec.ContinueRoute != runlog.RouteFreshHandover || rec.Task.Prompt != "review it" || rec.Backend != backend {
				t.Errorf("record: route=%q prompt=%q backend=%q", rec.ContinueRoute, rec.Task.Prompt, rec.Backend)
			}
			saved, err := os.ReadFile(filepath.Join(runlog.Root(), rec.ID, "handoff.md"))
			if err != nil || withCommitBlock(string(saved)+"review it", true) != task.Prompt {
				t.Errorf("handoff.md: %v", err)
			}
		})
	}
}

func TestRunWithoutFreshStillResumes(t *testing.T) {
	drain(t)
	t.Setenv("HRN_HOME", t.TempDir())
	w, err := runlog.Create("stub-cmd", agent.Task{Prompt: "first", Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.Dir(), "messages.json"), []byte(`{"model":"stub"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(agent.Result{Backend: "stub-cmd", Status: agent.StatusOK}); err != nil {
		t.Fatal(err)
	}
	if code, err := cmdRun([]string{"--continue", w.ID(), "--no-switch", "again"}); code != 0 || err != nil {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if task := <-recorded; task.Resume == nil || task.Prompt != withCommitBlock("again", true) {
		t.Errorf("ordinary continue should resume the conversation as before: resume=%v prompt=%q", task.Resume, task.Prompt)
	}
	if rec := continued(t, w.ID()); rec.ContinueRoute != "" {
		t.Errorf("ordinary continue recorded route %q", rec.ContinueRoute)
	}
	if code, err := cmdRun([]string{"--continue", w.ID(), "--fresh", "--no-switch", "again"}); code != 0 || err != nil {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if task := <-recorded; task.Resume != nil {
		t.Errorf("a fresh run must not resume messages.json")
	}
}

func TestRunFreshNeedsARunToContinue(t *testing.T) {
	drain(t)
	t.Setenv("HRN_HOME", t.TempDir())
	code, err := cmdRun([]string{"-b", "stub-cmd", "-C", t.TempDir(), "--fresh", "x"})
	if code != 2 || err == nil || !strings.Contains(err.Error(), "--fresh needs --continue") {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if recs, err := runlog.List(0); err != nil || len(recs) != 0 {
		t.Fatalf("a refused run left records: %v %v", recs, err)
	}
}

func TestContinueCodexDefaultAndResume(t *testing.T) {
	drain(t)
	t.Setenv("HRN_HOME", t.TempDir())
	old, _ := agent.Get("codex")
	agent.Register(recorder{"codex", recorded})
	defer agent.Register(old)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := runlog.Create("codex", agent.Task{Prompt: "first", Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.Dir(), "messages.json"), []byte(`{"model":"stub"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(agent.Result{Backend: "codex", Status: agent.StatusOK, Session: "s1"}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args  []string
		fresh bool
	}{{nil, true}, {[]string{"--resume"}, false}} {
		args := append([]string{"--continue", w.ID(), "--no-switch"}, tc.args...)
		args = append(args, "again")
		if code, err := cmdRun(args); code != 0 || err != nil {
			t.Fatalf("code=%d err=%v", code, err)
		}
		task := <-recorded
		if strings.Contains(task.Prompt, "=== now") != tc.fresh {
			t.Errorf("fresh=%v prompt=%q", tc.fresh, task.Prompt)
		}
	}
}

func TestResumeFlagValidation(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--resume", "x"}, "--resume needs --continue"},
		{[]string{"--continue", "run", "--resume", "--fresh", "x"}, "--resume and --fresh cannot be combined"},
	} {
		code, err := cmdRun(tc.args)
		if code != 2 || err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("args=%v code=%d err=%v", tc.args, code, err)
		}
	}
}
