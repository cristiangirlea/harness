package game

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/mask"
	"github.com/cristiangirlea/harness/internal/runlog"
)

const (
	kindRep  = "practice_rep"
	kindRead = "practice_read"
)

type Candidate struct {
	Kind     string `json:"kind"`
	Source   string `json:"source"`
	Material string `json:"material"`
	Reveal   string `json:"reveal,omitempty"`
}
type Source interface {
	Candidates(kind string) ([]Candidate, error)
	Read(ref string) (Candidate, error)
	Merged(ref string, after time.Time) (bool, error)
}
type Exercise struct {
	Candidate
	Attempt    int       `json:"attempt"`
	Answer     string    `json:"answer,omitempty"`
	AnsweredAt time.Time `json:"answered_at,omitempty"`
	Mark       string    `json:"mark,omitempty"`
}
type repFile struct {
	Date    string     `json:"date"`
	Daily   *Exercise  `json:"daily,omitempty"`
	Reads   []Exercise `json:"reads,omitempty"`
	Pending string     `json:"pending,omitempty"`
}
type Practice struct {
	source Source
	now    time.Time
}

func NewPractice(source Source, now time.Time) *Practice {
	return &Practice{source: source, now: now.UTC()}
}
func (p *Practice) date() string { return p.now.Format("2006-01-02") }
func repPath(date string) string { return filepath.Join(Dir(), "reps", date+".json") }
func loadRep(date string) (repFile, error) {
	if d, err := time.Parse("2006-01-02", date); err != nil || d.Format("2006-01-02") != date {
		return repFile{}, errors.New("invalid practice date")
	}
	r := repFile{Date: date}
	b, err := os.ReadFile(repPath(date))
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	r = repFile{}
	err = json.Unmarshal(b, &r)
	if err == nil && r.Date != date {
		return repFile{}, errors.New("practice record date does not match filename")
	}
	return r, err
}
func saveRep(date string, r repFile) error {
	if d, err := time.Parse("2006-01-02", date); err != nil || d.Format("2006-01-02") != date || r.Date != date {
		return errors.New("invalid practice record date")
	}
	dir := filepath.Join(Dir(), "reps")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".rep-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), repPath(date))
}
func safe(c Candidate) Candidate {
	c.Source = mask.Text(c.Source)
	c.Material = mask.Text(c.Material)
	c.Reveal = mask.Text(c.Reveal)
	return c
}
func visible(e Exercise) Exercise {
	if e.AnsweredAt.IsZero() {
		e.Reveal = ""
	}
	return e
}
func (p *Practice) history() (map[string]bool, map[string]string, error) {
	files, err := filepath.Glob(filepath.Join(Dir(), "reps", "*.json"))
	if err != nil {
		return nil, nil, err
	}
	reads, marks := map[string]bool{}, map[string]string{}
	for _, file := range files {
		date := strings.TrimSuffix(filepath.Base(file), ".json")
		r, err := loadRep(date)
		if err != nil {
			return nil, nil, err
		}
		var all []Exercise
		if r.Daily != nil {
			all = append(all, *r.Daily)
		}
		all = append(all, r.Reads...)
		for _, e := range all {
			if e.Kind == "read" {
				reads[strings.ToLower(e.Source)] = true
			}
			if e.Mark != "" {
				marks["kind:"+e.Kind] = e.Mark
				repo, _, _ := strings.Cut(e.Source, "#")
				marks["repo:"+strings.ToLower(repo)] = e.Mark
			}
		}
	}
	return reads, marks, nil
}
func needsPractice(mark string) bool { return mark == "missed" || mark == "partly" }

