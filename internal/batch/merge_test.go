package batch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/ws"
)

func TestMergeReceiptMustVerify(t *testing.T) {
	for _, squash := range []bool{false, true} {
		t.Run(map[bool]string{false: "merge", true: "squash"}[squash], func(t *testing.T) {
			dir := initRepo(t)
			res := mergeable(t, dir, Task{ID: "alpha", Prompt: "alpha"}, Task{ID: "beta", Prompt: "beta"})
			read := mergeReceiptRead
			t.Cleanup(func() { mergeReceiptRead = read })
			before := git(t, dir, "rev-parse", "HEAD")
			var landed string
			mergeReceiptRead = func(ctx context.Context, root string, args ...string) (string, error) {
				git(t, root, "commit", "--allow-empty", "-qm", "another commit")
				landed = git(t, root, "rev-parse", "HEAD")
				if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("keep"), 0600); err != nil {
					t.Fatal(err)
				}
				return read(ctx, root, args...)
			}
			out, err := Merge(context.Background(), res, nil, squash)
			if !errors.Is(err, ErrMergedUnconfirmed) || len(out) != 1 || out[0].Commit != "" || out[0].Error != "merged; the commit could not be confirmed" {
				t.Fatalf("out=%+v err=%v", out, err)
			}
			if out[0].Before != before {
				t.Fatalf("baseline=%s want %s", out[0].Before, before)
			}
			if git(t, dir, "rev-parse", "HEAD") != landed {
				t.Fatal("HEAD was reset")
			}
			if data, err := os.ReadFile(filepath.Join(dir, "README.md")); err != nil || string(data) != "keep" {
				t.Fatal("operator tree changed")
			}
		})
	}
}

func TestMergeRepositoryLock(t *testing.T) {
	dir := initRepo(t)
	res := mergeable(t, dir, Task{ID: "alpha", Prompt: "alpha"})
	path := filepath.Join(dir, ".git", "hrn-merge.lock")
	release, busy, err := ws.LockFile(path, "first merger")
	if err != nil || busy != "" {
		t.Fatalf("busy=%s err=%v", busy, err)
	}
	defer release()
	before := git(t, dir, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("operator work"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Merge(context.Background(), res, nil, false); !errors.Is(err, ErrMergeBusy) || !strings.Contains(err.Error(), "first merger") {
		t.Fatalf("err=%v", err)
	}
	if git(t, dir, "rev-parse", "HEAD") != before {
		t.Fatal("HEAD changed")
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "README.md")); string(data) != "operator work" {
		t.Fatal("tree changed")
	}
	release()
	git(t, dir, "reset", "--hard", "HEAD")
	if err := os.WriteFile(path, []byte(`{"holder":"dead","pid":2147483647}`), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := Merge(context.Background(), res, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if out[0].Before != before {
		t.Fatalf("baseline=%s want %s", out[0].Before, before)
	}
}

func TestMergeReceiptUsesFreshContext(t *testing.T) {
	for _, unreadable := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancelled", true: "unreadable"}[unreadable], func(t *testing.T) {
			dir := initRepo(t)
			res := mergeable(t, dir, Task{ID: "alpha", Prompt: "alpha"})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			read := mergeReceiptRead
			t.Cleanup(func() { mergeReceiptRead = read })
			mergeReceiptRead = func(receipt context.Context, root string, args ...string) (string, error) {
				cancel()
				if receipt.Err() != nil {
					t.Fatal("receipt inherits cancellation")
				}
				if _, ok := receipt.Deadline(); !ok {
					t.Fatal("receipt is unbounded")
				}
				if unreadable {
					return "", errors.New("receipt unavailable")
				}
				return read(receipt, root, args...)
			}
			out, err := Merge(ctx, res, nil, false)
			if unreadable {
				if !errors.Is(err, ErrMergedUnconfirmed) || len(out) != 1 || out[0].Commit != "" {
					t.Fatalf("out=%+v err=%v", out, err)
				}
			} else if err != nil || len(out) != 1 || out[0].Commit != git(t, dir, "rev-parse", "HEAD") {
				t.Fatalf("out=%+v err=%v", out, err)
			}
			if git(t, dir, "rev-parse", "HEAD^2") != res.Tasks[0].Commit {
				t.Fatal("merge was lost")
			}
		})
	}
}

