package runlog

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
)

func TestRunChangedFiles(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	write := func(name, data string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	git("init")
	git("config", "user.email", "test@example.test")
	git("config", "user.name", "Test")
	write("edit", "before")
	git("add", "edit")
	git("commit", "-m", "initial")
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	w, err := Create("test", agent.Task{Dir: filepath.Join(dir, "sub")})
	if err != nil {
		t.Fatal(err)
	}
	write("committed", "new")
	git("add", "committed")
	git("commit", "-m", "during")
	write("edit", "after")
	write("untracked", "new")
	write("staged", "new")
	git("add", "staged")
	write(".gitignore", "ignored\n")
	write("ignored", "ignored")
	if err := w.Close(agent.Result{}); err != nil {
		t.Fatal(err)
	}
	r, err := Load(w.ID())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.Result.ChangedFiles, []string{".gitignore", "committed", "edit", "staged", "untracked"}) || r.StartHead == "" || r.Result.EndHead == r.StartHead || !reflect.DeepEqual(r.Result.Commits, []string{r.Result.EndHead}) {
		t.Fatalf("record: %+v", r)
	}
}

func TestRunChangedFilesDoesNotRunWorkspaceFilters(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	dir := t.TempDir()
	marker := filepath.ToSlash(filepath.Join(t.TempDir(), "filter-ran"))
	git := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	write := func(name, data string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	git("init")
	git("config", "user.email", "test@example.test")
	git("config", "user.name", "Test")
	write("a.go", "before\n")
	write("b.txt", "before\n")
	git("add", ".")
	git("commit", "-m", "initial")
	w, err := Create("test", agent.Task{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	git("config", "filter.evil.clean", "echo clean >> '"+marker+"'; cat")
	git("config", "filter.evil.required", "true")
	git("config", "filter.loud.process", "echo process >> '"+marker+"'")
	write(".gitattributes", "*.go filter=evil\n*.txt filter=loud\n")
	write("a.go", "after\n")
	write("b.txt", "after\n")
	if err := w.Close(agent.Result{}); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(marker); err == nil {
		t.Fatalf("workspace filter ran: %q", data)
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
	r, err := Load(w.ID())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.Result.ChangedFiles, []string{".gitattributes", "a.go", "b.txt"}) {
		t.Fatalf("changed files: %v", r.Result.ChangedFiles)
	}
}

func TestRunChangedFilesTruncated(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git: %v: %s", err, out)
		}
	}
	git("init")
	git("config", "user.email", "test@example.test")
	git("config", "user.name", "Test")
	git("commit", "--allow-empty", "-m", "initial")
	w, err := Create("test", agent.Task{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 501; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("file%03d", i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(agent.Result{}); err != nil {
		t.Fatal(err)
	}
	r, err := Load(w.ID())
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Result.ChangedFiles) != 500 || !r.Result.ChangedFilesTruncated {
		t.Fatalf("result: %+v", r.Result)
	}
}

func TestRunChangedFilesNonRepoAndOldRecord(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	w, err := Create("test", agent.Task{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(agent.Result{}); err != nil {
		t.Fatal(err)
	}
	r, err := Load(w.ID())
	if err != nil || len(r.Result.ChangedFiles) != 0 {
		t.Fatalf("record: %+v, %v", r, err)
	}
	if err := os.WriteFile(filepath.Join(w.Dir(), "result.json"), []byte(`{"backend":"old","result":{"status":"ok"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	r, err = Load(w.ID())
	if err != nil || r.Result.Status != agent.StatusOK || len(r.Result.ChangedFiles) != 0 {
		t.Fatalf("old record: %+v, %v", r, err)
	}
}

func TestChangedFilesExcludesMergedSideCommits(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", dir}, args...)...)
		b, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, b)
		}
		return strings.TrimSpace(string(b))
	}
	git("init")
	git("config", "user.email", "test@example.test")
	git("config", "user.name", "Test")
	git("commit", "--allow-empty", "-m", "base")
	base := git("rev-parse", "HEAD")
	git("checkout", "-b", "side")
	git("commit", "--allow-empty", "-m", "side")
	side := git("rev-parse", "HEAD")
	git("checkout", "-b", "task", base)
	git("commit", "--allow-empty", "-m", "task")
	git("merge", "--no-ff", "side", "-m", "merge")
	_, _, _, commits := changedFiles(dir, base)
	for _, commit := range commits {
		if commit == side {
			t.Fatal("side commit included")
		}
	}
	if len(commits) != 2 {
		t.Fatalf("commits: %v", commits)
	}
}
