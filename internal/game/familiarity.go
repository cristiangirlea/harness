package game

import (
	"context"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/runlog"
)

// AreaExposure counts recorded encounters with an area, not authorship.
type AreaExposure struct {
	Repo, Area string
	Count      int
}

// Familiarity holds the most exposed and unseen agent-touched areas.
type Familiarity struct{ Known, Unseen []AreaExposure }

// ReadFamiliarity derives the view from local records and repositories.
func ReadFamiliarity(now time.Time) (Familiarity, error) {
	runs, err := runlog.List(0)
	if err != nil {
		return Familiarity{}, err
	}
	batches, _, err := batch.List()
	if err != nil {
		return Familiarity{}, err
	}
	return deriveFamiliarity(Records{Runs: runs, Batches: batches}, now)
}

func gitEvidence(dir string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir, "-c", "core.fsmonitor=false"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
	b, err := cmd.Output()
	if err != nil {
		return ""
	}
	return string(b)
}

func fileArea(file string) string {
	file = filepath.ToSlash(file)
	file = strings.TrimPrefix(file, "a/")
	file = strings.TrimPrefix(file, "b/")
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

func deriveFamiliarity(records Records, now time.Time) (Familiarity, error) {
	roots := map[string]string{}
	touched := map[string]map[string]bool{}
	excluded := map[string]map[string]bool{}
	addRoot := func(dir string) string {
		if dir == "" {
			return ""
		}
		root := repoRoot(dir)
		name := repoName(root)
		if name != "" {
			roots[name] = root
		}
		return name
	}
	for _, r := range records.Runs {
		name := addRoot(r.Task.Dir)
		if name == "" {
			continue
		}
		if touched[name] == nil {
			touched[name] = map[string]bool{}
			excluded[name] = map[string]bool{}
		}
		for _, f := range r.Result.ChangedFiles {
			if area := fileArea("./" + f); area != "" {
				touched[name][area] = true
			}
		}
		if safeOID(r.StartHead) && safeOID(r.Result.EndHead) {
			for _, h := range strings.Fields(gitEvidence(r.Task.Dir, "rev-list", r.StartHead+".."+r.Result.EndHead, "--")) {
				excluded[name][h] = true
			}
		}
	}
	for _, b := range records.Batches {
		name := addRoot(b.Dir)
		if name == "" {
			continue
		}
		if touched[name] == nil {
			touched[name] = map[string]bool{}
			excluded[name] = map[string]bool{}
		}
		for _, task := range b.Tasks {
			oid := strings.TrimSpace(task.Commit)
			if !safeOID(oid) {
				continue
			}
			excluded[name][oid] = true
			if !strings.HasPrefix(task.Branch, "hrn/") {
				continue
			}
			if safeOID(b.Base) {
				for _, h := range strings.Fields(gitEvidence(b.Dir, "rev-list", b.Base+".."+oid, "--")) {
					excluded[name][h] = true
				}
			}
			diff := gitEvidence(b.Dir, "diff-tree", "--root", "--no-commit-id", "--name-only", "-r", oid, "--")
			if safeOID(b.Base) {
				diff = gitEvidence(b.Dir, "log", "--format=", "--name-only", "--no-renames", b.Base+".."+oid, "--")
			}
			for _, f := range strings.Split(diff, "\n") {
				if a := fileArea(strings.TrimSpace(f)); a != "" {
					touched[name][a] = true
				}
			}
		}
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
	for _, f := range files {
		r, err := loadRep(strings.TrimSuffix(filepath.Base(f), ".json"))
		if err != nil {
			return Familiarity{}, err
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
							areas[fileArea(f)] = true
						}
					}
					if strings.HasPrefix(line, "--- ") {
						f := strings.TrimSpace(strings.TrimPrefix(line, "--- "))
						if f != "/dev/null" {
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
	for repo, root := range roots {
		email := strings.TrimSpace(gitEvidence(root, "config", "--get", "user.email"))
		if email == "" {
			continue
		}
		output := gitEvidence(root, "log", "--all", "--since="+now.AddDate(0, 0, -90).Format(time.RFC3339), "--until="+now.Format(time.RFC3339), "--format=%x1e%H%x00%ae", "--name-only", "--no-renames", "--")
		for _, entry := range strings.Split(output, "\x1e") {
			lines := strings.Split(strings.TrimSpace(entry), "\n")
			if len(lines) == 0 {
				continue
			}
			hash, author, ok := strings.Cut(lines[0], "\x00")
			if !ok || !strings.EqualFold(author, email) || excluded[repo][hash] {
				continue
			}
			areas := map[string]bool{}
			for _, f := range lines[1:] {
				areas[fileArea(strings.TrimSpace(f))] = true
			}
			for area := range areas {
				add(repo, area)
			}
		}
	}
	var out Familiarity
	for key, count := range counts {
		repo, area, _ := strings.Cut(key, "\x00")
		out.Known = append(out.Known, AreaExposure{repo, area, count})
	}
	for repo, areas := range touched {
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