func (p *Practice) pick(attempt int, allowRead bool) (Exercise, error) {
	kinds := []string{"cold read", "find the defect", "read", "recall"}
	reads, marks, err := p.history()
	if err != nil {
		return Exercise{}, err
	}
	h := fnv.New64a()
	fmt.Fprintf(h, "%s:%d", p.date(), attempt)
	seed := h.Sum64()
	var preferred, ordinary []Candidate
	for _, k := range kinds {
		if k == "read" && !allowRead {
			continue
		}
		cs, err := p.source.Candidates(k)
		if err != nil {
			continue
		}
		for _, c := range cs {
			if k == "read" && reads[strings.ToLower(c.Source)] {
				continue
			}
			c.Kind = k
			repo, _, _ := strings.Cut(c.Source, "#")
			if needsPractice(marks["kind:"+k]) || needsPractice(marks["repo:"+strings.ToLower(repo)]) {
				preferred = append(preferred, c)
			} else {
				ordinary = append(ordinary, c)
			}
		}
	}
	cs := preferred
	if len(cs) == 0 {
		cs = ordinary
	}
	sort.Slice(cs, func(i, j int) bool {
		if cs[i].Kind != cs[j].Kind {
			return cs[i].Kind < cs[j].Kind
		}
		return cs[i].Source < cs[j].Source
	})
	if len(cs) > 0 {
		return Exercise{Candidate: safe(cs[seed%uint64(len(cs))]), Attempt: attempt}, nil
	}
	return Exercise{}, errors.New("no practice candidates available")
}
func (p *Practice) Today() (Exercise, error) {
	r, err := loadRep(p.date())
	if err != nil {
		return Exercise{}, err
	}
	if r.Daily == nil {
		e, err := p.pick(0, len(r.Reads) < 3)
		if err != nil {
			return e, err
		}
		r.Daily = &e
		r.Pending = "daily"
		if err = saveRep(p.date(), r); err != nil {
			return Exercise{}, err
		}
	}
	if r.Pending != "daily" {
		r.Pending = "daily"
		if err := saveRep(p.date(), r); err != nil {
			return Exercise{}, err
		}
	}
	return visible(*r.Daily), nil
}
func (p *Practice) Swap() (Exercise, error) {
	r, err := loadRep(p.date())
	if err != nil {
		return Exercise{}, err
	}
	if r.Daily == nil {
		return p.Today()
	}
	if !r.Daily.AnsweredAt.IsZero() {
		return Exercise{}, errors.New("cannot swap an answered exercise")
	}
	e, err := p.pick(r.Daily.Attempt+1, len(r.Reads) < 3)
	if err != nil {
		return e, err
	}
	r.Daily = &e
	r.Pending = "daily"
	if err = saveRep(p.date(), r); err != nil {
		return Exercise{}, err
	}
	return visible(e), nil
}

var prRef = regexp.MustCompile(`^[A-Za-z0-9]+(?:-[A-Za-z0-9]+)*/[A-Za-z0-9_.-]+#[1-9][0-9]*$`)

func (p *Practice) Read(ref string) (Exercise, error) {
	owner, rest, _ := strings.Cut(ref, "/")
	repo, _, _ := strings.Cut(rest, "#")
	if !prRef.MatchString(ref) || len(owner) > 39 || len(repo) > 100 || repo == "." || repo == ".." || mask.Text(ref) != ref {
		return Exercise{}, errors.New("expected owner/repo#number")
	}
	files, err := filepath.Glob(filepath.Join(Dir(), "reps", "*.json"))
	if err != nil {
		return Exercise{}, err
	}
	for _, file := range files {
		previous, err := loadRep(strings.TrimSuffix(filepath.Base(file), ".json"))
		if err != nil {
			return Exercise{}, err
		}
		if previous.Daily != nil && previous.Daily.Kind == "read" && strings.EqualFold(previous.Daily.Source, ref) {
			return Exercise{}, errors.New("pull request already selected")
		}
		for _, read := range previous.Reads {
			if strings.EqualFold(read.Source, ref) {
				return Exercise{}, errors.New("pull request already selected")
			}
		}
	}
	r, err := loadRep(p.date())
	if err != nil {
		return Exercise{}, err
	}
	for _, x := range r.Reads {
		if strings.EqualFold(x.Source, ref) {
			return Exercise{}, errors.New("pull request already read today")
		}
	}
	if r.Daily != nil && r.Daily.Kind == "read" && strings.EqualFold(r.Daily.Source, ref) {
		return Exercise{}, errors.New("pull request is today's exercise")
	}
	count := len(r.Reads)
	if r.Daily != nil && r.Daily.Kind == "read" {
		count++
	}
	if count >= 3 {
		return Exercise{}, errors.New("three reads already started today")
	}
	c, err := p.source.Read(ref)
	if err != nil {
		return Exercise{}, err
	}
	c.Kind = "read"
	c.Source = ref
	e := Exercise{Candidate: safe(c)}
	r.Reads = append(r.Reads, e)
	r.Pending = e.Source
	if err = saveRep(p.date(), r); err != nil {
		return Exercise{}, err
	}
	return visible(e), nil
}
func (p *Practice) pending(r *repFile) *Exercise {
	if r.Pending == "daily" {
		return r.Daily
	}
	for i := range r.Reads {
		if r.Reads[i].Source == r.Pending {
			return &r.Reads[i]
		}
	}
	return nil
}

