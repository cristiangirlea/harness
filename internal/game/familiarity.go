package game

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/gitenv"
	"github.com/cristiangirlea/harness/internal/nowindow"
	"github.com/cristiangirlea/harness/internal/runlog"
)

// AreaExposure counts recorded encounters with an area, not authorship.
type AreaExposure struct {
	Repo, Area string
	Count      int
}

// Familiarity holds the most exposed and unseen agent-touched areas.
type Familiarity struct {
	Known, Unseen      []AreaExposure
	UnreadablePractice bool
	// Partial is set when the time budget ran out; the Git evidence of repositories not read in time is left out.
	Partial bool
}

// familiarityBudget bounds the whole read, since callers poll it with short limits.
var familiarityBudget = 3 * time.Second

const (
	gitReaders = 6
	// A repository read younger than repoFresh is reused; one up to repoStale stands in when a refresh runs out of time.
	repoFresh = 10 * time.Minute
	repoStale = 24 * time.Hour
)

// runGit runs one git read; tests replace it to stand in for a slow git.
var runGit = func(ctx context.Context, cmd *exec.Cmd) ([]byte, error) { return cmd.Output() }

// ReadFamiliarity derives the view from local records and repositories within familiarityBudget.
func ReadFamiliarity(ctx context.Context, now time.Time) (Familiarity, error) {
	runs, err := runlog.List(0)
	if err != nil {
		return Familiarity{}, err
	}
	batches, _, err := batch.List()
	if err != nil {
		return Familiarity{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, familiarityBudget)
	defer cancel()
	return deriveFamiliarity(ctx, Records{Runs: runs, Batches: batches}, now)
}

func gitEvidence(ctx context.Context, dir string, args ...string) string {
	out, _ := gitRead(ctx, dir, args...)
	return out
}

// gitRead reports settled when git ran to its own exit, so the answer would be the same next time.
func gitRead(ctx context.Context, dir string, args ...string) (out string, settled bool) {
	if !gitenv.SupportsNoLazyFetch() {
		return "", true
	}
	if ctx.Err() != nil {
		return "", false
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir, "-c", "core.fsmonitor=false", "-c", "core.hooksPath=" + os.DevNull}, args...)...)
	nowindow.Set(cmd)
	cmd.Env = append(gitenv.WithoutRepoVars(os.Environ()), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0", "GIT_NO_LAZY_FETCH=1")
	cmd.WaitDelay = time.Second
	b, err := runGit(ctx, cmd)
	if err == nil {
		return string(b), true
	}
	var exit *exec.ExitError
	return "", errors.As(err, &exit) && ctx.Err() == nil
}

func fileArea(file string) string {
	file = filepath.ToSlash(file)
	file = path.Clean(file)
	if file == "." || file == ".." || strings.HasPrefix(file, "../") || strings.HasPrefix(file, "/") {
		return ""
	}
	parts := strings.Split(file, "/")
	if len(parts) > 1 && (parts[0] == "internal" || parts[0] == "cmd") {
		return path.Dir(file)
	}
	return parts[0]
}

// familiarityCache keeps git reads between calls: batch evidence for good, since commit IDs fix it, and repository reads for a while.
type familiarityCache struct {
	Batches map[string]batchEvidence `json:"batches"`
	Repos   map[string]repoEvidence  `json:"repos"`
}

type batchEvidence struct {
	Excluded []string `json:"excluded"`
	Areas    []string `json:"areas"`
}

type repoEvidence struct {
	Checked time.Time  `json:"checked"`
	Commits []authored `json:"commits"`
}

// authored is a recent commit by the repository's Git identity.
type authored struct {
	Hash  string   `json:"hash"`
	At    int64    `json:"at"`
	Areas []string `json:"areas"`
}

func familiarityCachePath() string { return filepath.Join(Dir(), "familiarity-cache.json") }

func loadFamiliarityCache() familiarityCache {
	var c familiarityCache
	if data, err := os.ReadFile(familiarityCachePath()); err == nil && json.Unmarshal(data, &c) != nil {
		c = familiarityCache{}
	}
	if c.Batches == nil {
		c.Batches = map[string]batchEvidence{}
	}
	if c.Repos == nil {
		c.Repos = map[string]repoEvidence{}
	}
	return c
}

