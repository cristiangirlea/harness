package runlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// runAged makes a run directory whose id says it started days ago, holding
// size bytes, and returns its id.
func runAged(t *testing.T, days int, suffix string, size int) string {
	t.Helper()
	id := time.Now().AddDate(0, 0, -days).Format(idTimeLayout) + "-" + suffix
	write(t, filepath.Join(Root(), id), "events.jsonl", strings.Repeat("x", size))
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
	if res.Bytes != 200 {
		t.Errorf("bytes = %d, want 200", res.Bytes)
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
