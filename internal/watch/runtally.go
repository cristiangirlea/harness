package watch

import (
	"os"
	"path/filepath"
	"runtime"
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
	loads int // records read from disk, for tests
}

// tallied is one run's share of the card, with what the read decision needs.
type tallied struct {
	owner, dir    string
	purpose       string
	repoKey, repo string // the checkout's path, which tells same-named ones apart, and its name
	sum           purposeSum
}

func tallyOf(rec runlog.Record, repoKey, repo string) tallied {
	t := tallied{owner: rec.Owner, dir: rec.Task.Dir, purpose: purpose.Resolve(rec.Task.Purpose, rec.Task.Prompt).Category, repoKey: repoKey, repo: repo}
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
	// Resolved per refresh only, so a reused worktree path is looked at again.
	checkouts := map[string][2]string{}
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
			co, known := checkouts[rec.Task.Dir]
			if !known {
				co[0], co[1] = checkoutOf(rec.Task.Dir)
				checkouts[rec.Task.Dir] = co
			}
			t = tallyOf(rec, co[0], co[1])
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

// checkoutOf finds the checkout holding dir and returns its path and name; a worktree's .git file leads to its main checkout.
func checkoutOf(dir string) (key, name string) {
	if dir == "" {
		return "", ""
	}
	clean := filepath.Clean(dir)
	start := clean
	if repo := managedRepo(clean); repo != "" {
		start = repo
	}
	for d := start; ; {
		gitPath := filepath.Join(d, ".git")
		if info, err := os.Lstat(gitPath); err == nil {
			found := d
			if main := worktreeMain(d, gitPath, info); main != "" {
				found = main
			}
			return keyOf(found), strings.TrimSuffix(filepath.Base(found), ".git")
		}
		up := filepath.Dir(d)
		if up == d {
			break
		}
		d = up
	}
	return keyOf(start), filepath.Base(start)
}

// managedRepo is the repository holding an hrn slot or batch worktree, which may be gone.
func managedRepo(dir string) string {
	slots := filepath.Dir(dir)
	if hrn := filepath.Dir(slots); filepath.Base(hrn) == ".hrn" && (filepath.Base(slots) == "slots" || filepath.Base(slots) == "worktrees") {
		return filepath.Dir(hrn)
	}
	return ""
}

// worktreeMain reads a linked worktree's .git file; submodules and anything unreadable give "".
func worktreeMain(dir, gitPath string, info os.FileInfo) string {
	if !info.Mode().IsRegular() || info.Size() > 4096 {
		return ""
	}
	data, err := os.ReadFile(gitPath)
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(string(data), "\n")
	gitdir, ok := strings.CutPrefix(strings.TrimSpace(line), "gitdir:")
	if !ok {
		return ""
	}
	g := filepath.FromSlash(strings.ReplaceAll(strings.TrimSpace(gitdir), `\`, "/"))
	if !filepath.IsAbs(g) {
		g = filepath.Join(dir, g)
	}
	g = filepath.Clean(g)
	if filepath.Base(filepath.Dir(g)) != "worktrees" {
		return ""
	}
	common := filepath.Dir(filepath.Dir(g))
	if filepath.Base(common) == ".git" {
		return filepath.Dir(common)
	}
	return common // a bare repository
}

func keyOf(path string) string {
	if runtime.GOOS == "windows" {
		return strings.ToLower(path)
	}
	return path
}
