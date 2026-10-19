package batch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// DefaultSetupPaths are the files an agent's setup lives in, as git pathspecs:
// the instruction files anywhere in the tree and everything under .claude.
var DefaultSetupPaths = []string{":(glob)**/AGENTS.md", ":(glob)**/CLAUDE.md", ":(glob)**/HRN.md", ".claude"}

// Setup is a commit SetupCommit made, the source commit its files came from and the paths it changed.
type Setup struct {
	Commit, Source string
	Changed        []string
}

// SetupCommit makes a commit on top of head whose tree is head's except that the
// files paths match are as they were where head left source (their merge base),
// so later setup edits on source do not count as the branch's. Nothing is
// checked out: it is built in a scratch index.
func SetupCommit(ctx context.Context, repo, head, source string, paths []string) (Setup, error) {
	var out Setup
	if len(paths) == 0 {
		return out, errors.New("no setup paths to take from the source")
	}
	g, err := newRepoGit(ctx, repo)
	if err != nil {
		return out, err
	}
	defer g.close()
	// The pathspecs mean what they say whatever the operator's environment asks of git.
	base := append(g.env(ctx), "GIT_LITERAL_PATHSPECS=0", "GIT_GLOB_PATHSPECS=0", "GIT_NOGLOB_PATHSPECS=0", "GIT_ICASE_PATHSPECS=0")
	resolve := func(rev string) (string, error) {
		return gitOutEnv(ctx, g.dir, base, "", "rev-parse", "--verify", "--quiet", rev+"^{commit}")
	}
	if head, err = resolve(head); err != nil {
		return out, fmt.Errorf("head: %w", err)
	}
	tip, err := resolve(source)
	if err != nil {
		return out, fmt.Errorf("setup source %q is not a commit in %s: name another with --base", source, repo)
	}
	src, err := gitOutEnv(ctx, g.dir, base, "", "merge-base", head, tip)
	if err != nil {
		return out, fmt.Errorf("setup source %q shares no history with %s: %w", source, head, err)
	}
	args := append([]string{"diff-tree", "-r", "--name-only", "-z", "--no-renames", head, src, "--"}, paths...)
	list, err := gitStdoutEnv(ctx, g.dir, base, args...)
	if err != nil {
		return out, err
	}
	var changed []string
	for name := range strings.SplitSeq(list, "\x00") {
		if name != "" {
			changed = append(changed, name)
		}
	}
	if len(changed) == 0 {
		return out, fmt.Errorf("the setup files are the same where this branch left %s and at %s: nothing to compare", source, head)
	}
	slices.Sort(changed)
	scratch, err := os.MkdirTemp("", "hrn-setup-index-")
	if err != nil {
		return out, err
	}
	defer os.RemoveAll(scratch)
	env := append(slices.Clone(base), "GIT_INDEX_FILE="+filepath.Join(scratch, "index"))
	if _, err := gitOutEnv(ctx, g.dir, env, "", "read-tree", head+"^{tree}"); err != nil {
		return out, err
	}
	var stdin strings.Builder
	for _, name := range changed {
		entry, err := gitStdoutEnv(ctx, g.dir, env, "ls-tree", "-z", "--full-tree", src, "--", ":(literal)"+name)
		if err != nil {
			return out, err
		}
		entry = strings.TrimSuffix(entry, "\x00")
		meta, path, _ := strings.Cut(entry, "\t")
		mode, rest, _ := strings.Cut(meta, " ")
		kind, sha, _ := strings.Cut(rest, " ")
		if entry == "" || kind == "tree" {
			// Not a file at the source (absent, or a folder whose files come on their
			// own lines): an all-zero mode removes it from the index.
			fmt.Fprintf(&stdin, "0 %s\t%s\x00", strings.Repeat("0", len(src)), name)
			continue
		}
		fmt.Fprintf(&stdin, "%s %s\t%s\x00", mode, sha, path)
	}
	if _, err := gitOutEnv(ctx, g.dir, env, stdin.String(), "update-index", "-z", "--index-info"); err != nil {
		return out, err
	}
	tree, err := gitOutEnv(ctx, g.dir, env, "", "write-tree")
	if err != nil {
		return out, err
	}
	msg := fmt.Sprintf("hrn: setup files from %s (%s) on top of %s", source, src, head)
	commit, err := gitOutEnv(ctx, g.dir, env, "", "commit-tree", tree, "-p", head, "-m", msg)
	if err != nil {
		return out, err
	}
	return Setup{Commit: commit, Source: src, Changed: changed}, nil
}
