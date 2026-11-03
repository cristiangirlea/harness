package game

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/eval"
	"github.com/cristiangirlea/harness/internal/route/table"
	"github.com/cristiangirlea/harness/internal/runlog"
	"github.com/cristiangirlea/harness/internal/scripthint"
)

// Records are what one fold reads. AllBatches is every batch on disk: it says whether a run
// was a batch attempt and how that attempt's gates went, even when the batch is not folded.
type Records struct {
	Runs       []runlog.Record
	Batches    []batch.Result
	Evals      []eval.Record
	AllBatches []batch.Result
	// Removing names batches about to be pruned: nothing will finish their unsettled attempts.
	Removing map[string]bool
}

// Derive turns records into delivery events; it reads the events.jsonl of finished runs.
func Derive(r Records) ([]Event, error) {
	var events []Event
	seen := map[string]bool{}
	names := map[string]string{}
	name := func(dir string) string {
		root := repoRoot(dir)
		key := cleanPath(root)
		if n, ok := names[key]; ok {
			return n
		}
		names[key] = repoName(root)
		return names[key]
	}
	add := func(key, kind string, at time.Time, dir string) {
		if !seen[key] {
			seen[key] = true
			events = append(events, Event{Key: key, Kind: kind, Track: TrackDelivery, XP: rules[kind].xp, At: at.UTC(), Repo: name(dir)})
		}
	}
	type firstUse struct {
		at      time.Time
		dir, id string
	}
	firsts := map[string]firstUse{}
	first := func(feature, id string, at time.Time, dir string) {
		e, ok := firsts[feature]
		if !ok || at.Before(e.at) || at.Equal(e.at) && id < e.id {
			firsts[feature] = firstUse{at: at.UTC(), dir: dir, id: id}
		}
	}
	passed, settled, busy := map[string]bool{}, map[string]bool{}, map[string]bool{}
	batchDirs := map[string]string{}
	for _, b := range r.AllBatches {
		batchDirs[b.ID] = b.Dir
		for _, t := range b.Tasks {
			if t.Status == batch.StatusRunning {
				busy[cleanPath(t.Worktree)] = true
			}
			for i, a := range t.Attempts {
				if a.RunID != "" {
					p := gatesPass(a.Gates)
					s := t.Status != batch.StatusRunning || i < len(t.Attempts)-1
					if !s && r.Removing[b.ID] {
						p, s = false, true
					}
					if old, ok := passed[a.RunID]; ok {
						p = p && old
						s = s && settled[a.RunID]
					}
					passed[a.RunID] = p
					settled[a.RunID] = s
				}
			}
		}
	}
	scripts := map[string][]string{}
	for _, rec := range r.Runs {
		if !rec.Done {
			continue
		}
		at := rec.Started.Add(rec.Result.Duration).UTC()
		if rec.Result.Status == agent.StatusOK {
			if rec.Task.Purpose == "review" && strings.TrimSpace(rec.Result.Final) != "" && !blocking.MatchString(rec.Result.Final) {
				add("review:"+rec.ID, kindReview, at, rec.Task.Dir)
			}
			if rec.Task.Timeout > 0 || rec.Backend == "api" && rec.Task.MaxCostUSD > 0 && rec.Result.CostUSD > 0 &&
				!(rec.Result.Usage != nil && rec.Result.Usage.CostStatus == "missing") {
				add("ceiling:"+rec.ID, kindCeiling, at, rec.Task.Dir)
			}
			if rec.RoutedBy != nil {
				p, attempt := passed[rec.ID]
				if attempt && settled[rec.ID] || !attempt && !busy[cleanPath(rec.Task.Dir)] {
					kind := kindRouted
					if attempt && !p {
						kind = kindRoutedFailed
					}
					add("routed:"+rec.ID, kind, at, rec.Task.Dir)
				}
			}
		}
		if rec.RoutedBy != nil {
			first("routed", rec.ID, at, rec.Task.Dir)
		}
		if rec.Task.Dir == "" {
			continue
		}
		root, ok := scripthint.Root(rec.Task.Dir)
		if !ok {
			continue
		}
		kept, loaded := scripts[root]
		if !loaded {
			kept = keptScripts(root)
			scripts[root] = kept
		}
		if len(kept) == 0 {
			continue
		}
		reused, err := reusedScripts(rec.ID, kept)
		if err != nil {
			return nil, err
		}
		for _, script := range reused {
			add("script:"+rec.ID+"/"+script, kindScript, at, root)
		}
	}
	for _, b := range r.Batches {
		first("batch", b.ID, b.Started, b.Dir)
		for _, t := range b.Tasks {
			if t.Slot > 0 {
				first("slot", b.ID, b.Started, b.Dir)
			}
			if t.Status == batch.StatusRunning {
				continue
			}
			failed := false
			for _, a := range t.Attempts {
				if failed && len(a.Gates) > 0 && gatesPass(a.Gates) {
					add("batch:"+b.ID+"/"+t.ID, kindBug, b.Started, b.Dir)
					break
				}
				for _, g := range a.Gates {
					if !g.Guard && g.InfrastructureError == "" && (g.ExitCode != 0 || g.Killed != "") {
						failed = true
					}
				}
			}
		}
	}
	for _, e := range r.Evals {
		first("eval", e.ID, e.Started, batchDirs[e.BatchID])
		if e.SetupComparison != nil && e.SetupComparison.Verdict == eval.SetupBetter {
			key := "ada:setup:" + e.ID
			if !seen[key] {
				seen[key] = true
				events = append(events, Event{Key: key, Kind: "ada_setup", Track: TrackAda, XP: 30, At: e.Started.UTC(), Repo: name(batchDirs[e.BatchID])})
			}
		}
	}
	routes, err := table.Load(runlog.Home())
	if err != nil {
		return nil, err
	}
	for category, row := range routes {
		at, err := time.Parse("2006-01-02", row.Set)
		if row.Eval == "" || err != nil {
			continue
		}
		key := "ada:route:" + category + "/" + row.Set
		events = append(events, Event{Key: key, Kind: "ada_route", Track: TrackAda, XP: 10, At: at})
	}
	roots := map[string]bool{}
	for _, rec := range r.Runs {
		if rec.Task.Dir != "" {
			roots[repoRoot(rec.Task.Dir)] = true
		}
	}
	for _, b := range r.Batches {
		if b.Dir != "" {
			roots[repoRoot(b.Dir)] = true
		}
	}
	var rootList []string
	for root := range roots {
		rootList = append(rootList, root)
	}
	sort.Strings(rootList)
	for _, root := range rootList {
		for _, e := range mergedScripts(root) {
			if !seen[e.Key] {
				seen[e.Key] = true
				events = append(events, e)
			}
		}
	}
	for _, feature := range []string{"batch", "slot", "eval", "routed"} {
		if e, ok := firsts[feature]; ok {
			add("first:"+feature, kindFirst, e.at, e.dir)
		}
	}
	return events, nil
}