type RebuildWarning struct{ Err error }

func (w *RebuildWarning) Error() string { return "practice saved; rebuild warning: " + w.Err.Error() }
func (w *RebuildWarning) Unwrap() error { return w.Err }

func (p *Practice) Answer(answer string) (string, error) {
	if strings.TrimSpace(answer) == "" {
		return "", errors.New("answer is empty")
	}
	r, err := loadRep(p.date())
	if err != nil {
		return "", err
	}
	e := p.pending(&r)
	if e == nil {
		return "", errors.New("no exercise selected")
	}
	if !e.AnsweredAt.IsZero() {
		return "", errors.New("exercise already answered")
	}
	if r.Pending == "daily" && e.Kind == "read" && len(r.Reads) >= 3 {
		return "", errors.New("three reads already started today")
	}
	e.Answer = mask.Text(answer)
	e.AnsweredAt = p.now
	if err = saveRep(p.date(), r); err != nil {
		return "", err
	}
	if err = p.Rebuild(); err != nil {
		return e.Reveal, &RebuildWarning{Err: err}
	}
	return e.Reveal, nil
}
func (p *Practice) Mark(mark string) error {
	if mark != "got_it" && mark != "partly" && mark != "missed" {
		return errors.New("mark must be got_it, partly or missed")
	}
	r, err := loadRep(p.date())
	if err != nil {
		return err
	}
	e := p.pending(&r)
	if e == nil || e.AnsweredAt.IsZero() {
		return errors.New("answer the exercise first")
	}
	e.Mark = mark
	return saveRep(p.date(), r)
}
func (p *Practice) Streak() (int, []bool, error) {
	days := make([]bool, 35)
	for i := range days {
		d := p.now.AddDate(0, 0, i-34).Format("2006-01-02")
		r, err := loadRep(d)
		if err != nil {
			return 0, nil, err
		}
		days[i] = r.Daily != nil && !r.Daily.AnsweredAt.IsZero()
	}
	i := 34
	if !days[i] {
		i--
	}
	n := 0
	for i >= 0 && days[i] {
		n++
		i--
	}
	if i < 0 && n >= 34 {
		for d := p.now.AddDate(0, 0, -35); ; d = d.AddDate(0, 0, -1) {
			r, err := loadRep(d.Format("2006-01-02"))
			if err != nil {
				return 0, nil, err
			}
			if r.Daily == nil || r.Daily.AnsweredAt.IsZero() {
				break
			}
			n++
		}
	}
	return n, days, nil
}
func (p *Practice) Rebuild() error {
	files, err := filepath.Glob(filepath.Join(Dir(), "reps", "*.json"))
	if err != nil {
		return err
	}
	var events []Event
	counts := map[string]int{}
	readSeen := map[string]bool{}
	var badges []Event
	for _, file := range files {
		date := strings.TrimSuffix(filepath.Base(file), ".json")
		r, err := loadRep(date)
		if err != nil {
			return err
		}
		var all []Exercise
		if r.Daily != nil && !r.Daily.AnsweredAt.IsZero() {
			e := *r.Daily
			events = append(events, Event{Key: "rep:" + date, Kind: kindRep, Track: TrackPractice, XP: 30, At: e.AnsweredAt})
			all = append(all, e)
		}
		all = append(all, r.Reads...)
		for _, e := range all {
			if e.AnsweredAt.IsZero() {
				continue
			}
			if e.Kind == "read" {
				key := strings.ToLower(e.Source)
				if readSeen[key] {
					continue
				}
				readSeen[key] = true
				ok, err := p.source.Merged(e.Source, e.AnsweredAt)
				if err == nil && ok {
					events = append(events, Event{Key: "read:" + key, Kind: kindRead, Track: TrackPractice, XP: 20, At: e.AnsweredAt})
				}
			}
			counts[e.Kind]++
			if counts[e.Kind] == 10 {
				name := map[string]string{"cold read": "cold-reader", "find the defect": "defect-hunter", "read": "fresh-eyes"}[e.Kind]
				if name != "" {
					badges = append(badges, Event{Key: "badge:" + name, Kind: kindBadge, Track: TrackBadge, At: e.AnsweredAt})
				}
			}
		}
	}
	_, err = Append(append(events, badges...))
	return err
}

