package workmap

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/gitenv"
	"github.com/cristiangirlea/harness/internal/prefs"
	"github.com/cristiangirlea/harness/internal/runlog"
)

type Stage string

const (
	Idea     Stage = "idea"
	Branch   Stage = "branch"
	Agent    Stage = "agent"
	Gates    Stage = "gates"
	Review   Stage = "review"
	PR       Stage = "pr"
	CI       Stage = "ci"
	Merged   Stage = "merged"
	Released Stage = "released"
)

var stages = []Stage{Idea, Branch, Agent, Gates, Review, PR, CI, Merged, Released}

type Links struct {
	Runs     []string
	Batches  []string
	Branch   string
	Worktree string
}
type Piece struct {
	Repo, Key, Title string
	Stage            Stage
	Stuck            string
	Links            Links
	Updated          time.Time
}

func rank(s Stage) int {
	for i, v := range stages {
		if v == s {
			return i
		}
	}
	return -1
}
func git(ctx context.Context, dir string, args ...string) (string, error) {
	a := append([]string{"-C", dir, "-c", "core.fsmonitor=false", "-c", "core.hooksPath=" + os.DevNull}, args...)
	c := exec.CommandContext(ctx, "git", a...)
	c.Env = append(gitenv.WithoutRepoVars(os.Environ()), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0", "GIT_NO_LAZY_FETCH=1", "LC_ALL=C")
	c.WaitDelay = time.Second
	out, err := c.Output()
	return strings.TrimSpace(string(out)), err
}
func canonical(path string) string {
	p, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	if v, err := filepath.EvalSymlinks(p); err == nil {
		p = v
	}
	return filepath.Clean(p)
}
func entries(path string) ([]os.DirEntry, error) {
	e, err := os.ReadDir(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return e, err
}

// Build collects local work, returning partial results and joined errors when sources fail.
func Build(ctx context.Context, home string, repos []string) ([]Piece, error) {
	var errs []error
	runs := []runlog.Record{}
	batches := []batch.Result{}
	es, err := entries(filepath.Join(home, "runs"))
	if err != nil {
		errs = append(errs, err)
	}
	for _, e := range es {
		if !e.IsDir() {
			continue
		}
		r, err := runlog.LoadFrom(home, e.Name())
		if err != nil {
			errs = append(errs, fmt.Errorf("run %s: %w", e.Name(), err))
			continue
		}
		runs = append(runs, r)
	}
	es, err = entries(filepath.Join(home, "batches"))
	if err != nil {
		errs = append(errs, err)
	}
	for _, e := range es {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || e.IsDir() {
			continue
		}
		b, err := batch.LoadResultFrom(home, id)
		if err != nil {
			errs = append(errs, fmt.Errorf("batch %s: %w", id, err))
			continue
		}
		batches = append(batches, b)
	}
	if repos == nil {
		p, err := prefs.Load(home)
		if err != nil {
			errs = append(errs, err)
		}
		repos = p.WorkMapRepos
		if repos == nil {
			for _, r := range runs {
				if r.Task.Dir != "" {
					repos = append(repos, r.Task.Dir)
				}
			}
			for _, b := range batches {
				if b.Dir != "" {
					repos = append(repos, b.Dir)
				}
			}
		}
	}
	pieces := map[string]*Piece{}
	roots := map[string]string{}
	worktrees := map[string]map[string]string{}
	add := func(p Piece) {
		key := p.Repo + "\x00" + p.Key
		if p.Links.Branch != "" {
			key = p.Repo + "\x00branch:" + p.Links.Branch
		}
		old := pieces[key]
		if old == nil {
			q := p
			pieces[key] = &q
			return
		}
		if rank(p.Stage) > rank(old.Stage) {
			old.Stage = p.Stage
		}
		if p.Stuck != "" {
			old.Stuck = p.Stuck
		}
		if p.Updated.After(old.Updated) {
			old.Updated = p.Updated
		}
		old.Links.Runs = append(old.Links.Runs, p.Links.Runs...)
		old.Links.Batches = append(old.Links.Batches, p.Links.Batches...)
		if p.Links.Worktree != "" {
			old.Links.Worktree = p.Links.Worktree
		}
	}
	for _, dir := range repos {
		root, err := git(ctx, dir, "rev-parse", "--show-toplevel")
		if err != nil {
			errs = append(errs, fmt.Errorf("repository %s: %w", dir, err))
			continue
		}
		root = canonical(root)
		if list, e := git(ctx, root, "worktree", "list", "--porcelain"); e == nil {
			first, _, _ := strings.Cut(list, "\n")
			if path, ok := strings.CutPrefix(first, "worktree "); ok {
				root = canonical(path)
			}
		}
		if _, ok := roots[root]; ok {
			continue
		}
		roots[root] = root
		wt := map[string]string{}
		worktrees[root] = wt
		out, err := git(ctx, root, "worktree", "list", "--porcelain")
		if err != nil {
			errs = append(errs, fmt.Errorf("worktrees %s: %w", root, err))
		} else {
			var path string
			for _, line := range strings.Split(out, "\n") {
				if v, ok := strings.CutPrefix(line, "worktree "); ok {
					path = canonical(v)
				}
				if v, ok := strings.CutPrefix(line, "branch refs/heads/"); ok {
					wt[path] = v
				}
			}
		}
		def, err := git(ctx, root, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD")
		if err == nil {
			def = strings.TrimPrefix(def, "origin/")
		} else {
			for _, name := range []string{"main", "master"} {
				if _, e := git(ctx, root, "show-ref", "--verify", "--quiet", "refs/heads/"+name); e == nil {
					def = name
					break
				}
			}
		}
		if def == "" {
			def, err = git(ctx, root, "symbolic-ref", "--quiet", "--short", "HEAD")
			if err != nil {
				errs = append(errs, fmt.Errorf("default branch %s: %w", root, err))
			}
		}
		out, err = git(ctx, root, "for-each-ref", "--format=%(refname:short)%09%(committerdate:iso-strict)", "refs/heads/")
		if err != nil {
			errs = append(errs, fmt.Errorf("branches %s: %w", root, err))
		} else {
			for _, line := range strings.Split(out, "\n") {
				name, date, ok := strings.Cut(line, "\t")
				if !ok || name == def {
					continue
				}
				stage := Branch
				if def != "" {
					_, e := git(ctx, root, "merge-base", "--is-ancestor", "refs/heads/"+name, "refs/heads/"+def)
					if e == nil {
						stage = Merged
					} else if ex, ok := e.(*exec.ExitError); !ok || ex.ExitCode() != 1 {
						errs = append(errs, fmt.Errorf("merge status %s: %w", name, e))
					}
				}
				updated, _ := time.Parse(time.RFC3339, date)
				p := Piece{Repo: root, Key: "branch:" + name, Title: name, Stage: stage, Updated: updated, Links: Links{Branch: name}}
				for path, b := range wt {
					if b == name {
						p.Links.Worktree = path
					}
				}
				add(p)
			}
		}
		f, err := os.Open(filepath.Join(root, "PLAN.md"))
		if err == nil {
			stat, _ := f.Stat()
			sc := bufio.NewScanner(f)
			heading := false
			for sc.Scan() {
				line := strings.TrimSpace(sc.Text())
				if strings.HasPrefix(line, "#") {
					heading = true
				}
				if heading && strings.HasPrefix(line, "- [ ] ") {
					title := strings.TrimSpace(strings.TrimPrefix(line, "- [ ] "))
					p := Piece{Repo: root, Key: "plan:" + title, Title: title, Stage: Idea}
					if stat != nil {
						p.Updated = stat.ModTime()
					}
					add(p)
				}
			}
			if e := sc.Err(); e != nil {
				errs = append(errs, e)
			}
			f.Close()
		} else if !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	for _, b := range batches {
		root := canonical(b.Dir)
		if _, ok := roots[root]; !ok {
			for k, wt := range worktrees {
				if _, found := wt[root]; found {
					root = k
					break
				}
			}
		}
		if _, ok := roots[root]; !ok {
			continue
		}
		for _, t := range b.Tasks {
			p := Piece{Repo: root, Key: "batch:" + b.ID + "/" + t.ID, Title: t.Prompt, Stage: Agent, Updated: b.Started, Links: Links{Branch: t.Branch, Worktree: t.Worktree, Batches: []string{b.ID}}}
			if t.Branch != "" {
				p.Key = "branch:" + t.Branch
			}
			if len(t.Attempts) > 0 {
				a := t.Attempts[len(t.Attempts)-1]
				if len(a.Gates) > 0 {
					p.Stage = Gates
				}
				for _, g := range a.Gates {
					if g.ExitCode != 0 || g.InfrastructureError != "" || g.Killed != "" {
						p.Stuck = "gate " + g.Command + " failed"
						break
					}
				}
				if a.RunID != "" {
					p.Links.Runs = []string{a.RunID}
				}
			}
			add(p)
		}
	}
	for _, r := range runs {
		linked := false
		for _, p := range pieces {
			for _, id := range p.Links.Runs {
				if id == r.ID {
					if r.Started.After(p.Updated) {
						p.Updated = r.Started
					}
					linked = true
					break
				}
			}
		}
		if linked {
			continue
		}
		if r.Task.Dir == "" {
			continue
		}
		dir := canonical(r.Task.Dir)
		root := ""
		branch := ""
		for k, wt := range worktrees {
			for path, b := range wt {
				if dir == path {
					root = k
					branch = b
				}
			}
		}
		if root == "" {
			v, e := git(ctx, dir, "rev-parse", "--show-toplevel")
			if e == nil {
				root = canonical(v)
				if _, ok := roots[root]; !ok {
					continue
				}
				branch, _ = git(ctx, dir, "symbolic-ref", "--quiet", "--short", "HEAD")
			}
		}
		if root == "" {
			continue
		}
		p := Piece{Repo: root, Key: "run:" + r.ID, Title: r.Task.Prompt, Stage: Agent, Updated: r.Started, Links: Links{Branch: branch, Runs: []string{r.ID}}}
		if branch != "" {
			p.Key = "branch:" + branch
		}
		add(p)
	}
	result := make([]Piece, 0, len(pieces))
	for _, p := range pieces {
		sort.Strings(p.Links.Runs)
		p.Links.Runs = compact(p.Links.Runs)
		sort.Strings(p.Links.Batches)
		p.Links.Batches = compact(p.Links.Batches)
		result = append(result, *p)
	}
	sort.Slice(result, func(i, j int) bool {
		a, b := result[i], result[j]
		if a.Repo != b.Repo {
			return a.Repo < b.Repo
		}
		if a.Stage != b.Stage {
			return rank(a.Stage) < rank(b.Stage)
		}
		return a.Key < b.Key
	})
	return result, errors.Join(errs...)
}
func compact(s []string) []string {
	out := s[:0]
	for _, v := range s {
		if len(out) == 0 || out[len(out)-1] != v {
			out = append(out, v)
		}
	}
	return out
}
