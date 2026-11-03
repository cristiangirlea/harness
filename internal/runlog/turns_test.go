package runlog

import (
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/kernel"
)

func usageEvent(ctx, cacheRead int64) kernel.Event {
	return kernel.Event{Kind: kernel.EventUsage, Usage: &kernel.Usage{InputTokens: ctx - cacheRead, CacheRead: cacheRead, ContextTokens: ctx}}
}

func TestTurnsNumberEachUsageAndMarkCompactions(t *testing.T) {
	events := []kernel.Event{
		{Kind: kernel.EventText, Text: "hi"},
		usageEvent(10000, 0),
		usageEvent(50000, 10000),
		{Kind: kernel.EventCompaction, Text: "auto", Usage: &kernel.Usage{ContextTokens: 52000}},
		usageEvent(9000, 0),
		usageEvent(80000, 9000),
		usageEvent(30000, 30000), // fell by more than half, with nothing recorded
		usageEvent(16000, 15000), // fell by less than half
	}
	turns := Turns(events)
	want := []struct {
		n             int
		ctx, soFar    int64
		compaction    string
		compactedFrom int64
	}{
		{1, 10000, 0, "", 0},
		{2, 50000, 10000, "", 0},
		{3, 9000, 10000, CompactionRecorded, 52000},
		{4, 80000, 19000, "", 0},
		{5, 30000, 49000, CompactionProbable, 0},
		{6, 16000, 64000, "", 0},
	}
	if len(turns) != len(want) {
		t.Fatalf("got %d turns, want %d: %+v", len(turns), len(want), turns)
	}
	for i, w := range want {
		got := turns[i]
		if got.N != w.n || got.ContextTokens != w.ctx || got.CacheReadSoFar != w.soFar || got.Compaction != w.compaction ||
			got.CompactedFrom != w.compactedFrom {
			t.Errorf("turn %d = %+v, want %+v", i+1, got, w)
		}
	}
}

func TestTurnsOfARunWithoutUsageAreNone(t *testing.T) {
	if turns := Turns([]kernel.Event{{Kind: kernel.EventText, Text: "old record"}}); len(turns) != 0 {
		t.Errorf("turns = %+v", turns)
	}
}

// TurnsOf reads only a run's usage and compaction lines from disk.
func TestTurnsOfReadsARecordedRun(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	w, err := Create("claude", kernel.Task{Prompt: "go", Dir: t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	w.Event(kernel.Event{Time: at, Kind: kernel.EventText, Text: `a text that quotes "kind":"usage"`})
	e := usageEvent(1000, 0)
	e.Time = at
	w.Event(e)
	if err := w.Close(kernel.Result{Status: kernel.StatusOK}); err != nil {
		t.Fatal(err)
	}
	turns, err := TurnsOf(w.ID())
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || turns[0].ContextTokens != 1000 || !turns[0].Time.Equal(at) {
		t.Errorf("turns = %+v", turns)
	}
}

// A tool's input is recorded as given, so it can hold the usage kind as
// written; only events of the two kinds come back.
func TestUsageEventsKeepsOnlyUsageAndCompactionEvents(t *testing.T) {
	stream := `{"t":"2026-10-13T09:00:00Z","kind":"tool_call","tool":"x","input":{"kind":"usage"}}` + "\n" +
		`{"t":"2026-10-13T09:00:01Z","kind":"usage","usage":{"context_tokens":1000}}` + "\n" +
		`{"t":"2026-10-13T09:00:02Z","kind":"text","text":"no"}` + "\n" +
		`{"t":"2026-10-13T09:00:03Z","kind":"compaction","text":"auto"}` + "\n"
	events, err := UsageEvents(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Kind != kernel.EventUsage || events[1].Kind != kernel.EventCompaction {
		t.Errorf("events = %+v", events)
	}
}
