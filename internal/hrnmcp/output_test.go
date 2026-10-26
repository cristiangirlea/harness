package hrnmcp

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/runlog"
)

type outputBackend struct {
	stubBackend
	events []agent.Event
}

func (b outputBackend) Run(_ context.Context, _ agent.Task, emit func(agent.Event)) agent.Result {
	for _, e := range b.events {
		emit(e)
	}
	return agent.Result{Backend: "stub", Status: agent.StatusOK}
}

func TestRunProgressCannotSendTerminalControls(t *testing.T) {
	const bad = "x\x1b[31m\x07\u009b"
	const want = "x\ufffd[31m\ufffd\ufffd"
	orig, err := agent.Get("stub")
	if err != nil {
		t.Fatal(err)
	}
	agent.Register(outputBackend{events: []agent.Event{
		{Kind: agent.EventToolCall, Tool: bad + "\n\ttool", Text: bad},
		{Kind: agent.EventToolCall, Tool: strings.Repeat("t", 81)},
		{Kind: agent.EventError, Text: bad},
	}})
	t.Cleanup(func() { agent.Register(orig) })
	s := connect(t, t.TempDir())
	s.call(t, "run", `{"prompt":"hello","backend":"stub","no_switch":true}`)
	s.mu.Lock()
	progress := strings.Join(s.progress, "\n")
	s.mu.Unlock()
	for _, line := range []string{"→ " + want + " tool " + want, "→ " + strings.Repeat("t", 80) + "…", "error: " + want} {
		if !strings.Contains(progress, line) {
			t.Errorf("progress missing filtered line %q: %q", line, progress)
		}
	}
	if strings.ContainsAny(progress, "\x1b\x07\u009b") {
		t.Errorf("progress carries terminal controls: %q", progress)
	}
}

func TestRunResultCannotSendTerminalControls(t *testing.T) {
	const bad = "x\x1b[31m\x07\u009b\n\tsecond line"
	const want = "x\ufffd[31m\ufffd\ufffd\n\tsecond line"
	for _, status := range []agent.Status{agent.StatusOK, agent.StatusError} {
		t.Run(string(status), func(t *testing.T) {
			orig, err := agent.Get("stub")
			if err != nil {
				t.Fatal(err)
			}
			agent.Register(scripted{"stub", func(agent.Task) agent.Result {
				res := agent.Result{Status: status, Final: bad}
				if status == agent.StatusError {
					res.Error = bad
				}
				return res
			}})
			t.Cleanup(func() { agent.Register(orig) })
			s := connect(t, t.TempDir())
			res := s.call(t, "run", `{"prompt":"hello","backend":"stub","no_switch":true}`)
			if res.IsError != (status == agent.StatusError) {
				t.Fatalf("unexpected error status: %s", res.Text())
			}
			if strings.ContainsAny(res.Text(), "\x1b\x07\u009b") || !strings.Contains(res.Text(), want) {
				t.Errorf("run text did not replace terminal controls or preserve lines: %q", res.Text())
			}
			sc := res.StructuredContent.(map[string]any)
			result := sc["result"].(map[string]any)
			for _, field := range []string{"final", "error"} {
				if field == "error" && status == agent.StatusOK {
					continue
				}
				if got := result[field]; got != want {
					t.Errorf("structured %s = %q, want %q", field, got, want)
				}
			}
			rec, err := runlog.Load(sc["run_id"].(string))
			if err != nil {
				t.Fatal(err)
			}
			if rec.Result.Final != bad || (status == agent.StatusError && rec.Result.Error != bad) {
				t.Errorf("stored result was filtered: %+v", rec.Result)
			}
		})
	}
}