func saveFamiliarityCache(c familiarityCache) {
	data, err := json.Marshal(c)
	if err != nil || os.MkdirAll(Dir(), 0700) != nil {
		return
	}
	f, err := os.CreateTemp(Dir(), ".familiarity-*")
	if err != nil {
		return
	}
	_, werr := f.Write(data)
	cerr := f.Close()
	if werr != nil || cerr != nil || os.Rename(f.Name(), familiarityCachePath()) != nil {
		os.Remove(f.Name())
	}
}

func batchKey(root, base string, oids []string) string {
	sorted := append([]string(nil), oids...)
	sort.Strings(sorted)
	sum := sha256.Sum256([]byte(root + "\x00" + base + "\x00" + strings.Join(sorted, " ")))
	return hex.EncodeToString(sum[:])
}

// readAll runs the reads on a few workers; each read stops on its own once its context ends.
func readAll(reads []func()) {
	next := make(chan func())
	var wg sync.WaitGroup
	for range min(gitReaders, len(reads)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for read := range next {
				read()
			}
		}()
	}
	for _, read := range reads {
		next <- read
	}
	close(next)
	wg.Wait()
}

func deriveFamiliarity(ctx context.Context, records Records, now time.Time) (Familiarity, error) {
	roots := map[string]string{}
	var order []string
	touched := map[string]map[string]bool{}
	excluded := map[string]map[string]bool{}
	rootOf := map[string]string{}
	nameOf := map[string]string{}
	findRoot := func(dir string) string {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return ""
		}
		var walked []string
		root := ""
		for d := abs; ; {
			if r, ok := rootOf[d]; ok {
				root = r
				break
			}
			walked = append(walked, d)
			if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
				root = d
				break
			}
			parent := filepath.Dir(d)
			if parent == d {
				break
			}
			d = parent
		}
		for _, d := range walked {
			rootOf[d] = root
		}
		return root
	}
	addRoot := func(dir string) string {
		if dir == "" {
			return ""
		}
		root := findRoot(dir)
		if root == "" {
			return ""
		}
		name, ok := nameOf[root]
		if !ok {
			name = repoName(root)
			nameOf[root] = name
		}
		if name != "" {
			if _, seen := roots[name]; !seen {
				order = append(order, name)
			}
			roots[name] = root
			if touched[name] == nil {
				touched[name] = map[string]bool{}
				excluded[name] = map[string]bool{}
			}
		}
		return name
	}
	for _, r := range records.Runs {
		name := addRoot(r.Task.Dir)
		if name == "" {
			continue
		}
		for _, f := range r.Result.ChangedFiles {
			if area := fileArea(f); area != "" {
				touched[name][area] = true
			}
		}
		for _, h := range r.Result.Commits {
			if safeOID(h) {
				excluded[name][h] = true
			}
		}
	}
	type batchRead struct {
		repo, dir, base, key string
		oids                 []string
		found                batchEvidence
		settled              bool
	}
	kept := loadFamiliarityCache()
	var batchReads []*batchRead
	for _, b := range records.Batches {
		name := addRoot(b.Dir)
		if name == "" {
			continue
		}
		var oids []string
		for _, task := range b.Tasks {
			oid := strings.TrimSpace(task.Commit)
			if !safeOID(oid) {
				continue
			}
			excluded[name][oid] = true
			if strings.HasPrefix(task.Branch, "hrn/") {
				oids = append(oids, oid)
			}
		}
		if len(oids) == 0 {
			continue
		}
		base := ""
		if safeOID(b.Base) {
			base = b.Base
		}
		read := &batchRead{repo: name, dir: b.Dir, base: base, oids: oids}
		fixed := base == "" || commitHash.MatchString(base)
		for _, oid := range oids {
			fixed = fixed && commitHash.MatchString(oid)
		}
		// Only full commit IDs fix the answer; a ref name may move, so its evidence is read every time.
		if fixed {
			read.key = batchKey(findRoot(b.Dir), base, oids)
		}
		if found, ok := kept.Batches[read.key]; ok && read.key != "" {
			read.found, read.settled = found, true
		}
		batchReads = append(batchReads, read)
	}
	type repoRead struct {
		root    string
		cached  repoEvidence
		hit     bool
		found   repoEvidence
		settled bool
	}
	repoReads := map[string]*repoRead{}
	for _, name := range order {
		rr := &repoRead{root: roots[name]}
		rr.cached, rr.hit = kept.Repos[rr.root]
		if rr.hit && !rr.cached.Checked.After(now) && now.Sub(rr.cached.Checked) < repoFresh {
			rr.found, rr.settled = rr.cached, true
		}
		repoReads[name] = rr
	}
	queue := append([]string(nil), order...)
	sort.SliceStable(queue, func(i, j int) bool {
		a, b := repoReads[queue[i]], repoReads[queue[j]]
		if a.hit != b.hit {
			return !a.hit
		}
		return a.cached.Checked.Before(b.cached.Checked)
	})
	var reads []func()
	for _, name := range queue {
		for _, br := range batchReads {
			if br.repo != name || br.settled {
				continue
			}
			reads = append(reads, func() {
				var found batchEvidence
				if br.base != "" {
					args := append([]string{"rev-list", "--first-parent"}, br.oids...)
					args = append(args, "^"+br.base, "--")
					out, ok := gitRead(ctx, br.dir, args...)
					if !ok {
						return
					}
					found.Excluded = strings.Fields(out)
				}
				logArgs := append([]string{"log", "--no-show-signature", "--format=", "--name-only", "--no-renames"}, br.oids...)
				if br.base != "" {
					logArgs = append(logArgs, "^"+br.base)
				} else {
					logArgs = append(logArgs, "--no-walk")
				}
				logArgs = append(logArgs, "--")
				out, ok := gitRead(ctx, br.dir, logArgs...)
				if !ok {
					return
				}
				areas := map[string]bool{}
				for _, f := range strings.Split(out, "\n") {
					if a := fileArea(strings.TrimSpace(f)); a != "" && !areas[a] {
						areas[a] = true
						found.Areas = append(found.Areas, a)
					}
				}
				br.found, br.settled = found, true
			})
		}
		rr := repoReads[name]
		if rr.settled {
			continue
		}
		reads = append(reads, func() {
			email, ok := gitRead(ctx, rr.root, "config", "--get", "user.email")
			if !ok {
				return
			}
			email = strings.TrimSpace(email)
			found := repoEvidence{Checked: now, Commits: []authored{}}
			if email != "" {
				out, ok := gitRead(ctx, rr.root, "log", "--no-show-signature", "--all", "--since="+now.AddDate(0, 0, -90).Format(time.RFC3339), "--until="+now.Format(time.RFC3339), "--format=%x1e%H%x00%ae%x00%ct", "--name-only", "--no-renames", "--")
				if !ok {
					return
				}
				for _, entry := range strings.Split(out, "\x1e") {
					lines := strings.Split(strings.TrimSpace(entry), "\n")
					fields := strings.Split(lines[0], "\x00")
					if len(fields) != 3 || !strings.EqualFold(fields[1], email) {
						continue
					}
					at, err := strconv.ParseInt(fields[2], 10, 64)
					if err != nil {
						continue
					}
					c := authored{Hash: fields[0], At: at, Areas: []string{}}
					seen := map[string]bool{}
					for _, f := range lines[1:] {
						if a := fileArea(strings.TrimSpace(f)); a != "" && !seen[a] {
							seen[a] = true
							c.Areas = append(c.Areas, a)
						}
					}
					found.Commits = append(found.Commits, c)
				}
			}
			rr.found, rr.settled = found, true
		})
	}
	readAll(reads)
	next := familiarityCache{Batches: map[string]batchEvidence{}, Repos: map[string]repoEvidence{}}
	dirty := false
	incomplete := map[string]bool{}
	for _, br := range batchReads {
		if !br.settled {
			incomplete[br.repo] = true
			continue
		}
		if br.key != "" {
			if _, ok := kept.Batches[br.key]; !ok {
				dirty = true
			}
			next.Batches[br.key] = br.found
		}
		for _, h := range br.found.Excluded {
			excluded[br.repo][h] = true
		}
		for _, a := range br.found.Areas {
			touched[br.repo][a] = true
		}
	}
	for _, name := range order {
		rr := repoReads[name]
		switch {
		case rr.settled:
			if !rr.found.Checked.Equal(rr.cached.Checked) || !rr.hit {
				dirty = true
			}
		case rr.hit && !rr.cached.Checked.After(now) && now.Sub(rr.cached.Checked) < repoStale:
			rr.found, rr.settled = rr.cached, true
		default:
			incomplete[name] = true
			continue
		}
		next.Repos[rr.root] = rr.found
	}
	if len(incomplete) > 0 {
		for key, found := range kept.Batches {
			if _, ok := next.Batches[key]; !ok {
				next.Batches[key] = found
			}
		}
		for root, found := range kept.Repos {
			if _, ok := next.Repos[root]; !ok {
				next.Repos[root] = found
			}
		}
	}
	if len(next.Batches) != len(kept.Batches) || len(next.Repos) != len(kept.Repos) {
		dirty = true
	}
	if dirty && gitenv.SupportsNoLazyFetch() {
		saveFamiliarityCache(next)
	}
	counts := map[string]int{}
	add := func(repo, area string) {
		if repo != "" && area != "" {
			counts[repo+"\x00"+area]++
		}
	}
	files, err := filepath.Glob(filepath.Join(Dir(), "reps", "*.json"))
	if err != nil {
		return Familiarity{}, err
	}
	unreadablePractice := false
	for _, f := range files {
		r, err := loadRep(strings.TrimSuffix(filepath.Base(f), ".json"))
		if err != nil {
			unreadablePractice = true
			continue
		}
		all := append([]Exercise(nil), r.Reads...)
		if r.Daily != nil {
			all = append(all, *r.Daily)
		}
		for _, e := range all {
			if e.AnsweredAt.IsZero() {
				continue
			}
			repo, _, _ := strings.Cut(e.Source, "#")
			areas := map[string]bool{}
			if e.Kind == "cold read" || e.Kind == "find the defect" || e.Kind == "read" {
				for _, line := range strings.Split(e.Material, "\n") {
					if strings.HasPrefix(line, "+++ ") {
						f := strings.TrimSpace(strings.TrimPrefix(line, "+++ "))
						if f != "/dev/null" {
							if strings.HasPrefix(f, "a/") || strings.HasPrefix(f, "b/") {
								f = f[2:]
							}
							areas[fileArea(f)] = true
						}
					}
					if strings.HasPrefix(line, "--- ") {
						f := strings.TrimSpace(strings.TrimPrefix(line, "--- "))
						if f != "/dev/null" {
							if strings.HasPrefix(f, "a/") || strings.HasPrefix(f, "b/") {
								f = f[2:]
							}
							areas[fileArea(f)] = true
						}
					}
				}
			}
			for area := range areas {
				add(repo, area)
			}
		}
	}
	since, until := now.AddDate(0, 0, -90).Unix(), now.Unix()
	for _, name := range order {
		if incomplete[name] {
			continue
		}
		for _, c := range repoReads[name].found.Commits {
			if c.At < since || c.At > until || excluded[name][c.Hash] {
				continue
			}
			for _, area := range c.Areas {
				add(name, area)
			}
		}
	}
	out := Familiarity{UnreadablePractice: unreadablePractice, Partial: len(incomplete) > 0}
	for key, count := range counts {
		repo, area, _ := strings.Cut(key, "\x00")
		out.Known = append(out.Known, AreaExposure{repo, area, count})
	}
	for repo, areas := range touched {
		if incomplete[repo] {
			continue
		}
		for area := range areas {
			if counts[repo+"\x00"+area] == 0 {
				out.Unseen = append(out.Unseen, AreaExposure{Repo: repo, Area: area})
			}
		}
	}
	less := func(a, b AreaExposure) bool {
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		if a.Repo != b.Repo {
			return a.Repo < b.Repo
		}
		return a.Area < b.Area
	}
	sort.Slice(out.Known, func(i, j int) bool { return less(out.Known[i], out.Known[j]) })
	sort.Slice(out.Unseen, func(i, j int) bool { return less(out.Unseen[i], out.Unseen[j]) })
	if len(out.Known) > 5 {
		out.Known = out.Known[:5]
	}
	if len(out.Unseen) > 5 {
		out.Unseen = out.Unseen[:5]
	}
	return out, nil
}
