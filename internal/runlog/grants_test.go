package runlog

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/owner"
)

func TestViewerProjectGrantLoadsAndListsRuns(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	t.Setenv("HRN_OWNER", "local:me")
	project := t.TempDir()
	if err := os.Mkdir(filepath.Join(project, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(project, "sub")
	if err := os.Mkdir(sub, 0700); err != nil {
		t.Fatal(err)
	}
	w, err := Create("fake", agent.Task{Dir: sub})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(agent.Result{}); err != nil {
		t.Fatal(err)
	}
	v := owner.Viewer{ID: "user:reader", Grants: []owner.Grant{{Viewer: "user:reader", Project: project}}}
	if _, err := LoadFor(v, w.ID()); err != nil {
		t.Fatal(err)
	}
	if recs, err := ListFor(v, 1); err != nil || len(recs) != 1 {
		t.Fatalf("list = %v, %v", recs, err)
	}
	v.Grants[0].Project = sub
	if _, err := LoadFor(v, w.ID()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a repository-root decision must not use the task subdirectory: %v", err)
	}
	v.Grants[0].Project = project + "2"
	if _, err := LoadFor(v, w.ID()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("foreign project: %v", err)
	}
	if recs, err := ListFor(v, 1); err != nil || len(recs) != 0 {
		t.Fatalf("foreign list = %v, %v", recs, err)
	}
}
