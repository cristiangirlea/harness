package batch

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cristiangirlea/harness/internal/gitenv"
	"github.com/cristiangirlea/harness/internal/gitretry"
	"github.com/cristiangirlea/harness/internal/nowindow"
	"github.com/cristiangirlea/harness/internal/tools"
)

func branchRef(branch string) (string, error) {
	if branch == "" || strings.HasPrefix(branch, "-") {
		return "", fmt.Errorf("invalid recorded branch %q", branch)
	}
	return "refs/heads/" + branch, nil
}

// Agents start at HEAD and cannot see tracked edits.
func checkClean(ctx context.Context, root string) error {
	lines, err := dirtyPaths(ctx, root, false)
	if err != nil {
		return err
	}
	if len(lines) == 0 {
		return nil
	}
	shown, more := lines, ""
	if len(shown) > 5 {
		shown, more = shown[:5], fmt.Sprintf("\n  ... and %d more", len(lines)-5)
	}
	return fmt.Errorf("the repository has uncommitted changes; the agents branch from HEAD and would not see them. "+
		"Commit or stash them, or pass --allow-dirty (allow_dirty in the spec):\n  %s%s",
		strings.Join(shown, "\n  "), more)
}

// Commit despite cancellation to leave a consistent branch; note says why off-branch work must stay in the worktree.
func commitChanges(wt, branch, message string) (sha, note string, err error) {
	ref, err := branchRef(branch)
	if err != nil && branch != "" {
		return "", "", err
	}
	if _, err := os.Stat(wt); err != nil {
		return "", "", nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	g, err := newAgentGit(ctx, wt)
	if err != nil {
		return "", "", err
	}
	defer g.close()
	status, err := g.stdout(ctx, "status", "--porcelain", "-z", "--no-renames", "--ignore-submodules=all")
	if err != nil {
		return "", "", err
	}
	if branch != "" {
		// A merge takes the task's branch, so a commit anywhere else would be recorded for nothing.
		// Prints nothing for a detached HEAD and names an unborn branch, failing only when git does.
		head, err := g.stdout(ctx, "branch", "--show-current")
		if err != nil {
			return "", "", err
		}
		if head = strings.TrimSuffix(strings.TrimSuffix(head, "\n"), "\r"); head != branch {
			where := "HEAD is detached"
			if head != "" {
				where = "HEAD is on branch " + head
			}
			where += ", not the task's branch " + branch
			if status != "" {
				return "", "work left uncommitted in the worktree: " + where, nil
			}
			refs, err := g.stdout(ctx, "for-each-ref", "--format=%(refname) %(objectname)", "--", ref)
			if err != nil {
				return "", "", err
			}
			commit, err := g.stdout(ctx, "rev-parse", "-q", "--verify", "HEAD")
			if err != nil {
				if head == "" {
					return "", "", err
				}
				// Only a confirmed unborn branch can have no HEAD commit.
				headRef := "refs/heads/" + head
				headRefs, refErr := g.stdout(ctx, "for-each-ref", "--format=%(refname)", "--", headRef)
				if refErr != nil || slices.Contains(strings.Split(headRefs, "\n"), headRef) {
					return "", "", err
				}
			}
			commit = strings.TrimSpace(commit)
			if commit != "" && !slices.Contains(strings.Split(refs, "\n"), "refs/heads/"+branch+" "+commit) {
				return "", "commit " + tools.ShortSHA(commit) + " left in the worktree: " + where, nil
			}
		}
	}
	if status == "" {
		return "", "", nil
	}
	// Stage reported paths: add -A ignores diff.ignoreSubmodules.
	var paths []string
	for _, entry := range strings.Split(status, "\x00") {
		if len(entry) > 3 {
			paths = append(paths, entry[3:])
		}
	}
	if _, err := g.in(ctx, strings.Join(paths, "\x00"),
		"--literal-pathspecs", "add", "-A", "--pathspec-from-file=-", "--pathspec-file-nul"); err != nil {
		return "", "", err
	}
	if _, err := g.out(ctx, "commit", "-q", "-m", message); err != nil {
		return "", "", err
	}
	sha, err = g.out(ctx, "rev-parse", "HEAD")
	return sha, "", err
}

// gitTips is the task branch's tip and HEAD's commit, so a commit off the branch shows too; "" without a worktree.
func gitTips(wt, branch string) (string, error) {
	ref, err := branchRef(branch)
	if err != nil && branch != "" {
		return "", err
	}
	if _, err := os.Stat(wt); err != nil || branch == "" {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	g, err := newAgentGit(ctx, wt)
	if err != nil {
		return "", err
	}
	defer g.close()
	out, err := g.stdout(ctx, "for-each-ref", "--format=%(refname) %(objectname)", "--", ref)
	if err != nil {
		return "", err
	}
	tip := ""
	for _, line := range strings.Split(out, "\n") {
		if name, head, ok := strings.Cut(line, " "); ok && name == "refs/heads/"+branch {
			tip = head
		}
	}
	// An orphan branch has no HEAD commit yet; that is a move, not a failed read.
	head, _ := g.stdout(ctx, "rev-parse", "-q", "--verify", "HEAD")
	return tip + " " + strings.TrimSpace(head), nil
}

// agentCommit returns the branch's tip when the agent committed its own work on it
// and left nothing for commitChanges, or "" while the branch is still at the base;
// note says why a moved tip was left out. A commit on no branch is never taken.
func agentCommit(wt, branch, base string) (sha, note string, err error) {
	ref, err := branchRef(branch)
	if err != nil && branch != "" {
		return "", "", err
	}
	if _, err := os.Stat(wt); err != nil || branch == "" || base == "" {
		return "", "", nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	g, err := newAgentGit(ctx, wt)
	if err != nil {
		return "", "", err
	}
	defer g.close()
	// for-each-ref matches descendants too; require the exact ref.
	out, err := g.stdout(ctx, "for-each-ref", "--format=%(refname) %(objectname)", "--", ref)
	if err != nil {
		return "", "", err
	}
	for _, line := range strings.Split(out, "\n") {
		name, head, ok := strings.Cut(line, " ")
		if !ok || name != "refs/heads/"+branch || head == base {
			continue
		}
		// A tip that dropped the base would revert it when merged.
		missing, err := g.stdout(ctx, "rev-list", "--count", "--end-of-options", head+".."+base, "--")
		if err != nil {
			return "", "", err
		}
		if strings.TrimSpace(missing) != "0" {
			return "", "commit not recorded: the branch tip " + tools.ShortSHA(head) + " does not contain the base", nil
		}
		return head, "", nil
	}
	return "", "", nil
}

// Agent config must not execute hooks, drivers, signing or maintenance.
// Use GIT_CONFIG_KEY/VALUE (-c splits filter names at '='); never enter submodules, whose config the agent writes.
type agentGit struct {
	dir   string
	hooks string
	temp  bool // hooks is an empty directory close removes
	pin   []string
}

// Disable all hooks in agent worktrees.
func newAgentGit(ctx context.Context, dir string) (*agentGit, error) {
	g, err := pinnedGit(ctx, dir)
	if err != nil {
		return nil, err
	}
	if g.hooks, err = os.MkdirTemp("", "hrn-empty-hooks-"); err != nil {
		return nil, err
	}
	g.temp = true
	return g, nil
}

// Trust operator hooks, never hooks paths in shared config.
func newRepoGit(ctx context.Context, root string) (*agentGit, error) {
	g, err := pinnedGit(ctx, root)
	if err != nil {
		return nil, err
	}
	if g.hooks = operatorHooksPath(ctx, g.dir, g.pin); g.hooks != "" {
		return g, nil
	}
	common, err := gitStdoutEnv(ctx, g.dir, g.pin, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return nil, err
	}
	g.hooks = filepath.Join(strings.TrimSpace(common), "hooks")
	return g, nil
}

func operatorHooksPath(ctx context.Context, dir string, pin []string) string {
	out, err := gitStdoutEnv(ctx, dir, pin, "config", "--show-scope", "--get-regexp", `^core\.hookspath$`)
	if err != nil {
		return ""
	}
	path := ""
	for _, line := range strings.Split(out, "\n") {
		scope, entry, _ := strings.Cut(line, "\t")
		switch scope {
		case "global", "system", "command":
			_, path, _ = strings.Cut(entry, " ")
		}
	}
	return path
}

// Find .git directly; shared core.worktree can redirect rev-parse.
func repoRoot(ctx context.Context, dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for d := abs; ; d = filepath.Dir(d) {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			g, err := pinnedGit(ctx, d)
			if err != nil {
				return "", err
			}
			top, err := gitOutEnv(ctx, d, g.pin, "", "rev-parse", "--show-toplevel")
			if err != nil {
				return "", err
			}
			return filepath.Clean(filepath.FromSlash(top)), nil
		}
		if filepath.Dir(d) == d {
			return "", fmt.Errorf("%s is not inside a git work tree", dir)
		}
	}
}

func pinnedGit(ctx context.Context, dir string) (*agentGit, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	// Set GIT_WORK_TREE before discovery to bypass hostile core.worktree.
	work := "GIT_WORK_TREE=" + dir
	gitDir, err := gitStdoutEnv(ctx, dir, []string{work}, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return nil, err
	}
	// Disable lazy fetch to prevent shared promisor upload-pack execution.
	return &agentGit{dir: dir, pin: []string{"GIT_DIR=" + strings.TrimSpace(gitDir), work, "GIT_NO_LAZY_FETCH=1"}}, nil
}

func (g *agentGit) close() {
	if g.temp {
		_ = os.RemoveAll(g.hooks)
	}
}

func (g *agentGit) out(ctx context.Context, args ...string) (string, error) {
	return gitOutEnv(ctx, g.dir, g.env(ctx), "", args...)
}

func (g *agentGit) in(ctx context.Context, stdin string, args ...string) (string, error) {
	return gitOutEnv(ctx, g.dir, g.env(ctx), stdin, args...)
}

func (g *agentGit) stdout(ctx context.Context, args ...string) (string, error) {
	return gitStdoutEnv(ctx, g.dir, g.env(ctx), args...)
}

// Untranslated wording keeps a value git prints the same on every machine.
func (g *agentGit) stdoutIn(ctx context.Context, stdin string, args ...string) (string, error) {
	return gitStdoutIn(ctx, g.dir, append(g.env(ctx), "LC_ALL=C", "LANGUAGE="), stdin, args...)
}

// Rescan: agent descendants can install filters between commands.
func (g *agentGit) env(ctx context.Context) []string {
	return append(slices.Clone(g.pin), configEnv(agentConfigOverrides(ctx, g.dir, g.pin, g.hooks))...)
}

// Append guards last so they override operator entries.
func configEnv(pairs [][2]string) []string {
	n, err := strconv.Atoi(os.Getenv("GIT_CONFIG_COUNT"))
	if err != nil || n < 0 {
		n = 0
	}
	env := make([]string, 0, 2*len(pairs)+1)
	for i, p := range pairs {
		env = append(env,
			fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", n+i, p[0]),
			fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", n+i, p[1]))
	}
	return append(env, fmt.Sprintf("GIT_CONFIG_COUNT=%d", n+len(pairs)))
}

// Block local/worktree programs; trust global/system drivers.
func agentConfigOverrides(ctx context.Context, dir string, pin []string, hooks string) [][2]string {
	pairs := [][2]string{
		{"core.hooksPath", hooks},
		{"maintenance.auto", "false"},
		{"gc.auto", "0"},
		{"diff.ignoreSubmodules", "all"},
		{"status.submoduleSummary", "false"},
		{"submodule.recurse", "false"},
	}
	out, err := gitStdoutEnv(ctx, dir, pin, "config", "--show-scope", "--get-regexp",
		`^(filter\..*\.(clean|smudge|process)|merge\..*\.driver|core\.fsmonitor|gpg\..*program|gpg\.ssh\.defaultkeycommand|extensions\.partialclone|remote\..*\.(promisor|partialclonefilter))$`)
	if err != nil {
		return pairs
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		scope, entry, _ := strings.Cut(line, "\t")
		switch scope {
		case "global", "system", "command":
			continue
		}
		key, _, _ := strings.Cut(entry, " ")
		switch {
		case key == "extensions.partialclone" || strings.HasPrefix(key, "remote."):
			// Promisor remotes could start a transport during hrn's git commands.
			if !seen["promisor"] {
				seen["promisor"] = true
				pairs = append(pairs, [2]string{"protocol.allow", "never"})
			}
			continue
		case key == "core.fsmonitor":
			if !seen[key] {
				seen[key] = true
				pairs = append(pairs, [2]string{"core.fsmonitor", "false"})
			}
			continue
		case strings.HasPrefix(key, "gpg."):
			if !seen["gpg."] {
				seen["gpg."] = true
				pairs = append(pairs, [2]string{"commit.gpgSign", "false"}, [2]string{"merge.verifySignatures", "false"})
			}
			continue
		}
		if rest, ok := strings.CutPrefix(key, "merge."); ok {
			// Fail closed on merge drivers; conflict cleanup will undo the merge.
			if name, ok := strings.CutSuffix(rest, ".driver"); ok && name != "" && !seen["merge:"+name] {
				seen["merge:"+name] = true
				pairs = append(pairs, [2]string{"merge." + name + ".driver", "false"})
			}
			continue
		}
		rest, ok := strings.CutPrefix(key, "filter.")
		if !ok {
			continue
		}
		dot := strings.LastIndexByte(rest, '.')
		if dot <= 0 {
			continue
		}
		switch rest[dot+1:] {
		case "clean", "smudge", "process":
		default:
			continue
		}
		// Prefix filter keys to avoid collisions with guard markers.
		name := rest[:dot]
		if !seen["filter:"+name] {
			seen["filter:"+name] = true
			pairs = append(pairs,
				[2]string{"filter." + name + ".clean", ""},
				[2]string{"filter." + name + ".smudge", ""},
				[2]string{"filter." + name + ".process", ""},
				[2]string{"filter." + name + ".required", "false"},
			)
		}
	}
	return pairs
}

func hasChanges(wt string) (bool, error) {
	if _, err := os.Stat(wt); err != nil {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	// Preserve untracked work on removal; startup must allow .hrn/worktrees.
	lines, err := dirtyPaths(ctx, wt, true)
	return len(lines) > 0, err
}

func dirtyPaths(ctx context.Context, dir string, untracked bool) ([]string, error) {
	g, err := newAgentGit(ctx, dir)
	if err != nil {
		return nil, err
	}
	defer g.close()
	args := []string{"status", "--porcelain", "--ignore-submodules=all"}
	if !untracked {
		args = append(args, "--untracked-files=no")
	}
	status, err := g.out(ctx, args...)
	if err != nil || status == "" {
		return nil, err
	}
	return strings.Split(status, "\n"), nil
}

// Diff compares a task's commit or uncommitted fresh work with the base.
// Untracked paths are separate because a diff cannot show them.
func Diff(ctx context.Context, res Result, taskID string) (diff string, untracked []string, err error) {
	tr, err := res.Find(taskID)
	if err != nil {
		return "", nil, err
	}
	if tr.Commit != "" {
		// Branch removal permits commit collection; explain "bad object".
		if _, err := gitOut(ctx, res.Dir, "cat-file", "-e", "--", tr.Commit+"^{commit}"); err != nil {
			return "", nil, fmt.Errorf("task %s: commit %s is no longer in %s, so its branch was removed",
				taskID, tools.ShortSHA(tr.Commit), res.Dir)
		}
		out, err := gitStdout(ctx, res.Dir, "diff", "--no-ext-diff", "--no-textconv", "--ignore-submodules=all", "--end-of-options", res.Base+".."+tr.Commit, "--")
		return out, nil, err
	}
	if tr.Slot != 0 {
		// Released slots may now contain another task's work.
		return "", nil, nil
	}
	if _, err := os.Stat(tr.Worktree); err != nil {
		return "", nil, fmt.Errorf("task %s committed nothing and its worktree is gone", taskID)
	}
	g, err := newAgentGit(ctx, tr.Worktree)
	if err != nil {
		return "", nil, err
	}
	defer g.close()
	against := "HEAD"
	if _, err := g.stdout(ctx, "rev-parse", "-q", "--verify", "HEAD^{commit}"); err != nil {
		// An orphan checkout leaves HEAD unborn; its files still compare with the base.
		against = res.Base
	}
	out, err := g.stdout(ctx, "diff", "--no-ext-diff", "--no-textconv", "--ignore-submodules=all", "--end-of-options", against, "--")
	if err != nil {
		return "", nil, err
	}
	lines, err := dirtyPaths(ctx, tr.Worktree, true)
	for _, l := range lines {
		if rest, ok := strings.CutPrefix(l, "?? "); ok {
			untracked = append(untracked, rest)
		}
	}
	return out, untracked, err
}

// CheckoutTree exports a commit with guards against agent-written shared config.
func CheckoutTree(ctx context.Context, repo, commit, dir string) error {
	g, err := newAgentGit(ctx, repo)
	if err != nil {
		return err
	}
	defer g.close()
	scratch, err := os.MkdirTemp("", "hrn-index-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	env := append(g.env(ctx), "GIT_INDEX_FILE="+filepath.Join(scratch, "index"), "GIT_WORK_TREE="+dir)
	// Block fsmonitor and aliases like GIT~1 that could overwrite .git.
	guard := []string{"-c", "core.fsmonitor=false", "-c", "core.protectNTFS=true", "-c", "core.protectHFS=true"}
	if _, err := gitOutEnv(ctx, g.dir, env, "", append(slices.Clone(guard), "read-tree", "--", commit+"^{tree}")...); err != nil {
		return err
	}
	_, err = gitOutEnv(ctx, g.dir, env, "", append(guard, "checkout-index", "-a")...)
	return err
}

// Head returns the repository's current commit before a batch merge starts.
func Head(ctx context.Context, dir string) (string, error) {
	return gitOut(ctx, dir, "rev-parse", "HEAD")
}

func gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	return gitOutEnv(ctx, dir, nil, "", args...)
}

func gitOutEnv(ctx context.Context, dir string, env []string, stdin string, args ...string) (string, error) {
	out, err := gitretry.Output(ctx, func() ([]byte, error) {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
		nowindow.Set(cmd)
		cmd.Env = append(append(gitenv.WithoutRepoVars(cmd.Environ()), env...), "LC_ALL=C", "LANGUAGE=")
		cmd.Stdin = strings.NewReader(stdin)
		return cmd.CombinedOutput()
	})
	text := strings.TrimSpace(string(out))
	if err != nil {
		if text == "" {
			text = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), text)
	}
	return text, nil
}

// Exclude stderr to avoid corrupting patches.
func gitStdout(ctx context.Context, dir string, args ...string) (string, error) {
	return gitStdoutEnv(ctx, dir, nil, args...)
}

func gitStdoutEnv(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	return gitStdoutIn(ctx, dir, env, "", args...)
}

func gitStdoutIn(ctx context.Context, dir string, env []string, stdin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	nowindow.Set(cmd)
	cmd.Env = append(gitenv.WithoutRepoVars(cmd.Environ()), env...)
	cmd.Stdin = strings.NewReader(stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		text := strings.TrimSpace(stderr.String())
		if text == "" {
			text = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), text)
	}
	return string(out), nil
}
