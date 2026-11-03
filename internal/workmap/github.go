package workmap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/cristiangirlea/harness/internal/ws"
)

var githubTTL = 5 * time.Minute
var githubOrigin = regexp.MustCompile(`^(?:https://(?:[^@/]+@)?github\.com/|git@github\.com:|ssh://git@github\.com/)([A-Za-z0-9][A-Za-z0-9-]*)/([A-Za-z0-9._][A-Za-z0-9._-]*?)(?:\.git)?/?$`)

type githubCheck struct {
	Name       string `json:"name,omitempty"`
	Context    string `json:"context,omitempty"`
	State      string `json:"state,omitempty"`
	Status     string `json:"status,omitempty"`
	Conclusion string `json:"conclusion,omitempty"`
}
type githubItem struct {
	Number          int    `json:"number"`
	Title           string `json:"title"`
	URL             string `json:"url"`
	Head            string `json:"headRefName,omitempty"`
	CrossRepository bool   `json:"isCrossRepository,omitempty"`
	Draft           bool   `json:"isDraft,omitempty"`
	MergeCommit     struct {
		OID string `json:"oid,omitempty"`
	} `json:"mergeCommit,omitempty"`
	Review  string        `json:"reviewDecision,omitempty"`
	Checks  []githubCheck `json:"statusCheckRollup,omitempty"`
	Updated time.Time     `json:"updatedAt,omitzero"`
	Merged  time.Time     `json:"mergedAt,omitzero"`
}
type githubCache struct {
	Fetched time.Time    `json:"fetched"`
	Tried   time.Time    `json:"tried,omitzero"`
	Failed  string       `json:"failed,omitempty"`
	Issues  []githubItem `json:"issues"`
	Open    []githubItem `json:"open"`
	Merged  []githubItem `json:"merged"`
}

func parseGitHubRepo(url string) string {
	m := githubOrigin.FindStringSubmatch(url)
	if m == nil || m[2] == "." || m[2] == ".." {
		return ""
	}
	return strings.ToLower(m[1] + "/" + m[2])
}

func githubRecent(t time.Time) bool {
	age := time.Since(t)
	return !t.IsZero() && age >= 0 && age < githubTTL
}

