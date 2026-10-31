package workmap

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
