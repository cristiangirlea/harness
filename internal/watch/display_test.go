package watch

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/runlog"
)

func TestToolCallsShowOnlyOneShellLayerRemoved(t *testing.T) {
	sh := newShortener("/work", "/home/ana", testSystem)
	for _, tc := range []struct{ in, want string }{
		{`powershell.exe -Command "Get-Location; git status"`, `Get-Location; git status`},
		{`powershell -NoProfile -NonInteractive -ExecutionPolicy Bypass -Command "git status"`, `git status`},
		{`pwsh -NoProfile -Command 'git status'`, `git status`},
		{`pwsh.exe -NonInteractive -ExecutionPolicy RemoteSigned -Command 'git status'`, `git status`},
		{`"C:\Program Files\PowerShell\7\pwsh.exe" -Command "git status"`, `git status`},
		{`& 'D:\tools\pwsh.exe' -Command 'git status'`, `git status`},
		{`& 'D:\tools\powershell.exe' -NoProfile -Command 'git status'`, `git status`},
		{`bash -lc 'git status'`, `git status`},
		{`bash -c "git status"`, `git status`},
		{`/bin/bash -lc 'git status'`, `git status`},
		{`"C:\Program Files\Git\bin\bash.exe" -lc 'git status'`, `git status`},
		{`sh -c 'git status'`, `git status`},
		{`cmd /c git status`, `git status`},
		{`cmd.exe /d /s /c "git status"`, `git status`},
		{`zsh -lc 'git status'`, `git status`},
		{`bash -c 'sh -c "git status"'`, `sh -c "git status"`},
		{`pwsh -Command "cd /work; cat /home/ana/file"`, `cd .; cat ~/file`},
		{`bash -c 'echo "hello world"'`, `echo "hello world"`},
		{`cmd /c echo "hello world"`, `echo "hello world"`},
		{`bash -c 'git status`, `bash -c 'git status`},
		{`pwsh -Command "git status`, `pwsh -Command "git status`},
		{`cmd /c echo "hello`, `cmd /c echo "hello`},
		{`bash -c 'echo "hello'`, `bash -c 'echo "hello'`},
		{`'pwsh -Command "git status"`, `'pwsh -Command "git status"`},
		{`bash -c 'git status' trailing`, `bash -c 'git status' trailing`},
		{`git status`, `git status`},
		{`echo bash -c 'git status'`, `echo bash -c 'git status'`},
		{`pwsh -File script.ps1`, `pwsh -File script.ps1`},
		{`bash -c`, `bash -c`},
		{`pwsh -ExecutionPolicy -Command 'git status'`, `pwsh -ExecutionPolicy -Command 'git status'`},
	} {
		t.Run(tc.in, func(t *testing.T) {
			input, _ := json.Marshal(map[string]any{"command": tc.in, "other": "unchanged"})
			original := string(input)
			e := agent.Event{Kind: agent.EventToolCall, Text: tc.in, Input: input}
			got := sh.event(e)
			var fields map[string]string
			if err := json.Unmarshal(got.Input, &fields); err != nil {
				t.Fatal(err)
			}
			if got.Text != tc.want || fields["command"] != tc.want || fields["other"] != "unchanged" {
				t.Errorf("got text %q input %s, want command %q", got.Text, got.Input, tc.want)
			}
			if e.Text != tc.in || string(e.Input) != original {
				t.Fatal("changed the recorded event")
			}
		})
	}
}

