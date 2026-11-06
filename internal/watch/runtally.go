package watch

import (
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/cristiangirlea/harness/internal/owner"
	"github.com/cristiangirlea/harness/internal/purpose"
	"github.com/cristiangirlea/harness/internal/runlog"
)

// runTally keeps each finished run's share of the all-time card, so a refresh reads only new and unfinished runs.
type runTally struct {
	mu    sync.Mutex
	m     map[string]tallied
	repos map[string]string // run folder to repository name
	loads int               // records read from disk, for tests
}

// tallied is one run's share of the card, with what the read decision needs.
type tallied struct {
	owner, dir    string
	purpose, repo string
	sum           purposeSum
}

func tallyOf(rec runlog.Record, repo string) tallied {
	t := tallied{owner: rec.Owner, dir: rec.Task.Dir, purpose: purpose.Resolve(rec.Task.Purpose, rec.Task.Prompt).Category, repo: repo}
	t.sum.add(rec)
	return t
}

// tallyRuns returns every recorded run v may read; a run directory removed since the last call drops out.
func (s *server) tallyRuns(v owner.Viewer) []tallied {
	entries, err := os.ReadDir(runlog.Root())
	if err != nil {
		return nil // the run list reports the same failure
	}
	c := &s.tally
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.repos == nil {
		c.repos = map[string]string{}
	}
	kept := make(map[string]tallied, len(entries))
	var runs []tallied
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		t, ok := c.m[e.Name()]
		if !ok {
			rec, err := runlog.Load(e.Name())
			c.loads++
			if err != nil {
				continue
			}
			repo, known := c.repos[rec.Task.Dir]
			if !known {
				repo = repoName(rec.Task.Dir)
				c.repos[rec.Task.Dir] = repo
			}
			t = tallyOf(rec, repo)
			if !rec.Done {
				runs = appendReadable(runs, v, t)
				continue
			}
		}
		kept[e.Name()] = t
		runs = appendReadable(runs, v, t)
	}
	c.m = kept
	return runs
}

func appendReadable(runs []tallied, v owner.Viewer, t tallied) []tallied {
	if v.CanReadRecord(t.owner, t.dir) {
		return append(runs, t)
	}
	return runs
}

// repoName names the checkout a run folder belongs to; a worktree's .git file points back into it.
func repoName(dir string) string {
	if dir == "" {
		return ""
	}
	gitPath := filepath.Join(dir, ".git")
	info, err := os.Lstat(gitPath)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4096 {
		return repoOf(dir)
	}
	data, err := os.ReadFile(gitPath)
	if err != nil {
		return repoOf(dir)
	}
	line, _, _ := strings.Cut(string(data), "\n")
	gitdir, ok := strings.CutPrefix(strings.TrimSpace(line), "gitdir:")
	gitdir = strings.ReplaceAll(strings.TrimSpace(gitdir), `\`, "/")
	i := strings.LastIndex(gitdir, "/worktrees/")
	if !ok || i < 0 {
		return repoOf(dir)
	}
	checkout := strings.TrimSuffix(strings.TrimSuffix(gitdir[:i], "/.git"), ".git")
	if name := path.Base(checkout); name != "." && name != "/" && name != ".." {
		return name
	}
	return repoOf(dir)
}