var commitHash = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

func mergedScripts(root string) []Event {
	ctx := context.Background()
	name := repoName(root)
	if !strings.Contains(name, "/") {
		return nil
	}
	log := gitEvidence(ctx, root, "log", "--first-parent", "--merges", "--reverse", "--no-show-signature", "--format=%H %cI", "refs/remotes/origin/HEAD", "--")
	var events []Event
	for _, line := range strings.Split(strings.TrimSpace(log), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || !commitHash.MatchString(fields[0]) {
			continue
		}
		at, err := time.Parse(time.RFC3339, fields[1])
		if err != nil {
			continue
		}
		files := gitEvidence(ctx, root, "diff-tree", "--no-ext-diff", "--no-textconv", "-r", "--no-commit-id", "--name-only", "--diff-filter=A", "--no-renames", "-z", fields[0]+"^1", fields[0], "--", "scripts/")
		for _, file := range strings.Split(files, "\x00") {
			if !strings.HasPrefix(file, "scripts/") || file == "scripts/" {
				continue
			}
			body := gitEvidence(ctx, root, "show", "--no-ext-diff", "--no-textconv", fields[0]+":"+file)
			if !keptHeader(body) {
				continue
			}
			events = append(events, Event{Key: "ada:script:" + name + "/" + strings.TrimPrefix(file, "scripts/"), Kind: "ada_script", Track: TrackAda, XP: 10, At: at.UTC(), Repo: name})
		}
	}
	return events
}

