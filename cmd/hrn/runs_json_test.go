package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/runlog"
)

// writeRun writes a run record started ago; a running one has no result yet.
func writeRun(t *testing.T, ago time.Duration, suffix, repo, branch string, done bool) string {
	t.Helper()
	return writeRunIn(t, ago, ago, suffix, repo, branch, done)
}

// writeRunIn is writeRun with the folder named for one time and the record started at another.
func writeRunIn(t *testing.T, folderAgo, ago time.Duration, suffix, repo, branch string, done bool) string {
	t.Helper()
	started := time.Now().Add(-ago)
	id := time.Now().Add(-folderAgo).Format("20060102-150405") + "-" + suffix
	rec := runlog.Record{
		ID: id, Backend: "claude", Started: started, Repo: repo, Branch: branch,
		Task: agent.Task{Prompt: "secret-prompt-" + suffix},
	}
	file := "task.json"
	if done {
		rec.Result = agent.Result{Backend: "claude", Status: agent.StatusOK, Duration: 90 * time.Second, Final: "secret-final"}
		file = "result.json"
	}
	writeJSONFile(t, filepath.Join(runlog.Root(), id, file), rec)
	return id
}

type runsJSON struct {
	Version int `json:"version"`
	Runs    []struct {
		ID         string    `json:"id"`
		Backend    string    `json:"backend"`
		Status     string    `json:"status"`
		Started    time.Time `json:"started"`
		DurationMS int64     `json:"duration_ms"`
		Repo       string    `json:"repo"`
		Branch     string    `json:"branch"`
	} `json:"runs"`
}

func readRunsJSON(t *testing.T, args ...string) (runsJSON, []byte) {
	t.Helper()
	out, err := captureStdout(t, func() error { return cmdRuns(args) })
	if err != nil {
		t.Fatalf("hrn runs %v: %v", args, err)
	}
	var got runsJSON
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("hrn runs %v printed %s: %v", args, out, err)
	}
	return got, out
}

func TestRunsJSONWithRepoListsThatRepositorysRunsNewestFirst(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	done := writeRun(t, 3*time.Hour, "aaaaaa", "github.com/example/app", "pane-look", true)
	running := writeRun(t, 2*time.Hour, "bbbbbb", "github.com/Example/App", "", false)
	writeRun(t, time.Hour, "cccccc", "github.com/example/other", "main", true)
	writeRun(t, 50*time.Minute, "dddddd", "gitlab.com/example/app", "main", true)
	writeRun(t, 40*time.Minute, "eeeeee", "github.com/example/app-two", "main", true)
	writeRun(t, 20*24*time.Hour, "ffffff", "github.com/example/app", "old", true)

	got, out := readRunsJSON(t, "--json", "--repo", "example/APP")
	if got.Version != 1 || len(got.Runs) != 2 || got.Runs[0].ID != running || got.Runs[1].ID != done {
		t.Fatalf("runs = %s; want version 1 and the running run, then the finished one, nothing older than 14 days and no other repository or host", out)
	}
	r, d := got.Runs[0], got.Runs[1]
	if r.Status != "running" || r.DurationMS != 0 || r.Backend != "claude" || r.Repo != "github.com/Example/App" {
		t.Errorf("running run = %+v", r)
	}
	if d.Status != "ok" || d.DurationMS != 90000 || d.Branch != "pane-look" || d.Started.IsZero() {
		t.Errorf("finished run = %+v", d)
	}
	if bytes.Contains(out, []byte("secret")) {
		t.Errorf("hrn runs --json printed a prompt or an answer: %s", out)
	}
}

func TestRunsJSONWithRepoOpensNoOlderFolderAndListsNothingOlderThanFourteenDays(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	// The folder's name is old though the record says it started now: an opened folder would be listed.
	writeRunIn(t, 20*24*time.Hour, time.Minute, "aaaaaa", "github.com/example/app", "", true)
	// Inside the day of slack the folders are opened with, but older than the window.
	writeRun(t, 14*24*time.Hour+6*time.Hour, "bbbbbb", "github.com/example/app", "", true)
	recent := writeRun(t, 13*24*time.Hour, "cccccc", "github.com/example/app", "", true)
	got, out := readRunsJSON(t, "--json", "--repo", "example/app")
	if len(got.Runs) != 1 || got.Runs[0].ID != recent {
		t.Fatalf("runs = %s; want only the run of 13 days ago", out)
	}
}

func TestRunsJSONCountsOnlyMatchingRunsTowardN(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	writeRun(t, 3*time.Hour, "aaaaaa", "github.com/example/app", "", true)
	newest := writeRun(t, 2*time.Hour, "bbbbbb", "github.com/example/app", "", true)
	writeRun(t, time.Hour, "cccccc", "github.com/example/other", "", true)
	got, out := readRunsJSON(t, "--json", "--repo", "example/app", "-n", "1")
	if len(got.Runs) != 1 || got.Runs[0].ID != newest {
		t.Fatalf("-n 1 = %s; want the newest run of the repository, though a newer run of another came first", out)
	}
}

func TestRunsJSONWithoutRepoListsEveryRepositoryAndAnEmptyListAsAnArray(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	if _, out := readRunsJSON(t, "--json"); !bytes.Contains(out, []byte(`"runs":[]`)) {
		t.Fatalf("no runs = %s; want an empty array", out)
	}
	writeRun(t, 30*24*time.Hour, "aaaaaa", "github.com/example/app", "", true)
	newest := writeRun(t, time.Hour, "bbbbbb", "", "", true)
	got, out := readRunsJSON(t, "--json", "-n", "5")
	if len(got.Runs) != 2 || got.Runs[0].ID != newest {
		t.Fatalf("--json alone = %s; want both runs, any age and repository, newest first", out)
	}
}

func TestRunsJSONAcceptsADotLedRepositoryName(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	id := writeRun(t, time.Hour, "aaaaaa", "github.com/example/.github", "", true)
	if got, out := readRunsJSON(t, "--json", "--repo", "example/.github"); len(got.Runs) != 1 || got.Runs[0].ID != id {
		t.Fatalf("runs = %s; want the run of example/.github", out)
	}
}

func TestRunsRefusesABadRepo(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--repo", "example/app"}, "--repo needs --json"},
		{[]string{"--json", "--repo", "app"}, "OWNER/NAME"},
		{[]string{"--json", "--repo", "../x"}, "OWNER/NAME"},
		{[]string{"--json", "--repo", "example/.."}, "OWNER/NAME"},
		{[]string{"--json", "--repo", "github.com/example/app"}, "OWNER/NAME"},
		{[]string{"--json", "--repo="}, "OWNER/NAME"},
		{[]string{"example/app", "--json"}, "unexpected argument"},
		{[]string{"--json", "--repo", "example/app", "extra"}, "unexpected argument"},
	} {
		out, err := captureStdout(t, func() error { return cmdRuns(c.args) })
		if err == nil || !strings.Contains(err.Error(), c.want) || len(out) != 0 {
			t.Errorf("hrn runs %v: err %v, stdout %q; want an error with %q and no output", c.args, err, out, c.want)
		}
	}
}
