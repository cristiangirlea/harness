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
	repoKey, repo string // the remote, or a remote-less checkout's path, which tells same-named ones apart; and its name
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
			var co [2]string
			if co[0], co[1] = recordedRepo(rec); co[0] == "" {
				var known bool
				if co, known = checkouts[rec.Task.Dir]; !known {
					co[0], co[1] = repoOfDir(rec.Task.Dir)
					checkouts[rec.Task.Dir] = co
				}
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

// repoOfRun is the repository a run recorded, or for an older record the remote of the checkout its folder is in.
func repoOfRun(rec runlog.Record) (key, name string) {
	if key, name = recordedRepo(rec); key != "" {
		return key, name
	}
	return repoOfDir(rec.Task.Dir)
}

// recordedRepo is checked again on read, since links are built from it and a record can be edited by hand.
func recordedRepo(rec runlog.Record) (key, name string) {
	if !runlog.ValidRepo(rec.Repo) {
		return "", ""
	}
	return repoKey(rec.Repo), rec.Repo
}

// repoKey folds case only where the host does: GitHub's names ignore it, other servers' may not.
func repoKey(repo string) string {
	if strings.HasPrefix(repo, "github.com/") {
		return "r:" + strings.ToLower(repo)
	}
	return "r:" + repo
}

// repoOfDir names the checkout holding dir by its remote; "" when there is no checkout or no remote to name it by.
func repoOfDir(dir string) (key, name string) {
	found := findCheckout(dir)
	if found == "" {
		return "", ""
	}
	if remote := runlog.RepoName(runlog.PickRemote(remotesOf(found))); remote != "" {
		return repoKey(remote), remote
	}
	return "", ""
}

// remotesOf reads a checkout's remotes from its git config; anything unusual reads as none.
func remotesOf(checkout string) map[string]string {
	g := gitDirOf(checkout)
	if g == "" {
		return nil
	}
	cfg := filepath.Join(g, "config")
	info, err := os.Lstat(cfg)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 64<<10 {
		return nil
	}
	data, err := os.ReadFile(cfg)
	if err != nil {
		return nil
	}
	return runlog.RemotesInConfig(string(data))
}

// gitDirOf is a checkout's .git folder, the folder a submodule's .git file names, or a bare repository itself.
func gitDirOf(checkout string) string {
	dotGit := filepath.Join(checkout, ".git")
	if info, err := os.Lstat(dotGit); err == nil {
		if info.IsDir() {
			return dotGit
		}
		if g := gitdirIn(checkout, dotGit, info); g != "" {
			if gi, err := os.Stat(g); err == nil && gi.IsDir() {
				return g
			}
		}
		return ""
	}
	// Only a folder with no .git of its own can be bare, so a checkout's own config file is never read.
	if head, err := os.Lstat(filepath.Join(checkout, "HEAD")); err == nil && head.Mode().IsRegular() {
		return checkout
	}
	return ""
}

// findCheckout is the checkout holding dir; a worktree's .git file leads to its main checkout.
func findCheckout(dir string) string {
	if dir == "" {
		return ""
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
			// A link, junction or short name and the real path are one checkout.
			if real, err := filepath.EvalSymlinks(found); err == nil {
				found = real
			}
			return found
		}
		up := filepath.Dir(d)
		if up == d {
			return ""
		}
		d = up
	}
}

// managedRepo is the repository holding an hrn slot or batch worktree, which may be gone.
func managedRepo(dir string) string {
	slots := filepath.Dir(dir)
	if hrn := filepath.Dir(slots); filepath.Base(hrn) == ".hrn" && (filepath.Base(slots) == "slots" || filepath.Base(slots) == "worktrees") {
		return filepath.Dir(hrn)
	}
	return ""
}

// worktreeMain is the main checkout a linked worktree's .git file names; submodules and anything unreadable give "".
func worktreeMain(dir, gitPath string, info os.FileInfo) string {
	g := gitdirIn(dir, gitPath, info)
	if g == "" || filepath.Base(filepath.Dir(g)) != "worktrees" {
		return ""
	}
	common := filepath.Dir(filepath.Dir(g))
	if filepath.Base(common) == ".git" {
		return filepath.Dir(common)
	}
	return common // a bare repository
}

// gitdirIn reads the folder a .git file names, made absolute.
func gitdirIn(dir, gitPath string, info os.FileInfo) string {
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
	return filepath.Clean(g)
}

func keyOf(path string) string {
	if runtime.GOOS == "windows" {
		return strings.ToLower(path)
	}
	return path
}