func keptHeader(body string) bool {
	header := map[string]string{}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.HasPrefix(line, "#!") {
			continue
		}
		var ok bool
		if line, ok = strings.CutPrefix(line, "# "); !ok {
			if line, ok = strings.CutPrefix(line, "// "); !ok {
				break
			}
		}
		key, value, found := strings.Cut(line, ": ")
		if !found || key == "" || strings.Trim(key, "abcdefghijklmnopqrstuvwxyz") != "" || value == "" {
			break
		}
		header[key] = value
	}
	for _, key := range []string{"name", "summary", "effects", "parameters", "example", "hash"} {
		if header[key] == "" {
			return false
		}
	}
	return true
}

var blocking = regexp.MustCompile(`(?i)(^|[^\pL\pN_-])blocking($|[^\pL\pN_])`)

func cleanPath(dir string) string {
	dir = filepath.Clean(dir)
	if runtime.GOOS == "windows" {
		dir = strings.ToLower(dir)
	}
	return dir
}

func gatesPass(gates []batch.GateResult) bool {
	for _, g := range gates {
		if g.ExitCode != 0 || g.Killed != "" || g.InfrastructureError != "" {
			return false
		}
	}
	return true
}

func keptScripts(root string) []string {
	var names []string
	for _, pattern := range []string{"*.sh", "*.go", "*/*.go"} {
		files, _ := filepath.Glob(filepath.Join(root, "scripts", filepath.FromSlash(pattern)))
		for _, f := range files {
			if scripthint.Kept(f) {
				n, _ := filepath.Rel(filepath.Join(root, "scripts"), f)
				names = append(names, filepath.ToSlash(n))
			}
		}
	}
	return names
}

