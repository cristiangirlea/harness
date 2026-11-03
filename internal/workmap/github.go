package workmap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/cristiangirlea/harness/internal/ws"
)

var githubTTL = 5 * time.Minute
var githubOrigin = regexp.MustCompile(`^(?:https://(?:[^@/]+@)?github\.com/|git@github\.com:|ssh://git@github\.com/)([A-Za-z0-9-]+)/([A-Za-z0-9._-]+?)(?:\.git)?/?$`)

type githubCheck struct {
	Name       string `json:"name,omitempty"`
	Context    string `json:"context,omitempty"`
	State      string `json:"state,omitempty"`
	Status     string `json:"status,omitempty"`
	Conclusion string `json:"conclusion,omitempty"`
}
type githubItem struct {
	Number  int           `json:"number"`
	Title   string        `json:"title"`
	URL     string        `json:"url"`
	Head    string        `json:"headRefName,omitempty"`
	Draft   bool          `json:"isDraft,omitempty"`
	Review  string        `json:"reviewDecision,omitempty"`
	Checks  []githubCheck `json:"statusCheckRollup,omitempty"`
	Updated time.Time     `json:"updatedAt,omitzero"`
	Merged  time.Time     `json:"mergedAt,omitzero"`
}
type githubCache struct {
	Fetched time.Time    `json:"fetched"`
	Issues  []githubItem `json:"issues"`
	Open    []githubItem `json:"open"`
	Merged  []githubItem `json:"merged"`
}

func githubRepo(ctx context.Context, home, root, repo string, pieces map[string]*Piece) error {
	path := filepath.Join(home, "workmap", "github", strings.ReplaceAll(repo, "/", "__")+".json")
	var cached githubCache
	data, readErr := os.ReadFile(path)
	valid := readErr == nil && json.Unmarshal(data, &cached) == nil && !cached.Fetched.IsZero()
	answer := cached
	var result error
	if !valid || time.Since(cached.Fetched) >= githubTTL {
		var fresh githubCache
		calls := []struct {
			args []string
			dest *[]githubItem
		}{
			{[]string{"issue", "list", "-R", repo, "--state", "open", "--limit", "100", "--json", "number,title,url,updatedAt"}, &fresh.Issues},
			{[]string{"pr", "list", "-R", repo, "--state", "open", "--limit", "100", "--json", "number,title,url,headRefName,isDraft,reviewDecision,statusCheckRollup,updatedAt"}, &fresh.Open},
			{[]string{"pr", "list", "-R", repo, "--state", "merged", "--limit", "30", "--json", "number,title,url,headRefName,mergedAt"}, &fresh.Merged},
		}
		for _, call := range calls {
			raw, err := ws.GH(ctx, call.args...)
			if err == nil {
				err = json.Unmarshal(raw, call.dest)
			}
			if err != nil {
				result = fmt.Errorf("GitHub %s: %w", repo, err)
				break
			}
		}
		if result == nil {
			fresh.Fetched = time.Now()
			answer = fresh
			data, err := json.Marshal(fresh)
			if err == nil {
				err = os.MkdirAll(filepath.Dir(path), 0700)
			}
			if err == nil {
				var f *os.File
				f, err = os.CreateTemp(filepath.Dir(path), ".github-*.json")
				if err == nil {
					name := f.Name()
					defer os.Remove(name)
					_, err = f.Write(data)
					if e := f.Close(); err == nil {
						err = e
					}
					if err == nil {
						err = os.Rename(name, path)
					}
				}
			}
			if err != nil {
				result = fmt.Errorf("GitHub cache %s: %w", repo, err)
			}
		} else if !valid {
			return result
		}
	}
	for _, issue := range answer.Issues {
		p := &Piece{Repo: root, Key: fmt.Sprintf("issue#%d", issue.Number), Title: issue.Title, Stage: Idea, Updated: issue.Updated, Links: Links{Issue: issue.URL}}
		pieces[root+"\x00"+p.Key] = p
	}
	for _, pr := range answer.Open {
		key := root + "\x00branch:" + pr.Head
		p := pieces[key]
		if p == nil || pr.Head == "" {
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
			case "FAILURE", "FAILED", "ERROR", "CANCELLED", "TIMED_OUT":
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
		if rank(stage) > rank(p.Stage) {
			p.Stage = stage
		}
		p.Links.PR = pr.URL
		p.Links.Checks = checks
		if stuck != "" && rank(p.Stage) < rank(Merged) {
			p.Stuck = stuck
		}
		if pr.Updated.After(p.Updated) {
			p.Updated = pr.Updated
		}
	}
	for _, pr := range answer.Merged {
		if p := pieces[root+"\x00branch:"+pr.Head]; p != nil && pr.Head != "" {
			if rank(p.Stage) < rank(Merged) {
				p.Stage = Merged
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
		m := githubOrigin.FindStringSubmatch(url)
		if m != nil && m[2] != "." && m[2] != ".." {
			targets = append(targets, target{root, strings.ToLower(m[1] + "/" + m[2])})
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
			sem <- struct{}{}
			defer func() { <-sem }()
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
