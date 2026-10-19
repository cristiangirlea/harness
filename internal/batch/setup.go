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

// DefaultSetupPaths selects agent instructions and configuration as git pathspecs.
var DefaultSetupPaths = []string{":(glob)**/AGENTS.md", ":(glob)**/CLAUDE.md", ":(glob)**/HRN.md", ".claude"}

// Setup records a setup commit, its source and changed paths.
type Setup struct {
	Commit, Source string
	Changed        []string
}

// SetupCommit commits on head, in a scratch index, the matching files as they are at head and source's merge base.
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
	// Environment pathspec modes must not reinterpret the supplied patterns.
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
			// Zero mode removes absent files; folders' files have separate entries.
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