func reusedScripts(id string, kept []string) ([]string, error) {
	f, err := os.Open(filepath.Join(runlog.Root(), id, "events.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	seen := map[string]bool{}
	var reused []string
	r := bufio.NewReader(f)
	for {
		line, readErr := r.ReadBytes('\n')
		if bytes.Contains(line, []byte("scripts")) {
			var e agent.Event
			if json.Unmarshal(line, &e) == nil && e.Kind == agent.EventToolCall && shellTool(e.Tool) {
				command := e.Text
				var input map[string]json.RawMessage
				var cmd string
				if json.Unmarshal(e.Input, &input) == nil && json.Unmarshal(input["command"], &cmd) == nil && string(input["command"]) != "null" {
					command = cmd
				}
				fields := strings.Fields(strings.ReplaceAll(command, `\`, "/"))
				for i, field := range fields {
					field = strings.Trim(strings.TrimRight(strings.Trim(field, "\"'`"), ";)"), "\"'`")
					for _, n := range kept {
						if !seen[n] && (field == "scripts/"+n || strings.HasSuffix(field, "/scripts/"+n)) && scriptRuns(fields, i) {
							seen[n] = true
							reused = append(reused, n)
						}
					}
				}
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				readErr = nil
			}
			return reused, readErr
		}
	}
}

func shellTool(tool string) bool {
	switch strings.ToLower(tool) {
	case "bash", "powershell", "run_command", "shell", "command_execution", "exec_command", "local_shell", "run_shell_command":
		return true
	}
	return false
}

func scriptRuns(fields []string, i int) bool {
	if i == 0 {
		return true
	}
	syntax := false
	for j := i - 1; j >= 0; j-- {
		f := strings.ToLower(strings.Trim(fields[j], "\"'`"))
		if strings.HasPrefix(f, "-") {
			syntax = syntax || f == "-n"
			continue
		}
		switch f {
		case "run", "&&", "||", ";", "|", "&":
			return true
		}
		base := strings.TrimSuffix(path.Base(f), ".exe")
		return (base == "bash" || base == "sh") && !syntax
	}
	return false
}

// Fold appends what r derives, then the badges the ledger has earned by then.
func Fold(r Records) error {
	events, err := Derive(r)
	if err != nil {
		return err
	}
	if _, err := Append(events); err != nil {
		return err
	}
	events, _, err = Load()
	if err != nil {
		return err
	}
	var badges []Event
	badge := func(name string, at time.Time) {
		badges = append(badges, Event{Key: "badge:" + name, Kind: kindBadge, Track: TrackBadge, At: at.UTC()})
	}
	ceilings, routed := 0, 0
	for _, e := range ordered(events) {
		switch e.Kind {
		case kindCeiling:
			ceilings++
			if ceilings == 20 {
				badge("budget-keeper", e.At)
			}
		case kindRouted:
			routed++
			if routed == 10 {
				badge("cheap-and-right", e.At)
			}
		}
	}
	xp := 0
	for _, e := range Sum(events).Counted {
		if e.Track == TrackDelivery || e.Track == TrackPractice {
			xp += e.XP
		}
		if xp >= 5000 {
			badge("veteran", e.At)
			break
		}
	}
	_, err = Append(badges)
	return err
}

// Rebuild skips records that cannot be loaded, but fails if a run's events cannot be read.
func Rebuild() error {
	runs, err := runlog.List(0)
	if err != nil {
		return err
	}
	batches, _, err := batch.List()
	if err != nil {
		return err
	}
	evals, _, err := eval.ListAll()
	if err != nil {
		return err
	}
	if err := Fold(Records{Runs: runs, Batches: batches, Evals: evals, AllBatches: batches}); err != nil {
		return err
	}
	source, err := NewGitHubSource()
	if err != nil {
		return err
	}
	if err := NewPractice(source, time.Now()).Rebuild(); err != nil {
		return err
	}
	return Fold(Records{})
}

// repoName names a repository owner/name as GitHub does, so local and pull request events agree.
func repoName(dir string) string {
	root := repoRoot(dir)
	if root == "" {
		return ""
	}
	gitDir := filepath.Join(root, ".git")
	if info, err := os.Stat(gitDir); err == nil && !info.IsDir() {
		data, err := os.ReadFile(gitDir)
		if err != nil {
			return filepath.Base(root)
		}
		value, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir:")
		if !ok {
			return filepath.Base(root)
		}
		gitDir = strings.TrimSpace(value)
		if !filepath.IsAbs(gitDir) {
			gitDir = filepath.Join(root, gitDir)
		}
		if data, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
			common := strings.TrimSpace(string(data))
			if !filepath.IsAbs(common) {
				common = filepath.Join(gitDir, common)
			}
			gitDir = common
		}
	}
	f, err := os.Open(filepath.Join(gitDir, "config"))
	if err != nil {
		return filepath.Base(root)
	}
	defer f.Close()
	origin := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			origin = false
			if end := strings.IndexByte(line, ']'); end >= 0 {
				section := strings.TrimSpace(line[1:end])
				if i := strings.IndexAny(section, " \t"); i >= 0 {
					origin = strings.EqualFold(section[:i], "remote") && strings.TrimSpace(section[i:]) == `"origin"`
				}
			}
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !origin || !ok || !strings.EqualFold(strings.TrimSpace(key), "url") {
			continue
		}
		remote := strings.TrimSpace(value)
		if len(remote) >= 2 && remote[0] == '"' && remote[len(remote)-1] == '"' {
			remote = remote[1 : len(remote)-1]
		}
		if name := parseRemote(remote); name != "" {
			return name
		}
		break
	}
	return filepath.Base(root)
}

func repoRoot(dir string) string {
	if dir == "" {
		return ""
	}
	if root, ok := scripthint.Root(dir); ok {
		return root
	}
	return filepath.Clean(dir)
}

// parseRemote returns owner/name for a GitHub remote URL, or "" for any other.
func parseRemote(remote string) string {
	var p string
	if rest, ok := strings.CutPrefix(remote, "git@github.com:"); ok {
		p = rest
	} else {
		u, err := url.Parse(remote)
		if err != nil || u.Host != "github.com" || u.RawQuery != "" || u.Fragment != "" {
			return ""
		}
		if u.Scheme != "https" && u.Scheme != "http" && u.Scheme != "ssh" {
			return ""
		}
		if u.Scheme == "ssh" && (u.User == nil || u.User.String() != "git") || u.Scheme != "ssh" && u.User != nil {
			return ""
		}
		p = strings.TrimPrefix(u.Path, "/")
	}
	p = strings.TrimSuffix(strings.TrimSuffix(p, "/"), ".git")
	parts := strings.Split(p, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return ""
	}
	return p
}
