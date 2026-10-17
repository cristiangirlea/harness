package batch

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// An agent can point its worktree's core.worktree elsewhere; hrn's own add and
// commit must still take the task worktree's files and nothing from there.
func TestCommitChangesIgnoresACoreWorktreeOutsideTheTaskWorktree(t *testing.T) {
	dir := initRepo(t)
	wt := filepath.Join(t.TempDir(), "worktree")
	git(t, dir, "worktree", "add", "-q", "-b", "task", wt)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "id_secret"), []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, wt, "config", "extensions.worktreeConfig", "true")
	git(t, wt, "config", "--worktree", "core.worktree", filepath.ToSlash(outside))
	if err := os.WriteFile(filepath.Join(wt, "result.txt"), []byte("done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sha, err := commitChanges(wt, "task result")
	if err != nil || sha == "" {
		t.Fatalf("commitChanges = %q, %v", sha, err)
	}
	files := strings.Fields(git(t, wt, "ls-tree", "-r", "--name-only", "HEAD"))
	if !slices.Equal(files, []string{"README.md", "result.txt"}) {
		t.Fatalf("committed files = %q, want README.md and result.txt only", files)
	}
}

// A process the agent left running can write config after hrn first looked at
// it, so every command looks again.
func TestAgentGitLooksAtTheConfigAgainBeforeEachCommand(t *testing.T) {
	dir := initRepo(t)
	marker := filepath.Join(t.TempDir(), "ran")
	t.Setenv("HRN_TEST_HOOK_MARKER", marker)
	ctx := context.Background()
	g, err := newAgentGit(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer g.close()
	if err := os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte("*.txt filter=drv\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "config", "filter.drv.clean", markerScript(t))
	git(t, dir, "config", "filter.drv.required", "true")
	if err := os.WriteFile(filepath.Join(dir, "result.txt"), []byte("done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := g.out(ctx, "add", "result.txt"); err != nil {
		t.Fatal(err)
	}
	assertNotRan(t, marker, "a filter written after newAgentGit", func() {
		git(t, dir, "rm", "-q", "--cached", "result.txt")
		_ = exec.Command("git", "-C", dir, "add", "result.txt").Run()
	})
}

// Text git writes to stderr is no config entry, even when it looks like one.
func TestAgentGitReadsOnlyTheConfigScansOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows paths cannot hold a tab")
	}
	t.Setenv("HRN_HOME", t.TempDir())
	dir := filepath.Join(t.TempDir(), "x\tfilter.evil.clean prog")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "init", "-q")
	// One matching key, so the scan succeeds and its output is read.
	git(t, dir, "config", "core.fsmonitor", "false")
	t.Setenv("GIT_TRACE_SETUP", "1")
	ctx := context.Background()
	g, err := newAgentGit(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer g.close()
	for _, kv := range g.env(ctx) {
		if strings.HasPrefix(kv, "GIT_CONFIG_KEY_") && strings.Contains(kv, "filter.evil") {
			t.Fatalf("a setup trace line became an override: %q", kv)
		}
	}
}

// writeMainRepoPrograms sets, in the repository config an agent's worktree
// shares, a hooks path and a smudge filter that leave the marker when they run.
func writeMainRepoPrograms(t *testing.T, dir string) {
	t.Helper()
	hooks := t.TempDir()
	for _, name := range []string{"post-merge", "post-checkout", "post-commit"} {
		body := "#!/bin/sh\nprintf ran > \"$HRN_TEST_HOOK_MARKER\"\n"
		if err := os.WriteFile(filepath.Join(hooks, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	git(t, dir, "config", "core.hooksPath", filepath.ToSlash(hooks))
	attrs := filepath.Join(dir, ".git", "info", "attributes")
	if err := os.MkdirAll(filepath.Dir(attrs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(attrs, []byte("*.txt filter=drv\n*.md filter=drv\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "config", "filter.drv.smudge", markerScript(t))
	git(t, dir, "config", "filter.drv.required", "true")
}

func TestMergeDoesNotRunProgramsFromTheSharedRepositoryConfig(t *testing.T) {
	for _, squash := range []bool{false, true} {
		dir := initRepo(t)
		res := mergeable(t, dir, Task{ID: "alpha", Prompt: "alpha"})
		marker := filepath.Join(t.TempDir(), "ran")
		t.Setenv("HRN_TEST_HOOK_MARKER", marker)
		writeMainRepoPrograms(t, dir)
		merged, err := Merge(context.Background(), res, nil, squash)
		if err != nil || len(merged) != 1 || merged[0].Commit == "" {
			t.Fatalf("squash=%v: merged = %+v, %v", squash, merged, err)
		}
		assertNotRan(t, marker, "a hook or filter during batch merge", func() {
			_ = exec.Command("git", "-C", dir, "-c", "filter.drv.required=false", "commit", "-q", "--allow-empty", "-m", "control").Run()
		})
	}
}

func TestAddWorktreeDoesNotRunProgramsFromTheSharedRepositoryConfig(t *testing.T) {
	cases := map[string]func(t *testing.T, dir string){
		"core.hooksPath": func(t *testing.T, dir string) {
			_ = exec.Command("git", "-C", dir, "worktree", "add", "-q", "-b", "control", filepath.Join(t.TempDir(), "c")).Run()
		},
		"filter.smudge": func(t *testing.T, dir string) {
			_ = os.Remove(filepath.Join(dir, "README.md"))
			_ = exec.Command("git", "-C", dir, "checkout", "-f", "HEAD", "--", "README.md").Run()
		},
	}
	for name, control := range cases {
		t.Run(name, func(t *testing.T) {
			dir := initRepo(t)
			marker := filepath.Join(t.TempDir(), "ran")
			t.Setenv("HRN_TEST_HOOK_MARKER", marker)
			writeMainRepoPrograms(t, dir)
			wt := filepath.Join(t.TempDir(), "task")
			if err := addWorktree(context.Background(), dir, TaskResult{Branch: "task", Worktree: wt}, "HEAD"); err != nil {
				t.Fatal(err)
			}
			if data, err := os.ReadFile(filepath.Join(wt, "README.md")); err != nil || strings.TrimSpace(string(data)) != "hello" {
				t.Fatalf("README.md in the new worktree = %q, %v", data, err)
			}
			assertNotRan(t, marker, name+" during git worktree add", func() { control(t, dir) })
		})
	}
}

// A merge driver runs on a real three-way merge, so both sides add alpha.txt.
func TestMergeDoesNotRunAMergeDriverFromTheSharedRepositoryConfig(t *testing.T) {
	dir := initRepo(t)
	res := mergeable(t, dir, Task{ID: "alpha", Prompt: "alpha"})
	if err := os.WriteFile(filepath.Join(dir, "alpha.txt"), []byte("main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "alpha.txt")
	git(t, dir, "commit", "-q", "-m", "main side")
	before := git(t, dir, "rev-parse", "HEAD")
	marker := filepath.Join(t.TempDir(), "ran")
	t.Setenv("HRN_TEST_HOOK_MARKER", marker)
	if err := os.WriteFile(filepath.Join(dir, ".git", "info", "attributes"), []byte("* merge=evil\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "config", "merge.evil.driver", markerScript(t)+" %A %O %B")
	merged, err := Merge(context.Background(), res, nil, false)
	if err == nil || len(merged) != 1 || !merged[0].Conflict {
		t.Fatalf("merged = %+v, %v; want a conflict", merged, err)
	}
	if head := git(t, dir, "rev-parse", "HEAD"); head != before {
		t.Fatalf("HEAD moved to %s", head)
	}
	if status := git(t, dir, "status", "--porcelain"); status != "" {
		t.Fatalf("the tree must be clean after a refused merge:\n%s", status)
	}
	assertNotRan(t, marker, "a merge driver during batch merge", func() {
		_ = exec.Command("git", "-C", dir, "merge", "--no-ff", "--no-edit", res.Tasks[0].Branch).Run()
	})
}

// The squash path reads the staged names and a conflict the unmerged ones;
// both read the index, which asks the fsmonitor.
func TestMergeDoesNotRunAnFsmonitorFromTheSharedRepositoryConfig(t *testing.T) {
	cases := map[string]func(t *testing.T, dir string) ([]MergeResult, error){
		"squash": func(t *testing.T, dir string) ([]MergeResult, error) {
			res := mergeable(t, dir, Task{ID: "alpha", Prompt: "alpha"})
			git(t, dir, "config", "core.fsmonitor", markerScript(t))
			return Merge(context.Background(), res, nil, true)
		},
		"conflict": func(t *testing.T, dir string) ([]MergeResult, error) {
			res, _ := run(t, Spec{Dir: dir, Backend: "wb-ok", Tasks: []Task{
				{ID: "one", Prompt: "first"}, {ID: "two", Prompt: "second"}}})
			git(t, dir, "config", "core.fsmonitor", markerScript(t))
			merged, err := Merge(context.Background(), res, nil, false)
			if err == nil || len(merged) != 2 || !merged[1].Conflict {
				t.Fatalf("merged = %+v, %v; want the second to conflict", merged, err)
			}
			return merged, nil
		},
	}
	for name, merge := range cases {
		t.Run(name, func(t *testing.T) {
			dir := initRepo(t)
			marker := filepath.Join(t.TempDir(), "ran")
			t.Setenv("HRN_TEST_HOOK_MARKER", marker)
			if _, err := merge(t, dir); err != nil {
				t.Fatal(err)
			}
			assertNotRan(t, marker, "core.fsmonitor during batch merge", func() {
				_ = exec.Command("git", "-C", dir, "ls-files", "--unmerged").Run()
			})
		})
	}
}

// A hooks path from the operator's global config still runs in batch merge;
// one in the shared repository config does not.
func TestMergeKeepsTheOperatorsGlobalHooksPath(t *testing.T) {
	dir := initRepo(t)
	res := mergeable(t, dir, Task{ID: "alpha", Prompt: "alpha"})
	hooksDir := func(markerEnv string) string {
		hooks := t.TempDir()
		body := "#!/bin/sh\nprintf ran > \"$" + markerEnv + "\"\n"
		if err := os.WriteFile(filepath.Join(hooks, "post-merge"), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		return filepath.ToSlash(hooks)
	}
	operator := filepath.Join(t.TempDir(), "operator")
	agentRan := filepath.Join(t.TempDir(), "agent")
	t.Setenv("HRN_TEST_OPERATOR_MARKER", operator)
	t.Setenv("HRN_TEST_HOOK_MARKER", agentRan)
	global := filepath.Join(t.TempDir(), "gitconfig")
	git(t, dir, "config", "--file", global, "core.hooksPath", hooksDir("HRN_TEST_OPERATOR_MARKER"))
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	if _, err := Merge(context.Background(), res, nil, false); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(operator); err != nil || string(data) != "ran" {
		t.Fatalf("the operator's global post-merge hook did not run: %q, %v", data, err)
	}

	dir = initRepo(t)
	res = mergeable(t, dir, Task{ID: "alpha", Prompt: "alpha"})
	git(t, dir, "config", "core.hooksPath", hooksDir("HRN_TEST_HOOK_MARKER"))
	if _, err := Merge(context.Background(), res, nil, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(agentRan); !os.IsNotExist(err) {
		t.Fatalf("a hooks path in the shared config ran over the operator's global one: %v", err)
	}
}

// Without a promisor remote hrn leaves transports alone, so an operator's
// hook that fetches from a local repository still works during batch merge.
func TestMergeLetsTheOperatorsHookFetchWithoutAPromisorRemote(t *testing.T) {
	dir := initRepo(t)
	res := mergeable(t, dir, Task{ID: "alpha", Prompt: "alpha"})
	hooks := t.TempDir()
	body := "#!/bin/sh\ngit fetch -q \"$HRN_TEST_FETCH_FROM\" HEAD && printf ran > \"$HRN_TEST_OPERATOR_MARKER\"\n"
	if err := os.WriteFile(filepath.Join(hooks, "post-merge"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	operator := filepath.Join(t.TempDir(), "operator")
	t.Setenv("HRN_TEST_OPERATOR_MARKER", operator)
	t.Setenv("HRN_TEST_FETCH_FROM", filepath.ToSlash(initRepo(t)))
	global := filepath.Join(t.TempDir(), "gitconfig")
	git(t, dir, "config", "--file", global, "core.hooksPath", filepath.ToSlash(hooks))
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	if _, err := Merge(context.Background(), res, nil, false); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(operator); err != nil || string(data) != "ran" {
		t.Fatalf("the operator's fetching post-merge hook did not finish: %q, %v", data, err)
	}
}

// The next batch's root comes from the .git entry, not a core.worktree the
// shared config names.
func TestRepoRootIgnoresACoreWorktreeInTheSharedConfig(t *testing.T) {
	dir := initRepo(t)
	other := t.TempDir()
	git(t, other, "init", "-q")
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "config", "core.worktree", filepath.ToSlash(other))
	if plain := git(t, sub, "rev-parse", "--show-toplevel"); canon(t, plain) != canon(t, other) {
		t.Fatalf("setup: plain rev-parse --show-toplevel = %s, want the redirected tree", plain)
	}
	for _, from := range []string{dir, sub} {
		root, err := repoRoot(context.Background(), from)
		if err != nil || canon(t, root) != canon(t, dir) {
			t.Fatalf("repoRoot(%s) = %q, %v; want %s", from, root, err, dir)
		}
	}
}

// signTip moves the task's branch to a copy of its tip that carries a gpgsig
// header, which is all git needs to call gpg.program to verify it.
func signTip(t *testing.T, dir, branch string) {
	t.Helper()
	body := git(t, dir, "cat-file", "commit", branch)
	head, msg, _ := strings.Cut(body, "\n\n")
	sig := "gpgsig -----BEGIN PGP SIGNATURE-----\n \n iQEzBAABCAAdFiEE\n -----END PGP SIGNATURE-----"
	cmd := exec.Command("git", "-C", dir, "hash-object", "-t", "commit", "-w", "--stdin")
	cmd.Stdin = strings.NewReader(head + "\n" + sig + "\n\n" + msg + "\n")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("hash-object: %v", err)
	}
	git(t, dir, "update-ref", "refs/heads/"+branch, strings.TrimSpace(string(out)))
}

func TestMergeDoesNotVerifySignaturesWithAProgramFromTheSharedRepositoryConfig(t *testing.T) {
	for _, squash := range []bool{false, true} {
		dir := initRepo(t)
		res := mergeable(t, dir, Task{ID: "alpha", Prompt: "alpha"})
		signTip(t, dir, res.Tasks[0].Branch)
		marker := filepath.Join(t.TempDir(), "ran")
		t.Setenv("HRN_TEST_HOOK_MARKER", marker)
		git(t, dir, "config", "merge.verifySignatures", "true")
		git(t, dir, "config", "gpg.program", markerScript(t))
		if _, err := Merge(context.Background(), res, nil, squash); err != nil {
			t.Fatalf("squash=%v: %v", squash, err)
		}
		git(t, dir, "reset", "-q", "--hard", "HEAD~1")
		assertNotRan(t, marker, "gpg.program during batch merge", func() {
			_ = exec.Command("git", "-C", dir, "merge", "--no-ff", "--no-edit", res.Tasks[0].Branch).Run()
		})
	}
}

// A promisor remote makes git fetch a missing object on demand, running the
// remote's upload-pack program.
func TestMergeDoesNotLazyFetchFromAPromisorInTheSharedRepositoryConfig(t *testing.T) {
	dir := initRepo(t)
	res := mergeable(t, dir, Task{ID: "alpha", Prompt: "alpha"})
	branch := res.Tasks[0].Branch
	hash := exec.Command("git", "-C", dir, "hash-object", "--stdin")
	hash.Stdin = strings.NewReader("never stored\n")
	blob, err := hash.Output()
	if err != nil {
		t.Fatal(err)
	}
	mktree := exec.Command("git", "-C", dir, "mktree", "--missing")
	mktree.Stdin = strings.NewReader("100644 blob " + strings.TrimSpace(string(blob)) + "\tmissing.txt\n")
	tree, err := mktree.Output()
	if err != nil {
		t.Fatal(err)
	}
	commit := git(t, dir, "commit-tree", strings.TrimSpace(string(tree)), "-p", branch, "-m", "missing blob")
	git(t, dir, "update-ref", "refs/heads/"+branch, commit)

	marker := filepath.Join(t.TempDir(), "ran")
	t.Setenv("HRN_TEST_HOOK_MARKER", marker)
	// upload-pack speaks first, so a script that waits on stdin would hang the fetch.
	script := filepath.Join(t.TempDir(), "upload-pack.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf ran > \"$HRN_TEST_HOOK_MARKER\"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "config", "core.repositoryFormatVersion", "1")
	git(t, dir, "config", "extensions.partialClone", "origin")
	git(t, dir, "config", "remote.origin.url", filepath.ToSlash(t.TempDir()))
	git(t, dir, "config", "remote.origin.promisor", "true")
	git(t, dir, "config", "remote.origin.uploadpack", filepath.ToSlash(script))
	_, _ = Merge(context.Background(), res, nil, false)
	if status := git(t, dir, "status", "--porcelain"); status != "" {
		git(t, dir, "reset", "-q", "--hard", "HEAD")
	}
	assertNotRan(t, marker, "a promisor's upload-pack during batch merge", func() {
		_ = exec.Command("git", "-C", dir, "merge", "--no-ff", "--no-edit", branch).Run()
	})
}

// A hooks path from the operator's global config still runs in git worktree add.
func TestAddWorktreeKeepsTheOperatorsGlobalHooksPath(t *testing.T) {
	dir := initRepo(t)
	hooks := t.TempDir()
	body := "#!/bin/sh\nprintf ran > \"$HRN_TEST_OPERATOR_MARKER\"\n"
	if err := os.WriteFile(filepath.Join(hooks, "post-checkout"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	operator := filepath.Join(t.TempDir(), "operator")
	t.Setenv("HRN_TEST_OPERATOR_MARKER", operator)
	global := filepath.Join(t.TempDir(), "gitconfig")
	git(t, dir, "config", "--file", global, "core.hooksPath", filepath.ToSlash(hooks))
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	wt := filepath.Join(t.TempDir(), "task")
	if err := addWorktree(context.Background(), dir, TaskResult{Branch: "task", Worktree: wt}, "HEAD"); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(operator); err != nil || string(data) != "ran" {
		t.Fatalf("the operator's global post-checkout hook did not run: %q, %v", data, err)
	}
}

func TestOnlyAPromisorInTheRepositoryConfigBlocksTransports(t *testing.T) {
	global := filepath.Join(t.TempDir(), "gitconfig")
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	blocked := func(dir string) bool {
		return slices.Contains(agentConfigOverrides(context.Background(), dir, nil, t.TempDir()), [2]string{"protocol.allow", "never"})
	}
	for _, kv := range [][2]string{{"extensions.partialClone", "evil"}, {"remote.Evil.promisor", "true"}, {"remote.Evil.partialCloneFilter", "blob:none"}} {
		dir := initRepo(t)
		git(t, dir, "config", kv[0], kv[1])
		if !blocked(dir) {
			t.Errorf("%s in the repository config left transports open", kv[0])
		}
	}
	dir := initRepo(t)
	if blocked(dir) {
		t.Error("transports blocked without a promisor remote")
	}
	git(t, dir, "config", "--file", global, "remote.origin.promisor", "true")
	if blocked(dir) {
		t.Error("a promisor in the operator's global config blocked transports")
	}
}
