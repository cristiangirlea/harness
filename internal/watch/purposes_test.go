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

// saveRun writes a run record; a nil result leaves it running until the test ends.
func saveRun(t *testing.T, backend, prompt string, res *agent.Result) string {
	t.Helper()
	w, err := runlog.Create(backend, agent.Task{Prompt: prompt})
	if err != nil {
		t.Fatal(err)
	}
	if res == nil {
		t.Cleanup(func() { _ = w.Close(agent.Result{}) })
		return w.ID()
	}
	res.Backend = backend
	if res.Status == "" {
		res.Status = agent.StatusOK
	}
	if err := w.Close(*res); err != nil {
		t.Fatal(err)
	}
	return w.ID()
}

// cardOf builds the card over the runs on disk, as the index lists them.
func cardOf(t *testing.T) *purposeCard {
	t.Helper()
	runs, err := runlog.List(50)
	if err != nil {
		t.Fatal(err)
	}
	return purposesOf(runs)
}

func purposeRowOf(t *testing.T, c *purposeCard, name string) purposeRow {
	t.Helper()
	if c == nil {
		t.Fatal("no card")
	}
	for _, r := range c.Rows {
		if r.Purpose == name {
			return r
		}
	}
	t.Fatalf("the card has no %q row: %+v", name, c.Rows)
	return purposeRow{}
}

func TestAPurposeWithSomeCostsUnreportedReadsAsALowerBound(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	saveRun(t, "claude", "fix the login bug", &agent.Result{CostUSD: 0.0123})
	saveRun(t, "codex", "fix the logout bug", &agent.Result{})
	saveRun(t, "claude", "review the diff", &agent.Result{CostUSD: 0.01})
	saveRun(t, "codex", "review the branch", &agent.Result{})
	saveRun(t, "gemini", "review the plan", &agent.Result{Status: agent.StatusError})

	c := cardOf(t)
	if r := purposeRowOf(t, c, "debug"); r.Cost != "≥ $0.0123" || r.NoCost != "1 run reports no cost" || r.Runs != "2" {
		t.Errorf("debug row = %+v", r)
	}
	if r := purposeRowOf(t, c, "review"); r.Cost != "≥ $0.0100" || r.NoCost != "2 runs report no cost" {
		t.Errorf("review row = %+v", r)
	}
	if r := purposeRowOf(t, c, "all"); r.Cost != "≥ $0.0223" || r.NoCost != "3 runs report no cost" || r.Runs != "5" {
		t.Errorf("all row = %+v", r)
	}
	if !c.Partial {
		t.Error("the card does not say its costs are a lower bound")
	}
}

func TestAllCostsKnownGiveTheSumAndNoneKnownGiveADash(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	saveRun(t, "claude", "fix the login bug", &agent.Result{CostUSD: 0.01, InputTokens: 1000, OutputTokens: 200, CacheRead: 300, CacheWrite: 5000})
	saveRun(t, "claude", "fix the logout bug", &agent.Result{CostUSD: 0.02})
	saveRun(t, "codex", "add a cache", &agent.Result{InputTokens: 40})

	c := cardOf(t)
	if r := purposeRowOf(t, c, "debug"); r.Cost != "$0.0300" || r.NoCost != "" || r.Tokens != "1.5k" {
		t.Errorf("debug row = %+v", r)
	}
	if r := purposeRowOf(t, c, "implement"); r.Cost != "-" || r.Tokens != "40" {
		t.Errorf("implement row = %+v", r)
	}
}

