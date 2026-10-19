package batch

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// setupRepo is initRepo with a base commit holding setup files and code, and a
// head commit that changes both.
func setupRepo(t *testing.T) (dir, base, head string) {
	t.Helper()
	dir = initRepo(t)
	write := func(name, body string) {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("AGENTS.md", "old rules\n")
	write("sub/AGENTS.md", "old sub rules\n")
	write(".claude/rules/go.md", "old go rule\n")
	write("code.go", "package x // old\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "base")
	base = git(t, dir, "rev-parse", "HEAD")
	write("AGENTS.md", "new rules\n")
	write("sub/AGENTS.md", "new sub rules\n")
	write(".claude/skills/check/SKILL.md", "new skill\n")
	if err := os.Remove(filepath.Join(dir, ".claude", "rules", "go.md")); err != nil {
		t.Fatal(err)
	}
	write("code.go", "package x // new\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "head")
	head = git(t, dir, "rev-parse", "HEAD")
	return dir, base, head
}

func TestASetupCommitIsHeadWithOnlyTheSetupFilesTakenFromTheSource(t *testing.T) {
	dir, base, head := setupRepo(t)
	setup, err := SetupCommit(context.Background(), dir, head, base, DefaultSetupPaths)
	if err != nil {
		t.Fatal(err)
	}
	commit, changed := setup.Commit, setup.Changed
	if setup.Source != base {
		t.Errorf("source = %s, want the merge base %s", setup.Source, base)
	}
	want := []string{".claude/rules/go.md", ".claude/skills/check/SKILL.md", "AGENTS.md", "sub/AGENTS.md"}
	if !slices.Equal(changed, want) {
		t.Errorf("changed = %q, want %q", changed, want)
	}
	if parent := git(t, dir, "rev-parse", commit+"^"); parent != head {
		t.Errorf("parent = %s, want head %s", parent, head)
	}
	for name, body := range map[string]string{"AGENTS.md": "old rules", "sub/AGENTS.md": "old sub rules", ".claude/rules/go.md": "old go rule", "code.go": "package x // new"} {
		if got := git(t, dir, "show", commit+":"+name); got != body {
			t.Errorf("%s = %q, want %q", name, got, body)
		}
	}
	files := strings.Fields(git(t, dir, "ls-tree", "-r", "--name-only", commit))
	if slices.Contains(files, ".claude/skills/check/SKILL.md") {
		t.Errorf("the skill head added is still in the setup commit: %q", files)
	}
	if status := git(t, dir, "status", "--porcelain"); status != "" || git(t, dir, "rev-parse", "HEAD") != head {
		t.Errorf("the checkout moved or changed: HEAD %s, status %q", git(t, dir, "rev-parse", "HEAD"), status)
	}
}

func TestASetupCommitIsRefusedWhenTheSetupFilesAreTheSame(t *testing.T) {
	dir, _, head := setupRepo(t)
	if _, err := SetupCommit(context.Background(), dir, head, head, DefaultSetupPaths); err == nil || !strings.Contains(err.Error(), "same") {
		t.Errorf("err = %v, want a refusal naming the same setup", err)
	}
}

func TestABatchBranchesFromItsGivenBase(t *testing.T) {
	dir, base, _ := setupRepo(t)
	res, _ := run(t, Spec{Dir: dir, Backend: "wb-ok", Base: base, Tasks: []Task{{ID: "t", Prompt: "x"}}})
	if res.Base != base {
		t.Fatalf("base = %s, want %s", res.Base, base)
	}
	if got := git(t, dir, "show", res.Tasks[0].Commit+":code.go"); got != "package x // old" {
		t.Errorf("the task's branch holds %q, want the base's code", got)
	}
}

type blob struct{ mode, body string }

// plumbCommit commits files as given on top of parent (or as a root), through a
// scratch index, so names and modes the file system cannot hold are possible.
func plumbCommit(t *testing.T, dir, parent string, files map[string]blob) string {
	t.Helper()
	index := filepath.Join(t.TempDir(), "index")
	gitIO := func(stdin string, args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+index)
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if parent != "" {
		gitIO("", "read-tree", parent)
	}
	var lines strings.Builder
	for name, b := range files {
		if b.mode == "" {
			fmt.Fprintf(&lines, "0 %s\t%s\x00", strings.Repeat("0", len(parent)), name)
			continue
		}
		sha := gitIO(b.body, "hash-object", "-w", "--stdin")
		fmt.Fprintf(&lines, "%s %s\t%s\x00", b.mode, sha, name)
	}
	gitIO(lines.String(), "update-index", "--add", "-z", "--index-info")
	args := []string{"commit-tree", gitIO("", "write-tree"), "-m", "plumbed"}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	return gitIO("", args...)
}

func TestASetupCommitKeepsModesAndOddNamesAndSwapsFilesAndFolders(t *testing.T) {
	dir := initRepo(t)
	root := git(t, dir, "rev-parse", "HEAD")
	base := plumbCommit(t, dir, root, map[string]blob{
		".claude/hooks/run.sh":             {"100755", "#!/bin/sh\n"},
		".claude/link":                     {"120000", "hooks/run.sh"},
		".claude/odd name\twith\nlines.md": {"100644", "odd\n"},
		".claude/rules":                    {"100644", "a file at the base\n"},
		".claude/skills/a/SKILL.md":        {"100644", "skill at the base\n"},
	})
	head := plumbCommit(t, dir, base, map[string]blob{
		".claude/hooks/run.sh":             {"100644", "changed\n"},
		".claude/link":                     {"100644", "no longer a link\n"},
		".claude/odd name\twith\nlines.md": {"100644", "changed\n"},
		".claude/rules":                    {},
		".claude/rules/go.md":              {"100644", "a folder at head\n"},
		".claude/skills/a/SKILL.md":        {},
		".claude/skills":                   {"100644", "a file at head\n"},
	})
	setup, err := SetupCommit(context.Background(), dir, head, base, DefaultSetupPaths)
	if err != nil {
		t.Fatal(err)
	}
	got := git(t, dir, "ls-tree", "-r", "--full-tree", setup.Commit, "--", ".claude")
	baseTree := git(t, dir, "ls-tree", "-r", "--full-tree", base, "--", ".claude")
	if got != baseTree {
		t.Errorf("the setup commit's .claude is\n%s\nwant the base's\n%s", got, baseTree)
	}
}

func TestASetupCommitWorksInASHA256Repository(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init", "-q", "--object-format=sha256").CombinedOutput(); err != nil {
		t.Skipf("this git cannot make a sha256 repository: %v %s", err, out)
	}
	git(t, dir, "config", "user.email", "test@example.com")
	git(t, dir, "config", "user.name", "Test")
	base := plumbCommit(t, dir, "", map[string]blob{"AGENTS.md": {"100644", "base\n"}, "CLAUDE.md": {"100644", "base\n"}})
	head := plumbCommit(t, dir, base, map[string]blob{"AGENTS.md": {"100644", "head\n"}, "CLAUDE.md": {}})
	setup, err := SetupCommit(context.Background(), dir, head, base, DefaultSetupPaths)
	if err != nil {
		t.Fatal(err)
	}
	if got := git(t, dir, "show", setup.Commit+":CLAUDE.md"); got != "base" {
		t.Errorf("CLAUDE.md = %q, want the base's back", got)
	}
}

func TestASetupCommitTakesTheFilesWhereTheBranchLeftTheSource(t *testing.T) {
	dir, base, head := setupRepo(t)
	git(t, dir, "checkout", "-q", "-b", "later", base)
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("main moved on\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "commit", "-q", "-am", "main's later setup edit")
	git(t, dir, "checkout", "-q", head)
	setup, err := SetupCommit(context.Background(), dir, head, "later", DefaultSetupPaths)
	if err != nil {
		t.Fatal(err)
	}
	if setup.Source != base || git(t, dir, "show", setup.Commit+":AGENTS.md") != "old rules" {
		t.Errorf("source = %s, AGENTS.md = %q; want the fork point's", setup.Source, git(t, dir, "show", setup.Commit+":AGENTS.md"))
	}
}

func TestASetupCommitIgnoresTheOperatorsGitEnvironment(t *testing.T) {
	dir, base, head := setupRepo(t)
	stray := filepath.Join(t.TempDir(), "stray-index")
	t.Setenv("GIT_INDEX_FILE", stray)
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "elsewhere"))
	t.Setenv("GIT_LITERAL_PATHSPECS", "1")
	setup, err := SetupCommit(context.Background(), dir, head, base, DefaultSetupPaths)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(setup.Changed, "sub/AGENTS.md") {
		t.Errorf("changed = %q; the glob pathspecs must still match", setup.Changed)
	}
	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Errorf("the operator's GIT_INDEX_FILE was written: %v", err)
	}
}

func TestASetupCommitTakesOnlyTheNamedPaths(t *testing.T) {
	dir, base, head := setupRepo(t)
	setup, err := SetupCommit(context.Background(), dir, head, base, []string{"AGENTS.md"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(setup.Changed, []string{"AGENTS.md"}) || git(t, dir, "show", setup.Commit+":sub/AGENTS.md") != "new sub rules" {
		t.Errorf("changed = %q", setup.Changed)
	}
}

func TestAnUnknownSetupSourceOrBaseIsNamedInTheError(t *testing.T) {
	dir, _, head := setupRepo(t)
	if _, err := SetupCommit(context.Background(), dir, head, "origin/nowhere", DefaultSetupPaths); err == nil || !strings.Contains(err.Error(), "origin/nowhere") || !strings.Contains(err.Error(), "--base") {
		t.Errorf("err = %v", err)
	}
	if _, err := Run(context.Background(), Spec{Dir: dir, Backend: "wb-ok", Base: "nowhere", Tasks: []Task{{Prompt: "x"}}}, nil); err == nil || !strings.Contains(err.Error(), "base nowhere") {
		t.Errorf("err = %v", err)
	}
}
