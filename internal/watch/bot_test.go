package watch

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/runlog"
)

func TestTheBotSaysHowARunEndedInPlainEnglish(t *testing.T) {
	for _, c := range []struct {
		res        agent.Result
		kind, text string
	}{
		{agent.Result{Status: agent.StatusOK, Duration: 4 * time.Minute}, "done", "Claude has finished in harness. It took four minutes."},
		{agent.Result{Status: agent.StatusError, Error: "exit status 1\nmore detail", Duration: 2*time.Minute + 10*time.Second}, "failed", "Claude stopped with an error in harness after two minutes: exit status 1."},
		{agent.Result{Status: agent.StatusMaxTurns, Duration: time.Hour + 5*time.Minute}, "failed", "Claude ran out of turns in harness after an hour and five minutes."},
		{agent.Result{Status: agent.StatusTimeout, Duration: 20 * time.Minute}, "failed", "Claude ran out of time in harness after 20 minutes."},
		{agent.Result{Status: agent.StatusBudget, Duration: 45 * time.Second}, "failed", "Claude reached its budget in harness after 45 seconds."},
		{agent.Result{Status: agent.StatusCanceled, Duration: time.Minute}, "failed", "Claude's run in harness was cancelled after a minute."},
		{agent.Result{Status: agent.StatusRefused}, "failed", "Claude turned the task down in harness."},
		{agent.Result{Status: agent.StatusLimited, Duration: 3 * time.Minute}, "failed", "Claude hit its usage limit in harness after three minutes."},
	} {
		kind, text := runEnding("Claude", "harness", c.res)
		if kind != c.kind || text != c.text {
			t.Errorf("%s: got %s %q, want %s %q", c.res.Status, kind, text, c.kind, c.text)
		}
	}
	// No workspace: the sentence still reads.
	if _, text := runEnding("Codex", "", agent.Result{Status: agent.StatusOK, Duration: 30 * time.Second}); text != "Codex has finished. It took 30 seconds." {
		t.Errorf("without a place: %q", text)
	}
}

