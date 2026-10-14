package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestHelperAgy(t *testing.T) {
	if os.Getenv("HRN_AGY_HELPER") == "" {
		return
	}
	input, err := io.ReadAll(os.Stdin)
	var message struct {
		Event   string `json:"event"`
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err != nil || json.Unmarshal(input, &message) != nil || message.Event != "user" || message.Message.Content != os.Getenv("HRN_AGY_PROMPT") {
		fmt.Fprintln(os.Stderr, "invalid stdin prompt")
		os.Exit(2)
	}
	want := []string{"--input-format", "stream-json", "--output-format", "stream-json", "--mode", "plan", "--model", "requested-model", "--effort", "high", "--conversation", "previous-session"}
	separator := slices.Index(os.Args, "--")
	if separator < 0 || !reflect.DeepEqual(os.Args[separator+1:], want) {
		fmt.Fprintln(os.Stderr, "unexpected agy arguments")
		os.Exit(2)
	}
	data, err := os.ReadFile(os.Getenv("HRN_AGY_FIXTURE"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	fmt.Print(string(data))
	os.Exit(0)
}

func TestAgyRunsWithAStreamedPromptAndRecordsItsResult(t *testing.T) {
	fixture, err := filepath.Abs("testdata/agy.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	prompt := strings.Repeat("a long prompt\n\"quoted\" \\ unicode: 世界\n", 2000)
	t.Setenv("HRN_AGY_HELPER", "1")
	t.Setenv("HRN_AGY_PROMPT", prompt)
	t.Setenv("HRN_AGY_FIXTURE", fixture)
	b := NewAgyBackend()
	args := b.args
	b.name, b.candidates = os.Args[0], nil
	b.args = func(task Task, file string) []string {
		return append([]string{"-test.run=^TestHelperAgy$", "--"}, args(task, file)...)
	}
	for range 2 {
		var streamed strings.Builder
		var sessions []string
		res := b.Run(context.Background(), Task{Prompt: prompt, Dir: t.TempDir(), ForkSession: "previous-session", Options: Options{Mode: ModeReadOnly, Model: "requested-model", Effort: "high"}}, func(e Event) {
			if e.Kind == EventText && e.Partial {
				streamed.WriteString(e.Text)
			}
			if e.Kind == EventSession {
				sessions = append(sessions, e.Text)
			}
		})
		if res.Status != StatusOK || res.Final != "ok\n" || res.Turns != 1 || res.Session != "example-conversation" {
			t.Fatalf("result: %+v", res)
		}
		if res.InputTokens != 13789 || res.OutputTokens != 1052 || res.CacheRead != 0 || res.Model != "requested-model" || res.ModelReported {
			t.Fatalf("usage/model: %+v", res)
		}
		if streamed.String() != "ok\n" || !reflect.DeepEqual(sessions, []string{"example-conversation"}) {
			t.Fatalf("stream %q, sessions %v", streamed.String(), sessions)
		}
		if total, known, estimated := res.ModelTokens(); total != 14841 || !known || estimated {
			t.Fatalf("tokens = %d, known %v, estimated %v", total, known, estimated)
		}
	}
}

func TestAgyKeepsFailureStatusAndMessageThroughTheRunner(t *testing.T) {
	t.Setenv("HRN_AGY_HELPER", "1")
	t.Setenv("HRN_AGY_PROMPT", "work")
	fixture := filepath.Join(t.TempDir(), "failure.jsonl")
	if err := os.WriteFile(fixture, []byte(`{"event":"result","result":{"status":"FAILED","message":"permission denied","response":"partial"}}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HRN_AGY_FIXTURE", fixture)
	b := NewAgyBackend()
	args := b.args
	b.name, b.candidates = os.Args[0], nil
	b.args = func(task Task, file string) []string {
		return append([]string{"-test.run=^TestHelperAgy$", "--"}, args(task, file)...)
	}
	res := b.Run(context.Background(), Task{Prompt: "work", Dir: t.TempDir(), ForkSession: "previous-session", Options: Options{Mode: ModeReadOnly, Model: "requested-model", Effort: "high"}}, func(Event) {})
	if res.Status != StatusError || !strings.Contains(res.Error, "FAILED") || !strings.Contains(res.Error, "permission denied") || res.Final != "partial" {
		t.Fatalf("result: %+v", res)
	}
}

func TestAgyBuildsArgumentsForModesModelsAndEveryEffort(t *testing.T) {
	b := NewAgyBackend()
	for _, c := range []struct {
		mode  Mode
		flags []string
	}{
		{ModeReadOnly, []string{"--mode", "plan"}},
		{ModeAsk, []string{"--mode", "plan"}},
		{ModeAuto, []string{"--mode", "accept-edits"}},
		{ModeYolo, []string{"--dangerously-skip-permissions"}},
	} {
		for _, effort := range append([]string{""}, EffortLevels...) {
			task := Task{Prompt: "not an argument", Options: Options{Mode: c.mode, Effort: effort}}
			want := append([]string{"--input-format", "stream-json", "--output-format", "stream-json"}, c.flags...)
			if effort != "" {
				want = append(want, "--effort", effort)
			}
			if got := b.args(task, ""); !reflect.DeepEqual(got, want) {
				t.Errorf("%s/%s: %v, want %v", c.mode, effort, got, want)
			}
		}
	}
	registered, err := Get("agy")
	if err != nil || registered.Name() != "agy" {
		t.Fatalf("registry: %v, %v", registered, err)
	}
	caps := b.Capabilities()
	if !reflect.DeepEqual(caps.Modes, []Mode{ModeReadOnly, ModeAuto, ModeYolo}) || !reflect.DeepEqual(caps.Efforts, EffortLevels) || !slices.Contains(caps.Options, "model") || !ForksSessions(b) || caps.Sessions != "resume" {
		t.Fatalf("capabilities: %+v", caps)
	}
}

func TestAgyParsesUnknownStepsWithoutAddingThemToTheAnswer(t *testing.T) {
	parse := NewAgyBackend().newParse()
	var res Result
	var text strings.Builder
	emit := func(e Event) {
		if e.Kind == EventText {
			text.WriteString(e.Text)
		}
	}
	for _, line := range []string{
		`{"event":"step_update","step_update":{"step_type":"user_input","text_delta":"the prompt echoed"}}`,
		`{"event":"step_update","step_update":{"conversation_id":"step-session","step_type":"new_tool_type","text_delta":"tool detail"}}`,
		`{"event":"step_update","step_update":{"step_type":"new_empty_type"}}`,
		`{"event":"step_update","step_update":{"step_type":"agent_response","text_delta":"answer"}}`,
		`{"event":"step_update","step_update":{"step_type":"agent_response","text_delta":"\n"}}`,
	} {
		if !parse([]byte(line), emit, &res) {
			t.Fatalf("unparsed: %s", line)
		}
	}
	if text.String() != "tool detailanswer\n" || res.Final != "answer\n" || res.Session != "step-session" || res.Status == StatusError {
		t.Fatalf("text %q, result %+v", text.String(), res)
	}
	// A result without a response keeps the streamed answer.
	if !parse([]byte(`{"event":"result","result":{"status":"SUCCESS","num_turns":1}}`), emit, &res) {
		t.Fatal("unparsed result")
	}
	if res.Final != "answer\n" || res.Status != StatusOK || res.Turns != 1 {
		t.Fatalf("a result without a response lost the answer: %+v", res)
	}
	if parse([]byte("not JSON"), emit, &res) {
		t.Fatal("accepted non-JSON")
	}
	other := NewAgyBackend().newParse()
	var fresh Result
	other([]byte(`{"event":"step_update","step_update":{"step_type":"agent_response","text_delta":"fresh"}}`), func(Event) {}, &fresh)
	if fresh.Final != "fresh" {
		t.Fatalf("shared parser state: %+v", fresh)
	}
}

func TestAgyUsesResultTotalsAndReportsModelsOnlyWhenNamed(t *testing.T) {
	for _, location := range []string{"top", "init", "step_update", "result"} {
		parse := NewAgyBackend().newParse()
		res := Result{Backend: "agy"}
		line := `{"event":"init","model":"served-model"}`
		if location != "top" {
			line = fmt.Sprintf(`{"event":%q,%q:{"model":"served-model"}}`, location, location)
		}
		parse([]byte(line), func(Event) {}, &res)
		if res.Model != "served-model" || !res.ModelReported {
			t.Fatalf("%s: %+v", location, res)
		}
	}
	parse := NewAgyBackend().newParse()
	var res = Result{Backend: "agy"}
	for _, line := range []string{
		`{"event":"step_update","step_update":{"step_type":"agent_response","text_delta":"draft","usage":{"input_tokens":20,"output_tokens":5}}}`,
		`{"event":"result","result":{"conversation_id":"result-session","status":"SUCCESS","response":"final","num_turns":2,"usage":{"input_tokens":30,"output_tokens":10,"thinking_tokens":8,"cache_read_tokens":7}}}`,
	} {
		parse([]byte(line), func(Event) {}, &res)
	}
	if res.Final != "final" || res.Turns != 2 || res.Session != "result-session" || res.InputTokens != 30 || res.OutputTokens != 10 || res.CacheRead != 7 {
		t.Fatalf("result: %+v", res)
	}
	if total, known, estimated := res.ModelTokens(); total != 47 || !known || estimated {
		t.Fatalf("total %d, known %v, estimated %v", total, known, estimated)
	}
	if total, known, _ := (Result{Backend: "agy", InputTokens: 30, OutputTokens: 10, CacheRead: 7}).ModelTokens(); total != 47 || !known {
		t.Fatalf("inferred usage: %d, %v", total, known)
	}
}

func TestAgyFindsTheCLIOnPathBeforeItsInstallDirectory(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PATH", root)
	t.Setenv("HOME", root)
	t.Setenv("USERPROFILE", root)
	t.Setenv("LOCALAPPDATA", root)
	name, fallback := "agy", filepath.Join(root, ".local", "bin", "agy")
	if runtime.GOOS == "windows" {
		name, fallback = "agy.exe", filepath.Join(root, "agy", "bin", "agy.exe")
	}
	if err := os.MkdirAll(filepath.Dir(fallback), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fallback, nil, 0700); err != nil {
		t.Fatal(err)
	}
	b := NewAgyBackend()
	check := func(want string) {
		t.Helper()
		got, err := b.Detect()
		got, resolveErr := filepath.EvalSymlinks(got)
		want, _ = filepath.EvalSymlinks(want)
		if err != nil || resolveErr != nil || got != want {
			t.Fatalf("Detect = %q, %v, %v; want %q", got, err, resolveErr, want)
		}
	}
	check(fallback)
	onPath := filepath.Join(root, name)
	if err := os.WriteFile(onPath, nil, 0700); err != nil {
		t.Fatal(err)
	}
	check(onPath)
	if err := os.Remove(onPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(fallback); err != nil {
		t.Fatal(err)
	}
	ide := filepath.Join(root, "Programs", "Antigravity", "bin", "antigravity.exe")
	if err := os.MkdirAll(filepath.Dir(ide), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ide, nil, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Detect(); err == nil {
		t.Fatal("found the IDE as the CLI")
	}
}

func TestAgyFixtureContainsOnlyTheAnonymisedConversation(t *testing.T) {
	f, err := os.Open("testdata/agy.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	parse := NewAgyBackend().newParse()
	var res Result
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if !parse(sc.Bytes(), func(Event) {}, &res) {
			t.Fatalf("unparsed fixture: %s", sc.Text())
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if res.Session != "example-conversation" {
		t.Fatalf("session %q", res.Session)
	}
}
