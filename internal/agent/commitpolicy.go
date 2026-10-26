package agent

import (
	"io"
	"os"
	"strings"

	"github.com/cristiangirlea/harness/internal/tools"
)

const defaultCommitPolicy = "Reviews are text, commits are changes. Review findings and the reason for a fix go into the review on the pull request, not into commit messages and not into a commit per round or per finding. Before a commit exists, everything found goes into that one commit; no fix-round commits for work that was never pushed. After a commit, a new finding means one more review entry and one more commit for all of it. Every defect a review finds in what this run changes is fixed before the push; nits are dropped."

const (
	precedenceLine = "These commit rules take precedence over a repository instruction such as \"one logical change per commit\" when the two conflict."
	taskCommitLine = "Commit your work before you finish, as one clean commit for this task; leave the working tree clean."
	fixRoundLine   = "This is a fix round: one review, one commit; every fix from this review goes into one commit."
	rulesFile      = ".hrn/rules.md"
	maxRules       = 4 << 10
)

func commitBlock(task Task) string {
	if task.Mode == ModeReadOnly {
		return ""
	}
	policy := defaultCommitPolicy + "\n" + precedenceLine
	if rules := readRules(task.Dir); rules != "" {
		policy = rules
	}
	b := "# How to commit\n\n" + policy + "\n" + taskCommitLine + "\n"
	if task.FixRound {
		b += fixRoundLine + "\n"
	}
	return b + "\n"
}

// Reject links and special files to prevent escapes and blocking opens.
func readRules(dir string) string {
	ws, err := tools.NewWorkspace(dir)
	if err != nil {
		return ""
	}
	path, err := ws.Resolve(rulesFile)
	if err != nil {
		return ""
	}
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	// The file opened must be the one checked, not a link swapped in between.
	if after, err := f.Stat(); err != nil || !os.SameFile(before, after) {
		return ""
	}
	data, err := io.ReadAll(io.LimitReader(f, maxRules))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.ReplaceAll(trimToRune(string(data)), "\r\n", "\n"))
}
