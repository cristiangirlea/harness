package workmap

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func reviewRepo(t *testing.T) (string, func(...string)) {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", dir}, args...)...)
		c.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+filepath.Join(t.TempDir(), "none"))
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s %v", args, out, err)
		}
	}
	git("init", "-b", "main")
	git("config", "user.name", "Test")
	git("config", "user.email", "test@example.invalid")
	git("commit", "--allow-empty", "-m", "initial")
	return dir, git
}
func reviewWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}
func reviewFind(t *testing.T, pieces []Piece, key string) Piece {
	t.Helper()
	for _, p := range pieces {
		if p.Key == key {
			return p
		}
	}
	t.Fatalf("missing %s: %+v", key, pieces)
	return Piece{}
}
func TestBranchAtDefaultTipIsNotMerged(t *testing.T) {
	repo, git := reviewRepo(t)
	git("branch", "same")
	pieces, err := Build(context.Background(), t.TempDir(), []string{repo})
	if err != nil {
		t.Fatal(err)
	}
	if p := reviewFind(t, pieces, "branch:same"); p.Stage != Branch {
		t.Fatalf("%+v", p)
	}
}
func TestMergeUsesRemoteDefaultWithoutLocalMain(t *testing.T) {
	repo, git := reviewRepo(t)
	git("checkout", "-b", "feature")
	git("commit", "--allow-empty", "-m", "feature")
	git("update-ref", "refs/remotes/origin/main", "HEAD")
	git("symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	git("checkout", "--detach")
	git("branch", "-D", "main")
	git("commit", "--allow-empty", "-m", "remote")
	git("update-ref", "refs/remotes/origin/main", "HEAD")
	pieces, err := Build(context.Background(), t.TempDir(), []string{repo})
	if err != nil {
		t.Fatal(err)
	}
	if p := reviewFind(t, pieces, "branch:feature"); p.Stage != Merged {
		t.Fatalf("%+v", p)
	}
}
func TestMainCheckoutRunAndRepositoryDiscovery(t *testing.T) {
	repo, git := reviewRepo(t)
	git("branch", "unmerged")
	git("checkout", "unmerged")
	git("commit", "--allow-empty", "-m", "feature")
	git("checkout", "main")
	home := t.TempDir()
	reviewWrite(t, filepath.Join(home, "runs", "r1", "task.json"), fmt.Sprintf(`{"id":"r1","task":{"prompt":"run","dir":%s}}`, strconv.Quote(repo)))
	for _, configured := range []bool{false, true} {
		if configured {
			reviewWrite(t, filepath.Join(home, "backends.json"), fmt.Sprintf(`{"work_map_repos":[%s]}`, strconv.Quote(repo)))
		}
		pieces, err := Build(context.Background(), home, nil)
		if err != nil {
			t.Fatal(err)
		}
		if p := reviewFind(t, pieces, "run:r1"); p.Stage != Agent || p.Links.Branch != "" {
			t.Fatalf("%+v", p)
		}
		if p := reviewFind(t, pieces, "branch:unmerged"); p.Stage != Branch {
			t.Fatalf("%+v", p)
		}
	}
}
func TestStuckFollowsNewestSourceAndClearsWhenMerged(t *testing.T) {
	for _, tc := range []struct {
		name                string
		olderFailed, merged bool
		want                string
	}{{"newer success", true, false, ""}, {"newer failure", false, false, "gate test failed"}, {"merged", false, true, ""}} {
		t.Run(tc.name, func(t *testing.T) {
			repo, git := reviewRepo(t)
			git("checkout", "-b", "feature")
			git("commit", "--allow-empty", "-m", "feature")
			git("checkout", "main")
			if tc.merged {
				git("merge", "--no-ff", "feature", "-m", "merge")
			}
			home := t.TempDir()
			for i := 1; i <= 2; i++ {
				failed := (i == 2) == tc.olderFailed
				code := 0
				if failed {
					code = 1
				}
				raw := fmt.Sprintf(`{"id":"b%d","started":"2030-01-0%dT00:00:00Z","dir":%s,"tasks":[{"id":"t","branch":"feature","attempts":[{"gates":[{"command":"test","exit_code":%d}]}]}]}`, i, 3-i, strconv.Quote(repo), code)
				reviewWrite(t, filepath.Join(home, "batches", fmt.Sprintf("b%d.json", i)), raw)
			}
			pieces, err := Build(context.Background(), home, []string{repo})
			if err != nil {
				t.Fatal(err)
			}
			p := reviewFind(t, pieces, "branch:feature")
			if p.Stuck != tc.want {
				t.Fatalf("%+v", p)
			}
		})
	}
}

func TestFailedBatchBeforeBranchCommitRemainsStuck(t *testing.T) {
	repo, git := reviewRepo(t)
	git("checkout", "-b", "feature")
	git("commit", "--allow-empty", "-m", "feature")
	home := t.TempDir()
	raw := fmt.Sprintf(`{"id":"b1","started":"2020-01-01T00:00:00Z","dir":%s,"tasks":[{"id":"t","branch":"feature","attempts":[{"gates":[{"command":"test","exit_code":1}]}]}]}`, strconv.Quote(repo))
	reviewWrite(t, filepath.Join(home, "batches", "b1.json"), raw)
	pieces, err := Build(context.Background(), home, []string{repo})
	if err != nil {
		t.Fatal(err)
	}
	if p := reviewFind(t, pieces, "branch:feature"); p.Stuck != "gate test failed" {
		t.Fatalf("%+v", p)
	}
}

func TestConfiguredRepositoriesTakePrecedenceOverRecords(t *testing.T) {
	configured, git := reviewRepo(t)
	git("branch", "configured")
	recorded, git := reviewRepo(t)
	git("branch", "recorded")
	home := t.TempDir()
	reviewWrite(t, filepath.Join(home, "runs", "r1", "task.json"), fmt.Sprintf(`{"id":"r1","task":{"dir":%s}}`, strconv.Quote(recorded)))
	reviewWrite(t, filepath.Join(home, "backends.json"), fmt.Sprintf(`{"work_map_repos":[%s]}`, strconv.Quote(configured)))
	pieces, err := Build(context.Background(), home, nil)
	if err != nil {
		t.Fatal(err)
	}
	reviewFind(t, pieces, "branch:configured")
	for _, p := range pieces {
		if p.Repo == canonical(recorded) {
			t.Fatalf("record repository included: %+v", p)
		}
	}
}

func TestReviewCommitAssociationAndCaching(t *testing.T) {
	repo, runGit := reviewRepo(t)
	home := t.TempDir()
	head := func() string {
		b, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(b))
	}
	mainHead := head()
	runGit("checkout", "-b", "feature")
	runGit("commit", "--allow-empty", "-m", "feature")
	featureHead := head()
	runGit("checkout", "main")
	runGit("checkout", "-b", "other")
	for i, oid := range []string{mainHead, featureHead, featureHead, "", "invalid"} {
		id := fmt.Sprintf("r%d", i)
		raw := fmt.Sprintf(`{"id":%q,"start_head":%q,"task":{"dir":%s,"purpose":"review"}}`, id, oid, strconv.Quote(repo))
		reviewWrite(t, filepath.Join(home, "runs", id, "task.json"), raw)
		reviewWrite(t, filepath.Join(home, "runs", id, "result.json"), raw)
	}
	old := gitCall
	t.Cleanup(func() { gitCall = old })
	calls := 0
	gitCall = func(ctx context.Context, dir string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "for-each-ref" && len(args) > 1 && args[1] == "--contains" {
			calls++
		}
		return git(ctx, dir, args...)
	}
	pieces, err := Build(context.Background(), home, []string{repo})
	if err != nil {
		t.Fatal(err)
	}
	feature := reviewFind(t, pieces, "branch:feature")
	if feature.Stage != Review || strings.Join(feature.Links.Runs, ",") != "r1,r2" {
		t.Fatalf("feature: %+v", feature)
	}
	other := reviewFind(t, pieces, "branch:other")
	if other.Stage == Review || strings.Contains(strings.Join(other.Links.Runs, ","), "r0") {
		t.Fatalf("other: %+v", other)
	}
	if calls != 1 {
		t.Fatalf("contains calls: %d", calls)
	}
}
