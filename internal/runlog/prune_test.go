package runlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