// GitHubSource reads repository candidates from records and GitHub.
type GitHubSource struct{ roots map[string]string }

func NewGitHubSource() (*GitHubSource, error) {
	runs, err := runlog.List(0)
	if err != nil {
		return nil, err
	}
	batches, _, err := batch.List()
	if err != nil {
		return nil, err
	}
	roots := map[string]string{}
	for _, r := range runs {
		root := repoRoot(r.Task.Dir)
		if root != "" {
			roots[repoName(root)] = root
		}
	}
	for _, b := range batches {
		root := repoRoot(b.Dir)
		if root != "" {
			roots[repoName(root)] = root
		}
	}
	return &GitHubSource{roots: roots}, nil
}
func bounded(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, name, args...)
	var b bytes.Buffer
	c.Stdout = &limitWriter{w: &b, n: 4 << 20}
	c.Stderr = io.Discard
	err := c.Run()
	return b.Bytes(), err
}

type limitWriter struct {
	w io.Writer
	n int
}

func (w *limitWriter) Write(p []byte) (int, error) {
	if len(p) > w.n {
		return 0, errors.New("command output too large")
	}
	n, e := w.w.Write(p)
	w.n -= n
	return n, e
}

type prData struct {
	BaseRefOID string    `json:"baseRefOid"`
	Number     int       `json:"number"`
	Title      string    `json:"title"`
	Body       string    `json:"body"`
	State      string    `json:"state"`
	MergedAt   time.Time `json:"mergedAt"`
	Reviews    []struct {
		Body        string    `json:"body"`
		SubmittedAt time.Time `json:"submittedAt"`
		Commit      struct {
			OID string `json:"oid"`
		} `json:"commit"`
	} `json:"reviews"`
	Commits []struct {
		OID           string    `json:"oid"`
		CommittedDate time.Time `json:"committedDate"`
	} `json:"commits"`
}