func githubRetry(f func() error) error {
	var err error
	for i := 0; i < 20; i++ {
		err = f()
		if err == nil || errors.Is(err, fs.ErrNotExist) || runtime.GOOS != "windows" {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
	return err
}

func writeGitHubCache(path string, cache githubCache) error {
	data, err := json.Marshal(cache)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".github-*.json")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	_, err = f.Write(data)
	if e := f.Close(); err == nil {
		err = e
	}
	if err != nil {
		return err
	}
	return githubRetry(func() error { return os.Rename(name, path) })
}

func githubRepo(ctx context.Context, home, root, repo string, pieces map[string]*Piece) error {
	path := filepath.Join(home, "workmap", "github", strings.ReplaceAll(repo, "/", "__")+".json")
	var cached githubCache
	var data []byte
	readErr := githubRetry(func() (err error) { data, err = os.ReadFile(path); return err })
	decoded := readErr == nil && json.Unmarshal(data, &cached) == nil
	if !decoded {
		cached = githubCache{}
	}
	valid := decoded && !cached.Fetched.IsZero()
	answer := cached
	var result error
	if !githubRecent(cached.Fetched) && !githubRecent(cached.Tried) {
		var fresh githubCache
		calls := []struct {
			args []string
			dest *[]githubItem
		}{
			{[]string{"issue", "list", "--repo=" + repo, "--state", "open", "--limit", "100", "--json", "number,title,url,updatedAt"}, &fresh.Issues},
			{[]string{"pr", "list", "--repo=" + repo, "--state", "open", "--limit", "100", "--json", "number,title,url,headRefName,isCrossRepository,isDraft,reviewDecision,statusCheckRollup,updatedAt"}, &fresh.Open},
			{[]string{"pr", "list", "--repo=" + repo, "--state", "merged", "--limit", "30", "--json", "number,title,url,headRefName,isCrossRepository,mergeCommit,mergedAt"}, &fresh.Merged},
		}
		for _, call := range calls {
			raw, err := ws.GH(ctx, call.args...)
			if err == nil {
				err = json.Unmarshal(raw, call.dest)
			}
			if err != nil {
				result = fmt.Errorf("GitHub %s: %w", repo, err)
				fresh.Failed = err.Error()
				break
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if result == nil {
			fresh.Fetched = time.Now()
			fresh.Tried = fresh.Fetched
			answer = fresh
		} else {
			if !valid {
				answer = githubCache{}
			}
			answer.Tried, answer.Failed = time.Now(), fresh.Failed
		}
		if err := writeGitHubCache(path, answer); err != nil {
			result = errors.Join(result, fmt.Errorf("GitHub cache %s: %w", repo, err))
		}
		if result != nil && !valid && answer.Fetched.IsZero() {
			return result
		}
	} else if !githubRecent(cached.Fetched) {
		cause := cached.Failed
		if cause == "" {
			cause = "refresh recently failed"
		}
		result = fmt.Errorf("GitHub %s: %s", repo, cause)
		if !valid {
			return result
		}
	}
	for _, issue := range answer.Issues {
		p := &Piece{Repo: root, Key: fmt.Sprintf("issue#%d", issue.Number), Title: issue.Title, Stage: Idea, Updated: issue.Updated, Links: Links{Issue: issue.URL}}
		pieces[root+"\x00"+p.Key] = p
	}
	openHeads := map[string]bool{}
	for _, pr := range answer.Open {
		if !pr.CrossRepository && pr.Head != "" {
			openHeads[pr.Head] = true
		}
		key := root + "\x00branch:" + pr.Head
		p := pieces[key]
		if p == nil || pr.Head == "" || pr.CrossRepository {
			p = &Piece{Repo: root, Key: fmt.Sprintf("pr#%d", pr.Number), Title: pr.Title, Links: Links{Branch: pr.Head}}
			pieces[root+"\x00"+p.Key] = p
		}
		stage := PR
		checks := ""
		stuck := ""
		if len(pr.Checks) > 0 {
			stage = CI
			checks = "passing"
		}
		for _, c := range pr.Checks {
			state := strings.ToUpper(c.Conclusion)
			if state == "" {
				state = strings.ToUpper(c.State)
			}
			switch state {
			case "FAILURE", "FAILED", "ERROR", "CANCELLED", "TIMED_OUT", "ACTION_REQUIRED", "STARTUP_FAILURE":
				checks = "failing"
				if stuck == "" {
					name := c.Name
					if name == "" {
						name = c.Context
					}
					stuck = "CI red: " + name
				}
			case "SUCCESS", "NEUTRAL", "SKIPPED":
			default:
				if checks != "failing" {
					checks = "pending"
				}
			}
		}
		if stuck == "" && pr.Review == "CHANGES_REQUESTED" {
			stuck = "review: changes requested"
		}
		if stuck == "" && checks == "passing" && !pr.CrossRepository && !pr.Draft {
			stuck = "ready for you to merge"
		}
		if rank(stage) > rank(p.Stage) {
			p.Stage = stage
		}
		p.Links.PR = pr.URL
		p.Links.Checks = checks
		if stuck == "" && checks == "passing" && !p.stuckAt.IsZero() && pr.Updated.After(p.stuckAt) {
			p.Stuck = ""
		}
		if stuck != "" && rank(p.Stage) < rank(Merged) {
			p.Stuck = stuck
		}
		if pr.Updated.After(p.Updated) {
			p.Updated = pr.Updated
		}
	}
	tagged := map[string]bool{}
	if len(answer.Merged) > 0 {
		if commits, err := git(ctx, root, "rev-list", "--tags=v*"); err == nil {
			for _, oid := range strings.Split(commits, "\n") {
				if oid != "" {
					tagged[oid] = true
				}
			}
		}
	}
	for _, pr := range answer.Merged {
		stage := Merged
		if tagged[pr.MergeCommit.OID] {
			stage = Released
		}
		if pr.CrossRepository {
			p := &Piece{Repo: root, Key: fmt.Sprintf("pr#%d", pr.Number), Title: pr.Title, Stage: stage, Updated: pr.Merged, Links: Links{PR: pr.URL, Branch: pr.Head}}
			pieces[root+"\x00"+p.Key] = p
			continue
		}
		if openHeads[pr.Head] {
			continue
		}
		if pr.Head == "" || pieces[root+"\x00branch:"+pr.Head] == nil {
			p := &Piece{Repo: root, Key: fmt.Sprintf("pr#%d", pr.Number), Title: pr.Title, Stage: stage, Updated: pr.Merged, Links: Links{PR: pr.URL, Branch: pr.Head}}
			pieces[root+"\x00"+p.Key] = p
			continue
		}
		if p := pieces[root+"\x00branch:"+pr.Head]; p != nil && pr.Head != "" {
			if rank(p.Stage) < rank(stage) {
				p.Stage = stage
			}
			p.Stuck = ""
			p.Links.PR = pr.URL
			if pr.Merged.After(p.Updated) {
				p.Updated = pr.Merged
			}
		}
	}
	return result
}

func githubSources(ctx context.Context, home string, roots map[string]string, pieces map[string]*Piece) error {
	type target struct{ root, repo string }
	var targets []target
	for root := range roots {
		url, err := git(ctx, root, "remote", "get-url", "origin")
		if err != nil {
			continue
		}
		if repo := parseGitHubRepo(url); repo != "" {
			targets = append(targets, target{root, repo})
		}
	}
	// Each worker builds its own map so joins remain deterministic and race-free.
	type response struct {
		target target
		pieces map[string]*Piece
		err    error
	}
	results := make(chan response, len(targets))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for _, t := range targets {
		wg.Add(1)
		go func(t target) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				results <- response{target: t, err: ctx.Err()}
				return
			}
			local := map[string]*Piece{}
			for k, p := range pieces {
				if p.Repo == t.root {
					q := *p
					local[k] = &q
				}
			}
			err := githubRepo(ctx, home, t.root, t.repo, local)
			results <- response{t, local, err}
		}(t)
	}
	wg.Wait()
	close(results)
	var errs []error
	seen := map[string]bool{}
	for r := range results {
		for k, p := range r.pieces {
			pieces[k] = p
		}
		if r.err != nil {
			s := r.err.Error()
			if i := strings.Index(s, ": "); i >= 0 {
				s = s[i+2:]
			}
			if !seen[s] {
				errs = append(errs, r.err)
				seen[s] = true
			}
		}
	}
	return errors.Join(errs...)
}
