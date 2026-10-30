package runlog

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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
	w, err := Create("test", agent.Task{Dir: dir})
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
	if !reflect.DeepEqual(r.Result.ChangedFiles, []string{".gitignore", "committed", "edit", "staged", "untracked"}) || r.StartHead == "" || r.Result.EndHead == r.StartHead {
		t.Fatalf("record: %+v", r)
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
