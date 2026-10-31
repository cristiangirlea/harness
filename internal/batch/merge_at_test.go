package batch

import (
	"context"
	"strings"
	"testing"
)

func TestMergeAtUsesConfirmedHead(t *testing.T) {
	dir := initRepo(t)
	res := mergeable(t, dir, Task{ID: "alpha", Prompt: "alpha"})
	tr := res.Tasks[0]
	head := git(t, dir, "rev-parse", tr.Branch)
	git(t, dir, "update-ref", "refs/heads/"+tr.Branch, git(t, dir, "rev-parse", "HEAD"))
	if _, err := MergeAt(context.Background(), res, tr.ID, head); err == nil || !strings.Contains(err.Error(), "branch moved") {
		t.Fatalf("moved branch: %v", err)
	}
	git(t, dir, "update-ref", "refs/heads/"+tr.Branch, head)
	got, err := MergeAt(context.Background(), res, tr.ID, head)
	if err != nil || len(got) != 1 || got[0].Commit == "" {
		t.Fatalf("merge: %+v %v", got, err)
	}
	if git(t, dir, "rev-parse", "HEAD^2") != head {
		t.Fatal("merged wrong head")
	}
}
