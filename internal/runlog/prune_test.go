package runlog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPruneOrphanFlows(t *testing.T) {
	for _, dryRun := range []bool{false, true} {
		for _, withRuns := range []bool{false, true} {
			t.Run(fmt.Sprintf("dry=%t/runs=%t", dryRun, withRuns), func(t *testing.T) {
				t.Setenv("HRN_HOME", t.TempDir())
				if withRuns {
					runAged(t, 1, "cccccc", 10)
				}
				orphan, kept := "20261020-120000-aaaaaa", "20261020-120001-bbbbbb"
				batches := filepath.Join(Home(), "batches")
				flows := filepath.Join(Home(), "batch-flows")
				for _, id := range []string{orphan, kept} {
					write(t, batches, id+".json", `{"id":"`+id+`"}`)
					write(t, flows, id+".jsonl", "{}\n")
				}
				if err := os.Remove(filepath.Join(batches, orphan+".json")); err != nil {
					t.Fatal(err)
				}
				write(t, flows, "notes.jsonl", "keep")
				write(t, flows, orphan+".txt", "keep")
				write(t, filepath.Join(flows, "20261020-120002-dddddd.jsonl"), "notes", "keep")
				res, err := Prune(time.Now().AddDate(0, 0, -30), 0, nil, dryRun)
				if err != nil {
					t.Fatal(err)
				}
				var counts struct {
					OrphanFlows int `json:"orphan_flows"`
				}
				data, err := json.Marshal(res)
				if err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(data, &counts); err != nil {
					t.Fatal(err)
				}
				if counts.OrphanFlows != 1 || res.Bytes != 3 || len(res.Removed) != 0 {
					t.Fatalf("prune = %s, want one orphan flow and 3 bytes", data)
				}
				if got := exists(filepath.Join(flows, orphan+".jsonl")); got != dryRun {
					t.Errorf("orphan exists = %v, dry run = %v", got, dryRun)
				}
				for _, name := range []string{kept + ".jsonl", "notes.jsonl", orphan + ".txt", "20261020-120002-dddddd.jsonl"} {
					if !exists(filepath.Join(flows, name)) {
						t.Errorf("prune removed %s", name)
					}
				}
			})
		}
	}
}

