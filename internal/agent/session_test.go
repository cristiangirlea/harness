package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// The stream lines below are cut down from real runs of claude 2.1.278 and
// codex-cli 0.155.0-alpha.9.2, where a fork reported a new id.

func TestClaudeRecordsItsSession(t *testing.T) {
	var res Result
	for _, l := range []string{
		`{"type":"system","subtype":"init","model":"claude-haiku-4-5","session_id":"de269d41-b442-4d21-8437-18e091a1153d"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"alpha gamma"}]},"session_id":"de269d41-b442-4d21-8437-18e091a1153d"}`,
		`{"type":"result","subtype":"success","is_error":false,"num_turns":1,"result":"alpha gamma","session_id":"de269d41-b442-4d21-8437-18e091a1153d"}`,
	} {
		parseClaudeStream([]byte(l), func(Event) {}, &res)
	}
	if res.Session != "de269d41-b442-4d21-8437-18e091a1153d" || res.Final != "alpha gamma" {
		t.Errorf("session = %q, final = %q", res.Session, res.Final)
	}
}

func TestCodexRecordsItsThread(t *testing.T) {
	var res Result
	var events []Event
	for _, l := range []string{
		`{"type":"thread.started","thread_id":"01a0e2c3-0102-75c0-9330-39e30ffd8023"}`,
		`{"type":"turn.started"}`,
		`{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"alpha gamma"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":60207,"cached_input_tokens":46208,"cache_write_input_tokens":0,"output_tokens":17,"reasoning_output_tokens":0}}`,
	} {
		if !parseCodexStream([]byte(l), func(e Event) { events = append(events, e) }, &res) {
			t.Errorf("not parsed: %s", l)
		}
	}
	if res.Session != "01a0e2c3-0102-75c0-9330-39e30ffd8023" || res.Final != "alpha gamma" ||
		res.Turns != 1 || res.InputTokens != 60207 || res.CacheRead != 46208 || res.OutputTokens != 17 {
		t.Errorf("unexpected result %+v", res)
	}
	if len(events) != 1 || events[0].Text != "alpha gamma" {
		t.Errorf("unexpected events %+v", events)
	}
}

func TestClaudeForksTheSessionItIsGiven(t *testing.T) {
	b := NewClaudeBackend()
	if args := b.args(Task{}, ""); slices.Contains(args, "--resume") {
		t.Errorf("a new run must not resume anything: %q", args)
	}
	args := strings.Join(b.args(Task{ForkSession: "f1209"}, ""), " ")
	if !strings.Contains(args, "--resume f1209 --fork-session") {
		t.Errorf("args %q should fork f1209, not resume it in place", args)
	}
}

func TestCodexArgs(t *testing.T) {
	b := NewCodexBackend()
	for _, tc := range []struct {
		task Task
		want string
	}{
		{Task{Dir: "/w", Options: Options{Mode: ModeReadOnly}}, "exec --json --skip-git-repo-check -C /w --sandbox read-only -"},
		{Task{Dir: "/w", Options: Options{Mode: ModeAsk}}, "exec --json --skip-git-repo-check -C /w --sandbox read-only -"},
		// codex 0.155 refuses --full-auto; workspace-write is what it stood for.
		{Task{Dir: "/w", Options: Options{Mode: ModeAuto, Model: "gpt-5"}}, "exec --json --skip-git-repo-check -C /w -m gpt-5 --sandbox workspace-write -"},
		{Task{Dir: "/w", Options: Options{Mode: ModeYolo}}, "exec --json --skip-git-repo-check -C /w --dangerously-bypass-approvals-and-sandbox -"},
		// exec's options stay before the subcommand, where they apply to the fork too.
		{Task{Dir: "/w", ForkSession: "01a0e2c2", Options: Options{Mode: ModeReadOnly}}, "exec --json --skip-git-repo-check -C /w --sandbox read-only fork 01a0e2c2 -"},
	} {
		if got := strings.Join(b.args(tc.task, ""), " "); got != tc.want {
			t.Errorf("args = %q\n  want %q", got, tc.want)
		}
	}
}

func TestOnlyForkingBackendsTakeASession(t *testing.T) {
	for name, want := range map[string]bool{"claude": true, "codex": true, "gemini": false, "opencode": false, "api": false} {
		b, err := Get(name)
		if err != nil {
			t.Fatal(err)
		}
		if ForksSessions(b) != want {
			t.Errorf("%s: ForksSessions = %v, want %v", name, !want, want)
		}
	}
	// Refused before the binary is looked for, so this holds on any machine.
	res := NewGeminiBackend().Run(context.Background(), Task{Prompt: "x", ForkSession: "s1"}, func(Event) {})
	if res.Status != StatusError || !strings.Contains(res.Error, "cannot resume a session") {
		t.Errorf("gemini given a session: %+v", res)
	}
	for _, bad := range []string{"--dangerously-bypass-approvals-and-sandbox", "a b", "x;rm"} {
		res := NewCodexBackend().Run(context.Background(), Task{Prompt: "x", ForkSession: bad}, func(Event) {})
		if res.Status != StatusError || !strings.Contains(res.Error, "is not one the codex CLI issues") {
			t.Errorf("session %q should be refused before it becomes an argument: %+v", bad, res)
		}
	}
}

func TestNewestFilePrefersTheLatestBuild(t *testing.T) {
	root := t.TempDir()
	put := func(dir string, age time.Duration) string {
		p := filepath.Join(root, dir, "codex.exe")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o755); err != nil {
			t.Fatal(err)
		}
		mod := time.Now().Add(-age)
		if err := os.Chtimes(p, mod, mod); err != nil {
			t.Fatal(err)
		}
		return p
	}
	old := put("247581e40ee272fb", time.Hour)
	fresh := put("0a1b2c3d4e5f6071", time.Minute)
	// A build directory without the binary, as an update in progress leaves.
	if err := os.MkdirAll(filepath.Join(root, "73fd6465d9c76545"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := newestFile(root, "codex.exe"); !slices.Equal(got, []string{fresh, old}) {
		t.Errorf("newestFile = %q, want %q", got, []string{fresh, old})
	}
	if got := newestFile(filepath.Join(root, "nope"), "codex.exe"); got != nil {
		t.Errorf("a missing root lists nothing, got %q", got)
	}
}

func TestTheForkedSessionIsRecorded(t *testing.T) {
	data, err := json.Marshal(Task{Prompt: "p", ForkSession: "0199aaaa-bbbb"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"fork_session":"0199aaaa-bbbb"`) {
		t.Errorf("task.json should say which session a run started from: %s", data)
	}
}
