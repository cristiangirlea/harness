package batch

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"
)

const (
	// ProtectReport keeps the attempt's verdict and lists the protected files it changed.
	ProtectReport = "report"
	// ProtectFail fails the attempt as a gate would, before the gates run.
	ProtectFail = "fail"
)

// protection is what one task checks after its agent finishes.
type protection struct {
	globs []string
	fail  bool
	base  string
}

func checkProtect(globs []string, mode string) error {
	if mode != "" && mode != ProtectReport && mode != ProtectFail {
		return fmt.Errorf("protect_mode %q: %s or %s", mode, ProtectReport, ProtectFail)
	}
	for _, g := range globs {
		if strings.TrimSpace(g) == "" || path.IsAbs(g) || strings.Contains(g, `\`) || strings.Contains(g, ":") {
			return fmt.Errorf("protect %q: a glob relative to the repository, with / between folders", g)
		}
		for seg := range strings.SplitSeq(g, "/") {
			// git names files without ./, ../, empty parts or a trailing /, so such a glob never matches.
			if seg == "" || seg == "." || seg == ".." {
				return fmt.Errorf("protect %q: a glob relative to the repository, with / between folders", g)
			}
			if _, err := path.Match(seg, ""); err != nil {
				return fmt.Errorf("protect %q: %v", g, err)
			}
		}
	}
	return nil
}

// matchProtect matches a slash path against a glob in which ** stands for any
// number of whole folders and every other part is a path.Match pattern.
func matchProtect(glob, name string) bool {
	return matchSegments(strings.Split(glob, "/"), strings.Split(name, "/"))
}

func matchSegments(glob, name []string) bool {
	for len(glob) > 0 {
		if glob[0] == "**" {
			for i := 0; i <= len(name); i++ {
				if matchSegments(glob[1:], name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		if ok, _ := path.Match(glob[0], name[0]); !ok {
			return false
		}
		glob, name = glob[1:], name[1:]
	}
	return len(name) == 0
}

// changedProtected lists the files matching p's globs that differ from the
// base in dir, committed or not. Added files are left out: a new test is work.
func changedProtected(ctx context.Context, dir string, p protection) ([]string, error) {
	if len(p.globs) == 0 {
		return nil, nil
	}
	// The agent writes this worktree's config: its fsmonitor, filters and core.worktree must not apply.
	g, err := newAgentGit(ctx, dir)
	if err != nil {
		return nil, err
	}
	defer g.close()
	out, err := g.stdout(ctx, "diff", "--name-only", "-z", "--no-renames", "--no-ext-diff",
		"--ignore-submodules=all", "--diff-filter=MDT", p.base, "--")
	if err != nil {
		return nil, err
	}
	var changed []string
	for name := range strings.SplitSeq(out, "\x00") {
		if name == "" {
			continue
		}
		if slices.ContainsFunc(p.globs, func(g string) bool { return matchProtect(g, name) }) {
			changed = append(changed, name)
		}
	}
	slices.Sort(changed)
	return changed, nil
}

// protectGate reports a protect failure as a gate result, so the retry prompt
// names the files the way it names a failing gate command.
func protectGate(p protection, changed []string, err error) GateResult {
	gr := GateResult{Command: "protect " + strings.Join(p.globs, " ")}
	switch {
	case err != nil:
		gr.ExitCode, gr.Output = 1, "could not compare the protected files with the base: "+err.Error()
	case len(changed) > 0:
		gr.ExitCode = 1
		gr.Output = "These files the gates rely on were changed or removed; restore them and do the task another way:\n" +
			strings.Join(changed, "\n")
	}
	return gr
}