// runAged makes a finished run whose id says it started days ago, with size
// bytes of events and a two-byte result, and returns its id.
func runAged(t *testing.T, days int, suffix string, size int) string {
	t.Helper()
	id := time.Now().AddDate(0, 0, -days).Format(idTimeLayout) + "-" + suffix
	write(t, filepath.Join(Root(), id), "events.jsonl", strings.Repeat("x", size))
	write(t, filepath.Join(Root(), id), "result.json", "{}")
	return id
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestPruneRemovesOnlyOldUnprotectedRuns(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	old1 := runAged(t, 40, "aaaaaa", 100)
	cited := runAged(t, 35, "bbbbbb", 100)
	old3 := runAged(t, 31, "cccccc", 100)
	fresh := runAged(t, 1, "dddddd", 100)
	// Not an id: nothing can say how old it is, so it is never touched.
	write(t, filepath.Join(Root(), "notes"), "readme", "mine")

	cutoff := time.Now().AddDate(0, 0, -30)
	protect := map[string]bool{cited: true}

	res, err := Prune(cutoff, 0, protect, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(res.Removed, ","); got != old3+","+old1 {
		t.Fatalf("dry run would remove %q, want %s,%s", got, old3, old1)
	}
	if res.Bytes != 204 {
		t.Errorf("bytes = %d, want 204", res.Bytes)
	}
	for _, id := range []string{old1, cited, old3, fresh} {
		if !exists(filepath.Join(Root(), id)) {
			t.Fatalf("a dry run removed %s", id)
		}
	}

	if _, err := Prune(cutoff, 0, protect, false); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]bool{old1: false, old3: false, cited: true, fresh: true, "notes": true} {
		if got := exists(filepath.Join(Root(), id)); got != want {
			t.Errorf("%s exists = %v, want %v", id, got, want)
		}
	}
}

func TestPruneKeepsTheNewestRunsWhateverTheirAge(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	oldest := runAged(t, 60, "aaaaaa", 10)
	middle := runAged(t, 50, "bbbbbb", 10)
	newest := runAged(t, 40, "cccccc", 10)
	write(t, filepath.Join(Root(), "notes"), "readme", "mine") // does not count towards keep

	res, err := Prune(time.Now().AddDate(0, 0, -30), 2, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Removed, ",") != oldest {
		t.Fatalf("removed %v, want only %s", res.Removed, oldest)
	}
	if !exists(filepath.Join(Root(), middle)) || !exists(filepath.Join(Root(), newest)) {
		t.Error("--keep 2 must keep the two newest runs")
	}
}

func TestPruneWithNoRunsDirectory(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	res, err := Prune(time.Now(), 0, nil, false)
	if err != nil || len(res.Removed) != 0 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestPruneSkipsARunStillBeingWritten(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	old := time.Now().AddDate(0, 0, -40).Format(idTimeLayout)
	running, crashed := old+"-aaaaaa", old+"-bbbbbb"
	// Neither has a result; one is still being written to, one stopped long ago.
	write(t, filepath.Join(Root(), running), "events.jsonl", "{}")
	write(t, filepath.Join(Root(), crashed), "events.jsonl", "{}")
	stale := time.Now().Add(-3 * time.Hour)
	if err := os.Chtimes(filepath.Join(Root(), crashed, "events.jsonl"), stale, stale); err != nil {
		t.Fatal(err)
	}

	res, err := Prune(time.Now().AddDate(0, 0, -30), 0, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Removed, ",") != crashed {
		t.Fatalf("removed %v, want only the crashed run %s", res.Removed, crashed)
	}
	if !exists(filepath.Join(Root(), running)) {
		t.Error("a run still being written must not be removed")
	}
}

func TestPruneKeepsWhatAKeptRunContinues(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	first := runAged(t, 50, "aaaaaa", 1)
	second := runAged(t, 45, "bbbbbb", 1)
	third := runAged(t, 1, "cccccc", 1) // kept by age
	lone := runAged(t, 40, "dddddd", 1)
	// third continues second, which continues first: the chain stays whole.
	write(t, filepath.Join(Root(), second), "result.json", `{"id":"`+second+`","continues":"`+first+`"}`)
	write(t, filepath.Join(Root(), third), "result.json", `{"id":"`+third+`","continues":"`+second+`"}`)

	res, err := Prune(time.Now().AddDate(0, 0, -30), 0, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Removed, ",") != lone {
		t.Fatalf("removed %v, want only %s", res.Removed, lone)
	}
	for _, id := range []string{first, second, third} {
		if !exists(filepath.Join(Root(), id)) {
			t.Errorf("%s is cited by a kept run and must stay", id)
		}
	}
}

func TestPruneOnlyRemovesNoRunOutsideTheGivenList(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	listed := runAged(t, 40, "aaaaaa", 10)
	unlisted := runAged(t, 40, "bbbbbb", 10)
	res, err := PruneOnly(time.Now().AddDate(0, 0, -30), 0, nil, map[string]bool{listed: true}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Removed) != 1 || res.Removed[0] != listed {
		t.Fatalf("removed %v, want only %s", res.Removed, listed)
	}
	if exists(filepath.Join(Root(), listed)) || !exists(filepath.Join(Root(), unlisted)) {
		t.Fatal("PruneOnly must remove the listed run and keep the one it was not given")
	}
}

func TestPruneScriptHints(t *testing.T) {
	for _, dry := range []bool{true, false} {
		t.Run(fmt.Sprint(dry), func(t *testing.T) {
			t.Setenv("HRN_HOME", t.TempDir())
			path := filepath.Join(Home(), "script-hints.jsonl")
			input := "{\"time\":\"2020-01-01T00:00:00Z\"}\ninvalid\n{\"time\":\"2030-01-01T00:00:00Z\"}\n"
			if err := os.WriteFile(path, []byte(input), 0600); err != nil {
				t.Fatal(err)
			}
			res, err := Prune(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), 0, nil, dry)
			if err != nil {
				t.Fatal(err)
			}
			data, _ := json.Marshal(res)
			if !strings.Contains(string(data), `"script_hints":2`) {
				t.Fatalf("result %s", data)
			}
			got, _ := os.ReadFile(path)
			want := "{\"time\":\"2030-01-01T00:00:00Z\"}\n"
			if dry {
				want = input
			}
			if string(got) != want {
				t.Fatalf("got %q", got)
			}
		})
	}
}

func TestPruneHintsPreservesTail(t *testing.T) {
	cutoff := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct{ name, input, appended, want string }{
		{"missing", "", "", ""},
		{"unterminated", "{\"time\":\"2020-01-01T00:00:00Z\"}\n{\"time\":\"2030-01-01T00:00:00Z\"}", "", "{\"time\":\"2030-01-01T00:00:00Z\"}"},
		{"appended", "{\"time\":\"2020-01-01T00:00:00Z\"}\n", "new line\n", "new line\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HRN_HOME", t.TempDir())
			path := filepath.Join(Home(), "script-hints.jsonl")
			if tc.input != "" {
				if err := os.WriteFile(path, []byte(tc.input), 0600); err != nil {
					t.Fatal(err)
				}
			}
			beforeHintsRename = func() {
				f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.WriteString(tc.appended); err != nil {
					t.Fatal(err)
				}
				f.Close()
			}
			defer func() { beforeHintsRename = func() {} }()
			if _, err := pruneHints(cutoff, false); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(path)
			if tc.name == "missing" && os.IsNotExist(err) {
				return
			}
			if err != nil || string(got) != tc.want {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestPruneHintsRenameFailureKeepsFile(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	path := filepath.Join(Home(), "script-hints.jsonl")
	input := "{\"time\":\"2020-01-01T00:00:00Z\"}\n"
	if err := os.WriteFile(path, []byte(input), 0600); err != nil {
		t.Fatal(err)
	}
	renameHints = func(_, _ string) error { return fmt.Errorf("rename blocked") }
	defer func() { renameHints = os.Rename }()
	count, err := pruneHints(time.Now(), false)
	got, readErr := os.ReadFile(path)
	if err != nil || count != 0 || readErr != nil || string(got) != input {
		t.Fatalf("count %d, error %v, contents %q, read error %v", count, err, got, readErr)
	}
}
