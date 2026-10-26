package eval

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/gitenv"
)

// Hide variant names in paths and git metadata to avoid judge bias.
func blindCheckout(ctx context.Context, repo, base, commit string) (_ string, err error) {
	dir, err := os.MkdirTemp("", "hrn-judge-")
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			if rerr := os.RemoveAll(dir); rerr != nil {
				err = errors.Join(err, fmt.Errorf("leaving %s behind: %w", dir, rerr))
			}
		}
	}()
	hooks, err := os.MkdirTemp("", "hrn-no-hooks-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(hooks)
	// Agent files must not activate templates, hooks, fsmonitor or signing.
	fresh := []string{"-c", "core.hooksPath=" + hooks, "-c", "core.fsmonitor=false",
		"-c", "commit.gpgSign=false", "-c", "gc.auto=0", "-c", "maintenance.auto=false"}
	run := func(args ...string) error {
		_, err := blindGit(ctx, dir, append(slices.Clone(fresh), args...)...)
		return err
	}
	if err := run("-c", "init.defaultBranch=candidate", "init", "-q", "--template="); err != nil {
		return "", err
	}
	// The judge's git status must not start daemons holding this copy open.
	if err := run("config", "core.fsmonitor", "false"); err != nil {
		return "", err
	}
	for i, step := range []struct{ commit, message string }{{base, "base"}, {commit, "candidate"}} {
		if i > 0 {
			if err := run("rm", "-rqf", "--ignore-unmatch", "--", "."); err != nil {
				return "", err
			}
		}
		if err := batch.CheckoutTree(ctx, repo, step.commit, dir); err != nil {
			return "", err
		}
		if err := run("add", "-A", "-f"); err != nil {
			return "", err
		}
		if err := run("commit", "-q", "--no-verify", "--allow-empty", "-m", step.message); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// Hide variant authors and prevent inherited git variables redirecting the copy.
func blindEnv() []string {
	return append(gitenv.WithoutRepoVars(os.Environ()), "GIT_AUTHOR_NAME=judge", "GIT_AUTHOR_EMAIL=judge@localhost",
		"GIT_COMMITTER_NAME=judge", "GIT_COMMITTER_EMAIL=judge@localhost", "LC_ALL=C", "LANGUAGE=")
}

func blindGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = blindEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// Shuffle reproducibly to avoid judging variants in blocks.
func judgeOrder(n int, seed string) []int {
	h := fnv.New64a()
	h.Write([]byte(seed))
	s := h.Sum64()
	return rand.New(rand.NewPCG(s, s)).Perm(n)
}