func ghPR(ref string) (prData, error) {
	var p prData
	repo, num, _ := strings.Cut(ref, "#")
	b, err := bounded("gh", "pr", "view", num, "--repo", repo, "--json", "number,title,body,state,mergedAt,reviews,commits,baseRefOid")
	if err == nil {
		err = json.Unmarshal(b, &p)
	}
	return p, err
}
func diff(ref string) (string, error) {
	repo, num, _ := strings.Cut(ref, "#")
	b, err := bounded("gh", "pr", "diff", num, "--repo", repo)
	return string(b), err
}
func reviewText(p prData) string {
	s := p.Title + "\n\n" + p.Body
	for _, r := range p.Reviews {
		s += "\n\n" + r.Body
	}
	return s
}
func (s *GitHubSource) Read(ref string) (Candidate, error) {
	p, err := ghPR(ref)
	if err != nil {
		return Candidate{}, err
	}
	if p.State != "OPEN" {
		return Candidate{}, errors.New("pull request is not open")
	}
	d, err := diff(ref)
	return Candidate{Kind: "read", Source: ref, Material: d, Reveal: reviewText(p)}, err
}
func (s *GitHubSource) Merged(ref string, after time.Time) (bool, error) {
	p, err := ghPR(ref)
	return err == nil && p.State == "MERGED" && p.MergedAt.After(after), err
}
func (s *GitHubSource) Candidates(kind string) ([]Candidate, error) {
	var out []Candidate
	for repo, root := range s.roots {
		if kind == "recall" {
			b, err := bounded("git", "-C", root, "show", "HEAD:PLAN.md")
			if err == nil {
				out = append(out, parseDecisions(repo, string(b))...)
			}
			continue
		}
		if !strings.Contains(repo, "/") {
			continue
		}
		state := "merged"
		if kind == "read" {
			state = "open"
		}
		b, err := bounded("gh", "pr", "list", "--repo", repo, "--state", state, "--limit", "100", "--json", "number")
		if err != nil {
			continue
		}
		var prs []prData
		if json.Unmarshal(b, &prs) != nil {
			continue
		}
		for _, pr := range prs {
			ref := fmt.Sprintf("%s#%d", repo, pr.Number)
			p, err := ghPR(ref)
			if err != nil {
				continue
			}
			if kind == "read" {
				c, err := s.Read(ref)
				if err == nil {
					out = append(out, c)
				}
				continue
			}
			if kind == "cold read" {
				d, err := diff(ref)
				if err == nil {
					reveal := reviewText(p)
					for _, review := range p.Reviews {
						for i, commit := range p.Commits {
							if commit.OID == review.Commit.OID && i+1 < len(p.Commits) {
								later, err := bounded("gh", "api", "-H", "Accept: application/vnd.github.diff", fmt.Sprintf("repos/%s/compare/%s...%s", repo, commit.OID, p.Commits[len(p.Commits)-1].OID))
								if err == nil {
									reveal += "\n\nLater commits:\n" + string(later)
								}
								break
							}
						}
					}
					out = append(out, Candidate{Source: ref, Material: d, Reveal: reveal})
				}
				continue
			}
			if kind == "find the defect" {
				for _, r := range p.Reviews {
					if !blocking.MatchString(r.Body) || r.Commit.OID == "" {
						continue
					}
					if !reviewHasLaterCommit(p, r.Commit.OID, r.SubmittedAt) {
						continue
					}
					if len(p.Commits) == 0 || p.Commits[0].OID == "" {
						continue
					}
					parent, err := bounded("gh", "api", fmt.Sprintf("repos/%s/commits/%s", repo, p.Commits[0].OID), "--jq", ".parents[0].sha")
					if err != nil {
						continue
					}
					base := strings.TrimSpace(string(parent))
					if base == "" {
						continue
					}
					before, err := bounded("gh", "api", "-H", "Accept: application/vnd.github.diff", fmt.Sprintf("repos/%s/compare/%s...%s", repo, base, r.Commit.OID))
					if err != nil {
						continue
					}
					after, err := bounded("gh", "api", "-H", "Accept: application/vnd.github.diff", fmt.Sprintf("repos/%s/compare/%s...%s", repo, r.Commit.OID, p.Commits[len(p.Commits)-1].OID))
					if err == nil {
						out = append(out, Candidate{Source: ref + "@" + r.Commit.OID, Material: string(before), Reveal: r.Body + "\n" + string(after)})
					}
				}
			}
		}
	}
	return out, nil
}

func reviewHasLaterCommit(p prData, oid string, submitted time.Time) bool {
	if oid == "" || submitted.IsZero() {
		return false
	}
	for i, c := range p.Commits {
		if c.OID == oid {
			for _, later := range p.Commits[i+1:] {
				if !later.CommittedDate.IsZero() && later.CommittedDate.After(submitted) {
					return true
				}
			}
			return false
		}
	}
	return false
}

var decisionList = regexp.MustCompile(`^- [0-9]{4}-[0-9]{2}-[0-9]{2}: `)

func parseDecisions(repo, text string) []Candidate {
	var out []Candidate
	in := false
	var heading string
	var body []string
	listEntry := false
	flush := func() {
		if heading != "" {
			out = append(out, Candidate{Kind: "recall", Source: repo + "#" + heading, Material: heading, Reveal: strings.TrimSpace(strings.Join(body, "\n"))})
		}
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "## ") {
			if in {
				flush()
				in = false
				break
			}
			in = strings.TrimSpace(line) == "## Decisions log"
			continue
		}
		if !in {
			continue
		}
		if decisionList.MatchString(line) {
			flush()
			headline := line[len("- 2006-01-02: "):]
			end := len(headline)
			for _, sep := range []string{" (", ". "} {
				if i := strings.Index(headline, sep); i >= 0 && i < end {
					end = i
				}
			}
			heading = strings.TrimSpace(headline[:end])
			body = []string{line}
			listEntry = true
		} else if strings.HasPrefix(line, "### ") {
			flush()
			heading = strings.TrimSpace(strings.TrimPrefix(line, "### "))
			body = nil
			listEntry = false
		} else if listEntry && line != "" && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			flush()
			heading = ""
			body = nil
			listEntry = false
		} else if heading != "" {
			body = append(body, line)
		}
	}
	if in {
		flush()
	}
	return out
}
