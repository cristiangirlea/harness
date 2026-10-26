package game

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/eval"
	"github.com/cristiangirlea/harness/internal/runlog"
)

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeRun saves a finished run as hrn does: the whole record in result.json, events one per line.
func writeRun(t *testing.T, rec runlog.Record, events ...agent.Event) runlog.Record {
	t.Helper()
	writeJSON(t, filepath.Join(runlog.Root(), rec.ID, "result.json"), rec)
	if len(events) > 0 {
		var b bytes.Buffer
		for _, e := range events {
			line, err := json.Marshal(e)
			if err != nil {
				t.Fatal(err)
			}
			b.Write(append(line, '\n'))
		}
		if err := os.WriteFile(filepath.Join(runlog.Root(), rec.ID, "events.jsonl"), b.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rec.Done = true
	return rec
}

func run(t *testing.T, id, backend, dir, started string, status agent.Status) runlog.Record {
	return runlog.Record{ID: id, Backend: backend, Started: at(t, started),
		Task:   agent.Task{Prompt: "a task", Dir: dir},
		Result: agent.Result{Backend: backend, Status: status, Duration: 30 * time.Minute}}
}

func gate(code int) batch.GateResult {
	return batch.GateResult{Command: "go test ./...", ExitCode: code}
}

func attempt(runID string, gates ...batch.GateResult) batch.Attempt {
	return batch.Attempt{Backend: "claude", RunID: runID, Gates: gates}
}

func derived(t *testing.T, r Records, prefix string) map[string]Event {
	t.Helper()
	events, err := Derive(r)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	out := map[string]Event{}
	for _, e := range events {
		if strings.HasPrefix(e.Key, prefix) {
			if _, twice := out[e.Key]; twice {
				t.Fatalf("Derive returned %s twice", e.Key)
			}
			out[e.Key] = e
		}
	}
	return out
}

func sortedKeys(m map[string]Event) []string {
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestAReviewWithoutABlockingFindingEarnsTwenty(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	dir := filepath.Join(t.TempDir(), "app")
	review := func(id, final string, status agent.Status) runlog.Record {
		rec := run(t, id, "claude", dir, "2026-10-26T10:00:00Z", status)
		rec.Task.Purpose, rec.Result.Final = "review", final
		return writeRun(t, rec)
	}
	implement := run(t, "rv6", "claude", dir, "2026-10-26T10:00:00Z", agent.StatusOK)
	implement.Task.Purpose, implement.Result.Final = "implement", "No defects."
	runs := []runlog.Record{
		review("rv1", "No defects to fix. Nits, not to file: a non-blocking wording point.", agent.StatusOK),
		review("rv2", "1. **blocking**: the cap is applied before cancels.", agent.StatusOK),
		review("rv3", "Blocking: none of the above.", agent.StatusOK),
		review("rv4", "", agent.StatusOK),
		review("rv5", "Looks fine.", agent.StatusError),
		writeRun(t, implement),
		review("rv7", "Nothing is BLOCKING here.", agent.StatusOK),
	}
	got := derived(t, Records{Runs: runs}, "review:")
	if keys := sortedKeys(got); !reflect.DeepEqual(keys, []string{"review:rv1"}) {
		t.Fatalf("review events %v, want only review:rv1", keys)
	}
	e := got["review:rv1"]
	want := Event{Key: "review:rv1", Kind: kindReview, Track: TrackDelivery, XP: 20, At: at(t, "2026-10-26T10:30:00Z"), Repo: "app"}
	if !reflect.DeepEqual(e, want) || e.At.Location() != time.UTC {
		t.Fatalf("event %+v, want %+v (at is the run's end, in UTC)", e, want)
	}
}

func TestARunInsideItsCeilingEarnsFive(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	dir := filepath.Join(t.TempDir(), "app")
	timed := run(t, "c1", "claude", dir, "2026-10-26T10:00:00Z", agent.StatusOK)
	timed.Task.Timeout = agent.Duration(20 * time.Minute)
	apiCost := run(t, "c2", "api", dir, "2026-10-26T10:00:00Z", agent.StatusOK)
	apiCost.Task.MaxCostUSD, apiCost.Result.CostUSD, apiCost.Result.CostEstimate = 1, 0.3, true
	cliCost := run(t, "c3", "claude", dir, "2026-10-26T10:00:00Z", agent.StatusOK)
	cliCost.Task.MaxCostUSD, cliCost.Result.CostUSD = 1, 0.3
	apiNoCost := run(t, "c4", "api", dir, "2026-10-26T10:00:00Z", agent.StatusOK)
	apiNoCost.Task.MaxCostUSD = 1
	apiMissing := run(t, "c5", "api", dir, "2026-10-26T10:00:00Z", agent.StatusOK)
	apiMissing.Task.MaxCostUSD, apiMissing.Result.CostUSD = 1, 0.3
	apiMissing.Result.Usage = &agent.UsageMetadata{InputSemantics: "exclusive_cache", Status: "missing", CostStatus: "missing"}
	timedOut := run(t, "c6", "claude", dir, "2026-10-26T10:00:00Z", agent.StatusTimeout)
	timedOut.Task.Timeout = agent.Duration(20 * time.Minute)
	var runs []runlog.Record
	for _, r := range []runlog.Record{timed, apiCost, cliCost, apiNoCost, apiMissing, timedOut} {
		runs = append(runs, writeRun(t, r))
	}
	got := derived(t, Records{Runs: runs}, "ceiling:")
	if keys := sortedKeys(got); !reflect.DeepEqual(keys, []string{"ceiling:c1", "ceiling:c2"}) {
		t.Fatalf("ceiling events %v, want c1 (timeout) and c2 (api max_cost with a known cost)", keys)
	}
	if e := got["ceiling:c1"]; e.XP != 5 || e.Kind != kindCeiling || e.Track != TrackDelivery {
		t.Fatalf("event %+v", e)
	}
}

func TestARoutedRunPassesOnlyWhenItsBatchAttemptPassesItsGates(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	dir := filepath.Join(t.TempDir(), "app")
	busy := filepath.Join(t.TempDir(), "slot-3")
	routed := func(id, started, runDir string, status agent.Status) runlog.Record {
		rec := run(t, id, "claude", runDir, started, status)
		rec.RoutedBy = &runlog.RoutedBy{Category: "review", Set: "2026-10-19"}
		return writeRun(t, rec)
	}
	runs := []runlog.Record{
		routed("ro1", "2026-10-26T10:00:00Z", dir, agent.StatusOK),
		routed("ro2", "2026-10-26T11:00:00Z", dir, agent.StatusOK),
		routed("ro3", "2026-10-26T12:00:00Z", dir, agent.StatusOK),
		routed("ro4", "2026-10-26T09:00:00Z", dir, agent.StatusError),
		routed("ro5", "2026-10-26T13:00:00Z", busy, agent.StatusOK),
		writeRun(t, run(t, "ro6", "claude", dir, "2026-10-26T08:00:00Z", agent.StatusOK)),
	}
	batches := []batch.Result{
		{ID: "b1", Started: at(t, "2026-10-26T10:55:00Z"), Dir: dir, Tasks: []batch.TaskResult{
			{ID: "t1", Status: batch.StatusFail, Attempts: []batch.Attempt{attempt("ro2", gate(1))}},
			{ID: "t2", Status: batch.StatusPass, Attempts: []batch.Attempt{attempt("ro3", gate(0))}},
		}},
		// Still going: its attempt is not recorded yet, so whether ro5 passed is not known.
		{ID: "b2", Started: at(t, "2026-10-26T12:55:00Z"), Dir: dir, Tasks: []batch.TaskResult{
			{ID: "t1", Status: batch.StatusRunning, Worktree: busy, Slot: 3, Attempts: []batch.Attempt{}},
		}},
	}
	r := Records{Runs: runs, AllBatches: batches}
	got := derived(t, r, "routed:")
	if keys := sortedKeys(got); !reflect.DeepEqual(keys, []string{"routed:ro1", "routed:ro2", "routed:ro3"}) {
		t.Fatalf("routed events %v, want ro1 (on its own), ro2 (its attempt failed) and ro3 (its attempt passed)", keys)
	}
	for id, kind := range map[string]string{"ro1": kindRouted, "ro2": "routed_failed", "ro3": kindRouted} {
		want := 10
		if kind != kindRouted {
			want = 0
		}
		if e := got["routed:"+id]; e.Kind != kind || e.XP != want {
			t.Errorf("routed:%s = %+v, want kind %s with %d xp", id, e, kind, want)
		}
	}
	first := derived(t, r, "first:routed")["first:routed"]
	if !first.At.Equal(at(t, "2026-10-26T09:30:00Z")) || first.XP != 25 || first.Kind != kindFirst {
		t.Fatalf("first:routed = %+v, want the earliest routed run (ro4, whatever its status) at its end", first)
	}
}

func TestNothingIsDecidedForABatchTaskThatIsStillRunning(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	dir := filepath.Join(t.TempDir(), "app")
	var runs []runlog.Record
	for _, id := range []string{"roA", "roB"} {
		rec := run(t, id, "claude", dir, "2026-10-26T10:00:00Z", agent.StatusOK)
		rec.RoutedBy = &runlog.RoutedBy{Category: "implement", Set: "2026-10-19"}
		runs = append(runs, writeRun(t, rec))
	}
	// The first attempt is over; the second has passed one gate of two so far.
	task := batch.TaskResult{ID: "t1", Status: batch.StatusRunning, Worktree: dir, Attempts: []batch.Attempt{attempt("roA", gate(1)), attempt("roB", gate(0))}}
	b := batch.Result{ID: "b3", Started: at(t, "2026-10-26T09:00:00Z"), Dir: dir, Tasks: []batch.TaskResult{task}}
	r := Records{Runs: runs, Batches: []batch.Result{b}, AllBatches: []batch.Result{b}}
	got := derived(t, r, "")
	if e, ok := got["routed:roA"]; !ok || e.Kind != "routed_failed" {
		t.Errorf("routed:roA = %+v, want routed_failed: its attempt is over and failed", e)
	}
	for _, key := range []string{"routed:roB", "batch:b3/t1"} {
		if _, ok := got[key]; ok {
			t.Errorf("%s decided while its attempt's gates are still running", key)
		}
	}
	b.Tasks[0].Status = batch.StatusPass
	r = Records{Runs: runs, Batches: []batch.Result{b}, AllBatches: []batch.Result{b}}
	got = derived(t, r, "")
	if e := got["routed:roB"]; e.Kind != kindRouted || e.XP != 10 {
		t.Errorf("routed:roB = %+v once the task passed, want routed_passed", e)
	}
	if _, ok := got["batch:b3/t1"]; !ok {
		t.Error("no bug_squashed once the task passed")
	}
}

func TestABugIsSquashedWhenAGateThatIsNotAGuardFailsAndALaterAttemptPasses(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	dir := filepath.Join(t.TempDir(), "app")
	guard := batch.GateResult{Command: "protect scripts/**", ExitCode: 1, Guard: true}
	infra := batch.GateResult{Command: "go test ./...", ExitCode: -1, InfrastructureError: "the shell could not start"}
	killed := batch.GateResult{Command: "go test ./...", ExitCode: -1, Killed: "timeout"}
	tasks := []batch.TaskResult{
		{ID: "t1", Attempts: []batch.Attempt{attempt("", gate(1)), attempt("", gate(0))}},
		{ID: "t2", Attempts: []batch.Attempt{attempt("", guard, gate(0)), attempt("", gate(0))}},
		{ID: "t3", Attempts: []batch.Attempt{attempt("", infra), attempt("", gate(0))}},
		{ID: "t4", Attempts: []batch.Attempt{attempt("", gate(0))}},
		{ID: "t5", Attempts: []batch.Attempt{attempt("", gate(1)), attempt("", gate(1))}},
		{ID: "t6", Attempts: []batch.Attempt{attempt("", gate(1)), attempt("")}},
		{ID: "t7", Attempts: []batch.Attempt{attempt("", killed), attempt("", gate(1)), attempt("", gate(0), gate(0))}},
	}
	b := batch.Result{ID: "b1", Started: at(t, "2026-10-26T08:00:00Z"), Dir: dir, Tasks: tasks}
	got := derived(t, Records{Batches: []batch.Result{b}, AllBatches: []batch.Result{b}}, "batch:")
	if keys := sortedKeys(got); !reflect.DeepEqual(keys, []string{"batch:b1/t1", "batch:b1/t7"}) {
		t.Fatalf("bug events %v, want t1 and t7", keys)
	}
	want := Event{Key: "batch:b1/t1", Kind: kindBug, Track: TrackDelivery, XP: 15, At: at(t, "2026-10-26T08:00:00Z"), Repo: "app"}
	if e := got["batch:b1/t1"]; !reflect.DeepEqual(e, want) {
		t.Fatalf("event %+v, want %+v (a batch event is dated at the batch's start)", e, want)
	}
}

func TestFirstUseCountsTheEarliestRecordOfEachFeature(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	dir := filepath.Join(t.TempDir(), "app")
	batches := []batch.Result{
		{ID: "bC", Started: at(t, "2026-10-24T08:00:00Z"), Dir: dir, Tasks: []batch.TaskResult{{ID: "t1", Slot: 1}}},
		{ID: "bA", Started: at(t, "2026-10-20T08:00:00Z"), Dir: dir, Tasks: []batch.TaskResult{{ID: "t1"}}},
		{ID: "bB", Started: at(t, "2026-10-22T08:00:00Z"), Dir: dir, Tasks: []batch.TaskResult{{ID: "t1"}, {ID: "t2", Slot: 3}}},
	}
	evals := []eval.Record{
		{ID: "eX", BatchID: "bC", Started: at(t, "2026-10-23T08:00:00Z")},
		{ID: "eY", BatchID: "bA", Started: at(t, "2026-10-21T08:00:00Z")},
	}
	got := derived(t, Records{Batches: batches, Evals: evals, AllBatches: batches}, "first:")
	for key, when := range map[string]string{
		"first:batch": "2026-10-20T08:00:00Z",
		"first:slot":  "2026-10-22T08:00:00Z",
		"first:eval":  "2026-10-21T08:00:00Z",
	} {
		e, ok := got[key]
		if !ok {
			t.Errorf("no %s", key)
			continue
		}
		if !e.At.Equal(at(t, when)) || e.XP != 25 || e.Kind != kindFirst || e.Track != TrackDelivery || e.Repo != "app" {
			t.Errorf("%s = %+v, want at %s in app", key, e, when)
		}
	}
	if len(got) != 3 {
		t.Errorf("first-use events %v, want batch, slot and eval only", sortedKeys(got))
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// keptRepo is a repository with one kept script, one Go package script and one plain script.
func keptRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	gitIn(t, repo, "init", "-q")
	gitIn(t, repo, "remote", "add", "origin", "https://github.com/example/app.git")
	files := map[string]string{
		"scripts/check.sh":            "#!/usr/bin/env bash\n# name: check\n# summary: Run the checks.\n# effects: exec\n# parameters: none\n# example: bash scripts/check.sh\n# hash: none\necho ok\n",
		"scripts/tool-shapes/main.go": "// name: tool-shapes\n// summary: Measure tool output.\n// effects: read\n// parameters: none\n// example: none (fixture)\n// hash: none\npackage main\n",
		"scripts/plain.sh":            "#!/usr/bin/env bash\necho plain\n",
	}
	for name, body := range files {
		path := filepath.Join(repo, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return repo
}

func shellCall(tool, command string) agent.Event {
	input, _ := json.Marshal(map[string]string{"command": command})
	return agent.Event{Time: time.Now(), Kind: agent.EventToolCall, Tool: tool, Input: input, Text: command}
}

func TestAKeptScriptReusedEarnsFiveOncePerRunAndScript(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	repo := keptRepo(t)
	elsewhere := t.TempDir()
	codex := agent.Event{Time: time.Now(), Kind: agent.EventToolCall, Tool: "shell", Text: "go run scripts/tool-shapes/main.go -n 5"}
	runs := []runlog.Record{
		writeRun(t, run(t, "s1", "claude", repo, "2026-10-26T10:00:00Z", agent.StatusOK),
			shellCall("Bash", "bash scripts/check.sh --fast"),
			shellCall("Bash", "cat scripts/check.sh"),
			shellCall("Bash", "cd sub && bash scripts/check.sh"),
			shellCall("Bash", "bash scripts/plain.sh"),
			agent.Event{Time: time.Now(), Kind: agent.EventToolCall, Tool: "Read", Input: json.RawMessage(`{"file_path":"scripts/check.sh"}`)},
			agent.Event{Time: time.Now(), Kind: agent.EventToolResult, Tool: "Bash", Text: "bash scripts/check.sh"}),
		writeRun(t, run(t, "s2", "codex", repo, "2026-10-26T10:00:00Z", agent.StatusOK), codex),
		writeRun(t, run(t, "s3", "api", repo, "2026-10-26T10:00:00Z", agent.StatusError),
			shellCall("run_command", `& "C:\Program Files\Git\bin\bash.exe" scripts\check.sh`)),
		writeRun(t, run(t, "s4", "claude", elsewhere, "2026-10-26T10:00:00Z", agent.StatusOK),
			shellCall("Bash", "bash scripts/check.sh")),
		writeRun(t, run(t, "s5", "claude", repo, "2026-10-26T10:00:00Z", agent.StatusOK),
			shellCall("PowerShell", "./scripts/check.sh; echo done")),
		writeRun(t, run(t, "s6", "claude", repo, "2026-10-26T10:00:00Z", agent.StatusOK),
			shellCall("Bash", "bash myscripts/check.sh")),
	}
	got := derived(t, Records{Runs: runs}, "script:")
	want := []string{"script:s1/check.sh", "script:s2/tool-shapes/main.go", "script:s3/check.sh", "script:s5/check.sh"}
	if keys := sortedKeys(got); !reflect.DeepEqual(keys, want) {
		t.Fatalf("script events %v, want %v", keys, want)
	}
	if e := got["script:s1/check.sh"]; e.XP != 5 || e.Kind != kindScript || e.Repo != "example/app" {
		t.Fatalf("event %+v, want 5 xp in example/app", e)
	}
}

func TestRepoNameIsTheGitHubOwnerAndName(t *testing.T) {
	for url, want := range map[string]string{
		"https://github.com/example/app.git":      "example/app",
		"https://github.com/example/app":          "example/app",
		"git@github.com:example/app.git":          "example/app",
		"ssh://git@github.com/example/app.git":    "example/app",
		"https://github.com/example/app.git/":     "example/app",
		"https://gitlab.com/example/app.git":      "",
		"https://github.com/example":              "",
		"https://github.com/example/app/extra":    "",
		"":                                        "",
		"https://evil.example/github.com/x/y.git": "",
	} {
		if got := parseRemote(url); got != want {
			t.Errorf("parseRemote(%q) = %q, want %q", url, got, want)
		}
	}
	main := filepath.Join(t.TempDir(), "main")
	writeFile(t, filepath.Join(main, ".git", "config"), "[core]\n\tbare = false\n[remote \"upstream\"]\n\turl = https://github.com/other/fork.git\n"+
		"[Remote \"origin\"]\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n\tURL = \"git@github.com:example/app.git\"\n")
	writeFile(t, filepath.Join(main, ".git", "worktrees", "wt", "commondir"), "../..\n")
	wt := filepath.Join(t.TempDir(), "wt")
	writeFile(t, filepath.Join(wt, ".git"), "gitdir: "+filepath.Join(main, ".git", "worktrees", "wt")+"\n")
	for _, dir := range []string{main, wt, filepath.Join(wt, "sub")} {
		if got := repoName(dir); got != "example/app" {
			t.Errorf("repoName(%s) = %q, want example/app from origin in the shared config", dir, got)
		}
	}
	other := filepath.Join(t.TempDir(), "elsewhere")
	writeFile(t, filepath.Join(other, ".git", "config"), "[remote \"origin\"]\n\turl = https://gitlab.com/example/app.git\n")
	bare := filepath.Join(t.TempDir(), "noremote")
	writeFile(t, filepath.Join(bare, ".git", "config"), "[core]\n\tbare = false\n")
	// No folder recorded: no repository, not the one hrn happens to run in.
	for dir, want := range map[string]string{other: "elsewhere", bare: "noremote", "": ""} {
		if got := repoName(dir); got != want {
			t.Errorf("repoName(%s) = %q, want the folder's name %q", dir, got, want)
		}
	}
	repo := keptRepo(t)
	if got := repoName(filepath.Join(repo, ".hrn", "worktrees", "gone")); got != "example/app" {
		t.Errorf("a removed worktree inside the repository: %q, want example/app", got)
	}
	if got := repoName(filepath.Join(t.TempDir(), "loose")); got != "loose" {
		t.Errorf("a folder outside any repository: %q, want its name", got)
	}
}

func ledgerBytes(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(Dir(), "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestRebuildAddsEachEventOnceAndKeepsThoseWhoseRecordIsGone(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	dir := filepath.Join(t.TempDir(), "app")
	rec := run(t, "20261026-100000-aaaaaa", "claude", dir, "2026-10-26T10:00:00Z", agent.StatusOK)
	rec.Task.Purpose, rec.Result.Final = "review", "No defects."
	writeRun(t, rec)
	b := batch.Result{ID: "20261026-080000-b1b1b1", Started: at(t, "2026-10-26T08:00:00Z"), Dir: dir, Tasks: []batch.TaskResult{
		{ID: "t1", Status: batch.StatusPass, Attempts: []batch.Attempt{attempt("", gate(1)), attempt("", gate(0))}},
	}}
	writeJSON(t, filepath.Join(runlog.Home(), "batches", b.ID+".json"), b)
	writeJSON(t, filepath.Join(runlog.Home(), "evals", "e1.json"), eval.Record{ID: "e1", BatchID: b.ID, Started: at(t, "2026-10-26T09:00:00Z")})

	if err := Rebuild(); err != nil {
		t.Fatal(err)
	}
	first := ledgerBytes(t)
	events, _, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	keys := keysOf(events)
	sort.Strings(keys)
	want := []string{"batch:" + b.ID + "/t1", "first:batch", "first:eval", "review:" + rec.ID}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("ledger %v, want %v", keys, want)
	}
	if err := Rebuild(); err != nil {
		t.Fatal(err)
	}
	if again := ledgerBytes(t); !bytes.Equal(again, first) {
		t.Fatalf("a second rebuild changed the ledger:\n%s\nthen\n%s", first, again)
	}
	if err := os.RemoveAll(filepath.Join(runlog.Root(), rec.ID)); err != nil {
		t.Fatal(err)
	}
	if err := Rebuild(); err != nil {
		t.Fatal(err)
	}
	if after := ledgerBytes(t); !bytes.Equal(after, first) {
		t.Fatalf("a rebuild after the run was deleted changed the ledger:\n%s", after)
	}
}

func TestFoldRecordsEachBadgeOnceWhenItIsEarned(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	start := at(t, "2026-10-01T08:00:00Z")
	var events []Event
	for i := range 21 {
		events = append(events, Event{Key: fmt.Sprintf("ceiling:c%03d", i), Kind: kindCeiling,
			Track: TrackDelivery, XP: 5, At: start.Add(time.Duration(i) * 6 * time.Hour), Repo: "example/app"})
	}
	// One of the twenty is taken back, so the badge waits for the twenty-first.
	events = append(events, Event{Key: "revert:example/app#1", Kind: "revert", Track: TrackDelivery, At: start.Add(500 * time.Hour), Cancels: "ceiling:c004"})
	for i := range 9 {
		events = append(events, Event{Key: fmt.Sprintf("routed:r%d", i), Kind: kindRouted, Track: TrackDelivery, XP: 10, At: start.Add(time.Duration(i) * 24 * time.Hour)})
	}
	if _, err := Append(events); err != nil {
		t.Fatal(err)
	}
	if err := Fold(Records{}); err != nil {
		t.Fatal(err)
	}
	badges := func() map[string]Event {
		all, _, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]Event{}
		for _, e := range all {
			if e.Track == TrackBadge {
				if _, twice := out[e.Key]; twice {
					t.Fatalf("badge %s recorded twice", e.Key)
				}
				out[e.Key] = e
			}
		}
		return out
	}
	got := badges()
	keeper, ok := got["badge:budget-keeper"]
	if !ok || len(got) != 1 {
		t.Fatalf("badges %v, want Budget keeper only", sortedKeys(got))
	}
	if want := start.Add(20 * 6 * time.Hour); !keeper.At.Equal(want) || keeper.XP != 0 || keeper.Kind != kindBadge {
		t.Fatalf("Budget keeper %+v, want it at the twentieth run that still counts (%v)", keeper, want)
	}
	tenth := Event{Key: "routed:r9", Kind: kindRouted, Track: TrackDelivery, XP: 10, At: start.Add(9 * 24 * time.Hour)}
	big := Event{Key: "test:big", Kind: "test", Track: TrackDelivery, XP: 5000, At: start.Add(30 * 24 * time.Hour)}
	if _, err := Append([]Event{tenth, big}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := Fold(Records{}); err != nil {
			t.Fatal(err)
		}
	}
	got = badges()
	if want := []string{"badge:budget-keeper", "badge:cheap-and-right", "badge:veteran"}; !reflect.DeepEqual(sortedKeys(got), want) {
		t.Fatalf("badges %v, want %v", sortedKeys(got), want)
	}
	if !got["badge:cheap-and-right"].At.Equal(tenth.At) || !got["badge:veteran"].At.Equal(big.At) {
		t.Fatalf("badges dated %v and %v, want the tenth routed run and the event that reached level 10", got["badge:cheap-and-right"].At, got["badge:veteran"].At)
	}
}

func TestASyntaxCheckOfAKeptScriptIsNotAReuse(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	repo := keptRepo(t)
	runs := []runlog.Record{
		writeRun(t, run(t, "s7", "claude", repo, "2026-10-26T10:00:00Z", agent.StatusOK),
			shellCall("Bash", "bash -n scripts/check.sh")),
		writeRun(t, run(t, "s8", "claude", repo, "2026-10-26T10:00:00Z", agent.StatusOK),
			shellCall("Bash", "bash -x scripts/check.sh")),
		writeRun(t, run(t, "s9", "claude", repo, "2026-10-26T10:00:00Z", agent.StatusOK),
			shellCall("Bash", "sh -n scripts/check.sh && bash scripts/check.sh")),
	}
	got := derived(t, Records{Runs: runs}, "script:")
	if want := []string{"script:s8/check.sh", "script:s9/check.sh"}; !reflect.DeepEqual(sortedKeys(got), want) {
		t.Fatalf("script events %v, want %v: bash -n only reads the script", sortedKeys(got), want)
	}
}

func TestABatchBeingRemovedDecidesItsUnfinishedAttemptsAsFailed(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	dir := filepath.Join(t.TempDir(), "app")
	rec := run(t, "roC", "claude", dir, "2026-10-26T10:00:00Z", agent.StatusOK)
	rec.RoutedBy = &runlog.RoutedBy{Category: "implement", Set: "2026-10-19"}
	runs := []runlog.Record{writeRun(t, rec)}
	// A batch that died mid-attempt: its task stays running and nothing will finish it.
	b := batch.Result{ID: "b4", Started: at(t, "2026-10-26T09:00:00Z"), Dir: dir, Tasks: []batch.TaskResult{
		{ID: "t1", Status: batch.StatusRunning, Worktree: filepath.Join(dir, "gone"), Attempts: []batch.Attempt{attempt("roC", gate(0))}},
	}}
	r := Records{Runs: runs, Batches: []batch.Result{b}, AllBatches: []batch.Result{b}, Removing: map[string]bool{"b4": true}}
	got := derived(t, r, "routed:")
	if e, ok := got["routed:roC"]; !ok || e.Kind != "routed_failed" || e.XP != 0 {
		t.Fatalf("routed:roC = %+v (%v); want routed_failed, since its batch record goes and its gates never finished", e, ok)
	}
}