func TestRunningRunsCountOnlyAsRunning(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	saveRun(t, "claude", "fix the login bug", &agent.Result{CostUSD: 0.01})
	saveRun(t, "codex", "fix the logout bug", nil)
	saveRun(t, "codex", "add a cache", nil)

	c := cardOf(t)
	if r := purposeRowOf(t, c, "debug"); r.Runs != "2" || r.Running != "1" || r.Cost != "$0.0100" || r.NoCost != "" {
		t.Errorf("debug row = %+v", r)
	}
	if r := purposeRowOf(t, c, "implement"); r.Running != "1" || r.Cost != "-" || r.NoCost != "" {
		t.Errorf("implement row = %+v", r)
	}
	if r := purposeRowOf(t, c, "all"); r.Running != "2" || r.Cost != "$0.0100" || r.NoCost != "" {
		t.Errorf("all row = %+v", r)
	}
	if c.Partial {
		t.Error("running runs make the card's costs a lower bound")
	}
}

func TestAnEstimatedCostKeepsItsTilde(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	saveRun(t, "api", "add a cache", &agent.Result{CostUSD: 0.5, CostEstimate: true})
	saveRun(t, "claude", "add a flag", &agent.Result{CostUSD: 0.25})
	saveRun(t, "codex", "add a log line", &agent.Result{})

	if r := purposeRowOf(t, cardOf(t), "implement"); r.Cost != "≥ ~$0.7500" {
		t.Errorf("implement row = %+v", r)
	}
}

func TestPurposeRowsComeInCategoryOrderThenAll(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	for _, p := range []string{"hello there", "add a cache", "fix the bug", "review the diff"} {
		saveRun(t, "claude", p, &agent.Result{CostUSD: 0.01})
	}
	c := cardOf(t)
	var got []string
	for _, r := range c.Rows {
		got = append(got, r.Purpose)
	}
	if want := "review debug implement unknown all"; strings.Join(got, " ") != want {
		t.Errorf("rows = %q, want %q", got, want)
	}
	if !strings.Contains(c.Scope, "newest 4 runs") {
		t.Errorf("the card does not say what it covers: %q", c.Scope)
	}
}

func TestAnEmptyHomeShowsNoPurposeCard(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	if c := cardOf(t); c != nil {
		t.Errorf("card over no runs = %+v", c)
	}
	srv := httptest.NewServer(New(context.Background(), Options{Poll: 5 * time.Millisecond}))
	defer srv.Close()
	if _, body := get(t, srv.URL+"/"); strings.Contains(body, "What the runs were for") {
		t.Error("the index of an empty home shows the purpose card")
	}
}

func TestTheIndexAndItsStreamShowPurposes(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	fix := saveRun(t, "claude", "fix the login bug", &agent.Result{CostUSD: 0.0123})
	saveRun(t, "codex", "fix the logout bug", &agent.Result{})
	other := saveRun(t, "claude", "hello there", &agent.Result{})

	srv := httptest.NewServer(New(context.Background(), Options{Poll: 5 * time.Millisecond}))
	defer srv.Close()
	_, page := get(t, srv.URL+"/")
	s, stop := openStream(t, srv.URL+"/stream/index", "")
	defer stop()
	m, ok := s.next(t)
	if !ok || m.event != "replace" {
		t.Fatalf("first message = %+v, %v", m, ok)
	}
	for where, html := range map[string]string{"the index": page, "the streamed index": m.data} {
		card := strings.Index(html, "What the runs were for")
		if card < 0 || card > strings.Index(html, `id="batches-h"`) {
			t.Errorf("%s has no purpose card above the batches", where)
		}
		for _, want := range []string{"newest 3 runs", "≥ $0.0123", "1 run reports no cost", `<th scope="col">Purpose</th>`} {
			if !strings.Contains(html, want) {
				t.Errorf("%s lacks %q", where, want)
			}
		}
		if row := runRowOf(t, html, fix); !strings.Contains(row, `title="debug.fix">debug</td>`) {
			t.Errorf("%s: the purpose cell lacks its rule id:\n%s", where, row)
		}
		if row := runRowOf(t, html, other); !strings.Contains(row, `<td class="meta">unknown</td>`) {
			t.Errorf("%s: an unknown purpose has a title or no cell:\n%s", where, row)
		}
	}
}
