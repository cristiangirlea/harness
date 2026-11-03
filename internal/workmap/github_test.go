package workmap

import (
	"context"
	"fmt"
	"github.com/cristiangirlea/harness/internal/ws"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitHubJoinsBranchesAndKeepsOnlySelectedFields(t *testing.T) {
	home := t.TempDir()
	old := ws.GH
	t.Cleanup(func() { ws.GH = old })
	calls := 0
	ws.GH = func(_ context.Context, args ...string) ([]byte, error) {
		calls++
		switch strings.Join(args, " ") {
		case "issue list -R owner/repo --state open --limit 100 --json number,title,url,updatedAt":
			return []byte(`[{"number":1,"title":"idea","url":"issue","body":"secret body","author":{"login":"secret author"}}]`), nil
		case "pr list -R owner/repo --state open --limit 100 --json number,title,url,headRefName,isDraft,reviewDecision,statusCheckRollup,updatedAt":
			return []byte(`[{"number":2,"title":"pull","url":"pr","headRefName":"feature","statusCheckRollup":[{"name":"test","conclusion":"FAILURE"}]}]`), nil
		default:
			return []byte(`[]`), nil
		}
	}
	pieces := map[string]*Piece{"repo\x00branch:feature": {Repo: "repo", Key: "branch:feature", Stage: Branch, Links: Links{Branch: "feature"}}}
	if err := githubRepo(context.Background(), home, "repo", "owner/repo", pieces); err != nil {
		t.Fatal(err)
	}
	if pieces["repo\x00branch:feature"].Stuck != "CI red: test" || len(pieces) != 2 {
		t.Fatalf("pieces: %+v", pieces)
	}
	if err := githubRepo(context.Background(), home, "repo", "owner/repo", pieces); err != nil || calls != 3 {
		t.Fatalf("cache: %d %v", calls, err)
	}
	data, err := os.ReadFile(filepath.Join(home, "workmap", "github", "owner__repo.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "secret") {
		t.Fatal("private fields cached")
	}
	oldTTL := githubTTL
	t.Cleanup(func() { githubTTL = oldTTL })
	githubTTL = 0
	if err := githubRepo(context.Background(), home, "repo", "owner/repo", pieces); err != nil || calls != 6 {
		t.Fatalf("refresh: %d %v", calls, err)
	}
}

func TestGitHubRefreshFallsBackAndRaisesMergedBranches(t *testing.T) {
	home := t.TempDir()
	old := ws.GH
	ttl := githubTTL
	t.Cleanup(func() { ws.GH = old; githubTTL = ttl })
	calls := 0
	fail := false
	ws.GH = func(_ context.Context, args ...string) ([]byte, error) {
		calls++
		if fail {
			return nil, fmt.Errorf("unavailable")
		}
		if args[0] == "issue" {
			return []byte(`[]`), nil
		}
		if args[5] == "open" {
			return []byte(`[{"number":2,"title":"new","url":"open","headRefName":"new","reviewDecision":"CHANGES_REQUESTED"}]`), nil
		}
		return []byte(`[{"number":3,"title":"merged","url":"merged","headRefName":"feature"}]`), nil
	}
	makePieces := func() map[string]*Piece {
		return map[string]*Piece{"repo\x00branch:feature": {Repo: "repo", Key: "branch:feature", Stage: Review, Stuck: "local", Links: Links{Branch: "feature"}}}
	}
	pieces := makePieces()
	if err := githubRepo(context.Background(), home, "repo", "owner/repo", pieces); err != nil {
		t.Fatal(err)
	}
	if p := pieces["repo\x00branch:feature"]; p.Stage != Merged || p.Stuck != "" {
		t.Fatalf("merged: %+v", p)
	}
	if p := pieces["repo\x00pr#2"]; p == nil || p.Stuck != "review: changes requested" {
		t.Fatalf("open: %+v", p)
	}
	githubTTL = 0
	fail = true
	pieces = makePieces()
	if err := githubRepo(context.Background(), home, "repo", "owner/repo", pieces); err == nil || pieces["repo\x00pr#2"] == nil {
		t.Fatal("failed refresh did not return cached data and error")
	}
	if calls != 4 {
		t.Fatalf("calls: %d", calls)
	}
}

func TestGitHubSkipsOtherRemotesAndDisabledSource(t *testing.T) {
	home := t.TempDir()
	repo := t.TempDir()
	old := ws.GH
	t.Cleanup(func() { ws.GH = old })
	calls := 0
	ws.GH = func(_ context.Context, args ...string) ([]byte, error) { calls++; return []byte(`[]`), nil }
	run := func(args ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git: %s: %v", out, err)
		}
	}
	run("init", "-b", "main")
	run("config", "user.email", "test@example.invalid")
	run("config", "user.name", "Test")
	run("commit", "--allow-empty", "-m", "initial")
	run("remote", "add", "origin", "https://example.com/owner/repo")
	if _, err := Build(context.Background(), home, []string{repo}); err != nil || calls != 0 {
		t.Fatalf("non-GitHub: %v %d", err, calls)
	}
	run("remote", "set-url", "origin", "git@github.com:owner/repo.git")
	if err := os.WriteFile(filepath.Join(home, "backends.json"), []byte(`{"work_map_github":false}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(context.Background(), home, []string{repo}); err != nil || calls != 0 {
		t.Fatalf("disabled: %v %d", err, calls)
	}
}