func TestTheBotNamesBackendsAndPlacesAsPeopleSayThem(t *testing.T) {
	for in, want := range map[string]string{"claude": "Claude", "codex": "Codex", "gemini": "Gemini", "opencode": "OpenCode", "api": "the API loop", "fake": "fake"} {
		if got := backendName(in); got != want {
			t.Errorf("backendName(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{`E:\__Workdir\personal\harness`: "harness", "/home/me/tidedesk/": "tidedesk", "": "", "/": ""} {
		if got := place(in); got != want {
			t.Errorf("place(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"Add the budget guard.\nDetails follow": "add the budget guard",
		"# Fix the flaky test":                  "fix the flaky test",
		"API keys must never reach a run":       "API keys must never reach a run",
		"I want the guard on every backend":     "I want the guard on every backend",
		"I'm adding the budget":                 "I'm adding the budget",
		"A flag for prune":                      "a flag for prune",
		"one two three four five six seven eight nine ten eleven twelve thirteen": "one two three four five six seven eight nine ten eleven twelve…",
		"   ": "",
	} {
		if got := topic(in); got != want {
			t.Errorf("topic(%q) = %q, want %q", in, got, want)
		}
	}
	// A question or an exclamation keeps its own mark.
	for in, want := range map[string]string{"is it done?": "Is it done?", "done!": "Done!", "done": "Done.", "cut short…": "Cut short…"} {
		if got := sentence(in); got != want {
			t.Errorf("sentence(%q) = %q, want %q", in, got, want)
		}
	}
	for d, want := range map[time.Duration]string{0: "under a second", time.Second: "a second", 9 * time.Second: "nine seconds",
		time.Minute: "a minute", 59 * time.Minute: "59 minutes", 2 * time.Hour: "two hours", time.Hour + time.Minute: "an hour and a minute"} {
		if got := spoken(d); got != want {
			t.Errorf("spoken(%s) = %q, want %q", d, got, want)
		}
	}
}

func TestTheBotSaysWhatAnAgentIsDoingFromItsLatestToolCall(t *testing.T) {
	for _, c := range []struct{ tool, summary, want string }{
		{"Read", `E:\repo\internal\watch\server.go`, "reading server.go"},
		{"read_file", "internal/watch/bot.go", "reading bot.go"},
		{"Edit", "/repo/internal/watch/bot.go", "editing bot.go"},
		{"write_file", "", "editing a file"},
		{"Bash", "go test ./internal/watch -run TestBot", "running go test"},
		{"run_command", "cd internal && GOFLAGS=-v /usr/local/go/bin/go.exe build ./...", "running go build"},
		{"PowerShell", "", "running a command"},
		{"Grep", "func bot", "searching the code"},
		{"WebFetch", "https://example.com", "looking something up online"},
		{"Task", "review the diff", "handing part of the job to a helper"},
		{"TodoWrite", "", "updating its to-do list"},
		{"mcp__hrn__batch_show", "", "using batch show"},
	} {
		if got := activity(c.tool, c.summary); got != c.want {
			t.Errorf("activity(%q, %q) = %q, want %q", c.tool, c.summary, got, c.want)
		}
	}
}

// A small world for the transition tests: one run and one batch, at moments.
func lookAt(runs []runlog.Record, batches []batch.Result, doing map[string]string) botSnapshot {
	return botLook(runs, batches, doing, time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC))
}

func runRec(id string, started time.Time, done bool, res agent.Result) runlog.Record {
	return runlog.Record{ID: id, Backend: "claude", Started: started, Task: agent.Task{Prompt: "Fix the flaky test", Dir: "/repo/harness"}, Result: res, Done: done}
}

var noon = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

func kinds(says []say) []string {
	var out []string
	for _, s := range says {
		out = append(out, s.Kind)
	}
	return out
}

func TestTheFirstLookOnlySaysWhatIsGoing(t *testing.T) {
	cur := lookAt([]runlog.Record{
		runRec("20261011-115000-aaaaaa", noon.Add(-10*time.Minute), false, agent.Result{}),
		runRec("20261011-110000-bbbbbb", noon.Add(-time.Hour), true, agent.Result{Status: agent.StatusOK, Duration: time.Minute}),
	}, nil, map[string]string{"20261011-115000-aaaaaa": "editing bot.go"})
	says := announce(botSnapshot{}, cur, true)
	if len(says) != 1 || says[0].Kind != "status" {
		t.Fatalf("first look said %+v; want the status only, not old endings", says)
	}
	if want := "One thing is going: Claude in harness, editing bot.go."; says[0].Text != want {
		t.Errorf("status = %q, want %q", says[0].Text, want)
	}
	if got := announce(botSnapshot{}, lookAt(nil, nil, nil), true); got[0].Text != "Nothing is running." {
		t.Errorf("empty status = %q", got[0].Text)
	}
}

func TestTheBotAnnouncesARunStartingWorkingAndEnding(t *testing.T) {
	id := "20261011-115950-aaaaaa"
	before := lookAt(nil, nil, nil)
	going := lookAt([]runlog.Record{runRec(id, noon.Add(-10*time.Second), false, agent.Result{})}, nil, nil)
	says := announce(before, going, false)
	if strings.Join(kinds(says), ",") != "started,status" || says[0].Text != "Claude has started in harness: fix the flaky test." || says[0].URL != "/run/"+id || says[0].Ref != id {
		t.Fatalf("start: %+v", says)
	}
	working := lookAt([]runlog.Record{runRec(id, noon.Add(-10*time.Second), false, agent.Result{})}, nil, map[string]string{id: "editing bot.go"})
	says = announce(going, working, false)
	if len(says) < 1 || says[0].Kind != "working" || says[0].Text != "Claude is editing bot.go in harness." {
		t.Fatalf("working: %+v", says)
	}
	if again := announce(working, working, false); len(again) != 0 {
		t.Errorf("nothing changed, yet the bot said %+v", again)
	}
	ended := lookAt([]runlog.Record{runRec(id, noon.Add(-10*time.Second), true, agent.Result{Status: agent.StatusOK, Duration: 4 * time.Minute})}, nil, nil)
	says = announce(working, ended, false)
	if strings.Join(kinds(says), ",") != "done,status" || says[1].Text != "Nothing is running." {
		t.Fatalf("end: %+v", says)
	}
}

// A run younger than one look may be a batch's attempt whose record the batch
// has not saved yet: it is held back one look rather than announced twice.
func TestAJustStartedRunWaitsALookAndABatchsRunsAreNotItsOwn(t *testing.T) {
	runID, batchID := "20261011-115959-aaaaaa", "20261011-115958-bbbbbb"
	young := lookAt([]runlog.Record{runRec(runID, noon.Add(-time.Second), false, agent.Result{})}, nil, nil)
	if says := announce(lookAt(nil, nil, nil), young, false); len(says) != 0 {
		t.Fatalf("a run one second old was announced: %+v", says)
	}
	b := batch.Result{ID: batchID, Started: noon.Add(-2 * time.Second), Dir: "/repo/harness", Tasks: []batch.TaskResult{
		{ID: "t1", Prompt: "Add the guard", Status: batch.StatusRunning, Attempts: []batch.Attempt{{Backend: "claude", RunID: runID}}},
		{ID: "t2", Prompt: "Add the budget", Status: batch.StatusRunning},
	}}
	owned := lookAt([]runlog.Record{runRec(runID, noon.Add(-5*time.Second), false, agent.Result{})}, []batch.Result{b}, map[string]string{runID: "editing guard.go"})
	says := announce(lookAt(nil, nil, nil), owned, false)
	if strings.Join(kinds(says), ",") != "started,status" || says[0].Text != "A batch of two tasks has started in harness." {
		t.Fatalf("batch start: %+v", says)
	}
	for _, s := range says {
		if s.Ref == runID {
			t.Errorf("the batch's own run was announced too: %+v", s)
		}
	}
	if !strings.Contains(says[1].Text, "a batch of two tasks in harness") {
		t.Errorf("status = %q", says[1].Text)
	}
}

// A batch first seen already over (it ran between two looks) is told by its
// ending alone, not task by task.
func TestABatchFirstSeenOverIsToldByItsEnding(t *testing.T) {
	b := batch.Result{ID: "20261011-115959-cccccc", Started: noon.Add(-5 * time.Second), Dir: "/repo/harness", Duration: 4 * time.Second, Tasks: []batch.TaskResult{
		{ID: "t1", Prompt: "Add the guard", Status: batch.StatusPass}, {ID: "t2", Prompt: "Add the budget", Status: batch.StatusPass}}}
	says := announce(lookAt(nil, nil, nil), lookAt(nil, []batch.Result{b}, nil), false)
	if len(says) != 1 || says[0].Kind != "done" || says[0].Text != "The batch in harness is done: both tasks passed. It took four seconds." {
		t.Fatalf("says: %+v", says)
	}
	// And a cancelled run with no workspace still reads.
	if _, text := runEnding("Claude", "", agent.Result{Status: agent.StatusCanceled, Duration: time.Minute}); text != "Claude's run was cancelled after a minute." {
		t.Errorf("cancelled, no place: %q", text)
	}
}

// What a batch's agent is doing is told as the batch's, in a whole sentence.
func TestTheBotSaysWhatABatchsAgentIsDoing(t *testing.T) {
	runID := "20261011-115000-aaaaaa"
	b := batch.Result{ID: "20261011-114959-bbbbbb", Started: noon.Add(-time.Minute), Dir: "/repo/harness", Tasks: []batch.TaskResult{
		{ID: "guard", Prompt: "Add the guard", Status: batch.StatusRunning, Attempts: []batch.Attempt{{Backend: "codex", RunID: runID}}},
		{ID: "budget", Prompt: "Add the budget", Status: batch.StatusRunning}}}
	before := lookAt(nil, []batch.Result{b}, nil)
	after := lookAt(nil, []batch.Result{b}, map[string]string{runID: "reading main.go"})
	says := announce(before, after, false)
	if strings.Join(kinds(says), ",") != "working,status" {
		t.Fatalf("says: %+v", says)
	}
	if says[0].Text != "In the batch in harness, Codex is reading main.go." {
		t.Errorf("working = %q", says[0].Text)
	}
	if says[1].Text != "One thing is going: a batch of two tasks in harness, Codex is reading main.go." {
		t.Errorf("status = %q", says[1].Text)
	}
}

// A running batch rewrites its record as each task moves on, and on Windows
// a read can land on the rename: for that look the record cannot be read.
// The batch is kept from the look before, or it and its runs would be
// announced again as new.
func TestABatchCaughtMidSaveIsNotAnnouncedAgain(t *testing.T) {
	runID := "20261011-115000-aaaaaa"
	b := batch.Result{ID: "20261011-114959-bbbbbb", Started: noon.Add(-time.Minute), Dir: "/repo/harness", Tasks: []batch.TaskResult{
		{ID: "guard", Prompt: "Add the guard", Status: batch.StatusRunning, Attempts: []batch.Attempt{{Backend: "claude", RunID: runID}}}}}
	run := runRec(runID, noon.Add(-time.Minute), false, agent.Result{})
	prev := lookAt([]runlog.Record{run}, []batch.Result{b}, nil)
	caught := lookAt([]runlog.Record{run}, keepUnread([]batch.Result{b}, nil, 1), nil)
	if says := announce(prev, caught, false); len(says) != 0 {
		t.Fatalf("a batch that could not be read for one look was announced again: %+v", says)
	}
	// Removed for good (hrn batch rm): every record reads, and it is not kept.
	if got := keepUnread([]batch.Result{b}, nil, 0); len(got) != 0 {
		t.Errorf("a batch gone for good was kept: %+v", got)
	}
}

func TestTheBotAnnouncesEachBatchTaskAndTheEnd(t *testing.T) {
	id := "20261011-110000-cccccc"
	mk := func(s1, s2 batch.Status, d time.Duration) batch.Result {
		return batch.Result{ID: id, Started: noon.Add(-time.Hour), Dir: "/repo/harness", Duration: d, Tasks: []batch.TaskResult{
			{ID: "t1", Prompt: "Add the guard", Status: s1}, {ID: "t2", Prompt: "Add the budget", Status: s2}}}
	}
	start := lookAt(nil, []batch.Result{mk(batch.StatusRunning, batch.StatusRunning, 0)}, nil)
	one := lookAt(nil, []batch.Result{mk(batch.StatusPass, batch.StatusRunning, 0)}, nil)
	says := announce(start, one, false)
	if len(says) != 1 || says[0].Kind != "task" || says[0].Text != "Task one of two in harness passed: add the guard." {
		t.Fatalf("task: %+v", says)
	}
	end := lookAt(nil, []batch.Result{mk(batch.StatusPass, batch.StatusFail, 12*time.Minute)}, nil)
	says = announce(one, end, false)
	if strings.Join(kinds(says), ",") != "task,failed,status" {
		t.Fatalf("end: %+v", says)
	}
	if says[0].Text != "Task two of two in harness failed: add the budget." || says[1].Text != "The batch in harness is done: one of two tasks passed. It took twelve minutes." {
		t.Errorf("end: %q / %q", says[0].Text, says[1].Text)
	}
	all := lookAt(nil, []batch.Result{mk(batch.StatusPass, batch.StatusPass, 3*time.Minute)}, nil)
	if says := announce(one, all, false); says[len(says)-2].Kind != "done" || says[len(says)-2].Text != "The batch in harness is done: both tasks passed. It took three minutes." {
		t.Errorf("all passed: %+v", says)
	}
}

// The stream is what the page hears: the visible part as escaped HTML, and
// what to say as JSON, with secret values from hrn watch's environment blanked.
func TestTheBotStreamSaysWhatHappensAndBlanksSecrets(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	t.Setenv("SERVICE_TOKEN", "tok-0123456789abcdef")
	srv := httptest.NewServer(New(t.Context(), Options{Poll: 20 * time.Millisecond}))
	defer srv.Close()

	s, closeStream := openStream(t, srv.URL+"/stream/bot", "")
	defer closeStream()
	first := nextSay(t, s)
	if first.Kind != "status" || first.Text != "Nothing is running." {
		t.Fatalf("first message %+v", first)
	}

	w, err := runlog.Create("claude", agent.Task{Prompt: "Rotate tok-0123456789abcdef " + hostile, Dir: "/repo/harness"})
	if err != nil {
		t.Fatal(err)
	}
	started := nextSay(t, s)
	if started.Kind != "started" || strings.Contains(started.Text, "tok-0123456789abcdef") || !strings.Contains(started.Text, "Claude has started in harness") {
		t.Fatalf("started: %+v", started)
	}
	w.Event(agent.Event{Kind: agent.EventToolCall, Tool: "Edit", Text: "/repo/harness/internal/watch/bot.go"})
	for {
		m := nextSay(t, s)
		if m.Kind == "working" {
			if m.Text != "Claude is editing bot.go in harness." {
				t.Fatalf("working: %+v", m)
			}
			break
		}
	}
	w.Event(agent.Event{Kind: agent.EventToolCall, Tool: "Bash", Text: `powershell.exe -NoProfile -Command "go test ./..."`})
	for {
		m := nextSay(t, s)
		if m.Kind == "working" {
			if m.Text != "Claude is running go test in harness." {
				t.Fatalf("working: %+v", m)
			}
			break
		}
	}
	if err := w.Close(agent.Result{Status: agent.StatusOK, Duration: time.Minute}); err != nil {
		t.Fatal(err)
	}
	for {
		m := nextSay(t, s)
		if m.Kind == "done" {
			if m.Text != "Claude has finished in harness. It took a minute." {
				t.Fatalf("done: %+v", m)
			}
			break
		}
	}
}

// nextSay reads messages until the next one the page would speak, checking on
// the way that the visible parts are escaped and hold no secret.
func nextSay(t *testing.T, s sse) say {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		m, ok := s.next(t)
		if !ok {
			t.Fatal("the stream ended")
		}
		if strings.Contains(m.data, "tok-0123456789abcdef") {
			t.Fatalf("a secret reached the stream: %s %q", m.event, m.data)
		}
		switch m.event {
		case "replace":
			if strings.Contains(m.data, "<script>alert") {
				t.Fatalf("the bot's list holds unescaped text: %q", m.data)
			}
		case "say":
			var v say
			if err := json.Unmarshal([]byte(m.data), &v); err != nil {
				t.Fatalf("say is not JSON: %q", m.data)
			}
			return v
		}
	}
	t.Fatal("no message to say in time")
	return say{}
}

// Only the bot asks for the microphone, and only from this page's own origin.
func TestOnlyTheBotPageMayUseTheMicrophone(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	srv := httptest.NewServer(New(t.Context(), Options{}))
	defer srv.Close()
	for path, want := range map[string]string{"/bot": "microphone=(self)", "/": "microphone=()"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Permissions-Policy"), want) {
			t.Errorf("%s: %d, Permissions-Policy %q; want %s", path, resp.StatusCode, resp.Header.Get("Permissions-Policy"), want)
		}
		if !strings.Contains(resp.Header.Get("Permissions-Policy"), "camera=()") {
			t.Errorf("%s: the camera must stay off", path)
		}
	}
	_, body := get(t, srv.URL+"/bot")
	for _, want := range []string{`src="/static/bot.js"`, `data-stream="/stream/bot"`, `data-kind="done"`, "Let me talk"} {
		if !strings.Contains(body, want) {
			t.Errorf("the bot page lacks %q", want)
		}
	}
	if code, js := get(t, srv.URL+"/static/bot.js"); code != 200 || !strings.Contains(js, "speechSynthesis") {
		t.Errorf("bot.js: %d", code)
	}
}

// The card of what is going names who works where, as a heading, not a
// sentence: no full stop in the middle of a line.
func TestTheCardOfWhatIsGoingNamesWhoWorksWhere(t *testing.T) {
	b := batch.Result{ID: "20261011-115900-bbbbbb", Started: noon.Add(-time.Minute), Dir: "/work/tidedesk", Tasks: []batch.TaskResult{
		{ID: "t1", Prompt: "Fix it", Status: batch.StatusRunning}, {ID: "t2", Prompt: "Test it", Status: batch.StatusRunning}}}
	snap := lookAt([]runlog.Record{runRec("20261011-115000-aaaaaa", noon.Add(-10*time.Minute), false, agent.Result{})}, []batch.Result{b}, nil)
	html, err := render("botnow", botNowOf(snap))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{">A batch of two tasks in tidedesk</a>", ">Claude in harness</a>", `href="/batch/20261011-115900-bbbbbb"`, `data-mute="20261011-115000-aaaaaa"`} {
		if !strings.Contains(html, want) {
			t.Errorf("the card lacks %q:\n%s", want, html)
		}
	}
}

// A batch's tasks often open with the same paragraph (the repository's rules),
// so the bot names a task by its id, as the spec did, rather than read the
// shared opening out four times.
func TestTheBotNamesABatchsTasksByTheirNames(t *testing.T) {
	const preamble = "You are working in a git worktree of jobpilot. First read AGENTS.md and follow it.\n\n"
	mk := func(s batch.Status) batch.Result {
		return batch.Result{ID: "20261011-213338-c6fe69", Started: noon.Add(-time.Hour), Dir: "/work/jobpilot", Tasks: []batch.TaskResult{
			{ID: "employer-link-lookups", Prompt: preamble + "Look the links up", Status: s},
			{ID: "german-questions", Prompt: preamble + "Translate the questions", Status: batch.StatusRunning}}}
	}
	says := announce(lookAt(nil, []batch.Result{mk(batch.StatusRunning)}, nil), lookAt(nil, []batch.Result{mk(batch.StatusPass)}, nil), false)
	if len(says) != 1 || says[0].Text != "Task one of two in jobpilot passed: employer-link-lookups." {
		t.Fatalf("task: %+v", says)
	}
	// A task whose id hrn made up (t1, t2, ...) is named by its prompt instead;
	// t0 and t01 are not ids hrn makes, so a spec gave them.
	for id, want := range map[string]string{"t1": "fix the parser", "t12": "fix the parser", "": "fix the parser",
		"tidy": "tidy", "fix-add": "fix-add", "t0": "t0", "t01": "t01", "t-1": "t-1"} {
		if got := taskName(batch.TaskResult{ID: id, Prompt: "Fix the parser.\nMore"}); got != want {
			t.Errorf("taskName(%q) = %q, want %q", id, got, want)
		}
	}
}

func TestTheBotIsReachableFromTheIndex(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	_ = os.MkdirAll(runlog.Home(), 0o755)
	srv := httptest.NewServer(New(t.Context(), Options{}))
	defer srv.Close()
	if _, body := get(t, srv.URL+"/"); !strings.Contains(body, `href="/bot"`) {
		t.Error("the index does not link to the bot")
	}
}