func TestShowEventsCannotSendTerminalControls(t *testing.T) {
	const bad = "x\x1b[31m\x07\u009b\n\tsecond line"
	const want = "x\ufffd[31m\ufffd\ufffd\n\tsecond line"
	const line = "x\ufffd[31m\ufffd\ufffd second line"
	for _, kind := range []agent.EventKind{agent.EventText, agent.EventThinking, agent.EventToolCall, agent.EventToolResult, agent.EventInfo, agent.EventError} {
		t.Run(string(kind), func(t *testing.T) {
			s := connect(t, t.TempDir())
			w, err := runlog.Create("stub", agent.Task{Prompt: "hello"})
			if err != nil {
				t.Fatal(err)
			}
			w.Event(agent.Event{Kind: kind, Tool: "tool\x1b\x07\u009b", Text: bad})
			if err := w.Close(agent.Result{Status: agent.StatusError, Error: bad, Final: bad}); err != nil {
				t.Fatal(err)
			}
			res := s.call(t, "show", `{"run_id":"`+w.ID()+`"}`)
			wantText := line
			if kind == agent.EventText || kind == agent.EventError {
				wantText = want
			}
			if strings.ContainsAny(res.Text(), "\x1b\x07\u009b") || (kind != agent.EventThinking && !strings.Contains(res.Text(), wantText)) {
				t.Errorf("show text did not replace terminal controls or preserve layout: %q", res.Text())
			}
			sc := res.StructuredContent.(map[string]any)
			events := sc["events"].([]any)
			event := events[0].(map[string]any)
			if event["text"] != want || event["tool"] != "tool\ufffd\ufffd\ufffd" {
				t.Errorf("structured event did not replace terminal controls: %#v", event)
			}
			result := sc["record"].(map[string]any)["result"].(map[string]any)
			if result["error"] != want || result["final"] != want {
				t.Errorf("structured result did not replace terminal controls: %#v", result)
			}
			eventsOnDisk, err := runlog.Events(w.ID())
			if err != nil {
				t.Fatal(err)
			}
			if eventsOnDisk[0].Text != bad {
				t.Errorf("stored event was filtered: %+v", eventsOnDisk[0])
			}
		})
	}
}

func TestShowToolArgumentsCannotSendTerminalControls(t *testing.T) {
	const bad = "x\x1b[31m\x07\u009b\n\tsecond line"
	const want = "x\ufffd[31m\ufffd\ufffd\n\tsecond line"
	s := connect(t, t.TempDir())
	w, err := runlog.Create("stub", agent.Task{Prompt: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	input, err := json.Marshal(map[string]any{bad: []any{bad, map[string]any{"nested": bad}}, "number": json.Number("9007199254740993"), "bool": true, "null": nil})
	if err != nil {
		t.Fatal(err)
	}
	w.Event(agent.Event{Kind: agent.EventToolCall, Tool: "tool", Input: input})
	if err := w.Close(agent.Result{Status: agent.StatusOK}); err != nil {
		t.Fatal(err)
	}
	res := s.call(t, "show", `{"run_id":"`+w.ID()+`"}`)
	event := res.StructuredContent.(map[string]any)["events"].([]any)[0].(map[string]any)
	args := event["input"].(map[string]any)
	if got := args[want]; !reflect.DeepEqual(got, []any{want, map[string]any{"nested": want}}) {
		t.Errorf("structured tool arguments did not replace terminal controls: %#v", args)
	}
	if _, ok := args[bad]; ok {
		t.Errorf("structured tool argument key carries terminal controls: %#v", args)
	}
	if args["bool"] != true || args["null"] != nil {
		t.Errorf("structured tool argument types changed: %#v", args)
	}
	if data := printableInput(input); !strings.Contains(string(data), "9007199254740993") {
		t.Errorf("tool argument number lost precision: %s", data)
	}
	events, err := runlog.Events(w.ID())
	if err != nil {
		t.Fatal(err)
	}
	if string(events[0].Input) != string(input) {
		t.Errorf("stored tool arguments were filtered: %s", events[0].Input)
	}
}

func TestPrintableInputKeepsKeysThatDifferOnlyInFilteredCharacters(t *testing.T) {
	for i := 0; i < 20; i++ {
		var got map[string]string
		in := json.RawMessage(`{"a\u001b":"one","a\u0007":"two","a\u009b":"three"}`)
		if err := json.Unmarshal(printableInput(in), &got); err != nil {
			t.Fatal(err)
		}
		values := []string{}
		for _, v := range got {
			values = append(values, v)
		}
		slices.Sort(values)
		if len(got) != 3 || !slices.Equal(values, []string{"one", "three", "two"}) {
			t.Fatalf("keys merged and hid an argument: %v", got)
		}
	}
}

func TestPrintableInputReplacesInvalidJSONWithFilteredText(t *testing.T) {
	for _, input := range []string{"{\x1b\x07\u009b", `{} "trailing"`} {
		var got string
		if err := json.Unmarshal(printableInput(json.RawMessage(input)), &got); err != nil {
			t.Fatalf("invalid input was not replaced with a JSON string: %v", err)
		}
		if want := printableFlow(input); got != want {
			t.Errorf("fallback = %q, want %q", got, want)
		}
	}
}
