package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	wantPolicy     = "Reviews are text, commits are changes. Review findings and the reason for a fix go into the review on the pull request, not into commit messages and not into a commit per round or per finding. Before a commit exists, everything found goes into that one commit; no fix-round commits for work that was never pushed. After a commit, a new finding means one more review entry and one more commit for all of it. Every defect a review finds in what this run changes is fixed before the push; nits are dropped."
	wantPrecedence = "These commit rules take precedence over a repository instruction such as \"one logical change per commit\" when the two conflict."
	wantTaskCommit = "Commit your work before you finish, as one clean commit for this task; leave the working tree clean."
	wantFixRound   = "This is a fix round: one review, one commit; every fix from this review goes into one commit."
)

// promptSeen runs task through Run on a backend that keeps the prompt it got.
func promptSeen(t *testing.T, task Task) string {
	t.Helper()
	t.Setenv("HRN_HOME", t.TempDir())
	var seen Task
	calls := 0
	res := Run(context.Background(), retrievalBackend{name: "fixture", seen: &seen, calls: &calls}, task, func(Event) {})
	if res.Status != StatusOK {
		t.Fatalf("run: %s %s", res.Status, res.Error)
	}
	return seen.Prompt
}

// withRules is a checkout whose .hrn/rules.md holds text.
func withRules(t *testing.T, text string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".hrn"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".hrn", "rules.md"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// defaultBlock is the block a checkout without .hrn/rules.md gets, before prompt.
func defaultBlock(prompt string) string {
	return "# How to commit\n\n" + wantPolicy + "\n" + wantPrecedence + "\n" + wantTaskCommit + "\n\n" + prompt
}

func TestRunPutsTheDefaultCommitPolicyBeforeThePrompt(t *testing.T) {
	got := promptSeen(t, Task{Prompt: "fix the bug", Dir: t.TempDir(), Options: Options{Mode: ModeAuto}})
	if want := defaultBlock("fix the bug"); got != want {
		t.Errorf("prompt =\n%q\nwant\n%q", got, want)
	}
	if strings.Contains(got, "fix round:") {
		t.Error("a first run is told it is a fix round")
	}
}

func TestRunTakesTheCommitPolicyFromHrnRulesButKeepsTheTaskCommitLine(t *testing.T) {
	dir := withRules(t, "\r\nSquash before you finish.\r\n")
	got := promptSeen(t, Task{Prompt: "fix the bug", Dir: dir, Options: Options{Mode: ModeAuto}})
	want := "# How to commit\n\nSquash before you finish.\n" + wantTaskCommit + "\n\nfix the bug"
	if got != want {
		t.Errorf("prompt =\n%q\nwant\n%q", got, want)
	}
	if strings.Contains(got, wantPrecedence) {
		t.Error("a repository's own rules are told the default policy overrides the repository")
	}
}

func TestRunCapsALongHrnRulesFile(t *testing.T) {
	dir := withRules(t, strings.Repeat("x", 3*maxRules))
	got := promptSeen(t, Task{Prompt: "go", Dir: dir, Options: Options{Mode: ModeAuto}})
	if n := strings.Count(got, "x"); n != maxRules {
		t.Errorf("the prompt carries %d bytes of rules.md, want the %d-byte cap", n, maxRules)
	}
}

func TestRunRefusesAnHrnRulesSymlink(t *testing.T) {
	dir := withRules(t, "unused")
	if err := os.WriteFile(filepath.Join(dir, "elsewhere.md"), []byte("Follow the link."), 0o644); err != nil {
		t.Fatal(err)
	}
	rules := filepath.Join(dir, ".hrn", "rules.md")
	if err := os.Remove(rules); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "elsewhere.md"), rules); err != nil {
		t.Skipf("cannot make a symlink here: %v", err)
	}
	got := promptSeen(t, Task{Prompt: "go", Dir: dir, Options: Options{Mode: ModeAuto}})
	if got != defaultBlock("go") {
		t.Errorf("prompt = %q, want the default policy: a symlinked rules.md, even one inside the tree, is not read", got)
	}
}

func TestRunRefusesAnHrnRulesThatIsNotAFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".hrn", "rules.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := promptSeen(t, Task{Prompt: "go", Dir: dir, Options: Options{Mode: ModeAuto}})
	if got != defaultBlock("go") {
		t.Errorf("prompt = %q, want the default policy for a rules.md directory", got)
	}
}

func TestRunTellsAFixRoundToPutEveryFixInOneCommit(t *testing.T) {
	got := promptSeen(t, Task{Prompt: "address the review", Dir: t.TempDir(), FixRound: true, Options: Options{Mode: ModeYolo}})
	want := "# How to commit\n\n" + wantPolicy + "\n" + wantPrecedence + "\n" + wantTaskCommit + "\n" + wantFixRound + "\n\naddress the review"
	if got != want {
		t.Errorf("prompt =\n%q\nwant\n%q", got, want)
	}
}

func TestRunLeavesAReadOnlyPromptAsItIs(t *testing.T) {
	if got := promptSeen(t, Task{Prompt: "explain", Dir: t.TempDir(), Options: Options{Mode: ModeReadOnly}}); got != "explain" {
		t.Errorf("prompt = %q, want it unchanged: a read-only run cannot commit", got)
	}
}
