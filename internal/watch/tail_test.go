package watch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/runlog"
)

func appendFile(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

func texts(lines []Line) []string {
	var out []string
	for _, l := range lines {
		if l.Err != nil {
			out = append(out, "ERR "+l.Err.Error())
			continue
		}
		out = append(out, l.Event.Text)
	}
	return out
}

func TestTailReadsNothingFromAFileThatDoesNotExistYet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	tail := &Tail{Path: path}
	lines, err := tail.Read()
	if err != nil || len(lines) != 0 || tail.Offset != 0 {
		t.Fatalf("Read on a missing file = %v, %v, offset %d; want nothing and no error", lines, err, tail.Offset)
	}
	appendFile(t, path, `{"kind":"text","text":"first"}`+"\n")
	lines, err = tail.Read()
	if err != nil || strings.Join(texts(lines), ",") != "first" {
		t.Fatalf("after the file appeared: %v, %v", texts(lines), err)
	}
}

func TestTailLeavesAPartlyWrittenLastLineForLater(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	first := `{"t":"2026-10-10T10:00:00Z","kind":"text","text":"a"}` + "\n"
	appendFile(t, path, first+`{"kind":"tool_call","tool":"Ba`)
	tail := &Tail{Path: path}
	lines, err := tail.Read()
	if err != nil || strings.Join(texts(lines), ",") != "a" {
		t.Fatalf("lines = %v, %v; want only the complete one", texts(lines), err)
	}
	if tail.Offset != int64(len(first)) || lines[0].End != tail.Offset {
		t.Fatalf("offset = %d, line end = %d; want both at the end of the complete line (%d)", tail.Offset, lines[0].End, len(first))
	}
	if again, _ := tail.Read(); len(again) != 0 {
		t.Fatalf("the half line came back before it was finished: %v", texts(again))
	}
	appendFile(t, path, `sh","text":"b"}`+"\r\n")
	lines, err = tail.Read()
	if err != nil || len(lines) != 1 || lines[0].Event.Tool != "Bash" || lines[0].Event.Text != "b" {
		t.Fatalf("finished line = %+v, %v", lines, err)
	}
}

func TestTailFinishReportsALastLineThatWillNeverBeCompleted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	appendFile(t, path, `{"kind":"text","text":"a"}`+"\n"+`{"kind":"te`)
	tail := &Tail{Path: path}
	lines, err := tail.Finish()
	got := texts(lines)
	if err != nil || len(got) != 2 || got[0] != "a" || !strings.HasPrefix(got[1], "ERR ") {
		t.Fatalf("Finish = %v, %v; want the line and an error for the cut-off one", got, err)
	}
}

func TestTailKeepsGoingPastAnUnreadableLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	appendFile(t, path, "not json\n\n"+`{"kind":"info","text":"after"}`+"\n")
	lines, err := (&Tail{Path: path}).Read()
	got := texts(lines)
	if err != nil || len(got) != 2 || !strings.HasPrefix(got[0], "ERR ") || got[1] != "after" {
		t.Fatalf("lines = %v, %v; want an error for the bad line, the blank one skipped, then the event", got, err)
	}
}

// collect is an emit callback that tests can read while Follow writes.
type collect struct {
	mu    sync.Mutex
	lines []Line
}

func (c *collect) add(l Line) { c.mu.Lock(); c.lines = append(c.lines, l); c.mu.Unlock() }

func (c *collect) waitFor(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		got := len(c.lines)
		c.mu.Unlock()
		if got >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("waited for %d lines", n)
}

func TestFollowReturnsTheResultOnceTheRunFinishes(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	w, err := runlog.Create("fake", agent.Task{Prompt: "do it"})
	if err != nil {
		t.Fatal(err)
	}
	w.Event(agent.Event{Kind: agent.EventText, Text: "starting"})

	var got collect
	type outcome struct {
		rec runlog.Record
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		rec, err := Follow(context.Background(), w.ID(), 0, 5*time.Millisecond, got.add)
		done <- outcome{rec, err}
	}()
	got.waitFor(t, 1)
	select {
	case o := <-done:
		t.Fatalf("Follow returned while the run was going: %+v", o)
	default:
	}
	w.Event(agent.Event{Kind: agent.EventToolCall, Tool: "Bash", Text: "go test"})
	w.Event(agent.Event{Kind: agent.EventToolResult, Text: "ok"})
	if err := w.Close(agent.Result{Backend: "fake", Status: agent.StatusOK, Turns: 2, CostUSD: 0.5}); err != nil {
		t.Fatal(err)
	}
	select {
	case o := <-done:
		if o.err != nil || !o.rec.Done || o.rec.Result.Status != agent.StatusOK || o.rec.Result.CostUSD != 0.5 {
			t.Fatalf("Follow = %+v, %v; want the finished record", o.rec, o.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Follow did not return after the run finished")
	}
	if strings.Join(texts(got.lines), ",") != "starting,go test,ok" {
		t.Fatalf("events = %v", texts(got.lines))
	}
}

func TestFollowStopsWhenItsContextEnds(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	w, err := runlog.Create("fake", agent.Task{Prompt: "forever"})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close(agent.Result{})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := Follow(ctx, w.ID(), 0, 5*time.Millisecond, func(Line) {}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the context's", err)
	}
}

func TestFollowRefusesARunThatDoesNotExist(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	if _, err := Follow(context.Background(), "nope", 0, time.Millisecond, func(Line) {}); err == nil {
		t.Fatal("want an error for a run that is not there")
	}
	// A finished run planted where ../escape leads: only the id check stops it.
	w, err := runlog.Create("fake", agent.Task{Prompt: "planted"})
	if err != nil {
		t.Fatal(err)
	}
	_ = w.Close(agent.Result{Status: agent.StatusOK})
	if err := os.Rename(w.Dir(), filepath.Join(home, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := Follow(context.Background(), "../escape", 0, time.Millisecond, func(Line) {}); err == nil {
		t.Fatal("want an error for an id that leaves the runs directory")
	}
}

// Close writes result.json in place, so a reader can catch it half written:
// that is a run not finished yet, with its record from task.json.
func TestAResultStillBeingWrittenIsNotTheEnd(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	w, err := runlog.Create("fake", agent.Task{Prompt: "half"})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close(agent.Result{})
	if err := os.WriteFile(filepath.Join(w.Dir(), "result.json"), []byte(`{"id":`), 0o644); err != nil {
		t.Fatal(err)
	}
	f, rec, err := NewFollower(w.ID(), 0)
	if err != nil || rec.ID != w.ID() || rec.Done {
		t.Fatalf("NewFollower = %+v, %v; want the running record from task.json", rec, err)
	}
	_, rec, done, err := f.Poll()
	if err != nil || done || rec.ID != w.ID() || rec.Task.Prompt != "half" {
		t.Fatalf("Poll = %+v, done %v, %v; want not done, with the record from task.json", rec, done, err)
	}
}

func TestTailSaysWhenItsFileShrank(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	appendFile(t, path, `{"kind":"text","text":"a"}`+"\n")
	tail := &Tail{Path: path}
	if _, err := tail.Read(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := tail.Read(); err == nil || !strings.Contains(err.Error(), "shorter") {
		t.Fatalf("err = %v; want the file named as shorter than what was read", err)
	}
}