func TestCommandArraysAreUnwrappedWithoutChangingOtherInput(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`{"command":["bash","-lc","git status"],"n":9007199254740993}`, `{"command":"git status","n":9007199254740993}`},
		{`{"command":["pwsh.exe","-NoProfile","-Command","git status"]}`, `{"command":"git status"}`},
		{`{"command":["cmd.exe","/d","/s","/c","git","status"]}`, `{"command":"git status"}`},
		{`{"command":["bash","-c","echo \"hello\""]}`, `{"command":"echo \"hello\""}`},
		{`{"command":["git","status"]}`, `{"command":["git","status"]}`},
		{`{"command":["bash","-c","echo 'hello"]}`, `{"command":["bash","-c","echo 'hello"]}`},
		{`{"command":42}`, `{"command":42}`},
		{`{"command":`, `{"command":`},
	} {
		e := agent.Event{Kind: agent.EventToolCall, Input: json.RawMessage(tc.in)}
		if got := (shortener{}).event(e); string(got.Input) != tc.want {
			t.Errorf("input %s: got %s, want %s", tc.in, got.Input, tc.want)
		}
	}
	for _, kind := range []agent.EventKind{agent.EventInfo, agent.EventText, agent.EventToolResult} {
		e := agent.Event{Kind: kind, Text: `bash -c 'git status'`, Input: json.RawMessage(`{"command":"bash -c 'git status'"}`)}
		if got := (shortener{}).event(e); got.Text != e.Text || string(got.Input) != string(e.Input) {
			t.Errorf("changed a %s event", kind)
		}
	}
}

func TestWatchHidesOnlyBackendStartEvents(t *testing.T) {
	cases := []struct {
		backend, text string
		kind          agent.EventKind
		hide          bool
	}{
		{filepath.Join("tools", "custom.exe"), "custom.exe exec --start-marker", agent.EventInfo, true},
		{filepath.Join("tools", "codex.exe"), "codex.exe exec --start-marker", agent.EventInfo, true},
		{"other", "codex exec --start-marker", agent.EventInfo, false},
		{"", "routing to codex later --start-marker", agent.EventInfo, false},
		{"", "codex: warning --start-marker", agent.EventInfo, false},
		{"", "codex-other --start-marker", agent.EventInfo, false},
		{"", "codex", agent.EventInfo, false},
		{"", "codex exec --start-marker", agent.EventText, false},
	}
	for _, name := range []string{"claude", "codex", "agy", "gemini", "opencode"} {
		for _, ext := range []string{"", ".exe", ".cmd"} {
			cases = append(cases, struct {
				backend, text string
				kind          agent.EventKind
				hide          bool
			}{"", name + ext + " --start-marker", agent.EventInfo, true})
		}
	}
	for i, tc := range cases {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			t.Setenv("HRN_HOME", t.TempDir())
			w, err := runlog.Create("fake", agent.Task{Prompt: "test"})
			if err != nil {
				t.Fatal(err)
			}
			w.Event(agent.Event{Kind: tc.kind, Text: tc.text})
			w.Event(agent.Event{Kind: agent.EventInfo, Text: "guard stays"})
			if tc.backend != "" {
				if err := w.Close(agent.Result{Backend: tc.backend, Status: agent.StatusOK}); err != nil {
					t.Fatal(err)
				}
			} else {
				defer w.Close(agent.Result{Status: agent.StatusOK})
			}
			srv := httptest.NewServer(New(context.Background(), Options{Poll: time.Millisecond}))
			defer srv.Close()
			_, page := get(t, srv.URL+"/run/"+w.ID())
			needle := "--start-marker"
			if tc.text == "codex" {
				needle = ">codex<"
			}
			if strings.Contains(page, needle) == tc.hide || !strings.Contains(page, "guard stays") {
				t.Errorf("page start visibility is wrong: hidden=%v", tc.hide)
			}
			stream, stop := openStream(t, srv.URL+"/stream/run/"+w.ID(), "")
			defer stop()
			m, ok := stream.next(t)
			if !ok || strings.Contains(m.data, needle) == tc.hide || tc.hide && !strings.Contains(m.data, "guard stays") {
				t.Errorf("stream start visibility is wrong: hidden=%v, message=%+v", tc.hide, m)
			}
			_, index := get(t, srv.URL+"/")
			if strings.Contains(index, "--start-marker") {
				t.Error("index shows start event")
			}
		})
	}
}
