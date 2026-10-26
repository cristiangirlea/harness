package watch

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/runlog"
)

func turnEvent(model string, ctx, cacheRead int64) agent.Event {
	return agent.Event{Kind: agent.EventUsage, Usage: &agent.Usage{Model: model, InputTokens: ctx - cacheRead, CacheRead: cacheRead, ContextTokens: ctx}}
}

// chartOf is the usage card of a page, from its opening tag to its end.
func chartOf(t *testing.T, body string) string {
	t.Helper()
	start := strings.Index(body, `<section class="card usage"`)
	if start < 0 {
		t.Fatalf("no usage chart in:\n%s", body)
	}
	end := strings.Index(body[start:], "</section>")
	return body[start : start+end]
}

func TestTheRunPageChartsContextPerTurnAndMarksCompactions(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	w, err := runlog.Create("claude", agent.Task{Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}
	w.Event(turnEvent(hostile, 10000, 0))
	w.Event(agent.Event{Kind: agent.EventText, Text: "working"})
	w.Event(turnEvent("claude-opus-5", 50000, 10000))
	w.Event(agent.Event{Kind: agent.EventCompaction, Text: hostile, Usage: &agent.Usage{ContextTokens: 52000}})
	w.Event(turnEvent("claude-opus-5", 9000, 0))
	w.Event(turnEvent("claude-opus-5", 80000, 9000))
	w.Event(turnEvent("claude-opus-5", 30000, 30000))
	if err := w.Close(agent.Result{Status: agent.StatusOK}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(context.Background(), Options{}))
	defer srv.Close()
	code, body := get(t, srv.URL+"/run/"+w.ID())
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	chart := chartOf(t, body)
	for _, want := range []string{
		`<svg class="usage-chart"`, `role="img"`, `<polyline class="chart-line"`,
		"5 turns", "peak context 80.0k", "49.0k cache read",
		`class="chart-mark"`, `class="chart-mark probable"`,
		"compacted before turn 3", "probably compacted before turn 5",
		"<table", "<td class=\"num\">80000</td>",
	} {
		if !strings.Contains(chart, want) {
			t.Errorf("the chart lacks %q:\n%s", want, chart)
		}
	}
	// A record's text reaches the chart escaped, and the chart needs no
	// inline style or script: the page's policy allows neither.
	if strings.Contains(chart, "<script>") || strings.Contains(chart, "<img") || !strings.Contains(chart, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Errorf("the chart holds a record's text unescaped:\n%s", chart)
	}
	if strings.Contains(chart, "style=") || strings.Contains(chart, "<style") {
		t.Errorf("the chart carries inline style:\n%s", chart)
	}
	// Usage is drawn, not listed: the timeline keeps its steps and the compaction.
	if strings.Contains(body, `<span class="tag">usage</span>`) {
		t.Error("usage events are listed in the timeline")
	}
	if !strings.Contains(body, `<span class="tag">compaction</span>`) {
		t.Error("the recorded compaction is missing from the timeline")
	}
}

func TestTheRunPageHasNoChartWithoutUsage(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	w, err := runlog.Create("codex", agent.Task{Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}
	w.Event(agent.Event{Kind: agent.EventText, Text: "done"})
	if err := w.Close(agent.Result{Status: agent.StatusOK, InputTokens: 900}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(context.Background(), Options{}))
	defer srv.Close()
	if _, body := get(t, srv.URL+"/run/"+w.ID()); strings.Contains(body, "usage-chart") {
		t.Error("a run without usage events got a chart")
	}
}

// The chart follows a running run: a new turn redraws it over every turn
// so far, those before the page was opened included.
func TestTheRunStreamRedrawsTheChartAsTurnsArrive(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	w, err := runlog.Create("claude", agent.Task{Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close(agent.Result{Status: agent.StatusOK}) })
	w.Event(turnEvent("m", 10000, 0))
	srv := httptest.NewServer(New(context.Background(), Options{Poll: 5 * time.Millisecond}))
	defer srv.Close()
	_, page := get(t, srv.URL+"/run/"+w.ID())
	if !strings.Contains(chartOf(t, page), "1 turn ") {
		t.Fatalf("the page lacks the first turn:\n%s", chartOf(t, page))
	}
	i := strings.Index(page, `data-stream="`)
	if i < 0 {
		t.Fatal("a running run's page has no stream")
	}
	url := page[i+len(`data-stream="`):]
	url = strings.ReplaceAll(url[:strings.Index(url, `"`)], "&amp;", "&")
	st, closeStream := openStream(t, srv.URL+url, "")
	defer closeStream()

	w.Event(turnEvent("m", 30000, 10000))
	m, ok := st.next(t)
	if !ok || m.event != "usage" || m.id == "" {
		t.Fatalf("message = %+v, want the chart redrawn", m)
	}
	if !strings.Contains(m.data, "2 turns") || !strings.Contains(m.data, "peak context 30.0k") {
		t.Errorf("the redrawn chart does not hold both turns:\n%s", m.data)
	}

	// A browser that reconnects after that message gets the earlier turns too.
	w.Event(turnEvent("m", 40000, 30000))
	st2, close2 := openStream(t, srv.URL+"/stream/run/"+w.ID(), m.id)
	defer close2()
	m2, ok := st2.next(t)
	if !ok || m2.event != "usage" || !strings.Contains(m2.data, "3 turns") {
		t.Errorf("after a reconnect: %+v", m2)
	}
}

// Turns that arrive together redraw the chart once, after the steps they
// came with, not once a turn.
func TestTheRunStreamRedrawsTheChartOncePerPoll(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	w, err := runlog.Create("claude", agent.Task{Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close(agent.Result{Status: agent.StatusOK}) })
	w.Event(turnEvent("m", 10000, 0))
	srv := httptest.NewServer(New(context.Background(), Options{Poll: 5 * time.Millisecond}))
	defer srv.Close()
	_, page := get(t, srv.URL+"/run/"+w.ID())
	i := strings.Index(page, `data-stream="`)
	if i < 0 {
		t.Fatal("a running run's page has no stream")
	}
	url := page[i+len(`data-stream="`):]
	url = strings.ReplaceAll(url[:strings.Index(url, `"`)], "&amp;", "&")
	// Written before the stream opens, so its first poll reads them all.
	w.Event(turnEvent("m", 20000, 10000))
	w.Event(agent.Event{Kind: agent.EventText, Text: "working"})
	w.Event(turnEvent("m", 30000, 20000))
	w.Event(turnEvent("m", 40000, 30000))
	st, closeStream := openStream(t, srv.URL+url, "")
	defer closeStream()
	m, ok := st.next(t)
	if !ok || m.event != "item" || !strings.Contains(m.data, "working") {
		t.Fatalf("first message = %+v, want the step", m)
	}
	m, ok = st.next(t)
	if !ok || m.event != "usage" || !strings.Contains(m.data, "4 turns") {
		t.Fatalf("second message = %+v, want the chart over all four turns", m)
	}
	w.Event(agent.Event{Kind: agent.EventText, Text: "still working"})
	if m, ok = st.next(t); !ok || m.event != "item" {
		t.Errorf("next message = %+v, want the new step and no other chart", m)
	}
}

// A long run draws its line through every turn but a point and a row for
// about 500 of them: the last, each compaction, and an even spread.
func TestTheChartThinsItsPointsAndRowsPastFiveHundredTurns(t *testing.T) {
	var events []agent.Event
	for n := 1; n <= 3000; n++ {
		if n == 1234 {
			events = append(events, agent.Event{Kind: agent.EventCompaction, Text: "auto"})
		}
		events = append(events, turnEvent("m", int64(1000+n), 0))
	}
	v := usageOf(events)
	if v.Turns != 3000 || strings.Count(v.Panels[0].Points, ",") != 3000 {
		t.Fatalf("turns %d, line points %d: want every turn on the line", v.Turns, strings.Count(v.Panels[0].Points, ","))
	}
	if len(v.Rows) > 520 || len(v.Rows) < 450 || len(v.Panels[0].Dots) != len(v.Rows) || len(v.Panels[1].Dots) != len(v.Rows) {
		t.Fatalf("rows %d, dots %d and %d: want about 500 each", len(v.Rows), len(v.Panels[0].Dots), len(v.Panels[1].Dots))
	}
	shown := map[int]bool{}
	for _, r := range v.Rows {
		shown[r.N] = true
	}
	if !shown[1] || !shown[3000] || !shown[1234] {
		t.Errorf("the first, last and compacted turns must keep their rows")
	}
	html, err := render("usage", v)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(html, "every turn") || len(html) > 300_000 {
		t.Errorf("the thinned chart is %d bytes and must say it is thinned:\n%.2000s", len(html), html)
	}
	if few := usageOf(events[:200]); len(few.Rows) != 200 || few.Every > 1 {
		t.Errorf("a short run lost rows: %d of 200", len(few.Rows))
	}
}

// The page's own pass blanks only the environment's secrets; a model name is
// also masked like any record text, so a key shaped like one stays hidden.
func TestTheChartMasksAKeyShapedModelName(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	const secret = "sk-ant-Abcdefghijklmnop"
	w, err := runlog.Create("claude", agent.Task{Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}
	w.Event(turnEvent(secret, 10000, 0))
	if err := w.Close(agent.Result{Status: agent.StatusOK}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(context.Background(), Options{}))
	defer srv.Close()
	if _, page := get(t, srv.URL+"/run/"+w.ID()); strings.Contains(page, secret) {
		t.Errorf("the page shows a key-shaped model name:\n%s", chartOf(t, page))
	}
}

// A model name comes from a CLI's output or a record, so it is blanked of
// the environment's secrets like every other text the page shows.
func TestTheChartBlanksASecretInAModelName(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	const secret = "Abcdefghijklmnop"
	t.Setenv("SERVICE_TOKEN", secret)
	w, err := runlog.Create("claude", agent.Task{Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close(agent.Result{Status: agent.StatusOK}) })
	w.Event(turnEvent("model-"+secret, 10000, 0))
	srv := httptest.NewServer(New(context.Background(), Options{Poll: 5 * time.Millisecond}))
	defer srv.Close()
	_, page := get(t, srv.URL+"/run/"+w.ID())
	if chart := chartOf(t, page); strings.Contains(chart, secret) {
		t.Errorf("the chart shows the secret in a model name:\n%s", chart)
	}
	i := strings.Index(page, `data-stream="`)
	if i < 0 {
		t.Fatal("a running run's page has no stream")
	}
	url := page[i+len(`data-stream="`):]
	url = strings.ReplaceAll(url[:strings.Index(url, `"`)], "&amp;", "&")
	st, closeStream := openStream(t, srv.URL+url, "")
	defer closeStream()

	w.Event(turnEvent("model-"+secret, 30000, 10000))
	m, ok := st.next(t)
	if !ok || m.event != "usage" {
		t.Fatalf("message = %+v, want the chart redrawn", m)
	}
	if strings.Contains(m.data, secret) {
		t.Errorf("the streamed chart shows the secret in a model name:\n%s", m.data)
	}
}