func TestMergeLockCoversOtherWorktrees(t *testing.T) {
	dir := initRepo(t)
	res := mergeable(t, dir, Task{ID: "alpha", Prompt: "alpha"})
	other := filepath.Join(t.TempDir(), "other")
	git(t, dir, "worktree", "add", "--detach", other, "HEAD")
	read := mergeReceiptRead
	t.Cleanup(func() { mergeReceiptRead = read })
	mergeReceiptRead = func(ctx context.Context, root string, args ...string) (string, error) {
		before := git(t, root, "rev-parse", "HEAD")
		if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("keep"), 0600); err != nil {
			t.Fatal(err)
		}
		second := res
		second.Dir = other
		if _, err := Merge(ctx, second, nil, false); !errors.Is(err, ErrMergeBusy) {
			t.Fatalf("second merge: %v", err)
		}
		if git(t, root, "rev-parse", "HEAD") != before {
			t.Fatal("first merge moved")
		}
		if data, err := os.ReadFile(filepath.Join(root, "README.md")); err != nil || string(data) != "keep" {
			t.Fatal("first tree touched")
		}
		return read(ctx, root, args...)
	}
	if _, err := Merge(context.Background(), res, nil, false); err != nil {
		t.Fatal(err)
	}
}

func TestMergeCommandErrorAfterCommitKeepsReceipt(t *testing.T) {
	for _, squash := range []bool{false, true} {
		for _, unknown := range []bool{false, true} {
			t.Run(fmt.Sprintf("squash=%v/unknown=%v", squash, unknown), func(t *testing.T) {
				dir := initRepo(t)
				res := mergeable(t, dir, Task{ID: "alpha", Prompt: "alpha"})
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				step := mergeStep
				t.Cleanup(func() { mergeStep = step })
				var landed string
				mergeStep = func(ctx context.Context, g *agentGit, args ...string) (string, error) {
					out, err := step(ctx, g, args...)
					if err != nil {
						return out, err
					}
					if (!squash && args[0] == "merge") || (squash && args[0] == "commit") {
						if unknown {
							git(t, dir, "commit", "--allow-empty", "-qm", "intervening")
						}
						landed = git(t, dir, "rev-parse", "HEAD")
						if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("keep"), 0600); err != nil {
							t.Fatal(err)
						}
						cancel()
						return out, errors.New("command reported failure after committing")
					}
					return out, nil
				}
				out, err := Merge(ctx, res, nil, squash)
				if unknown {
					if !errors.Is(err, ErrMergedUnconfirmed) || len(out) != 1 || out[0].Commit != "" || out[0].Error != ErrMergedUnconfirmed.Error() {
						t.Fatalf("out=%+v err=%v", out, err)
					}
				} else if err != nil || len(out) != 1 || out[0].Commit != landed {
					t.Fatalf("out=%+v err=%v", out, err)
				}
				if git(t, dir, "rev-parse", "HEAD") != landed {
					t.Fatal("HEAD moved")
				}
				if data, err := os.ReadFile(filepath.Join(dir, "README.md")); err != nil || string(data) != "keep" {
					t.Fatalf("tree reset: %q %v", data, err)
				}
			})
		}
	}
}

func TestMergeOrderKeepsConfirmedBeforeUnknown(t *testing.T) {
	dir := initRepo(t)
	res := mergeable(t, dir, Task{ID: "alpha", Prompt: "alpha"}, Task{ID: "beta", Prompt: "beta"})
	read := mergeReceiptRead
	t.Cleanup(func() { mergeReceiptRead = read })
	var alpha string
	calls := 0
	mergeReceiptRead = func(ctx context.Context, root string, args ...string) (string, error) {
		calls++
		if calls == 2 {
			git(t, root, "commit", "--allow-empty", "-qm", "intervening")
		}
		commit, err := read(ctx, root, args...)
		if calls == 1 {
			alpha = commit
		}
		return commit, err
	}
	out, err := Merge(context.Background(), res, nil, false)
	if !errors.Is(err, ErrMergedUnconfirmed) || len(out) != 2 {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if out[0].Task != "alpha" || out[0].Commit != alpha || alpha == "" || out[0].Error != "" || out[1].Task != "beta" || out[1].Commit != "" || out[1].Error != ErrMergedUnconfirmed.Error() {
		t.Fatalf("out=%+v", out)
	}
}
